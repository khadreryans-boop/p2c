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

const numClients = 15
const numWebSockets = 5
const parallelTakes = 5

var (
	pauseTaking atomic.Bool
	seenOrders  sync.Map
)

// Pre-built request parts (no allocation during take)
var (
	reqPrefix  []byte
	reqMiddle  []byte
	reqSuffix  []byte
	cookieData []byte
)

// ============ HTTP Client Pool ============

type httpClient struct {
	conn     net.Conn
	br       *bufio.Reader
	bw       *bufio.Writer
	mu       sync.Mutex
	ready    atomic.Bool
	name     string
	lastUsed time.Time
	inUse    atomic.Bool
	lastRtt  atomic.Uint64

	totalRequests atomic.Uint64
	totalLatency  atomic.Uint64
	minLatency    atomic.Uint64
	maxLatency    atomic.Uint64
	wins          atomic.Uint64
}

var clients [numClients]*httpClient

func newHTTPClient(name string) *httpClient {
	c := &httpClient{name: name}
	c.ready.Store(false)
	c.minLatency.Store(999999999)
	c.lastRtt.Store(999999)
	return c
}

func (c *httpClient) connect() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.conn != nil {
		c.conn.Close()
		c.conn = nil
	}

	dialer := &net.Dialer{
		Timeout:   2 * time.Second,
		KeepAlive: 5 * time.Second,
	}

	tlsConfig := &tls.Config{
		ServerName:         host,
		InsecureSkipVerify: false,
		MinVersion:         tls.VersionTLS12,
		MaxVersion:         tls.VersionTLS12, // TLS 1.2 faster handshake than 1.3
	}

	conn, err := tls.DialWithDialer(dialer, "tcp", host+":443", tlsConfig)
	if err != nil {
		return err
	}

	if tcpConn, ok := conn.NetConn().(*net.TCPConn); ok {
		tcpConn.SetNoDelay(true)
		tcpConn.SetKeepAlive(true)
		tcpConn.SetKeepAlivePeriod(5 * time.Second)
		tcpConn.SetWriteBuffer(4096)
		tcpConn.SetReadBuffer(4096)
	}

	c.conn = conn
	c.br = bufio.NewReaderSize(conn, 2048)
	c.bw = bufio.NewWriterSize(conn, 1024)
	c.lastUsed = time.Now()
	c.ready.Store(true)
	return nil
}

func (c *httpClient) warmup() (time.Duration, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.conn == nil {
		return 0, fmt.Errorf("no conn")
	}

	c.conn.SetDeadline(time.Now().Add(1500 * time.Millisecond))

	// Minimal warmup request
	c.bw.Write(reqPrefix)
	c.bw.WriteString("000000000000000000000000") // fake ID
	c.bw.Write(reqSuffix)

	start := time.Now()
	if err := c.bw.Flush(); err != nil {
		return 0, err
	}

	line, err := c.br.ReadString('\n')
	dur := time.Since(start)
	if err != nil {
		return dur, err
	}
	_ = line

	// Drain response
	for {
		line, err = c.br.ReadString('\n')
		if err != nil || line == "\r\n" {
			break
		}
	}
	// Read small body (error response)
	tmp := make([]byte, 256)
	c.br.Read(tmp)

	c.lastUsed = time.Now()
	c.lastRtt.Store(uint64(dur.Milliseconds()))
	return dur, nil
}

func (c *httpClient) doTake(orderID string) (int, time.Duration, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.conn == nil {
		return 0, 0, fmt.Errorf("no conn")
	}

	c.conn.SetDeadline(time.Now().Add(1500 * time.Millisecond))

	// Write request with pre-built parts
	c.bw.Write(reqPrefix)
	c.bw.WriteString(orderID)
	c.bw.Write(reqSuffix)

	start := time.Now()
	if err := c.bw.Flush(); err != nil {
		c.conn.Close()
		c.conn = nil
		c.ready.Store(false)
		return 0, 0, err
	}

	line, err := c.br.ReadString('\n')
	dur := time.Since(start)

	if err != nil {
		c.conn.Close()
		c.conn = nil
		c.ready.Store(false)
		return 0, dur, err
	}

	if len(line) < 12 {
		c.conn.Close()
		c.conn = nil
		c.ready.Store(false)
		return 0, dur, fmt.Errorf("short")
	}
	code, _ := strconv.Atoi(line[9:12])

	// Drain headers
	contentLen := 0
	for {
		line, err := c.br.ReadString('\n')
		if err != nil || line == "\r\n" {
			break
		}
		if len(line) > 16 && (line[0] == 'C' || line[0] == 'c') {
			fmt.Sscanf(line, "Content-Length: %d", &contentLen)
		}
	}

	// Drain body
	if contentLen > 0 && contentLen < 4096 {
		tmp := make([]byte, contentLen)
		io.ReadFull(c.br, tmp)
	}

	c.lastUsed = time.Now()
	c.lastRtt.Store(uint64(dur.Milliseconds()))

	c.totalRequests.Add(1)
	latMs := uint64(dur.Milliseconds())
	c.totalLatency.Add(latMs)

	for {
		old := c.minLatency.Load()
		if latMs >= old || c.minLatency.CompareAndSwap(old, latMs) {
			break
		}
	}
	for {
		old := c.maxLatency.Load()
		if latMs <= old || c.maxLatency.CompareAndSwap(old, latMs) {
			break
		}
	}

	return code, dur, nil
}

func getBestClients(count int) []*httpClient {
	type cs struct {
		c   *httpClient
		rtt uint64
	}

	var avail []cs
	for i := 0; i < numClients; i++ {
		c := clients[i]
		if c.ready.Load() && !c.inUse.Load() {
			avail = append(avail, cs{c, c.lastRtt.Load()})
		}
	}

	sort.Slice(avail, func(i, j int) bool {
		return avail[i].rtt < avail[j].rtt
	})

	var result []*httpClient
	for i := 0; i < len(avail) && i < count; i++ {
		result = append(result, avail[i].c)
	}
	return result
}

// ============ Take ============

type takeResult struct {
	client *httpClient
	code   int
	dur    time.Duration
	err    error
}

func instantTake(id, amt string, wsID int, wsTime time.Time) {
	if pauseTaking.Load() {
		return
	}

	if _, loaded := seenOrders.LoadOrStore(id, struct{}{}); loaded {
		return
	}

	best := getBestClients(parallelTakes)
	if len(best) == 0 {
		fmt.Printf("   ❌ NO CLIENTS\n")
		return
	}

	fmt.Printf("📥 [WS%d] %s amt=%s (%d clients)\n", wsID, id, amt, len(best))

	results := make(chan takeResult, len(best))
	var wg sync.WaitGroup

	for _, c := range best {
		c.inUse.Store(true)
		wg.Add(1)
		go func(cl *httpClient) {
			defer wg.Done()
			defer cl.inUse.Store(false)
			code, dur, err := cl.doTake(id)
			if err != nil {
				go cl.connect()
			}
			results <- takeResult{cl, code, dur, err}
		}(c)
	}

	go func() {
		wg.Wait()
		close(results)
	}()

	var all []takeResult
	for r := range results {
		all = append(all, r)
	}

	sort.Slice(all, func(i, j int) bool {
		return all[i].dur < all[j].dur
	})

	e2e := time.Since(wsTime).Milliseconds()

	var winner *takeResult
	var details []string

	for _, r := range all {
		if r.err != nil {
			details = append(details, r.client.name+":ERR")
			continue
		}
		s := fmt.Sprintf("%s:%d", r.client.name, r.dur.Milliseconds())
		if r.code == 200 {
			s += "✓"
			if winner == nil {
				winner = &r
				r.client.wins.Add(1)
			}
		} else {
			s += fmt.Sprintf("(%d)", r.code)
		}
		details = append(details, s)
	}

	if winner != nil {
		fmt.Printf("✅ TAKEN e2e=%dms | %s | %s\n", e2e, strings.Join(details, " "), id)
		pauseTaking.Store(true)
		go func() {
			time.Sleep(pauseSeconds * time.Second)
			pauseTaking.Store(false)
			fmt.Println("▶ Resumed")
		}()
		return
	}

	fmt.Printf("   LATE e2e=%dms | %s | %s\n", e2e, strings.Join(details, " "), id)

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

	go instantTake(id, amt, wsID, wsTime)
}

// ============ WebSocket ============

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

func connectWS(cookie string) (net.Conn, error) {
	dialer := ws.Dialer{
		Header: ws.HandshakeHeaderHTTP(http.Header{
			"Cookie": []string{cookie},
			"Origin": []string{"https://app.send.tg"},
		}),
		Timeout: 10 * time.Second,
		NetDial: func(ctx context.Context, network, addr string) (net.Conn, error) {
			d := &net.Dialer{Timeout: 5 * time.Second}
			conn, err := d.DialContext(ctx, network, addr)
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

func runWS(wsID int, cookie string, minCents int64, wg *sync.WaitGroup) {
	defer wg.Done()

	for {
		conn, err := connectWS(cookie)
		if err != nil {
			fmt.Printf("[WS%d] err: %v\n", wsID, err)
			time.Sleep(2 * time.Second)
			continue
		}

		wsc := &wsConn{conn: conn}
		fmt.Printf("[WS%d] 🔌\n", wsID)

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

		time.Sleep(30 * time.Millisecond)
		wsc.write([]byte(`42["list:initialize"]`))
		time.Sleep(30 * time.Millisecond)
		wsc.write([]byte(`42["list:snapshot",[]]`))

		fmt.Printf("[WS%d] 🚀\n", wsID)

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
					msg := make([]byte, len(p)-2)
					copy(msg, p[2:])
					go parse(msg, t, wsID, minCents)
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
		fmt.Printf("[WS%d] 🔄\n", wsID)
		time.Sleep(1 * time.Second)
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
	type s struct {
		n    string
		rtt  uint64
		reqs uint64
		avg  uint64
		min  uint64
		max  uint64
		wins uint64
	}
	var stats []s
	for i := 0; i < numClients; i++ {
		c := clients[i]
		minL := c.minLatency.Load()
		if minL == 999999999 {
			minL = 0
		}
		total := c.totalRequests.Load()
		var avg uint64
		if total > 0 {
			avg = c.totalLatency.Load() / total
		}
		stats = append(stats, s{
			c.name, c.lastRtt.Load(), total, avg, minL, c.maxLatency.Load(), c.wins.Load(),
		})
	}
	sort.Slice(stats, func(i, j int) bool {
		return stats[i].rtt < stats[j].rtt
	})
	for _, x := range stats {
		fmt.Printf("  %s: rtt=%3d reqs=%d avg=%d min=%d max=%d wins=%d\n",
			x.n, x.rtt, x.reqs, x.avg, x.min, x.max, x.wins)
	}
}

func main() {
	in := bufio.NewReader(os.Stdin)

	fmt.Println("╔═══════════════════════════════════════════╗")
	fmt.Println("║  P2C SNIPER - Ultra Optimized             ║")
	fmt.Println("╚═══════════════════════════════════════════╝")

	fmt.Print("\naccess_token cookie:\n> ")
	cookie, _ := in.ReadString('\n')
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

	// Build minimal request template
	// POST /internal/v1/p2c/payments/take/ORDER_ID HTTP/1.1\r\n
	// Host: app.send.tg\r\n
	// Content-Type: application/json\r\n
	// Cookie: access_token=...\r\n
	// Content-Length: 2\r\n
	// \r\n
	// {}
	reqPrefix = []byte("POST " + takePathPrefix)
	reqSuffix = []byte(" HTTP/1.1\r\n" +
		"Host: " + host + "\r\n" +
		"Content-Type: application/json\r\n" +
		"Cookie: " + cookie + "\r\n" +
		"Content-Length: 2\r\n" +
		"\r\n{}")

	fmt.Printf("\n⏳ Connecting %d clients...\n", numClients)

	var connWg sync.WaitGroup
	for i := 0; i < numClients; i++ {
		clients[i] = newHTTPClient(fmt.Sprintf("C%02d", i+1))
		connWg.Add(1)
		go func(c *httpClient) {
			defer connWg.Done()
			if c.connect() == nil {
				c.warmup()
			}
		}(clients[i])
	}
	connWg.Wait()

	ready := 0
	for i := 0; i < numClients; i++ {
		if clients[i].ready.Load() {
			ready++
		}
	}
	fmt.Printf("✅ %d/%d ready\n", ready, numClients)

	// Warmup every 500ms per client (staggered)
	for i := 0; i < numClients; i++ {
		go func(idx int) {
			c := clients[idx]
			time.Sleep(time.Duration(idx*33) * time.Millisecond)
			for {
				time.Sleep(500 * time.Millisecond)
				if c.inUse.Load() {
					continue
				}
				if !c.ready.Load() {
					c.connect()
					continue
				}
				dur, err := c.warmup()
				if err != nil || dur > 400*time.Millisecond {
					c.ready.Store(false)
					c.connect()
				}
			}
		}(i)
	}

	go func() {
		for {
			time.Sleep(30 * time.Second)
			printStats()
		}
	}()

	fmt.Printf("\n⏳ Starting %d WebSockets...\n", numWebSockets)

	var wsWg sync.WaitGroup
	for i := 1; i <= numWebSockets; i++ {
		wsWg.Add(1)
		go runWS(i, cookie, minCents, &wsWg)
		time.Sleep(80 * time.Millisecond)
	}

	fmt.Println("\n════════════════════════════════════════════")
	fmt.Println("  Warmup: 500ms | Timeout: 1.5s | Min headers")
	fmt.Println("════════════════════════════════════════════\n")

	wsWg.Wait()
}
