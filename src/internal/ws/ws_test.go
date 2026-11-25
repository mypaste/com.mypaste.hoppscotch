package ws

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha1"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// --- an independent server side ---------------------------------------
//
// The tests speak the wire protocol themselves instead of reusing readFrame
// and writeFrame, so an encoder bug cannot cancel itself out on both sides.

type peer struct {
	t    *testing.T
	conn net.Conn
	rw   *bufio.ReadWriter
}

func newServer(t *testing.T, script func(p *peer)) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hijacker, ok := w.(http.Hijacker)
		if !ok {
			t.Error("test server cannot hijack")
			return
		}
		conn, rw, err := hijacker.Hijack()
		if err != nil {
			t.Errorf("hijack: %v", err)
			return
		}
		defer conn.Close()
		key := r.Header.Get("Sec-WebSocket-Key")
		sum := sha1.Sum([]byte(key + acceptGUID))
		fmt.Fprintf(rw, "HTTP/1.1 101 Switching Protocols\r\n"+
			"Upgrade: websocket\r\nConnection: Upgrade\r\n"+
			"Sec-WebSocket-Accept: %s\r\nX-Test: yes\r\n\r\n",
			base64.StdEncoding.EncodeToString(sum[:]))
		if err := rw.Flush(); err != nil {
			return
		}
		script(&peer{t: t, conn: conn, rw: rw})
	}))
	t.Cleanup(server.Close)
	return server
}

func (p *peer) read() (byte, []byte) {
	p.t.Helper()
	var head [2]byte
	if _, err := io.ReadFull(p.rw, head[:]); err != nil {
		p.t.Fatalf("peer read head: %v", err)
	}
	opcode := head[0] & 0x0F
	masked := head[1]&0x80 != 0
	length := uint64(head[1] & 0x7F)
	switch length {
	case 126:
		var ext [2]byte
		if _, err := io.ReadFull(p.rw, ext[:]); err != nil {
			p.t.Fatalf("peer read length: %v", err)
		}
		length = uint64(binary.BigEndian.Uint16(ext[:]))
	case 127:
		var ext [8]byte
		if _, err := io.ReadFull(p.rw, ext[:]); err != nil {
			p.t.Fatalf("peer read length: %v", err)
		}
		length = binary.BigEndian.Uint64(ext[:])
	}
	var mask [4]byte
	if masked {
		if _, err := io.ReadFull(p.rw, mask[:]); err != nil {
			p.t.Fatalf("peer read mask: %v", err)
		}
	}
	payload := make([]byte, length)
	if _, err := io.ReadFull(p.rw, payload); err != nil {
		p.t.Fatalf("peer read payload: %v", err)
	}
	if masked {
		for index := range payload {
			payload[index] ^= mask[index%4]
		}
	} else {
		p.t.Errorf("client frame was not masked")
	}
	return opcode, payload
}

func (p *peer) write(opcode byte, payload []byte) {
	p.t.Helper()
	head := []byte{0x80 | opcode}
	const maxHeader = 1 << 16
	switch length := len(payload); {
	case length < 126:
		head = append(head, byte(length))
	case length <= maxHeader:
		head = append(head, 126, 0, 0)
		binary.BigEndian.PutUint16(head[2:], uint16(length))
	default:
		head = append(head, 127, 0, 0, 0, 0, 0, 0, 0, 0)
		binary.BigEndian.PutUint64(head[2:], uint64(length))
	}
	head = append(head, payload...)
	if _, err := p.rw.Write(head); err != nil {
		p.t.Errorf("peer write: %v", err)
		return
	}
	if err := p.rw.Flush(); err != nil {
		p.t.Errorf("peer flush: %v", err)
	}
}

// --- vectors -----------------------------------------------------------

func TestAcceptForMatchesTheRFCExample(t *testing.T) {
	// RFC 6455 §1.3.
	if got := acceptFor("dGhlIHNhbXBsZSBub25jZQ=="); got != "s3pPLMBiTxaQ9kYGzzhZRbK+xOo=" {
		t.Fatalf("accept = %q", got)
	}
}

func TestWriteFrameMatchesTheRFCExample(t *testing.T) {
	// RFC 6455 §5.7: one masked text frame carrying "Hello".
	var out bytes.Buffer
	mask := [4]byte{0x37, 0xFA, 0x21, 0x3D}
	if err := writeFrame(&out, opText, []byte("Hello"), mask); err != nil {
		t.Fatalf("writeFrame: %v", err)
	}
	want := []byte{0x81, 0x85, 0x37, 0xFA, 0x21, 0x3D,
		0x7F, 0x9F, 0x4D, 0x51, 0x58}
	if !bytes.Equal(out.Bytes(), want) {
		t.Fatalf("frame = % x, want % x", out.Bytes(), want)
	}
}

// --- sessions ----------------------------------------------------------

func TestSessionSendsMessagesAndCollectsReplies(t *testing.T) {
	server := newServer(t, func(p *peer) {
		if op, payload := p.read(); op != opText || string(payload) != "hello" {
			t.Errorf("first message = %d %q", op, payload)
		}
		p.write(opText, []byte("world"))
		if op, payload := p.read(); op != opText || string(payload) != "bye" {
			t.Errorf("second message = %d %q", op, payload)
		}
		p.write(opText, []byte("again"))
		// Keep the connection open while the client's collection window runs.
		time.Sleep(300 * time.Millisecond)
	})
	session, err := Dial(context.Background(), Request{
		URL:      wsURL(server),
		Headers:  []Header{{Name: "X-Token", Value: "yes", Enabled: true}},
		Messages: []string{"hello", "bye"},
		HoldMs:   200,
	})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	if session.Status != 101 || session.StatusText != "Switching Protocols" {
		t.Fatalf("status = %d %q", session.Status, session.StatusText)
	}
	if session.Protocol != "ws" {
		t.Fatalf("protocol = %q", session.Protocol)
	}
	found := false
	for _, header := range session.Headers {
		if header.Name == "X-Test" && header.Value == "yes" {
			found = true
		}
	}
	if !found {
		t.Fatalf("handshake headers = %#v", session.Headers)
	}
	var texts []string
	for _, frame := range session.Frames {
		texts = append(texts, frame.Direction+":"+frame.Type+":"+frame.Text)
	}
	want := []string{
		"out:text:hello", "in:text:world",
		"out:text:bye", "in:text:again",
	}
	if strings.Join(texts, "|") != strings.Join(want, "|") {
		t.Fatalf("frames = %v", texts)
	}
	if session.BytesOut != int64(len("hello")+len("bye")) {
		t.Fatalf("bytes out = %d", session.BytesOut)
	}
	if session.BytesIn != int64(len("world")+len("again")) {
		t.Fatalf("bytes in = %d", session.BytesIn)
	}
	if session.Close != nil || session.Note != "" || session.Truncated {
		t.Fatalf("unexpected end: close=%v note=%q truncated=%v",
			session.Close, session.Note, session.Truncated)
	}
}

func TestSessionAnswersPingAndRecordsPong(t *testing.T) {
	server := newServer(t, func(p *peer) {
		p.read()
		p.write(opPing, []byte("beat"))
		if op, payload := p.read(); op != opPong || string(payload) != "beat" {
			t.Errorf("pong = %d %q", op, payload)
		}
		p.write(opPong, []byte("back"))
		p.write(opText, []byte("done"))
		time.Sleep(200 * time.Millisecond)
	})
	session, err := Dial(context.Background(), Request{
		URL:      wsURL(server),
		Messages: []string{"hi"},
		HoldMs:   150,
	})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	var kinds []string
	for _, frame := range session.Frames {
		kinds = append(kinds, frame.Direction+":"+frame.Type)
	}
	want := "out:text|in:ping|in:pong|in:text"
	if strings.Join(kinds, "|") != want {
		t.Fatalf("frames = %v", kinds)
	}
	if last := session.Frames[len(session.Frames)-1]; last.Text != "done" {
		t.Fatalf("last text = %q", last.Text)
	}
}

func TestSessionStopsWhenTheServerCloses(t *testing.T) {
	echoed := make(chan int, 1)
	server := newServer(t, func(p *peer) {
		p.read()
		payload := make([]byte, 2)
		binary.BigEndian.PutUint16(payload, 1000)
		p.write(opClose, append(payload, []byte("bye")...))
		op, answer := p.read()
		if op != opClose {
			t.Errorf("echo was opcode %d", op)
			return
		}
		code := int(binary.BigEndian.Uint16(answer[:2]))
		echoed <- code
	})
	session, err := Dial(context.Background(), Request{
		URL:      wsURL(server),
		Messages: []string{"hi"},
		HoldMs:   3000,
	})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	if session.Close == nil || session.Close.Code != 1000 || session.Close.Reason != "bye" {
		t.Fatalf("close = %#v", session.Close)
	}
	if session.DurationMs > 1000 {
		t.Fatalf("session kept listening after close: %d ms", session.DurationMs)
	}
	select {
	case code := <-echoed:
		if code != 1000 {
			t.Fatalf("echoed code = %d", code)
		}
	case <-time.After(time.Second):
		t.Fatal("client did not echo the close frame")
	}
}

func TestSessionReassemblesFragments(t *testing.T) {
	server := newServer(t, func(p *peer) {
		p.read()
		p.writeOptions(opText, []byte("Hel"), false)
		p.writeOptions(opContinuation, []byte("lo, "), false)
		p.writeOptions(opContinuation, []byte("world"), true)
		time.Sleep(200 * time.Millisecond)
	})
	session, err := Dial(context.Background(), Request{
		URL:      wsURL(server),
		Messages: []string{"hi"},
		HoldMs:   150,
	})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	if len(session.Frames) != 2 {
		t.Fatalf("frames = %#v", session.Frames)
	}
	if got := session.Frames[1].Text; got != "Hello, world" {
		t.Fatalf("reassembled = %q", got)
	}
}

func TestSessionCapsFrameTextButKeepsItsSize(t *testing.T) {
	big := strings.Repeat("a", MaxFrameText+512)
	server := newServer(t, func(p *peer) {
		p.read()
		p.write(opText, []byte(big))
		p.write(opBinary, []byte{0, 1, 2, 3})
		time.Sleep(200 * time.Millisecond)
	})
	session, err := Dial(context.Background(), Request{
		URL:      wsURL(server),
		Messages: []string{"hi"},
		HoldMs:   150,
	})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	if len(session.Frames) != 3 {
		t.Fatalf("frames = %d", len(session.Frames))
	}
	text := session.Frames[1]
	if !text.Truncated || len(text.Text) > MaxFrameText || text.SizeBytes != int64(len(big)) {
		t.Fatalf("text frame = %#v", text)
	}
	if !session.Truncated {
		t.Fatal("session did not mark the truncation")
	}
	if binary := session.Frames[2]; binary.Type != "binary" || binary.Text != "" {
		t.Fatalf("binary frame = %#v", binary)
	}
}

func TestSessionRefusesAMaskedServerFrame(t *testing.T) {
	server := newServer(t, func(p *peer) {
		p.read()
		// 0x81 with the mask bit set: servers must never mask (§5.1).
		_, _ = p.rw.Write([]byte{0x81, 0x80 | 1, 1, 2, 3, 4, 'h' ^ 1})
		_ = p.rw.Flush()
		time.Sleep(100 * time.Millisecond)
	})
	session, err := Dial(context.Background(), Request{
		URL:      wsURL(server),
		Messages: []string{"hi"},
		HoldMs:   150,
	})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	if session.Note == "" || !strings.Contains(session.Note, "掩码") {
		t.Fatalf("note = %q", session.Note)
	}
}

func TestSessionHoldsTheWindowWithoutReplies(t *testing.T) {
	server := newServer(t, func(p *peer) {
		p.read()
		time.Sleep(400 * time.Millisecond)
	})
	start := time.Now()
	session, err := Dial(context.Background(), Request{
		URL:      wsURL(server),
		Messages: []string{"hi"},
		HoldMs:   150,
	})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	elapsed := time.Since(start)
	if len(session.Frames) != 1 {
		t.Fatalf("frames = %#v", session.Frames)
	}
	if elapsed < 120*time.Millisecond {
		t.Fatalf("returned before the window: %s", elapsed)
	}
	if session.Note != "" || session.Close != nil {
		t.Fatalf("silent end should be clean: %q %v", session.Note, session.Close)
	}
}

func TestSessionUsesWSS(t *testing.T) {
	server := newServer(t, func(p *peer) {
		p.read()
		p.write(opText, []byte("secure"))
		time.Sleep(200 * time.Millisecond)
	})
	secure := httptest.NewTLSServer(server.Config.Handler)
	t.Cleanup(secure.Close)
	pool := x509.NewCertPool()
	pool.AddCert(secure.Certificate())
	previous := tlsConfigFor
	tlsConfigFor = func(host string) *tls.Config {
		return &tls.Config{ServerName: host, RootCAs: pool, MinVersion: tls.VersionTLS12}
	}
	t.Cleanup(func() { tlsConfigFor = previous })

	session, err := Dial(context.Background(), Request{
		URL:      "wss" + strings.TrimPrefix(secure.URL, "https") + "/socket",
		Messages: []string{"hello"},
		HoldMs:   150,
	})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	if session.Protocol != "wss" {
		t.Fatalf("protocol = %q", session.Protocol)
	}
	if len(session.Frames) != 2 || session.Frames[1].Text != "secure" {
		t.Fatalf("frames = %#v", session.Frames)
	}
}

// --- refusals ----------------------------------------------------------

func TestDialRefusesWhatIsNotAWebSocketSession(t *testing.T) {
	cases := []struct {
		name  string
		build func() Request
		code  string
	}{
		{"http address", func() Request {
			return Request{URL: "https://example.com/socket", Messages: []string{"x"}}
		}, "request"},
		{"no message", func() Request {
			return Request{URL: "ws://example.com/socket"}
		}, "request"},
		{"too many messages", func() Request {
			messages := make([]string, MaxMessages+1)
			for index := range messages {
				messages[index] = "x"
			}
			return Request{URL: "ws://example.com/socket", Messages: messages}
		}, "request"},
		{"a message too large", func() Request {
			return Request{URL: "ws://example.com/socket",
				Messages: []string{strings.Repeat("a", MaxMessageBytes+1)}}
		}, "request"},
		{"an account in the address", func() Request {
			return Request{URL: "ws://user:pass@example.com/socket", Messages: []string{"x"}}
		}, "request"},
		{"a reserved header", func() Request {
			return Request{URL: "ws://example.com/socket", Messages: []string{"x"},
				Headers: []Header{{Name: "Sec-WebSocket-Key", Value: "x", Enabled: true}}}
		}, "request"},
		{"a folded header", func() Request {
			return Request{URL: "ws://example.com/socket", Messages: []string{"x"},
				Headers: []Header{{Name: "X-Test", Value: "a\r\nB: c", Enabled: true}}}
		}, "request"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			_, err := Dial(context.Background(), test.build())
			var typed *Error
			if !errors.As(err, &typed) || typed.Code != test.code {
				t.Fatalf("err = %v", err)
			}
		})
	}
}

func TestDialReportsARefusedUpgrade(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Why", "nope")
		http.Error(w, "no sockets here", http.StatusForbidden)
	}))
	defer server.Close()
	_, err := Dial(context.Background(), Request{
		URL:      wsURL(server),
		Messages: []string{"hi"},
	})
	var typed *Error
	if !errors.As(err, &typed) || typed.Code != "handshake" {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(typed.Message, "403") {
		t.Fatalf("message = %q", typed.Message)
	}
}

func TestHoldAndTimeoutAreClamped(t *testing.T) {
	if holdWindow(0) != DefaultHoldMs*time.Millisecond {
		t.Fatalf("default hold = %s", holdWindow(0))
	}
	if holdWindow(1) != MinHoldMs*time.Millisecond {
		t.Fatalf("small hold = %s", holdWindow(1))
	}
	if holdWindow(10*60*1000) != MaxHoldMs*time.Millisecond {
		t.Fatalf("large hold = %s", holdWindow(10*60*1000))
	}
	if requestTimeout(0) != DefaultTimeout {
		t.Fatalf("default timeout = %s", requestTimeout(0))
	}
	if requestTimeout(10*60*1000) != MaxTimeout {
		t.Fatalf("large timeout = %s", requestTimeout(10*60*1000))
	}
}

func wsURL(server *httptest.Server) string {
	return "ws" + strings.TrimPrefix(server.URL, "http") + "/socket"
}

// writeOptions writes a frame with explicit fragmentation bits.
func (p *peer) writeOptions(opcode byte, payload []byte, final bool) {
	p.t.Helper()
	b0 := opcode
	if final {
		b0 |= 0x80
	}
	head := []byte{b0, byte(len(payload))}
	head = append(head, payload...)
	if _, err := p.rw.Write(head); err != nil {
		p.t.Errorf("peer write: %v", err)
		return
	}
	if err := p.rw.Flush(); err != nil {
		p.t.Errorf("peer flush: %v", err)
	}
}
