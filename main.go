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

var accessCookie string // "access_token=..."

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

		_, _, _ = readWSFrame(conn) // "0{...}"
		_ = writeWSFrame(conn, []byte("40"))
		_, _, _ = readWSFrame(conn) // "40"

		time.Sleep(30 * time.Millisecond)
		_ = writeWSFrame(conn, []byte(`42["list:initialize"]`))
		time.Sleep(30 * time.Millisecond)
		_ = writeWSFrame(conn, []byte(`42["list:snapshot",[]]`))

		fmt.Printf("[WS] 🚀 connected\n")

		for {
			_ = conn.SetReadDeadline(time.Now().Add(120 * time.Second))
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
			"Cookie": []string{accessCookie},
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
	_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
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

// ========================= Polling cookie jar =========================

var (
	pollCookiesMu sync.Mutex
	pollCookies   = map[string]string{} // name -> value
)

func initPollCookies() {
	pollCookiesMu.Lock()
	defer pollCookiesMu.Unlock()

	pollCookies = map[string]string{}
	kv := strings.SplitN(accessCookie, "=", 2)
	if len(kv) == 2 {
		pollCookies[kv[0]] = kv[1]
	}
}

func cookieHeader() string {
	pollCookiesMu.Lock()
	defer pollCookiesMu.Unlock()

	out := make([]string, 0, len(pollCookies))
	for k, v := range pollCookies {
		out = append(out, k+"="+v)
	}
	return strings.Join(out, "; ")
}

func absorbSetCookies(hdr map[string][]string) {
	sc := hdr["set-cookie"]
	if len(sc) == 0 {
		return
	}
	pollCookiesMu.Lock()
	defer pollCookiesMu.Unlock()

	for _, line := range sc {
		// "name=value; Path=/; ..."
		nv := strings.TrimSpace(strings.SplitN(line, ";", 2)[0])
		kv := strings.SplitN(nv, "=", 2)
		if len(kv) != 2 {
			continue
		}
		name := strings.TrimSpace(kv[0])
		val := strings.TrimSpace(kv[1])
		if name == "" {
			continue
		}
		pollCookies[name] = val
	}
}

// ========================= Polling (Engine.IO v4-ish) =========================

type eioHandshake struct {
	SID          string `json:"sid"`
	PingInterval int    `json:"pingInterval"` // ms
	PingTimeout  int    `json:"pingTimeout"`  // ms
	MaxPayload   int    `json:"maxPayload"`
}

func runPoll() {
	for {
		initPollCookies()

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
			fmt.Fprintf(bw, "Cookie: %s\r\n", cookieHeader())
			fmt.Fprintf(bw, "Connection: keep-alive\r\n\r\n")
			if err := bw.Flush(); err != nil {
				fmt.Printf("[POLL] flush err: %v\n", err)
				break
			}

			_ = conn.SetReadDeadline(time.Now().Add(readWait))
			body, hdr, code := readHTTPResponse(br)
			absorbSetCookies(hdr)

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
					if err := pollPostOne(conn, br, bw, sid, "3"); err != nil {
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
	// ВАЖНО: identity — чтобы не ловить gzip/br (мы не распаковываем)
	fmt.Fprintf(bw, "Accept: application/json, text/plain, */*\r\n")
	fmt.Fprintf(bw, "Accept-Language: en,ru;q=0.9,de;q=0.8\r\n")
	fmt.Fprintf(bw, "Accept-Encoding: identity\r\n")
	fmt.Fprintf(bw, "Cache-Control: no-cache\r\n")
	fmt.Fprintf(bw, "Pragma: no-cache\r\n")
	fmt.Fprintf(bw, "User-Agent: Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/142.0.0.0 Safari/537.36 OPR/126.0.0.0\r\n")

	// Часто проверяют
	fmt.Fprintf(bw, "Sec-CH-UA: \"Chromium\";v=\"142\", \"Opera GX\";v=\"126\", \"Not_A Brand\";v=\"99\"\r\n")
	fmt.Fprintf(bw, "Sec-CH-UA-Mobile: ?0\r\n")
	fmt.Fprintf(bw, "Sec-CH-UA-Platform: \"Windows\"\r\n")

	fmt.Fprintf(bw, "Referer: https://app.send.tg/p2c/orders\r\n")
	fmt.Fprintf(bw, "Origin: https://app.send.tg\r\n")

	fmt.Fprintf(bw, "Sec-Fetch-Dest: empty\r\n")
	fmt.Fprintf(bw, "Sec-Fetch-Mode: cors\r\n")
	fmt.Fprintf(bw, "Sec-Fetch-Site: same-origin\r\n")

	// иногда без этого WAF режет POST
	fmt.Fprintf(bw, "X-Requested-With: XMLHttpRequest\r\n")

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
	fmt.Fprintf(bw, "Cookie: %s\r\n", cookieHeader())
	fmt.Fprintf(bw, "Connection: keep-alive\r\n\r\n")
	if err := bw.Flush(); err != nil {
		_ = conn.Close()
		return nil, nil, nil, "", 0, err
	}

	_ = conn.SetReadDeadline(time.Now().Add(30 * time.Second))
	body, hdr, code := readHTTPResponse(br)
	absorbSetCookies(hdr)

	// debug: печатаем set-cookie и cookie header
	if sc := hdr["set-cookie"]; len(sc) > 0 {
		fmt.Printf("[POLL] handshake set-cookie: %q\n", truncate(strings.Join(sc, " | "), 240))
	}
	fmt.Printf("[POLL] handshake cookie: %q\n", truncate(cookieHeader(), 240))
	fmt.Printf("[POLL] handshake status=%d body=%q\n", code, safeBody(body))

	if code != 200 {
		_ = conn.Close()
		return nil, nil, nil, "", 0, fmt.Errorf("handshake code: %d body=%q", code, safeBody(body))
	}

	// Parse open packet "0{...}"
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

	// 2) POST "40" — сначала ПРЯМО "40" (как часто делает клиент), если не ок — fallback
	if err := pollPostOne(conn, br, bw, sid, "40"); err != nil {
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
		fmt.Fprintf(bw, "Cookie: %s\r\n", cookieHeader())
		fmt.Fprintf(bw, "Connection: keep-alive\r\n\r\n")
		if err := bw.Flush(); err != nil {
			_ = conn.Close()
			return nil, nil, nil, "", 0, err
		}

		_ = conn.SetReadDeadline(time.Now().Add(30 * time.Second))
		body, hdr, code := readHTTPResponse(br)
		absorbSetCookies(hdr)
		if code != 200 {
			_ = conn.Close()
			return nil, nil, nil, "", 0, fmt.Errorf("ack code: %d body=%q", code, safeBody(body))
		}
	}

	// 4) init
	time.Sleep(30 * time.Millisecond)
	if err := pollPostOne(conn, br, bw, sid, `42["list:initialize"]`); err != nil {
		_ = conn.Close()
		return nil, nil, nil, "", 0, err
	}
	time.Sleep(30 * time.Millisecond)
	if err := pollPostOne(conn, br, bw, sid, `42["list:snapshot",[]]`); err != nil {
		_ = conn.Close()
		return nil, nil, nil, "", 0, err
	}

	return conn, br, bw, sid, readWait, nil
}

// pollPostOne tries plain body first ("40"), then fallback to len:packet ("2:40")
func pollPostOne(conn net.Conn, br *bufio.Reader, bw *bufio.Writer, sid string, packet string) error {
	// 1) plain
	if err := pollPostBody(conn, br, bw, sid, packet); err == nil {
		return nil
	} else {
		// If server wants payload encoding, fallback:
		payload := encodeEIOPayload(packet)
		if err2 := pollPostBody(conn, br, bw, sid, payload); err2 == nil {
			return nil
		} else {
			// return the plain error (more informative) but include fallback too
			return fmt.Errorf("post plain failed, fallback failed: plain=%v fallback=%v", err, err2)
		}
	}
}

func pollPostBody(conn net.Conn, br *bufio.Reader, bw *bufio.Writer, sid string, body string) error {
	t := time.Now().UnixNano()

	_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	fmt.Fprintf(bw, "POST %s&sid=%s&t=%d HTTP/1.1\r\n", pollPath, sid, t)
	fmt.Fprintf(bw, "Host: %s\r\n", host)
	writeBrowserHeaders(bw)
	fmt.Fprintf(bw, "Cookie: %s\r\n", cookieHeader())
	fmt.Fprintf(bw, "Content-Type: text/plain;charset=UTF-8\r\n")
	fmt.Fprintf(bw, "Content-Length: %d\r\n", len(body))
	fmt.Fprintf(bw, "Connection: keep-alive\r\n\r\n")
	fmt.Fprintf(bw, "%s", body)

	if err := bw.Flush(); err != nil {
		return err
	}

	_ = conn.SetReadDeadline(time.Now().Add(30 * time.Second))
	respBody, hdr, code := readHTTPResponse(br)
	absorbSetCookies(hdr)

	if code != 200 {
		return fmt.Errorf("post code: %d body=%q", code, safeBody(respBody))
	}
	return nil
}

// Engine.IO payload: "len:packet"
func encodeEIOPayload(packet string) string {
	return strconv.Itoa(len(packet)) + ":" + packet
}

// splitEIOPayload supports:
// 1) 0x1e separators
// 2) length-prefixed "len:packet"
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

// ========================= HTTP response reader (with headers) =========================

func readHTTPResponse(br *bufio.Reader) ([]byte, map[string][]string, int) {
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
			_, _ = br.ReadString('\n')
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

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
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
	fmt.Println("║  RACE: 1 WS vs 1 POLL (poll fixes)        ║")
	fmt.Println("╚═══════════════════════════════════════════╝")

	fmt.Print("\naccess_token cookie (format: access_token=...):\n> ")
	accessCookie, _ = in.ReadString('\n')
	accessCookie = strings.TrimSpace(accessCookie)
	if !strings.HasPrefix(accessCookie, "access_token=") {
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
