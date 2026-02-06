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

	if source == "WS" {
		wsWins++
	} else {
		pollWins++
	}

	fmt.Printf("🥇 %s FIRST: %s (WS:%d POLL:%d)\n", source, id[:12], wsWins, pollWins)
}

// ============ WebSocket ============

func runWS(ip string) {
	for {
		conn, err := connectWS(ip)
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

		fmt.Printf("[WS] 🚀 connected\n")

		for {
			data, op, err := readFrame(conn)
			if err != nil {
				fmt.Printf("[WS] err: %v\n", err)
				break
			}

			if op == ws.OpText {
				if len(data) == 1 && data[0] == '2' {
					writeFrame(conn, []byte("3"))
					continue
				}
				if len(data) > 2 && data[0] == '4' && data[1] == '2' {
					parseOrder(data[2:], "WS")
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

// ============ Polling (exact copy from debug) ============

func runPoll(ip string) {
	for {
		conn, br, bw, sid, err := pollConnect(ip)
		if err != nil {
			fmt.Printf("[POLL] connect err: %v\n", err)
			time.Sleep(2 * time.Second)
			continue
		}

		fmt.Printf("[POLL] 🚀 connected (sid=%s...)\n", sid[:12])

		// Poll loop
		for {
			conn.SetDeadline(time.Now().Add(30 * time.Second))
			fmt.Fprintf(bw, "GET %s&sid=%s HTTP/1.1\r\n", pollPath, sid)
			fmt.Fprintf(bw, "Host: %s\r\n", host)
			fmt.Fprintf(bw, "Cookie: %s\r\n", cookie)
			fmt.Fprintf(bw, "Origin: https://app.send.tg\r\n")
			fmt.Fprintf(bw, "Connection: keep-alive\r\n")
			fmt.Fprintf(bw, "\r\n")
			bw.Flush()

			body, code := readHTTPResponse(br)
			if code != 200 {
				fmt.Printf("[POLL] poll code: %d\n", code)
				break
			}

			// Ping
			if len(body) == 1 && body[0] == '2' {
				conn.SetDeadline(time.Now().Add(5 * time.Second))
				fmt.Fprintf(bw, "POST %s&sid=%s HTTP/1.1\r\n", pollPath, sid)
				fmt.Fprintf(bw, "Host: %s\r\n", host)
				fmt.Fprintf(bw, "Cookie: %s\r\n", cookie)
				fmt.Fprintf(bw, "Origin: https://app.send.tg\r\n")
				fmt.Fprintf(bw, "Content-Type: text/plain;charset=UTF-8\r\n")
				fmt.Fprintf(bw, "Content-Length: 1\r\n")
				fmt.Fprintf(bw, "Connection: keep-alive\r\n")
				fmt.Fprintf(bw, "\r\n")
				fmt.Fprintf(bw, "3")
				bw.Flush()

				_, code = readHTTPResponse(br)
				if code != 200 {
					fmt.Printf("[POLL] pong code: %d\n", code)
					break
				}
				continue
			}

			// Parse orders
			if len(body) > 5 {
				parseOrder(body, "POLL")
			}
		}

		conn.Close()
		time.Sleep(1 * time.Second)
	}
}

func pollConnect(ip string) (net.Conn, *bufio.Reader, *bufio.Writer, string, error) {
	conn, err := tls.DialWithDialer(
		&net.Dialer{Timeout: 5 * time.Second},
		"tcp",
		ip+":443",
		&tls.Config{ServerName: host},
	)
	if err != nil {
		return nil, nil, nil, "", err
	}

	br := bufio.NewReaderSize(conn, 16384)
	bw := bufio.NewWriterSize(conn, 4096)

	// Step 1: Handshake
	conn.SetDeadline(time.Now().Add(10 * time.Second))
	fmt.Fprintf(bw, "GET %s HTTP/1.1\r\n", pollPath)
	fmt.Fprintf(bw, "Host: %s\r\n", host)
	fmt.Fprintf(bw, "Cookie: %s\r\n", cookie)
	fmt.Fprintf(bw, "Origin: https://app.send.tg\r\n")
	fmt.Fprintf(bw, "Connection: keep-alive\r\n")
	fmt.Fprintf(bw, "\r\n")
	bw.Flush()

	body, code := readHTTPResponse(br)
	if code != 200 {
		conn.Close()
		return nil, nil, nil, "", fmt.Errorf("handshake code: %d", code)
	}

	// Parse sid
	sidIdx := bytes.Index(body, []byte(`"sid":"`))
	if sidIdx == -1 {
		conn.Close()
		return nil, nil, nil, "", fmt.Errorf("no sid")
	}
	sid := string(body[sidIdx+7 : sidIdx+7+bytes.IndexByte(body[sidIdx+7:], '"')])

	// Step 2: Send 40
	conn.SetDeadline(time.Now().Add(10 * time.Second))
	msg := "40"
	fmt.Fprintf(bw, "POST %s&sid=%s HTTP/1.1\r\n", pollPath, sid)
	fmt.Fprintf(bw, "Host: %s\r\n", host)
	fmt.Fprintf(bw, "Cookie: %s\r\n", cookie)
	fmt.Fprintf(bw, "Origin: https://app.send.tg\r\n")
	fmt.Fprintf(bw, "Content-Type: text/plain;charset=UTF-8\r\n")
	fmt.Fprintf(bw, "Content-Length: %d\r\n", len(msg))
	fmt.Fprintf(bw, "Connection: keep-alive\r\n")
	fmt.Fprintf(bw, "\r\n")
	fmt.Fprintf(bw, "%s", msg)
	bw.Flush()

	_, code = readHTTPResponse(br)
	if code != 200 {
		conn.Close()
		return nil, nil, nil, "", fmt.Errorf("send 40 code: %d", code)
	}

	// Step 3: Poll for ACK
	conn.SetDeadline(time.Now().Add(30 * time.Second))
	fmt.Fprintf(bw, "GET %s&sid=%s HTTP/1.1\r\n", pollPath, sid)
	fmt.Fprintf(bw, "Host: %s\r\n", host)
	fmt.Fprintf(bw, "Cookie: %s\r\n", cookie)
	fmt.Fprintf(bw, "Origin: https://app.send.tg\r\n")
	fmt.Fprintf(bw, "Connection: keep-alive\r\n")
	fmt.Fprintf(bw, "\r\n")
	bw.Flush()

	_, code = readHTTPResponse(br)
	if code != 200 {
		conn.Close()
		return nil, nil, nil, "", fmt.Errorf("ack code: %d", code)
	}

	// Initialize
	time.Sleep(30 * time.Millisecond)

	conn.SetDeadline(time.Now().Add(5 * time.Second))
	msg = `42["list:initialize"]`
	fmt.Fprintf(bw, "POST %s&sid=%s HTTP/1.1\r\n", pollPath, sid)
	fmt.Fprintf(bw, "Host: %s\r\n", host)
	fmt.Fprintf(bw, "Cookie: %s\r\n", cookie)
	fmt.Fprintf(bw, "Origin: https://app.send.tg\r\n")
	fmt.Fprintf(bw, "Content-Type: text/plain;charset=UTF-8\r\n")
	fmt.Fprintf(bw, "Content-Length: %d\r\n", len(msg))
	fmt.Fprintf(bw, "Connection: keep-alive\r\n")
	fmt.Fprintf(bw, "\r\n")
	fmt.Fprintf(bw, "%s", msg)
	bw.Flush()
	readHTTPResponse(br)

	time.Sleep(30 * time.Millisecond)

	conn.SetDeadline(time.Now().Add(5 * time.Second))
	msg = `42["list:snapshot",[]]`
	fmt.Fprintf(bw, "POST %s&sid=%s HTTP/1.1\r\n", pollPath, sid)
	fmt.Fprintf(bw, "Host: %s\r\n", host)
	fmt.Fprintf(bw, "Cookie: %s\r\n", cookie)
	fmt.Fprintf(bw, "Origin: https://app.send.tg\r\n")
	fmt.Fprintf(bw, "Content-Type: text/plain;charset=UTF-8\r\n")
	fmt.Fprintf(bw, "Content-Length: %d\r\n", len(msg))
	fmt.Fprintf(bw, "Connection: keep-alive\r\n")
	fmt.Fprintf(bw, "\r\n")
	fmt.Fprintf(bw, "%s", msg)
	bw.Flush()
	readHTTPResponse(br)

	return conn, br, bw, sid, nil
}

func readHTTPResponse(br *bufio.Reader) ([]byte, int) {
	line, err := br.ReadString('\n')
	if err != nil {
		return nil, 0
	}

	code := 0
	if len(line) >= 12 {
		code, _ = strconv.Atoi(strings.TrimSpace(line[9:12]))
	}

	contentLen := 0
	chunked := false
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return nil, code
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

	return body, code
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
	fmt.Println("║  RACE: 1 WS vs 1 POLL (debug code)        ║")
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
	ip := ips[0]
	fmt.Printf("✅ Using IP: %s\n", ip)

	fmt.Println("\n⏳ Starting WebSocket...")
	go runWS(ip)

	time.Sleep(2 * time.Second) // Wait for WS to connect

	fmt.Println("⏳ Starting Polling...")
	go runPoll(ip)

	fmt.Println("\n════════════════════════════════════════════")
	fmt.Println("  1 WS vs 1 POLL")
	fmt.Println("  🥇 = first to see order")
	fmt.Println("════════════════════════════════════════════\n")

	select {}
}
