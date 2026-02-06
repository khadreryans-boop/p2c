package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/binary"
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
	numWebSockets = 30 // Много WS для надёжности
	numTakers     = 20 // Много HTTP клиентов
	parallelTakes = 5
	warmupMs      = 500 // Warmup каждые 500ms - держим горячими
)

var (
	pauseTaking atomic.Bool
	seenOrders  sync.Map
)

var (
	cookie    string
	reqPrefix []byte
	reqSuffix []byte
	popIPs    []string
)

// ============ HTTP Taker ============

type httpTaker struct {
	conn    net.Conn
	br      *bufio.Reader
	bw      *bufio.Writer
	mu      sync.Mutex
	ready   atomic.Bool
	name    string
	ip      string
	inUse   atomic.Bool
	lastRtt atomic.Uint64
	wins    atomic.Uint64
	reqs    atomic.Uint64
}

var takers []*httpTaker

func newTaker(name, ip string) *httpTaker {
	t := &httpTaker{name: name, ip: ip}
	t.ready.Store(false)
	t.lastRtt.Store(999999)
	return t
}

func (t *httpTaker) connect() error {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.conn != nil {
		t.conn.Close()
		t.conn = nil
	}

	dialer := &net.Dialer{
		Timeout:   2 * time.Second,
		KeepAlive: 5 * time.Second,
	}

	conn, err := dialer.Dial("tcp", t.ip+":443")
	if err != nil {
		return err
	}

	if tc, ok := conn.(*net.TCPConn); ok {
		tc.SetNoDelay(true)
		tc.SetKeepAlive(true)
		tc.SetKeepAlivePeriod(5 * time.Second)
	}

	tlsConn := tls.Client(conn, &tls.Config{
		ServerName: host,
		MinVersion: tls.VersionTLS12,
	})
	if err := tlsConn.Handshake(); err != nil {
		conn.Close()
		return err
	}

	t.conn = tlsConn
	t.br = bufio.NewReaderSize(tlsConn, 4096)
	t.bw = bufio.NewWriterSize(tlsConn, 2048)
	t.ready.Store(true)
	return nil
}

func (t *httpTaker) warmup() (time.Duration, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.conn == nil {
		return 0, fmt.Errorf("no conn")
	}

	t.conn.SetDeadline(time.Now().Add(1 * time.Second))

	req := "HEAD /p2c/orders HTTP/1.1\r\nHost: " + host + "\r\nCookie: " + cookie + "\r\n\r\n"

	start := time.Now()
	t.bw.WriteString(req)
	t.bw.Flush()

	line, err := t.br.ReadString('\n')
	dur := time.Since(start)

	if err != nil {
		return dur, err
	}
	_ = line

	for {
		line, err = t.br.ReadString('\n')
		if err != nil || line == "\r\n" {
			break
		}
	}

	t.lastRtt.Store(uint64(dur.Milliseconds()))
	return dur, nil
}

func (t *httpTaker) take(orderID string) (int, time.Duration, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.conn == nil {
		return 0, 0, fmt.Errorf("no conn")
	}

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

	// Drain response
	contentLen := 0
	for {
		line, err := t.br.ReadString('\n')
		if err != nil || line == "\r\n" {
			break
		}
		if strings.HasPrefix(strings.ToLower(line), "content-length:") {
			fmt.Sscanf(line[15:], "%d", &contentLen)
		}
	}
	if contentLen > 0 && contentLen < 4096 {
		tmp := make([]byte, contentLen)
		io.ReadFull(t.br, tmp)
	}

	t.lastRtt.Store(uint64(dur.Milliseconds()))
	t.reqs.Add(1)

	return code, dur, nil
}

func getBestTakers(count int) []*httpTaker {
	type ts struct {
		t   *httpTaker
		rtt uint64
	}

	var avail []ts
	for _, t := range takers {
		if t.ready.Load() && !t.inUse.Load() {
			avail = append(avail, ts{t, t.lastRtt.Load()})
		}
	}

	sort.Slice(avail, func(i, j int) bool {
		return avail[i].rtt < avail[j].rtt
	})

	var result []*httpTaker
	for i := 0; i < len(avail) && i < count; i++ {
		result = append(result, avail[i].t)
	}
	return result
}

// ============ Take Logic ============

func instantTake(id, amt string, wsID int, wsTime time.Time) {
	if pauseTaking.Load() {
		return
	}

	if _, loaded := seenOrders.LoadOrStore(id, struct{}{}); loaded {
		return
	}

	best := getBestTakers(parallelTakes)
	if len(best) == 0 {
		fmt.Printf("   ❌ NO TAKERS\n")
		return
	}

	fmt.Printf("📥 [WS%02d] %s amt=%s\n", wsID, id[:16], amt)

	type result struct {
		t    *httpTaker
		code int
		dur  time.Duration
		err  error
	}

	results := make(chan result, len(best))
	var wg sync.WaitGroup

	for _, t := range best {
		t.inUse.Store(true)
		wg.Add(1)
		go func(tk *httpTaker) {
			defer wg.Done()
			defer tk.inUse.Store(false)
			code, dur, err := tk.take(id)
			if err != nil {
				go tk.connect()
			}
			results <- result{tk, code, dur, err}
		}(t)
	}

	go func() {
		wg.Wait()
		close(results)
	}()

	var all []result
	for r := range results {
		all = append(all, r)
	}

	sort.Slice(all, func(i, j int) bool {
		return all[i].dur < all[j].dur
	})

	e2e := time.Since(wsTime).Milliseconds()

	var winner *result
	var details []string

	for _, r := range all {
		if r.err != nil {
			details = append(details, r.t.name+":ERR")
			continue
		}
		s := fmt.Sprintf("%s:%d", r.t.name, r.dur.Milliseconds())
		if r.code == 200 {
			s += "✓"
			if winner == nil {
				winner = &r
				r.t.wins.Add(1)
			}
		} else {
			s += fmt.Sprintf("(%d)", r.code)
		}
		details = append(details, s)
	}

	if winner != nil {
		fmt.Printf("✅ TAKEN e2e=%dms | %s\n", e2e, strings.Join(details, " "))
		pauseTaking.Store(true)
		go func() {
			time.Sleep(pauseSeconds * time.Second)
			pauseTaking.Store(false)
			fmt.Println("▶ Resumed")
		}()
		return
	}

	fmt.Printf("   LATE e2e=%dms | %s\n", e2e, strings.Join(details, " "))

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

func parse(msg []byte, wsTime time.Time, wsID int, minCents int64) {
	if !bytes.Contains(msg, opAddBytes) {
		return
	}

	idx := bytes.Index(msg, idPrefix)
	if idx == -1 {
		return
	}
	start := idx + 6
	end := bytes.IndexByte(msg[start:], '"')
	if end == -1 || end > 30 {
		return
	}
	id := string(msg[start : start+end])

	var amt string
	idx = bytes.Index(msg, amtPrefix)
	if idx != -1 {
		start = idx + 13
		end = bytes.IndexByte(msg[start:], '"')
		if end != -1 && end < 20 {
			amt = string(msg[start : start+end])
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

	// Вызываем синхронно для минимальной задержки!
	instantTake(id, amt, wsID, wsTime)
}

// ============ WebSocket ============

func connectWS(ip string) (net.Conn, error) {
	dialer := ws.Dialer{
		Header: ws.HandshakeHeaderHTTP(http.Header{
			"Cookie": []string{cookie},
			"Origin": []string{"https://app.send.tg"},
		}),
		Timeout: 10 * time.Second,
		NetDial: func(ctx context.Context, network, addr string) (net.Conn, error) {
			d := &net.Dialer{Timeout: 5 * time.Second}
			conn, err := d.DialContext(ctx, network, ip+":443")
			if err != nil {
				return nil, err
			}
			if tc, ok := conn.(*net.TCPConn); ok {
				tc.SetNoDelay(true)
			}
			return conn, nil
		},
		TLSConfig: &tls.Config{
			ServerName: host,
			MinVersion: tls.VersionTLS12,
		},
	}
	conn, _, _, err := dialer.Dial(context.Background(), "wss://"+host+wsPath)
	return conn, err
}

type wsConn struct {
	conn net.Conn
	mu   sync.Mutex
}

func (w *wsConn) write(msg []byte) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	frame := ws.NewTextFrame(msg)
	frame = ws.MaskFrameInPlace(frame)
	return ws.WriteFrame(w.conn, frame)
}

func readFrame(conn net.Conn) ([]byte, ws.OpCode, error) {
	h, err := ws.ReadHeader(conn)
	if err != nil {
		return nil, 0, err
	}
	p := make([]byte, h.Length)
	if h.Length > 0 {
		_, err = io.ReadFull(conn, p)
		if err != nil {
			return nil, 0, err
		}
	}
	if h.Masked {
		ws.Cipher(p, h.Mask, 0)
	}
	return p, h.OpCode, nil
}

func runWS(wsID int, ip string, minCents int64, wg *sync.WaitGroup) {
	defer wg.Done()
	ipShort := ip[strings.LastIndex(ip, ".")+1:]

	for {
		conn, err := connectWS(ip)
		if err != nil {
			time.Sleep(1 * time.Second)
			continue
		}

		wsc := &wsConn{conn: conn}

		// Handshake
		p, op, err := readFrame(conn)
		if err != nil || op == ws.OpClose {
			conn.Close()
			continue
		}
		_ = p

		wsc.write([]byte("40"))
		p, op, err = readFrame(conn)
		if err != nil || op == ws.OpClose {
			conn.Close()
			continue
		}

		time.Sleep(20 * time.Millisecond)
		wsc.write([]byte(`42["list:initialize"]`))
		time.Sleep(20 * time.Millisecond)
		wsc.write([]byte(`42["list:snapshot",[]]`))

		fmt.Printf("[WS%02d@%s] 🚀\n", wsID, ipShort)

		for {
			p, op, err := readFrame(conn)
			t := time.Now()

			if err != nil {
				break
			}

			switch op {
			case ws.OpText:
				if len(p) == 1 && p[0] == '2' {
					wsc.write([]byte("3"))
					continue
				}
				if len(p) == 1 && p[0] == '3' {
					continue
				}
				if len(p) > 2 && p[0] == '4' && p[1] == '2' {
					// Парсим немедленно в этой горутине!
					parse(p[2:], t, wsID, minCents)
				}
			case ws.OpPing:
				f := ws.NewPongFrame(p)
				f = ws.MaskFrameInPlace(f)
				ws.WriteFrame(conn, f)
			case ws.OpClose:
				goto reconn
			}
		}

	reconn:
		conn.Close()
		time.Sleep(500 * time.Millisecond)
	}
}

func parseCloseReason(p []byte) (uint16, string) {
	if len(p) >= 2 {
		return binary.BigEndian.Uint16(p[:2]), string(p[2:])
	}
	return 0, ""
}

func printStats() {
	fmt.Println("\n📊 STATS:")
	type ts struct {
		name string
		rtt  uint64
		reqs uint64
		wins uint64
	}
	var stats []ts
	for _, t := range takers {
		stats = append(stats, ts{t.name, t.lastRtt.Load(), t.reqs.Load(), t.wins.Load()})
	}
	sort.Slice(stats, func(i, j int) bool {
		return stats[i].rtt < stats[j].rtt
	})
	for i, s := range stats {
		if i >= 10 {
			break
		}
		fmt.Printf("  %s: rtt=%dms reqs=%d wins=%d\n", s.name, s.rtt, s.reqs, s.wins)
	}
}

func main() {
	in := bufio.NewReader(os.Stdin)

	fmt.Println("╔═══════════════════════════════════════════╗")
	fmt.Println("║  P2C SNIPER - Hybrid WS + Fast HTTP       ║")
	fmt.Println("╚═══════════════════════════════════════════╝")

	fmt.Print("\naccess_token cookie:\n> ")
	cookie, _ = in.ReadString('\n')
	cookie = strings.TrimSpace(cookie)
	if !strings.HasPrefix(cookie, "access_token=") {
		fmt.Println("Invalid")
		return
	}

	fmt.Print("MIN amount (0=none):\n> ")
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
	popIPs = ips
	fmt.Printf("✅ Found %d POPs: %v\n", len(popIPs), popIPs)

	reqPrefix = []byte("POST " + takePathPrefix)
	reqSuffix = []byte(" HTTP/1.1\r\n" +
		"Host: " + host + "\r\n" +
		"Content-Type: application/json\r\n" +
		"Cookie: " + cookie + "\r\n" +
		"Content-Length: 2\r\n" +
		"\r\n{}")

	// Create takers
	fmt.Printf("\n⏳ Creating %d HTTP takers...\n", numTakers)
	for i := 0; i < numTakers; i++ {
		ip := popIPs[i%len(popIPs)]
		name := fmt.Sprintf("T%d", i+1)
		t := newTaker(name, ip)
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

	// Ultra-aggressive warmup every 500ms
	for i, t := range takers {
		go func(idx int, tk *httpTaker) {
			time.Sleep(time.Duration(idx*25) * time.Millisecond)
			for {
				time.Sleep(warmupMs * time.Millisecond)
				if tk.inUse.Load() {
					continue
				}
				if !tk.ready.Load() {
					tk.connect()
					continue
				}
				dur, err := tk.warmup()
				if err != nil || dur > 300*time.Millisecond {
					tk.ready.Store(false)
					tk.connect()
				}
			}
		}(i, t)
	}

	go func() {
		for {
			time.Sleep(30 * time.Second)
			printStats()
		}
	}()

	// Start WebSockets
	fmt.Printf("\n⏳ Starting %d WebSockets...\n", numWebSockets)
	var wsWg sync.WaitGroup
	for i := 1; i <= numWebSockets; i++ {
		ip := popIPs[(i-1)%len(popIPs)]
		wsWg.Add(1)
		go runWS(i, ip, minCents, &wsWg)
		time.Sleep(30 * time.Millisecond)
	}

	fmt.Println("\n════════════════════════════════════════════")
	fmt.Printf("  %d WS | %d HTTP | warmup every %dms\n", numWebSockets, numTakers, warmupMs)
	fmt.Println("════════════════════════════════════════════\n")

	wsWg.Wait()
}
