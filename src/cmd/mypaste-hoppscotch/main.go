// Command mypaste-hoppscotch is the Hoppscotch workspace's runtime. The host
// starts it for one menu-page call, writes one invocation line, answers the
// storage calls it makes, and reads one response line (PLUGINS.md §4). The
// page owns the request builder and the workspace JSON; this binary owns the
// HTTP exchange and the one saved document, and nothing else.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"time"

	"github.com/mypaste/com.mypaste.hoppscotch/src/internal/api"
	"github.com/mypaste/com.mypaste.hoppscotch/src/internal/hook"
	"github.com/mypaste/com.mypaste.hoppscotch/src/internal/state"
	"github.com/mypaste/com.mypaste.hoppscotch/src/internal/ws"
)

// Version is the extension version written into the package manifest.
const Version = "1.2.1"

const (
	commandCall = "hoppscotch.call"
	storageKey  = "workspace.json"
)

// input is the text one invocation carries: the page's JSON request.
type input struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type invocation struct {
	ID        string `json:"id"`
	Method    string `json:"method"`
	CommandID string `json:"commandId"`
	Deadline  int64  `json:"deadlineUnixMs"`
	Input     *input `json:"input,omitempty"`
}

type failure struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type result struct {
	Output string `json:"output,omitempty"`
}

type response struct {
	ID     string   `json:"id"`
	OK     bool     `json:"ok"`
	Result *result  `json:"result,omitempty"`
	Error  *failure `json:"error,omitempty"`
}

// call is the page's JSON request.
type call struct {
	Action  string          `json:"action"`
	Request *api.Request    `json:"request,omitempty"`
	Session *ws.Request     `json:"session,omitempty"`
	State   json.RawMessage `json:"state,omitempty"`
}

// answer is the JSON the page reads back from the command's output.
type answer struct {
	OK       bool            `json:"ok"`
	Response *api.Response   `json:"response,omitempty"`
	Session  *ws.Session     `json:"session,omitempty"`
	State    json.RawMessage `json:"state,omitempty"`
	Error    *failure        `json:"error,omitempty"`
}

func main() {
	client := hook.NewClient(os.Stdin, os.Stdout)
	line, err := client.ReadRequestLine()
	if err != nil {
		os.Exit(1)
	}
	var requested invocation
	if err := json.Unmarshal(line, &requested); err != nil || requested.ID == "" {
		os.Exit(1)
	}
	ctx, cancel := deadline(requested.Deadline)
	defer cancel()
	output, runErr := run(ctx, client, requested)
	if runErr != nil {
		write(client, response{ID: requested.ID, OK: false,
			Error: &failure{Code: "runtime", Message: runErr.Error()}})
		return
	}
	write(client, response{ID: requested.ID, OK: true, Result: &result{Output: output}})
}

func run(ctx context.Context, client *hook.Client, requested invocation) (string, error) {
	// The host writes every menu-page invocation with method "invoke"; the
	// page's request arrives as pipe input, so its type is text/plain.
	if requested.Method != "invoke" || requested.CommandID != commandCall {
		return "", errors.New("这个扩展只提供 Hoppscotch 页面使用的一个操作")
	}
	if requested.Input == nil || requested.Input.Type != "text/plain" ||
		strings.TrimSpace(requested.Input.Text) == "" {
		return "", errors.New("请求内容为空")
	}
	var asked call
	if err := json.Unmarshal([]byte(requested.Input.Text), &asked); err != nil {
		return "", errors.New("请求内容无法解析")
	}
	switch asked.Action {
	case "send":
		if asked.Request == nil {
			return encode(answer{OK: false, Error: &failure{
				Code: "request", Message: "没有要发送的请求"}})
		}
		response, err := api.Do(ctx, *asked.Request)
		if err != nil {
			return encode(answer{OK: false, Error: failureOf(err)})
		}
		return encode(answer{OK: true, Response: response})
	case "ws":
		// One call is one whole session: connect, send, collect, close. The
		// host's command runtime has no live channel to stream frames on,
		// so the transcript comes back in this one answer.
		if asked.Session == nil {
			return encode(answer{OK: false, Error: &failure{
				Code: "request", Message: "没有要连接的会话"}})
		}
		session, err := ws.Dial(ctx, *asked.Session)
		if err != nil {
			return encode(answer{OK: false, Error: failureOf(err)})
		}
		return encode(answer{OK: true, Session: session})
	case "load":
		data, found, err := client.Read(ctx, storageKey)
		if err != nil {
			return encode(answer{OK: false, Error: &failure{
				Code: "storage", Message: "无法读取本机保存的工作区：" + err.Error()}})
		}
		if !found {
			return encode(answer{OK: true})
		}
		if err := state.Validate(data); err != nil {
			return encode(answer{OK: false, Error: &failure{
				Code:    "corrupt",
				Message: "已保存的工作区无法读取（" + err.Error() + "），本次从空白开始",
			}})
		}
		return encode(answer{OK: true, State: json.RawMessage(data)})
	case "save":
		if len(asked.State) == 0 {
			return encode(answer{OK: false, Error: &failure{
				Code: "storage", Message: "没有要保存的工作区"}})
		}
		if err := state.Validate(asked.State); err != nil {
			return encode(answer{OK: false, Error: &failure{
				Code: "storage", Message: "工作区无法保存：" + err.Error()}})
		}
		if err := client.Write(ctx, storageKey, asked.State); err != nil {
			return encode(answer{OK: false, Error: &failure{
				Code: "storage", Message: "无法写入本机存储：" + err.Error()}})
		}
		return encode(answer{OK: true})
	default:
		return "", errors.New("未知操作")
	}
}

// failureOf keeps a typed API error's code and sentence; anything else is a
// request failure the page can still show.
func failureOf(err error) *failure {
	var typed *api.Error
	if errors.As(err, &typed) {
		return &failure{Code: typed.Code, Message: typed.Message}
	}
	var socket *ws.Error
	if errors.As(err, &socket) {
		return &failure{Code: socket.Code, Message: socket.Message}
	}
	return &failure{Code: "request", Message: err.Error()}
}

func encode(value answer) (string, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return "", errors.New("无法编码结果")
	}
	return string(encoded), nil
}

func write(client *hook.Client, payload response) {
	encoded, err := json.Marshal(payload)
	if err != nil {
		os.Exit(1)
	}
	if err := client.WriteLine(encoded); err != nil {
		os.Exit(1)
	}
}

// deadline keeps two seconds of the host's budget for the final line. The
// host raises the budget for this command because the manifest declares it
// longRunning; without that, the ten-second default still applies.
func deadline(ms int64) (context.Context, context.CancelFunc) {
	if ms <= 0 {
		return context.WithTimeout(context.Background(), 2*time.Minute)
	}
	limit := time.Until(time.UnixMilli(ms)) - 2*time.Second
	if limit < time.Second {
		limit = time.Second
	}
	return context.WithTimeout(context.Background(), limit)
}
