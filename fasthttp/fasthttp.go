package main

import (
	"bufio"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
	"github.com/valyala/fasthttp"
	"github.com/valyala/fastjson"
)

const UA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/142.0.0.0 Safari/537.36 OPR/126.0.0.0"

const (
	host          = "app.send.tg"
	wsURL         = "wss://app.send.tg/internal/v1/p2c-socket/?EIO=4&transport=websocket"
	origin        = "https://app.send.tg"
	referer       = "https://app.send.tg/p2c/orders"
	pauseSeconds  = 20
	numClients    = 20
	topSlotsCount = 4
)

// Предкомпилированные константы (избегаем аллокаций)
var (
	takeURLBase     = []byte("https://" + host + "/internal/v1/p2c/payments/take/")
	emptyJSONBody   = []byte(`{}`)
	hostBytes       = []byte(host)
	uaBytes         = []byte(UA)
	acceptJSON      = []byte("application/json")
	contentTypeJSON = []byte("application/json")
	originBytes     = []byte(origin)
	refererBytes    = []byte(referer)

	// Socket.IO
	pongMsg     = []byte("3")
	initMsg     = []byte(`42["list:initialize"]`)
	snapshotMsg = []byte(`42["list:snapshot",[]]`)
	connectMsg  = []byte("40")
)

var (
	pauseTaking atomic.Bool

	// Cookie для take запросов (глобально)
	cookieBytes []byte
)

type orderEvent struct {
	id     string
	wsTime time.Time
	amtStr string
}

// sync.Pool для переиспользования orderEvent
var orderEventPool = sync.Pool{
	New: func() interface{} {
		return &orderEvent{}
	},
}

// Каждая горутина имеет свой парсер (избегаем блокировок)
var parserPool = sync.Pool{
	New: func() interface{} {
		return &fastjson.Parser{}
	},
}

type clientSlot struct {
	name   string
	client *fasthttp.Client

	// EWMA RTT (микросекунды) - упакован для cache line
	ewmaUs  atomic.Uint64
	samples atomic.Uint64
	_pad    [40]byte // padding до 64 байт (cache line)
}

// Кешированный топ слотов - lockless через atomic
var topSlotIndexes atomic.Value // []int

var slots [numClients]*clientSlot

const (
	aNumer = 2
	aDenom = 10
)

func init() {
	// Максимизируем использование CPU
	runtime.GOMAXPROCS(runtime.NumCPU())

	// Инициализируем дефолтные индексы
	topSlotIndexes.Store([]int{0, 1, 2, 3})
}

// Обновляем кеш топ-слотов каждые 100ms (lockless)
func startTopCacheUpdater() {
	go func() {
		ticker := time.NewTicker(100 * time.Millisecond)

		type sv struct {
			idx int
			us  uint64
		}
		buf := make([]sv, numClients)

		for range ticker.C {
			for i := 0; i < numClients; i++ {
				buf[i].idx = i
				us := slots[i].ewmaUs.Load()
				if us == 0 {
					us = 9_000_000_000
				}
				buf[i].us = us
			}

			// Частичная сортировка топ-4
			for i := 0; i < topSlotsCount; i++ {
				minIdx := i
				for j := i + 1; j < numClients; j++ {
					if buf[j].us < buf[minIdx].us {
						minIdx = j
					}
				}
				buf[i], buf[minIdx] = buf[minIdx], buf[i]
			}

			// Atomic store новых индексов
			newIndexes := make([]int, topSlotsCount)
			for i := 0; i < topSlotsCount; i++ {
				newIndexes[i] = buf[i].idx
			}
			topSlotIndexes.Store(newIndexes)
		}
	}()
}

// Быстрое получение топ-слотов (lockless)
func getTopSlotIndexes() []int {
	return topSlotIndexes.Load().([]int)
}

// Оптимизированный парсинг decimal -> cents
// Без аллокаций, inline-friendly
func parseDecimalToCents(s string) (cents int64, ok bool) {
	if len(s) == 0 {
		return 0, false
	}

	var whole, frac int64
	var fracDigits int
	seenDot := false

	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '.':
			if seenDot {
				return 0, false
			}
			seenDot = true
		case c >= '0' && c <= '9':
			d := int64(c - '0')
			if !seenDot {
				whole = whole*10 + d
			} else if fracDigits < 2 {
				frac = frac*10 + d
				fracDigits++
			}
		default:
			return 0, false
		}
	}

	// Нормализация дробной части
	switch fracDigits {
	case 0:
		frac = 0
	case 1:
		frac *= 10
	}

	return whole*100 + frac, true
}

func makeFastClient() *fasthttp.Client {
	dialer := &net.Dialer{
		Timeout:   300 * time.Millisecond,
		KeepAlive: 30 * time.Second,
	}

	return &fasthttp.Client{
		Name:                          UA,
		ReadTimeout:                   500 * time.Millisecond,
		WriteTimeout:                  500 * time.Millisecond,
		MaxConnsPerHost:               256,
		MaxIdleConnDuration:           90 * time.Second,
		MaxConnWaitTimeout:            20 * time.Millisecond,
		DisableHeaderNamesNormalizing: true,
		DisablePathNormalizing:        true,
		TLSConfig: &tls.Config{
			MinVersion: tls.VersionTLS12,
			ServerName: host,
		},
		Dial: func(addr string) (net.Conn, error) {
			conn, err := dialer.Dial("tcp", addr)
			if err != nil {
				return nil, err
			}
			if tc, ok := conn.(*net.TCPConn); ok {
				tc.SetNoDelay(true)
				tc.SetLinger(0)
			}
			return conn, nil
		},
	}
}

func warmupSlot(slot *clientSlot) {
	req := fasthttp.AcquireRequest()
	resp := fasthttp.AcquireResponse()
	defer fasthttp.ReleaseRequest(req)
	defer fasthttp.ReleaseResponse(resp)

	req.SetRequestURI("https://" + host + "/p2c/orders")
	req.Header.SetMethodBytes([]byte("GET"))
	req.Header.SetBytesV("User-Agent", uaBytes)

	t0 := time.Now()
	err := slot.client.DoTimeout(req, resp, 1500*time.Millisecond)
	dur := time.Since(t0)

	us := uint64(dur.Microseconds())
	if err != nil {
		us = 2_000_000
	}

	updateEWMA(&slot.ewmaUs, us)
	slot.samples.Add(1)
}

// Inline EWMA update
func updateEWMA(ewma *atomic.Uint64, newUs uint64) {
	for {
		old := ewma.Load()
		var newVal uint64
		if old == 0 {
			newVal = newUs
		} else {
			newVal = (old*(aDenom-aNumer) + newUs*aNumer) / aDenom
		}
		if ewma.CompareAndSwap(old, newVal) {
			return
		}
		// CAS failed, retry
	}
}

func doTakeOnce(client *fasthttp.Client, cookieBytes []byte, id string) (code int, dur time.Duration, err error) {
	req := fasthttp.AcquireRequest()
	resp := fasthttp.AcquireResponse()
	defer fasthttp.ReleaseRequest(req)
	defer fasthttp.ReleaseResponse(resp)

	// Строим URL без аллокаций где возможно
	urlBuf := make([]byte, 0, len(takeURLBase)+len(id))
	urlBuf = append(urlBuf, takeURLBase...)
	urlBuf = append(urlBuf, id...)

	req.SetRequestURIBytes(urlBuf)
	req.Header.SetMethodBytes([]byte("POST"))
	req.Header.SetHostBytes(hostBytes)
	req.Header.SetBytesV("User-Agent", uaBytes)
	req.Header.SetBytesKV([]byte("Accept"), acceptJSON)
	req.Header.SetBytesKV([]byte("Content-Type"), contentTypeJSON)
	req.Header.SetBytesKV([]byte("Origin"), originBytes)
	req.Header.SetBytesKV([]byte("Referer"), refererBytes)
	req.Header.SetBytesKV([]byte("Cookie"), cookieBytes)
	req.SetBodyRaw(emptyJSONBody)

	t0 := time.Now()
	err = client.DoRedirects(req, resp, 5)
	dur = time.Since(t0)

	if err != nil {
		return 0, dur, err
	}
	return resp.StatusCode(), dur, nil
}

func takeOrderFast(slot *clientSlot, cookieBytes []byte, ev *orderEvent, ewmaAtPickUs uint64) {
	if pauseTaking.Load() {
		orderEventPool.Put(ev)
		return
	}

	code, dur, err := doTakeOnce(slot.client, cookieBytes, ev.id)

	rttUs := dur.Microseconds()
	e2eUs := time.Since(ev.wsTime).Microseconds()

	// Обновляем EWMA
	us := uint64(rttUs)
	if err != nil {
		us = 2_000_000
	}
	updateEWMA(&slot.ewmaUs, us)
	slot.samples.Add(1)

	preMs := e2eUs/1000 - rttUs/1000
	rttMs := rttUs / 1000
	e2eMs := e2eUs / 1000

	if err != nil {
		fmt.Printf("✗ %s err pre=%dms rtt=%dms id=%s\n", slot.name, preMs, rttMs, ev.id[:12])
		orderEventPool.Put(ev)
		return
	}

	switch code {
	case 200:
		fmt.Printf("✓ OK %s e2e=%dms id=%s\n", slot.name, e2eMs, ev.id)
		pauseTaking.Store(true)
		go func() {
			time.Sleep(pauseSeconds * time.Second)
			pauseTaking.Store(false)
		}()
	case 400:
		fmt.Printf("✗ %s 400 pre=%dms rtt=%dms e2e=%dms\n", slot.name, preMs, rttMs, e2eMs)
	default:
		fmt.Printf("✗ %s %d pre=%dms rtt=%dms e2e=%dms\n", slot.name, code, preMs, rttMs, e2eMs)
	}

	orderEventPool.Put(ev)
}

func handleSocketIOMessage(parser *fastjson.Parser, minCents int64, baseTime time.Time, msg string) {
	// Быстрый выход для ping/pong
	if len(msg) < 50 || msg[0] != '4' || msg[1] != '2' {
		return
	}

	// Ищем "op":"add" - если нет, выходим
	if !strings.Contains(msg, `"op":"add"`) {
		return
	}

	if pauseTaking.Load() {
		return
	}

	// Быстрый поиск id после "op":"add"
	// Формат: ..."id":"6983cf2dc79a7ad174193fc6"...
	idStart := strings.Index(msg, `"id":"`)
	if idStart == -1 {
		return
	}
	idStart += 6 // длина `"id":"`
	idEnd := strings.IndexByte(msg[idStart:], '"')
	if idEnd == -1 || idEnd < 20 {
		return
	}
	id := msg[idStart : idStart+idEnd]

	// Быстрый поиск in_amount
	var amtStr string
	amtStart := strings.Index(msg, `"in_amount":"`)
	if amtStart != -1 {
		amtStart += 13
		amtEnd := strings.IndexByte(msg[amtStart:], '"')
		if amtEnd > 0 {
			amtStr = msg[amtStart : amtStart+amtEnd]
		}
	}

	// Фильтр по сумме
	if minCents > 0 {
		cents, ok := parseDecimalToCents(amtStr)
		if !ok || cents < minCents {
			return
		}
	}

	// Выбираем топ-2 клиента по EWMA (минимальный RTT)
	var best1, best2 *clientSlot
	var min1, min2 uint64 = ^uint64(0), ^uint64(0)

	for i := 0; i < numClients; i++ {
		s := slots[i]
		ewma := s.ewmaUs.Load()
		if ewma == 0 {
			ewma = 9_000_000_000
		}
		if ewma < min1 {
			min2, best2 = min1, best1
			min1, best1 = ewma, s
		} else if ewma < min2 {
			min2, best2 = ewma, s
		}
	}

	fmt.Printf("ORDER %s amt=%s → %s(%.1fms) %s(%.1fms)\n",
		id[:12], amtStr,
		best1.name, float64(min1)/1000.0,
		best2.name, float64(min2)/1000.0)

	// Отправляем take на лучших клиентов
	if best1 != nil {
		ev := orderEventPool.Get().(*orderEvent)
		ev.id = id
		ev.wsTime = baseTime
		ev.amtStr = amtStr
		go takeOrderFast(best1, cookieBytes, ev, min1)
	}
	if best2 != nil {
		ev := orderEventPool.Get().(*orderEvent)
		ev.id = id
		ev.wsTime = baseTime
		ev.amtStr = amtStr
		go takeOrderFast(best2, cookieBytes, ev, min2)
	}
}

func splitPackets(s string) []string {
	if !strings.Contains(s, "\x1e") {
		return []string{s}
	}
	parts := strings.Split(s, "\x1e")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func main() {
	in := bufio.NewReader(os.Stdin)

	fmt.Print("Enter access_token cookie (format: access_token=...):\n> ")
	accessCookie, _ := in.ReadString('\n')
	accessCookie = strings.TrimSpace(accessCookie)

	if accessCookie == "" {
		fmt.Println("Cookie is empty. Exit.")
		return
	}
	if !strings.HasPrefix(accessCookie, "access_token=") {
		fmt.Println("Expected format: access_token=...")
		return
	}

	// Сохраняем в глобальную переменную
	cookieBytes = []byte(accessCookie)

	fmt.Print("Enter MIN in_amount (e.g. 300). 0 = no filter:\n> ")
	minLine, _ := in.ReadString('\n')
	minLine = strings.TrimSpace(minLine)

	minCents := int64(0)
	if minLine != "" {
		f, err := strconv.ParseFloat(minLine, 64)
		if err != nil {
			fmt.Println("Bad MIN amount, expected number. Exit.")
			return
		}
		minCents = int64(f * 100.0)
	}

	// Инициализация клиентов
	for i := 0; i < numClients; i++ {
		slots[i] = &clientSlot{
			name:   fmt.Sprintf("C%d", i+1),
			client: makeFastClient(),
		}
	}

	startTopCacheUpdater()

	// Прогрев соединений
	fmt.Println("Warming up connections...")
	var wg sync.WaitGroup
	for i := 0; i < numClients; i++ {
		wg.Add(1)
		go func(s *clientSlot) {
			defer wg.Done()
			warmupSlot(s)
		}(slots[i])
	}
	wg.Wait()
	fmt.Println("Warmup complete")

	// Фоновый прогрев
	for i := 0; i < numClients; i++ {
		s := slots[i]
		go func() {
			ticker := time.NewTicker(2 * time.Second)
			for range ticker.C {
				warmupSlot(s)
			}
		}()
	}

	// WebSocket
	wsDialer := websocket.Dialer{
		HandshakeTimeout:  6 * time.Second,
		EnableCompression: false,
		TLSClientConfig: &tls.Config{
			MinVersion: tls.VersionTLS12,
			ServerName: host,
		},
		NetDialContext: (&net.Dialer{
			Timeout:   3 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
	}

	header := http.Header{}
	header.Set("Host", host)
	header.Set("Origin", origin)
	header.Set("User-Agent", UA)
	header.Set("Cookie", accessCookie)
	header.Set("Pragma", "no-cache")
	header.Set("Cache-Control", "no-cache")

	ws, resp, err := wsDialer.Dial(wsURL, header)
	if err != nil {
		if resp != nil {
			fmt.Println("WS dial failed, HTTP status:", resp.Status)
		}
		fmt.Println("WS dial err:", err)
		return
	}
	defer ws.Close()

	pongWait := 60 * time.Second
	ws.SetReadLimit(1 << 20)
	ws.SetReadDeadline(time.Now().Add(pongWait))

	ws.SetPongHandler(func(string) error {
		ws.SetReadDeadline(time.Now().Add(pongWait))
		return nil
	})
	ws.SetPingHandler(func(appData string) error {
		ws.WriteControl(websocket.PongMessage, []byte(appData), time.Now().Add(2*time.Second))
		ws.SetReadDeadline(time.Now().Add(pongWait))
		return nil
	})

	stopPing := make(chan struct{})
	go func() {
		ticker := time.NewTicker(25 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				ws.WriteControl(websocket.PingMessage, []byte("ping"), time.Now().Add(2*time.Second))
			case <-stopPing:
				return
			}
		}
	}()
	defer close(stopPing)

	// OPEN packet
	_, _, err = ws.ReadMessage()
	if err != nil {
		return
	}

	ws.WriteMessage(websocket.TextMessage, connectMsg)

	// ACK
	ws.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, _, err = ws.ReadMessage()
	ws.SetReadDeadline(time.Now().Add(pongWait))
	if err != nil {
		return
	}

	ws.WriteMessage(websocket.TextMessage, initMsg)
	ws.WriteMessage(websocket.TextMessage, snapshotMsg)

	fmt.Println("Listening for orders...")

	// Получаем парсер для главной горутины
	parser := parserPool.Get().(*fastjson.Parser)
	defer parserPool.Put(parser)

	for {
		_, raw, err := ws.ReadMessage()
		if err != nil {
			fmt.Println("WS read error:", err)
			return
		}
		wsNow := time.Now()

		s := string(raw)
		for _, pkt := range splitPackets(s) {
			if pkt == "2" {
				ws.WriteMessage(websocket.TextMessage, pongMsg)
				continue
			}
			handleSocketIOMessage(parser, minCents, wsNow, pkt)
		}
	}
}
