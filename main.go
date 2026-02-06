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
	pollPath       = "/internal/v1/p2c-socket/?EIO=4&transport=polling"
	takePathPrefix = "/internal/v1/p2c/payments/take/"
	pauseSeconds   = 20
)

const (
	numWebSockets = 10
	numPollers    = 10
	numTakers     = 15
	parallelTakes = 5
)

var (
	pauseTaking atomic.Bool
	seenOrders  sync.Map
	cookie      string
	reqPrefix   []byte
	reqSuffix   []byte
	popIPs      []string
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
	ip      string
	id      int
}

var takers []*taker

func (t *taker) connect() error {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.conn != nil {
		t.conn.Close()
	}

	conn, err := net.DialTimeout("tcp", t.ip+":443", 2*time.Second)
	if err != nil {
		return err
	}

	if tc, ok := conn.(*net.TCPConn); ok {
		tc.SetNoDelay(true)
	}

	tlsConn := tls.Client(conn, &tls.Config{ServerName: host})
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

func (t *taker) take(orderID string) (int, time.Duration, error) {
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

func (t *taker) warmup() {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.conn == nil {
		return
	}

	t.conn.SetDeadline(time.Now().Add(1 * time.Second))
	t.bw.WriteString("HEAD / HTTP/1.1\r\nHost: " + host + "\r\n\r\n")
	t.bw.Flush()
	t.br.ReadString('\n')
	for {
		line, _ := t.br.ReadString('\n')
		if line == "\r\n" || line == "" {
			break
		}
	}
}

func getBest(n int) []*taker {
	type tr struct {
		t   *taker
		rtt uint64
	}
	var avail []tr
	for _, t := range takers {
		if t.ready.Load() && !t.inUse.Load() {
			avail = append(avail, tr{t, t.lastRtt.Load()})
		}
	}
	sort.Slice(avail, func(i, j int) bool { return avail[i].rtt < avail[j].rtt })

	var res []*taker
	for i := 0; i < len(avail) && i < n; i++ {
		res = append(res, avail[i].t)
	}
	return res
}

// ============ Take Logic ============

func doTake(id, amt, source string, detectTime time.Time) {
	if pauseTaking.Load() {
		return
	}
	if _, loaded := seenOrders.LoadOrStore(id, struct{}{}); loaded {
		return
	}

	best := getBest(parallelTakes)
	if len(best) == 0 {
		return
	}

	type res struct {
		id   int
		code int
		dur  time.Duration
		err  error
	}

	ch := make(chan res, len(best))
	for _, t := range best {
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
	for i := 0; i < len(best); i++ {
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
		} else {
			s += fmt.Sprintf("(%d)", r.code)
		}
		parts = append(parts, s)
	}

	if won {
		fmt.Printf("✅ [%s] e2e=%dms amt=%s | %s\n", source, e2e, amt, strings.Join(parts, " "))
		pauseTaking.Store(true)
		go func() {
			time.Sleep(pauseSeconds * time.Second)
			pauseTaking.Store(false)
			fmt.Println("▶ Resumed")
		}()
	} else {
		fmt.Printf("   [%s] LATE e2e=%dms amt=%s | %s\n", source, e2e, amt, strings.Join(parts, " "))
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

func parseAndTake(data []byte, source string, detectTime time.Time, minCents int64) {
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

	doTake(id, amt, source, detectTime)
}

// ============ WebSocket ============

func runWS(wsID int, ip string, minCents int64) {
	source := fmt.Sprintf("WS%02d", wsID)

	for {
		conn, err := connectWS(ip)
		if err != nil {
			time.Sleep(1 * time.Second)
			continue
		}

		// Handshake
		readFrame(conn)
		writeFrame(conn, []byte("40"))
		readFrame(conn)

		time.Sleep(20 * time.Millisecond)
		writeFrame(conn, []byte(`42["list:initialize"]`))
		time.Sleep(20 * time.Millisecond)
		writeFrame(conn, []byte(`42["list:snapshot",[]]`))

		fmt.Printf("[%s] 🚀\n", source)

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
					parseAndTake(data[2:], source, detectTime, minCents)
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
		time.Sleep(500 * time.Millisecond)
	}
}

func connectWS(ip string) (net.Conn, error) {
	dialer := ws.Dialer{
		Header: ws.HandshakeHeaderHTTP(http.Header{
			"Cookie": []string{cookie},
			"Origin": []string{"https://app.send.tg"},
		}),
		Timeout: 10 * time.Second,
		NetDial: func(ctx context.Context, network, addr string) (net.Conn, error) {
			conn, err := net.DialTimeout("tcp", ip+":443", 5*time.Second)
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

// ============ HTTP Polling ============

func runPoll(pollID int, ip string, minCents int64) {
	source := fmt.Sprintf("P%02d", pollID)

	for {
		conn, br, bw, sid, err := pollConnect(ip)
		if err != nil {
			time.Sleep(1 * time.Second)
			continue
		}

		fmt.Printf("[%s] 🚀\n", source)

		for {
			data, err := pollRequest(conn, br, bw, sid)
			detectTime := time.Now()

			if err != nil {
				break
			}

			if len(data) == 1 && data[0] == '2' {
				pollSend(conn, bw, sid, "3")
				continue
			}

			if len(data) > 5 {
				parseAndTake(data, source, detectTime, minCents)
			}
		}

		conn.Close()
		time.Sleep(500 * time.Millisecond)
	}
}

func pollConnect(ip string) (net.Conn, *bufio.Reader, *bufio.Writer, string, error) {
	conn, err := net.DialTimeout("tcp", ip+":443", 5*time.Second)
	if err != nil {
		return nil, nil, nil, "", err
	}

	if tc, ok := conn.(*net.TCPConn); ok {
		tc.SetNoDelay(true)
	}

	tlsConn := tls.Client(conn, &tls.Config{ServerName: host})
	if err := tlsConn.Handshake(); err != nil {
		conn.Close()
		return nil, nil, nil, "", err
	}

	br := bufio.NewReaderSize(tlsConn, 16384)
	bw := bufio.NewWriterSize(tlsConn, 4096)

	// Handshake
	req := "GET " + pollPath + " HTTP/1.1\r\nHost: " + host + "\r\nCookie: " + cookie + "\r\nConnection: keep-alive\r\n\r\n"
	bw.WriteString(req)
	bw.Flush()

	body, _ := readHTTP(br)

	sidIdx := bytes.Index(body, []byte(`"sid":"`))
	if sidIdx == -1 {
		tlsConn.Close()
		return nil, nil, nil, "", fmt.Errorf("no sid")
	}
	sidStart := sidIdx + 7
	sidEnd := bytes.IndexByte(body[sidStart:], '"')
	sid := string(body[sidStart : sidStart+sidEnd])

	// Socket.IO connect
	pollSend(tlsConn, bw, sid, "40")
	pollRequest(tlsConn, br, bw, sid)

	time.Sleep(20 * time.Millisecond)
	pollSend(tlsConn, bw, sid, `42["list:initialize"]`)
	time.Sleep(20 * time.Millisecond)
	pollSend(tlsConn, bw, sid, `42["list:snapshot",[]]`)

	return tlsConn, br, bw, sid, nil
}

func pollSend(conn net.Conn, bw *bufio.Writer, sid, msg string) {
	conn.SetDeadline(time.Now().Add(5 * time.Second))
	req := fmt.Sprintf("POST %s&sid=%s HTTP/1.1\r\nHost: %s\r\nCookie: %s\r\nContent-Type: text/plain\r\nContent-Length: %d\r\nConnection: keep-alive\r\n\r\n%s",
		pollPath, sid, host, cookie, len(msg), msg)
	bw.WriteString(req)
	bw.Flush()
	// Read response
	br := bufio.NewReader(conn)
	readHTTP(br)
}

func pollRequest(conn net.Conn, br *bufio.Reader, bw *bufio.Writer, sid string) ([]byte, error) {
	conn.SetDeadline(time.Now().Add(30 * time.Second))
	req := fmt.Sprintf("GET %s&sid=%s HTTP/1.1\r\nHost: %s\r\nCookie: %s\r\nConnection: keep-alive\r\n\r\n",
		pollPath, sid, host, cookie)
	bw.WriteString(req)
	bw.Flush()
	return readHTTP(br)
}

func readHTTP(br *bufio.Reader) ([]byte, error) {
	line, err := br.ReadString('\n')
	if err != nil {
		return nil, err
	}
	_ = line

	contentLen := 0
	chunked := false
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return nil, err
		}
		if line == "\r\n" {
			break
		}
		lower := strings.ToLower(line)
		if strings.HasPrefix(lower, "content-length:") {
			fmt.Sscanf(line[15:], "%d", &contentLen)
		}
		if strings.Contains(lower, "chunked") {
			chunked = true
		}
	}

	var body []byte
	if chunked {
		for {
			sizeLine, _ := br.ReadString('\n')
			sizeLine = strings.TrimSpace(sizeLine)
			size, _ := strconv.ParseInt(sizeLine, 16, 64)
			if size == 0 {
				br.ReadString('\n')
				break
			}
			chunk := make([]byte, size)
			io.ReadFull(br, chunk)
			body = append(body, chunk...)
			br.ReadString('\n')
		}
	} else if contentLen > 0 {
		body = make([]byte, contentLen)
		io.ReadFull(br, body)
	}

	return body, nil
}

// ============ Main ============

func main() {
	in := bufio.NewReader(os.Stdin)

	fmt.Println("╔═══════════════════════════════════════════╗")
	fmt.Println("║  P2C SNIPER - WS + POLL Hybrid            ║")
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
	reqSuffix = []byte(" HTTP/1.1\r\nHost: " + host + "\r\nContent-Type: application/json\r\nCookie: " + cookie + "\r\nContent-Length: 2\r\n\r\n{}")

	// Create takers
	fmt.Printf("\n⏳ Creating %d takers...\n", numTakers)
	for i := 0; i < numTakers; i++ {
		t := &taker{ip: popIPs[i%len(popIPs)], id: i + 1}
		t.connect()
		takers = append(takers, t)
	}

	// Warmup
	for _, t := range takers {
		go func(tk *taker) {
			for {
				time.Sleep(500 * time.Millisecond)
				if !tk.inUse.Load() && tk.ready.Load() {
					tk.warmup()
				} else if !tk.ready.Load() {
					tk.connect()
				}
			}
		}(t)
	}

	// Start WebSockets
	fmt.Printf("⏳ Starting %d WebSockets...\n", numWebSockets)
	for i := 1; i <= numWebSockets; i++ {
		go runWS(i, popIPs[(i-1)%len(popIPs)], minCents)
		time.Sleep(50 * time.Millisecond)
	}

	// Start Pollers
	fmt.Printf("⏳ Starting %d Pollers...\n", numPollers)
	for i := 1; i <= numPollers; i++ {
		go runPoll(i, popIPs[(i-1)%len(popIPs)], minCents)
		time.Sleep(50 * time.Millisecond)
	}

	fmt.Println("\n════════════════════════════════════════════")
	fmt.Printf("  %d WS + %d POLL | %d takers | Hybrid mode\n", numWebSockets, numPollers, numTakers)
	fmt.Println("  Whoever sees order first - triggers take!")
	fmt.Println("════════════════════════════════════════════\n")

	select {}
}
