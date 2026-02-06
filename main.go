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
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gobwas/ws"
)

const (
	host     = "app.send.tg"
	wsPath   = "/internal/v1/p2c-socket/?EIO=4&transport=websocket"
	pollPath = "/internal/v1/p2c-socket/?EIO=4&transport=polling"
)

var cookie string
var popIPs []string

// Track results
var (
	mu       sync.Mutex
	seen     = make(map[string]string)    // id -> first source
	seenTime = make(map[string]time.Time) // id -> time
	wsWins   int
	pollWins int
)

func recordOrder(id, source string) {
	mu.Lock()
	defer mu.Unlock()

	if first, exists := seen[id]; exists {
		delay := time.Since(seenTime[id]).Milliseconds()
		fmt.Printf("      %s +%dms (first: %s)\n", source, delay, first)
		return
	}

	seen[id] = source
	seenTime[id] = time.Now()

	if strings.HasPrefix(source, "WS") {
		wsWins++
	} else {
		pollWins++
	}

	fmt.Printf("🥇 %s FIRST: %s  [WS:%d POLL:%d]\n", source, id[:16], wsWins, pollWins)
}

// ============ WebSocket ============

func runWS(id int, ip string) {
	name := fmt.Sprintf("WS%d", id)
	ipShort := ip[strings.LastIndex(ip, ".")+1:]

	for {
		conn, err := connectWS(ip)
		if err != nil {
			fmt.Printf("[%s@%s] connect err: %v\n", name, ipShort, err)
			time.Sleep(2 * time.Second)
			continue
		}

		// Handshake
		readFrame(conn)
		writeFrame(conn, []byte("40"))
		readFrame(conn)

		time.Sleep(30 * time.Millisecond)
		writeFrame(conn, []byte(`42["list:initialize"]`))
		time.Sleep(30 * time.Millisecond)
		writeFrame(conn, []byte(`42["list:snapshot",[]]`))

		fmt.Printf("[%s@%s] 🚀 connected\n", name, ipShort)

		for {
			data, op, err := readFrame(conn)
			if err != nil {
				fmt.Printf("[%s] disconnected: %v\n", name, err)
				break
			}

			if op == ws.OpText {
				if len(data) == 1 && data[0] == '2' {
					writeFrame(conn, []byte("3"))
					continue
				}
				if len(data) > 2 && data[0] == '4' && data[1] == '2' {
					parseOrder(data[2:], name)
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

func connectWS(ip string) (net.Conn, error) {
	dialer := ws.Dialer{
		Header: ws.HandshakeHeaderHTTP(http.Header{
			"Cookie": []string{cookie},
			"Origin": []string{"https://app.send.tg"},
		}),
		Timeout: 10 * time.Second,
		NetDial: func(ctx context.Context, network, addr string) (net.Conn, error) {
			conn, err := net.DialTimeout("tcp", ip+":443", 5*time.Second)
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

// ============ HTTP Polling (stable) ============

type poller struct {
	id   int
	ip   string
	name string
	conn net.Conn
	br   *bufio.Reader
	bw   *bufio.Writer
	sid  string
}

func (p *poller) connect() error {
	conn, err := net.DialTimeout("tcp", p.ip+":443", 5*time.Second)
	if err != nil {
		return err
	}

	if tc, ok := conn.(*net.TCPConn); ok {
		tc.SetNoDelay(true)
		tc.SetKeepAlive(true)
		tc.SetKeepAlivePeriod(10 * time.Second)
	}

	tlsConn := tls.Client(conn, &tls.Config{ServerName: host})
	if err := tlsConn.Handshake(); err != nil {
		conn.Close()
		return err
	}

	p.conn = tlsConn
	p.br = bufio.NewReaderSize(tlsConn, 32768)
	p.bw = bufio.NewWriterSize(tlsConn, 4096)
	return nil
}

func (p *poller) handshake() error {
	// GET handshake
	p.conn.SetDeadline(time.Now().Add(10 * time.Second))
	req := "GET " + pollPath + " HTTP/1.1\r\n" +
		"Host: " + host + "\r\n" +
		"Cookie: " + cookie + "\r\n" +
		"Accept: */*\r\n" +
		"Connection: keep-alive\r\n\r\n"
	p.bw.WriteString(req)
	p.bw.Flush()

	body, err := p.readResponse()
	if err != nil {
		return err
	}

	// Parse sid from: 0{"sid":"xxx",...}
	sidIdx := bytes.Index(body, []byte(`"sid":"`))
	if sidIdx == -1 {
		return fmt.Errorf("no sid in: %s", string(body[:min(100, len(body))]))
	}
	sidStart := sidIdx + 7
	sidEnd := bytes.IndexByte(body[sidStart:], '"')
	if sidEnd == -1 {
		return fmt.Errorf("bad sid")
	}
	p.sid = string(body[sidStart : sidStart+sidEnd])

	// Send Socket.IO connect: 40
	if err := p.send("40"); err != nil {
		return err
	}
	// Read ACK
	if _, err := p.poll(); err != nil {
		return err
	}

	// Initialize
	time.Sleep(30 * time.Millisecond)
	p.send(`42["list:initialize"]`)
	time.Sleep(30 * time.Millisecond)
	p.send(`42["list:snapshot",[]]`)

	return nil
}

func (p *poller) send(msg string) error {
	p.conn.SetDeadline(time.Now().Add(10 * time.Second))

	req := fmt.Sprintf("POST %s&sid=%s HTTP/1.1\r\n"+
		"Host: %s\r\n"+
		"Cookie: %s\r\n"+
		"Content-Type: text/plain;charset=UTF-8\r\n"+
		"Content-Length: %d\r\n"+
		"Connection: keep-alive\r\n\r\n%s",
		pollPath, p.sid, host, cookie, len(msg), msg)

	p.bw.WriteString(req)
	if err := p.bw.Flush(); err != nil {
		return err
	}

	_, err := p.readResponse()
	return err
}

func (p *poller) poll() ([]byte, error) {
	// Long timeout - server holds connection up to 25s
	p.conn.SetDeadline(time.Now().Add(60 * time.Second))

	req := fmt.Sprintf("GET %s&sid=%s HTTP/1.1\r\n"+
		"Host: %s\r\n"+
		"Cookie: %s\r\n"+
		"Accept: */*\r\n"+
		"Connection: keep-alive\r\n\r\n",
		pollPath, p.sid, host, cookie)

	p.bw.WriteString(req)
	if err := p.bw.Flush(); err != nil {
		return nil, err
	}

	return p.readResponse()
}

func (p *poller) readResponse() ([]byte, error) {
	// Status line
	line, err := p.br.ReadString('\n')
	if err != nil {
		return nil, fmt.Errorf("status: %v", err)
	}

	// Check status code
	if len(line) >= 12 {
		code, _ := strconv.Atoi(line[9:12])
		if code != 200 {
			return nil, fmt.Errorf("http %d", code)
		}
	}

	// Headers
	contentLen := 0
	chunked := false
	for {
		line, err := p.br.ReadString('\n')
		if err != nil {
			return nil, fmt.Errorf("header: %v", err)
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

	// Body
	var body []byte
	if chunked {
		for {
			sizeLine, err := p.br.ReadString('\n')
			if err != nil {
				return body, err
			}
			sizeLine = strings.TrimSpace(sizeLine)
			size, _ := strconv.ParseInt(sizeLine, 16, 64)
			if size == 0 {
				p.br.ReadString('\n')
				break
			}
			chunk := make([]byte, size)
			_, err = io.ReadFull(p.br, chunk)
			if err != nil {
				return body, err
			}
			body = append(body, chunk...)
			p.br.ReadString('\n')
		}
	} else if contentLen > 0 {
		body = make([]byte, contentLen)
		_, err = io.ReadFull(p.br, body)
	}

	return body, err
}

func (p *poller) close() {
	if p.conn != nil {
		p.conn.Close()
		p.conn = nil
	}
}

func runPoll(id int, ip string) {
	ipShort := ip[strings.LastIndex(ip, ".")+1:]
	p := &poller{id: id, ip: ip, name: fmt.Sprintf("POLL%d", id)}

	for {
		// Connect
		if err := p.connect(); err != nil {
			fmt.Printf("[%s@%s] connect err: %v\n", p.name, ipShort, err)
			time.Sleep(2 * time.Second)
			continue
		}

		// Handshake
		if err := p.handshake(); err != nil {
			fmt.Printf("[%s@%s] handshake err: %v\n", p.name, ipShort, err)
			p.close()
			time.Sleep(2 * time.Second)
			continue
		}

		fmt.Printf("[%s@%s] 🚀 connected (sid=%s...)\n", p.name, ipShort, p.sid[:12])

		// Poll loop
		for {
			data, err := p.poll()
			if err != nil {
				fmt.Printf("[%s] disconnected: %v\n", p.name, err)
				break
			}

			// Handle ping
			if len(data) == 1 && data[0] == '2' {
				p.send("3")
				continue
			}

			// Parse orders
			if len(data) > 5 {
				parseOrder(data, p.name)
			}
		}

		p.close()
		time.Sleep(1 * time.Second)
	}
}

// ============ Parser ============

var (
	opAddBytes = []byte(`"op":"add"`)
	idPrefix   = []byte(`"id":"`)
)

func parseOrder(data []byte, source string) {
	if !bytes.Contains(data, opAddBytes) {
		return
	}

	idx := bytes.Index(data, idPrefix)
	if idx == -1 {
		return
	}
	start := idx + 6
	end := bytes.IndexByte(data[start:], '"')
	if end == -1 || end > 30 {
		return
	}
	id := string(data[start : start+end])

	recordOrder(id, source)
}

// ============ Main ============

func main() {
	in := bufio.NewReader(os.Stdin)

	fmt.Println("╔═══════════════════════════════════════════╗")
	fmt.Println("║  RACE TEST: 3 WS vs 3 POLL                ║")
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
	popIPs = ips
	fmt.Printf("✅ Found %d POPs: %v\n", len(popIPs), popIPs)

	// Start 3 WebSockets
	fmt.Println("\n⏳ Starting 3 WebSockets...")
	for i := 1; i <= 3; i++ {
		go runWS(i, popIPs[(i-1)%len(popIPs)])
		time.Sleep(100 * time.Millisecond)
	}

	time.Sleep(1 * time.Second)

	// Start 3 Pollers
	fmt.Println("⏳ Starting 3 Pollers...")
	for i := 1; i <= 3; i++ {
		go runPoll(i, popIPs[(i-1)%len(popIPs)])
		time.Sleep(100 * time.Millisecond)
	}

	fmt.Println("\n════════════════════════════════════════════")
	fmt.Println("  3 WS vs 3 POLL - watching for orders...")
	fmt.Println("  🥇 = first to see order")
	fmt.Println("════════════════════════════════════════════\n")

	select {}
}
