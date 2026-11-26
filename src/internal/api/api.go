// Package api performs the one HTTP request the workspace page asks for.
//
// The page builds the request (variables resolved, auth header composed) and
// hands it over as JSON; this package owns the network. It accepts only
// http/https, bounds every input and every output, and never writes the URL,
// headers or body anywhere except into the answer line the page reads.
package api

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	// MaxResponseBytes is the most body text handed back to the page. The
	// host's invoke line is capped at 1 MiB and JSON escaping can double a
	// text body (" and \\ escapes, control characters when a body is text but
	// unusual), so this stays at a quarter of that; the header budget below
	// and the binary check keep the rest of the answer small.
	MaxResponseBytes = 256 << 10
	// MaxPreviewBytes is the most image body handed back as base64, so the
	// page's preview tab can draw it from a data URL. Base64 grows a body by
	// a third; this keeps the answer well inside the host's 1 MiB line.
	MaxPreviewBytes = 384 << 10
	// MaxRequestBytes bounds what the page may ask this package to send.
	MaxRequestBytes = 1 << 20
	// MaxFormBytes bounds a form body this package assembles and MaxUploadBytes
	// one file row in it. A file never crosses the page bridge, so these can be
	// larger than the text budget the page itself is held to.
	MaxFormBytes   = 32 << 20
	MaxUploadBytes = 32 << 20
	MaxFormFields  = 200
	// MaxHeaders, MaxHeaderBytes and MaxURLBytes keep one request a request.
	MaxHeaders     = 64
	MaxHeaderBytes = 64 << 10
	MaxURLBytes    = 8 << 10
	// MaxResponseHeaderBytes bounds the header half of the answer, which the
	// server — not the page — decides the size of.
	MaxResponseHeaderBytes = 32 << 10
	// DefaultTimeout and MaxTimeout bound the wait; the host's own deadline
	// still wins when it is shorter.
	DefaultTimeout = 30 * time.Second
	MaxTimeout     = 120 * time.Second
	maxRedirects   = 10
	maxHeaderRows  = 200
	userAgent      = "MyPaste-Hoppscotch/1.0"
)

// Header is one user-visible request or response header row.
type Header struct {
	Name    string `json:"name"`
	Value   string `json:"value"`
	Enabled bool   `json:"enabled"`
}

// Field is one row of a form body the runtime assembles. A text row carries
// its value; a file row carries the path the page typed, because the page has
// no way to read the user's disk and this process does.
type Field struct {
	Name  string `json:"name"`
	Value string `json:"value,omitempty"`
	Kind  string `json:"kind,omitempty"`
}

// Form is a form body: the page hands over rows instead of bytes so a file
// never has to cross the 256 KiB text bridge.
type Form struct {
	ContentType string  `json:"contentType"`
	Fields      []Field `json:"fields"`
}

// Request is the page's JSON request body.
type Request struct {
	Method    string   `json:"method"`
	URL       string   `json:"url"`
	Headers   []Header `json:"headers,omitempty"`
	Body      string   `json:"body,omitempty"`
	Form      *Form    `json:"form,omitempty"`
	TimeoutMs int      `json:"timeoutMs,omitempty"`
}

// Response is what the page renders: status, timing, headers and body text.
type Response struct {
	Status     int      `json:"status"`
	StatusText string   `json:"statusText"`
	Protocol   string   `json:"protocol"`
	URL        string   `json:"url"`
	DurationMs int64    `json:"durationMs"`
	SizeBytes  int64    `json:"sizeBytes"`
	Headers    []Header `json:"headers"`
	Body       string   `json:"body"`
	BodyBytes  int64    `json:"bodyBytes"`
	// BodyBase64 carries an image body so the page's preview can draw it from
	// a data URL. It is empty for text bodies and for images over the preview
	// budget; the page never gets bytes for anything else.
	BodyBase64  string `json:"bodyBase64,omitempty"`
	Truncated   bool   `json:"truncated"`
	ContentType string `json:"contentType"`
	Binary      bool   `json:"binary"`
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

// Do performs one request and reads at most MaxResponseBytes of its body.
func Do(ctx context.Context, req Request) (*Response, error) {
	method, err := methodOf(req.Method)
	if err != nil {
		return nil, err
	}
	target, err := targetURL(req.URL)
	if err != nil {
		return nil, err
	}
	if len(req.Body) > MaxRequestBytes {
		return nil, failf("request", "请求体超过 %s", sizeNote(MaxRequestBytes))
	}
	if req.Form != nil && req.Body != "" {
		return nil, failf("request", "请求体不能同时是文本和表单")
	}
	headers, err := requestHeaders(req.Headers)
	if err != nil {
		return nil, err
	}
	timeout := requestTimeout(req.TimeoutMs)
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var body io.Reader
	if req.Form != nil {
		data, formType, err := req.Form.build()
		if err != nil {
			return nil, err
		}
		body = bytes.NewReader(data)
		if headers.Get("Content-Type") == "" {
			headers.Set("Content-Type", formType)
		}
	} else if req.Body != "" {
		body = strings.NewReader(req.Body)
	}
	httpReq, err := http.NewRequestWithContext(ctx, method, target.String(), body)
	if err != nil {
		return nil, failf("request", "无法构造请求：%s", oneLine(err.Error()))
	}
	httpReq.Header = headers
	if httpReq.Header.Get("User-Agent") == "" {
		httpReq.Header.Set("User-Agent", userAgent)
	}
	// Go takes the wire's Host from the URL and never from the header map, so
	// a Host row would be dropped without a word. It is how one address is
	// asked for a named site (curl's --header "Host: …"), so it becomes the
	// request's own Host instead.
	if host := httpReq.Header.Get("Host"); host != "" {
		if !validHostOverride(host) {
			return nil, failf("request", "Host 的值必须是主机名，可带端口")
		}
		httpReq.Host = host
		httpReq.Header.Del("Host")
	}

	client := &http.Client{
		Transport: http.DefaultTransport,
		CheckRedirect: func(redirect *http.Request, via []*http.Request) error {
			if len(via) >= maxRedirects {
				return errors.New("重定向次数过多")
			}
			return nil
		},
	}
	started := time.Now()
	resp, err := client.Do(httpReq)
	if err != nil {
		return nil, transportError(ctx, err, timeout)
	}
	defer resp.Body.Close()

	contentType := mediaType(resp.Header.Get("Content-Type"))
	// An image body is read under the preview budget so the page can draw it;
	// everything else keeps the text budget.
	limit := MaxResponseBytes
	if isImage(contentType) {
		limit = MaxPreviewBytes
	}
	data, readErr := io.ReadAll(io.LimitReader(resp.Body, int64(limit)+1))
	duration := time.Since(started)
	truncated := len(data) > limit
	if truncated {
		data = data[:limit]
	}
	if readErr != nil && len(data) == 0 {
		return nil, failf("request", "读取响应失败：%s", oneLine(readErr.Error()))
	}
	if readErr != nil {
		truncated = true
	}

	binary := isBinary(data, contentType)
	text := ""
	preview := ""
	if !binary {
		text = strings.ToValidUTF8(string(data), "�")
	} else if isImage(contentType) && !truncated {
		preview = base64.StdEncoding.EncodeToString(data)
	}
	size := int64(len(data))
	if resp.ContentLength > size {
		size = resp.ContentLength
	}
	return &Response{
		Status:      resp.StatusCode,
		StatusText:  statusText(resp),
		Protocol:    resp.Proto,
		URL:         resp.Request.URL.String(),
		DurationMs:  duration.Milliseconds(),
		SizeBytes:   size,
		Headers:     responseHeaders(resp.Header),
		Body:        text,
		BodyBytes:   int64(len(data)),
		BodyBase64:  preview,
		Truncated:   truncated,
		ContentType: contentType,
		Binary:      binary,
	}, nil
}

// methodOf accepts an HTTP token so a local API's custom verb still works,
// and refuses anything that is not one.
func methodOf(raw string) (string, error) {
	method := strings.ToUpper(strings.TrimSpace(raw))
	if method == "" || len(method) > 16 {
		return "", failf("request", "请求方法无效")
	}
	for _, char := range method {
		if char < 'A' || char > 'Z' {
			return "", failf("request", "请求方法只能包含英文字母")
		}
	}
	return method, nil
}

// targetURL is the one scheme gate: this is an HTTP client, not a file or
// data reader.
func targetURL(raw string) (*url.URL, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, failf("request", "先填写请求地址")
	}
	if len(raw) > MaxURLBytes {
		return nil, failf("request", "地址超过 %s", sizeNote(MaxURLBytes))
	}
	target, err := url.Parse(raw)
	if err != nil || target.Host == "" {
		return nil, failf("request", "地址无法解析；请写成 https://主机/路径")
	}
	if target.Scheme != "http" && target.Scheme != "https" {
		return nil, failf("request", "地址只能使用 http 或 https")
	}
	return target, nil
}

// validHostOverride keeps a Host row to what a host can be: a name or an
// address, optionally with a port. No whitespace, no path, no user info.
func validHostOverride(value string) bool {
	if value == "" || len(value) > 260 {
		return false
	}
	for index := 0; index < len(value); index++ {
		char := value[index]
		if char <= 0x20 || char == 0x7f || char == '/' || char == '\\' || char == '@' {
			return false
		}
	}
	return true
}

func requestHeaders(rows []Header) (http.Header, error) {
	headers := make(http.Header)
	total := 0
	count := 0
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
		if !validHeaderValue(row.Value) {
			return nil, failf("request", "请求头 %s 的值包含换行或控制字符", oneLine(name))
		}
		count++
		if count > MaxHeaders {
			return nil, failf("request", "请求头最多 %d 个", MaxHeaders)
		}
		total += len(name) + len(row.Value)
		if total > MaxHeaderBytes {
			return nil, failf("request", "请求头总大小超过 %s", sizeNote(MaxHeaderBytes))
		}
		headers.Add(name, row.Value)
	}
	return headers, nil
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

func transportError(ctx context.Context, err error, timeout time.Duration) error {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) ||
		errors.Is(err, context.DeadlineExceeded) {
		return failf("timeout", "请求超时（%s）", timeout.Round(time.Second))
	}
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		err = urlErr.Err
	}
	if errors.Is(err, context.Canceled) {
		return failf("request", "请求已取消")
	}
	return failf("network", "请求失败：%s", oneLine(err.Error()))
}

func statusText(resp *http.Response) string {
	if text := http.StatusText(resp.StatusCode); text != "" {
		return text
	}
	return strings.TrimSpace(strings.TrimPrefix(resp.Status, fmt.Sprint(resp.StatusCode)))
}

func mediaType(raw string) string {
	if raw == "" {
		return ""
	}
	parsed, _, err := mime.ParseMediaType(raw)
	if err != nil {
		return strings.ToLower(strings.TrimSpace(strings.Split(raw, ";")[0]))
	}
	return strings.ToLower(parsed)
}

// isImage reports the one binary kind the page draws rather than prints.
func isImage(contentType string) bool {
	return strings.HasPrefix(contentType, "image/")
}

// isBinary decides whether the body is text the page can show. Invalid UTF-8
// is never guessed at; a known binary media type skips the decode entirely.
func isBinary(data []byte, contentType string) bool {
	if !utf8.Valid(data) {
		return true
	}
	for _, prefix := range []string{"image/", "audio/", "video/", "font/"} {
		if strings.HasPrefix(contentType, prefix) {
			return true
		}
	}
	switch contentType {
	case "application/octet-stream", "application/pdf", "application/zip",
		"application/gzip", "application/x-gzip", "application/x-tar",
		"application/wasm", "application/vnd.ms-fontobject":
		return true
	}
	// Valid UTF-8 can still be machine bytes. A NUL is a file, not a
	// document; a body that is more than one percent control characters is
	// the same, and showing it would also expand the answer line several
	// times over when it is JSON-escaped. Tab, newline and return are text.
	controls := 0
	for _, b := range data {
		if b >= 0x20 || b == '\t' || b == '\n' || b == '\r' {
			continue
		}
		if b == 0 {
			return true
		}
		controls++
	}
	return controls > len(data)/100
}

func responseHeaders(header http.Header) []Header {
	names := make([]string, 0, len(header))
	for name := range header {
		names = append(names, name)
	}
	sort.Strings(names)
	rows := make([]Header, 0, len(names))
	used := 0
	for _, name := range names {
		for _, value := range header[name] {
			if len(rows) >= maxHeaderRows {
				return rows
			}
			cost := len(name) + len(value)
			if used+cost > MaxResponseHeaderBytes {
				// The server decides how many headers arrive; the page sees
				// the part that fits, cut at a character boundary.
				room := MaxResponseHeaderBytes - used - len(name)
				if room > 0 {
					cut := value[:room]
					for len(cut) > 0 && !utf8.ValidString(cut) {
						cut = cut[:len(cut)-1]
					}
					if cut != "" {
						rows = append(rows, Header{Name: name, Value: cut, Enabled: true})
					}
				}
				return rows
			}
			used += cost
			rows = append(rows, Header{Name: name, Value: value, Enabled: true})
		}
	}
	return rows
}

func sizeNote(bytes int) string {
	switch {
	case bytes >= 1<<20:
		return fmt.Sprintf("%d MiB", bytes>>20)
	case bytes >= 1<<10:
		return fmt.Sprintf("%d KiB", bytes>>10)
	default:
		return fmt.Sprintf("%d B", bytes)
	}
}

// build writes the form's bytes and returns the Content-Type that describes
// them. It is the only writer of a form body: the page hands over rows, so a
// text field and a file part are encoded in one place.
func (f *Form) build() ([]byte, string, error) {
	if len(f.Fields) > MaxFormFields {
		return nil, "", failf("request", "表单最多 %d 个字段", MaxFormFields)
	}
	fields := make([]Field, 0, len(f.Fields))
	for _, field := range f.Fields {
		if strings.TrimSpace(field.Name) != "" {
			fields = append(fields, field)
		}
	}
	switch f.ContentType {
	case "application/x-www-form-urlencoded":
		var buf bytes.Buffer
		for _, field := range fields {
			if field.Kind == "file" {
				return nil, "", failf("request", "表单编码没有文件字段，请改用 multipart/form-data")
			}
			if buf.Len() > 0 {
				buf.WriteByte('&')
			}
			buf.WriteString(url.QueryEscape(field.Name))
			buf.WriteByte('=')
			buf.WriteString(url.QueryEscape(field.Value))
			if buf.Len() > MaxRequestBytes {
				return nil, "", failf("request", "表单超过 %s", sizeNote(MaxRequestBytes))
			}
		}
		return buf.Bytes(), f.ContentType, nil
	case "multipart/form-data":
		var buf bytes.Buffer
		writer := multipart.NewWriter(&buf)
		for _, field := range fields {
			if field.Kind != "file" {
				if err := writer.WriteField(field.Name, field.Value); err != nil {
					return nil, "", failf("request", "无法组装表单：%s", oneLine(err.Error()))
				}
			} else if err := writeFilePart(writer, field, buf.Len()); err != nil {
				return nil, "", err
			}
			if buf.Len() > MaxFormBytes {
				return nil, "", failf("upload", "表单超过 %s", sizeNote(MaxFormBytes))
			}
		}
		if err := writer.Close(); err != nil {
			return nil, "", failf("request", "无法组装表单：%s", oneLine(err.Error()))
		}
		return buf.Bytes(), writer.FormDataContentType(), nil
	default:
		return nil, "", failf("request", "不支持的正文类型 %s", oneLine(f.ContentType))
	}
}

// writeFilePart appends one file row: the path is resolved here, never in the
// page, and the part keeps the file's own name.
func writeFilePart(writer *multipart.Writer, field Field, written int) error {
	path, err := uploadPath(field.Value)
	if err != nil {
		return err
	}
	name := filepath.Base(path)
	file, err := os.Open(path)
	if err != nil {
		return failf("upload", "打不开文件 %s", name)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return failf("upload", "%s 不是普通文件", name)
	}
	if info.Size() > MaxUploadBytes {
		return failf("upload", "%s 超过 %s", name, sizeNote(MaxUploadBytes))
	}
	if int64(written)+info.Size() > MaxFormBytes {
		return failf("upload", "表单超过 %s", sizeNote(MaxFormBytes))
	}
	part, err := writer.CreateFormFile(field.Name, name)
	if err != nil {
		return failf("request", "无法组装表单：%s", oneLine(err.Error()))
	}
	if _, err := io.Copy(part, file); err != nil {
		return failf("upload", "读取 %s 失败", name)
	}
	return nil
}

// uploadPath resolves the path a file row names. It must be absolute, or start
// with ~ for the user's home; relative paths would depend on the host's cwd.
func uploadPath(raw string) (string, error) {
	path := strings.TrimSpace(raw)
	if path == "" {
		return "", failf("upload", "文件行没有填写路径")
	}
	if path == "~" || strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", failf("upload", "找不到用户目录")
		}
		path = filepath.Join(home, strings.TrimPrefix(path, "~/"))
	}
	if !filepath.IsAbs(path) {
		return "", failf("upload", "文件路径需要以 / 或 ~/ 开头")
	}
	return filepath.Clean(path), nil
}

// oneLine keeps an error readable inside one JSON field and one status line.
func oneLine(text string) string {
	text = strings.ReplaceAll(text, "\r", " ")
	text = strings.ReplaceAll(text, "\n", " ")
	text = strings.TrimSpace(text)
	if len(text) > 300 {
		text = text[:300] + "…"
	}
	return text
}
