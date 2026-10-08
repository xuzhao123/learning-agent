package protocol

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
)

// B0：agent 与界面之间的结构化协议，参考 Codex app-server。
// 一行一条 JSON-RPC 2.0 消息，双向：
//
//	界面 → agent   请求  turn/start（问题、续聊上下文）、turn/interrupt
//	agent → 界面   通知  turn/started、item/started、item/completed、turn/completed 等
//	agent → 界面   请求  network/requestApproval：agent 暂停，等界面回复允许或拒绝再继续
//
// agent 以 -app-server 运行时，stdout 只承载协议，给人看的日志改走 stderr。
// 子 agent 也是这样被父 agent 启动的：父进程的 RunChild 就是它的“界面”。

type Message struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      *int64          `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *Error          `json:"error,omitempty"`
}

type Error struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *Error) Error() string { return e.Message }

// IsRequest：带 ID 和方法名的是请求，只带 ID 的是响应，没有 ID 的是通知。
func (m Message) IsRequest() bool { return m.ID != nil && m.Method != "" }

// Conn 是协议的一端。读到的响应交给等待它的 Call；请求和通知交给 handle（在读循环里依次调用，handle 不能阻塞）。
type Conn struct {
	mu      sync.Mutex
	w       io.Writer
	next    int64
	pending map[int64]chan Message
	closed  chan struct{}
}

func NewConn(r io.Reader, w io.Writer, handle func(Message)) *Conn {
	c := &Conn{w: w, pending: map[int64]chan Message{}, closed: make(chan struct{})}
	go func() {
		defer close(c.closed)
		scanner := bufio.NewScanner(r)
		scanner.Buffer(make([]byte, 64*1024), 16<<20)
		for scanner.Scan() {
			var m Message
			if json.Unmarshal(scanner.Bytes(), &m) != nil || m.JSONRPC != "2.0" {
				continue
			}
			if m.ID != nil && m.Method == "" {
				c.mu.Lock()
				ch := c.pending[*m.ID]
				delete(c.pending, *m.ID)
				c.mu.Unlock()
				if ch != nil {
					ch <- m
				}
				continue
			}
			handle(m)
		}
	}()
	return c
}

// Closed 在对端关闭（读到 EOF）后关闭。
func (c *Conn) Closed() <-chan struct{} { return c.closed }

func (c *Conn) write(m Message) error {
	m.JSONRPC = "2.0"
	data, err := json.Marshal(m)
	if err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	_, err = c.w.Write(append(data, '\n'))
	return err
}

func raw(v any) json.RawMessage {
	if v == nil {
		return nil
	}
	if r, ok := v.(json.RawMessage); ok {
		return r
	}
	data, _ := json.Marshal(v)
	return data
}

func (c *Conn) Notify(method string, params any) error {
	return c.write(Message{Method: method, Params: raw(params)})
}

func (c *Conn) Reply(id int64, result any, err *Error) error {
	if err != nil {
		return c.write(Message{ID: &id, Error: err})
	}
	if result == nil {
		result = struct{}{}
	}
	return c.write(Message{ID: &id, Result: raw(result)})
}

// Call 发出请求并等待响应；ctx 取消或对端关闭时返回错误。result 为 nil 时忽略返回值。
func (c *Conn) Call(ctx context.Context, method string, params any, result any) error {
	c.mu.Lock()
	c.next++
	id := c.next
	ch := make(chan Message, 1)
	c.pending[id] = ch
	c.mu.Unlock()
	if err := c.write(Message{ID: &id, Method: method, Params: raw(params)}); err != nil {
		return err
	}
	select {
	case m := <-ch:
		if m.Error != nil {
			return m.Error
		}
		if result != nil {
			return json.Unmarshal(m.Result, result)
		}
		return nil
	case <-ctx.Done():
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return ctx.Err()
	case <-c.closed:
		return errors.New("界面连接已关闭")
	}
}

// ---------- agent 一侧 ----------

// Client 是本进程的界面连接；为 nil 表示命令行直接运行，通知不发出，请求不可用。
var Client *Conn

// Notify 在没有界面连接时什么都不做：命令行运行照常只看终端日志。
func Notify(method string, params any) {
	if Client != nil {
		Client.Notify(method, params)
	}
}

// ErrNoClient：命令行直接运行或队列里的任务，没有可以回答请求的人。
var ErrNoClient = errors.New("没有连接界面，无法向用户发起请求")

func Call(ctx context.Context, method string, params any, result any) error {
	if Client == nil {
		return ErrNoClient
	}
	return Client.Call(ctx, method, params, result)
}

// Item 是一轮里的一个工作单元，有 started、completed 两个阶段（流式输出时再加 delta，见 B1）。
type Item struct {
	ID   string `json:"id"`
	Type string `json:"type"` // userMessage、modelCall、toolCall、compaction
	Data any    `json:"data,omitempty"`
}

func ItemStarted(turn string, item Item) {
	Notify("item/started", map[string]any{"turn_id": turn, "item": item})
}

func ItemCompleted(turn string, item Item) {
	Notify("item/completed", map[string]any{"turn_id": turn, "item": item})
}

// Event 记录一条给人看的日志，并在有界面时发出对应的结构化通知。
// 记忆、能力（skill、MCP）这类启动和收尾阶段的事件以前只打印文本，界面要用正则去解析；
// 现在文本照旧打印到日志，界面只读通知。
func Event(method, kind, text string, extra ...any) {
	fmt.Println(text)
	params := map[string]any{"kind": kind, "text": text}
	for i := 0; i+1 < len(extra); i += 2 {
		params[fmt.Sprint(extra[i])] = extra[i+1]
	}
	Notify(method, params)
}
