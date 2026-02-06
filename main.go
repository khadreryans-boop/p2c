package main

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	host     = "app.send.tg"
	pollPath = "/internal/v1/p2c-socket/?EIO=4&transport=polling"
)

var cookie string

func main() {
	in := bufio.NewReader(os.Stdin)

	fmt.Println("╔═══════════════════════════════════════════╗")
	fmt.Println("║  DEBUG POLLING                            ║")
	fmt.Println("╚═══════════════════════════════════════════╝")

	fmt.Print("\naccess_token cookie:\n> ")
	cookie, _ = in.ReadString('\n')
	cookie = strings.TrimSpace(cookie)

	fmt.Println("\n⏳ Resolving DNS...")
	ips, _ := net.LookupHost(host)
	ip := ips[0]
	fmt.Printf("Using IP: %s\n\n", ip)

	// Connect
	conn, err := net.DialTimeout("tcp", ip+":443", 5*time.Second)
	if err != nil {
		fmt.Printf("Connect error: %v\n", err)
		return
	}

	if tc, ok := conn.(*net.TCPConn); ok {
		tc.SetNoDelay(true)
	}

	tlsConn := tls.Client(conn, &tls.Config{ServerName: host})
	if err := tlsConn.Handshake(); err != nil {
		fmt.Printf("TLS error: %v\n", err)
		return
	}

	br := bufio.NewReaderSize(tlsConn, 16384)
	bw := bufio.NewWriterSize(tlsConn, 4096)

	// Step 1: Engine.IO handshake
	fmt.Println("═══════════════════════════════════════")
	fmt.Println("STEP 1: Engine.IO Handshake")
	fmt.Println("═══════════════════════════════════════")

	req1 := "GET " + pollPath + " HTTP/1.1\r\n" +
		"Host: " + host + "\r\n" +
		"Cookie: " + cookie + "\r\n" +
		"Accept: */*\r\n" +
		"Connection: keep-alive\r\n\r\n"

	fmt.Printf(">>> REQUEST:\n%s\n", req1)

	tlsConn.SetDeadline(time.Now().Add(10 * time.Second))
	bw.WriteString(req1)
	bw.Flush()

	body1, code1, headers1 := readFullResponse(br)
	fmt.Printf("<<< RESPONSE: %d\n", code1)
	fmt.Printf("<<< HEADERS:\n%s\n", headers1)
	fmt.Printf("<<< BODY: %s\n\n", string(body1))

	if code1 != 200 {
		fmt.Println("Handshake failed!")
		return
	}

	// Parse sid
	sidIdx := bytes.Index(body1, []byte(`"sid":"`))
	if sidIdx == -1 {
		fmt.Println("No sid found!")
		return
	}
	sidStart := sidIdx + 7
	sidEnd := bytes.IndexByte(body1[sidStart:], '"')
	sid := string(body1[sidStart : sidStart+sidEnd])
	fmt.Printf("✅ Got SID: %s\n\n", sid)

	// Step 2: Socket.IO connect (send "40")
	fmt.Println("═══════════════════════════════════════")
	fmt.Println("STEP 2: Send Socket.IO Connect (40)")
	fmt.Println("═══════════════════════════════════════")

	msg := "40"
	req2 := fmt.Sprintf("POST %s&sid=%s HTTP/1.1\r\n"+
		"Host: %s\r\n"+
		"Cookie: %s\r\n"+
		"Content-Type: text/plain;charset=UTF-8\r\n"+
		"Content-Length: %d\r\n"+
		"Accept: */*\r\n"+
		"Origin: https://app.send.tg\r\n"+
		"Connection: keep-alive\r\n\r\n%s",
		pollPath, sid, host, cookie, len(msg), msg)

	fmt.Printf(">>> REQUEST:\n%s\n", req2)

	tlsConn.SetDeadline(time.Now().Add(10 * time.Second))
	bw.WriteString(req2)
	bw.Flush()

	body2, code2, headers2 := readFullResponse(br)
	fmt.Printf("<<< RESPONSE: %d\n", code2)
	fmt.Printf("<<< HEADERS:\n%s\n", headers2)
	fmt.Printf("<<< BODY: %s\n\n", string(body2))

	if code2 != 200 {
		fmt.Println("Send 40 failed!")
		return
	}

	// Step 3: Poll for connect ACK
	fmt.Println("═══════════════════════════════════════")
	fmt.Println("STEP 3: Poll for Connect ACK")
	fmt.Println("═══════════════════════════════════════")

	req3 := fmt.Sprintf("GET %s&sid=%s HTTP/1.1\r\n"+
		"Host: %s\r\n"+
		"Cookie: %s\r\n"+
		"Accept: */*\r\n"+
		"Connection: keep-alive\r\n\r\n",
		pollPath, sid, host, cookie)

	fmt.Printf(">>> REQUEST:\n%s\n", req3)

	tlsConn.SetDeadline(time.Now().Add(30 * time.Second))
	bw.WriteString(req3)
	bw.Flush()

	body3, code3, headers3 := readFullResponse(br)
	fmt.Printf("<<< RESPONSE: %d\n", code3)
	fmt.Printf("<<< HEADERS:\n%s\n", headers3)
	fmt.Printf("<<< BODY: %s\n\n", string(body3))

	fmt.Println("✅ Debug complete!")
}

func readFullResponse(br *bufio.Reader) ([]byte, int, string) {
	// Status line
	line, err := br.ReadString('\n')
	if err != nil {
		return nil, 0, fmt.Sprintf("read error: %v", err)
	}

	code := 0
	if len(line) >= 12 {
		code, _ = strconv.Atoi(strings.TrimSpace(line[9:12]))
	}

	// Headers
	var headers strings.Builder
	contentLen := 0
	chunked := false

	for {
		line, err := br.ReadString('\n')
		if err != nil {
			break
		}
		headers.WriteString(line)
		if line == "\r\n" {
			break
		}
		lower := strings.ToLower(line)
		if strings.HasPrefix(lower, "content-length:") {
			fmt.Sscanf(line[15:], "%d", &contentLen)
		}
		if strings.Contains(lower, "chunked") {
			chunked = true
		}
	}

	// Body
	var body []byte
	if chunked {
		for {
			sizeLine, _ := br.ReadString('\n')
			sizeLine = strings.TrimSpace(sizeLine)
			size, _ := strconv.ParseInt(sizeLine, 16, 64)
			if size == 0 {
				br.ReadString('\n')
				break
			}
			chunk := make([]byte, size)
			io.ReadFull(br, chunk)
			body = append(body, chunk...)
			br.ReadString('\n')
		}
	} else if contentLen > 0 {
		body = make([]byte, contentLen)
		io.ReadFull(br, body)
	}

	return body, code, headers.String()
}
