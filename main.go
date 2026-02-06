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
	pollInterval   = 50 * time.Millisecond
)

const (
	numPollers    = 3
	numTakers     = 10
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
	c.br = bufio.NewReaderSize(tlsConn, 16384)
	c.bw = bufio.NewWriterSize(tlsConn, 4096)
	c.lastUsed = time.Now()
	c.ready.Store(true)
	return nil
}

func (c *httpClient) pollPaymentsRaw() ([]byte, time.Duration, error) {
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

	line, err := c.br.ReadString('\n')
	if err != nil {
		c.conn.Close()
		c.conn = nil
		c.ready.Store(false)
		return nil, 0, err
	}

	if len(line) < 12 {
		return nil, 0, fmt.Errorf("short status: %s", line)
	}

	contentLen := 0
	chunked := false
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
		if strings.Contains(lower, "transfer-encoding") && strings.Contains(lower, "chunked") {
			chunked = true
		}
	}

	var body []byte
	if chunked {
		// Read chunked encoding
		for {
			sizeLine, err := c.br.ReadString('\n')
			if err != nil {
				return nil, 0, err
			}
			sizeLine = strings.TrimSpace(sizeLine)
			size, err := strconv.ParseInt(sizeLine, 16, 64)
			if err != nil {
				return nil, 0, fmt.Errorf("bad chunk size: %s", sizeLine)
			}
			if size == 0 {
				c.br.ReadString('\n') // trailing CRLF
				break
			}
			chunk := make([]byte, size)
			_, err = io.ReadFull(c.br, chunk)
			if err != nil {
				return nil, 0, err
			}
			body = append(body, chunk...)
			c.br.ReadString('\n') // CRLF after chunk
		}
	} else if contentLen > 0 {
		body = make([]byte, contentLen)
		_, err = io.ReadFull(c.br, body)
		if err != nil {
			c.conn.Close()
			c.conn = nil
			c.ready.Store(false)
			return nil, 0, err
		}
	}

	dur := time.Since(start)
	c.lastUsed = time.Now()
	c.lastRtt.Store(uint64(dur.Milliseconds()))

	return body, dur, nil
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

// Global known IDs
var globalKnownIDs sync.Map

// ============ Polling Loop ============

func runPoller(pollerID int, client *httpClient, minCents int64) {
	var pollCount uint64
	var firstRun = true

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

		body, dur, err := client.pollPaymentsRaw()
		detectTime := time.Now()
		pollCount++

		if err != nil {
			fmt.Printf("   [P%d] ERROR: %v\n", pollerID, err)
			client.ready.Store(false)
			continue
		}

		// First run - print raw JSON sample
		if firstRun && pollerID == 1 {
			firstRun = false
			fmt.Println("\n════════ RAW JSON SAMPLE ════════")
			if len(body) > 500 {
				fmt.Println(string(body[:500]) + "...")
			} else {
				fmt.Println(string(body))
			}
			fmt.Println("════════════════════════════════════\n")
		}

		// Parse JSON - try to be flexible
		var rawData map[string]interface{}
		if err := json.Unmarshal(body, &rawData); err != nil {
			fmt.Printf("   [P%d] JSON parse error: %v\n", pollerID, err)
			continue
		}

		// Find the array with payments
		var payments []map[string]interface{}

		if data, ok := rawData["data"].([]interface{}); ok {
			for _, item := range data {
				if m, ok := item.(map[string]interface{}); ok {
					payments = append(payments, m)
				}
			}
		}

		// Debug: show what we found
		if pollCount%100 == 1 && pollerID == 1 {
			fmt.Printf("   [P%d] poll=%dms found=%d payments\n", pollerID, dur.Milliseconds(), len(payments))
			if len(payments) > 0 {
				// Show first payment structure
				fmt.Printf("   Sample payment keys: ")
				for k := range payments[0] {
					fmt.Printf("%s ", k)
				}
				fmt.Println()
			}
		}

		for _, p := range payments {
			// Get ID - try different field names
			var id string
			if v, ok := p["id"].(string); ok {
				id = v
			} else if v, ok := p["_id"].(string); ok {
				id = v
			} else if v, ok := p["payment_id"].(string); ok {
				id = v
			}

			if id == "" {
				continue
			}

			// Check if already known
			if _, exists := globalKnownIDs.Load(id); exists {
				continue
			}
			globalKnownIDs.Store(id, time.Now())

			// Get amount - try different field names
			var amt string
			if v, ok := p["in_amount"].(string); ok {
				amt = v
			} else if v, ok := p["amount"].(string); ok {
				amt = v
			} else if v, ok := p["in_amount"].(float64); ok {
				amt = fmt.Sprintf("%.2f", v)
			} else if v, ok := p["amount"].(float64); ok {
				amt = fmt.Sprintf("%.2f", v)
			}

			// Get status
			var status string
			if v, ok := p["status"].(string); ok {
				status = v
			}

			// Log every new payment we see
			fmt.Printf("🔍 [P%d] NEW ID: %s amt=%s status=%s\n", pollerID, id, amt, status)

			// Filter by amount
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
					fmt.Printf("   [P%d] SKIP amt=%s < min\n", pollerID, amt)
					continue
				}
			}

			go instantTake(id, amt, detectTime)
		}

		// Cleanup old IDs
		if pollCount%500 == 0 {
			now := time.Now()
			globalKnownIDs.Range(func(key, value interface{}) bool {
				if t, ok := value.(time.Time); ok {
					if now.Sub(t) > 5*time.Minute {
						globalKnownIDs.Delete(key)
					}
				}
				return true
			})
		}

		time.Sleep(pollInterval)
	}
}

func main() {
	in := bufio.NewReader(os.Stdin)

	fmt.Println("╔═══════════════════════════════════════════╗")
	fmt.Println("║  P2C SNIPER - HTTP Polling (DEBUG v2)     ║")
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

	fmt.Printf("\n⏳ Creating %d takers...\n", numTakers)
	for i := 0; i < numTakers; i++ {
		ip := popIPs[i%len(popIPs)]
		name := fmt.Sprintf("T%d@%s", i+1, ip[strings.LastIndex(ip, ".")+1:])
		c := newHTTPClient(name, ip)
		c.connect()
		takers = append(takers, c)
	}

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

	fmt.Printf("\n⏳ Starting %d pollers (every %v)...\n", numPollers, pollInterval)
	for i, c := range pollers {
		go runPoller(i+1, c, minCents)
		time.Sleep(pollInterval / time.Duration(numPollers))
	}

	fmt.Println("\n════════════════════════════════════════════")
	fmt.Printf("  HTTP POLLING every %v\n", pollInterval)
	fmt.Printf("  Watch for 🔍 NEW ID messages!\n")
	fmt.Println("════════════════════════════════════════════\n")

	select {}
}
