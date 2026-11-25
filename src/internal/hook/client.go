// Package hook is this extension's side of the host's line protocol
// (PLUGINS.md §4). The workspace needs exactly one host service — its own
// private storage, where collections, history and environments live — so this
// client owns one request line in, one answer line out, and the host calls in
// between. It is the package's own copy of the conversation; packages share
// no code.
package hook

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"strconv"
)

// Client is one invocation's conversation with the host. Calls are serial:
// the protocol never has two host calls in flight.
type Client struct {
	input  *bufio.Reader
	output *bufio.Writer
	calls  int
}

// NewClient wraps the invocation's streams.
func NewClient(input io.Reader, output io.Writer) *Client {
	return &Client{
		input:  bufio.NewReaderSize(input, 1<<20),
		output: bufio.NewWriterSize(output, 1<<20),
	}
}

// ReadRequestLine reads the invocation's one request line. It must use this
// reader: the buffered bytes belong to the same host conversation.
func (c *Client) ReadRequestLine() ([]byte, error) {
	line, err := c.input.ReadBytes('\n')
	if err != nil && len(line) == 0 {
		return nil, errors.New("没有收到宿主请求")
	}
	return line, nil
}

// WriteLine writes one line and flushes it.
func (c *Client) WriteLine(line []byte) error {
	if _, err := c.output.Write(append(line, '\n')); err != nil {
		return errors.New("无法写入宿主响应")
	}
	return c.output.Flush()
}

type hostCall struct {
	Type    string          `json:"type"`
	ID      string          `json:"id"`
	Service string          `json:"service"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type hostCallResult struct {
	Type   string          `json:"type"`
	ID     string          `json:"id"`
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  string          `json:"error,omitempty"`
}

// Call performs one host call and returns its raw result.
func (c *Client) Call(ctx context.Context, service, method string, params any) (json.RawMessage, error) {
	if err := ctx.Err(); err != nil {
		return nil, errors.New("操作已超时")
	}
	encoded, err := json.Marshal(params)
	if err != nil {
		return nil, errors.New("无法编码宿主调用")
	}
	c.calls++
	call := hostCall{Type: "hostCall", ID: "h" + strconv.Itoa(c.calls),
		Service: service, Method: method, Params: encoded}
	line, err := json.Marshal(call)
	if err != nil {
		return nil, errors.New("无法编码宿主调用")
	}
	if _, err := c.output.Write(append(line, '\n')); err != nil {
		return nil, errors.New("无法写入宿主调用")
	}
	if err := c.output.Flush(); err != nil {
		return nil, errors.New("无法写入宿主调用")
	}
	for {
		response, err := c.input.ReadBytes('\n')
		if err != nil {
			return nil, errors.New("宿主调用没有响应")
		}
		var result hostCallResult
		if err := json.Unmarshal(response, &result); err != nil {
			return nil, errors.New("宿主调用响应无法读取")
		}
		if result.Type != "hostCallResult" {
			return nil, errors.New("宿主返回了意外的消息")
		}
		if result.ID != call.ID {
			continue
		}
		if !result.OK {
			if result.Error == "" {
				result.Error = "宿主调用失败"
			}
			return nil, errors.New(result.Error)
		}
		return result.Result, nil
	}
}

// ErrStorageMissing is the host's answer for an absent key, matched exactly.
var ErrStorageMissing = errors.New("存储项不存在")

// StorageFile is one stored item.
type StorageFile struct {
	Key  string `json:"key"`
	Data string `json:"data"`
	Size int64  `json:"size"`
}

// Read returns nil, false for an absent key.
func (c *Client) Read(ctx context.Context, key string) ([]byte, bool, error) {
	raw, err := c.Call(ctx, "storage.plugin", "read", map[string]string{"key": key})
	if err != nil {
		if err.Error() == ErrStorageMissing.Error() {
			return nil, false, nil
		}
		return nil, false, err
	}
	var file StorageFile
	if err := json.Unmarshal(raw, &file); err != nil {
		return nil, false, errors.New("宿主返回的存储内容无法读取")
	}
	data, err := base64.StdEncoding.DecodeString(file.Data)
	if err != nil {
		return nil, false, errors.New("宿主返回的存储内容无法解码")
	}
	return data, true, nil
}

// Write stores one item.
func (c *Client) Write(ctx context.Context, key string, data []byte) error {
	_, err := c.Call(ctx, "storage.plugin", "write", map[string]string{
		"key":  key,
		"data": base64.StdEncoding.EncodeToString(data),
	})
	return err
}
