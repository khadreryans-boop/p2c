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
	"golang.org/x/net/http2"
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
	h1Wins    atomic.Int64
	h2Wins    atomic.Int64
	totalSeen atomic.Int64
	seenMu    sync.Mutex
	seen      = make(map[string]struct{})
)

// HTTP/1.1 client - raw socket for max speed
type http1Client struct {
	conn net.Conn
	br   *bufio.Reader
	bw   *bufio.Writer
	mu   sync.Mutex
}

func newHttp1Client() (*http1Client, error) {
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

	return &http1Client{
		conn: tlsConn,
		br:   bufio.NewReaderSize(tlsConn, 4096),
		bw:   bufio.NewWriterSize(tlsConn, 2048),
	}, nil
}

func (c *http1Client) take(orderID string) (int, time.Duration, error) {
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

	// Drain headers
	for {
		l, _ := c.br.ReadString('\n')
		if l == "\r\n" || l == "" {
			break
		}
	}

	return code, dur, nil
}

func (c *http1Client) warmup() {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.conn.SetDeadline(time.Now().Add(1 * time.Second))

	req := "POST /internal/v1/p2c/accounts HTTP/1.1\r\n" +
		"Host: " + host + "\r\n" +
		"Cookie: " + cookie + "\r\n" +
		"Content-Type: application/json\r\n" +
		"Content-Length: 2\r\n" +
		"Connection: keep-alive\r\n\r\n{}"

	c.bw.WriteString(req)
	c.bw.Flush()
	c.br.ReadString('\n')

	// Drain
	for {
		l, _ := c.br.ReadString('\n')
		if l == "\r\n" || l == "" {
			break
		}
	}
}

// HTTP/2 client
type http2Client struct {
	client *http.Client
}

func newHttp2Client() *http2Client {
	transport := &http2.Transport{
		TLSClientConfig: &tls.Config{
			ServerName: host,
		},
		DialTLSContext: func(ctx context.Context, network, addr string, cfg *tls.Config) (net.Conn, error) {
			conn, err := net.DialTimeout("tcp", serverIP+":443", 2*time.Second)
			if err != nil {
				return nil, err
			}
			if tc, ok := conn.(*net.TCPConn); ok {
				tc.SetNoDelay(true)
			}
			tlsConn := tls.Client(conn, cfg)
			if err := tlsConn.Handshake(); err != nil {
				conn.Close()
				return nil, err
			}
			return tlsConn, nil
		},
	}

	return &http2Client{
		client: &http.Client{
			Transport: transport,
			Timeout:   5 * time.Second,
		},
	}
}

func (c *http2Client) take(orderID string) (int, time.Duration, error) {
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

func (c *http2Client) warmup() {
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

// WebSocket listener
func runWS(h1 *http1Client, h2 *http2Client) {
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
					handleOrder(data[2:], h1, h2)
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

func handleOrder(data []byte, h1 *http1Client, h2 *http2Client) {
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

	// Race HTTP/1.1 vs HTTP/2
	type result struct {
		name string
		code int
		dur  time.Duration
		err  error
	}

	ch := make(chan result, 2)

	go func() {
		code, dur, err := h1.take(orderID)
		ch <- result{"H1", code, dur, err}
	}()

	go func() {
		code, dur, err := h2.take(orderID)
		ch <- result{"H2", code, dur, err}
	}()

	r1 := <-ch
	r2 := <-ch

	// Determine winner
	var winner, loser result
	if r1.dur < r2.dur {
		winner, loser = r1, r2
	} else {
		winner, loser = r2, r1
	}

	if strings.HasPrefix(winner.name, "H1") {
		h1Wins.Add(1)
	} else {
		h2Wins.Add(1)
	}

	winStr := fmt.Sprintf("%s:%dms", winner.name, winner.dur.Milliseconds())
	if winner.err != nil {
		winStr = fmt.Sprintf("%s:ERR", winner.name)
	} else if winner.code != 200 && winner.code != 400 {
		winStr = fmt.Sprintf("%s:%dms(%d)", winner.name, winner.dur.Milliseconds(), winner.code)
	}

	loseStr := fmt.Sprintf("%s:%dms", loser.name, loser.dur.Milliseconds())
	if loser.err != nil {
		loseStr = fmt.Sprintf("%s:ERR", loser.name)
	} else if loser.code != 200 && loser.code != 400 {
		loseStr = fmt.Sprintf("%s:%dms(%d)", loser.name, loser.dur.Milliseconds(), loser.code)
	}

	diff := loser.dur.Milliseconds() - winner.dur.Milliseconds()

	fmt.Printf("🏁 %s wins (+%dms) | %s vs %s | amt=%s (H1:%d H2:%d)\n",
		winner.name, diff, winStr, loseStr, amt, h1Wins.Load(), h2Wins.Load())

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
	fmt.Println("║  RACE: HTTP/1.1 vs HTTP/2 Take Requests   ║")
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

	// Create HTTP/1.1 client
	fmt.Println("\n⏳ Creating HTTP/1.1 client...")
	h1, err := newHttp1Client()
	if err != nil {
		fmt.Printf("H1 error: %v\n", err)
		return
	}
	fmt.Println("✅ HTTP/1.1 ready")

	// Create HTTP/2 client
	fmt.Println("⏳ Creating HTTP/2 client...")
	h2 := newHttp2Client()
	// Warmup to establish H2 connection
	h2.warmup()
	fmt.Println("✅ HTTP/2 ready")

	// Warmup goroutines
	go func() {
		for {
			time.Sleep(200 * time.Millisecond)
			h1.warmup()
		}
	}()

	go func() {
		for {
			time.Sleep(200 * time.Millisecond)
			h2.warmup()
		}
	}()

	fmt.Println("\n════════════════════════════════════════════")
	fmt.Println("  HTTP/1.1 (raw socket) vs HTTP/2 (h2)")
	fmt.Println("  Both send take request simultaneously")
	fmt.Println("════════════════════════════════════════════\n")

	// Start WebSocket
	go runWS(h1, h2)

	// Stats
	go func() {
		for {
			time.Sleep(60 * time.Second)
			h1w, h2w := h1Wins.Load(), h2Wins.Load()
			total := h1w + h2w
			fmt.Printf("\n📊 STATS: H1=%d (%.0f%%) H2=%d (%.0f%%) total=%d\n\n",
				h1w, float64(h1w)/float64(max(total, 1))*100,
				h2w, float64(h2w)/float64(max(total, 1))*100,
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
