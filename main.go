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
	"sync/atomic"
	"time"

	"github.com/gobwas/ws"
)

const (
	host           = "app.send.tg"
	wsPath         = "/internal/v1/p2c-socket/?EIO=4&transport=websocket"
	takePathPrefix = "/internal/v1/p2c/payments/take/"
	pauseSeconds   = 20
)

const (
	numWebSockets = 20
	numTakers     = 5
)

var (
	pauseTaking atomic.Bool
	cookie      string
	serverIP    string
)

// Pre-built request parts (zero alloc on hot path)
var reqPrefix []byte
var reqSuffix []byte

// Dedupe with minimal locking
var (
	seenMu sync.Mutex
	seen   = make(map[string]struct{})
)

func markSeen(id string) bool {
	seenMu.Lock()
	if _, ok := seen[id]; ok {
		seenMu.Unlock()
		return false
	}
	seen[id] = struct{}{}
	seenMu.Unlock()
	go func() {
		time.Sleep(5 * time.Second)
		seenMu.Lock()
		delete(seen, id)
		seenMu.Unlock()
	}()
	return true
}

// Stats
var (
	totalSeen atomic.Int64
	totalWon  atomic.Int64
	totalLate atomic.Int64
)

// ============ Taker (Fire-and-Forget) ============

type taker struct {
	conn  net.Conn
	br    *bufio.Reader
	bw    *bufio.Writer
	mu    sync.Mutex
	ready atomic.Bool
	id    int
}

var takers []*taker

func (t *taker) connect() error {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.conn != nil {
		t.conn.Close()
	}

	conn, err := net.DialTimeout("tcp", serverIP+":443", 2*time.Second)
	if err != nil {
		t.ready.Store(false)
		return err
	}

	if tc, ok := conn.(*net.TCPConn); ok {
		tc.SetNoDelay(true)
		tc.SetKeepAlive(true)
		tc.SetKeepAlivePeriod(30 * time.Second)
	}

	tlsConn := tls.Client(conn, &tls.Config{ServerName: host})
	if err := tlsConn.Handshake(); err != nil {
		conn.Close()
		t.ready.Store(false)
		return err
	}

	t.conn = tlsConn
	t.br = bufio.NewReaderSize(tlsConn, 4096)
	t.bw = bufio.NewWriterSize(tlsConn, 2048)
	t.ready.Store(true)
	return nil
}

// Fire-and-forget: отправляем и НЕ ЖДЁМ ответа
func (t *taker) fireAndForget(orderID []byte) bool {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.conn == nil {
		return false
	}

	t.conn.SetWriteDeadline(time.Now().Add(100 * time.Millisecond))

	// Пишем запрос напрямую без аллокаций
	t.bw.Write(reqPrefix)
	t.bw.Write(orderID)
	t.bw.Write(reqSuffix)

	if err := t.bw.Flush(); err != nil {
		t.conn.Close()
		t.conn = nil
		t.ready.Store(false)
		return false
	}

	return true
}

// Читаем ответ отдельно (в фоне)
func (t *taker) readResponse() (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.conn == nil {
		return 0, fmt.Errorf("no conn")
	}

	t.conn.SetReadDeadline(time.Now().Add(2 * time.Second))

	line, err := t.br.ReadString('\n')
	if err != nil {
		t.conn.Close()
		t.conn = nil
		t.ready.Store(false)
		return 0, err
	}

	if len(line) < 12 {
		return 0, fmt.Errorf("short")
	}

	code, _ := strconv.Atoi(line[9:12])

	// Drain
	for {
		line, _ := t.br.ReadString('\n')
		if line == "\r\n" || line == "" {
			break
		}
	}

	return code, nil
}

// Warmup
func (t *taker) warmup() {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.conn == nil {
		return
	}

	t.conn.SetDeadline(time.Now().Add(2 * time.Second))

	req := "POST /internal/v1/p2c/accounts HTTP/1.1\r\n" +
		"Host: " + host + "\r\n" +
		"Cookie: " + cookie + "\r\n" +
		"Content-Type: application/json\r\n" +
		"Content-Length: 2\r\n" +
		"Connection: keep-alive\r\n\r\n{}"

	t.bw.WriteString(req)
	if err := t.bw.Flush(); err != nil {
		t.conn.Close()
		t.conn = nil
		t.ready.Store(false)
		return
	}

	line, err := t.br.ReadString('\n')
	if err != nil {
		t.conn.Close()
		t.conn = nil
		t.ready.Store(false)
		return
	}

	// Drain headers
	contentLen := 0
	for {
		line, _ = t.br.ReadString('\n')
		if line == "\r\n" || line == "" {
			break
		}
		if strings.HasPrefix(strings.ToLower(line), "content-length:") {
			fmt.Sscanf(line[15:], "%d", &contentLen)
		}
	}

	if contentLen > 0 {
		body := make([]byte, contentLen)
		io.ReadFull(t.br, body)
	}
}

// ============ Ultra-fast Take ============

// Паттерны для поиска (pre-allocated)
var (
	patternOpAdd = []byte(`"op":"add"`)
	patternID    = []byte(`"id":"`)
	patternAmt   = []byte(`"in_amount":"`)
)

func ultraFastTake(data []byte, wsID int, detectTime time.Time, minCents int64) {
	if pauseTaking.Load() {
		return
	}

	// Быстрая проверка без аллокаций
	if !bytes.Contains(data, patternOpAdd) {
		return
	}

	// Ищем ID напрямую в bytes
	idx := bytes.Index(data, patternID)
	if idx == -1 {
		return
	}

	start := idx + 6
	end := bytes.IndexByte(data[start:], '"')
	if end == -1 || end > 30 {
		return
	}

	orderID := data[start : start+end] // []byte, не string!
	orderIDStr := string(orderID)      // Только для dedupe

	// Dedupe
	if !markSeen(orderIDStr) {
		return
	}

	totalSeen.Add(1)

	// Парсим сумму параллельно (но не блокируем take!)
	var amt string
	if idx := bytes.Index(data, patternAmt); idx != -1 {
		s := idx + 13
		e := bytes.IndexByte(data[s:], '"')
		if e != -1 && e < 20 {
			amt = string(data[s : s+e])
		}
	}

	// Фильтр по сумме
	if minCents > 0 && amt != "" {
		cents := parseCents(amt)
		if cents < minCents {
			return
		}
	}

	// 🚀 FIRE ALL TAKERS IN PARALLEL (no waiting at all)
	fireTime := time.Now()

	for _, t := range takers {
		if t.ready.Load() {
			go t.fireAndForget(orderID)
		}
	}

	fireLatency := time.Since(fireTime).Microseconds()

	// Читаем ответы в фоне
	go func() {
		var results []string
		var won bool

		for _, t := range takers {
			if !t.ready.Load() {
				continue
			}

			code, err := t.readResponse()
			if err != nil {
				results = append(results, fmt.Sprintf("T%d:ERR", t.id))
				go t.connect()
				continue
			}

			if code == 200 {
				results = append(results, fmt.Sprintf("T%d:OK", t.id))
				won = true
			} else {
				results = append(results, fmt.Sprintf("T%d:%d", t.id, code))
			}
		}

		e2e := time.Since(detectTime).Milliseconds()

		if won {
			totalWon.Add(1)
			fmt.Printf("✅ [WS%02d] e2e=%dms fire=%dμs amt=%s | %s\n",
				wsID, e2e, fireLatency, amt, strings.Join(results, " "))
			pauseTaking.Store(true)
			time.Sleep(pauseSeconds * time.Second)
			pauseTaking.Store(false)
			fmt.Println("▶ Resumed")
		} else {
			totalLate.Add(1)
			fmt.Printf("   [WS%02d] LATE e2e=%dms fire=%dμs amt=%s | %s\n",
				wsID, e2e, fireLatency, amt, strings.Join(results, " "))
		}
	}()
}

func parseCents(amt string) int64 {
	var whole, frac int64
	var fracDigits int
	var seenDot bool

	for i := 0; i < len(amt); i++ {
		c := amt[i]
		if c == '.' {
			seenDot = true
			continue
		}
		if c >= '0' && c <= '9' {
			d := int64(c - '0')
			if !seenDot {
				whole = whole*10 + d
			} else if fracDigits < 2 {
				frac = frac*10 + d
				fracDigits++
			}
		}
	}

	if fracDigits == 1 {
		frac *= 10
	}

	return whole*100 + frac
}

// ============ WebSocket ============

func runWS(wsID int, minCents int64) {
	for {
		conn, err := connectWS()
		if err != nil {
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

		fmt.Printf("[WS%02d] 🚀\n", wsID)

		for {
			data, op, err := readFrame(conn)
			detectTime := time.Now() // Timestamp СРАЗУ после получения фрейма

			if err != nil {
				break
			}

			if op == ws.OpText {
				if len(data) == 1 && data[0] == '2' {
					writeFrame(conn, []byte("3"))
					continue
				}
				if len(data) > 10 && data[0] == '4' && data[1] == '2' {
					// 🚀 INSTANT TRIGGER
					ultraFastTake(data[2:], wsID, detectTime, minCents)
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

// ============ Main ============

func main() {
	in := bufio.NewReader(os.Stdin)

	fmt.Println("╔═══════════════════════════════════════════╗")
	fmt.Println("║  ULTRA SNIPER - Fire & Forget             ║")
	fmt.Println("╚═══════════════════════════════════════════╝")

	fmt.Print("\naccess_token cookie:\n> ")
	cookie, _ = in.ReadString('\n')
	cookie = strings.TrimSpace(cookie)
	if !strings.HasPrefix(cookie, "access_token=") {
		fmt.Println("Invalid")
		return
	}

	fmt.Print("MIN amount (0=all):\n> ")
	minLine, _ := in.ReadString('\n')
	minLine = strings.TrimSpace(minLine)
	var minCents int64
	if minLine != "" {
		f, _ := strconv.ParseFloat(minLine, 64)
		minCents = int64(f * 100)
	}

	fmt.Println("\n⏳ Resolving DNS...")
	ips, err := net.LookupHost(host)
	if err != nil {
		fmt.Printf("DNS error: %v\n", err)
		return
	}
	serverIP = ips[0]
	fmt.Printf("✅ Server IP: %s\n", serverIP)

	// Pre-build request (zero alloc on hot path)
	reqPrefix = []byte("POST " + takePathPrefix)
	reqSuffix = []byte(" HTTP/1.1\r\nHost: " + host + "\r\nContent-Type: application/json\r\nCookie: " + cookie + "\r\nContent-Length: 2\r\n\r\n{}")

	// Create takers
	fmt.Printf("\n⏳ Creating %d takers...\n", numTakers)
	for i := 0; i < numTakers; i++ {
		t := &taker{id: i + 1}
		t.connect()
		takers = append(takers, t)
	}

	ready := 0
	for _, t := range takers {
		if t.ready.Load() {
			ready++
		}
	}
	fmt.Printf("✅ %d/%d takers ready\n", ready, numTakers)

	// Warmup goroutines
	for i, t := range takers {
		go func(idx int, tk *taker) {
			time.Sleep(time.Duration(idx*50) * time.Millisecond)
			for {
				time.Sleep(200 * time.Millisecond)
				if tk.ready.Load() {
					tk.warmup()
				} else {
					tk.connect()
				}
			}
		}(i, t)
	}

	// Start WebSockets
	fmt.Printf("⏳ Starting %d WebSockets...\n", numWebSockets)
	for i := 1; i <= numWebSockets; i++ {
		go runWS(i, minCents)
		time.Sleep(50 * time.Millisecond)
	}

	fmt.Println("\n════════════════════════════════════════════")
	fmt.Printf("  %d WS | %d takers | FIRE-AND-FORGET mode\n", numWebSockets, numTakers)
	fmt.Println("  🔥 All takers fire in TRUE parallel")
	if minCents > 0 {
		fmt.Printf("  MIN: %.2f RUB\n", float64(minCents)/100)
	}
	fmt.Println("════════════════════════════════════════════\n")

	// Stats
	go func() {
		for {
			time.Sleep(60 * time.Second)
			s, w, l := totalSeen.Load(), totalWon.Load(), totalLate.Load()
			rate := float64(w) / float64(max(s, 1)) * 100
			fmt.Printf("\n📊 STATS: seen=%d won=%d late=%d (%.1f%%)\n\n", s, w, l, rate)
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
