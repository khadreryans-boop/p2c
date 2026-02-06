package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gobwas/ws"
	"github.com/gobwas/ws/wsutil"
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

const numClients = 40
const numWebSockets = 5

var (
	pauseTaking atomic.Bool
	seenOrders  sync.Map
)

// ============ Order Event для дедупликации с таймингами ============

type orderEvent struct {
	id      string
	amtStr  string
	wsID    int
	wsTime  time.Time
	handled atomic.Bool
}

var (
	pendingOrders sync.Map // id -> *orderEvent
)

// ============ HTTP Client Pool ============

type httpClient struct {
	conn   net.Conn
	br     *bufio.Reader
	bw     *bufio.Writer
	mu     sync.Mutex
	ready  atomic.Bool
	ewmaUs atomic.Uint64
	name   string
}

var (
	clients            [numClients]*httpClient
	accessCookieGlobal string
	cookieHeader       []byte
)

func newHTTPClient(name string) *httpClient {
	c := &httpClient{name: name}
	c.ready.Store(false)
	c.ewmaUs.Store(15000)
	return c
}

func (c *httpClient) connect() error {
	dialer := &net.Dialer{
		Timeout:   2 * time.Second,
		KeepAlive: 30 * time.Second,
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

	// SetNoDelay
	if tcpConn, ok := conn.NetConn().(*net.TCPConn); ok {
		_ = tcpConn.SetNoDelay(true)
	}

	c.conn = conn
	c.br = bufio.NewReaderSize(conn, 4096)
	c.bw = bufio.NewWriterSize(conn, 4096)
	c.ready.Store(true)
	return nil
}

func (c *httpClient) reconnect() {
	if c.conn != nil {
		c.conn.Close()
	}
	c.ready.Store(false)

	for i := 0; i < 3; i++ {
		if err := c.connect(); err == nil {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
}

var takeReqTemplate = []byte("POST " + takePathPrefix)
var takeReqSuffix = []byte(` HTTP/1.1
Host: app.send.tg
Content-Type: application/json
Accept: application/json
Origin: https://app.send.tg
Referer: https://app.send.tg/p2c/orders
Content-Length: 2
Connection: keep-alive
User-Agent: ` + UA + `
`)

func (c *httpClient) doTake(orderID string) (int, time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.conn == nil {
		c.reconnect()
		if c.conn == nil {
			return 0, 0
		}
	}

	_ = c.conn.SetDeadline(time.Now().Add(2 * time.Second))

	c.bw.Write(takeReqTemplate)
	c.bw.WriteString(orderID)
	c.bw.Write(takeReqSuffix)
	c.bw.Write(cookieHeader)
	c.bw.WriteString("\r\n{}")

	start := time.Now()
	if err := c.bw.Flush(); err != nil {
		c.reconnect()
		return 0, 0
	}

	line, err := c.br.ReadString('\n')
	dur := time.Since(start)

	if err != nil {
		c.reconnect()
		return 0, dur
	}

	if len(line) < 12 {
		c.reconnect()
		return 0, dur
	}
	code, _ := strconv.Atoi(line[9:12])

	// drain headers
	for {
		line, err := c.br.ReadString('\n')
		if err != nil || line == "\r\n" {
			break
		}
	}
	// drain body
	body := make([]byte, 512)
	c.br.Read(body)

	return code, dur
}

func pickBestClient() *httpClient {
	var best *httpClient
	bestUs := uint64(1 << 62)

	for i := 0; i < numClients; i++ {
		c := clients[i]
		if !c.ready.Load() {
			continue
		}
		us := c.ewmaUs.Load()
		if us < bestUs {
			bestUs = us
			best = c
		}
	}

	if best == nil {
		for i := 0; i < numClients; i++ {
			if clients[i].conn != nil {
				return clients[i]
			}
		}
	}
	return best
}

func updateEWMA(old, x uint64) uint64 {
	if old == 0 {
		return x
	}
	return (old*8 + x*2) / 10
}

// ============ Ultra-fast Take ============

func ultraFastTake(ev *orderEvent) {
	if pauseTaking.Load() {
		return
	}

	// Только первый WS обрабатывает
	if !ev.handled.CompareAndSwap(false, true) {
		return
	}

	client := pickBestClient()
	if client == nil {
		fmt.Printf("❌ NO CLIENT for %s\n", ev.id)
		return
	}

	client.ready.Store(false)
	code, dur := client.doTake(ev.id)
	client.ready.Store(true)

	us := uint64(dur.Microseconds())
	if code == 0 {
		us = 2_000_000
	}
	old := client.ewmaUs.Load()
	client.ewmaUs.Store(updateEWMA(old, us))

	rttMs := dur.Milliseconds()
	e2eMs := time.Since(ev.wsTime).Milliseconds()

	if code == 200 {
		fmt.Printf("✅ TAKEN [WS%d] %s rtt=%dms e2e=%dms id=%s amt=%s\n",
			ev.wsID, client.name, rttMs, e2eMs, ev.id, ev.amtStr)

		pauseTaking.Store(true)
		go func() {
			time.Sleep(pauseSeconds * time.Second)
			pauseTaking.Store(false)
		}()
		return
	}

	if code == 400 || code == 404 || code == 409 {
		fmt.Printf("LATE [WS%d] %s HTTP=%d rtt=%dms e2e=%dms id=%s\n",
			ev.wsID, client.name, code, rttMs, e2eMs, ev.id)
		return
	}

	if code != 0 {
		fmt.Printf("ERR [WS%d] %s HTTP=%d rtt=%dms id=%s\n",
			ev.wsID, client.name, code, rttMs, ev.id)
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

	// Проверяем, видели ли уже этот ордер
	ev := &orderEvent{
		id:     id,
		amtStr: amtStr,
		wsID:   wsID,
		wsTime: wsTime,
	}

	existing, loaded := pendingOrders.LoadOrStore(id, ev)
	if loaded {
		// Уже видели - логируем задержку между WS
		existingEv := existing.(*orderEvent)
		delay := wsTime.Sub(existingEv.wsTime)
		fmt.Printf("   ├─ [WS%d] DUPLICATE id=%s (WS%d was first by %v)\n",
			wsID, id, existingEv.wsID, delay)
		return
	}

	// Первый WS увидел этот ордер
	fmt.Printf("📥 [WS%d] NEW: %s amt=%s\n", wsID, id, amtStr)
	go ultraFastTake(ev)

	// Cleanup через 5 секунд
	go func() {
		time.Sleep(5 * time.Second)
		pendingOrders.Delete(id)
	}()
}

// ============ WebSocket (gobwas/ws) ============

type wsConn struct {
	conn     net.Conn
	id       int
	cookie   string
	minCents int64
}

func connectWebSocket(cookie string, wsID int) (net.Conn, error) {
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
				_ = tc.SetKeepAlivePeriod(30 * time.Second)
			}
			return conn, nil
		},
		TLSConfig: &tls.Config{
			ServerName: host,
			MinVersion: tls.VersionTLS12,
		},
	}

	conn, _, _, err := dialer.Dial(context.Background(), "wss://"+host+wsPath)
	if err != nil {
		return nil, err
	}

	return conn, nil
}

func wsWrite(conn net.Conn, msg []byte) error {
	return wsutil.WriteClientText(conn, msg)
}

func wsRead(conn net.Conn) ([]byte, error) {
	return wsutil.ReadServerText(conn)
}

func runWebSocket(wsID int, cookie string, minCents int64, wg *sync.WaitGroup) {
	defer wg.Done()

	reconnectDelay := 1 * time.Second

	for {
		conn, err := connectWebSocket(cookie, wsID)
		if err != nil {
			fmt.Printf("[WS%d] Connect error: %v\n", wsID, err)
			time.Sleep(reconnectDelay)
			continue
		}

		fmt.Printf("[WS%d] 🔌 Connected\n", wsID)
		reconnectDelay = 1 * time.Second

		// Engine.IO OPEN
		msg, err := wsRead(conn)
		if err != nil {
			fmt.Printf("[WS%d] Read OPEN error: %v\n", wsID, err)
			conn.Close()
			continue
		}
		_ = msg

		// Socket.IO connect
		if err := wsWrite(conn, []byte("40")); err != nil {
			conn.Close()
			continue
		}

		// ACK
		msg, err = wsRead(conn)
		if err != nil {
			fmt.Printf("[WS%d] Read ACK error: %v\n", wsID, err)
			conn.Close()
			continue
		}
		_ = msg

		// Subscribe
		wsWrite(conn, []byte(`42["list:initialize"]`))
		wsWrite(conn, []byte(`42["list:snapshot",[]]`))

		fmt.Printf("[WS%d] 🚀 Active\n", wsID)

		// Ping goroutine
		stopPing := make(chan struct{})
		go func() {
			ticker := time.NewTicker(20 * time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-ticker.C:
					if err := wsWrite(conn, []byte("2")); err != nil {
						return
					}
				case <-stopPing:
					return
				}
			}
		}()

		// Main read loop
		for {
			msg, err := wsRead(conn)
			wsTime := time.Now()

			if err != nil {
				fmt.Printf("[WS%d] Read error: %v\n", wsID, err)
				break
			}

			// Engine.IO ping
			if len(msg) == 1 && msg[0] == '2' {
				wsWrite(conn, []byte("3"))
				continue
			}

			// Socket.IO message
			if len(msg) > 2 && msg[0] == '4' && msg[1] == '2' {
				msgCopy := make([]byte, len(msg)-2)
				copy(msgCopy, msg[2:])
				go speculativeParse(msgCopy, wsTime, wsID, minCents)
			}
		}

		close(stopPing)
		conn.Close()
		fmt.Printf("[WS%d] 🔄 Reconnecting...\n", wsID)
		time.Sleep(reconnectDelay)
	}
}

// ============ Main ============

func main() {
	in := bufio.NewReader(os.Stdin)

	fmt.Println("╔═══════════════════════════════════════════╗")
	fmt.Println("║     P2C SNIPER - 5 WebSocket Edition      ║")
	fmt.Println("╚═══════════════════════════════════════════╝")
	fmt.Println()

	fmt.Print("Enter access_token cookie (format: access_token=...):\n> ")
	accessCookie, _ := in.ReadString('\n')
	accessCookie = strings.TrimSpace(accessCookie)

	if accessCookie == "" || !strings.HasPrefix(accessCookie, "access_token=") {
		fmt.Println("Invalid cookie format. Expected: access_token=...")
		return
	}

	accessCookieGlobal = accessCookie
	cookieHeader = []byte("Cookie: " + accessCookie + "\r\n")

	fmt.Print("Enter MIN in_amount (e.g. 300). 0 = no filter:\n> ")
	minLine, _ := in.ReadString('\n')
	minLine = strings.TrimSpace(minLine)

	minCents := int64(0)
	if minLine != "" {
		f, err := strconv.ParseFloat(minLine, 64)
		if err != nil {
			fmt.Println("Bad MIN amount")
			return
		}
		minCents = int64(f * 100)
	}

	fmt.Println()
	fmt.Println("⏳ Initializing", numClients, "HTTP clients...")

	// Init HTTP clients
	var httpWg sync.WaitGroup
	for i := 0; i < numClients; i++ {
		clients[i] = newHTTPClient(fmt.Sprintf("C%02d", i+1))
		httpWg.Add(1)
		go func(c *httpClient) {
			defer httpWg.Done()
			c.connect()
		}(clients[i])
	}
	httpWg.Wait()

	ready := 0
	for i := 0; i < numClients; i++ {
		if clients[i].ready.Load() {
			ready++
		}
	}
	fmt.Printf("✅ %d/%d HTTP clients ready\n", ready, numClients)

	// Keep-alive goroutine
	go func() {
		ticker := time.NewTicker(15 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			for i := 0; i < numClients; i++ {
				c := clients[i]
				if !c.ready.Load() || c.conn == nil {
					go c.reconnect()
				}
			}
		}
	}()

	fmt.Println()
	fmt.Println("⏳ Starting", numWebSockets, "WebSocket connections...")
	fmt.Println()

	// Start WebSockets
	var wsWg sync.WaitGroup
	for i := 1; i <= numWebSockets; i++ {
		wsWg.Add(1)
		go runWebSocket(i, accessCookie, minCents, &wsWg)
		time.Sleep(200 * time.Millisecond) // stagger connections
	}

	fmt.Println()
	fmt.Println("════════════════════════════════════════════")
	fmt.Println("  Watching for orders... (Ctrl+C to stop)")
	fmt.Println("════════════════════════════════════════════")
	fmt.Println()

	// Wait forever
	wsWg.Wait()
}
