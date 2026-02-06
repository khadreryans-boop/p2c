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
	pollTimeout    = 800 * time.Millisecond // Короткий таймаут!
)

const (
	numPollers    = 30 // Много поллеров
	numTakers     = 15
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
	conn    net.Conn
	br      *bufio.Reader
	bw      *bufio.Writer
	mu      sync.Mutex
	ready   atomic.Bool
	name    string
	ip      string
	sid     string
	inUse   atomic.Bool
	lastRtt atomic.Uint64
	wins    atomic.Uint64
	reqs    atomic.Uint64
}

var takers []*httpClient
var pollers []*httpClient

func newClient(name, ip string) *httpClient {
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
	c.sid = ""

	dialer := &net.Dialer{
		Timeout:   2 * time.Second,
		KeepAlive: 5 * time.Second,
	}

	conn, err := dialer.Dial("tcp", c.ip+":443")
	if err != nil {
		return err
	}

	if tc, ok := conn.(*net.TCPConn); ok {
		tc.SetNoDelay(true)
		tc.SetKeepAlive(true)
		tc.SetKeepAlivePeriod(5 * time.Second)
	}

	tlsConn := tls.Client(conn, &tls.Config{
		ServerName: host,
		MinVersion: tls.VersionTLS12,
	})
	if err := tlsConn.Handshake(); err != nil {
		conn.Close()
		return err
	}

	c.conn = tlsConn
	c.br = bufio.NewReaderSize(tlsConn, 16384)
	c.bw = bufio.NewWriterSize(tlsConn, 4096)
	c.ready.Store(true)
	return nil
}

func (c *httpClient) readResponse(timeout time.Duration) ([]byte, int, error) {
	c.conn.SetDeadline(time.Now().Add(timeout))

	line, err := c.br.ReadString('\n')
	if err != nil {
		return nil, 0, err
	}

	if len(line) < 12 {
		return nil, 0, fmt.Errorf("short status")
	}
	code, _ := strconv.Atoi(line[9:12])

	contentLen := 0
	chunked := false
	for {
		line, err := c.br.ReadString('\n')
		if err != nil {
			return nil, code, err
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
			sizeLine, err := c.br.ReadString('\n')
			if err != nil {
				return body, code, err
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
		io.ReadFull(c.br, body)
	}

	return body, code, nil
}

func (c *httpClient) engineIOHandshake() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.conn == nil {
		return fmt.Errorf("no conn")
	}

	req := "GET " + pollPath + " HTTP/1.1\r\n" +
		"Host: " + host + "\r\n" +
		"Cookie: " + cookie + "\r\n" +
		"Accept: */*\r\n" +
		"Connection: keep-alive\r\n" +
		"\r\n"

	c.bw.WriteString(req)
	c.bw.Flush()

	body, _, err := c.readResponse(5 * time.Second)
	if err != nil {
		return err
	}

	if len(body) < 2 || body[0] != '0' {
		return fmt.Errorf("invalid handshake")
	}

	sidIdx := bytes.Index(body, []byte(`"sid":"`))
	if sidIdx == -1 {
		return fmt.Errorf("no sid")
	}
	sidStart := sidIdx + 7
	sidEnd := bytes.IndexByte(body[sidStart:], '"')
	if sidEnd == -1 {
		return fmt.Errorf("invalid sid")
	}
	c.sid = string(body[sidStart : sidStart+sidEnd])
	return nil
}

func (c *httpClient) engineIOSend(msg string) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.conn == nil || c.sid == "" {
		return fmt.Errorf("not connected")
	}

	c.conn.SetDeadline(time.Now().Add(2 * time.Second))

	req := fmt.Sprintf("POST %s&sid=%s HTTP/1.1\r\n"+
		"Host: %s\r\n"+
		"Cookie: %s\r\n"+
		"Content-Type: text/plain;charset=UTF-8\r\n"+
		"Content-Length: %d\r\n"+
		"Connection: keep-alive\r\n"+
		"\r\n%s",
		pollPath, c.sid, host, cookie, len(msg), msg)

	c.bw.WriteString(req)
	c.bw.Flush()
	c.readResponse(2 * time.Second)
	return nil
}

func (c *httpClient) engineIOPoll() ([]byte, time.Duration, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.conn == nil || c.sid == "" {
		return nil, 0, fmt.Errorf("not connected")
	}

	req := fmt.Sprintf("GET %s&sid=%s HTTP/1.1\r\n"+
		"Host: %s\r\n"+
		"Cookie: %s\r\n"+
		"Accept: */*\r\n"+
		"Connection: keep-alive\r\n"+
		"\r\n",
		pollPath, c.sid, host, cookie)

	start := time.Now()
	c.bw.WriteString(req)
	c.bw.Flush()

	body, code, err := c.readResponse(pollTimeout)
	dur := time.Since(start)

	if err != nil {
		return nil, dur, err
	}

	if code != 200 {
		return nil, dur, fmt.Errorf("code %d", code)
	}

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

	c.lastRtt.Store(uint64(dur.Milliseconds()))
	c.reqs.Add(1)
	return code, dur, nil
}

func getBestTakers(count int) []*httpClient {
	type ts struct {
		t   *httpClient
		rtt uint64
	}
	var avail []ts
	for _, t := range takers {
		if t.ready.Load() && !t.inUse.Load() {
			avail = append(avail, ts{t, t.lastRtt.Load()})
		}
	}
	sort.Slice(avail, func(i, j int) bool {
		return avail[i].rtt < avail[j].rtt
	})
	var result []*httpClient
	for i := 0; i < len(avail) && i < count; i++ {
		result = append(result, avail[i].t)
	}
	return result
}

// ============ Take ============

func instantTake(id, amt string, pollerID int, detectTime time.Time) {
	if pauseTaking.Load() {
		return
	}

	if _, loaded := seenOrders.LoadOrStore(id, struct{}{}); loaded {
		return
	}

	best := getBestTakers(parallelTakes)
	if len(best) == 0 {
		fmt.Printf("   ❌ NO TAKERS\n")
		return
	}

	fmt.Printf("📥 [P%02d] %s amt=%s\n", pollerID, id[:16], amt)

	type result struct {
		t    *httpClient
		code int
		dur  time.Duration
		err  error
	}

	results := make(chan result, len(best))
	var wg sync.WaitGroup

	for _, t := range best {
		t.inUse.Store(true)
		wg.Add(1)
		go func(tk *httpClient) {
			defer wg.Done()
			defer tk.inUse.Store(false)
			code, dur, err := tk.doTake(id)
			if err != nil {
				go tk.connect()
			}
			results <- result{tk, code, dur, err}
		}(t)
	}

	go func() {
		wg.Wait()
		close(results)
	}()

	var all []result
	for r := range results {
		all = append(all, r)
	}

	sort.Slice(all, func(i, j int) bool {
		return all[i].dur < all[j].dur
	})

	e2e := time.Since(detectTime).Milliseconds()

	var winner *result
	var details []string

	for _, r := range all {
		if r.err != nil {
			details = append(details, r.t.name+":ERR")
			continue
		}
		s := fmt.Sprintf("%s:%d", r.t.name, r.dur.Milliseconds())
		if r.code == 200 {
			s += "✓"
			if winner == nil {
				winner = &r
				r.t.wins.Add(1)
			}
		} else {
			s += fmt.Sprintf("(%d)", r.code)
		}
		details = append(details, s)
	}

	if winner != nil {
		fmt.Printf("✅ TAKEN e2e=%dms | %s\n", e2e, strings.Join(details, " "))
		pauseTaking.Store(true)
		go func() {
			time.Sleep(pauseSeconds * time.Second)
			pauseTaking.Store(false)
			fmt.Println("▶ Resumed")
		}()
		return
	}

	fmt.Printf("   LATE e2e=%dms | %s\n", e2e, strings.Join(details, " "))

	go func() {
		time.Sleep(3 * time.Second)
		seenOrders.Delete(id)
	}()
}

// ============ Parse ============

var (
	opAddBytes = []byte(`"op":"add"`)
	idPrefix   = []byte(`"id":"`)
	amtPrefix  = []byte(`"in_amount":"`)
)

func parseMessages(data []byte, pollerID int, detectTime time.Time, minCents int64) {
	if !bytes.Contains(data, opAddBytes) {
		return
	}

	idx := 0
	for {
		addIdx := bytes.Index(data[idx:], opAddBytes)
		if addIdx == -1 {
			break
		}
		addIdx += idx

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

		var amt string
		amtIdx := bytes.Index(chunk, amtPrefix)
		if amtIdx != -1 {
			amtStart := amtIdx + 13
			amtEnd := bytes.IndexByte(chunk[amtStart:], '"')
			if amtEnd != -1 && amtEnd < 20 {
				amt = string(chunk[amtStart : amtStart+amtEnd])
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
				idx = addIdx + 10
				continue
			}
		}

		instantTake(id, amt, pollerID, detectTime)
		idx = addIdx + 10
	}
}

// ============ Poller ============

func runPoller(pollerID int, client *httpClient, minCents int64) {
	ipShort := client.ip[strings.LastIndex(client.ip, ".")+1:]

	for {
		// Connect
		if err := client.connect(); err != nil {
			time.Sleep(500 * time.Millisecond)
			continue
		}

		// Handshake
		if err := client.engineIOHandshake(); err != nil {
			time.Sleep(500 * time.Millisecond)
			continue
		}

		// Socket.IO connect
		client.engineIOSend("40")
		client.engineIOPoll() // ACK

		time.Sleep(30 * time.Millisecond)
		client.engineIOSend(`42["list:initialize"]`)
		time.Sleep(30 * time.Millisecond)
		client.engineIOSend(`42["list:snapshot",[]]`)

		fmt.Printf("[P%02d@%s] 🚀\n", pollerID, ipShort)

		// Poll loop
		for {
			if pauseTaking.Load() {
				time.Sleep(200 * time.Millisecond)
				continue
			}

			data, dur, err := client.engineIOPoll()
			detectTime := time.Now()

			if err != nil {
				// Timeout или ошибка - переподключаемся к новому воркеру!
				break
			}

			// Ping/pong
			if len(data) == 1 && data[0] == '2' {
				client.engineIOSend("3")
				continue
			}

			// Parse
			if len(data) > 5 {
				parseMessages(data, pollerID, detectTime, minCents)
			}

			_ = dur
		}

		// Reconnect = новый воркер CDN
		time.Sleep(100 * time.Millisecond)
	}
}

func main() {
	in := bufio.NewReader(os.Stdin)

	fmt.Println("╔═══════════════════════════════════════════╗")
	fmt.Println("║  P2C SNIPER - Aggressive HTTP Polling     ║")
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
	reqSuffix = []byte(" HTTP/1.1\r\n" +
		"Host: " + host + "\r\n" +
		"Content-Type: application/json\r\n" +
		"Cookie: " + cookie + "\r\n" +
		"Content-Length: 2\r\n" +
		"\r\n{}")

	// Takers
	fmt.Printf("\n⏳ Creating %d takers...\n", numTakers)
	for i := 0; i < numTakers; i++ {
		ip := popIPs[i%len(popIPs)]
		t := newClient(fmt.Sprintf("T%d", i+1), ip)
		t.connect()
		takers = append(takers, t)
	}

	// Warmup takers
	for i, t := range takers {
		go func(idx int, tk *httpClient) {
			time.Sleep(time.Duration(idx*30) * time.Millisecond)
			for {
				time.Sleep(500 * time.Millisecond)
				if tk.inUse.Load() || !tk.ready.Load() {
					if !tk.ready.Load() {
						tk.connect()
					}
					continue
				}
				tk.mu.Lock()
				if tk.conn != nil {
					tk.conn.SetDeadline(time.Now().Add(1 * time.Second))
					req := "HEAD /p2c/orders HTTP/1.1\r\nHost: " + host + "\r\nCookie: " + cookie + "\r\n\r\n"
					tk.bw.WriteString(req)
					tk.bw.Flush()
					tk.br.ReadString('\n')
					for {
						line, _ := tk.br.ReadString('\n')
						if line == "\r\n" || line == "" {
							break
						}
					}
				}
				tk.mu.Unlock()
			}
		}(i, t)
	}

	// Pollers
	fmt.Printf("⏳ Creating %d pollers...\n", numPollers)
	for i := 0; i < numPollers; i++ {
		ip := popIPs[i%len(popIPs)]
		p := newClient(fmt.Sprintf("P%d", i+1), ip)
		pollers = append(pollers, p)
	}

	// Start pollers
	for i, p := range pollers {
		go runPoller(i+1, p, minCents)
		time.Sleep(50 * time.Millisecond)
	}

	fmt.Println("\n════════════════════════════════════════════")
	fmt.Printf("  %d pollers | %d takers | timeout=%v\n", numPollers, numTakers, pollTimeout)
	fmt.Println("  Reconnect on timeout = new CDN worker!")
	fmt.Println("════════════════════════════════════════════\n")

	select {}
}
