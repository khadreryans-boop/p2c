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
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gobwas/ws"
)

const UA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"

const (
	host           = "app.send.tg"
	wsPath         = "/internal/v1/p2c-socket/?EIO=4&transport=websocket"
	takePathPrefix = "/internal/v1/p2c/payments/take/"
	origin         = "https://app.send.tg"
	referer        = "https://app.send.tg/p2c/orders"
	pauseSeconds   = 20
)

const numClients = 5
const numWebSockets = 5
const parallelTakes = 5

var (
	pauseTaking atomic.Bool
	seenOrders  sync.Map
)

// ============ Order Event ============

type orderEvent struct {
	id      string
	amtStr  string
	wsID    int
	wsTime  time.Time
	handled atomic.Bool
}

var pendingOrders sync.Map

// ============ HTTP Client Pool ============

type httpClient struct {
	conn     net.Conn
	br       *bufio.Reader
	bw       *bufio.Writer
	mu       sync.Mutex
	ready    atomic.Bool
	ewmaUs   atomic.Uint64
	name     string
	lastUsed time.Time
	inUse    atomic.Bool
}

var (
	clients            [numClients]*httpClient
	accessCookieGlobal string
)

func newHTTPClient(name string) *httpClient {
	c := &httpClient{name: name}
	c.ready.Store(false)
	c.ewmaUs.Store(50000)
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
		Timeout:   3 * time.Second,
		KeepAlive: 15 * time.Second,
	}

	tlsConfig := &tls.Config{
		ServerName:         host,
		InsecureSkipVerify: false,
		MinVersion:         tls.VersionTLS12,
	}

	conn, err := tls.DialWithDialer(dialer, "tcp", host+":443", tlsConfig)
	if err != nil {
		return err
	}

	if tcpConn, ok := conn.NetConn().(*net.TCPConn); ok {
		_ = tcpConn.SetNoDelay(true)
		_ = tcpConn.SetKeepAlive(true)
		_ = tcpConn.SetKeepAlivePeriod(15 * time.Second)
	}

	c.conn = conn
	c.br = bufio.NewReaderSize(conn, 4096)
	c.bw = bufio.NewWriterSize(conn, 2048)
	c.lastUsed = time.Now()
	c.ready.Store(true)
	return nil
}

func (c *httpClient) warmup() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.conn == nil {
		return fmt.Errorf("no connection")
	}

	_ = c.conn.SetDeadline(time.Now().Add(2 * time.Second))

	req := fmt.Sprintf("HEAD /p2c/orders HTTP/1.1\r\n"+
		"Host: %s\r\n"+
		"User-Agent: %s\r\n"+
		"Cookie: %s\r\n"+
		"Connection: keep-alive\r\n"+
		"\r\n",
		host, UA, accessCookieGlobal)

	_, err := c.bw.WriteString(req)
	if err != nil {
		return err
	}

	if err := c.bw.Flush(); err != nil {
		return err
	}

	line, err := c.br.ReadString('\n')
	if err != nil {
		return err
	}
	_ = line

	for {
		line, err = c.br.ReadString('\n')
		if err != nil || line == "\r\n" {
			break
		}
	}

	c.lastUsed = time.Now()
	return nil
}

func (c *httpClient) doTake(orderID string) (int, time.Duration, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.conn == nil {
		return 0, 0, fmt.Errorf("no connection")
	}

	_ = c.conn.SetDeadline(time.Now().Add(2 * time.Second))

	req := fmt.Sprintf("POST %s%s HTTP/1.1\r\n"+
		"Host: %s\r\n"+
		"Content-Type: application/json\r\n"+
		"Accept: application/json\r\n"+
		"Origin: %s\r\n"+
		"Referer: %s\r\n"+
		"User-Agent: %s\r\n"+
		"Cookie: %s\r\n"+
		"Content-Length: 2\r\n"+
		"Connection: keep-alive\r\n"+
		"\r\n{}",
		takePathPrefix, orderID, host, origin, referer, UA, accessCookieGlobal)

	start := time.Now()
	_, err := c.bw.WriteString(req)
	if err != nil {
		c.conn.Close()
		c.conn = nil
		c.ready.Store(false)
		return 0, 0, err
	}

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
		return 0, dur, fmt.Errorf("short response")
	}
	code, _ := strconv.Atoi(line[9:12])

	contentLength := 0
	for {
		line, err := c.br.ReadString('\n')
		if err != nil {
			break
		}
		if line == "\r\n" {
			break
		}
		lower := strings.ToLower(line)
		if strings.HasPrefix(lower, "content-length:") {
			fmt.Sscanf(line[15:], "%d", &contentLength)
		}
	}

	if contentLength > 0 {
		body := make([]byte, contentLength)
		io.ReadFull(c.br, body)
	}

	c.lastUsed = time.Now()
	return code, dur, nil
}

func updateEWMA(old, x uint64) uint64 {
	if old == 0 {
		return x
	}
	return (old*7 + x*3) / 10
}

// ============ Parallel Takes ============

func parallelTake(ev *orderEvent) {
	if pauseTaking.Load() {
		return
	}

	if !ev.handled.CompareAndSwap(false, true) {
		return
	}

	type result struct {
		client *httpClient
		code   int
		dur    time.Duration
		err    error
	}

	results := make(chan result, parallelTakes)
	var wg sync.WaitGroup

	// Launch parallel takes
	for i := 0; i < parallelTakes; i++ {
		c := clients[i]
		if !c.ready.Load() {
			continue
		}

		wg.Add(1)
		go func(client *httpClient) {
			defer wg.Done()
			client.inUse.Store(true)
			code, dur, err := client.doTake(ev.id)
			client.inUse.Store(false)

			if err != nil {
				go client.connect()
			}

			results <- result{client, code, dur, err}
		}(c)
	}

	// Close results when all done
	go func() {
		wg.Wait()
		close(results)
	}()

	// Process results
	var winner *result
	var lateCount int
	var errCount int

	for r := range results {
		if r.err != nil {
			errCount++
			fmt.Printf("   ❌ %s ERR: %v\n", r.client.name, r.err)
			continue
		}

		us := uint64(r.dur.Microseconds())
		old := r.client.ewmaUs.Load()
		r.client.ewmaUs.Store(updateEWMA(old, us))

		if r.code == 200 {
			if winner == nil {
				winner = &r
			}
		} else if r.code == 400 || r.code == 404 || r.code == 409 {
			lateCount++
		}
	}

	e2eMs := time.Since(ev.wsTime).Milliseconds()

	if winner != nil {
		fmt.Printf("✅ TAKEN [WS%d] %s rtt=%dms e2e=%dms id=%s amt=%s (late=%d)\n",
			ev.wsID, winner.client.name, winner.dur.Milliseconds(), e2eMs, ev.id, ev.amtStr, lateCount)

		pauseTaking.Store(true)
		go func() {
			time.Sleep(pauseSeconds * time.Second)
			pauseTaking.Store(false)
			fmt.Println("▶ Resumed")
		}()
		return
	}

	if lateCount > 0 {
		fmt.Printf("   LATE [WS%d] e2e=%dms id=%s (%d tries)\n",
			ev.wsID, e2eMs, ev.id, lateCount)
	}
}

// ============ Speculative Parser ============

func parseDecimalToCents(s string) (int64, bool) {
	if len(s) == 0 {
		return 0, false
	}

	var whole, frac int64
	var fracDigits int
	seenDot := false

	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '.' {
			if seenDot {
				return 0, false
			}
			seenDot = true
			continue
		}
		if c < '0' || c > '9' {
			return 0, false
		}
		d := int64(c - '0')
		if !seenDot {
			whole = whole*10 + d
		} else if fracDigits < 2 {
			frac = frac*10 + d
			fracDigits++
		}
	}

	if fracDigits == 1 {
		frac *= 10
	}
	return whole*100 + frac, true
}

var (
	opAddBytes     = []byte(`"op":"add"`)
	idPrefixBytes  = []byte(`"id":"`)
	amtPrefixBytes = []byte(`"in_amount":"`)
)

func speculativeParse(msg []byte, wsTime time.Time, wsID int, minCents int64) {
	if !bytes.Contains(msg, opAddBytes) {
		return
	}

	idIdx := bytes.Index(msg, idPrefixBytes)
	if idIdx == -1 {
		return
	}
	idStart := idIdx + 6
	idEnd := bytes.IndexByte(msg[idStart:], '"')
	if idEnd == -1 || idEnd > 30 {
		return
	}
	id := string(msg[idStart : idStart+idEnd])

	var amtStr string
	amtIdx := bytes.Index(msg, amtPrefixBytes)
	if amtIdx != -1 {
		amtStart := amtIdx + 13
		amtEnd := bytes.IndexByte(msg[amtStart:], '"')
		if amtEnd != -1 && amtEnd < 20 {
			amtStr = string(msg[amtStart : amtStart+amtEnd])
		}
	}

	if minCents > 0 && amtStr != "" {
		cents, ok := parseDecimalToCents(amtStr)
		if !ok || cents < minCents {
			return
		}
	}

	ev := &orderEvent{
		id:     id,
		amtStr: amtStr,
		wsID:   wsID,
		wsTime: wsTime,
	}

	existing, loaded := pendingOrders.LoadOrStore(id, ev)
	if loaded {
		existingEv := existing.(*orderEvent)
		delay := wsTime.Sub(existingEv.wsTime)
		if delay > time.Millisecond {
			fmt.Printf("   [WS%d] +%v (WS%d first)\n", wsID, delay.Round(100*time.Microsecond), existingEv.wsID)
		}
		return
	}

	fmt.Printf("📥 [WS%d] NEW: %s amt=%s\n", wsID, id, amtStr)
	parallelTake(ev)

	go func() {
		time.Sleep(5 * time.Second)
		pendingOrders.Delete(id)
	}()
}

// ============ WebSocket ============

type wsConn struct {
	conn net.Conn
	mu   sync.Mutex
}

func (w *wsConn) writeText(msg []byte) error {
	w.mu.Lock()
	defer w.mu.Unlock()

	frame := ws.NewTextFrame(msg)
	frame = ws.MaskFrameInPlace(frame)
	return ws.WriteFrame(w.conn, frame)
}

func connectWebSocket(cookie string) (net.Conn, error) {
	dialer := ws.Dialer{
		Header: ws.HandshakeHeaderHTTP(http.Header{
			"Host":            []string{host},
			"Origin":          []string{origin},
			"User-Agent":      []string{UA},
			"Cookie":          []string{cookie},
			"Pragma":          []string{"no-cache"},
			"Cache-Control":   []string{"no-cache"},
			"Accept-Language": []string{"ru-RU,ru;q=0.9,en-US;q=0.8,en;q=0.7"},
		}),
		Timeout: 10 * time.Second,
		NetDial: func(ctx context.Context, network, addr string) (net.Conn, error) {
			d := &net.Dialer{Timeout: 5 * time.Second}
			conn, err := d.DialContext(ctx, network, addr)
			if err != nil {
				return nil, err
			}
			if tc, ok := conn.(*net.TCPConn); ok {
				_ = tc.SetNoDelay(true)
				_ = tc.SetKeepAlive(true)
				_ = tc.SetKeepAlivePeriod(15 * time.Second)
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
	header, err := ws.ReadHeader(conn)
	if err != nil {
		return nil, 0, err
	}

	payload := make([]byte, header.Length)
	if header.Length > 0 {
		_, err = io.ReadFull(conn, payload)
		if err != nil {
			return nil, 0, err
		}
	}

	if header.Masked {
		ws.Cipher(payload, header.Mask, 0)
	}

	return payload, header.OpCode, nil
}

func parseCloseReason(payload []byte) (code uint16, reason string) {
	if len(payload) >= 2 {
		code = binary.BigEndian.Uint16(payload[:2])
		if len(payload) > 2 {
			reason = string(payload[2:])
		}
	}
	return
}

func runWebSocket(wsID int, cookie string, minCents int64, wg *sync.WaitGroup) {
	defer wg.Done()

	for {
		conn, err := connectWebSocket(cookie)
		if err != nil {
			fmt.Printf("[WS%d] Connect error: %v\n", wsID, err)
			time.Sleep(2 * time.Second)
			continue
		}

		wsc := &wsConn{conn: conn}
		fmt.Printf("[WS%d] 🔌 Connected\n", wsID)

		// Engine.IO OPEN
		payload, op, err := readFrame(conn)
		if err != nil {
			conn.Close()
			continue
		}

		if op == ws.OpClose {
			code, reason := parseCloseReason(payload)
			fmt.Printf("[WS%d] ❌ Closed: %d %s\n", wsID, code, reason)
			conn.Close()
			time.Sleep(5 * time.Second)
			continue
		}

		// Socket.IO connect
		wsc.writeText([]byte("40"))

		payload, op, err = readFrame(conn)
		if err != nil || op == ws.OpClose {
			conn.Close()
			continue
		}

		// Subscribe
		time.Sleep(50 * time.Millisecond)
		wsc.writeText([]byte(`42["list:initialize"]`))
		time.Sleep(50 * time.Millisecond)
		wsc.writeText([]byte(`42["list:snapshot",[]]`))

		fmt.Printf("[WS%d] 🚀 Active\n", wsID)

		// Main loop
		for {
			payload, op, err := readFrame(conn)
			wsTime := time.Now()

			if err != nil {
				fmt.Printf("[WS%d] Read error: %v\n", wsID, err)
				break
			}

			switch op {
			case ws.OpText:
				if len(payload) == 1 && payload[0] == '2' {
					wsc.writeText([]byte("3"))
					continue
				}

				if len(payload) == 1 && payload[0] == '3' {
					continue
				}

				if len(payload) > 2 && payload[0] == '4' && payload[1] == '2' {
					msgCopy := make([]byte, len(payload)-2)
					copy(msgCopy, payload[2:])
					go speculativeParse(msgCopy, wsTime, wsID, minCents)
				}

			case ws.OpPing:
				frame := ws.NewPongFrame(payload)
				frame = ws.MaskFrameInPlace(frame)
				ws.WriteFrame(conn, frame)

			case ws.OpClose:
				goto reconnect
			}
		}

	reconnect:
		conn.Close()
		fmt.Printf("[WS%d] 🔄 Reconnecting...\n", wsID)
		time.Sleep(2 * time.Second)
	}
}

// ============ Main ============

func main() {
	in := bufio.NewReader(os.Stdin)

	fmt.Println("╔═══════════════════════════════════════════╗")
	fmt.Println("║  P2C SNIPER - 5 WS + 5 Parallel Takes     ║")
	fmt.Println("╚═══════════════════════════════════════════╝")
	fmt.Println()

	fmt.Print("access_token cookie:\n> ")
	accessCookie, _ := in.ReadString('\n')
	accessCookie = strings.TrimSpace(accessCookie)

	if accessCookie == "" || !strings.HasPrefix(accessCookie, "access_token=") {
		fmt.Println("Invalid format")
		return
	}

	accessCookieGlobal = accessCookie

	fmt.Print("MIN amount (0 = no filter):\n> ")
	minLine, _ := in.ReadString('\n')
	minLine = strings.TrimSpace(minLine)

	minCents := int64(0)
	if minLine != "" {
		f, _ := strconv.ParseFloat(minLine, 64)
		minCents = int64(f * 100)
	}

	fmt.Println()
	fmt.Printf("⏳ Connecting %d HTTP clients...\n", numClients)

	for i := 0; i < numClients; i++ {
		clients[i] = newHTTPClient(fmt.Sprintf("C%d", i+1))
		if err := clients[i].connect(); err == nil {
			clients[i].warmup()
		}
		time.Sleep(30 * time.Millisecond)
	}

	ready := 0
	for i := 0; i < numClients; i++ {
		if clients[i].ready.Load() {
			ready++
		}
	}
	fmt.Printf("✅ %d/%d HTTP clients ready\n", ready, numClients)

	// Warmup goroutine
	go func() {
		for {
			for i := 0; i < numClients; i++ {
				c := clients[i]
				if c.inUse.Load() {
					continue
				}
				if !c.ready.Load() {
					go c.connect()
					continue
				}
				if time.Since(c.lastUsed) > 2*time.Second {
					if err := c.warmup(); err != nil {
						c.ready.Store(false)
						go c.connect()
					}
				}
				time.Sleep(400 * time.Millisecond)
			}
		}
	}()

	fmt.Println()
	fmt.Printf("⏳ Starting %d WebSockets...\n", numWebSockets)

	var wsWg sync.WaitGroup
	for i := 1; i <= numWebSockets; i++ {
		wsWg.Add(1)
		go runWebSocket(i, accessCookie, minCents, &wsWg)
		time.Sleep(150 * time.Millisecond)
	}

	fmt.Println()
	fmt.Println("════════════════════════════════════════════")
	fmt.Println()

	wsWg.Wait()
}
