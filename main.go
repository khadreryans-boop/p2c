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
)

const (
	host    = "app.send.tg"
	wsPath  = "/internal/v1/p2c-socket/?EIO=4&transport=websocket"
	pollURL = "https://app.send.tg/internal/v1/p2c-socket/?EIO=4&transport=polling"
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
				fmt.Printf("[WS] read err: %v\n", err)
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

// ============ HTTP Polling ============

func runPoll(ip string) {
	transport := &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			conn, err := net.DialTimeout("tcp", ip+":443", 5*time.Second)
			if err != nil {
				return nil, err
			}
			if tc, ok := conn.(*net.TCPConn); ok {
				tc.SetNoDelay(true)
				tc.SetKeepAlive(true)
			}
			return conn, nil
		},
		TLSClientConfig:   &tls.Config{ServerName: host},
		DisableKeepAlives: false,
		ForceAttemptHTTP2: false,
	}

	client := &http.Client{
		Transport: transport,
		Timeout:   30 * time.Second,
	}

	for {
		// Handshake
		req, _ := http.NewRequest("GET", pollURL, nil)
		req.Header.Set("Cookie", cookie)
		req.Header.Set("Origin", "https://app.send.tg")

		resp, err := client.Do(req)
		if err != nil {
			fmt.Printf("[POLL] handshake err: %v\n", err)
			time.Sleep(2 * time.Second)
			continue
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		if resp.StatusCode != 200 {
			fmt.Printf("[POLL] handshake code: %d\n", resp.StatusCode)
			time.Sleep(2 * time.Second)
			continue
		}

		// Parse sid
		sidIdx := bytes.Index(body, []byte(`"sid":"`))
		if sidIdx == -1 {
			fmt.Printf("[POLL] no sid\n")
			time.Sleep(2 * time.Second)
			continue
		}
		sid := string(body[sidIdx+7 : sidIdx+7+bytes.IndexByte(body[sidIdx+7:], '"')])

		// Send 40
		url := pollURL + "&sid=" + sid
		req, _ = http.NewRequest("POST", url, strings.NewReader("40"))
		req.Header.Set("Cookie", cookie)
		req.Header.Set("Content-Type", "text/plain;charset=UTF-8")
		req.Header.Set("Origin", "https://app.send.tg")

		resp, err = client.Do(req)
		if err != nil {
			fmt.Printf("[POLL] send 40 err: %v\n", err)
			time.Sleep(2 * time.Second)
			continue
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()

		if resp.StatusCode != 200 {
			fmt.Printf("[POLL] send 40 code: %d\n", resp.StatusCode)
			time.Sleep(2 * time.Second)
			continue
		}

		// Poll for ACK
		req, _ = http.NewRequest("GET", url, nil)
		req.Header.Set("Cookie", cookie)
		req.Header.Set("Origin", "https://app.send.tg")

		resp, err = client.Do(req)
		if err != nil {
			fmt.Printf("[POLL] ack err: %v\n", err)
			time.Sleep(2 * time.Second)
			continue
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()

		if resp.StatusCode != 200 {
			fmt.Printf("[POLL] ack code: %d\n", resp.StatusCode)
			time.Sleep(2 * time.Second)
			continue
		}

		// Initialize
		time.Sleep(30 * time.Millisecond)
		req, _ = http.NewRequest("POST", url, strings.NewReader(`42["list:initialize"]`))
		req.Header.Set("Cookie", cookie)
		req.Header.Set("Content-Type", "text/plain;charset=UTF-8")
		req.Header.Set("Origin", "https://app.send.tg")
		resp, _ = client.Do(req)
		if resp != nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}

		time.Sleep(30 * time.Millisecond)
		req, _ = http.NewRequest("POST", url, strings.NewReader(`42["list:snapshot",[]]`))
		req.Header.Set("Cookie", cookie)
		req.Header.Set("Content-Type", "text/plain;charset=UTF-8")
		req.Header.Set("Origin", "https://app.send.tg")
		resp, _ = client.Do(req)
		if resp != nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}

		fmt.Printf("[POLL] 🚀 connected (sid=%s...)\n", sid[:12])

		// Poll loop
		for {
			req, _ = http.NewRequest("GET", url, nil)
			req.Header.Set("Cookie", cookie)
			req.Header.Set("Origin", "https://app.send.tg")

			resp, err = client.Do(req)
			if err != nil {
				fmt.Printf("[POLL] poll err: %v\n", err)
				break
			}

			if resp.StatusCode != 200 {
				io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
				fmt.Printf("[POLL] poll code: %d\n", resp.StatusCode)
				break
			}

			body, _ = io.ReadAll(resp.Body)
			resp.Body.Close()

			// Ping
			if len(body) == 1 && body[0] == '2' {
				req, _ = http.NewRequest("POST", url, strings.NewReader("3"))
				req.Header.Set("Cookie", cookie)
				req.Header.Set("Content-Type", "text/plain;charset=UTF-8")
				req.Header.Set("Origin", "https://app.send.tg")
				resp, err = client.Do(req)
				if err != nil || resp.StatusCode != 200 {
					if resp != nil {
						io.Copy(io.Discard, resp.Body)
						resp.Body.Close()
					}
					fmt.Printf("[POLL] pong err\n")
					break
				}
				io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
				continue
			}

			// Parse orders
			if len(body) > 5 {
				parseOrder(body, "POLL")
			}
		}

		// Create new client for fresh connection
		transport = &http.Transport{
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				conn, err := net.DialTimeout("tcp", ip+":443", 5*time.Second)
				if err != nil {
					return nil, err
				}
				if tc, ok := conn.(*net.TCPConn); ok {
					tc.SetNoDelay(true)
					tc.SetKeepAlive(true)
				}
				return conn, nil
			},
			TLSClientConfig:   &tls.Config{ServerName: host},
			DisableKeepAlives: false,
			ForceAttemptHTTP2: false,
		}
		client = &http.Client{
			Transport: transport,
			Timeout:   30 * time.Second,
		}

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
	id := string(data[start : start+end])

	recordOrder(id, source)
}

// ============ Main ============

func main() {
	in := bufio.NewReader(os.Stdin)

	fmt.Println("╔═══════════════════════════════════════════╗")
	fmt.Println("║  RACE: 1 WS vs 1 POLL                     ║")
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

	time.Sleep(1 * time.Second)

	fmt.Println("⏳ Starting Polling...")
	go runPoll(ip)

	fmt.Println("\n════════════════════════════════════════════")
	fmt.Println("  1 WS vs 1 POLL on same IP")
	fmt.Println("  🥇 = first to see order")
	fmt.Println("════════════════════════════════════════════\n")

	select {}
}
