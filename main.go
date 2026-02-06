package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
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
		fmt.Printf("   %s saw %s +%dms (first: %s)\n", source, id[:min(12, len(id))], delay, first)
		return
	}

	ordersFirst[id] = source
	ordersTimes[id] = time.Now()

	if source == "WS" {
		wsWins++
	} else {
		pollWins++
	}

	fmt.Printf("🥇 %s FIRST: %s (WS:%d POLL:%d)\n", source, id[:min(12, len(id))], wsWins, pollWins)
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// ========================= WebSocket =========================

func runWS(ip string) {
	for {
		conn, err := connectWS(ip)
		if err != nil {
			fmt.Printf("[WS] connect err: %v\n", err)
			time.Sleep(2 * time.Second)
			continue
		}

		// socket.io handshake
		_, _, _ = readFrame(conn) // typically "0{...}"
		_ = writeFrame(conn, []byte("40"))
		_, _, _ = readFrame(conn) // "40" ack

		time.Sleep(30 * time.Millisecond)
		_ = writeFrame(conn, []byte(`42["list:initialize"]`))
		time.Sleep(30 * time.Millisecond)
		_ = writeFrame(conn, []byte(`42["list:snapshot",[]]`))

		fmt.Printf("[WS] 🚀 connected\n")

		for {
			// keep a long read deadline so we don't hang forever if connection dies silently
			_ = conn.SetReadDeadline(time.Now().Add(120 * time.Second))

			data, op, err := readFrame(conn)
			if err != nil {
				fmt.Printf("[WS] err: %v\n", err)
				break
			}

			if op == ws.OpText {
				// engine.io ping/pong on text channel: "2" -> respond "3"
				if len(data) == 1 && data[0] == '2' {
					_ = writeFrame(conn, []byte("3"))
					continue
				}
				// socket.io message: "42[...]"
				if len(data) > 2 && data[0] == '4' && data[1] == '2' {
					parseOrder(data[2:], "WS")
				}
			} else if op == ws.OpPing {
				f := ws.NewPongFrame(data)
				f = ws.MaskFrameInPlace(f)
				_ = ws.WriteFrame(conn, f)
			} else if op == ws.OpClose {
				break
			}
		}

		_ = conn.Close()
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
				_ = tc.SetNoDelay(true)
			}
			return conn, nil
		},
		TLSConfig: &tls.Config{ServerName: host},
	}
	conn, _, _, err := dialer.Dial(context.Background(), "wss://"+host+wsPath)
	return conn, err
}

func writeFrame(conn net.Conn, data []byte) error {
	_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	frame := ws.NewTextFrame(data)
	frame = ws.MaskFrameInPlace(frame)
	return ws.WriteFrame(conn, frame)
}

func readFrame(conn net.Conn) ([]byte, ws.OpCode, error) {
	h, err := ws.ReadHeader(conn)
	if err != nil {
		return nil, 0, err
	}
	p := make([]byte, h.Length)
	if h.Length > 0 {
		if _, err := io.ReadFull(conn, p); err != nil {
			return nil, 0, err
		}
	}
	if h.Masked {
		ws.Cipher(p, h.Mask, 0)
	}
	return p, h.OpCode, nil
}

// ========================= Polling (Engine.IO v4) =========================

type eioHandshake struct {
	SID          string `json:"sid"`
	PingInterval int    `json:"pingInterval"` // ms
	PingTimeout  int    `json:"pingTimeout"`  // ms
}

func runPoll(ip string) {
	for {
		conn, br, bw, sid, readWait, err := pollConnect(ip)
		if err != nil {
			fmt.Printf("[POLL] connect err: %v\n", err)
			time.Sleep(2 * time.Second)
			continue
		}

		fmt.Printf("[POLL] 🚀 connected (sid=%s... wait=%s)\n", sid[:min(12, len(sid))], readWait)

		for {
			// One long-poll GET
			t := time.Now().UnixNano()

			_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
			fmt.Fprintf(bw, "GET %s&sid=%s&t=%d HTTP/1.1\r\n", pollPath, sid, t)
			fmt.Fprintf(bw, "Host: %s\r\n", host)
			fmt.Fprintf(bw, "Cookie: %s\r\n", cookie)
			fmt.Fprintf(bw, "Origin: https://app.send.tg\r\n")
			fmt.Fprintf(bw, "Connection: keep-alive\r\n")
			fmt.Fprintf(bw, "\r\n")
			if err := bw.Flush(); err != nil {
				fmt.Printf("[POLL] flush err: %v\n", err)
				break
			}

			// wait long enough for long-poll
			_ = conn.SetReadDeadline(time.Now().Add(readWait))

			body, code := readHTTPResponse(br)
			if code != 200 {
				fmt.Printf("[POLL] poll code: %d (len=%d)\n", code, len(body))
				break
			}
			if len(body) == 0 {
				continue
			}

			// Engine.IO polling can return multiple packets in one response
			packets := splitEIOPayload(body)
			for _, p := range packets {
				if len(p) == 0 {
					continue
				}

				// engine.io ping "2" => respond POST "3"
				if len(p) == 1 && p[0] == '2' {
					if err := pollSendPong(conn, br, bw, sid); err != nil {
						fmt.Printf("[POLL] pong err: %v\n", err)
						goto reconnect
					}
					continue
				}

				// socket.io message "42[...]"
				if len(p) > 2 && p[0] == '4' && p[1] == '2' {
					parseOrder(p[2:], "POLL")
					continue
				}

				// Optional debug:
				// fmt.Printf("[POLL] packet: %q\n", string(p))
			}
		}

	reconnect:
		_ = conn.Close()
		time.Sleep(1 * time.Second)
	}
}

func pollSendPong(conn net.Conn, br *bufio.Reader, bw *bufio.Writer, sid string) error {
	t := time.Now().UnixNano()
	msg := "3"

	_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	fmt.Fprintf(bw, "POST %s&sid=%s&t=%d HTTP/1.1\r\n", pollPath, sid, t)
	fmt.Fprintf(bw, "Host: %s\r\n", host)
	fmt.Fprintf(bw, "Cookie: %s\r\n", cookie)
	fmt.Fprintf(bw, "Origin: https://app.send.tg\r\n")
	fmt.Fprintf(bw, "Content-Type: text/plain;charset=UTF-8\r\n")
	fmt.Fprintf(bw, "Content-Length: %d\r\n", len(msg))
	fmt.Fprintf(bw, "Connection: keep-alive\r\n")
	fmt.Fprintf(bw, "\r\n")
	fmt.Fprintf(bw, "%s", msg)
	if err := bw.Flush(); err != nil {
		return err
	}

	_ = conn.SetReadDeadline(time.Now().Add(30 * time.Second))
	_, code := readHTTPResponse(br)
	if code != 200 {
		return fmt.Errorf("pong code: %d", code)
	}
	return nil
}

func pollConnect(ip string) (net.Conn, *bufio.Reader, *bufio.Writer, string, time.Duration, error) {
	conn, err := tls.DialWithDialer(
		&net.Dialer{Timeout: 5 * time.Second},
		"tcp",
		ip+":443",
		&tls.Config{ServerName: host},
	)
	if err != nil {
		return nil, nil, nil, "", 0, err
	}

	br := bufio.NewReaderSize(conn, 1<<16)
	bw := bufio.NewWriterSize(conn, 1<<12)

	// Step 1: Handshake GET (receive open packet: '0{...}')
	{
		t := time.Now().UnixNano()
		_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
		fmt.Fprintf(bw, "GET %s&t=%d HTTP/1.1\r\n", pollPath, t)
		fmt.Fprintf(bw, "Host: %s\r\n", host)
		fmt.Fprintf(bw, "Cookie: %s\r\n", cookie)
		fmt.Fprintf(bw, "Origin: https://app.send.tg\r\n")
		fmt.Fprintf(bw, "Connection: keep-alive\r\n")
		fmt.Fprintf(bw, "\r\n")
		if err := bw.Flush(); err != nil {
			_ = conn.Close()
			return nil, nil, nil, "", 0, err
		}

		_ = conn.SetReadDeadline(time.Now().Add(30 * time.Second))
		body, code := readHTTPResponse(br)
		if code != 200 {
			_ = conn.Close()
			return nil, nil, nil, "", 0, fmt.Errorf("handshake code: %d", code)
		}

		// Extract engine.io open packet(s) and parse sid/pingInterval/pingTimeout
		var hs eioHandshake
		found := false

		packets := splitEIOPayload(body)
		for _, p := range packets {
			if len(p) < 2 || p[0] != '0' {
				continue
			}
			// p = '0' + json
			if err := json.Unmarshal(p[1:], &hs); err == nil && hs.SID != "" {
				found = true
				break
			}
		}
		if !found {
			// fallback: try legacy string scan
			sidIdx := bytes.Index(body, []byte(`"sid":"`))
			if sidIdx == -1 {
				_ = conn.Close()
				return nil, nil, nil, "", 0, fmt.Errorf("no sid in handshake")
			}
			sid := string(body[sidIdx+7 : sidIdx+7+bytes.IndexByte(body[sidIdx+7:], '"')])
			hs.SID = sid
			// reasonable defaults if not parsed
			hs.PingInterval = 25000
			hs.PingTimeout = 20000
		}

		sid := hs.SID
		// long-poll wait: pingInterval + pingTimeout + запас
		readWait := time.Duration(hs.PingInterval+hs.PingTimeout+10000) * time.Millisecond
		if readWait < 60*time.Second {
			readWait = 60 * time.Second
		}
		if readWait > 180*time.Second {
			readWait = 180 * time.Second
		}

		// Step 2: Send "40" (socket.io connect)
		{
			t2 := time.Now().UnixNano()
			msg := "40"
			_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
			fmt.Fprintf(bw, "POST %s&sid=%s&t=%d HTTP/1.1\r\n", pollPath, sid, t2)
			fmt.Fprintf(bw, "Host: %s\r\n", host)
			fmt.Fprintf(bw, "Cookie: %s\r\n", cookie)
			fmt.Fprintf(bw, "Origin: https://app.send.tg\r\n")
			fmt.Fprintf(bw, "Content-Type: text/plain;charset=UTF-8\r\n")
			fmt.Fprintf(bw, "Content-Length: %d\r\n", len(msg))
			fmt.Fprintf(bw, "Connection: keep-alive\r\n")
			fmt.Fprintf(bw, "\r\n")
			fmt.Fprintf(bw, "%s", msg)
			if err := bw.Flush(); err != nil {
				_ = conn.Close()
				return nil, nil, nil, "", 0, err
			}

			_ = conn.SetReadDeadline(time.Now().Add(30 * time.Second))
			_, code := readHTTPResponse(br)
			if code != 200 {
				_ = conn.Close()
				return nil, nil, nil, "", 0, fmt.Errorf("send 40 code: %d", code)
			}
		}

		// Step 3: One GET to receive "40" ack and possibly buffered packets
		{
			t3 := time.Now().UnixNano()
			_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
			fmt.Fprintf(bw, "GET %s&sid=%s&t=%d HTTP/1.1\r\n", pollPath, sid, t3)
			fmt.Fprintf(bw, "Host: %s\r\n", host)
			fmt.Fprintf(bw, "Cookie: %s\r\n", cookie)
			fmt.Fprintf(bw, "Origin: https://app.send.tg\r\n")
			fmt.Fprintf(bw, "Connection: keep-alive\r\n")
			fmt.Fprintf(bw, "\r\n")
			if err := bw.Flush(); err != nil {
				_ = conn.Close()
				return nil, nil, nil, "", 0, err
			}

			_ = conn.SetReadDeadline(time.Now().Add(30 * time.Second))
			_, code := readHTTPResponse(br)
			if code != 200 {
				_ = conn.Close()
				return nil, nil, nil, "", 0, fmt.Errorf("ack code: %d", code)
			}
		}

		// Initialize
		time.Sleep(30 * time.Millisecond)
		if err := pollPost(conn, br, bw, sid, `42["list:initialize"]`); err != nil {
			_ = conn.Close()
			return nil, nil, nil, "", 0, err
		}
		time.Sleep(30 * time.Millisecond)
		if err := pollPost(conn, br, bw, sid, `42["list:snapshot",[]]`); err != nil {
			_ = conn.Close()
			return nil, nil, nil, "", 0, err
		}

		return conn, br, bw, sid, readWait, nil
	}
}

func pollPost(conn net.Conn, br *bufio.Reader, bw *bufio.Writer, sid, msg string) error {
	t := time.Now().UnixNano()

	_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	fmt.Fprintf(bw, "POST %s&sid=%s&t=%d HTTP/1.1\r\n", pollPath, sid, t)
	fmt.Fprintf(bw, "Host: %s\r\n", host)
	fmt.Fprintf(bw, "Cookie: %s\r\n", cookie)
	fmt.Fprintf(bw, "Origin: https://app.send.tg\r\n")
	fmt.Fprintf(bw, "Content-Type: text/plain;charset=UTF-8\r\n")
	fmt.Fprintf(bw, "Content-Length: %d\r\n", len(msg))
	fmt.Fprintf(bw, "Connection: keep-alive\r\n")
	fmt.Fprintf(bw, "\r\n")
	fmt.Fprintf(bw, "%s", msg)
	if err := bw.Flush(); err != nil {
		return err
	}

	_ = conn.SetReadDeadline(time.Now().Add(30 * time.Second))
	_, code := readHTTPResponse(br)
	if code != 200 {
		return fmt.Errorf("post code: %d", code)
	}
	return nil
}

// splitEIOPayload splits Engine.IO polling payload into packets.
// It supports:
// 1) record separator 0x1e (sometimes used)
// 2) length-prefixed "len:packet" format (Engine.IO v4 polling)
func splitEIOPayload(b []byte) [][]byte {
	if len(b) == 0 {
		return nil
	}

	// Variant A: 0x1e separators
	if bytes.IndexByte(b, 0x1e) >= 0 {
		parts := bytes.Split(b, []byte{0x1e})
		out := make([][]byte, 0, len(parts))
		for _, p := range parts {
			if len(p) > 0 {
				out = append(out, p)
			}
		}
		return out
	}

	// Variant B: length-prefixed: <digits>:<packet>...
	out := [][]byte{}
	i := 0
	for i < len(b) {
		j := i
		for j < len(b) && b[j] >= '0' && b[j] <= '9' {
			j++
		}
		if j == i || j >= len(b) || b[j] != ':' {
			// not in that format
			return [][]byte{b}
		}
		n, err := strconv.Atoi(string(b[i:j]))
		if err != nil || n < 0 {
			return [][]byte{b}
		}
		j++ // skip ':'
		if j+n > len(b) {
			return [][]byte{b}
		}
		out = append(out, b[j:j+n])
		i = j + n
	}
	if len(out) == 0 {
		return [][]byte{b}
	}
	return out
}

func readHTTPResponse(br *bufio.Reader) ([]byte, int) {
	line, err := br.ReadString('\n')
	if err != nil {
		return nil, 0
	}

	code := 0
	// HTTP/1.1 200 OK
	if len(line) >= 12 {
		code, _ = strconv.Atoi(strings.TrimSpace(line[9:12]))
	}

	contentLen := 0
	chunked := false
	for {
		h, err := br.ReadString('\n')
		if err != nil {
			return nil, code
		}
		if h == "\r\n" {
			break
		}
		lower := strings.ToLower(h)
		if strings.HasPrefix(lower, "content-length:") {
			fmt.Sscanf(h[15:], "%d", &contentLen)
		}
		if strings.Contains(lower, "transfer-encoding:") && strings.Contains(lower, "chunked") {
			chunked = true
		}
	}

	var body []byte
	if chunked {
		for {
			sizeLine, err := br.ReadString('\n')
			if err != nil {
				return body, code
			}
			sizeLine = strings.TrimSpace(sizeLine)
			if sizeLine == "" {
				continue
			}
			size, err := strconv.ParseInt(sizeLine, 16, 64)
			if err != nil {
				return body, code
			}
			if size == 0 {
				// read trailers until empty line
				for {
					l, err := br.ReadString('\n')
					if err != nil {
						break
					}
					if l == "\r\n" {
						break
					}
				}
				break
			}
			chunk := make([]byte, size)
			if _, err := io.ReadFull(br, chunk); err != nil {
				return body, code
			}
			body = append(body, chunk...)
			// trailing CRLF after chunk
			_, _ = br.ReadString('\n')
		}
	} else if contentLen > 0 {
		body = make([]byte, contentLen)
		_, _ = io.ReadFull(br, body)
	}

	return body, code
}

// ========================= Parser =========================

func parseOrder(data []byte, source string) {
	// data is the JSON payload part for socket.io "42[...]"
	if !bytes.Contains(data, []byte(`"op":"add"`)) {
		return
	}

	idIdx := bytes.Index(data, []byte(`"id":"`))
	if idIdx == -1 {
		return
	}
	start := idIdx + 6
	end := bytes.IndexByte(data[start:], '"')
	if end == -1 || end > 64 {
		return
	}
	id := string(data[start : start+end])

	recordOrder(id, source)
}

// ========================= Main =========================

func main() {
	in := bufio.NewReader(os.Stdin)

	fmt.Println("╔═══════════════════════════════════════════╗")
	fmt.Println("║  RACE: 1 WS vs 1 POLL (fixed polling)     ║")
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

	time.Sleep(2 * time.Second)

	fmt.Println("⏳ Starting Polling...")
	go runPoll(ip)

	fmt.Println("\n════════════════════════════════════════════")
	fmt.Println("  1 WS vs 1 POLL")
	fmt.Println("  🥇 = first to see order")
	fmt.Println("════════════════════════════════════════════\n")

	select {}
}
