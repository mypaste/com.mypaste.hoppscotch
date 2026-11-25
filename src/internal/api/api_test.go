package api

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestDoSendsMethodHeadersAndBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		answer, _ := json.Marshal(map[string]string{
			"method": r.Method,
			"token":  r.Header.Get("X-Test"),
			"body":   string(body),
		})
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(answer)
	}))
	defer server.Close()

	response, err := Do(context.Background(), Request{
		Method: "post",
		URL:    server.URL + "/echo",
		Headers: []Header{
			{Name: "X-Test", Value: "yes", Enabled: true},
			{Name: "X-Off", Value: "no", Enabled: false},
		},
		Body: `{"hello":"world"}`,
	})
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if response.Status != http.StatusOK || response.StatusText != "OK" {
		t.Fatalf("status = %d %q", response.Status, response.StatusText)
	}
	var echoed map[string]string
	if err := json.Unmarshal([]byte(response.Body), &echoed); err != nil {
		t.Fatalf("body: %v (%q)", err, response.Body)
	}
	if echoed["method"] != "POST" || echoed["token"] != "yes" || echoed["body"] != `{"hello":"world"}` {
		t.Fatalf("echoed = %#v", echoed)
	}
	if response.ContentType != "application/json" || response.Binary {
		t.Fatalf("content type = %q binary=%v", response.ContentType, response.Binary)
	}
	if response.DurationMs < 0 || response.SizeBytes == 0 {
		t.Fatalf("timing/size = %d/%d", response.DurationMs, response.SizeBytes)
	}
}

func TestDoTruncatesALargeBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := []byte(strings.Repeat("a", MaxResponseBytes+128))
		w.Header().Set("Content-Type", "text/plain")
		w.Header().Set("Content-Length", fmt.Sprint(len(body)))
		_, _ = w.Write(body)
	}))
	defer server.Close()

	response, err := Do(context.Background(), Request{Method: "GET", URL: server.URL})
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if !response.Truncated || len(response.Body) != MaxResponseBytes {
		t.Fatalf("truncated=%v len=%d", response.Truncated, len(response.Body))
	}
	if response.SizeBytes <= int64(MaxResponseBytes) {
		t.Fatalf("size = %d", response.SizeBytes)
	}
}

func TestDoMarksBinaryBodies(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write([]byte{0xff, 0x00, 0x01})
	}))
	defer server.Close()

	response, err := Do(context.Background(), Request{Method: "GET", URL: server.URL})
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if !response.Binary || response.Body != "" {
		t.Fatalf("binary=%v body=%q", response.Binary, response.Body)
	}
}

func TestDoCarriesAnImagePreview(t *testing.T) {
	pixels := []byte{0x89, 'P', 'N', 'G', 0x00, 0xff, 0x10, 0x20}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(pixels)
	}))
	defer server.Close()

	response, err := Do(context.Background(), Request{Method: "GET", URL: server.URL})
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if !response.Binary || response.Body != "" {
		t.Fatalf("binary=%v body=%q", response.Binary, response.Body)
	}
	decoded, err := base64.StdEncoding.DecodeString(response.BodyBase64)
	if err != nil {
		t.Fatalf("BodyBase64 is not base64: %v", err)
	}
	if string(decoded) != string(pixels) {
		t.Fatalf("preview %x, want %x", decoded, pixels)
	}
}

func TestDoCapsAnOversizeImagePreview(t *testing.T) {
	bytes := make([]byte, MaxPreviewBytes+16)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(bytes)
	}))
	defer server.Close()

	response, err := Do(context.Background(), Request{Method: "GET", URL: server.URL})
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if response.BodyBase64 != "" {
		t.Fatalf("preview of %d bytes came back", len(response.BodyBase64))
	}
	if !response.Truncated {
		t.Fatal("an oversized image is not marked truncated")
	}
}

func TestDoRefusesWhatIsNotHTTP(t *testing.T) {
	for _, raw := range []string{
		"", "file:///etc/hosts", "ftp://example.com/x", "mailto:a@b",
		"not a url",
	} {
		if _, err := Do(context.Background(), Request{Method: "GET", URL: raw}); err == nil {
			t.Fatalf("accepted %q", raw)
		}
	}
}

func TestDoRefusesBadMethodsAndHeaders(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer server.Close()
	cases := []Request{
		{Method: "GE T", URL: server.URL},
		{Method: "", URL: server.URL},
		{Method: "GET", URL: server.URL, Headers: []Header{
			{Name: "Bad Header", Value: "x", Enabled: true}}},
		{Method: "GET", URL: server.URL, Headers: []Header{
			{Name: "X-Test", Value: "a\r\nb", Enabled: true}}},
	}
	for index, request := range cases {
		if _, err := Do(context.Background(), request); err == nil {
			t.Fatalf("case %d accepted", index)
		}
	}
}

func TestDoHonoursTimeout(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(1500 * time.Millisecond)
	}))
	defer server.Close()

	_, err := Do(context.Background(), Request{
		Method: "GET", URL: server.URL, TimeoutMs: 1000,
	})
	if err == nil {
		t.Fatal("timeout was not reported")
	}
	var typed *Error
	if !errors.As(err, &typed) || typed.Code != "timeout" {
		t.Fatalf("err = %#v", err)
	}
}

func TestMethodOfKeepsACustomVerb(t *testing.T) {
	method, err := methodOf("propfind")
	if err != nil || method != "PROPFIND" {
		t.Fatalf("method=%q err=%v", method, err)
	}
}

func TestDoMarksControlHeavyTextBinary(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Content-Length", fmt.Sprint(400))
		_, _ = w.Write(bytes.Repeat([]byte{0x01}, 400))
	}))
	defer server.Close()

	response, err := Do(context.Background(), Request{Method: "GET", URL: server.URL})
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if !response.Binary || response.Body != "" {
		t.Fatalf("binary=%v body=%q", response.Binary, response.Body)
	}
}

func TestDoBoundsResponseHeaders(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for index := 0; index < 100; index++ {
			w.Header().Add(fmt.Sprintf("X-Part-%03d", index), strings.Repeat("v", 1<<10))
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	response, err := Do(context.Background(), Request{Method: "GET", URL: server.URL})
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if len(response.Headers) == 0 || len(response.Headers) > maxHeaderRows {
		t.Fatalf("rows = %d", len(response.Headers))
	}
	total := 0
	for _, header := range response.Headers {
		total += len(header.Name) + len(header.Value)
	}
	if total > MaxResponseHeaderBytes {
		t.Fatalf("header bytes = %d, limit %d", total, MaxResponseHeaderBytes)
	}
}

// TestAnswerStaysInsideTheHostsLineCap pins the arithmetic the constants above
// describe: body and headers, each twice for JSON escaping, stay under the
// host's 1 MiB output line.
func TestAnswerStaysInsideTheHostsLineCap(t *testing.T) {
	lineCap := 1 << 20
	if 2*(MaxResponseBytes+MaxResponseHeaderBytes) >= lineCap {
		t.Fatalf("worst-case answer %d does not fit the %d line",
			2*(MaxResponseBytes+MaxResponseHeaderBytes), lineCap)
	}
	previewLine := base64.StdEncoding.EncodedLen(MaxPreviewBytes) + MaxResponseHeaderBytes
	if previewLine >= lineCap {
		t.Fatalf("worst-case image answer %d does not fit the %d line", previewLine, lineCap)
	}
}
