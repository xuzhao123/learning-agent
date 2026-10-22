package observer

import (
	"bytes"
	"encoding/json"
	"slices"
	"strings"
)

// O20：代理存档写增量。同一任务的主请求，上一次请求的消息原样是这一次的前缀时，只存新增的消息并标 append，
// 工具定义没变就不再存（沿用导入对话时页面已经认识的格式）；压缩、改写历史时前缀对不上，照旧存整份。
// 一段对话的存档原来随请求数平方增长（每次请求都带着全部历史），现在大致只随新增内容线性增长。
// 记忆辅助调用、独立验证这类辅助请求是另一个上下文，照旧整份存，也不参与接续。
// 读请求体的地方（续聊恢复、归因）用 requestBodies 还原成整份。

func auxPurpose(purpose string) bool {
	return strings.HasPrefix(purpose, "memory_") || purpose == "verify"
}

// chain 是一个任务最近一次主请求（整份）的消息和工具定义。
type chain struct {
	messages []json.RawMessage
	tools    json.RawMessage
}

// compactRequest 在写进存档之前把请求体换成增量。解析不了就原样存。
func (r *run) compactRequest(task, purpose string, body []byte) []byte {
	if auxPurpose(purpose) {
		return body
	}
	var fields map[string]json.RawMessage
	var messages []json.RawMessage
	if json.Unmarshal(body, &fields) != nil || json.Unmarshal(fields["messages"], &messages) != nil || len(messages) == 0 {
		return body
	}
	r.mu.Lock()
	if r.chains == nil {
		r.chains = map[string]*chain{}
	}
	prev := r.chains[task]
	r.chains[task] = &chain{messages, fields["tools"]}
	r.mu.Unlock()
	if prev == nil || len(prev.messages) > len(messages) {
		return body
	}
	for i := range prev.messages {
		if !bytes.Equal(prev.messages[i], messages[i]) {
			return body
		}
	}
	fields["messages"], _ = json.Marshal(messages[len(prev.messages):])
	fields["append"] = json.RawMessage("true")
	if bytes.Equal(fields["tools"], prev.tools) {
		delete(fields, "tools")
	}
	out, err := json.Marshal(fields)
	if err != nil {
		return body
	}
	return out
}

// requestBodies 把每个请求事件还原成整份请求体（与 events 一一对应，非请求事件为 nil）：
// 带 append 的接在同一任务上一次主请求后面，工具定义缺省时沿用上一次的。旧存档没有 append，原样返回。
func requestBodies(events []event) []json.RawMessage {
	out := make([]json.RawMessage, len(events))
	last := map[string]*chain{}
	for i, e := range events {
		if e.Kind != "request" {
			continue
		}
		out[i] = e.Body
		if auxPurpose(e.Purpose) {
			continue
		}
		var fields map[string]json.RawMessage
		var messages []json.RawMessage
		if json.Unmarshal(e.Body, &fields) != nil {
			continue
		}
		json.Unmarshal(fields["messages"], &messages)
		if string(fields["append"]) == "true" {
			if prev := last[e.Task]; prev != nil {
				messages = append(slices.Clone(prev.messages), messages...)
				if _, ok := fields["tools"]; !ok && prev.tools != nil {
					fields["tools"] = prev.tools
				}
			}
			delete(fields, "append")
			fields["messages"], _ = json.Marshal(messages)
			out[i], _ = json.Marshal(fields)
		}
		last[e.Task] = &chain{messages, fields["tools"]}
	}
	return out
}
