// Package transcript 把其他 agent 的会话记录（Codex 的 rollout、Claude Code 的会话 JSONL）还原成一串模型请求，
// 供观测台导入后只读查看。消息统一换成 OpenAI Chat 格式（system / user / assistant + tool_calls / tool），
// 和观测台代理记下的请求同一个形状，页面不用为每家写一套视图。
//
// 记录里只有对话历史，没有原始请求：每次请求的输入按“到这一步为止的历史”重建，耗时按记录的时间戳估算。
package transcript

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

type Message = map[string]any

// Call 是一次模型请求。New 只放相比上一次请求新增的消息，长会话才不会把整段历史存几百遍；
// Reset 表示历史被重建（压缩之后），这时 New 是完整的消息列表。
type Call struct {
	Start, End time.Time
	Compact    bool
	Reset      bool
	New        []Message
	Tools      []any // 工具定义，只在第一次请求或变化时给
	Model      string
	Reply      Message
	Usage      map[string]any
}

// Event 按时间顺序排列：一轮的开始（Prompt）、一次请求（Call）、一轮的结束（End："ok" 或 "cancelled"）。
type Event struct {
	Time   time.Time
	Prompt string
	Call   *Call
	End    string
}

type Conversation struct {
	Source string // codex 或 claude-code
	Dir    string // 会话当时的工作目录
	Events []Event
}

// Parse 按第一条能解析的记录判断来源：Codex 的每行是 {timestamp, type, payload}，Claude Code 的每行带 sessionId。
func Parse(data []byte) (*Conversation, error) {
	lines := splitLines(data)
	for _, line := range lines {
		var probe struct {
			Payload   json.RawMessage `json:"payload"`
			SessionID string          `json:"sessionId"`
		}
		if json.Unmarshal(line, &probe) != nil {
			continue
		}
		switch {
		case probe.Payload != nil:
			return parseCodex(lines)
		case probe.SessionID != "":
			return parseClaude(lines)
		}
	}
	return nil, errors.New("认不出这个文件：需要 Codex 的 rollout-*.jsonl 或 Claude Code 的会话 .jsonl")
}

func splitLines(data []byte) [][]byte {
	var lines [][]byte
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 1<<20), 64<<20)
	for scanner.Scan() {
		if line := bytes.TrimSpace(scanner.Bytes()); len(line) > 0 {
			lines = append(lines, bytes.Clone(line))
		}
	}
	return lines
}

// builder 维护到当前为止的对话历史，并在每次请求时切出新增的部分。
type builder struct {
	conv      Conversation
	history   []Message
	sent      int  // 上一次请求时历史的长度
	reset     bool // 历史被重建过，下一次请求要给完整列表
	tools     []any
	toolsSent bool
	last      time.Time
	model     string
	prompt    bool // 当前在一轮之中（已有 Prompt、还没有 End）
	calls     int  // 本轮已有的请求数
	at        int  // 本轮 Prompt 在 Events 里的位置
}

func newBuilder(source string) *builder {
	return &builder{conv: Conversation{Source: source}}
}

func (b *builder) add(m Message, at time.Time) {
	b.history = append(b.history, m)
	b.touch(at)
}

// touch 推进“最后发生的时间”，下一次请求从这里算起；只向后推（流式期间完成的工具结果可能早于回复结束）。
func (b *builder) touch(at time.Time) {
	if at.After(b.last) {
		b.last = at
	}
}

// begin 开始新的一轮；上一轮没有明确结束的，按正常结束收尾。
func (b *builder) begin(text string, at time.Time) {
	if b.prompt {
		b.end("ok", at)
	}
	b.conv.Events = append(b.conv.Events, Event{Time: at, Prompt: text})
	b.prompt, b.calls, b.at = true, 0, len(b.conv.Events)-1
	b.touch(at)
}

func (b *builder) end(outcome string, at time.Time) {
	if !b.prompt {
		return
	}
	// 没有请求模型的斜杠命令（/clear、/context 之类）只是客户端操作，不算一轮。
	if b.calls == 0 && strings.HasPrefix(b.conv.Events[b.at].Prompt, "/") {
		b.conv.Events, b.prompt = b.conv.Events[:b.at], false
		return
	}
	b.conv.Events = append(b.conv.Events, Event{Time: at, End: outcome})
	b.prompt = false
	b.touch(at)
}

// call 记一次请求：输入是当前历史，回复追加进历史（空回复没进过下一次请求，不追加）。
func (b *builder) call(reply Message, usage map[string]any, end time.Time, compact bool) {
	if !b.prompt {
		b.begin("（导入的对话）", b.last)
	}
	c := &Call{Start: b.last, End: end, Compact: compact, Reset: b.reset || b.sent == 0, Model: b.model, Reply: reply, Usage: usage}
	if c.Start.IsZero() || c.Start.After(end) {
		c.Start = end
	}
	from := b.sent
	if c.Reset {
		from = 0
	}
	c.New = b.history[from:len(b.history):len(b.history)]
	if !b.toolsSent && len(b.tools) > 0 {
		c.Tools, b.toolsSent = b.tools, true
	}
	b.conv.Events = append(b.conv.Events, Event{Time: c.Start, Call: c})
	b.sent, b.reset = len(b.history), false
	if !compact && (text(reply["content"]) != "" || reply["tool_calls"] != nil) {
		b.history = append(b.history, reply)
	}
	b.calls++
	b.touch(end)
}

// compact 记一次上下文压缩：输入是压缩前的历史，回复是摘要；之后的历史从摘要重新开始。
func (b *builder) compact(summary string, rebuilt []Message, at time.Time) {
	b.call(Message{"role": "assistant", "content": summary}, nil, at, true)
	if rebuilt == nil {
		rebuilt = []Message{{"role": "user", "content": summary}}
	}
	b.history, b.sent, b.reset = rebuilt, 0, true
}

func (b *builder) finish() *Conversation {
	b.end("ok", b.last)
	return &b.conv
}

// assistant 把一次回复的几段（正文、推理、工具调用）合成一条 assistant 消息。
type assistant struct {
	id        string
	content   []string
	reasoning []string
	calls     []any
	end       time.Time
	usage     map[string]any
}

func (a *assistant) message() Message {
	m := Message{"role": "assistant", "content": strings.Join(a.content, "\n\n")}
	if len(a.reasoning) > 0 {
		m["reasoning_content"] = strings.Join(a.reasoning, "\n\n")
	}
	if len(a.calls) > 0 {
		m["tool_calls"] = a.calls
	}
	return m
}

func (a *assistant) empty() bool {
	return len(a.content) == 0 && len(a.reasoning) == 0 && len(a.calls) == 0
}

func toolCall(id, name, arguments string) map[string]any {
	return map[string]any{"id": id, "type": "function", "function": map[string]any{"name": name, "arguments": arguments}}
}

// usage 换成 OpenAI 的字段：prompt_tokens 含缓存命中的部分。
func usage(input, cached, output, reasoning int) map[string]any {
	return map[string]any{"prompt_tokens": input, "completion_tokens": output, "total_tokens": input + output,
		"prompt_tokens_details": map[string]any{"cached_tokens": cached}, "completion_tokens_details": map[string]any{"reasoning_tokens": reasoning}}
}

// text 取消息内容里的文字：字符串原样返回，内容块数组拼接其中的文字，图片记成占位。
func text(v any) string {
	switch v := v.(type) {
	case string:
		return v
	case []any:
		var parts []string
		for _, item := range v {
			block, _ := item.(map[string]any)
			switch {
			case block == nil:
			case block["text"] != nil:
				parts = append(parts, text(block["text"]))
			case block["type"] == "image" || block["type"] == "input_image":
				parts = append(parts, "[图片]")
			case block["content"] != nil:
				parts = append(parts, text(block["content"]))
			}
		}
		return strings.Join(parts, "\n")
	case map[string]any:
		if v["content"] != nil {
			return text(v["content"])
		}
		if v["text"] != nil {
			return text(v["text"])
		}
	}
	return ""
}

func raw(v json.RawMessage) any {
	var out any
	json.Unmarshal(v, &out)
	return out
}
