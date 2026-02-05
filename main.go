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

var (
	pauseTaking atomic.Bool
	seenOrders  sync.Map
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

	// SetNoDelay через underlying TCP conn
	if err := conn.NetConn().(*net.TCPConn).SetNoDelay(true); err != nil {
		// ignore
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

	// build request directly
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

	// read response
	line, err := c.br.ReadString('\n')
	dur := time.Since(start)

	if err != nil {
		c.reconnect()
		return 0, dur
	}

	// parse status: "HTTP/1.1 200 OK"
	if len(line) < 12 {
		c.reconnect()
		return 0, dur
	}
	code, _ := strconv.Atoi(line[9:12])

	// drain response body
	for {
		line, err := c.br.ReadString('\n')
		if err != nil || line == "\r\n" {
			break
		}
	}
	// read body (assume small)
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

func ultraFastTake(id, amtStr string, wsTime time.Time) {
	if pauseTaking.Load() {
		return
	}

	if _, loaded := seenOrders.LoadOrStore(id, struct{}{}); loaded {
		return
	}

	client := pickBestClient()
	if client == nil {
		fmt.Printf("❌ NO CLIENT for %s\n", id)
		return
	}

	client.ready.Store(false)
	code, dur := client.doTake(id)
	client.ready.Store(true)

	us := uint64(dur.Microseconds())
	if code == 0 {
		us = 2_000_000
	}
	old := client.ewmaUs.Load()
	client.ewmaUs.Store(updateEWMA(old, us))

	rttMs := dur.Milliseconds()
	e2eMs := time.Since(wsTime).Milliseconds()

	if code == 200 {
		fmt.Printf("✅ TAKEN %s rtt=%dms e2e=%dms id=%s amt=%s\n",
			client.name, rttMs, e2eMs, id, amtStr)

		pauseTaking.Store(true)
		go func() {
			time.Sleep(pauseSeconds * time.Second)
			pauseTaking.Store(false)
		}()
		return
	}

	if code == 400 || code == 404 || code == 409 {
		fmt.Printf("LATE %s HTTP=%d rtt=%dms e2e=%dms id=%s\n",
			client.name, code, rttMs, e2eMs, id)
		return
	}

	if code != 0 {
		fmt.Printf("ERR %s HTTP=%d rtt=%dms id=%s\n", client.name, code, rttMs, id)
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

func speculativeParse(msg []byte, wsTime time.Time, minCents int64) {
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

	fmt.Printf("📥 NEW: %s amt=%s\n", id, amtStr)
	go ultraFastTake(id, amtStr, wsTime)
}

// ============ WebSocket (gobwas/ws) ============

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
	msg, err := wsutil.ReadServerText(conn)
	return msg, err
}

func runWebSocket(cookie string, minCents int64) {
	for {
		conn, err := connectWebSocket(cookie)
		if err != nil {
			fmt.Println("WS connect error:", err)
			time.Sleep(2 * time.Second)
			continue
		}

		fmt.Println("🔌 WebSocket connected")

		// Engine.IO handshake
		msg, err := wsRead(conn)
		if err != nil {
			conn.Close()
			continue
		}
		_ = msg // OPEN packet

		// Socket.IO connect
		wsWrite(conn, []byte("40"))

		msg, err = wsRead(conn)
		if err != nil {
			conn.Close()
			continue
		}
		_ = msg // ACK

		// Subscribe
		wsWrite(conn, []byte(`42["list:initialize"]`))
		wsWrite(conn, []byte(`42["list:snapshot",[]]`))

		fmt.Println("🚀 FAST MODE ACTIVE")

		// Ping goroutine
		stopPing := make(chan struct{})
		go func() {
			ticker := time.NewTicker(25 * time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-ticker.C:
					wsWrite(conn, []byte("2"))
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
				fmt.Println("WS read error:", err)
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
				go speculativeParse(msgCopy, wsTime, minCents)
			}
		}

		close(stopPing)
		conn.Close()
		fmt.Println("🔄 Reconnecting...")
		time.Sleep(1 * time.Second)
	}
}

// ============ Main ============

func main() {
	in := bufio.NewReader(os.Stdin)

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

	fmt.Println("⏳ Initializing", numClients, "HTTP clients...")

	// Init HTTP clients
	var wg sync.WaitGroup
	for i := 0; i < numClients; i++ {
		clients[i] = newHTTPClient(fmt.Sprintf("C%02d", i+1))
		wg.Add(1)
		go func(c *httpClient) {
			defer wg.Done()
			c.connect()
		}(clients[i])
	}
	wg.Wait()

	ready := 0
	for i := 0; i < numClients; i++ {
		if clients[i].ready.Load() {
			ready++
		}
	}
	fmt.Printf("✅ %d/%d clients ready\n", ready, numClients)

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

	// Run WebSocket
	runWebSocket(accessCookie, minCents)
}
