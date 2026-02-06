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
	"net/http/cookiejar"
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

// ============ HTTP Polling with http.Client ============

func runPoll(id int, ip string) {
	source := fmt.Sprintf("POLL%d", id)
	ipShort := ip[strings.LastIndex(ip, ".")+1:]

	for {
		sid, client, err := pollConnect(ip)
		if err != nil {
			fmt.Printf("[%s] connect err: %v\n", source, err)
			time.Sleep(2 * time.Second)
			continue
		}

		fmt.Printf("[%s@%s] 🚀 connected (sid=%s...)\n", source, ipShort, sid[:12])

		errCount := 0
		for {
			data, err := pollGet(client, sid)
			if err != nil {
				errCount++
				fmt.Printf("[%s] poll err #%d: %v\n", source, errCount, err)
				if errCount >= 3 {
					break
				}
				time.Sleep(200 * time.Millisecond)
				continue
			}
			errCount = 0

			if len(data) == 1 && data[0] == '2' {
				if err := pollPost(client, sid, "3"); err != nil {
					fmt.Printf("[%s] pong err: %v\n", source, err)
					break
				}
				continue
			}

			if len(data) > 5 {
				parseOrder(data, source)
			}
		}

		time.Sleep(1 * time.Second)
	}
}

func pollConnect(ip string) (string, *http.Client, error) {
	// Create transport that forces specific IP
	transport := &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			conn, err := net.DialTimeout("tcp", ip+":443", 5*time.Second)
			if err != nil {
				return nil, err
			}
			if tc, ok := conn.(*net.TCPConn); ok {
				tc.SetNoDelay(true)
				tc.SetKeepAlive(true)
				tc.SetKeepAlivePeriod(10 * time.Second)
			}
			return conn, nil
		},
		TLSClientConfig: &tls.Config{
			ServerName: host,
		},
		MaxIdleConns:        1,
		MaxIdleConnsPerHost: 1,
		IdleConnTimeout:     60 * time.Second,
		DisableKeepAlives:   false,
		ForceAttemptHTTP2:   false, // Disable HTTP/2!
	}

	jar, _ := cookiejar.New(nil)
	client := &http.Client{
		Transport: transport,
		Jar:       jar,
		Timeout:   30 * time.Second,
	}

	// Handshake
	req, _ := http.NewRequest("GET", pollURL, nil)
	req.Header.Set("Cookie", cookie)
	req.Header.Set("Origin", "https://app.send.tg")

	resp, err := client.Do(req)
	if err != nil {
		return "", nil, fmt.Errorf("handshake: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if resp.StatusCode != 200 {
		return "", nil, fmt.Errorf("handshake code: %d", resp.StatusCode)
	}

	// Parse sid
	sidIdx := bytes.Index(body, []byte(`"sid":"`))
	if sidIdx == -1 {
		return "", nil, fmt.Errorf("no sid")
	}
	sidStart := sidIdx + 7
	sidEnd := bytes.IndexByte(body[sidStart:], '"')
	if sidEnd == -1 {
		return "", nil, fmt.Errorf("invalid sid")
	}
	sid := string(body[sidStart : sidStart+sidEnd])

	// Send 40
	if err := pollPost(client, sid, "40"); err != nil {
		return "", nil, fmt.Errorf("send 40: %v", err)
	}

	// Poll for ACK
	if _, err := pollGet(client, sid); err != nil {
		return "", nil, fmt.Errorf("poll ack: %v", err)
	}

	time.Sleep(30 * time.Millisecond)
	pollPost(client, sid, `42["list:initialize"]`)
	time.Sleep(30 * time.Millisecond)
	pollPost(client, sid, `42["list:snapshot",[]]`)

	return sid, client, nil
}

func pollPost(client *http.Client, sid, msg string) error {
	url := pollURL + "&sid=" + sid
	req, _ := http.NewRequest("POST", url, strings.NewReader(msg))
	req.Header.Set("Cookie", cookie)
	req.Header.Set("Content-Type", "text/plain;charset=UTF-8")
	req.Header.Set("Origin", "https://app.send.tg")

	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	if resp.StatusCode != 200 {
		return fmt.Errorf("http %d", resp.StatusCode)
	}
	return nil
}

func pollGet(client *http.Client, sid string) ([]byte, error) {
	url := pollURL + "&sid=" + sid
	req, _ := http.NewRequest("GET", url, nil)
	req.Header.Set("Cookie", cookie)
	req.Header.Set("Origin", "https://app.send.tg")

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("http %d", resp.StatusCode)
	}
	return body, nil
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
	fmt.Println("║  RACE: 2 WS vs 2 POLL (http.Client)       ║")
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
