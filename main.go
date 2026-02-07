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
	numWebSockets = 20
	numTakers     = 5
	warmupMs      = 1000 // Warmup interval
)

var (
	pauseTaking atomic.Bool
	cookie      string
	serverIP    string
)

var reqPrefix []byte
var reqSuffix []byte

// Dedupe
var (
	seenMu sync.Mutex
	seen   = make(map[string]struct{})
)

func markSeen(id string) bool {
	seenMu.Lock()
	if _, ok := seen[id]; ok {
		seenMu.Unlock()
		return false
	}
	seen[id] = struct{}{}
	seenMu.Unlock()
	go func() {
		time.Sleep(5 * time.Second)
		seenMu.Lock()
		delete(seen, id)
		seenMu.Unlock()
	}()
	return true
}

// Stats
var (
	totalSeen atomic.Int64
	totalWon  atomic.Int64
	totalLate atomic.Int64
)

// ============ Taker ============

type taker struct {
	conn  net.Conn
	br    *bufio.Reader
	bw    *bufio.Writer
	mu    sync.Mutex
	ready atomic.Bool
	inUse atomic.Bool
	id    int
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

func (t *taker) warmup() {
	if !t.inUse.CompareAndSwap(false, true) {
		return
	}
	defer t.inUse.Store(false)

	t.mu.Lock()
	defer t.mu.Unlock()

	if t.conn == nil {
		return
	}

	t.conn.SetDeadline(time.Now().Add(500 * time.Millisecond))

	req := "POST /internal/v1/p2c/accounts HTTP/1.1\r\n" +
		"Host: " + host + "\r\n" +
		"Cookie: " + cookie + "\r\n" +
		"Content-Type: application/json\r\n" +
		"Content-Length: 2\r\n" +
		"Connection: keep-alive\r\n\r\n{}"

	t.bw.WriteString(req)
	if err := t.bw.Flush(); err != nil {
		t.conn.Close()
		t.conn = nil
		t.ready.Store(false)
		return
	}

	line, err := t.br.ReadString('\n')
	if err != nil {
		t.conn.Close()
		t.conn = nil
		t.ready.Store(false)
		return
	}
	_ = line

	// Drain headers
	contentLen := 0
	for {
		line, _ = t.br.ReadString('\n')
		if line == "\r\n" || line == "" {
			break
		}
		if strings.HasPrefix(strings.ToLower(line), "content-length:") {
			fmt.Sscanf(line[15:], "%d", &contentLen)
		}
	}

	if contentLen > 0 {
		body := make([]byte, contentLen)
		io.ReadFull(t.br, body)
	}
}

// ============ Ultra-fast Take ============

var (
	patternOpAdd = []byte(`"op":"add"`)
	patternID    = []byte(`"id":"`)
	patternAmt   = []byte(`"in_amount":"`)
)

func ultraFastTake(data []byte, wsID int, detectTime time.Time, minCents int64) {
	if pauseTaking.Load() {
		return
	}

	if !bytes.Contains(data, patternOpAdd) {
		return
	}

	idx := bytes.Index(data, patternID)
	if idx == -1 {
		return
	}

	start := idx + 6
	end := bytes.IndexByte(data[start:], '"')
	if end == -1 || end > 30 {
		return
	}

	orderID := data[start : start+end]
	orderIDStr := string(orderID)

	if !markSeen(orderIDStr) {
		return
	}

	totalSeen.Add(1)

	var amt string
	if idx := bytes.Index(data, patternAmt); idx != -1 {
		s := idx + 13
		e := bytes.IndexByte(data[s:], '"')
		if e != -1 && e < 20 {
			amt = string(data[s : s+e])
		}
	}

	if minCents > 0 && amt != "" {
		cents := parseCents(amt)
		if cents < minCents {
			return
		}
	}

	// Собираем доступные takers
	fireTime := time.Now()

	var available []*taker
	for _, t := range takers {
		if t.ready.Load() && t.inUse.CompareAndSwap(false, true) {
			available = append(available, t)
		}
	}

	if len(available) == 0 {
		totalLate.Add(1)
		fmt.Printf("   [WS%02d] NO TAKERS amt=%s\n", wsID, amt)
		return
	}

	// Fire все параллельно
	var wg sync.WaitGroup
	for _, t := range available {
		wg.Add(1)
		go func(tk *taker) {
			defer wg.Done()
			tk.mu.Lock()
			if tk.conn != nil {
				tk.conn.SetWriteDeadline(time.Now().Add(50 * time.Millisecond))
				tk.bw.Write(reqPrefix)
				tk.bw.Write(orderID)
				tk.bw.Write(reqSuffix)
				tk.bw.Flush()
			}
			tk.mu.Unlock()
		}(t)
	}
	wg.Wait()

	fireLatency := time.Since(fireTime).Microseconds()

	// Читаем ответы ПАРАЛЛЕЛЬНО
	type result struct {
		id   int
		code int
		err  bool
		dur  time.Duration
	}

	resultCh := make(chan result, len(available))

	for _, t := range available {
		go func(tk *taker) {
			start := time.Now()
			tk.mu.Lock()

			if tk.conn == nil {
				tk.mu.Unlock()
				tk.inUse.Store(false)
				resultCh <- result{tk.id, 0, true, time.Since(start)}
				go tk.connect()
				return
			}

			tk.conn.SetReadDeadline(time.Now().Add(2 * time.Second))
			line, err := tk.br.ReadString('\n')

			if err != nil || len(line) < 12 {
				tk.conn.Close()
				tk.conn = nil
				tk.ready.Store(false)
				tk.mu.Unlock()
				tk.inUse.Store(false)
				resultCh <- result{tk.id, 0, true, time.Since(start)}
				go tk.connect()
				return
			}

			code, _ := strconv.Atoi(line[9:12])

			// Drain
			for {
				l, _ := tk.br.ReadString('\n')
				if l == "\r\n" || l == "" {
					break
				}
			}

			tk.mu.Unlock()
			tk.inUse.Store(false)
			resultCh <- result{tk.id, code, false, time.Since(start)}
		}(t)
	}

	// Собираем результаты
	var results []result
	for range available {
		results = append(results, <-resultCh)
	}

	e2e := time.Since(detectTime).Milliseconds()

	var parts []string
	var won bool
	for _, r := range results {
		if r.err {
			parts = append(parts, fmt.Sprintf("T%d:ERR", r.id))
		} else if r.code == 200 {
			parts = append(parts, fmt.Sprintf("T%d:OK", r.id))
			won = true
		} else {
			parts = append(parts, fmt.Sprintf("T%d:%d", r.id, r.code))
		}
	}

	if won {
		totalWon.Add(1)
		fmt.Printf("✅ [WS%02d] e2e=%dms fire=%dμs amt=%s | %s\n",
			wsID, e2e, fireLatency, amt, strings.Join(parts, " "))
		pauseTaking.Store(true)
		go func() {
			time.Sleep(pauseSeconds * time.Second)
			pauseTaking.Store(false)
			fmt.Println("▶ Resumed")
		}()
	} else {
		totalLate.Add(1)
		fmt.Printf("   [WS%02d] LATE e2e=%dms fire=%dμs amt=%s | %s\n",
			wsID, e2e, fireLatency, amt, strings.Join(parts, " "))
	}
}

func parseCents(amt string) int64 {
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

	return whole*100 + frac
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
				if len(data) > 10 && data[0] == '4' && data[1] == '2' {
					ultraFastTake(data[2:], wsID, detectTime, minCents)
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
	fmt.Println("║  P2C SNIPER - Warmup 900ms                ║")
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

	// Create takers
	fmt.Printf("\n⏳ Creating %d takers...\n", numTakers)
	for i := 0; i < numTakers; i++ {
		t := &taker{id: i + 1}
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

	// Warmup goroutines - 900ms interval
	for i, t := range takers {
		go func(idx int, tk *taker) {
			time.Sleep(time.Duration(idx*180) * time.Millisecond) // Stagger
			for {
				time.Sleep(warmupMs * time.Millisecond)
				if tk.ready.Load() {
					tk.warmup()
				} else {
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
	fmt.Printf("  %d WS | %d takers | warmup %dms\n", numWebSockets, numTakers, warmupMs)
	if minCents > 0 {
		fmt.Printf("  MIN: %.2f RUB\n", float64(minCents)/100)
	}
	fmt.Println("════════════════════════════════════════════\n")

	// Stats
	go func() {
		for {
			time.Sleep(60 * time.Second)
			s, w, l := totalSeen.Load(), totalWon.Load(), totalLate.Load()
			rate := float64(w) / float64(max(s, 1)) * 100
			fmt.Printf("\n📊 STATS: seen=%d won=%d late=%d (%.1f%%)\n\n", s, w, l, rate)
		}
	}()

	select {}
}

func max(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
