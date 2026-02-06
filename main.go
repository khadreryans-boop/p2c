package main

import (
	"bufio"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	host           = "app.send.tg"
	paymentsPath   = "/internal/v1/p2c/payments"
	takePathPrefix = "/internal/v1/p2c/payments/take/"
	pauseSeconds   = 20
	pollInterval   = 50 * time.Millisecond // Poll every 50ms!
)

const (
	numPollers    = 5  // 5 parallel pollers
	numTakers     = 10 // 10 HTTP clients for taking
	parallelTakes = 5
)

var (
	pauseTaking atomic.Bool
	seenOrders  sync.Map
)

var (
	cookie    string
	reqPrefix []byte
	reqSuffix []byte
)

// Payment from API
type Payment struct {
	ID       string `json:"id"`
	InAmount string `json:"in_amount"`
	Status   string `json:"status"`
}

type PaymentsResponse struct {
	Data []Payment `json:"data"`
}

// ============ HTTP Client ============

type httpClient struct {
	conn     net.Conn
	br       *bufio.Reader
	bw       *bufio.Writer
	mu       sync.Mutex
	ready    atomic.Bool
	name     string
	ip       string
	lastUsed time.Time
	inUse    atomic.Bool
	lastRtt  atomic.Uint64

	totalRequests atomic.Uint64
	wins          atomic.Uint64
}

var takers []*httpClient
var pollers []*httpClient
var popIPs []string

func newHTTPClient(name, ip string) *httpClient {
	c := &httpClient{name: name, ip: ip}
	c.ready.Store(false)
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

	conn, err := dialer.Dial("tcp", c.ip+":443")
	if err != nil {
		return err
	}

	if tcpConn, ok := conn.(*net.TCPConn); ok {
		tcpConn.SetNoDelay(true)
		tcpConn.SetKeepAlive(true)
		tcpConn.SetKeepAlivePeriod(10 * time.Second)
	}

	tlsConfig := &tls.Config{
		ServerName: host,
		MinVersion: tls.VersionTLS12,
	}
	tlsConn := tls.Client(conn, tlsConfig)
	if err := tlsConn.Handshake(); err != nil {
		conn.Close()
		return err
	}

	c.conn = tlsConn
	c.br = bufio.NewReaderSize(tlsConn, 8192)
	c.bw = bufio.NewWriterSize(tlsConn, 4096)
	c.lastUsed = time.Now()
	c.ready.Store(true)
	return nil
}

// Poll payments list
func (c *httpClient) pollPayments() ([]Payment, time.Duration, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.conn == nil {
		return nil, 0, fmt.Errorf("no conn")
	}

	c.conn.SetDeadline(time.Now().Add(2 * time.Second))

	req := "GET " + paymentsPath + " HTTP/1.1\r\n" +
		"Host: " + host + "\r\n" +
		"Cookie: " + cookie + "\r\n" +
		"Accept: application/json\r\n" +
		"Connection: keep-alive\r\n" +
		"\r\n"

	start := time.Now()
	_, err := c.bw.WriteString(req)
	if err != nil {
		c.conn.Close()
		c.conn = nil
		c.ready.Store(false)
		return nil, 0, err
	}
	if err := c.bw.Flush(); err != nil {
		c.conn.Close()
		c.conn = nil
		c.ready.Store(false)
		return nil, 0, err
	}

	// Read status line
	line, err := c.br.ReadString('\n')
	if err != nil {
		c.conn.Close()
		c.conn = nil
		c.ready.Store(false)
		return nil, 0, err
	}

	if len(line) < 12 {
		return nil, 0, fmt.Errorf("short status")
	}

	// Read headers
	contentLen := 0
	for {
		line, err := c.br.ReadString('\n')
		if err != nil {
			return nil, 0, err
		}
		if line == "\r\n" {
			break
		}
		lower := strings.ToLower(line)
		if strings.HasPrefix(lower, "content-length:") {
			fmt.Sscanf(line[15:], "%d", &contentLen)
		}
	}

	// Read body
	if contentLen == 0 {
		return nil, 0, fmt.Errorf("no content")
	}

	body := make([]byte, contentLen)
	_, err = io.ReadFull(c.br, body)
	dur := time.Since(start)

	if err != nil {
		c.conn.Close()
		c.conn = nil
		c.ready.Store(false)
		return nil, dur, err
	}

	c.lastUsed = time.Now()
	c.lastRtt.Store(uint64(dur.Milliseconds()))

	// Parse JSON
	var resp PaymentsResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, dur, err
	}

	return resp.Data, dur, nil
}

func (c *httpClient) doTake(orderID string) (int, time.Duration, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.conn == nil {
		return 0, 0, fmt.Errorf("no conn")
	}

	c.conn.SetDeadline(time.Now().Add(2 * time.Second))

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

	// Drain response
	contentLen := 0
	for {
		line, err := c.br.ReadString('\n')
		if err != nil || line == "\r\n" {
			break
		}
		if strings.HasPrefix(strings.ToLower(line), "content-length:") {
			fmt.Sscanf(line[15:], "%d", &contentLen)
		}
	}

	if contentLen > 0 && contentLen < 4096 {
		tmp := make([]byte, contentLen)
		io.ReadFull(c.br, tmp)
	}

	c.lastUsed = time.Now()
	c.lastRtt.Store(uint64(dur.Milliseconds()))
	c.totalRequests.Add(1)

	return code, dur, nil
}

func getBestTakers(count int) []*httpClient {
	type cs struct {
		c   *httpClient
		rtt uint64
	}

	var avail []cs
	for _, c := range takers {
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

// ============ Take Logic ============

type takeResult struct {
	client *httpClient
	code   int
	dur    time.Duration
	err    error
}

func instantTake(id, amt string, detectTime time.Time) {
	if pauseTaking.Load() {
		return
	}

	if _, loaded := seenOrders.LoadOrStore(id, struct{}{}); loaded {
		return
	}

	best := getBestTakers(parallelTakes)
	if len(best) == 0 {
		fmt.Printf("   ❌ NO CLIENTS\n")
		return
	}

	fmt.Printf("📥 NEW: %s amt=%s (%d takers)\n", id, amt, len(best))

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

	e2e := time.Since(detectTime).Milliseconds()

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

// ============ Polling Loop ============

func runPoller(pollerID int, client *httpClient, minCents int64) {
	knownIDs := make(map[string]bool)
	var pollCount uint64

	for {
		if pauseTaking.Load() {
			time.Sleep(500 * time.Millisecond)
			continue
		}

		if !client.ready.Load() {
			client.connect()
			time.Sleep(100 * time.Millisecond)
			continue
		}

		payments, dur, err := client.pollPayments()
		detectTime := time.Now()
		pollCount++

		if err != nil {
			client.ready.Store(false)
			continue
		}

		// Check for new payments
		for _, p := range payments {
			if p.Status != "pending" && p.Status != "available" {
				continue
			}

			if knownIDs[p.ID] {
				continue
			}

			// New payment found!
			knownIDs[p.ID] = true

			// Filter by amount
			if minCents > 0 && p.InAmount != "" {
				var whole, frac int64
				var fracDigits int
				var seenDot bool
				for i := 0; i < len(p.InAmount); i++ {
					c := p.InAmount[i]
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
					continue
				}
			}

			go instantTake(p.ID, p.InAmount, detectTime)
		}

		// Update known IDs (remove old ones)
		if pollCount%100 == 0 {
			currentIDs := make(map[string]bool)
			for _, p := range payments {
				currentIDs[p.ID] = true
			}
			knownIDs = currentIDs
		}

		// Log periodically
		if pollCount%200 == 0 {
			fmt.Printf("   [P%d] poll=%dms count=%d\n", pollerID, dur.Milliseconds(), pollCount)
		}

		time.Sleep(pollInterval)
	}
}

func printStats() {
	fmt.Println("\n📊 STATS:")
	fmt.Println("  Takers:")
	for _, c := range takers {
		if c.totalRequests.Load() > 0 {
			fmt.Printf("    %s: rtt=%dms reqs=%d wins=%d\n",
				c.name, c.lastRtt.Load(), c.totalRequests.Load(), c.wins.Load())
		}
	}
	fmt.Println("  Pollers:")
	for _, c := range pollers {
		fmt.Printf("    %s: rtt=%dms\n", c.name, c.lastRtt.Load())
	}
}

func main() {
	in := bufio.NewReader(os.Stdin)

	fmt.Println("╔═══════════════════════════════════════════╗")
	fmt.Println("║  P2C SNIPER - HTTP Polling (no WebSocket) ║")
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
	fmt.Printf("✅ Found %d POP IPs: %v\n", len(popIPs), popIPs)

	reqPrefix = []byte("POST " + takePathPrefix)
	reqSuffix = []byte(" HTTP/1.1\r\n" +
		"Host: " + host + "\r\n" +
		"Content-Type: application/json\r\n" +
		"Cookie: " + cookie + "\r\n" +
		"Content-Length: 2\r\n" +
		"\r\n{}")

	// Create takers (for taking orders)
	fmt.Printf("\n⏳ Creating %d takers...\n", numTakers)
	for i := 0; i < numTakers; i++ {
		ip := popIPs[i%len(popIPs)]
		name := fmt.Sprintf("T%d@%s", i+1, ip[strings.LastIndex(ip, ".")+1:])
		c := newHTTPClient(name, ip)
		c.connect()
		takers = append(takers, c)
	}

	// Create pollers (for polling payments)
	fmt.Printf("⏳ Creating %d pollers...\n", numPollers)
	for i := 0; i < numPollers; i++ {
		ip := popIPs[i%len(popIPs)]
		name := fmt.Sprintf("P%d@%s", i+1, ip[strings.LastIndex(ip, ".")+1:])
		c := newHTTPClient(name, ip)
		c.connect()
		pollers = append(pollers, c)
	}

	ready := 0
	for _, c := range takers {
		if c.ready.Load() {
			ready++
		}
	}
	for _, c := range pollers {
		if c.ready.Load() {
			ready++
		}
	}
	fmt.Printf("✅ %d/%d connections ready\n", ready, numTakers+numPollers)

	// Warmup takers
	for i, c := range takers {
		go func(idx int, cl *httpClient) {
			time.Sleep(time.Duration(idx*100) * time.Millisecond)
			for {
				time.Sleep(2 * time.Second)
				if cl.inUse.Load() || !cl.ready.Load() {
					if !cl.ready.Load() {
						cl.connect()
					}
					continue
				}
				// Simple warmup - HEAD request
				cl.mu.Lock()
				if cl.conn != nil {
					cl.conn.SetDeadline(time.Now().Add(2 * time.Second))
					req := "HEAD /p2c/orders HTTP/1.1\r\nHost: " + host + "\r\nCookie: " + cookie + "\r\nConnection: keep-alive\r\n\r\n"
					cl.bw.WriteString(req)
					cl.bw.Flush()
					cl.br.ReadString('\n')
					for {
						line, _ := cl.br.ReadString('\n')
						if line == "\r\n" || line == "" {
							break
						}
					}
					cl.lastUsed = time.Now()
				}
				cl.mu.Unlock()
			}
		}(i, c)
	}

	go func() {
		for {
			time.Sleep(30 * time.Second)
			printStats()
		}
	}()

	// Start pollers
	fmt.Printf("\n⏳ Starting %d pollers (every %v)...\n", numPollers, pollInterval)
	for i, c := range pollers {
		go runPoller(i+1, c, minCents)
		time.Sleep(pollInterval / time.Duration(numPollers)) // Stagger
	}

	fmt.Println("\n════════════════════════════════════════════")
	fmt.Printf("  HTTP POLLING every %v (no WebSocket!)\n", pollInterval)
	fmt.Printf("  %d pollers | %d takers | 5 parallel takes\n", numPollers, numTakers)
	fmt.Println("════════════════════════════════════════════\n")

	// Keep running
	select {}
}
