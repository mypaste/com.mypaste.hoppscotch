// Package ws performs one short WebSocket session for the workspace page:
// connect, send the messages the page composed, collect whatever frames come
// back for a bounded window, close, and report the transcript.
//
// The host's command runtime is one process per call and one answer line, so
// there is no socket left open between sends: a WebSocket request *is* a whole
// session, and the page's 连接 button runs it once. This package is the client
// side of RFC 6455 with exactly what that session needs — text frames, control
// frames and close — and no extensions, compression or reconnect. Nothing here
// writes a URL, header or message anywhere but into the answer line.
package ws

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha1"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/textproto"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	// MaxMessages, MaxMessageBytes and MaxSendBytes bound what one session
	// may hand the server; the page enforces the same numbers so a session is
	// never refused after it has connected.
	MaxMessages     = 50
	MaxMessageBytes = 64 << 10
	MaxSendBytes    = 256 << 10
	// MaxFrameBytes bounds one incoming frame. A frame longer than this is a
	// protocol error (RFC 6455 §7.4.1) and ends the session.
	MaxFrameBytes = 256 << 10
	// MaxFrames, MaxFrameText and MaxTranscriptBytes bound the transcript the
	// answer line carries back to the page.
	MaxFrames          = 200
	MaxFrameText       = 8 << 10
	MaxTranscriptBytes = 128 << 10
	// MaxHeaders and MaxHeaderBytes bound the handshake response recorded in
	// the transcript's header tab.
	MaxHeaders     = 64
	MaxHeaderBytes = 32 << 10
	MaxURLBytes    = 8 << 10
	// holdWindow is how long the session keeps listening after its last
	// message; the page offers 1, 3 and 10 seconds.
	DefaultHoldMs = 3000
	MaxHoldMs     = 10000
	MinHoldMs     = 100
	// DefaultTimeout and MaxTimeout bound the whole session, connect
	// included. The host's own deadline still wins when it is shorter.
	DefaultTimeout = 30 * time.Second
	MaxTimeout     = 60 * time.Second
	connectTimeout = 10 * time.Second
	// sendGap gives a server that answers per message its turn between two
	// sends: the session listens for this long after each message.
	sendGap = 120 * time.Millisecond
	// handshakeBudget is the most the handshake may read, headers and all.
	handshakeBudget = 128 << 10
	acceptGUID      = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"
	userAgent       = "MyPaste-hoppscotch/1.0"
)

// Frame opcodes (RFC 6455 §5.2).
const (
	opContinuation = 0x0
	opText         = 0x1
	opBinary       = 0x2
	opClose        = 0x8
	opPing         = 0x9
	opPong         = 0xA
)

// Header is one user-visible handshake header row.
type Header struct {
	Name    string `json:"name"`
	Value   string `json:"value"`
	Enabled bool   `json:"enabled"`
}

// Request is the page's JSON session description.
type Request struct {
	URL       string   `json:"url"`
	Headers   []Header `json:"headers,omitempty"`
	Messages  []string `json:"messages"`
	HoldMs    int      `json:"holdMs,omitempty"`
	TimeoutMs int      `json:"timeoutMs,omitempty"`
}

// Frame is one line of the transcript.
type Frame struct {
	AtMs      int64  `json:"atMs"`
	Direction string `json:"direction"`
	Type      string `json:"type"`
	SizeBytes int64  `json:"sizeBytes"`
	Text      string `json:"text,omitempty"`
	Truncated bool   `json:"truncated,omitempty"`
	Invalid   bool   `json:"invalid,omitempty"`
}

// Close is the close the server sent, or the one this client sent.
type Close struct {
	Code   int    `json:"code"`
	Reason string `json:"reason,omitempty"`
}

// Session is what the page renders: the handshake, the transcript and why the
// session ended.
type Session struct {
	Status      int      `json:"status"`
	StatusText  string   `json:"statusText"`
	Protocol    string   `json:"protocol"`
	URL         string   `json:"url"`
	ConnectedMs int64    `json:"connectedMs"`
	DurationMs  int64    `json:"durationMs"`
	Headers     []Header `json:"headers"`
	Frames      []Frame  `json:"frames"`
	Close       *Close   `json:"close,omitempty"`
	BytesIn     int64    `json:"bytesIn"`
	BytesOut    int64    `json:"bytesOut"`
	Truncated   bool     `json:"truncated"`
	Note        string   `json:"note,omitempty"`
}

// Error carries the code the page shows next to the sentence.
type Error struct {
	Code    string
	Message string
}

func (e *Error) Error() string { return e.Message }

func failf(code, format string, args ...any) error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

// Dial runs one session: it returns only after the connection is closed, so
// the answer is the whole transcript.
func Dial(ctx context.Context, req Request) (*Session, error) {
	target, err := targetURL(req.URL)
	if err != nil {
		return nil, err
	}
	headers, err := handshakeHeaders(req.Headers)
	if err != nil {
		return nil, err
	}
	messages, err := messagesOf(req.Messages)
	if err != nil {
		return nil, err
	}
	timeout := requestTimeout(req.TimeoutMs)
	hold := holdWindow(req.HoldMs)
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	start := time.Now()
	link, err := dial(ctx, target, headers)
	if err != nil {
		return nil, err
	}
	defer link.conn.Close()

	session := &Session{
		Status:      link.status,
		StatusText:  link.statusText,
		Protocol:    target.Scheme,
		URL:         target.String(),
		ConnectedMs: time.Since(start).Milliseconds(),
		Headers:     link.headers,
		Frames:      []Frame{},
	}
	wire := &sessionWire{
		ctx:   ctx,
		conn:  link.conn,
		br:    link.reader,
		bw:    bufio.NewWriterSize(link.conn, 8<<10),
		s:     session,
		start: start,
	}
	// The host's deadline is a hard stop; closing the connection is what
	// wakes a read that is waiting on the server.
	stopped := make(chan struct{})
	defer close(stopped)
	go func() {
		select {
		case <-ctx.Done():
			_ = link.conn.SetDeadline(time.Now())
		case <-stopped:
		}
	}()

	var last time.Time
	for _, message := range messages {
		if err := wire.send(opText, []byte(message)); err != nil {
			wire.stop("发送失败：" + oneLine(err.Error()))
			break
		}
		wire.record("out", "text", int64(len(message)), message, false)
		last = time.Now()
		if !wire.pump(last.Add(sendGap)) {
			break
		}
	}
	// The collection window opens when the last message is on the wire, so a
	// fast server loses nothing and a slow one gets the window it was given.
	if len(messages) > 0 && wire.stillOpen() {
		wire.pump(last.Add(hold))
	}

	session.DurationMs = time.Since(start).Milliseconds()
	session.BytesIn = wire.bytesIn
	session.BytesOut = wire.bytesOut
	return session, nil
}

// --- connection --------------------------------------------------------

type handshake struct {
	conn       net.Conn
	reader     *bufio.Reader
	status     int
	statusText string
	headers    []Header
}

// tlsConfigFor is the one place a wss:// connection gets its TLS settings;
// tests replace it to trust a local server's certificate.
var tlsConfigFor = func(host string) *tls.Config {
	return &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12}
}

func dial(ctx context.Context, target *url.URL, rows []Header) (*handshake, error) {
	secure := target.Scheme == "wss"
	port := target.Port()
	if port == "" {
		if secure {
			port = "443"
		} else {
			port = "80"
		}
	}
	address := net.JoinHostPort(target.Hostname(), port)
	dialer := net.Dialer{Timeout: connectTimeout, KeepAlive: 30 * time.Second}
	conn, err := dialer.DialContext(ctx, "tcp", address)
	if err != nil {
		return nil, failf("connect", "无法连接 %s：%s", address, oneLine(err.Error()))
	}
	owned := false
	defer func() {
		if !owned {
			conn.Close()
		}
	}()
	if secure {
		tlsConn := tls.Client(conn, tlsConfigFor(target.Hostname()))
		handshakeCtx, cancel := context.WithTimeout(ctx, connectTimeout)
		err := tlsConn.HandshakeContext(handshakeCtx)
		cancel()
		if err != nil {
			return nil, failf("connect", "TLS 握手失败：%s", oneLine(err.Error()))
		}
		conn = tlsConn
	}
	key, err := randomKey()
	if err != nil {
		return nil, failf("connect", "无法生成握手密钥")
	}
	_ = conn.SetDeadline(time.Now().Add(connectTimeout))
	if _, err := io.WriteString(conn, handshakeRequest(target, key, rows)); err != nil {
		return nil, failf("handshake", "握手请求没有写出：%s", oneLine(err.Error()))
	}
	budget := &budgetReader{reader: conn, left: handshakeBudget}
	reader := bufio.NewReaderSize(budget, 32<<10)
	text := textproto.NewReader(reader)
	line, err := text.ReadLine()
	if err != nil {
		return nil, failf("handshake", "读不到服务器的握手响应：%s", oneLine(err.Error()))
	}
	status, statusText, ok := parseStatusLine(line)
	if !ok {
		return nil, failf("handshake", "服务器返回的握手响应无法识别")
	}
	header, err := text.ReadMIMEHeader()
	if err != nil {
		return nil, failf("handshake", "握手响应头无法读取：%s", oneLine(err.Error()))
	}
	if status != 101 {
		return nil, failf("handshake", "服务器拒绝了 WebSocket 升级：HTTP %d %s",
			status, statusText)
	}
	if !strings.EqualFold(strings.TrimSpace(header.Get("Upgrade")), "websocket") {
		return nil, failf("handshake", "服务器的响应没有声明 Upgrade: websocket")
	}
	if !headerTokens(header.Values("Connection"), "upgrade") {
		return nil, failf("handshake", "服务器的响应没有声明 Connection: Upgrade")
	}
	if strings.TrimSpace(header.Get("Sec-WebSocket-Accept")) != acceptFor(key) {
		return nil, failf("handshake", "服务器的握手校验值不匹配")
	}
	// The upgraded connection is a byte stream now: no more HTTP deadlines,
	// and no more handshake budget — the frames that follow are bounded
	// frame by frame instead.
	_ = conn.SetDeadline(time.Time{})
	budget.left = 1 << 62
	owned = true
	return &handshake{
		conn:       conn,
		reader:     reader,
		status:     status,
		statusText: statusText,
		headers:    responseHeaders(header),
	}, nil
}

// acceptFor is the RFC 6455 §1.3 answer to the client's key.
func acceptFor(key string) string {
	sum := sha1.Sum([]byte(key + acceptGUID))
	return base64.StdEncoding.EncodeToString(sum[:])
}

func randomKey() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(buf), nil
}

func handshakeRequest(target *url.URL, key string, rows []Header) string {
	path := target.RequestURI()
	if path == "" {
		path = "/"
	}
	var request strings.Builder
	request.WriteString("GET " + path + " HTTP/1.1\r\n")
	request.WriteString("Host: " + target.Host + "\r\n")
	request.WriteString("Upgrade: websocket\r\n")
	request.WriteString("Connection: Upgrade\r\n")
	request.WriteString("Sec-WebSocket-Key: " + key + "\r\n")
	request.WriteString("Sec-WebSocket-Version: 13\r\n")
	request.WriteString("User-Agent: " + userAgent + "\r\n")
	for _, row := range rows {
		request.WriteString(row.Name + ": " + row.Value + "\r\n")
	}
	request.WriteString("\r\n")
	return request.String()
}

func parseStatusLine(line string) (int, string, bool) {
	parts := strings.SplitN(strings.TrimSpace(line), " ", 3)
	if len(parts) < 2 || !strings.HasPrefix(parts[0], "HTTP/") {
		return 0, "", false
	}
	status, err := strconv.Atoi(parts[1])
	if err != nil || status < 100 || status > 999 {
		return 0, "", false
	}
	text := ""
	if len(parts) == 3 {
		text = strings.TrimSpace(parts[2])
	}
	return status, text, true
}

func headerTokens(values []string, want string) bool {
	for _, value := range values {
		for _, token := range strings.Split(value, ",") {
			if strings.EqualFold(strings.TrimSpace(token), want) {
				return true
			}
		}
	}
	return false
}

// budgetReader bounds what the handshake may read. Once the upgrade is
// accepted the budget is lifted and the connection is read through the same
// buffer, so bytes the server sent right after the headers are not lost.
type budgetReader struct {
	reader io.Reader
	left   int64
}

func (b *budgetReader) Read(p []byte) (int, error) {
	if b.left <= 0 {
		return 0, errors.New("握手响应超过大小上限")
	}
	if int64(len(p)) > b.left {
		p = p[:b.left]
	}
	n, err := b.reader.Read(p)
	b.left -= int64(n)
	return n, err
}

// --- frames ------------------------------------------------------------

type frame struct {
	final   bool
	opcode  byte
	payload []byte
}

// writeFrame sends one masked client frame (RFC 6455 §5.3 requires masking).
func writeFrame(w io.Writer, opcode byte, payload []byte, mask [4]byte) error {
	head := make([]byte, 0, 14)
	head = append(head, 0x80|opcode)
	length := len(payload)
	switch {
	case length < 126:
		head = append(head, 0x80|byte(length))
	case length <= 0xFFFF:
		head = append(head, 0x80|126, 0, 0)
		binary.BigEndian.PutUint16(head[len(head)-2:], uint16(length))
	default:
		head = append(head, 0x80|127, 0, 0, 0, 0, 0, 0, 0, 0)
		binary.BigEndian.PutUint64(head[len(head)-8:], uint64(length))
	}
	head = append(head, mask[:]...)
	masked := make([]byte, length)
	for index, value := range payload {
		masked[index] = value ^ mask[index%4]
	}
	if _, err := w.Write(append(head, masked...)); err != nil {
		return err
	}
	return nil
}

// readFrame reads one frame. A server frame must never be masked (§5.1), and
// an oversized length is refused before its payload is allocated.
func readFrame(r io.Reader) (frame, error) {
	var head [2]byte
	if _, err := io.ReadFull(r, head[:]); err != nil {
		return frame{}, err
	}
	if head[0]&0x70 != 0 {
		return frame{}, failf("protocol", "服务器使用了未协商的扩展位")
	}
	opcode := head[0] & 0x0F
	length := int64(head[1] & 0x7F)
	if head[1]&0x80 != 0 {
		return frame{}, failf("protocol", "服务器发来了带掩码的帧")
	}
	switch length {
	case 126:
		var extended [2]byte
		if _, err := io.ReadFull(r, extended[:]); err != nil {
			return frame{}, err
		}
		length = int64(binary.BigEndian.Uint16(extended[:]))
	case 127:
		var extended [8]byte
		if _, err := io.ReadFull(r, extended[:]); err != nil {
			return frame{}, err
		}
		if extended[0]&0x80 != 0 {
			return frame{}, failf("protocol", "帧长度无效")
		}
		length = int64(binary.BigEndian.Uint64(extended[:]))
	}
	if length > MaxFrameBytes {
		return frame{}, failf("protocol", "单帧超过 %s", sizeNote(MaxFrameBytes))
	}
	if opcode >= opClose && (length > 125 || head[0]&0x80 == 0) {
		return frame{}, failf("protocol", "控制帧格式不正确")
	}
	payload := make([]byte, length)
	if _, err := io.ReadFull(r, payload); err != nil {
		return frame{}, err
	}
	return frame{final: head[0]&0x80 != 0, opcode: opcode, payload: payload}, nil
}

// --- session -----------------------------------------------------------

type sessionWire struct {
	ctx      context.Context
	conn     net.Conn
	br       *bufio.Reader
	bw       *bufio.Writer
	s        *Session
	start    time.Time
	bytesIn  int64
	bytesOut int64
	kept     int
	fragment []byte
	fragOp   byte
	closed   bool
	failed   bool
	full     bool
}

func (w *sessionWire) stillOpen() bool {
	return !w.closed && !w.failed && !w.full
}

// stop records why the session ended early. It never overwrites a note the
// protocol already set.
func (w *sessionWire) stop(note string) {
	w.failed = true
	if w.s.Note == "" {
		w.s.Note = note
	}
}

func (w *sessionWire) send(opcode byte, payload []byte) error {
	var mask [4]byte
	if _, err := rand.Read(mask[:]); err != nil {
		return err
	}
	_ = w.conn.SetWriteDeadline(time.Now().Add(connectTimeout))
	if err := writeFrame(w.bw, opcode, payload, mask); err != nil {
		return err
	}
	if err := w.bw.Flush(); err != nil {
		return err
	}
	w.bytesOut += int64(len(payload))
	return nil
}

// pump reads frames until the deadline, a close, or a protocol failure. It
// reports whether the session may continue.
func (w *sessionWire) pump(deadline time.Time) bool {
	// A deadline set by the host's own budget must not be undone by the
	// window this pump is about to arm.
	if w.ctx.Err() != nil {
		w.stop("会话在收集完成前被截止")
		return false
	}
	_ = w.conn.SetReadDeadline(deadline)
	for w.stillOpen() {
		fr, err := readFrame(w.br)
		if err != nil {
			if isTimeout(err) {
				return true
			}
			var typed *Error
			if errors.As(err, &typed) {
				w.stop(typed.Message)
				return false
			}
			w.stop("连接中断：" + oneLine(err.Error()))
			return false
		}
		if !w.handle(fr) {
			return false
		}
	}
	return false
}

func (w *sessionWire) handle(fr frame) bool {
	switch fr.opcode {
	case opText, opBinary:
		if w.fragOp != 0 {
			w.stop("服务器在分片消息中间开始了新消息")
			return false
		}
		if !fr.final {
			w.fragOp = fr.opcode
			w.fragment = append(w.fragment[:0], fr.payload...)
			return true
		}
		w.message(fr.opcode, fr.payload)
		return true
	case opContinuation:
		if w.fragOp == 0 {
			w.stop("服务器发来了没有开头的分片")
			return false
		}
		if len(w.fragment)+len(fr.payload) > MaxFrameBytes {
			w.stop("分片消息超过 " + sizeNote(MaxFrameBytes))
			return false
		}
		w.fragment = append(w.fragment, fr.payload...)
		if fr.final {
			opcode := w.fragOp
			payload := w.fragment
			w.fragOp = 0
			w.fragment = nil
			w.message(opcode, payload)
		}
		return true
	case opPing:
		if err := w.send(opPong, fr.payload); err != nil {
			w.stop("回复 ping 失败：" + oneLine(err.Error()))
			return false
		}
		w.record("in", "ping", int64(len(fr.payload)), "", false)
		return true
	case opPong:
		w.record("in", "pong", int64(len(fr.payload)), "", false)
		return true
	case opClose:
		w.closed = true
		code, reason := parseClose(fr.payload)
		w.s.Close = &Close{Code: code, Reason: reason}
		w.record("in", "close", int64(len(fr.payload)), closeText(code, reason), false)
		_ = w.send(opClose, fr.payload)
		return false
	default:
		w.stop("服务器使用了未知的帧类型")
		return false
	}
}

func (w *sessionWire) message(opcode byte, payload []byte) {
	w.bytesIn += int64(len(payload))
	if opcode == opBinary {
		w.record("in", "binary", int64(len(payload)), "", false)
		return
	}
	if !utf8.Valid(payload) {
		w.record("in", "text", int64(len(payload)), "", true)
		return
	}
	w.record("in", "text", int64(len(payload)), string(payload), false)
}

// record appends one transcript line inside the frame and byte caps. The
// metadata of a dropped frame is dropped with it; the session says so instead
// of pretending the transcript is whole.
func (w *sessionWire) record(direction, kind string, size int64, text string, invalid bool) {
	if len(w.s.Frames) >= MaxFrames {
		w.s.Truncated = true
		w.full = true
		if w.s.Note == "" {
			w.s.Note = "已记录 " + strconv.Itoa(MaxFrames) + " 帧，之后的帧不再收集"
		}
		return
	}
	entry := Frame{
		AtMs:      time.Since(w.start).Milliseconds(),
		Direction: direction,
		Type:      kind,
		SizeBytes: size,
		Invalid:   invalid,
	}
	if text != "" {
		room := MaxTranscriptBytes - w.kept
		if room <= 0 {
			w.s.Truncated = true
			entry.Truncated = true
		} else {
			cut := len(text)
			if cut > MaxFrameText {
				cut = MaxFrameText
			}
			if cut > room {
				cut = room
			}
			if cut < len(text) {
				entry.Truncated = true
				w.s.Truncated = true
			}
			entry.Text = trimUTF8(text[:cut])
			w.kept += len(entry.Text)
		}
	}
	w.s.Frames = append(w.s.Frames, entry)
}

func parseClose(payload []byte) (int, string) {
	if len(payload) < 2 {
		return 0, ""
	}
	return int(binary.BigEndian.Uint16(payload[:2])), trimUTF8(string(payload[2:]))
}

func closeText(code int, reason string) string {
	if code == 0 {
		return ""
	}
	return strconv.Itoa(code) + " " + reason
}

func isTimeout(err error) bool {
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}

// --- validation --------------------------------------------------------

// targetURL is the scheme gate of this package: a WebSocket client, not an
// HTTP or file reader.
func targetURL(raw string) (*url.URL, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, failf("request", "先填写连接地址")
	}
	if len(raw) > MaxURLBytes {
		return nil, failf("request", "地址超过 %s", sizeNote(MaxURLBytes))
	}
	target, err := url.Parse(raw)
	if err != nil || target.Host == "" {
		return nil, failf("request", "地址无法解析；请写成 ws://主机/路径")
	}
	if target.Scheme != "ws" && target.Scheme != "wss" {
		return nil, failf("request", "地址只能使用 ws 或 wss")
	}
	if target.User != nil {
		return nil, failf("request", "地址里不能带账号；请把凭据放进请求头")
	}
	// A fragment is never sent on the wire (§3), so it is dropped here rather
	// than being echoed back as if it had been.
	target.Fragment = ""
	return target, nil
}

// reserved are the headers this client writes itself. A row naming one is
// refused instead of silently ignored: the page is where the user can see it.
var reserved = map[string]bool{
	"host":                     true,
	"upgrade":                  true,
	"connection":               true,
	"sec-websocket-key":        true,
	"sec-websocket-version":    true,
	"sec-websocket-extensions": true,
	"sec-websocket-protocol":   true,
	"content-length":           true,
	"transfer-encoding":        true,
}

func handshakeHeaders(rows []Header) ([]Header, error) {
	result := make([]Header, 0, len(rows))
	total := 0
	for _, row := range rows {
		if !row.Enabled {
			continue
		}
		name := strings.TrimSpace(row.Name)
		if name == "" {
			continue
		}
		if !validHeaderName(name) {
			return nil, failf("request", "请求头名称无效：%s", oneLine(name))
		}
		if reserved[strings.ToLower(name)] {
			return nil, failf("request", "请求头 %s 由扩展自己发送，请删掉这一行", name)
		}
		value := strings.TrimSpace(row.Value)
		if !validHeaderValue(value) {
			return nil, failf("request", "请求头 %s 的值包含换行或控制字符", oneLine(name))
		}
		if len(result) >= MaxHeaders {
			return nil, failf("request", "请求头最多 %d 个", MaxHeaders)
		}
		total += len(name) + len(value)
		if total > MaxHeaderBytes {
			return nil, failf("request", "请求头总大小超过 %s", sizeNote(MaxHeaderBytes))
		}
		result = append(result, Header{Name: name, Value: value, Enabled: true})
	}
	return result, nil
}

func messagesOf(raw []string) ([]string, error) {
	messages := make([]string, 0, len(raw))
	total := 0
	for _, value := range raw {
		value = strings.TrimSuffix(value, "\r")
		if strings.TrimSpace(value) == "" {
			continue
		}
		if len(value) > MaxMessageBytes {
			return nil, failf("request", "单条消息超过 %s", sizeNote(MaxMessageBytes))
		}
		total += len(value)
		if total > MaxSendBytes {
			return nil, failf("request", "消息合计超过 %s", sizeNote(MaxSendBytes))
		}
		messages = append(messages, value)
		if len(messages) > MaxMessages {
			return nil, failf("request", "一次会话最多发送 %d 条消息", MaxMessages)
		}
	}
	if len(messages) == 0 {
		return nil, failf("request", "没有要发送的消息")
	}
	return messages, nil
}

func holdWindow(ms int) time.Duration {
	if ms <= 0 {
		return DefaultHoldMs * time.Millisecond
	}
	hold := time.Duration(ms) * time.Millisecond
	if hold < MinHoldMs*time.Millisecond {
		return MinHoldMs * time.Millisecond
	}
	if hold > MaxHoldMs*time.Millisecond {
		return MaxHoldMs * time.Millisecond
	}
	return hold
}

func requestTimeout(ms int) time.Duration {
	if ms <= 0 {
		return DefaultTimeout
	}
	timeout := time.Duration(ms) * time.Millisecond
	if timeout < time.Second {
		return time.Second
	}
	if timeout > MaxTimeout {
		return MaxTimeout
	}
	return timeout
}

func validHeaderName(name string) bool {
	if name == "" || len(name) > 128 {
		return false
	}
	for index := 0; index < len(name); index++ {
		char := name[index]
		switch {
		case char >= 'a' && char <= 'z',
			char >= 'A' && char <= 'Z',
			char >= '0' && char <= '9':
		case strings.ContainsRune("!#$%&'*+-.^_`|~", rune(char)):
		default:
			return false
		}
	}
	return true
}

func validHeaderValue(value string) bool {
	for index := 0; index < len(value); index++ {
		char := value[index]
		if char == '\r' || char == '\n' || char == 0 || char == 0x7f {
			return false
		}
		if char < 0x20 && char != '\t' {
			return false
		}
	}
	return len(value) <= 8<<10
}

func responseHeaders(header textproto.MIMEHeader) []Header {
	names := make([]string, 0, len(header))
	for name := range header {
		names = append(names, name)
	}
	sort.Strings(names)
	rows := make([]Header, 0, len(names))
	total := 0
	for _, name := range names {
		for _, value := range header[name] {
			if len(rows) >= MaxHeaders {
				return rows
			}
			total += len(name) + len(value)
			if total > MaxHeaderBytes {
				return rows
			}
			rows = append(rows, Header{Name: name, Value: value, Enabled: true})
		}
	}
	return rows
}

// trimUTF8 cuts a string to whole characters so a capped frame never shows
// mojibake.
func trimUTF8(value string) string {
	for len(value) > 0 && !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return strings.ToValidUTF8(value, "�")
}

func sizeNote(bytes int) string {
	switch {
	case bytes >= 1<<20:
		return strconv.Itoa(bytes>>20) + " MiB"
	case bytes >= 1<<10:
		return strconv.Itoa(bytes>>10) + " KiB"
	default:
		return strconv.Itoa(bytes) + " B"
	}
}

// oneLine keeps an error sentence on one line; it carries nothing the page
// does not already know (no URL, header or message).
func oneLine(value string) string {
	value = strings.TrimSpace(strings.ReplaceAll(strings.ReplaceAll(value, "\r", " "), "\n", " "))
	if len(value) > 300 {
		value = value[:300] + "…"
	}
	return value
}
