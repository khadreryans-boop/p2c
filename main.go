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

// Track who sees first
var (
	ordersMu    sync.Mutex
	ordersFirst = make(map[string]string)
	ordersTimes = make(map[string]time.Time)
	wsWins      int
	pollWins    int
)

func recordOrder(id, source string) {
	ordersMu.Lock()
	defer ordersMu.Unlock()

	if first, exists := ordersFirst[id]; exists {
		delay := time.Since(ordersTimes[id]).Milliseconds()
		fmt.Printf("   %s saw %s +%dms (first: %s)\n", source, id[:12], delay, first)
		return
	}

	ordersFirst[id] = source
	ordersTimes[id] = time.Now()

	if strings.HasPrefix(source, "WS") {
		wsWins++
	} else {
		pollWins++
	}

	fmt.Printf("🥇 %s FIRST: %s (WS:%d POLL:%d)\n", source, id[:12], wsWins, pollWins)
}

// ============ WebSocket ============

func runWS(id int, ip string) {
	source := fmt.Sprintf("WS%d", id)
	ipShort := ip[strings.LastIndex(ip, ".")+1:]

	for {
		conn, err := connectWS(ip)
		if err != nil {
			fmt.Printf("[%s] connect err: %v\n", source, err)
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

		fmt.Printf("[%s@%s] 🚀 connected\n", source, ipShort)

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
					parseOrder(data[2:], source)
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

// ============ HTTP Polling (Robust) ============

type poller struct {
	id     int
	ip     string
	conn   net.Conn
	br     *bufio.Reader
	bw     *bufio.Writer
	sid    string
	source string
}

func runPoll(id int, ip string) {
	p := &poller{
		id:     id,
		ip:     ip,
		source: fmt.Sprintf("POLL%d", id),
	}
	ipShort := ip[strings.LastIndex(ip, ".")+1:]

	for {
		if err := p.connect(); err != nil {
			fmt.Printf("[%s] connect err: %v\n", p.source, err)
			time.Sleep(2 * time.Second)
			continue
		}

		fmt.Printf("[%s@%s] 🚀 connected (sid=%s...)\n", p.source, ipShort, p.sid[:12])

		errCount := 0
		for {
			data, err := p.poll()
			if err != nil {
				errCount++
				fmt.Printf("[%s] poll err #%d: %v\n", p.source, errCount, err)
				if errCount >= 3 {
					break // Reconnect after 3 errors
				}
				time.Sleep(100 * time.Millisecond)
				continue
			}
			errCount = 0

			if len(data) == 1 && data[0] == '2' {
				if err := p.send("3"); err != nil {
					fmt.Printf("[%s] pong err: %v\n", p.source, err)
					break
				}
				continue
			}

			if len(data) > 5 {
				parseOrder(data, p.source)
			}
		}

		p.close()
		time.Sleep(1 * time.Second)
	}
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
	p.br = bufio.NewReaderSize(tlsConn, 16384)
	p.bw = bufio.NewWriterSize(tlsConn, 4096)

	// Engine.IO handshake
	p.conn.SetDeadline(time.Now().Add(10 * time.Second))

	req := "GET " + pollPath + " HTTP/1.1\r\n" +
		"Host: " + host + "\r\n" +
		"Cookie: " + cookie + "\r\n" +
		"Accept: */*\r\n" +
		"Origin: https://app.send.tg\r\n" +
		"Connection: keep-alive\r\n\r\n"

	p.bw.WriteString(req)
	p.bw.Flush()

	body, code, err := p.readResponse()
	if err != nil {
		return fmt.Errorf("handshake read: %v", err)
	}
	if code != 200 {
		return fmt.Errorf("handshake code: %d", code)
	}

	// Parse sid
	sidIdx := bytes.Index(body, []byte(`"sid":"`))
	if sidIdx == -1 {
		return fmt.Errorf("no sid")
	}
	sidStart := sidIdx + 7
	sidEnd := bytes.IndexByte(body[sidStart:], '"')
	if sidEnd == -1 {
		return fmt.Errorf("invalid sid")
	}
	p.sid = string(body[sidStart : sidStart+sidEnd])

	// Socket.IO connect
	if err := p.send("40"); err != nil {
		return fmt.Errorf("send 40: %v", err)
	}

	// Poll for ACK
	if _, err := p.poll(); err != nil {
		return fmt.Errorf("poll ack: %v", err)
	}

	time.Sleep(30 * time.Millisecond)
	p.send(`42["list:initialize"]`)
	time.Sleep(30 * time.Millisecond)
	p.send(`42["list:snapshot",[]]`)

	return nil
}

func (p *poller) send(msg string) error {
	p.conn.SetDeadline(time.Now().Add(5 * time.Second))

	req := fmt.Sprintf("POST %s&sid=%s HTTP/1.1\r\n"+
		"Host: %s\r\n"+
		"Cookie: %s\r\n"+
		"Content-Type: text/plain;charset=UTF-8\r\n"+
		"Content-Length: %d\r\n"+
		"Accept: */*\r\n"+
		"Origin: https://app.send.tg\r\n"+
		"Connection: keep-alive\r\n\r\n%s",
		pollPath, p.sid, host, cookie, len(msg), msg)

	p.bw.WriteString(req)
	if err := p.bw.Flush(); err != nil {
		return err
	}

	_, code, err := p.readResponse()
	if err != nil {
		return err
	}
	if code != 200 {
		return fmt.Errorf("http %d", code)
	}
	return nil
}

func (p *poller) poll() ([]byte, error) {
	p.conn.SetDeadline(time.Now().Add(30 * time.Second))

	req := fmt.Sprintf("GET %s&sid=%s HTTP/1.1\r\n"+
		"Host: %s\r\n"+
		"Cookie: %s\r\n"+
		"Accept: */*\r\n"+
		"Origin: https://app.send.tg\r\n"+
		"Connection: keep-alive\r\n\r\n",
		pollPath, p.sid, host, cookie)

	p.bw.WriteString(req)
	if err := p.bw.Flush(); err != nil {
		return nil, err
	}

	body, code, err := p.readResponse()
	if err != nil {
		return nil, err
	}
	if code != 200 {
		return nil, fmt.Errorf("http %d", code)
	}
	return body, nil
}

func (p *poller) readResponse() ([]byte, int, error) {
	line, err := p.br.ReadString('\n')
	if err != nil {
		return nil, 0, err
	}

	code := 0
	if len(line) >= 12 {
		code, _ = strconv.Atoi(strings.TrimSpace(line[9:12]))
	}

	contentLen := 0
	chunked := false
	for {
		line, err := p.br.ReadString('\n')
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
			sizeLine, err := p.br.ReadString('\n')
			if err != nil {
				return body, code, err
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
				return body, code, err
			}
			body = append(body, chunk...)
			p.br.ReadString('\n')
		}
	} else if contentLen > 0 {
		body = make([]byte, contentLen)
		_, err = io.ReadFull(p.br, body)
		if err != nil {
			return body, code, err
		}
	}

	return body, code, nil
}

func (p *poller) close() {
	if p.conn != nil {
		p.conn.Close()
		p.conn = nil
	}
}

// ============ Parser ============

func parseOrder(data []byte, source string) {
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
	id := string(data[start : start+end])

	recordOrder(id, source)
}

// ============ Main ============

func main() {
	in := bufio.NewReader(os.Stdin)

	fmt.Println("╔═══════════════════════════════════════════╗")
	fmt.Println("║  RACE: 2 WS vs 2 POLL (Robust)            ║")
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
	fmt.Printf("✅ Found %d POPs: %v\n", len(ips), ips)

	// Start 2 WebSockets
	fmt.Println("\n⏳ Starting 2 WebSockets...")
	go runWS(1, ips[0])
	time.Sleep(100 * time.Millisecond)
	go runWS(2, ips[1%len(ips)])

	time.Sleep(500 * time.Millisecond)

	// Start 2 Pollers
	fmt.Println("⏳ Starting 2 Pollers...")
	go runPoll(1, ips[0])
	time.Sleep(100 * time.Millisecond)
	go runPoll(2, ips[1%len(ips)])

	fmt.Println("\n════════════════════════════════════════════")
	fmt.Println("  2 WS vs 2 POLL - watching for orders...")
	fmt.Println("  🥇 = first to see order")
	fmt.Println("════════════════════════════════════════════\n")

	select {}
}
