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

// ========================= WebSocket (unchanged) =========================

func runWS(ip string) {
	for {
		conn, err := connectWS(ip)
		if err != nil {
			fmt.Printf("[WS] connect err: %v\n", err)
			time.Sleep(2 * time.Second)
			continue
		}

		_, _, _ = readWSFrame(conn) // "0{...}"
		_ = writeWSFrame(conn, []byte("40"))
		_, _, _ = readWSFrame(conn) // "40"

		time.Sleep(30 * time.Millisecond)
		_ = writeWSFrame(conn, []byte(`42["list:initialize"]`))
		time.Sleep(30 * time.Millisecond)
		_ = writeWSFrame(conn, []byte(`42["list:snapshot",[]]`))

		fmt.Printf("[WS] 🚀 connected\n")

		for {
			data, op, err := readWSFrame(conn)
			if err != nil {
				fmt.Printf("[WS] err: %v\n", err)
				break
			}

			if op == ws.OpText {
				if len(data) == 1 && data[0] == '2' {
					_ = writeWSFrame(conn, []byte("3"))
					continue
				}
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

func writeWSFrame(conn net.Conn, data []byte) error {
	frame := ws.NewTextFrame(data)
	frame = ws.MaskFrameInPlace(frame)
	return ws.WriteFrame(conn, frame)
}

func readWSFrame(conn net.Conn) ([]byte, ws.OpCode, error) {
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

// ========================= Polling (FIXED Engine.IO v4) =========================

type eioHandshake struct {
	SID          string `json:"sid"`
	PingInterval int    `json:"pingInterval"`
	PingTimeout  int    `json:"pingTimeout"`
	MaxPayload   int    `json:"maxPayload"`
}

// Cookie jar (for any Set-Cookie sticky markers)
type cookieJar struct {
	mu sync.Mutex
	m  map[string]string
}

func newJar(seed string) *cookieJar {
	j := &cookieJar{m: map[string]string{}}
	kv := strings.SplitN(seed, "=", 2)
	if len(kv) == 2 {
		j.m[strings.TrimSpace(kv[0])] = strings.TrimSpace(kv[1])
	}
	return j
}

func (j *cookieJar) absorbSetCookies(hdr map[string][]string) {
	sc := hdr["set-cookie"]
	if len(sc) == 0 {
		return
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	for _, line := range sc {
		// name=value; Path=/; ...
		nv := strings.TrimSpace(strings.SplitN(line, ";", 2)[0])
		kv := strings.SplitN(nv, "=", 2)
		if len(kv) != 2 {
			continue
		}
		name := strings.TrimSpace(kv[0])
		val := strings.TrimSpace(kv[1])
		if name != "" {
			j.m[name] = val
		}
	}
}

func (j *cookieJar) header() string {
	j.mu.Lock()
	defer j.mu.Unlock()
	out := make([]string, 0, len(j.m))
	for k, v := range j.m {
		out = append(out, k+"="+v)
	}
	return strings.Join(out, "; ")
}

// IMPORTANT: polling endpoint часто требует &t=... (cache-buster)
func runPollFixed(ip string) {
	jar := newJar(cookie)

	for {
		conn, br, bw, sid, pingEvery, err := pollConnectFixed(ip, jar)
		if err != nil {
			fmt.Printf("[POLL] connect err: %v\n", err)
			time.Sleep(2 * time.Second)
			continue
		}

		fmt.Printf("[POLL] 🚀 connected (sid=%s..., pingEvery=%s)\n", sid[:min(12, len(sid))], pingEvery)

		for {
			t := time.Now().UnixNano()

			_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
			fmt.Fprintf(bw, "GET %s&sid=%s&t=%d HTTP/1.1\r\n", pollPath, sid, t)
			fmt.Fprintf(bw, "Host: %s\r\n", host)
			writePollHeaders(bw)
			fmt.Fprintf(bw, "Cookie: %s\r\n", jar.header())
			fmt.Fprintf(bw, "Connection: keep-alive\r\n\r\n")

			if err := bw.Flush(); err != nil {
				fmt.Printf("[POLL] flush err: %v\n", err)
				break
			}

			_ = conn.SetReadDeadline(time.Now().Add(pingEvery + 30*time.Second))
			body, hdr, code := readHTTPResponseWithHeaders(br)
			jar.absorbSetCookies(hdr)

			if code != 200 {
				fmt.Printf("[POLL] poll code: %d body=%q\n", code, safeBody(body))
				break
			}
			if len(body) == 0 {
				continue
			}

			// Split Engine.IO payload and handle packets one by one.
			for _, p := range splitEIOPayload(body) {
				if len(p) == 0 {
					continue
				}

				// Engine.IO ping
				if len(p) == 1 && p[0] == '2' {
					// Pong must be POSTed as packet "3" (encode as payload)
					if err := pollPostPackets(conn, br, bw, sid, jar, "3"); err != nil {
						fmt.Printf("[POLL] pong err: %v\n", err)
						goto reconnect
					}
					continue
				}

				// Socket.IO event packet "42[...]"
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

func pollConnectFixed(ip string, jar *cookieJar) (net.Conn, *bufio.Reader, *bufio.Writer, string, time.Duration, error) {
	// Keep the same IP for fairness vs WS; Host header is still app.send.tg
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

	// 1) Handshake GET (no sid)
	{
		t := time.Now().UnixNano()
		_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
		fmt.Fprintf(bw, "GET %s&t=%d HTTP/1.1\r\n", pollPath, t)
		fmt.Fprintf(bw, "Host: %s\r\n", host)
		writePollHeaders(bw)
		fmt.Fprintf(bw, "Cookie: %s\r\n", jar.header())
		fmt.Fprintf(bw, "Connection: keep-alive\r\n\r\n")
		if err := bw.Flush(); err != nil {
			_ = conn.Close()
			return nil, nil, nil, "", 0, err
		}

		_ = conn.SetReadDeadline(time.Now().Add(30 * time.Second))
		body, hdr, code := readHTTPResponseWithHeaders(br)
		jar.absorbSetCookies(hdr)

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
			return nil, nil, nil, "", 0, fmt.Errorf("no open packet in body=%q", safeBody(body))
		}

		sid := hs.SID
		pingEvery := time.Duration(hs.PingInterval) * time.Millisecond
		if pingEvery <= 0 {
			pingEvery = 25 * time.Second
		}

		// 2) socket.io connect "40" as Engine.IO payload
		if err := pollPostPackets(conn, br, bw, sid, jar, "40"); err != nil {
			_ = conn.Close()
			return nil, nil, nil, "", 0, fmt.Errorf("send 40: %w", err)
		}

		// 3) GET ack (optional, but keeps state aligned)
		{
			t3 := time.Now().UnixNano()
			_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
			fmt.Fprintf(bw, "GET %s&sid=%s&t=%d HTTP/1.1\r\n", pollPath, sid, t3)
			fmt.Fprintf(bw, "Host: %s\r\n", host)
			writePollHeaders(bw)
			fmt.Fprintf(bw, "Cookie: %s\r\n", jar.header())
			fmt.Fprintf(bw, "Connection: keep-alive\r\n\r\n")
			if err := bw.Flush(); err != nil {
				_ = conn.Close()
				return nil, nil, nil, "", 0, err
			}

			_ = conn.SetReadDeadline(time.Now().Add(30 * time.Second))
			_, hdr, code := readHTTPResponseWithHeaders(br)
			jar.absorbSetCookies(hdr)
			if code != 200 {
				_ = conn.Close()
				return nil, nil, nil, "", 0, fmt.Errorf("ack code: %d", code)
			}
		}

		// 4) init + snapshot (as payload packets)
		time.Sleep(30 * time.Millisecond)
		if err := pollPostPackets(conn, br, bw, sid, jar, `42["list:initialize"]`); err != nil {
			_ = conn.Close()
			return nil, nil, nil, "", 0, fmt.Errorf("init: %w", err)
		}
		time.Sleep(30 * time.Millisecond)
		if err := pollPostPackets(conn, br, bw, sid, jar, `42["list:snapshot",[]]`); err != nil {
			_ = conn.Close()
			return nil, nil, nil, "", 0, fmt.Errorf("snapshot: %w", err)
		}

		return conn, br, bw, sid, pingEvery, nil
	}
}

func writePollHeaders(bw *bufio.Writer) {
	// просим не сжимать ответ, чтобы парсер работал стабильно
	fmt.Fprintf(bw, "Accept: */*\r\n")
	fmt.Fprintf(bw, "Accept-Encoding: identity\r\n")
	fmt.Fprintf(bw, "Origin: https://app.send.tg\r\n")
	fmt.Fprintf(bw, "Referer: https://app.send.tg/\r\n")
	fmt.Fprintf(bw, "User-Agent: Mozilla/5.0\r\n")
}

func encodeEIOPayload(packets ...string) string {
	// Engine.IO polling payload: "len:packetlen:packet..."
	var b strings.Builder
	for _, p := range packets {
		b.WriteString(strconv.Itoa(len(p)))
		b.WriteByte(':')
		b.WriteString(p)
	}
	return b.String()
}

func pollPostPackets(conn net.Conn, br *bufio.Reader, bw *bufio.Writer, sid string, jar *cookieJar, packets ...string) error {
	t := time.Now().UnixNano()
	payload := encodeEIOPayload(packets...)

	_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	fmt.Fprintf(bw, "POST %s&sid=%s&t=%d HTTP/1.1\r\n", pollPath, sid, t)
	fmt.Fprintf(bw, "Host: %s\r\n", host)
	writePollHeaders(bw)
	fmt.Fprintf(bw, "Cookie: %s\r\n", jar.header())
	fmt.Fprintf(bw, "Content-Type: text/plain;charset=UTF-8\r\n")
	fmt.Fprintf(bw, "Content-Length: %d\r\n", len(payload))
	fmt.Fprintf(bw, "Connection: keep-alive\r\n\r\n")
	fmt.Fprintf(bw, "%s", payload)

	if err := bw.Flush(); err != nil {
		return err
	}

	_ = conn.SetReadDeadline(time.Now().Add(30 * time.Second))
	_, hdr, code := readHTTPResponseWithHeaders(br)
	jar.absorbSetCookies(hdr)
	if code != 200 {
		return fmt.Errorf("post code: %d", code)
	}
	return nil
}

// splitEIOPayload handles:
// 1) record-separator 0x1e
// 2) length-prefixed polling payload "len:packet..."
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
			return [][]byte{b} // unknown format
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

// ========================= HTTP reader (with headers) =========================

func readHTTPResponseWithHeaders(br *bufio.Reader) ([]byte, map[string][]string, int) {
	line, err := br.ReadString('\n')
	if err != nil {
		return nil, map[string][]string{}, 0
	}

	code := 0
	if len(line) >= 12 {
		code, _ = strconv.Atoi(strings.TrimSpace(line[9:12]))
	}

	hdr := map[string][]string{}
	contentLen := 0
	chunked := false

	for {
		h, err := br.ReadString('\n')
		if err != nil {
			return nil, hdr, code
		}
		if h == "\r\n" {
			break
		}
		h = strings.TrimRight(h, "\r\n")
		colon := strings.IndexByte(h, ':')
		if colon <= 0 {
			continue
		}
		k := strings.ToLower(strings.TrimSpace(h[:colon]))
		v := strings.TrimSpace(h[colon+1:])
		hdr[k] = append(hdr[k], v)

		switch k {
		case "content-length":
			fmt.Sscanf(v, "%d", &contentLen)
		case "transfer-encoding":
			if strings.Contains(strings.ToLower(v), "chunked") {
				chunked = true
			}
		}
	}

	var body []byte
	if chunked {
		for {
			sizeLine, err := br.ReadString('\n')
			if err != nil {
				return body, hdr, code
			}
			sizeLine = strings.TrimSpace(sizeLine)
			if sizeLine == "" {
				continue
			}
			size, err := strconv.ParseInt(sizeLine, 16, 64)
			if err != nil {
				return body, hdr, code
			}
			if size == 0 {
				// trailers
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
				return body, hdr, code
			}
			body = append(body, chunk...)
			_, _ = br.ReadString('\n') // CRLF
		}
	} else if contentLen > 0 {
		body = make([]byte, contentLen)
		_, _ = io.ReadFull(br, body)
	}

	return body, hdr, code
}

func safeBody(b []byte) string {
	s := string(b)
	if len(s) > 220 {
		return s[:220] + "..."
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
	fmt.Println("║  RACE: 1 WS vs 1 POLL (FIXED polling)     ║")
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

	fmt.Println("⏳ Starting Polling (fixed)...")
	go runPollFixed(ip)

	fmt.Println("\n════════════════════════════════════════════")
	fmt.Println("  1 WS vs 1 POLL")
	fmt.Println("  🥇 = first to see order")
	fmt.Println("════════════════════════════════════════════\n")

	select {}
}
