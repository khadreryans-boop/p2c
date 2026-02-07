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
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gobwas/ws"
)

const (
	host   = "app.send.tg"
	wsPath = "/internal/v1/p2c-socket/?EIO=4&transport=websocket"
	wsURL  = "wss://app.send.tg/internal/v1/p2c-socket/?EIO=4&transport=websocket"
)

var cookie string
var serverIP string

// Stats
var (
	goWins     atomic.Int64
	wsocatWins atomic.Int64
	mu         sync.Mutex
	orders     = make(map[string]string)
	orderTs    = make(map[string]time.Time)
)

func recordOrder(orderID, source string) {
	mu.Lock()
	defer mu.Unlock()

	if first, exists := orders[orderID]; exists {
		delay := time.Since(orderTs[orderID]).Milliseconds()
		fmt.Printf("   %s saw %s +%dms (first: %s)\n", source, orderID[:12], delay, first)
		return
	}

	orders[orderID] = source
	orderTs[orderID] = time.Now()

	if source == "GO" {
		goWins.Add(1)
	} else {
		wsocatWins.Add(1)
	}

	fmt.Printf("🥇 %s FIRST: %s (GO:%d WSOCAT:%d)\n", source, orderID[:12], goWins.Load(), wsocatWins.Load())

	go func() {
		time.Sleep(10 * time.Second)
		mu.Lock()
		delete(orders, orderID)
		delete(orderTs, orderID)
		mu.Unlock()
	}()
}

// ============ Go WebSocket ============

func runGoWS() {
	for {
		conn, err := connectGoWS()
		if err != nil {
			fmt.Printf("[GO] connect err: %v\n", err)
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

		fmt.Println("[GO] 🚀 connected")

		for {
			data, op, err := readFrame(conn)
			if err != nil {
				fmt.Printf("[GO] err: %v\n", err)
				break
			}

			if op == ws.OpText {
				if len(data) == 1 && data[0] == '2' {
					writeFrame(conn, []byte("3"))
					continue
				}
				if len(data) > 2 && data[0] == '4' && data[1] == '2' {
					parseOrder(data[2:], "GO")
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

func connectGoWS() (net.Conn, error) {
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

// ============ Websocat WebSocket ============

func runWsocatWS() {
	for {
		err := runWsocatSession()
		if err != nil {
			fmt.Printf("[WSOCAT] session err: %v\n", err)
		}
		time.Sleep(2 * time.Second)
	}
}

func runWsocatSession() error {
	// websocat with headers - use -H for headers, no -t
	cmd := exec.Command("websocat", "-v",
		"-H", "Cookie: "+cookie,
		"-H", "Origin: https://app.send.tg",
		"--no-close",
		wsURL)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}

	stderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}

	if err := cmd.Start(); err != nil {
		return err
	}

	// Log stderr in background
	go func() {
		scanner := bufio.NewScanner(stderr)
		for scanner.Scan() {
			fmt.Printf("[WSOCAT-DBG] %s\n", scanner.Text())
		}
	}()

	defer func() {
		stdin.Close()
		cmd.Process.Kill()
		cmd.Wait()
	}()

	reader := bufio.NewReader(stdout)

	// Read Engine.IO open packet
	line, err := reader.ReadString('\n')
	if err != nil {
		return fmt.Errorf("read open: %v", err)
	}
	fmt.Printf("[WSOCAT] got: %s\n", strings.TrimSpace(line))

	// Send Socket.IO connect
	stdin.Write([]byte("40\n"))

	// Read connect ack
	line, err = reader.ReadString('\n')
	if err != nil {
		return fmt.Errorf("read ack: %v", err)
	}

	time.Sleep(30 * time.Millisecond)
	stdin.Write([]byte(`42["list:initialize"]` + "\n"))
	time.Sleep(30 * time.Millisecond)
	stdin.Write([]byte(`42["list:snapshot",[]]` + "\n"))

	fmt.Println("[WSOCAT] 🚀 connected")

	// Read loop
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return fmt.Errorf("read: %v", err)
		}

		line = strings.TrimSpace(line)
		if len(line) == 0 {
			continue
		}

		// Ping
		if line == "2" {
			stdin.Write([]byte("3\n"))
			continue
		}

		// Message
		if len(line) > 2 && line[0] == '4' && line[1] == '2' {
			parseOrder([]byte(line[2:]), "WSOCAT")
		}
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
	fmt.Println("║  RACE: Go WebSocket vs Websocat           ║")
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
	serverIP = ips[0]
	fmt.Printf("✅ Server IP: %s\n", serverIP)

	// Check websocat
	fmt.Println("\n⏳ Checking websocat...")
	cmd := exec.Command("websocat", "--version")
	output, err := cmd.Output()
	if err != nil {
		fmt.Printf("❌ websocat not found: %v\n", err)
		fmt.Println("\nInstall with:")
		fmt.Println("  wget https://github.com/vi/websocat/releases/download/v1.12.0/websocat.x86_64-unknown-linux-musl -O /usr/local/bin/websocat")
		fmt.Println("  chmod +x /usr/local/bin/websocat")
		return
	}
	fmt.Printf("✅ %s", string(output))

	fmt.Println("\n════════════════════════════════════════════")
	fmt.Println("  GO = gobwas/ws library")
	fmt.Println("  WSOCAT = websocat CLI")
	fmt.Println("  🥇 = first to see order")
	fmt.Println("════════════════════════════════════════════\n")

	// Start Go WebSocket
	go runGoWS()

	time.Sleep(1 * time.Second)

	// Start Websocat WebSocket
	go runWsocatWS()

	// Stats
	go func() {
		for {
			time.Sleep(60 * time.Second)
			gw, ww := goWins.Load(), wsocatWins.Load()
			total := gw + ww
			fmt.Printf("\n📊 STATS: GO=%d (%.0f%%) WSOCAT=%d (%.0f%%) total=%d\n\n",
				gw, float64(gw)/float64(max(total, 1))*100,
				ww, float64(ww)/float64(max(total, 1))*100,
				total)
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
