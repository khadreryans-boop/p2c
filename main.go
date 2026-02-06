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
	fmt.Println("║  DEBUG POLLING - Check Headers            ║")
	fmt.Println("╚═══════════════════════════════════════════╝")

	fmt.Print("\naccess_token cookie:\n> ")
	cookie, _ = in.ReadString('\n')
	cookie = strings.TrimSpace(cookie)

	fmt.Println("\n⏳ Resolving DNS...")
	ips, _ := net.LookupHost(host)
	ip := ips[0]
	fmt.Printf("Using IP: %s\n\n", ip)

	// Connect
	conn, err := tls.DialWithDialer(
		&net.Dialer{Timeout: 5 * time.Second},
		"tcp",
		ip+":443",
		&tls.Config{ServerName: host},
	)
	if err != nil {
		fmt.Printf("Connect error: %v\n", err)
		return
	}

	br := bufio.NewReaderSize(conn, 16384)
	bw := bufio.NewWriterSize(conn, 4096)

	// Extra cookies from responses
	var extraCookies []string

	// ═══════════════════════════════════════
	fmt.Println("═══════════════════════════════════════")
	fmt.Println("STEP 1: Handshake GET")
	fmt.Println("═══════════════════════════════════════")

	conn.SetDeadline(time.Now().Add(10 * time.Second))
	fmt.Fprintf(bw, "GET %s HTTP/1.1\r\n", pollPath)
	fmt.Fprintf(bw, "Host: %s\r\n", host)
	fmt.Fprintf(bw, "Cookie: %s\r\n", cookie)
	fmt.Fprintf(bw, "Origin: https://app.send.tg\r\n")
	fmt.Fprintf(bw, "Connection: keep-alive\r\n")
	fmt.Fprintf(bw, "\r\n")
	bw.Flush()

	body, code, headers, cookies := readResponseFull(br)
	extraCookies = append(extraCookies, cookies...)

	fmt.Printf("Code: %d\n", code)
	fmt.Printf("Headers:\n%s", headers)
	fmt.Printf("Cookies: %v\n", cookies)
	fmt.Printf("Body: %s\n\n", string(body))

	if code != 200 {
		return
	}

	// Parse sid
	sidIdx := bytes.Index(body, []byte(`"sid":"`))
	if sidIdx == -1 {
		fmt.Println("No sid!")
		return
	}
	sid := string(body[sidIdx+7 : sidIdx+7+bytes.IndexByte(body[sidIdx+7:], '"')])
	fmt.Printf("SID: %s\n\n", sid)

	// Build full cookie string
	fullCookie := cookie
	for _, c := range extraCookies {
		fullCookie += "; " + c
	}
	fmt.Printf("Full Cookie for next requests: %s\n\n", fullCookie)

	// ═══════════════════════════════════════
	fmt.Println("═══════════════════════════════════════")
	fmt.Println("STEP 2: Send 40 POST")
	fmt.Println("═══════════════════════════════════════")

	msg := "40"
	conn.SetDeadline(time.Now().Add(10 * time.Second))
	fmt.Fprintf(bw, "POST %s&sid=%s HTTP/1.1\r\n", pollPath, sid)
	fmt.Fprintf(bw, "Host: %s\r\n", host)
	fmt.Fprintf(bw, "Cookie: %s\r\n", fullCookie)
	fmt.Fprintf(bw, "Origin: https://app.send.tg\r\n")
	fmt.Fprintf(bw, "Content-Type: text/plain;charset=UTF-8\r\n")
	fmt.Fprintf(bw, "Content-Length: %d\r\n", len(msg))
	fmt.Fprintf(bw, "Connection: keep-alive\r\n")
	fmt.Fprintf(bw, "\r\n")
	fmt.Fprintf(bw, "%s", msg)
	bw.Flush()

	body, code, headers, cookies = readResponseFull(br)
	extraCookies = append(extraCookies, cookies...)

	fmt.Printf("Code: %d\n", code)
	fmt.Printf("Headers:\n%s", headers)
	fmt.Printf("Body: %s\n\n", string(body))

	if code != 200 {
		fmt.Println("FAILED at step 2!")
		return
	}

	// ═══════════════════════════════════════
	fmt.Println("═══════════════════════════════════════")
	fmt.Println("STEP 3: Poll GET for ACK")
	fmt.Println("═══════════════════════════════════════")

	// Rebuild cookie
	fullCookie = cookie
	for _, c := range extraCookies {
		fullCookie += "; " + c
	}

	conn.SetDeadline(time.Now().Add(30 * time.Second))
	fmt.Fprintf(bw, "GET %s&sid=%s HTTP/1.1\r\n", pollPath, sid)
	fmt.Fprintf(bw, "Host: %s\r\n", host)
	fmt.Fprintf(bw, "Cookie: %s\r\n", fullCookie)
	fmt.Fprintf(bw, "Origin: https://app.send.tg\r\n")
	fmt.Fprintf(bw, "Connection: keep-alive\r\n")
	fmt.Fprintf(bw, "\r\n")
	bw.Flush()

	body, code, headers, _ = readResponseFull(br)

	fmt.Printf("Code: %d\n", code)
	fmt.Printf("Headers:\n%s", headers)
	fmt.Printf("Body: %s\n\n", string(body))

	fmt.Println("✅ Debug complete!")
}

func readResponseFull(br *bufio.Reader) ([]byte, int, string, []string) {
	line, err := br.ReadString('\n')
	if err != nil {
		return nil, 0, "", nil
	}

	code := 0
	if len(line) >= 12 {
		code, _ = strconv.Atoi(strings.TrimSpace(line[9:12]))
	}

	var headersBuilder strings.Builder
	var cookies []string
	contentLen := 0
	chunked := false

	for {
		line, err := br.ReadString('\n')
		if err != nil {
			break
		}
		headersBuilder.WriteString(line)
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
		if strings.HasPrefix(lower, "set-cookie:") {
			// Extract cookie name=value
			cookiePart := strings.TrimSpace(line[11:])
			if idx := strings.Index(cookiePart, ";"); idx > 0 {
				cookiePart = cookiePart[:idx]
			}
			cookies = append(cookies, cookiePart)
		}
	}

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

	return body, code, headersBuilder.String(), cookies
}
