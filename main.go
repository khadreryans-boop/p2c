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
	goWins   atomic.Int64
	curlWins atomic.Int64
	mu       sync.Mutex
	orders   = make(map[string]string) // orderID -> who saw first
	orderTs  = make(map[string]time.Time)
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
		curlWins.Add(1)
	}

	fmt.Printf("🥇 %s FIRST: %s (GO:%d CURL:%d)\n", source, orderID[:12], goWins.Load(), curlWins.Load())

	// Cleanup after 10s
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

		// Handshake
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

// ============ Curl WebSocket ============

func runCurlWS() {
	for {
		err := runCurlWSSession()
		if err != nil {
			fmt.Printf("[CURL] session err: %v\n", err)
		}
		time.Sleep(2 * time.Second)
	}
}

func runCurlWSSession() error {
	// curl --ws with headers
	cmd := exec.Command("curl",
		"--ws", "-s", "-N",
		"-H", "Cookie: "+cookie,
		"-H", "Origin: https://app.send.tg",
		wsURL)

	stdout, err := cmd.StdoutPipe()
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
	_ = line

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

	fmt.Println("[CURL] 🚀 connected")

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
			parseOrder([]byte(line[2:]), "CURL")
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
	fmt.Println("║  RACE: Go WebSocket vs Curl WebSocket     ║")
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

	// Check curl version for --ws support
	fmt.Println("\n⏳ Checking curl WebSocket support...")
	cmd := exec.Command("curl", "--version")
	output, err := cmd.Output()
	if err != nil {
		fmt.Printf("curl not found: %v\n", err)
		return
	}
	version := strings.Split(string(output), "\n")[0]
	fmt.Printf("✅ %s\n", version)

	// Test if curl supports --ws
	testCmd := exec.Command("curl", "--ws", "-s", "--help")
	if err := testCmd.Run(); err != nil {
		fmt.Println("❌ curl --ws not supported (need curl >= 7.86)")
		fmt.Println("   Falling back to Go-only mode")

		// Run only Go WebSocket
		go runGoWS()

		select {}
	}

	fmt.Println("\n════════════════════════════════════════════")
	fmt.Println("  GO = gobwas/ws library")
	fmt.Println("  CURL = curl --ws")
	fmt.Println("  🥇 = first to see order")
	fmt.Println("════════════════════════════════════════════\n")

	// Start Go WebSocket
	go runGoWS()

	time.Sleep(1 * time.Second)

	// Start Curl WebSocket
	go runCurlWS()

	// Stats
	go func() {
		for {
			time.Sleep(60 * time.Second)
			gw, cw := goWins.Load(), curlWins.Load()
			total := gw + cw
			fmt.Printf("\n📊 STATS: GO=%d (%.0f%%) CURL=%d (%.0f%%) total=%d\n\n",
				gw, float64(gw)/float64(max(total, 1))*100,
				cw, float64(cw)/float64(max(total, 1))*100,
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
