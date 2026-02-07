package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gobwas/ws"
	"github.com/valyala/fasthttp"
)

const (
	host           = "app.send.tg"
	wsPath         = "/internal/v1/p2c-socket/?EIO=4&transport=websocket"
	takePathPrefix = "/internal/v1/p2c/payments/take/"
)

var (
	cookie   string
	serverIP string
)

// Stats
var (
	rawWins   atomic.Int64
	fastWins  atomic.Int64
	netWins   atomic.Int64
	totalSeen atomic.Int64
	seenMu    sync.Mutex
	seen      = make(map[string]struct{})
)

// ============ Raw Socket HTTP/1.1 ============

type rawClient struct {
	conn net.Conn
	br   *bufio.Reader
	bw   *bufio.Writer
	mu   sync.Mutex
}

func newRawClient() (*rawClient, error) {
	conn, err := net.DialTimeout("tcp", serverIP+":443", 2*time.Second)
	if err != nil {
		return nil, err
	}

	if tc, ok := conn.(*net.TCPConn); ok {
		tc.SetNoDelay(true)
	}

	tlsConn := tls.Client(conn, &tls.Config{ServerName: host})
	if err := tlsConn.Handshake(); err != nil {
		conn.Close()
		return nil, err
	}

	return &rawClient{
		conn: tlsConn,
		br:   bufio.NewReaderSize(tlsConn, 4096),
		bw:   bufio.NewWriterSize(tlsConn, 2048),
	}, nil
}

func (c *rawClient) take(orderID string) (int, time.Duration, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.conn.SetDeadline(time.Now().Add(2 * time.Second))

	req := fmt.Sprintf("POST %s%s HTTP/1.1\r\n"+
		"Host: %s\r\n"+
		"Cookie: %s\r\n"+
		"Content-Type: application/json\r\n"+
		"Content-Length: 2\r\n"+
		"Connection: keep-alive\r\n\r\n{}",
		takePathPrefix, orderID, host, cookie)

	start := time.Now()
	c.bw.WriteString(req)
	if err := c.bw.Flush(); err != nil {
		return 0, 0, err
	}

	line, err := c.br.ReadString('\n')
	dur := time.Since(start)

	if err != nil {
		return 0, dur, err
	}

	if len(line) < 12 {
		return 0, dur, fmt.Errorf("short")
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

func (c *rawClient) warmup() {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.conn.SetDeadline(time.Now().Add(500 * time.Millisecond))

	req := "POST /internal/v1/p2c/accounts HTTP/1.1\r\n" +
		"Host: " + host + "\r\n" +
		"Cookie: " + cookie + "\r\n" +
		"Content-Type: application/json\r\n" +
		"Content-Length: 2\r\n" +
		"Connection: keep-alive\r\n\r\n{}"

	c.bw.WriteString(req)
	c.bw.Flush()
	c.br.ReadString('\n')

	for {
		l, _ := c.br.ReadString('\n')
		if l == "\r\n" || l == "" {
			break
		}
	}
}

// ============ FastHTTP Client ============

type fastClient struct {
	client *fasthttp.Client
}

func newFastClient() *fastClient {
	return &fastClient{
		client: &fasthttp.Client{
			MaxConnsPerHost:     10,
			MaxIdleConnDuration: 30 * time.Second,
			ReadTimeout:         2 * time.Second,
			WriteTimeout:        2 * time.Second,
			TLSConfig:           &tls.Config{ServerName: host},
			Dial: func(addr string) (net.Conn, error) {
				conn, err := net.DialTimeout("tcp", serverIP+":443", 2*time.Second)
				if err != nil {
					return nil, err
				}
				if tc, ok := conn.(*net.TCPConn); ok {
					tc.SetNoDelay(true)
				}
				return conn, nil
			},
		},
	}
}

func (c *fastClient) take(orderID string) (int, time.Duration, error) {
	url := "https://" + host + takePathPrefix + orderID

	req := fasthttp.AcquireRequest()
	resp := fasthttp.AcquireResponse()
	defer fasthttp.ReleaseRequest(req)
	defer fasthttp.ReleaseResponse(resp)

	req.SetRequestURI(url)
	req.Header.SetMethod("POST")
	req.Header.Set("Cookie", cookie)
	req.Header.Set("Content-Type", "application/json")
	req.SetBody([]byte("{}"))

	start := time.Now()
	err := c.client.Do(req, resp)
	dur := time.Since(start)

	if err != nil {
		return 0, dur, err
	}

	return resp.StatusCode(), dur, nil
}

func (c *fastClient) warmup() {
	url := "https://" + host + "/internal/v1/p2c/accounts"

	req := fasthttp.AcquireRequest()
	resp := fasthttp.AcquireResponse()
	defer fasthttp.ReleaseRequest(req)
	defer fasthttp.ReleaseResponse(resp)

	req.SetRequestURI(url)
	req.Header.SetMethod("POST")
	req.Header.Set("Cookie", cookie)
	req.Header.Set("Content-Type", "application/json")
	req.SetBody([]byte("{}"))

	c.client.Do(req, resp)
}

// ============ net/http Client ============

type netClient struct {
	client *http.Client
}

func newNetClient() *netClient {
	transport := &http.Transport{
		TLSClientConfig: &tls.Config{ServerName: host},
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			conn, err := net.DialTimeout("tcp", serverIP+":443", 2*time.Second)
			if err != nil {
				return nil, err
			}
			if tc, ok := conn.(*net.TCPConn); ok {
				tc.SetNoDelay(true)
			}
			return conn, nil
		},
		MaxIdleConns:        10,
		MaxIdleConnsPerHost: 10,
		IdleConnTimeout:     30 * time.Second,
	}

	return &netClient{
		client: &http.Client{
			Transport: transport,
			Timeout:   5 * time.Second,
		},
	}
}

func (c *netClient) take(orderID string) (int, time.Duration, error) {
	url := "https://" + host + takePathPrefix + orderID

	req, _ := http.NewRequest("POST", url, strings.NewReader("{}"))
	req.Header.Set("Cookie", cookie)
	req.Header.Set("Content-Type", "application/json")

	start := time.Now()
	resp, err := c.client.Do(req)
	dur := time.Since(start)

	if err != nil {
		return 0, dur, err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)

	return resp.StatusCode, dur, nil
}

func (c *netClient) warmup() {
	url := "https://" + host + "/internal/v1/p2c/accounts"

	req, _ := http.NewRequest("POST", url, strings.NewReader("{}"))
	req.Header.Set("Cookie", cookie)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.client.Do(req)
	if err == nil {
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
}

// ============ WebSocket Listener ============

func runWS(raw *rawClient, fast *fastClient, netc *netClient) {
	for {
		conn, err := connectWS()
		if err != nil {
			fmt.Printf("[WS] connect err: %v\n", err)
			time.Sleep(2 * time.Second)
			continue
		}

		readFrame(conn)
		writeFrame(conn, []byte("40"))
		readFrame(conn)

		time.Sleep(30 * time.Millisecond)
		writeFrame(conn, []byte(`42["list:initialize"]`))
		time.Sleep(30 * time.Millisecond)
		writeFrame(conn, []byte(`42["list:snapshot",[]]`))

		fmt.Println("[WS] 🚀 connected")

		for {
			data, op, err := readFrame(conn)
			if err != nil {
				break
			}

			if op == ws.OpText {
				if len(data) == 1 && data[0] == '2' {
					writeFrame(conn, []byte("3"))
					continue
				}
				if len(data) > 2 && data[0] == '4' && data[1] == '2' {
					handleOrder(data[2:], raw, fast, netc)
				}
			} else if op == ws.OpPing {
				f := ws.NewPongFrame(data)
				f = ws.MaskFrameInPlace(f)
				ws.WriteFrame(conn, f)
			} else if op == ws.OpClose {
				break
			}
		}

		conn.Close()
		time.Sleep(1 * time.Second)
	}
}

func handleOrder(data []byte, raw *rawClient, fast *fastClient, netc *netClient) {
	if !bytes.Contains(data, []byte(`"op":"add"`)) {
		return
	}

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

	// Dedupe
	seenMu.Lock()
	if _, ok := seen[orderID]; ok {
		seenMu.Unlock()
		return
	}
	seen[orderID] = struct{}{}
	seenMu.Unlock()

	totalSeen.Add(1)

	// Parse amount
	var amt string
	if idx := bytes.Index(data, []byte(`"in_amount":"`)); idx != -1 {
		s := idx + 13
		e := bytes.IndexByte(data[s:], '"')
		if e != -1 && e < 20 {
			amt = string(data[s : s+e])
		}
	}

	// Race all three
	type result struct {
		name string
		code int
		dur  time.Duration
		err  error
	}

	ch := make(chan result, 3)

	go func() {
		code, dur, err := raw.take(orderID)
		ch <- result{"RAW", code, dur, err}
	}()

	go func() {
		code, dur, err := fast.take(orderID)
		ch <- result{"FAST", code, dur, err}
	}()

	go func() {
		code, dur, err := netc.take(orderID)
		ch <- result{"NET", code, dur, err}
	}()

	// Collect all results
	results := make([]result, 3)
	for i := 0; i < 3; i++ {
		results[i] = <-ch
	}

	// Sort by duration (find winner)
	winner := results[0]
	for _, r := range results[1:] {
		if r.dur < winner.dur {
			winner = r
		}
	}

	switch winner.name {
	case "RAW":
		rawWins.Add(1)
	case "FAST":
		fastWins.Add(1)
	case "NET":
		netWins.Add(1)
	}

	// Format output
	var parts []string
	for _, r := range results {
		if r.err != nil {
			parts = append(parts, fmt.Sprintf("%s:ERR", r.name))
		} else {
			parts = append(parts, fmt.Sprintf("%s:%dms(%d)", r.name, r.dur.Milliseconds(), r.code))
		}
	}

	fmt.Printf("🏁 %s wins | %s | amt=%s (RAW:%d FAST:%d NET:%d)\n",
		winner.name, strings.Join(parts, " "), amt,
		rawWins.Load(), fastWins.Load(), netWins.Load())

	// Cleanup
	go func() {
		time.Sleep(5 * time.Second)
		seenMu.Lock()
		delete(seen, orderID)
		seenMu.Unlock()
	}()
}

func connectWS() (net.Conn, error) {
	dialer := ws.Dialer{
		Header: ws.HandshakeHeaderHTTP(http.Header{
			"Cookie": []string{cookie},
			"Origin": []string{"https://app.send.tg"},
		}),
		Timeout: 10 * time.Second,
		NetDial: func(ctx context.Context, network, addr string) (net.Conn, error) {
			conn, err := net.DialTimeout("tcp", serverIP+":443", 5*time.Second)
			if err != nil {
				return nil, err
			}
			if tc, ok := conn.(*net.TCPConn); ok {
				tc.SetNoDelay(true)
			}
			return conn, nil
		},
		TLSConfig: &tls.Config{ServerName: host},
	}
	conn, _, _, err := dialer.Dial(context.Background(), "wss://"+host+wsPath)
	return conn, err
}

func writeFrame(conn net.Conn, data []byte) {
	frame := ws.NewTextFrame(data)
	frame = ws.MaskFrameInPlace(frame)
	ws.WriteFrame(conn, frame)
}

func readFrame(conn net.Conn) ([]byte, ws.OpCode, error) {
	h, err := ws.ReadHeader(conn)
	if err != nil {
		return nil, 0, err
	}
	p := make([]byte, h.Length)
	if h.Length > 0 {
		io.ReadFull(conn, p)
	}
	if h.Masked {
		ws.Cipher(p, h.Mask, 0)
	}
	return p, h.OpCode, nil
}

func main() {
	in := bufio.NewReader(os.Stdin)

	fmt.Println("╔═══════════════════════════════════════════╗")
	fmt.Println("║  RACE: Raw vs FastHTTP vs net/http (TAKE) ║")
	fmt.Println("╚═══════════════════════════════════════════╝")

	fmt.Print("\naccess_token cookie:\n> ")
	cookie, _ = in.ReadString('\n')
	cookie = strings.TrimSpace(cookie)
	if !strings.HasPrefix(cookie, "access_token=") {
		fmt.Println("Invalid")
		return
	}

	fmt.Println("\n⏳ Resolving DNS...")
	ips, err := net.LookupHost(host)
	if err != nil {
		fmt.Printf("DNS error: %v\n", err)
		return
	}
	serverIP = ips[0]
	fmt.Printf("✅ Server IP: %s\n", serverIP)

	// Create clients
	fmt.Println("\n⏳ Creating clients...")

	raw, err := newRawClient()
	if err != nil {
		fmt.Printf("Raw error: %v\n", err)
		return
	}
	fmt.Println("✅ Raw Socket ready")

	fast := newFastClient()
	fast.warmup()
	fmt.Println("✅ FastHTTP ready")

	netc := newNetClient()
	netc.warmup()
	fmt.Println("✅ net/http ready")

	// Warmup goroutines
	go func() {
		for {
			time.Sleep(500 * time.Millisecond)
			raw.warmup()
		}
	}()

	go func() {
		for {
			time.Sleep(500 * time.Millisecond)
			fast.warmup()
		}
	}()

	go func() {
		for {
			time.Sleep(500 * time.Millisecond)
			netc.warmup()
		}
	}()

	fmt.Println("\n════════════════════════════════════════════")
	fmt.Println("  RAW  = raw socket HTTP/1.1 keep-alive")
	fmt.Println("  FAST = valyala/fasthttp")
	fmt.Println("  NET  = standard net/http")
	fmt.Println("  All send take request simultaneously")
	fmt.Println("════════════════════════════════════════════\n")

	// Start WebSocket
	go runWS(raw, fast, netc)

	// Stats
	go func() {
		for {
			time.Sleep(60 * time.Second)
			rw, fw, nw := rawWins.Load(), fastWins.Load(), netWins.Load()
			total := rw + fw + nw
			fmt.Printf("\n📊 STATS: RAW=%d (%.0f%%) FAST=%d (%.0f%%) NET=%d (%.0f%%) total=%d\n\n",
				rw, float64(rw)/float64(max(total, 1))*100,
				fw, float64(fw)/float64(max(total, 1))*100,
				nw, float64(nw)/float64(max(total, 1))*100,
				total)
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
