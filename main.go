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
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gobwas/ws"
)

const (
	host   = "app.send.tg"
	wsPath = "/internal/v1/p2c-socket/?EIO=4&transport=websocket"
)

var cookie string

// DNS servers to try
var dnsServers = []string{
	"1.1.1.1:53",        // Cloudflare
	"1.0.0.1:53",        // Cloudflare secondary
	"8.8.8.8:53",        // Google
	"8.8.4.4:53",        // Google secondary
	"9.9.9.9:53",        // Quad9
	"208.67.222.222:53", // OpenDNS
	"208.67.220.220:53", // OpenDNS secondary
	"",                  // System default
}

// Track results
var (
	mu         sync.Mutex
	orderFirst = make(map[string]int) // order_id -> ws_id who saw first
	orderTime  = make(map[string]time.Time)
	wsWins     = make(map[int]int)     // ws_id -> win count
	wsDelays   = make(map[int][]int64) // ws_id -> delays when not first (ms)
	wsIPs      = make(map[int]string)  // ws_id -> IP
)

func recordOrder(orderID string, wsID int) {
	mu.Lock()
	defer mu.Unlock()

	if firstWS, exists := orderFirst[orderID]; exists {
		// Already seen
		delay := time.Since(orderTime[orderID]).Milliseconds()
		wsDelays[wsID] = append(wsDelays[wsID], delay)
		fmt.Printf("   WS%02d saw %s +%dms (first: WS%02d)\n", wsID, orderID[:12], delay, firstWS)
		return
	}

	// First!
	orderFirst[orderID] = wsID
	orderTime[orderID] = time.Now()
	wsWins[wsID]++

	fmt.Printf("🥇 WS%02d FIRST: %s (wins: %d, ip: %s)\n", wsID, orderID[:12], wsWins[wsID], wsIPs[wsID])
}

func resolveAllIPs() []string {
	ipSet := make(map[string]bool)
	var allIPs []string

	for _, dns := range dnsServers {
		var ips []string
		var err error

		if dns == "" {
			// System default
			ips, err = net.LookupHost(host)
		} else {
			// Custom DNS
			resolver := &net.Resolver{
				PreferGo: true,
				Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
					d := net.Dialer{Timeout: 3 * time.Second}
					return d.DialContext(ctx, "udp", dns)
				},
			}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			ips, err = resolver.LookupHost(ctx, host)
			cancel()
		}

		if err != nil {
			continue
		}

		for _, ip := range ips {
			if !ipSet[ip] {
				ipSet[ip] = true
				allIPs = append(allIPs, ip)
				source := dns
				if source == "" {
					source = "system"
				}
				fmt.Printf("  Found IP: %s (from %s)\n", ip, source)
			}
		}
	}

	return allIPs
}

func runWS(wsID int, ip string) {
	wsIPs[wsID] = ip
	ipShort := ip[strings.LastIndex(ip, ".")+1:]

	for {
		conn, err := connectWS(ip)
		if err != nil {
			fmt.Printf("[WS%02d@%s] connect err: %v\n", wsID, ipShort, err)
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

		fmt.Printf("[WS%02d@%s] 🚀 connected\n", wsID, ipShort)

		for {
			data, op, err := readFrame(conn)
			if err != nil {
				fmt.Printf("[WS%02d] disconnected: %v\n", wsID, err)
				break
			}

			if op == ws.OpText {
				if len(data) == 1 && data[0] == '2' {
					writeFrame(conn, []byte("3"))
					continue
				}
				if len(data) > 2 && data[0] == '4' && data[1] == '2' {
					parseOrder(data[2:], wsID)
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

func parseOrder(data []byte, wsID int) {
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

	recordOrder(orderID, wsID)
}

func printStats() {
	mu.Lock()
	defer mu.Unlock()

	fmt.Println("\n════════════════════════════════════════════")
	fmt.Println("📊 STATISTICS")
	fmt.Println("════════════════════════════════════════════")

	// Sort by wins
	type stat struct {
		wsID     int
		wins     int
		ip       string
		avgDelay float64
	}
	var stats []stat

	for wsID := 1; wsID <= 20; wsID++ {
		s := stat{wsID: wsID, wins: wsWins[wsID], ip: wsIPs[wsID]}
		if delays := wsDelays[wsID]; len(delays) > 0 {
			var sum int64
			for _, d := range delays {
				sum += d
			}
			s.avgDelay = float64(sum) / float64(len(delays))
		}
		stats = append(stats, s)
	}

	sort.Slice(stats, func(i, j int) bool {
		return stats[i].wins > stats[j].wins
	})

	fmt.Println("\nTop performers (by wins):")
	for i, s := range stats {
		if i >= 10 || s.wins == 0 {
			break
		}
		delayStr := "n/a"
		if s.avgDelay > 0 {
			delayStr = fmt.Sprintf("%.1fms", s.avgDelay)
		}
		ipShort := ""
		if s.ip != "" {
			ipShort = s.ip[strings.LastIndex(s.ip, ".")+1:]
		}
		fmt.Printf("  WS%02d: %d wins (IP ...%s, avg delay when late: %s)\n", s.wsID, s.wins, ipShort, delayStr)
	}

	// Group by IP
	ipWins := make(map[string]int)
	for wsID, wins := range wsWins {
		ip := wsIPs[wsID]
		ipWins[ip] += wins
	}

	fmt.Println("\nWins by IP:")
	type ipStat struct {
		ip   string
		wins int
	}
	var ipStats []ipStat
	for ip, wins := range ipWins {
		ipStats = append(ipStats, ipStat{ip, wins})
	}
	sort.Slice(ipStats, func(i, j int) bool {
		return ipStats[i].wins > ipStats[j].wins
	})
	for _, s := range ipStats {
		if s.wins > 0 {
			fmt.Printf("  %s: %d wins\n", s.ip, s.wins)
		}
	}

	fmt.Printf("\nTotal orders seen: %d\n", len(orderFirst))
	fmt.Println("════════════════════════════════════════════\n")
}

func main() {
	in := bufio.NewReader(os.Stdin)

	fmt.Println("╔═══════════════════════════════════════════╗")
	fmt.Println("║  WS RACE - 20 WebSockets, Multiple IPs    ║")
	fmt.Println("╚═══════════════════════════════════════════╝")

	fmt.Print("\naccess_token cookie:\n> ")
	cookie, _ = in.ReadString('\n')
	cookie = strings.TrimSpace(cookie)
	if !strings.HasPrefix(cookie, "access_token=") {
		fmt.Println("Invalid")
		return
	}

	fmt.Println("\n⏳ Resolving DNS from multiple servers...")
	allIPs := resolveAllIPs()

	if len(allIPs) == 0 {
		fmt.Println("❌ No IPs found!")
		return
	}

	fmt.Printf("\n✅ Found %d unique IPs\n", len(allIPs))

	// Start 20 WebSockets distributed across IPs
	fmt.Println("\n⏳ Starting 20 WebSockets...")
	for i := 1; i <= 20; i++ {
		ip := allIPs[(i-1)%len(allIPs)]
		go runWS(i, ip)
		time.Sleep(100 * time.Millisecond)
	}

	fmt.Println("\n════════════════════════════════════════════")
	fmt.Println("  20 WebSockets racing!")
	fmt.Println("  🥇 = first to see order")
	fmt.Println("  Stats every 60 seconds")
	fmt.Println("════════════════════════════════════════════\n")

	// Print stats periodically
	go func() {
		for {
			time.Sleep(60 * time.Second)
			printStats()
		}
	}()

	select {}
}
