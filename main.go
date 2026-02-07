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
	"strings"
	"sync"
	"time"

	"github.com/gobwas/ws"
	"golang.org/x/net/http2"
	nh "nhooyr.io/websocket"
)

const (
	host   = "app.send.tg"
	wsPath = "/internal/v1/p2c-socket/?EIO=4&transport=websocket"
	wsURL  = "wss://app.send.tg/internal/v1/p2c-socket/?EIO=4&transport=websocket"
)

var cookie string

// Track results
var (
	mu         sync.Mutex
	orderFirst = make(map[string]string) // order_id -> "H1-X" or "H2-X"
	orderTime  = make(map[string]time.Time)
	h1Wins     int
	h2Wins     int
)

func recordOrder(orderID, source string) {
	mu.Lock()
	defer mu.Unlock()

	if first, exists := orderFirst[orderID]; exists {
		delay := time.Since(orderTime[orderID]).Milliseconds()
		fmt.Printf("   %s saw %s +%dms (first: %s)\n", source, orderID[:12], delay, first)
		return
	}

	orderFirst[orderID] = source
	orderTime[orderID] = time.Now()

	if strings.HasPrefix(source, "H1") {
		h1Wins++
	} else {
		h2Wins++
	}

	fmt.Printf("🥇 %s FIRST: %s (H1:%d H2:%d)\n", source, orderID[:12], h1Wins, h2Wins)
}

// ============ HTTP/1.1 WebSocket (gobwas/ws) ============

func runWSHttp1(id int, ip string) {
	source := fmt.Sprintf("H1-%d", id)

	for {
		conn, err := connectWSHttp1(ip)
		if err != nil {
			fmt.Printf("[%s] connect err: %v\n", source, err)
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

		fmt.Printf("[%s] 🚀 connected (HTTP/1.1)\n", source)

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

func connectWSHttp1(ip string) (net.Conn, error) {
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

// ============ HTTP/2 WebSocket (nhooyr.io/websocket) ============

func runWSHttp2(id int) {
	source := fmt.Sprintf("H2-%d", id)

	// HTTP/2 transport
	transport := &http2.Transport{
		TLSClientConfig: &tls.Config{
			ServerName: host,
			NextProtos: []string{"h2"},
		},
	}

	httpClient := &http.Client{
		Transport: transport,
		Timeout:   30 * time.Second,
	}

	for {
		ctx := context.Background()

		// Добавляем cookie в header
		header := http.Header{}
		header.Set("Cookie", cookie)
		header.Set("Origin", "https://app.send.tg")

		conn, resp, err := nh.Dial(ctx, wsURL, &nh.DialOptions{
			HTTPClient: httpClient,
			HTTPHeader: header,
		})

		if err != nil {
			fmt.Printf("[%s] connect err: %v\n", source, err)
			if resp != nil {
				fmt.Printf("[%s] HTTP status: %d, Proto: %s\n", source, resp.StatusCode, resp.Proto)
			}
			time.Sleep(2 * time.Second)
			continue
		}

		proto := "HTTP/?"
		if resp != nil {
			proto = resp.Proto
		}

		// Handshake - read Engine.IO open
		_, data, err := conn.Read(ctx)
		if err != nil {
			fmt.Printf("[%s] read open err: %v\n", source, err)
			conn.Close(nh.StatusNormalClosure, "")
			time.Sleep(2 * time.Second)
			continue
		}
		_ = data

		// Send Socket.IO connect
		conn.Write(ctx, nh.MessageText, []byte("40"))

		// Read connect ack
		_, _, err = conn.Read(ctx)
		if err != nil {
			fmt.Printf("[%s] read ack err: %v\n", source, err)
			conn.Close(nh.StatusNormalClosure, "")
			time.Sleep(2 * time.Second)
			continue
		}

		time.Sleep(30 * time.Millisecond)
		conn.Write(ctx, nh.MessageText, []byte(`42["list:initialize"]`))
		time.Sleep(30 * time.Millisecond)
		conn.Write(ctx, nh.MessageText, []byte(`42["list:snapshot",[]]`))

		fmt.Printf("[%s] 🚀 connected (%s)\n", source, proto)

		for {
			_, data, err := conn.Read(ctx)
			if err != nil {
				break
			}

			if len(data) == 1 && data[0] == '2' {
				conn.Write(ctx, nh.MessageText, []byte("3"))
				continue
			}

			if len(data) > 2 && data[0] == '4' && data[1] == '2' {
				parseOrder(data[2:], source)
			}
		}

		conn.Close(nh.StatusNormalClosure, "")
		time.Sleep(1 * time.Second)
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
	orderID := string(data[start : start+end])

	recordOrder(orderID, source)
}

// ============ Main ============

func main() {
	in := bufio.NewReader(os.Stdin)

	fmt.Println("╔═══════════════════════════════════════════╗")
	fmt.Println("║  RACE: HTTP/1.1 vs HTTP/2 WebSocket       ║")
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

	// Start 5 HTTP/1.1 WebSockets
	fmt.Println("\n⏳ Starting 5 HTTP/1.1 WebSockets...")
	for i := 1; i <= 5; i++ {
		go runWSHttp1(i, ip)
		time.Sleep(100 * time.Millisecond)
	}

	time.Sleep(1 * time.Second)

	// Start 5 HTTP/2 WebSockets
	fmt.Println("⏳ Starting 5 HTTP/2 WebSockets...")
	for i := 1; i <= 5; i++ {
		go runWSHttp2(i)
		time.Sleep(100 * time.Millisecond)
	}

	fmt.Println("\n════════════════════════════════════════════")
	fmt.Println("  5 HTTP/1.1 (H1-X) vs 5 HTTP/2 (H2-X)")
	fmt.Println("  🥇 = first to see order")
	fmt.Println("════════════════════════════════════════════\n")

	// Stats every 60 sec
	go func() {
		for {
			time.Sleep(60 * time.Second)
			mu.Lock()
			total := h1Wins + h2Wins
			fmt.Printf("\n📊 STATS: H1=%d (%.0f%%) H2=%d (%.0f%%) total=%d\n\n",
				h1Wins, float64(h1Wins)/float64(max(total, 1))*100,
				h2Wins, float64(h2Wins)/float64(max(total, 1))*100,
				total)
			mu.Unlock()
		}
	}()

	select {}
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
