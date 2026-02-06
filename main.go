
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

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

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

// ========================= WebSocket =========================

func runWS(ip string) {
	for {
		conn, err := connectWS(ip)
		if err != nil {
			fmt.Printf("[WS] connect err: %v\n", err)
			time.Sleep(2 * time.Second)
			continue
		}

		// Engine.IO open packet
		_, _, _ = readFrame(conn) // "0{...}"
		_ = writeFrame(conn, []byte("40"))
		_, _, _ = readFrame(conn) // "40"

		time.Sleep(30 * time.Millisecond)
		_ = writeFrame(conn, []byte(`42["list:initialize"]`))
		time.Sleep(30 * time.Millisecond)
		_ = writeFrame(conn, []byte(`42["list:snapshot",[]]`))

		fmt.Printf("[WS] 🚀 connected\n")

		for {
			_ = conn.SetReadDeadline(time.Now().Add(120 * time.Second))
			data, op, err := readFrame(conn)
			if err != nil {
				fmt.Printf("[WS] err: %v\n", err)
				break
			}

			if op == ws.OpText {
				// ping/pong in Engine.IO
				if len(data) == 1 && data[0] == '2' {
					_ = writeFrame(conn, []byte("3"))
					continue
				}
				// socket.io message
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
	MaxPayload   int    `json:"maxPayload"`   // optional
}

func runPoll() {
	for {
		conn, br, bw, sid, readWait, err := pollConnect()
		if err != nil {
			fmt.Printf("[POLL] connect err: %v\n", err)
			time.Sleep(2 * time.Second)
			continue
		}

		fmt.Printf("[POLL] 🚀 connected (sid=%s... wait=%s)\n", sid[:min(12, len(sid))], readWait)

		for {
			t := time.Now().UnixNano()

			_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
			fmt.Fprintf(bw, "GET %s&sid=%s&t=%d HTTP/1.1\r\n", pollPath, sid, t)
			fmt.Fprintf(bw, "Host: %s\r\n", host)
			writeBrowserHeaders(bw)
			fmt.Fprintf(bw, "Cookie: %s\r\n", cookie)
			fmt.Fprintf(bw, "Connection: keep-alive\r\n\r\n")

			if err := bw.Flush(); err != nil {
				fmt.Printf("[POLL] flush err: %v\n", err)
				break
			}

			_ = conn.SetReadDeadline(time.Now().Add(readWait))
			body, code := readHTTPResponse(br)
			if code != 200 {
				fmt.Printf("[POLL] poll code: %d body=%q\n", code, safeBody(body))
				break
			}
			if len(body) == 0 {
				continue
			}

			for _, p := range splitEIOPayload(body) {
				if len(p) == 0 {
					continue
				}

				if len(p) == 1 && p[0] == '2' {
					if err := pollPostPackets(conn, br, bw, sid, "3"); err != nil {
						fmt.Printf("[POLL] pong err: %v\n", err)
						goto reconnect
					}
					continue
				}

				if len(p) > 2 && p[0] == '4' && p[1] == '2' {
					parseOrder(p[2:], "POLL")
				}
			}
		}

	reconnect:
		_ = conn.Close()
		time.Sleep(1 * time.Second)
	}
}

func writeBrowserHeaders(bw *bufio.Writer) {
	// максимально “безопасно” для твоего парсера: просим identity (без gzip/br)
	fmt.Fprintf(bw, "Accept: application/json, text/plain, */*\r\n")
	fmt.Fprintf(bw, "Accept-Language: en,ru;q=0.9,de;q=0.8\r\n")
	fmt.Fprintf(bw, "Accept-Encoding: identity\r\n")
	fmt.Fprintf(bw, "Cache-Control: no-cache\r\n")
	fmt.Fprintf(bw, "Pragma: no-cache\r\n")

	fmt.Fprintf(bw, "User-Agent: Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/142.0.0.0 Safari/537.36 OPR/126.0.0.0\r\n")

	// client hints (часто проверяют)
	fmt.Fprintf(bw, "Sec-CH-UA: \"Chromium\";v=\"142\", \"Opera GX\";v=\"126\", \"Not_A Brand\";v=\"99\"\r\n")
	fmt.Fprintf(bw, "Sec-CH-UA-Mobile: ?0\r\n")
	fmt.Fprintf(bw, "Sec-CH-UA-Platform: \"Windows\"\r\n")

	fmt.Fprintf(bw, "Referer: https://app.send.tg/p2c/orders\r\n")
	fmt.Fprintf(bw, "Origin: https://app.send.tg\r\n")

	fmt.Fprintf(bw, "Sec-Fetch-Dest: empty\r\n")
	fmt.Fprintf(bw, "Sec-Fetch-Mode: cors\r\n")
	fmt.Fprintf(bw, "Sec-Fetch-Site: same-origin\r\n")

	fmt.Fprintf(bw, "Priority: u=1, i\r\n")
}

func pollConnect() (net.Conn, *bufio.Reader, *bufio.Writer, string, time.Duration, error) {
	conn, err := tls.DialWithDialer(
		&net.Dialer{Timeout: 5 * time.Second},
		"tcp",
		host+":443",
		&tls.Config{ServerName: host},
	)
	if err != nil {
		return nil, nil, nil, "", 0, err
	}

	br := bufio.NewReaderSize(conn, 1<<16)
	bw := bufio.NewWriterSize(conn, 1<<12)

	// 1) Handshake GET (без sid)
	t := time.Now().UnixNano()
	_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	fmt.Fprintf(bw, "GET %s&t=%d HTTP/1.1\r\n", pollPath, t)
	fmt.Fprintf(bw, "Host: %s\r\n", host)
	writeBrowserHeaders(bw)
	fmt.Fprintf(bw, "Cookie: %s\r\n", cookie)
	fmt.Fprintf(bw, "Connection: keep-alive\r\n\r\n")
	if err := bw.Flush(); err != nil {
		_ = conn.Close()
		return nil, nil, nil, "", 0, err
	}

	_ = conn.SetReadDeadline(time.Now().Add(30 * time.Second))
	body, code := readHTTPResponse(br)
	fmt.Printf("[POLL] handshake status=%d body=%q\n", code, safeBody(body))
	if code != 200 {
		_ = conn.Close()
		return nil, nil, nil, "", 0, fmt.Errorf("handshake code: %d body=%q", code, safeBody(body))
	}

	var hs eioHandshake
	found := false
	for _, p := range splitEIOPayload(body) {
		if len(p) >= 2 && p[0] == '0' {
			if err := json.Unmarshal(p[1:], &hs); err == nil && hs.SID != "" {
				found = true
				break
			}
		}
	}
	if !found {
		_ = conn.Close()
		return nil, nil, nil, "", 0, fmt.Errorf("no open packet body=%q", safeBody(body))
	}

	sid := hs.SID
	readWait := time.Duration(hs.PingInterval+hs.PingTimeout+10000) * time.Millisecond
	if readWait < 60*time.Second {
		readWait = 60 * time.Second
	}
	if readWait > 180*time.Second {
		readWait = 180 * time.Second
	}

	// 2) POST "40" (socket.io connect) как EIO payload
	if err := pollPostPackets(conn, br, bw, sid, "40"); err != nil {
		_ = conn.Close()
		return nil, nil, nil, "", 0, err
	}

	// 3) GET ack (ВАЖНО: те же заголовки!)
	{
		t3 := time.Now().UnixNano()
		_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
		fmt.Fprintf(bw, "GET %s&sid=%s&t=%d HTTP/1.1\r\n", pollPath, sid, t3)
		fmt.Fprintf(bw, "Host: %s\r\n", host)
		writeBrowserHeaders(bw)
		fmt.Fprintf(bw, "Cookie: %s\r\n", cookie)
		fmt.Fprintf(bw, "Connection: keep-alive\r\n\r\n")
		if err := bw.Flush(); err != nil {
			_ = conn.Close()
			return nil, nil, nil, "", 0, err
		}

		_ = conn.SetReadDeadline(time.Now().Add(30 * time.Second))
		body, code := readHTTPResponse(br)
		if code != 200 {
			_ = conn.Close()
			return nil, nil, nil, "", 0, fmt.Errorf("ack code: %d body=%q", code, safeBody(body))
		}
		_ = body
	}

	// 4) init events
	time.Sleep(30 * time.Millisecond)
	if err := pollPostPackets(conn, br, bw, sid, `42["list:initialize"]`); err != nil {
		_ = conn.Close()
		return nil, nil, nil, "", 0, err
	}
	time.Sleep(30 * time.Millisecond)
	if err := pollPostPackets(conn, br, bw, sid, `42["list:snapshot",[]]`); err != nil {
		_ = conn.Close()
		return nil, nil, nil, "", 0, err
	}

	return conn, br, bw, sid, readWait, nil
}

// Engine.IO polling POST body: "len:packetlen:packet..."
func encodeEIOPayload(packets ...string) string {
	var b strings.Builder
	for _, p := range packets {
		b.WriteString(strconv.Itoa(len(p)))
		b.WriteByte(':')
		b.WriteString(p)
	}
	return b.String()
}

func pollPostPackets(conn net.Conn, br *bufio.Reader, bw *bufio.Writer, sid string, packets ...string) error {
	t := time.Now().UnixNano()
	payload := encodeEIOPayload(packets...)

	_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	fmt.Fprintf(bw, "POST %s&sid=%s&t=%d HTTP/1.1\r\n", pollPath, sid, t)
	fmt.Fprintf(bw, "Host: %s\r\n", host)
	writeBrowserHeaders(bw)
	fmt.Fprintf(bw, "Cookie: %s\r\n", cookie)
	fmt.Fprintf(bw, "Content-Type: text/plain;charset=UTF-8\r\n")
	fmt.Fprintf(bw, "Content-Length: %d\r\n", len(payload))
	fmt.Fprintf(bw, "Connection: keep-alive\r\n\r\n")
	fmt.Fprintf(bw, "%s", payload)

	if err := bw.Flush(); err != nil {
		return err
	}

	_ = conn.SetReadDeadline(time.Now().Add(30 * time.Second))
	body, code := readHTTPResponse(br)
	if code != 200 {
		return fmt.Errorf("post code: %d body=%q", code, safeBody(body))
	}
	return nil
}

// splitEIOPayload supports both:
// 1) 0x1e separators
// 2) length-prefixed "len:packet" (Engine.IO polling)
func splitEIOPayload(b []byte) [][]byte {
	if len(b) == 0 {
		return nil
	}

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

	out := [][]byte{}
	i := 0
	for i < len(b) {
		j := i
		for j < len(b) && b[j] >= '0' && b[j] <= '9' {
			j++
		}
		if j == i || j >= len(b) || b[j] != ':' {
			return [][]byte{b}
		}
		n, err := strconv.Atoi(string(b[i:j]))
		if err != nil || n < 0 {
			return [][]byte{b}
		}
		j++
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
			_, _ = br.ReadString('\n')
		}
	} else if contentLen > 0 {
		body = make([]byte, contentLen)
		_, _ = io.ReadFull(br, body)
	}

	return body, code
}

func safeBody(b []byte) string {
	s := string(b)
	if len(s) > 200 {
		return s[:200] + "..."
	}
	return s
}

// ========================= Parser =========================

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
	fmt.Println("║  RACE: 1 WS vs 1 POLL (working polling)   ║")
	fmt.Println("╚═══════════════════════════════════════════╝")

	fmt.Print("\naccess_token cookie:\n> ")
	cookie, _ = in.ReadString('\n')
	cookie = strings.TrimSpace(cookie)
	if !strings.HasPrefix(cookie, "access_token=") {
		fmt.Println("Invalid")
		return
	}

	fmt.Println("\n⏳ Resolving DNS (for WS only)...")
	ips, err := net.LookupHost(host)
	if err != nil {
		fmt.Printf("DNS error: %v\n", err)
		return
	}
	ip := ips[0]
	fmt.Printf("✅ WS using IP: %s\n", ip)

	fmt.Println("\n⏳ Starting WebSocket...")
	go runWS(ip)

	time.Sleep(2 * time.Second)

	fmt.Println("⏳ Starting Polling...")
	go runPoll()

	fmt.Println("\n════════════════════════════════════════════")
	fmt.Println("  1 WS vs 1 POLL")
	fmt.Println("  🥇 = first to see order")
	fmt.Println("════════════════════════════════════════════\n")

	select {}
}
```
