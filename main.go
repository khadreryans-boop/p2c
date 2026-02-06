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

const numWebSockets = 20
const parallelTakes = 5

var (
	pauseTaking atomic.Bool
	seenOrders  sync.Map
)

var (
	reqPrefix []byte
	reqSuffix []byte
	cookie    string
	popIPs    []string
)

// ============ HTTP Client Pool ============

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
	totalLatency  atomic.Uint64
	minLatency    atomic.Uint64
	maxLatency    atomic.Uint64
	wins          atomic.Uint64
}

var clients []*httpClient

func newHTTPClient(name, ip string) *httpClient {
	c := &httpClient{name: name, ip: ip}
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
	c.br = bufio.NewReaderSize(tlsConn, 4096)
	c.bw = bufio.NewWriterSize(tlsConn, 2048)
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

	c.conn.SetDeadline(time.Now().Add(2 * time.Second))

	req := "HEAD /p2c/orders HTTP/1.1\r\n" +
		"Host: " + host + "\r\n" +
		"Cookie: " + cookie + "\r\n" +
		"Connection: keep-alive\r\n" +
		"\r\n"

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
		if len(line) > 15 && (line[0] == 'C' || line[0] == 'c') {
			fmt.Sscanf(line, "Content-Length: %d", &contentLen)
		}
	}

	if contentLen > 0 && contentLen < 4096 {
		tmp := make([]byte, contentLen)
		io.ReadFull(c.br, tmp)
	}

	c.lastUsed = time.Now()
	rttMs := uint64(dur.Milliseconds())
	c.lastRtt.Store(rttMs)

	c.totalRequests.Add(1)
	c.totalLatency.Add(rttMs)

	for {
		old := c.minLatency.Load()
		if rttMs >= old || c.minLatency.CompareAndSwap(old, rttMs) {
			break
		}
	}
	for {
		old := c.maxLatency.Load()
		if rttMs <= old || c.maxLatency.CompareAndSwap(old, rttMs) {
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
	for _, c := range clients {
		if c.ready.Load() && !c.inUse.Load() {
			avail = append(avail, cs{c, c.lastRtt.Load()})
		}
	}

	sort.Slice(avail, func(i, j int) bool {
		return avail[i].rtt < avail[j].rtt
	})

	usedIPs := make(map[string]int)
	var result []*httpClient

	for _, a := range avail {
		if len(result) >= count {
			break
		}
		if usedIPs[a.c.ip] < 2 {
			result = append(result, a.c)
			usedIPs[a.c.ip]++
		}
	}

	for _, a := range avail {
		if len(result) >= count {
			break
		}
		found := false
		for _, r := range result {
			if r == a.c {
				found = true
				break
			}
		}
		if !found {
			result = append(result, a.c)
		}
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

	ips := make(map[string]bool)
	for _, c := range best {
		ips[c.ip] = true
	}
	fmt.Printf("📥 [WS%02d] %s amt=%s (%d clients, %d IPs)\n", wsID, id, amt, len(best), len(ips))

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
		ipShort := r.client.ip[strings.LastIndex(r.client.ip, ".")+1:]
		s := fmt.Sprintf("%s[%s]:%d", r.client.name, ipShort, r.dur.Milliseconds())
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

func connectWS(cookie string, ip string) (net.Conn, error) {
	dialer := ws.Dialer{
		Header: ws.HandshakeHeaderHTTP(http.Header{
			"Cookie": []string{cookie},
			"Origin": []string{"https://app.send.tg"},
		}),
		Timeout: 10 * time.Second,
		NetDial: func(ctx context.Context, network, addr string) (net.Conn, error) {
			d := &net.Dialer{Timeout: 5 * time.Second}
			conn, err := d.DialContext(ctx, network, ip+":443")
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

func runWS(wsID int, cookie string, ip string, minCents int64, wg *sync.WaitGroup) {
	defer wg.Done()

	for {
		conn, err := connectWS(cookie, ip)
		if err != nil {
			fmt.Printf("[WS%02d@%s] err: %v\n", wsID, ip[strings.LastIndex(ip, ".")+1:], err)
			time.Sleep(2 * time.Second)
			continue
		}

		wsc := &wsConn{conn: conn}
		fmt.Printf("[WS%02d@%s] 🔌\n", wsID, ip[strings.LastIndex(ip, ".")+1:])

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

		fmt.Printf("[WS%02d@%s] 🚀\n", wsID, ip[strings.LastIndex(ip, ".")+1:])

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
		fmt.Printf("[WS%02d@%s] 🔄\n", wsID, ip[strings.LastIndex(ip, ".")+1:])
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
	fmt.Println("\n📊 STATS by IP:")

	ipData := make(map[string]struct {
		reqs, wins       uint64
		totalLat, minLat uint64
		clients          int
	})

	for _, c := range clients {
		d := ipData[c.ip]
		d.reqs += c.totalRequests.Load()
		d.wins += c.wins.Load()
		d.totalLat += c.totalLatency.Load()
		min := c.minLatency.Load()
		if min != 999999999 && (d.minLat == 0 || min < d.minLat) {
			d.minLat = min
		}
		d.clients++
		ipData[c.ip] = d
	}

	type ipStat struct {
		ip      string
		clients int
		min     uint64
		avg     uint64
		reqs    uint64
		wins    uint64
	}
	var stats []ipStat
	for ip, d := range ipData {
		avg := uint64(0)
		if d.reqs > 0 {
			avg = d.totalLat / d.reqs
		}
		stats = append(stats, ipStat{ip, d.clients, d.minLat, avg, d.reqs, d.wins})
	}
	sort.Slice(stats, func(i, j int) bool {
		return stats[i].min < stats[j].min
	})

	for i, s := range stats {
		marker := ""
		if i == 0 {
			marker = " ⭐"
		}
		fmt.Printf("  %s: %d clients | min=%dms avg=%dms | reqs=%d wins=%d%s\n",
			s.ip, s.clients, s.min, s.avg, s.reqs, s.wins, marker)
	}
}

// Measure IP latency
func measureIP(ip string) time.Duration {
	c := newHTTPClient("probe", ip)
	if err := c.connect(); err != nil {
		return 999 * time.Second
	}
	defer c.conn.Close()

	var total time.Duration
	for i := 0; i < 3; i++ {
		dur, err := c.warmup()
		if err != nil {
			return 999 * time.Second
		}
		total += dur
		time.Sleep(50 * time.Millisecond)
	}
	return total / 3
}

func main() {
	in := bufio.NewReader(os.Stdin)

	fmt.Println("╔═══════════════════════════════════════════╗")
	fmt.Println("║  P2C SNIPER - Smart IP Allocation         ║")
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
	fmt.Printf("✅ Found %d POP IPs\n", len(popIPs))

	// Measure latency to each IP
	fmt.Println("\n⏳ Measuring latency to each POP...")
	type ipLatency struct {
		ip  string
		lat time.Duration
	}
	var latencies []ipLatency

	for _, ip := range popIPs {
		lat := measureIP(ip)
		latencies = append(latencies, ipLatency{ip, lat})
		fmt.Printf("   %s: %dms\n", ip, lat.Milliseconds())
	}

	// Sort by latency (best first)
	sort.Slice(latencies, func(i, j int) bool {
		return latencies[i].lat < latencies[j].lat
	})

	// Allocate clients: more on faster IPs
	// Best: 7 clients, Middle: 5 clients, Worst: 3 clients
	clientCounts := []int{7, 5, 3}
	if len(latencies) > 3 {
		// If more than 3 IPs, distribute evenly with bonus to best
		clientCounts = make([]int, len(latencies))
		for i := range clientCounts {
			clientCounts[i] = 3
		}
		clientCounts[0] = 6
		if len(clientCounts) > 1 {
			clientCounts[1] = 5
		}
	}

	fmt.Println("\n📊 Client allocation (sorted by latency):")
	reqPrefix = []byte("POST " + takePathPrefix)
	reqSuffix = []byte(" HTTP/1.1\r\n" +
		"Host: " + host + "\r\n" +
		"Content-Type: application/json\r\n" +
		"Cookie: " + cookie + "\r\n" +
		"Content-Length: 2\r\n" +
		"\r\n{}")

	totalClients := 0
	for i, il := range latencies {
		count := 3
		if i < len(clientCounts) {
			count = clientCounts[i]
		}

		marker := ""
		if i == 0 {
			marker = " ⭐ BEST"
		}
		fmt.Printf("   %s: %d clients (lat=%dms)%s\n", il.ip, count, il.lat.Milliseconds(), marker)

		for j := 0; j < count; j++ {
			name := fmt.Sprintf("%s_%d", il.ip[strings.LastIndex(il.ip, ".")+1:], j+1)
			c := newHTTPClient(name, il.ip)
			clients = append(clients, c)
		}
		totalClients += count
	}

	fmt.Printf("\n⏳ Connecting %d clients...\n", totalClients)

	var connWg sync.WaitGroup
	for _, c := range clients {
		connWg.Add(1)
		go func(cl *httpClient) {
			defer connWg.Done()
			if cl.connect() == nil {
				cl.warmup()
			}
		}(c)
	}
	connWg.Wait()

	ready := 0
	for _, c := range clients {
		if c.ready.Load() {
			ready++
		}
	}
	fmt.Printf("✅ %d/%d clients ready\n", ready, len(clients))

	// Warmup loop
	for i, c := range clients {
		go func(idx int, cl *httpClient) {
			time.Sleep(time.Duration(idx*100) * time.Millisecond)
			for {
				time.Sleep(2 * time.Second)
				if cl.inUse.Load() {
					continue
				}
				if !cl.ready.Load() {
					cl.connect()
					continue
				}
				_, err := cl.warmup()
				if err != nil {
					cl.ready.Store(false)
					cl.connect()
				}
			}
		}(i, c)
	}

	go func() {
		for {
			time.Sleep(30 * time.Second)
			printStats()
		}
	}()

	// Distribute WS across POPs (more on best)
	fmt.Printf("\n⏳ Starting %d WebSockets...\n", numWebSockets)

	var wsWg sync.WaitGroup
	wsPerIP := make(map[string]int)

	// Allocate WS similar to clients: more on faster IPs
	wsAlloc := []int{10, 6, 4} // For 3 IPs
	if len(latencies) > 3 {
		wsAlloc = make([]int, len(latencies))
		base := numWebSockets / len(latencies)
		for i := range wsAlloc {
			wsAlloc[i] = base
		}
		wsAlloc[0] += numWebSockets - base*len(latencies) + 2
	}

	wsID := 1
	for i, il := range latencies {
		count := 4
		if i < len(wsAlloc) {
			count = wsAlloc[i]
		}
		for j := 0; j < count && wsID <= numWebSockets; j++ {
			wsWg.Add(1)
			go runWS(wsID, cookie, il.ip, minCents, &wsWg)
			wsPerIP[il.ip]++
			wsID++
			time.Sleep(50 * time.Millisecond)
		}
	}

	fmt.Println("\n════════════════════════════════════════════")
	fmt.Printf("  %d POPs | %d clients | %d WS\n", len(popIPs), len(clients), numWebSockets)
	fmt.Println("  More resources on faster POPs!")
	fmt.Println("════════════════════════════════════════════\n")

	wsWg.Wait()
}
