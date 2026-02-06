package main

import (
	"bufio"
	"bytes"
	"crypto/tls"
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
	pollPath       = "/internal/v1/p2c-socket/?EIO=4&transport=polling"
	takePathPrefix = "/internal/v1/p2c/payments/take/"
	pauseSeconds   = 20
)

const (
	numPollers    = 10 // 10 Engine.IO polling clients
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
	popIPs    []string
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
	sid      string // Engine.IO session ID
	lastUsed time.Time
	inUse    atomic.Bool
	lastRtt  atomic.Uint64

	totalRequests atomic.Uint64
	wins          atomic.Uint64
}

var takers []*httpClient
var pollers []*httpClient

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

// Engine.IO handshake - get session ID
func (c *httpClient) engineIOHandshake() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.conn == nil {
		return fmt.Errorf("no conn")
	}

	c.conn.SetDeadline(time.Now().Add(5 * time.Second))

	req := "GET " + pollPath + " HTTP/1.1\r\n" +
		"Host: " + host + "\r\n" +
		"Cookie: " + cookie + "\r\n" +
		"Accept: */*\r\n" +
		"Connection: keep-alive\r\n" +
		"\r\n"

	_, err := c.bw.WriteString(req)
	if err != nil {
		return err
	}
	if err := c.bw.Flush(); err != nil {
		return err
	}

	// Read response
	body, err := c.readResponse()
	if err != nil {
		return err
	}

	// Parse Engine.IO OPEN packet: 0{"sid":"xxx","upgrades":["websocket"],"pingInterval":25000,"pingTimeout":20000}
	if len(body) < 2 || body[0] != '0' {
		return fmt.Errorf("invalid handshake: %s", string(body[:min(50, len(body))]))
	}

	// Extract sid
	sidIdx := bytes.Index(body, []byte(`"sid":"`))
	if sidIdx == -1 {
		return fmt.Errorf("no sid in response")
	}
	sidStart := sidIdx + 7
	sidEnd := bytes.IndexByte(body[sidStart:], '"')
	if sidEnd == -1 {
		return fmt.Errorf("invalid sid")
	}
	c.sid = string(body[sidStart : sidStart+sidEnd])

	fmt.Printf("   [%s] Engine.IO sid=%s\n", c.name, c.sid[:12]+"...")
	return nil
}

// Send Engine.IO message via polling
func (c *httpClient) engineIOSend(msg string) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.conn == nil || c.sid == "" {
		return fmt.Errorf("not connected")
	}

	c.conn.SetDeadline(time.Now().Add(3 * time.Second))

	body := msg
	req := fmt.Sprintf("POST %s&sid=%s HTTP/1.1\r\n"+
		"Host: %s\r\n"+
		"Cookie: %s\r\n"+
		"Content-Type: text/plain;charset=UTF-8\r\n"+
		"Content-Length: %d\r\n"+
		"Connection: keep-alive\r\n"+
		"\r\n%s",
		pollPath, c.sid, host, cookie, len(body), body)

	_, err := c.bw.WriteString(req)
	if err != nil {
		return err
	}
	if err := c.bw.Flush(); err != nil {
		return err
	}

	_, err = c.readResponse()
	return err
}

// Long-poll for messages
func (c *httpClient) engineIOPoll() ([]byte, time.Duration, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.conn == nil || c.sid == "" {
		return nil, 0, fmt.Errorf("not connected")
	}

	c.conn.SetDeadline(time.Now().Add(30 * time.Second))

	req := fmt.Sprintf("GET %s&sid=%s HTTP/1.1\r\n"+
		"Host: %s\r\n"+
		"Cookie: %s\r\n"+
		"Accept: */*\r\n"+
		"Connection: keep-alive\r\n"+
		"\r\n",
		pollPath, c.sid, host, cookie)

	start := time.Now()
	_, err := c.bw.WriteString(req)
	if err != nil {
		c.ready.Store(false)
		return nil, 0, err
	}
	if err := c.bw.Flush(); err != nil {
		c.ready.Store(false)
		return nil, 0, err
	}

	body, err := c.readResponse()
	dur := time.Since(start)

	if err != nil {
		c.ready.Store(false)
		return nil, dur, err
	}

	c.lastRtt.Store(uint64(dur.Milliseconds()))
	return body, dur, nil
}

func (c *httpClient) readResponse() ([]byte, error) {
	// Read status line
	line, err := c.br.ReadString('\n')
	if err != nil {
		return nil, err
	}

	if len(line) < 12 {
		return nil, fmt.Errorf("short status")
	}

	// Read headers
	contentLen := 0
	chunked := false
	for {
		line, err := c.br.ReadString('\n')
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

	// Read body
	var body []byte
	if chunked {
		for {
			sizeLine, err := c.br.ReadString('\n')
			if err != nil {
				return nil, err
			}
			sizeLine = strings.TrimSpace(sizeLine)
			size, _ := strconv.ParseInt(sizeLine, 16, 64)
			if size == 0 {
				c.br.ReadString('\n')
				break
			}
			chunk := make([]byte, size)
			io.ReadFull(c.br, chunk)
			body = append(body, chunk...)
			c.br.ReadString('\n')
		}
	} else if contentLen > 0 {
		body = make([]byte, contentLen)
		_, err = io.ReadFull(c.br, body)
	}

	return body, err
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

func instantTake(id, amt string, pollerID int, detectTime time.Time) {
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

	fmt.Printf("📥 [P%d] NEW: %s amt=%s\n", pollerID, id, amt)

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

// ============ Parse Engine.IO messages ============

var (
	opAddBytes = []byte(`"op":"add"`)
	idPrefix   = []byte(`"id":"`)
	amtPrefix  = []byte(`"in_amount":"`)
)

func parseMessages(data []byte, pollerID int, detectTime time.Time, minCents int64) {
	// Engine.IO polling can return multiple messages concatenated
	// Format: <length>:<packet><length>:<packet>...
	// Or just single packet for short messages

	// Check for "list:update" with "add"
	if !bytes.Contains(data, opAddBytes) {
		return
	}

	// Find all "add" operations
	idx := 0
	for {
		addIdx := bytes.Index(data[idx:], opAddBytes)
		if addIdx == -1 {
			break
		}
		addIdx += idx

		// Find ID near this add
		searchStart := addIdx
		if searchStart > 200 {
			searchStart = addIdx - 200
		}
		searchEnd := addIdx + 500
		if searchEnd > len(data) {
			searchEnd = len(data)
		}
		chunk := data[searchStart:searchEnd]

		idIdx := bytes.Index(chunk, idPrefix)
		if idIdx == -1 {
			idx = addIdx + 10
			continue
		}
		idStart := idIdx + 6
		idEnd := bytes.IndexByte(chunk[idStart:], '"')
		if idEnd == -1 || idEnd > 30 {
			idx = addIdx + 10
			continue
		}
		id := string(chunk[idStart : idStart+idEnd])

		// Get amount
		var amt string
		amtIdx := bytes.Index(chunk, amtPrefix)
		if amtIdx != -1 {
			amtStart := amtIdx + 13
			amtEnd := bytes.IndexByte(chunk[amtStart:], '"')
			if amtEnd != -1 && amtEnd < 20 {
				amt = string(chunk[amtStart : amtStart+amtEnd])
			}
		}

		// Filter
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
				idx = addIdx + 10
				continue
			}
		}

		go instantTake(id, amt, pollerID, detectTime)
		idx = addIdx + 10
	}
}

// ============ Polling Loop ============

func runPoller(pollerID int, client *httpClient, minCents int64) {
	for {
		// Connect
		if !client.ready.Load() {
			if err := client.connect(); err != nil {
				fmt.Printf("   [P%d] connect error: %v\n", pollerID, err)
				time.Sleep(2 * time.Second)
				continue
			}
		}

		// Engine.IO handshake
		if err := client.engineIOHandshake(); err != nil {
			fmt.Printf("   [P%d] handshake error: %v\n", pollerID, err)
			client.ready.Store(false)
			time.Sleep(2 * time.Second)
			continue
		}

		// Send Socket.IO connect
		if err := client.engineIOSend("40"); err != nil {
			fmt.Printf("   [P%d] send 40 error: %v\n", pollerID, err)
			client.ready.Store(false)
			continue
		}

		// Poll for connect ACK
		data, _, err := client.engineIOPoll()
		if err != nil {
			fmt.Printf("   [P%d] poll error: %v\n", pollerID, err)
			client.ready.Store(false)
			continue
		}
		_ = data

		// Initialize list
		time.Sleep(50 * time.Millisecond)
		client.engineIOSend(`42["list:initialize"]`)
		time.Sleep(50 * time.Millisecond)
		client.engineIOSend(`42["list:snapshot",[]]`)

		fmt.Printf("[P%d] 🚀 Connected via HTTP polling\n", pollerID)

		// Continuous polling
		for {
			if pauseTaking.Load() {
				time.Sleep(500 * time.Millisecond)
				continue
			}

			data, dur, err := client.engineIOPoll()
			detectTime := time.Now()

			if err != nil {
				fmt.Printf("   [P%d] poll error: %v\n", pollerID, err)
				client.ready.Store(false)
				break
			}

			// Handle ping (2) - respond with pong (3)
			if len(data) == 1 && data[0] == '2' {
				client.engineIOSend("3")
				continue
			}

			// Parse messages for new orders
			if len(data) > 5 {
				parseMessages(data, pollerID, detectTime, minCents)
			}

			// Log occasionally
			if dur.Milliseconds() > 1000 {
				fmt.Printf("   [P%d] poll took %dms\n", pollerID, dur.Milliseconds())
			}
		}

		fmt.Printf("[P%d] 🔄 Reconnecting...\n", pollerID)
		time.Sleep(1 * time.Second)
	}
}

func main() {
	in := bufio.NewReader(os.Stdin)

	fmt.Println("╔═══════════════════════════════════════════╗")
	fmt.Println("║  P2C SNIPER - Engine.IO HTTP Polling      ║")
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

	// Create takers
	fmt.Printf("\n⏳ Creating %d takers...\n", numTakers)
	for i := 0; i < numTakers; i++ {
		ip := popIPs[i%len(popIPs)]
		name := fmt.Sprintf("T%d@%s", i+1, ip[strings.LastIndex(ip, ".")+1:])
		c := newHTTPClient(name, ip)
		c.connect()
		takers = append(takers, c)
	}

	// Create pollers
	fmt.Printf("⏳ Creating %d Engine.IO pollers...\n", numPollers)
	for i := 0; i < numPollers; i++ {
		ip := popIPs[i%len(popIPs)]
		name := fmt.Sprintf("P%d@%s", i+1, ip[strings.LastIndex(ip, ".")+1:])
		c := newHTTPClient(name, ip)
		pollers = append(pollers, c)
	}

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

	// Start pollers
	fmt.Printf("\n⏳ Starting %d Engine.IO pollers...\n", numPollers)
	for i, c := range pollers {
		go runPoller(i+1, c, minCents)
		time.Sleep(200 * time.Millisecond)
	}

	fmt.Println("\n════════════════════════════════════════════")
	fmt.Println("  Engine.IO HTTP POLLING (not WebSocket!)")
	fmt.Printf("  %d pollers | %d takers\n", numPollers, numTakers)
	fmt.Println("════════════════════════════════════════════\n")

	select {}
}
