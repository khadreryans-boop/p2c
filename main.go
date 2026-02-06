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

// Track who sees order first
var (
	ordersMu    sync.Mutex
	ordersFirst = make(map[string]string) // id -> "WS" or "POLL"
	ordersTimes = make(map[string]time.Time)
)

func recordOrder(id, source string) {
	ordersMu.Lock()
	defer ordersMu.Unlock()

	if _, exists := ordersFirst[id]; exists {
		// Already seen
		first := ordersFirst[id]
		firstTime := ordersTimes[id]
		delay := time.Since(firstTime).Milliseconds()
		fmt.Printf("   %s saw %s +%dms (first: %s)\n", source, id[:12], delay, first)
		return
	}

	ordersFirst[id] = source
	ordersTimes[id] = time.Now()
	fmt.Printf("🥇 %s FIRST: %s\n", source, id[:12])
}

// ============ WebSocket ============

func runWebSocket(ip string) {
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

	for {
		conn, _, _, err := dialer.Dial(context.Background(), "wss://"+host+wsPath)
		if err != nil {
			fmt.Printf("[WS] connect error: %v\n", err)
			time.Sleep(2 * time.Second)
			continue
		}

		// Handshake
		readFrame(conn) // OPEN
		writeFrame(conn, []byte("40"))
		readFrame(conn) // ACK

		time.Sleep(30 * time.Millisecond)
		writeFrame(conn, []byte(`42["list:initialize"]`))
		time.Sleep(30 * time.Millisecond)
		writeFrame(conn, []byte(`42["list:snapshot",[]]`))

		fmt.Println("[WS] 🚀 Connected")

		for {
			data, op, err := readFrame(conn)
			if err != nil {
				fmt.Printf("[WS] error: %v\n", err)
				break
			}

			if op == ws.OpText {
				if len(data) == 1 && data[0] == '2' {
					writeFrame(conn, []byte("3"))
					continue
				}
				if len(data) > 2 && data[0] == '4' && data[1] == '2' {
					parseAndRecord(data[2:], "WS")
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

// ============ HTTP Polling ============

func runPolling(ip string) {
	for {
		conn, br, bw, sid, err := pollConnect(ip)
		if err != nil {
			fmt.Printf("[POLL] connect error: %v\n", err)
			time.Sleep(2 * time.Second)
			continue
		}

		fmt.Printf("[POLL] 🚀 Connected (sid=%s...)\n", sid[:12])

		for {
			data, err := pollRequest(conn, br, bw, sid)
			if err != nil {
				fmt.Printf("[POLL] error: %v\n", err)
				break
			}

			if len(data) == 1 && data[0] == '2' {
				pollSend(conn, br, bw, sid, "3")
				continue
			}

			if bytes.Contains(data, []byte(`"op":"add"`)) {
				parseAndRecord(data, "POLL")
			}
		}

		conn.Close()
		time.Sleep(1 * time.Second)
	}
}

func pollConnect(ip string) (net.Conn, *bufio.Reader, *bufio.Writer, string, error) {
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	conn, err := dialer.Dial("tcp", ip+":443")
	if err != nil {
		return nil, nil, nil, "", err
	}

	if tc, ok := conn.(*net.TCPConn); ok {
		tc.SetNoDelay(true)
	}

	tlsConn := tls.Client(conn, &tls.Config{
		ServerName: host,
		MinVersion: tls.VersionTLS12,
	})
	if err := tlsConn.Handshake(); err != nil {
		conn.Close()
		return nil, nil, nil, "", err
	}

	br := bufio.NewReaderSize(tlsConn, 16384)
	bw := bufio.NewWriterSize(tlsConn, 4096)

	// Handshake
	req := "GET " + pollPath + " HTTP/1.1\r\n" +
		"Host: " + host + "\r\n" +
		"Cookie: " + cookie + "\r\n" +
		"Connection: keep-alive\r\n\r\n"
	bw.WriteString(req)
	bw.Flush()

	body, err := readHTTPResponse(tlsConn, br)
	if err != nil {
		tlsConn.Close()
		return nil, nil, nil, "", err
	}

	// Parse sid
	sidIdx := bytes.Index(body, []byte(`"sid":"`))
	if sidIdx == -1 {
		tlsConn.Close()
		return nil, nil, nil, "", fmt.Errorf("no sid")
	}
	sidStart := sidIdx + 7
	sidEnd := bytes.IndexByte(body[sidStart:], '"')
	sid := string(body[sidStart : sidStart+sidEnd])

	// Socket.IO connect
	pollSend(tlsConn, br, bw, sid, "40")
	pollRequest(tlsConn, br, bw, sid) // ACK

	time.Sleep(30 * time.Millisecond)
	pollSend(tlsConn, br, bw, sid, `42["list:initialize"]`)
	time.Sleep(30 * time.Millisecond)
	pollSend(tlsConn, br, bw, sid, `42["list:snapshot",[]]`)

	return tlsConn, br, bw, sid, nil
}

func pollSend(conn net.Conn, br *bufio.Reader, bw *bufio.Writer, sid, msg string) error {
	conn.SetDeadline(time.Now().Add(5 * time.Second))
	req := fmt.Sprintf("POST %s&sid=%s HTTP/1.1\r\n"+
		"Host: %s\r\n"+
		"Cookie: %s\r\n"+
		"Content-Type: text/plain;charset=UTF-8\r\n"+
		"Content-Length: %d\r\n"+
		"Connection: keep-alive\r\n\r\n%s",
		pollPath, sid, host, cookie, len(msg), msg)
	bw.WriteString(req)
	bw.Flush()
	readHTTPResponse(conn, br)
	return nil
}

func pollRequest(conn net.Conn, br *bufio.Reader, bw *bufio.Writer, sid string) ([]byte, error) {
	conn.SetDeadline(time.Now().Add(30 * time.Second))
	req := fmt.Sprintf("GET %s&sid=%s HTTP/1.1\r\n"+
		"Host: %s\r\n"+
		"Cookie: %s\r\n"+
		"Connection: keep-alive\r\n\r\n",
		pollPath, sid, host, cookie)
	bw.WriteString(req)
	bw.Flush()
	return readHTTPResponse(conn, br)
}

func readHTTPResponse(conn net.Conn, br *bufio.Reader) ([]byte, error) {
	line, err := br.ReadString('\n')
	if err != nil {
		return nil, err
	}
	_ = line

	contentLen := 0
	chunked := false
	for {
		line, err := br.ReadString('\n')
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

	var body []byte
	if chunked {
		for {
			sizeLine, _ := br.ReadString('\n')
			sizeLine = strings.TrimSpace(sizeLine)
			size, _ := strconv.ParseInt(sizeLine, 16, 64)
			if size == 0 {
				br.ReadString('\n')
				break
			}
			chunk := make([]byte, size)
			io.ReadFull(br, chunk)
			body = append(body, chunk...)
			br.ReadString('\n')
		}
	} else if contentLen > 0 {
		body = make([]byte, contentLen)
		io.ReadFull(br, body)
	}

	return body, nil
}

// ============ Parser ============

var (
	opAddBytes = []byte(`"op":"add"`)
	idPrefix   = []byte(`"id":"`)
)

func parseAndRecord(data []byte, source string) {
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
	fmt.Println("║  WS vs POLL - Who Sees Order First?       ║")
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

	ip := ips[0]

	fmt.Println("\n⏳ Starting WebSocket...")
	go runWebSocket(ip)

	time.Sleep(1 * time.Second)

	fmt.Println("⏳ Starting HTTP Polling...")
	go runPolling(ip)

	fmt.Println("\n════════════════════════════════════════════")
	fmt.Println("  Watching for new orders...")
	fmt.Println("  🥇 = first to see order")
	fmt.Println("════════════════════════════════════════════\n")

	// Stats every 30s
	go func() {
		for {
			time.Sleep(30 * time.Second)
			ordersMu.Lock()
			wsFirst := 0
			pollFirst := 0
			for _, first := range ordersFirst {
				if first == "WS" {
					wsFirst++
				} else {
					pollFirst++
				}
			}
			ordersMu.Unlock()
			fmt.Printf("\n📊 STATS: WS first=%d | POLL first=%d\n\n", wsFirst, pollFirst)
		}
	}()

	select {}
}
