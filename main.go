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

const UA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"

const (
	host           = "app.send.tg"
	wsPath         = "/internal/v1/p2c-socket/?EIO=4&transport=websocket"
	takePathPrefix = "/internal/v1/p2c/payments/take/"
	origin         = "https://app.send.tg"
	referer        = "https://app.send.tg/p2c/orders"
	pauseSeconds   = 20
)

const numClients = 15
const numWebSockets = 5
const parallelTakes = 5

var (
	pauseTaking atomic.Bool
	seenOrders  sync.Map
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

	// Stats
	totalRequests atomic.Uint64
	totalLatency  atomic.Uint64
	minLatency    atomic.Uint64
	maxLatency    atomic.Uint64
	wins          atomic.Uint64
}

var (
	clients            [numClients]*httpClient
	accessCookieGlobal string
)

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
		Timeout:   3 * time.Second,
		KeepAlive: 10 * time.Second,
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
		_ = tcpConn.SetKeepAlivePeriod(10 * time.Second)
	}

	c.conn = conn
	c.br = bufio.NewReaderSize(conn, 4096)
	c.bw = bufio.NewWriterSize(conn, 2048)
	c.lastUsed = time.Now()
	c.ready.Store(true)
	return nil
}

func (c *httpClient) warmup() (time.Duration, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.conn == nil {
		return 0, fmt.Errorf("no connection")
	}

	_ = c.conn.SetDeadline(time.Now().Add(2 * time.Second))

	req := fmt.Sprintf("HEAD /p2c/orders HTTP/1.1\r\n"+
		"Host: %s\r\n"+
		"User-Agent: %s\r\n"+
		"Cookie: %s\r\n"+
		"Connection: keep-alive\r\n"+
		"\r\n",
		host, UA, accessCookieGlobal)

	start := time.Now()

	_, err := c.bw.WriteString(req)
	if err != nil {
		return 0, err
	}

	if err := c.bw.Flush(); err != nil {
		return 0, err
	}

	line, err := c.br.ReadString('\n')
	dur := time.Since(start)

	if err != nil {
		return dur, err
	}
	_ = line

	for {
		line, err = c.br.ReadString('\n')
		if err != nil || line == "\r\n" {
			break
		}
	}

	c.lastUsed = time.Now()
	c.lastRtt.Store(uint64(dur.Milliseconds()))
	return dur, nil
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

func (c *httpClient) avgLatency() uint64 {
	total := c.totalRequests.Load()
	if total == 0 {
		return 0
	}
	return c.totalLatency.Load() / total
}

// Получить 5 лучших клиентов по lastRtt
func getBestClients(count int) []*httpClient {
	type clientScore struct {
		client *httpClient
		rtt    uint64
	}

	var available []clientScore
	for i := 0; i < numClients; i++ {
		c := clients[i]
		if c.ready.Load() && !c.inUse.Load() {
			available = append(available, clientScore{c, c.lastRtt.Load()})
		}
	}

	// Сортируем по RTT
	sort.Slice(available, func(i, j int) bool {
		return available[i].rtt < available[j].rtt
	})

	var result []*httpClient
	for i := 0; i < len(available) && i < count; i++ {
		result = append(result, available[i].client)
	}

	return result
}

// ============ Instant Parallel Takes ============

type takeResult struct {
	client *httpClient
	code   int
	dur    time.Duration
	err    error
}

func instantParallelTake(id, amtStr string, wsID int, wsTime time.Time) {
	if pauseTaking.Load() {
		return
	}

	// Проверяем дедупликацию
	if _, loaded := seenOrders.LoadOrStore(id, wsTime); loaded {
		return
	}

	// Получаем 5 лучших клиентов СРАЗУ
	bestClients := getBestClients(parallelTakes)
	if len(bestClients) == 0 {
		fmt.Printf("   ❌ NO READY CLIENTS\n")
		return
	}

	fmt.Printf("📥 [WS%d] NEW: %s amt=%s (using %d clients)\n", wsID, id, amtStr, len(bestClients))

	results := make(chan takeResult, len(bestClients))
	var wg sync.WaitGroup

	// Запускаем ВСЕ параллельно СРАЗУ
	for _, c := range bestClients {
		c.inUse.Store(true)
		wg.Add(1)
		go func(client *httpClient) {
			defer wg.Done()
			defer client.inUse.Store(false)

			code, dur, err := client.doTake(id)
			if err != nil {
				go client.connect()
			}
			results <- takeResult{client, code, dur, err}
		}(c)
	}

	go func() {
		wg.Wait()
		close(results)
	}()

	// Собираем результаты
	var allResults []takeResult
	for r := range results {
		allResults = append(allResults, r)
	}

	sort.Slice(allResults, func(i, j int) bool {
		return allResults[i].dur < allResults[j].dur
	})

	e2eMs := time.Since(wsTime).Milliseconds()

	var winner *takeResult
	var rttDetails []string

	for _, r := range allResults {
		if r.err != nil {
			rttDetails = append(rttDetails, fmt.Sprintf("%s:ERR", r.client.name))
			continue
		}

		status := fmt.Sprintf("%s:%dms", r.client.name, r.dur.Milliseconds())
		if r.code == 200 {
			status += "✓"
			if winner == nil {
				winner = &r
				r.client.wins.Add(1)
			}
		} else {
			status += fmt.Sprintf("(%d)", r.code)
		}
		rttDetails = append(rttDetails, status)
	}

	if winner != nil {
		fmt.Printf("✅ TAKEN [WS%d] e2e=%dms | %s | id=%s amt=%s\n",
			wsID, e2eMs, strings.Join(rttDetails, " "), id, amtStr)

		pauseTaking.Store(true)
		go func() {
			time.Sleep(pauseSeconds * time.Second)
			pauseTaking.Store(false)
			fmt.Println("▶ Resumed")
		}()
		return
	}

	fmt.Printf("   LATE [WS%d] e2e=%dms | %s | id=%s\n",
		wsID, e2eMs, strings.Join(rttDetails, " "), id)

	// Cleanup
	go func() {
		time.Sleep(5 * time.Second)
		seenOrders.Delete(id)
	}()
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

	// СРАЗУ запускаем take - не ждём другие WS!
	go instantParallelTake(id, amtStr, wsID, wsTime)
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
				_ = tc.SetKeepAlivePeriod(10 * time.Second)
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

		wsc.writeText([]byte("40"))

		payload, op, err = readFrame(conn)
		if err != nil || op == ws.OpClose {
			conn.Close()
			continue
		}

		time.Sleep(50 * time.Millisecond)
		wsc.writeText([]byte(`42["list:initialize"]`))
		time.Sleep(50 * time.Millisecond)
		wsc.writeText([]byte(`42["list:snapshot",[]]`))

		fmt.Printf("[WS%d] 🚀 Active\n", wsID)

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
					// Парсим и запускаем СРАЗУ в горутине
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

func printStats() {
	fmt.Println("\n📊 CLIENT STATS (sorted by lastRtt):")
	fmt.Println("────────────────────────────────────────────")

	type stat struct {
		name    string
		reqs    uint64
		avg     uint64
		min     uint64
		max     uint64
		wins    uint64
		lastRtt uint64
	}

	var stats []stat
	for i := 0; i < numClients; i++ {
		c := clients[i]
		minLat := c.minLatency.Load()
		if minLat == 999999999 {
			minLat = 0
		}
		stats = append(stats, stat{
			name:    c.name,
			reqs:    c.totalRequests.Load(),
			avg:     c.avgLatency(),
			min:     minLat,
			max:     c.maxLatency.Load(),
			wins:    c.wins.Load(),
			lastRtt: c.lastRtt.Load(),
		})
	}

	sort.Slice(stats, func(i, j int) bool {
		return stats[i].lastRtt < stats[j].lastRtt
	})

	for _, s := range stats {
		fmt.Printf("   %s: lastRtt=%3dms | reqs=%d avg=%dms min=%dms max=%dms wins=%d\n",
			s.name, s.lastRtt, s.reqs, s.avg, s.min, s.max, s.wins)
	}
	fmt.Println("────────────────────────────────────────────")
}

// ============ Main ============

func main() {
	in := bufio.NewReader(os.Stdin)

	fmt.Println("╔═══════════════════════════════════════════╗")
	fmt.Println("║  P2C SNIPER - 15 Clients, Best 5 Takes    ║")
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

	var connectWg sync.WaitGroup
	for i := 0; i < numClients; i++ {
		clients[i] = newHTTPClient(fmt.Sprintf("C%02d", i+1))
		connectWg.Add(1)
		go func(c *httpClient) {
			defer connectWg.Done()
			if err := c.connect(); err == nil {
				c.warmup()
			}
		}(clients[i])
	}
	connectWg.Wait()

	ready := 0
	for i := 0; i < numClients; i++ {
		if clients[i].ready.Load() {
			ready++
		}
	}
	fmt.Printf("✅ %d/%d HTTP clients ready\n", ready, numClients)

	// Warmup - каждый клиент каждую секунду
	for i := 0; i < numClients; i++ {
		go func(idx int) {
			c := clients[idx]
			// Стаггеринг чтобы не все одновременно
			time.Sleep(time.Duration(idx*67) * time.Millisecond)

			for {
				time.Sleep(1 * time.Second)

				if c.inUse.Load() {
					continue
				}

				if !c.ready.Load() {
					c.connect()
					continue
				}

				dur, err := c.warmup()
				if err != nil {
					c.ready.Store(false)
					c.connect()
				} else if dur > 500*time.Millisecond {
					c.ready.Store(false)
					c.connect()
				}
			}
		}(i)
	}

	// Stats every 30 sec
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			printStats()
		}
	}()

	fmt.Println()
	fmt.Printf("⏳ Starting %d WebSockets...\n", numWebSockets)

	var wsWg sync.WaitGroup
	for i := 1; i <= numWebSockets; i++ {
		wsWg.Add(1)
		go runWebSocket(i, accessCookie, minCents, &wsWg)
		time.Sleep(100 * time.Millisecond)
	}

	fmt.Println()
	fmt.Println("════════════════════════════════════════════")
	fmt.Println("  15 HTTP clients, 5 best used per take")
	fmt.Println("  First WS triggers instant parallel takes")
	fmt.Println("════════════════════════════════════════════")
	fmt.Println()

	wsWg.Wait()
}
