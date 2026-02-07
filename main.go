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
	"os/exec"
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
)

var (
	cookie   string
	serverIP string
)

// Stats
var (
	rawWins   atomic.Int64
	curlWins  atomic.Int64
	totalSeen atomic.Int64
	seenMu    sync.Mutex
	seen      = make(map[string]struct{})
)

// HTTP/1.1 raw socket client
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

	// Drain headers
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

// Curl client
func curlTake(orderID string) (int, time.Duration, error) {
	url := fmt.Sprintf("https://%s%s%s", host, takePathPrefix, orderID)

	start := time.Now()

	cmd := exec.Command("curl", "-s", "-o", "/dev/null", "-w", "%{http_code}",
		"-X", "POST",
		"-H", "Cookie: "+cookie,
		"-H", "Content-Type: application/json",
		"-d", "{}",
		"--connect-timeout", "2",
		"--max-time", "3",
		url)

	output, err := cmd.Output()
	dur := time.Since(start)

	if err != nil {
		return 0, dur, err
	}

	code := 0
	fmt.Sscanf(string(output), "%d", &code)

	return code, dur, nil
}

// WebSocket listener
func runWS(raw *rawClient) {
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
					handleOrder(data[2:], raw)
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

func handleOrder(data []byte, raw *rawClient) {
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

	// Race RAW vs CURL
	type result struct {
		name string
		code int
		dur  time.Duration
		err  error
	}

	ch := make(chan result, 2)

	go func() {
		code, dur, err := raw.take(orderID)
		ch <- result{"RAW", code, dur, err}
	}()

	go func() {
		code, dur, err := curlTake(orderID)
		ch <- result{"CURL", code, dur, err}
	}()

	r1 := <-ch
	r2 := <-ch

	// Determine winner (by duration)
	var winner, loser result
	if r1.dur < r2.dur {
		winner, loser = r1, r2
	} else {
		winner, loser = r2, r1
	}

	if winner.name == "RAW" {
		rawWins.Add(1)
	} else {
		curlWins.Add(1)
	}

	winStr := fmt.Sprintf("%s:%dms", winner.name, winner.dur.Milliseconds())
	if winner.err != nil {
		winStr = fmt.Sprintf("%s:ERR", winner.name)
	} else {
		winStr = fmt.Sprintf("%s:%dms(%d)", winner.name, winner.dur.Milliseconds(), winner.code)
	}

	loseStr := fmt.Sprintf("%s:%dms", loser.name, loser.dur.Milliseconds())
	if loser.err != nil {
		loseStr = fmt.Sprintf("%s:ERR", loser.name)
	} else {
		loseStr = fmt.Sprintf("%s:%dms(%d)", loser.name, loser.dur.Milliseconds(), loser.code)
	}

	diff := loser.dur.Milliseconds() - winner.dur.Milliseconds()

	fmt.Printf("🏁 %s wins (+%dms) | %s vs %s | amt=%s (RAW:%d CURL:%d)\n",
		winner.name, diff, winStr, loseStr, amt, rawWins.Load(), curlWins.Load())

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
	fmt.Println("║  RACE: Raw Socket vs Curl                 ║")
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

	// Create raw client
	fmt.Println("\n⏳ Creating Raw Socket client...")
	raw, err := newRawClient()
	if err != nil {
		fmt.Printf("Raw error: %v\n", err)
		return
	}
	fmt.Println("✅ Raw Socket ready")

	// Test curl
	fmt.Println("⏳ Testing curl...")
	cmd := exec.Command("curl", "--version")
	output, err := cmd.Output()
	if err != nil {
		fmt.Printf("curl not found: %v\n", err)
		return
	}
	fmt.Printf("✅ %s\n", strings.Split(string(output), "\n")[0])

	// Warmup raw socket
	go func() {
		for {
			time.Sleep(500 * time.Millisecond)
			raw.warmup()
		}
	}()

	fmt.Println("\n════════════════════════════════════════════")
	fmt.Println("  RAW = HTTP/1.1 persistent connection")
	fmt.Println("  CURL = new connection each request")
	fmt.Println("  Both send take request simultaneously")
	fmt.Println("════════════════════════════════════════════\n")

	// Start WebSocket
	go runWS(raw)

	// Stats
	go func() {
		for {
			time.Sleep(60 * time.Second)
			rw, cw := rawWins.Load(), curlWins.Load()
			total := rw + cw
			fmt.Printf("\n📊 STATS: RAW=%d (%.0f%%) CURL=%d (%.0f%%) total=%d\n\n",
				rw, float64(rw)/float64(max(total, 1))*100,
				cw, float64(cw)/float64(max(total, 1))*100,
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
