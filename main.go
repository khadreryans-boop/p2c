package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gobwas/ws"
)

const (
	host           = "app.send.tg"
	wsPath         = "/internal/v1/p2c-socket/?EIO=4&transport=websocket"
	takePathPrefix = "/internal/v1/p2c/payments/take/"
	pauseSeconds   = 20
)

const (
	numWebSockets  = 20
	numTakers      = 2    // Только 2 лучших
	parallelTakes  = 2    // Оба делают take
	warmupInterval = 5000 // 15 сек между warmup (консервативно)
)

// 200 req / 5 min = 40 req/min = 0.66 req/sec
// 2 takers * 4 warmup/min = 8 warmup/min = 40 за 5 мин
// Остаётся 160 req на take = 80 заказов

var (
	pauseTaking atomic.Bool
	seenOrders  sync.Map
	cookie      string
	reqPrefix   []byte
	reqSuffix   []byte
	serverIP    string
)

// Rate limiter
var (
	rateMu      sync.Mutex
	lastRequest time.Time
	minGap      = 1500 * time.Millisecond // ~0.66 req/sec max
)

func rateLimit() bool {
	rateMu.Lock()
	defer rateMu.Unlock()
	if time.Since(lastRequest) < minGap {
		return false
	}
	lastRequest = time.Now()
	return true
}

// Stats
var (
	statsMu   sync.Mutex
	totalSeen int
	totalWon  int
	totalLate int
	totalReqs int
)

// ============ Taker ============

type taker struct {
	conn    net.Conn
	br      *bufio.Reader
	bw      *bufio.Writer
	mu      sync.Mutex
	ready   atomic.Bool
	inUse   atomic.Bool
	lastRtt atomic.Uint64
	id      int
}

var takers []*taker

func (t *taker) connect() error {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.conn != nil {
		t.conn.Close()
	}

	conn, err := net.DialTimeout("tcp", serverIP+":443", 2*time.Second)
	if err != nil {
		t.ready.Store(false)
		return err
	}

	if tc, ok := conn.(*net.TCPConn); ok {
		tc.SetNoDelay(true)
		tc.SetKeepAlive(true)
		tc.SetKeepAlivePeriod(30 * time.Second)
	}

	tlsConn := tls.Client(conn, &tls.Config{ServerName: host})
	if err := tlsConn.Handshake(); err != nil {
		conn.Close()
		t.ready.Store(false)
		return err
	}

	t.conn = tlsConn
	t.br = bufio.NewReaderSize(tlsConn, 4096)
	t.bw = bufio.NewWriterSize(tlsConn, 2048)
	t.ready.Store(true)
	return nil
}

func (t *taker) take(orderID string) (int, time.Duration, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.conn == nil {
		return 0, 0, fmt.Errorf("no conn")
	}

	statsMu.Lock()
	totalReqs++
	statsMu.Unlock()

	t.conn.SetDeadline(time.Now().Add(2 * time.Second))

	t.bw.Write(reqPrefix)
	t.bw.WriteString(orderID)
	t.bw.Write(reqSuffix)

	start := time.Now()
	if err := t.bw.Flush(); err != nil {
		t.conn.Close()
		t.conn = nil
		t.ready.Store(false)
		return 0, 0, err
	}

	line, err := t.br.ReadString('\n')
	dur := time.Since(start)

	if err != nil {
		t.conn.Close()
		t.conn = nil
		t.ready.Store(false)
		return 0, dur, err
	}

	if len(line) < 12 {
		return 0, dur, fmt.Errorf("short")
	}
	code, _ := strconv.Atoi(line[9:12])

	// Drain
	for {
		line, _ := t.br.ReadString('\n')
		if line == "\r\n" || line == "" {
			break
		}
	}

	t.lastRtt.Store(uint64(dur.Milliseconds()))
	return code, dur, nil
}

// Warmup через fake take
func (t *taker) warmupViaTake() {
	if !rateLimit() {
		return
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	if t.conn == nil {
		return
	}

	statsMu.Lock()
	totalReqs++
	statsMu.Unlock()

	fakeID := "000000000000000000000000"

	t.conn.SetDeadline(time.Now().Add(2 * time.Second))

	t.bw.Write(reqPrefix)
	t.bw.WriteString(fakeID)
	t.bw.Write(reqSuffix)

	start := time.Now()
	if err := t.bw.Flush(); err != nil {
		t.conn.Close()
		t.conn = nil
		t.ready.Store(false)
		return
	}

	line, err := t.br.ReadString('\n')
	dur := time.Since(start)

	if err != nil {
		t.conn.Close()
		t.conn = nil
		t.ready.Store(false)
		return
	}

	// Drain
	for {
		line, _ = t.br.ReadString('\n')
		if line == "\r\n" || line == "" {
			break
		}
	}

	t.lastRtt.Store(uint64(dur.Milliseconds()))
	fmt.Printf("   [T%d] warmup rtt=%dms\n", t.id, dur.Milliseconds())
}

func getAvailable() []*taker {
	var avail []*taker
	for _, t := range takers {
		if t.ready.Load() && !t.inUse.Load() {
			avail = append(avail, t)
		}
	}
	return avail
}

// ============ Take Logic ============

func doTake(id, amt string, wsID int, detectTime time.Time) {
	if pauseTaking.Load() {
		return
	}
	if _, loaded := seenOrders.LoadOrStore(id, struct{}{}); loaded {
		return
	}

	statsMu.Lock()
	totalSeen++
	statsMu.Unlock()

	avail := getAvailable()
	if len(avail) == 0 {
		fmt.Printf("   ❌ No takers\n")
		return
	}

	type res struct {
		id   int
		code int
		dur  time.Duration
		err  error
	}

	ch := make(chan res, len(avail))
	for _, t := range avail {
		t.inUse.Store(true)
		go func(tk *taker) {
			code, dur, err := tk.take(id)
			tk.inUse.Store(false)
			if err != nil {
				go tk.connect()
			}
			ch <- res{tk.id, code, dur, err}
		}(t)
	}

	var results []res
	for i := 0; i < len(avail); i++ {
		results = append(results, <-ch)
	}

	sort.Slice(results, func(i, j int) bool { return results[i].dur < results[j].dur })

	e2e := time.Since(detectTime).Milliseconds()
	var parts []string
	var won bool

	for _, r := range results {
		if r.err != nil {
			parts = append(parts, fmt.Sprintf("T%d:ERR", r.id))
			continue
		}
		s := fmt.Sprintf("T%d:%d", r.id, r.dur.Milliseconds())
		if r.code == 200 {
			s += "✓"
			won = true
		} else if r.code == 429 {
			s += "(RATE)"
		} else {
			s += fmt.Sprintf("(%d)", r.code)
		}
		parts = append(parts, s)
	}

	if won {
		statsMu.Lock()
		totalWon++
		statsMu.Unlock()
		fmt.Printf("✅ [WS%02d] e2e=%dms amt=%s | %s\n", wsID, e2e, amt, strings.Join(parts, " "))
		pauseTaking.Store(true)
		go func() {
			time.Sleep(pauseSeconds * time.Second)
			pauseTaking.Store(false)
			fmt.Println("▶ Resumed")
		}()
	} else {
		statsMu.Lock()
		totalLate++
		statsMu.Unlock()
		fmt.Printf("   [WS%02d] LATE e2e=%dms amt=%s | %s\n", wsID, e2e, amt, strings.Join(parts, " "))
	}

	go func() {
		time.Sleep(3 * time.Second)
		seenOrders.Delete(id)
	}()
}

// ============ Parser ============

var (
	opAddBytes = []byte(`"op":"add"`)
	idPrefix   = []byte(`"id":"`)
	amtPrefix  = []byte(`"in_amount":"`)
)

func parseAndTake(data []byte, wsID int, detectTime time.Time, minCents int64) {
	if !bytes.Contains(data, opAddBytes) {
		return
	}

	idx := bytes.Index(data, idPrefix)
	if idx == -1 {
		return
	}
	start := idx + 6
	end := bytes.IndexByte(data[start:], '"')
	if end == -1 || end > 30 {
		return
	}
	id := string(data[start : start+end])

	var amt string
	idx = bytes.Index(data, amtPrefix)
	if idx != -1 {
		start = idx + 13
		end = bytes.IndexByte(data[start:], '"')
		if end != -1 && end < 20 {
			amt = string(data[start : start+end])
		}
	}

	if minCents > 0 && amt != "" {
		var whole, frac int64
		var fracDigits int
		var seenDot bool
		for i := 0; i < len(amt); i++ {
			c := amt[i]
			if c == '.' {
				seenDot = true
				continue
			}
			if c >= '0' && c <= '9' {
				d := int64(c - '0')
				if !seenDot {
					whole = whole*10 + d
				} else if fracDigits < 2 {
					frac = frac*10 + d
					fracDigits++
				}
			}
		}
		if fracDigits == 1 {
			frac *= 10
		}
		if whole*100+frac < minCents {
			return
		}
	}

	doTake(id, amt, wsID, detectTime)
}

// ============ WebSocket ============

func runWS(wsID int, minCents int64) {
	for {
		conn, err := connectWS()
		if err != nil {
			time.Sleep(2 * time.Second)
			continue
		}

		readFrame(conn)
		writeFrame(conn, []byte("40"))
		readFrame(conn)

		time.Sleep(30 * time.Millisecond)
		writeFrame(conn, []byte(`42["list:initialize"]`))
		time.Sleep(30 * time.Millisecond)
		writeFrame(conn, []byte(`42["list:snapshot",[]]`))

		fmt.Printf("[WS%02d] 🚀\n", wsID)

		for {
			data, op, err := readFrame(conn)
			detectTime := time.Now()

			if err != nil {
				break
			}

			if op == ws.OpText {
				if len(data) == 1 && data[0] == '2' {
					writeFrame(conn, []byte("3"))
					continue
				}
				if len(data) > 2 && data[0] == '4' && data[1] == '2' {
					parseAndTake(data[2:], wsID, detectTime, minCents)
				}
			} else if op == ws.OpPing {
				f := ws.NewPongFrame(data)
				f = ws.MaskFrameInPlace(f)
				ws.WriteFrame(conn, f)
			} else if op == ws.OpClose {
				break
			}
		}

		conn.Close()
		time.Sleep(1 * time.Second)
	}
}

func connectWS() (net.Conn, error) {
	dialer := ws.Dialer{
		Header: ws.HandshakeHeaderHTTP(http.Header{
			"Cookie": []string{cookie},
			"Origin": []string{"https://app.send.tg"},
		}),
		Timeout: 10 * time.Second,
		NetDial: func(ctx context.Context, network, addr string) (net.Conn, error) {
			conn, err := net.DialTimeout("tcp", serverIP+":443", 5*time.Second)
			if err != nil {
				return nil, err
			}
			if tc, ok := conn.(*net.TCPConn); ok {
				tc.SetNoDelay(true)
			}
			return conn, nil
		},
		TLSConfig: &tls.Config{ServerName: host},
	}
	conn, _, _, err := dialer.Dial(context.Background(), "wss://"+host+wsPath)
	return conn, err
}

func writeFrame(conn net.Conn, data []byte) {
	frame := ws.NewTextFrame(data)
	frame = ws.MaskFrameInPlace(frame)
	ws.WriteFrame(conn, frame)
}

func readFrame(conn net.Conn) ([]byte, ws.OpCode, error) {
	h, err := ws.ReadHeader(conn)
	if err != nil {
		return nil, 0, err
	}
	p := make([]byte, h.Length)
	if h.Length > 0 {
		io.ReadFull(conn, p)
	}
	if h.Masked {
		ws.Cipher(p, h.Mask, 0)
	}
	return p, h.OpCode, nil
}

// ============ Main ============

func main() {
	in := bufio.NewReader(os.Stdin)

	fmt.Println("╔═══════════════════════════════════════════╗")
	fmt.Println("║  P2C SNIPER - 2 Takers (Rate Limited)     ║")
	fmt.Println("╚═══════════════════════════════════════════╝")

	fmt.Print("\naccess_token cookie:\n> ")
	cookie, _ = in.ReadString('\n')
	cookie = strings.TrimSpace(cookie)
	if !strings.HasPrefix(cookie, "access_token=") {
		fmt.Println("Invalid")
		return
	}

	fmt.Print("MIN amount (0=all):\n> ")
	minLine, _ := in.ReadString('\n')
	minLine = strings.TrimSpace(minLine)
	var minCents int64
	if minLine != "" {
		f, _ := strconv.ParseFloat(minLine, 64)
		minCents = int64(f * 100)
	}

	fmt.Println("\n⏳ Resolving DNS...")
	ips, err := net.LookupHost(host)
	if err != nil {
		fmt.Printf("DNS error: %v\n", err)
		return
	}
	serverIP = ips[0]
	fmt.Printf("✅ Server IP: %s\n", serverIP)

	reqPrefix = []byte("POST " + takePathPrefix)
	reqSuffix = []byte(" HTTP/1.1\r\nHost: " + host + "\r\nContent-Type: application/json\r\nCookie: " + cookie + "\r\nContent-Length: 2\r\n\r\n{}")

	// Create 2 takers
	fmt.Printf("\n⏳ Creating %d takers...\n", numTakers)
	for i := 0; i < numTakers; i++ {
		t := &taker{id: i + 1}
		t.lastRtt.Store(999999)
		t.connect()
		takers = append(takers, t)
	}

	ready := 0
	for _, t := range takers {
		if t.ready.Load() {
			ready++
		}
	}
	fmt.Printf("✅ %d/%d takers ready\n", ready, numTakers)

	// Warmup goroutines (rate limited)
	for i, t := range takers {
		go func(idx int, tk *taker) {
			// Stagger
			time.Sleep(time.Duration(idx*7500) * time.Millisecond)
			for {
				time.Sleep(time.Duration(warmupInterval) * time.Millisecond)
				if pauseTaking.Load() {
					continue
				}
				if !tk.inUse.Load() && tk.ready.Load() {
					tk.warmupViaTake()
				} else if !tk.ready.Load() {
					tk.connect()
				}
			}
		}(i, t)
	}

	// Start WebSockets
	fmt.Printf("⏳ Starting %d WebSockets...\n", numWebSockets)
	for i := 1; i <= numWebSockets; i++ {
		go runWS(i, minCents)
		time.Sleep(50 * time.Millisecond)
	}

	fmt.Println("\n════════════════════════════════════════════")
	fmt.Printf("  %d WS | %d takers | warmup every %ds\n", numWebSockets, numTakers, warmupInterval/1000)
	fmt.Println("  Rate limit: <200 req / 5 min")
	if minCents > 0 {
		fmt.Printf("  MIN: %.2f RUB\n", float64(minCents)/100)
	}
	fmt.Println("════════════════════════════════════════════\n")

	// Stats
	go func() {
		for {
			time.Sleep(60 * time.Second)
			statsMu.Lock()
			rate := float64(totalWon) / float64(max(totalSeen, 1)) * 100
			fmt.Printf("\n📊 STATS: seen=%d won=%d late=%d (%.1f%%) | total_reqs=%d\n\n",
				totalSeen, totalWon, totalLate, rate, totalReqs)
			statsMu.Unlock()
		}
	}()

	select {}
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
