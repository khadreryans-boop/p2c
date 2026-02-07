package main

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	host           = "app.send.tg"
	wsURL          = "wss://app.send.tg/internal/v1/p2c-socket/?EIO=4&transport=websocket"
	takePathPrefix = "/internal/v1/p2c/payments/take/"
	warmupPath     = "/internal/v1/p2c/accounts"
	pauseSeconds   = 20
)

const (
	numClients    = 40 // HTTP клиентов
	numWebSockets = 5  // websocat процессов
	numTakers     = 4  // Лучших клиентов для take
	warmupMs      = 1000
)

var (
	pauseTaking atomic.Bool
	cookie      string
	serverIP    string
	minCents    int64
)

// ============ HTTP Client ============

type httpClient struct {
	id      int
	conn    net.Conn
	br      *bufio.Reader
	bw      *bufio.Writer
	mu      sync.Mutex
	ready   atomic.Bool
	inUse   atomic.Bool
	lastRTT atomic.Int64 // Последний RTT в микросекундах
}

var clients []*httpClient

func (c *httpClient) connect() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.conn != nil {
		c.conn.Close()
	}

	conn, err := net.DialTimeout("tcp", serverIP+":443", 2*time.Second)
	if err != nil {
		c.ready.Store(false)
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
		c.ready.Store(false)
		return err
	}

	c.conn = tlsConn
	c.br = bufio.NewReaderSize(tlsConn, 4096)
	c.bw = bufio.NewWriterSize(tlsConn, 2048)
	c.ready.Store(true)
	c.lastRTT.Store(999999) // Reset RTT
	return nil
}

func (c *httpClient) warmup() {
	if !c.inUse.CompareAndSwap(false, true) {
		return
	}
	defer c.inUse.Store(false)

	c.mu.Lock()
	defer c.mu.Unlock()

	if c.conn == nil {
		return
	}

	c.conn.SetDeadline(time.Now().Add(800 * time.Millisecond))

	req := "POST " + warmupPath + " HTTP/1.1\r\n" +
		"Host: " + host + "\r\n" +
		"Cookie: " + cookie + "\r\n" +
		"Content-Type: application/json\r\n" +
		"Content-Length: 2\r\n" +
		"Connection: keep-alive\r\n\r\n{}"

	start := time.Now()
	c.bw.WriteString(req)
	if err := c.bw.Flush(); err != nil {
		c.conn.Close()
		c.conn = nil
		c.ready.Store(false)
		return
	}

	line, err := c.br.ReadString('\n')
	rtt := time.Since(start)

	if err != nil {
		c.conn.Close()
		c.conn = nil
		c.ready.Store(false)
		return
	}
	_ = line

	// Drain headers and body
	contentLen := 0
	for {
		line, _ := c.br.ReadString('\n')
		if line == "\r\n" || line == "" {
			break
		}
		if strings.HasPrefix(strings.ToLower(line), "content-length:") {
			fmt.Sscanf(line[15:], "%d", &contentLen)
		}
	}
	if contentLen > 0 {
		body := make([]byte, contentLen)
		io.ReadFull(c.br, body)
	}

	c.lastRTT.Store(rtt.Microseconds())
}

func (c *httpClient) take(orderID string) (int, time.Duration, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.conn == nil {
		return 0, 0, fmt.Errorf("no conn")
	}

	c.conn.SetDeadline(time.Now().Add(2 * time.Second))

	req := "POST " + takePathPrefix + orderID + " HTTP/1.1\r\n" +
		"Host: " + host + "\r\n" +
		"Cookie: " + cookie + "\r\n" +
		"Content-Type: application/json\r\n" +
		"Content-Length: 2\r\n" +
		"Connection: keep-alive\r\n\r\n{}"

	start := time.Now()
	c.bw.WriteString(req)
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
		return 0, dur, fmt.Errorf("short response")
	}

	code := 0
	fmt.Sscanf(line[9:12], "%d", &code)

	// Drain
	for {
		l, _ := c.br.ReadString('\n')
		if l == "\r\n" || l == "" {
			break
		}
	}

	return code, dur, nil
}

// Get top N clients by RTT
func getTopClients(n int) []*httpClient {
	type clientRTT struct {
		c   *httpClient
		rtt int64
	}

	var available []clientRTT
	for _, c := range clients {
		if c.ready.Load() && !c.inUse.Load() {
			available = append(available, clientRTT{c, c.lastRTT.Load()})
		}
	}

	// Sort by RTT (lowest first)
	sort.Slice(available, func(i, j int) bool {
		return available[i].rtt < available[j].rtt
	})

	// Take top N
	result := make([]*httpClient, 0, n)
	for i := 0; i < len(available) && i < n; i++ {
		result = append(result, available[i].c)
	}

	return result
}

// ============ Dedupe ============

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

// ============ Stats ============

var (
	totalSeen atomic.Int64
	totalWon  atomic.Int64
	totalLate atomic.Int64
)

// ============ Take Handler ============

func handleOrder(orderID, amt string, wsID int, detectTime time.Time) {
	if pauseTaking.Load() {
		return
	}

	if !markSeen(orderID) {
		return
	}

	totalSeen.Add(1)

	// Filter by amount
	if minCents > 0 && amt != "" {
		cents := parseCents(amt)
		if cents < minCents {
			return
		}
	}

	// Get top 4 clients by RTT
	topClients := getTopClients(numTakers)

	if len(topClients) == 0 {
		totalLate.Add(1)
		fmt.Printf("   [WS%d] NO CLIENTS amt=%s\n", wsID, amt)
		return
	}

	// Mark as in use
	for _, c := range topClients {
		c.inUse.Store(true)
	}

	// Fire all in parallel
	fireTime := time.Now()

	type result struct {
		id   int
		code int
		dur  time.Duration
		rtt  int64
		err  error
	}

	ch := make(chan result, len(topClients))

	for _, c := range topClients {
		go func(client *httpClient) {
			code, dur, err := client.take(orderID)
			ch <- result{client.id, code, dur, client.lastRTT.Load(), err}
			client.inUse.Store(false)
		}(c)
	}

	fireLatency := time.Since(fireTime).Microseconds() // Только spawn goroutines

	// Collect results
	var results []result
	for range topClients {
		results = append(results, <-ch)
	}

	e2e := time.Since(detectTime).Milliseconds()

	// Format output
	var parts []string
	var won bool
	for _, r := range results {
		if r.err != nil {
			parts = append(parts, fmt.Sprintf("C%d:ERR", r.id))
			go clients[r.id-1].connect()
		} else if r.code == 200 {
			parts = append(parts, fmt.Sprintf("C%d:OK(%dms,rtt=%dμs)", r.id, r.dur.Milliseconds(), r.rtt))
			won = true
		} else {
			parts = append(parts, fmt.Sprintf("C%d:%d(%dms,rtt=%dμs)", r.id, r.code, r.dur.Milliseconds(), r.rtt))
		}
	}

	if won {
		totalWon.Add(1)
		fmt.Printf("✅ [WS%d] e2e=%dms fire=%dμs amt=%s | %s\n",
			wsID, e2e, fireLatency, amt, strings.Join(parts, " "))
		pauseTaking.Store(true)
		go func() {
			time.Sleep(pauseSeconds * time.Second)
			pauseTaking.Store(false)
			fmt.Println("▶ Resumed")
		}()
	} else {
		totalLate.Add(1)
		fmt.Printf("   [WS%d] LATE e2e=%dms fire=%dμs amt=%s | %s\n",
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

// ============ WebSocket (websocat) ============

func runWebsocat(wsID int) {
	for {
		err := runWsocatSession(wsID)
		if err != nil {
			fmt.Printf("[WS%d] err: %v\n", wsID, err)
		}
		time.Sleep(2 * time.Second)
	}
}

func runWsocatSession(wsID int) error {
	cmd := exec.Command("websocat",
		"-H", "Cookie: "+cookie,
		"-H", "Origin: https://app.send.tg",
		"--no-close",
		wsURL)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}

	stderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}

	if err := cmd.Start(); err != nil {
		return err
	}

	// Drain stderr
	go func() {
		io.Copy(io.Discard, stderr)
	}()

	defer func() {
		stdin.Close()
		cmd.Process.Kill()
		cmd.Wait()
	}()

	reader := bufio.NewReader(stdout)

	// Read Engine.IO open
	line, err := reader.ReadString('\n')
	if err != nil {
		return fmt.Errorf("read open: %v", err)
	}
	_ = line

	// Send Socket.IO connect
	stdin.Write([]byte("40\n"))

	// Read connect ack
	line, err = reader.ReadString('\n')
	if err != nil {
		return fmt.Errorf("read ack: %v", err)
	}

	time.Sleep(30 * time.Millisecond)
	stdin.Write([]byte(`42["list:initialize"]` + "\n"))
	time.Sleep(30 * time.Millisecond)
	stdin.Write([]byte(`42["list:snapshot",[]]` + "\n"))

	fmt.Printf("[WS%d] 🚀 connected\n", wsID)

	// Read loop
	for {
		line, err := reader.ReadString('\n')
		detectTime := time.Now()

		if err != nil {
			return fmt.Errorf("read: %v", err)
		}

		line = strings.TrimSpace(line)
		if len(line) == 0 {
			continue
		}

		// Ping
		if line == "2" {
			stdin.Write([]byte("3\n"))
			continue
		}

		// Message
		if len(line) > 2 && line[0] == '4' && line[1] == '2' {
			go parseAndHandle([]byte(line[2:]), wsID, detectTime)
		}
	}
}

func parseAndHandle(data []byte, wsID int, detectTime time.Time) {
	if !bytes.Contains(data, []byte(`"op":"add"`)) {
		return
	}

	// Parse order ID
	idIdx := bytes.Index(data, []byte(`"id":"`))
	if idIdx == -1 {
		return
	}
	start := idIdx + 6
	end := bytes.IndexByte(data[start:], '"')
	if end == -1 || end > 30 {
		return
	}
	orderID := string(data[start : start+end])

	// Parse amount
	var amt string
	if idx := bytes.Index(data, []byte(`"in_amount":"`)); idx != -1 {
		s := idx + 13
		e := bytes.IndexByte(data[s:], '"')
		if e != -1 && e < 20 {
			amt = string(data[s : s+e])
		}
	}

	handleOrder(orderID, amt, wsID, detectTime)
}

// ============ Main ============

func main() {
	in := bufio.NewReader(os.Stdin)

	fmt.Println("╔═══════════════════════════════════════════╗")
	fmt.Println("║  FINAL SNIPER - 40 clients, 5 WS, top 4   ║")
	fmt.Println("╚═══════════════════════════════════════════╝")

	fmt.Print("\naccess_token cookie:\n> ")
	cookie, _ = in.ReadString('\n')
	cookie = strings.TrimSpace(cookie)
	if !strings.HasPrefix(cookie, "access_token=") {
		fmt.Println("Invalid cookie")
		return
	}

	fmt.Print("MIN amount (0=all):\n> ")
	minLine, _ := in.ReadString('\n')
	minLine = strings.TrimSpace(minLine)
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

	// Check websocat
	fmt.Println("\n⏳ Checking websocat...")
	cmd := exec.Command("websocat", "--version")
	output, err := cmd.Output()
	if err != nil {
		fmt.Printf("❌ websocat not found\n")
		fmt.Println("Install: wget https://github.com/vi/websocat/releases/download/v1.12.0/websocat.x86_64-unknown-linux-musl -O /usr/local/bin/websocat && chmod +x /usr/local/bin/websocat")
		return
	}
	fmt.Printf("✅ %s", string(output))

	// Create HTTP clients
	fmt.Printf("\n⏳ Creating %d HTTP clients...\n", numClients)
	for i := 1; i <= numClients; i++ {
		c := &httpClient{id: i}
		if err := c.connect(); err != nil {
			fmt.Printf("   C%d: connect error\n", i)
		}
		clients = append(clients, c)
	}

	ready := 0
	for _, c := range clients {
		if c.ready.Load() {
			ready++
		}
	}
	fmt.Printf("✅ %d/%d clients ready\n", ready, numClients)

	// Initial warmup to measure RTT
	fmt.Println("⏳ Initial warmup (measuring RTT)...")
	var wg sync.WaitGroup
	for _, c := range clients {
		wg.Add(1)
		go func(client *httpClient) {
			client.warmup()
			wg.Done()
		}(c)
	}
	wg.Wait()

	// Show top 10 by RTT
	top := getTopClients(10)
	fmt.Println("📊 Top 10 clients by RTT:")
	for i, c := range top {
		fmt.Printf("   %d. C%d: %dμs\n", i+1, c.id, c.lastRTT.Load())
	}

	// Warmup goroutines (staggered)
	for i, c := range clients {
		go func(idx int, client *httpClient) {
			// Stagger
			time.Sleep(time.Duration(idx*25) * time.Millisecond)
			for {
				time.Sleep(warmupMs * time.Millisecond)
				if client.ready.Load() && !client.inUse.Load() {
					client.warmup()
				} else if !client.ready.Load() {
					client.connect()
				}
			}
		}(i, c)
	}

	// Start WebSockets
	fmt.Printf("\n⏳ Starting %d websocat WebSockets...\n", numWebSockets)
	for i := 1; i <= numWebSockets; i++ {
		go runWebsocat(i)
		time.Sleep(200 * time.Millisecond)
	}

	fmt.Println("\n════════════════════════════════════════════")
	fmt.Printf("  %d HTTP clients | %d WebSockets | top %d take\n", numClients, numWebSockets, numTakers)
	fmt.Printf("  Warmup: %dms | Select by RTT\n", warmupMs)
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

			// Show current top 4
			top := getTopClients(4)
			var topIDs []string
			for _, c := range top {
				topIDs = append(topIDs, fmt.Sprintf("C%d(%dμs)", c.id, c.lastRTT.Load()))
			}

			fmt.Printf("\n📊 STATS: seen=%d won=%d late=%d (%.1f%%) | top4: %s\n\n",
				s, w, l, rate, strings.Join(topIDs, " "))
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
