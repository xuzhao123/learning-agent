package transcript

import (
	"encoding/json"
	"strings"
	"time"
)

// Codex 的 rollout 每行 {timestamp, type, payload}：
//   - response_item：发给模型或模型产出的条目（developer/user/assistant 消息、reasoning、各种工具调用与输出、additional_tools）
//   - token_usage_record：一次模型响应结束，带这次的用量；以它为界切分请求
//   - event_msg：界面事件，用到 UserMessage（用户原话，标出一轮的开始）、task_complete、turn_aborted
//   - turn_context：本轮的模型、目录与权限；compacted：压缩后的替换历史
func parseCodex(lines [][]byte) (*Conversation, error) {
	b := newBuilder("codex")
	var open *assistant
	lastPrompt := ""
	flush := func(u map[string]any, at time.Time) {
		if open == nil && u == nil {
			return
		}
		if open == nil {
			open = &assistant{}
		}
		if at.IsZero() {
			at = open.end
		}
		b.call(open.message(), u, at, false)
		open = nil
	}
	for _, line := range lines {
		var l struct {
			Timestamp time.Time       `json:"timestamp"`
			Type      string          `json:"type"`
			Payload   json.RawMessage `json:"payload"`
		}
		if json.Unmarshal(line, &l) != nil {
			continue
		}
		var p struct {
			Type      string          `json:"type"`
			Role      string          `json:"role"`
			Name      string          `json:"name"`
			CallID    string          `json:"call_id"`
			Arguments string          `json:"arguments"`
			Input     string          `json:"input"`
			Content   json.RawMessage `json:"content"`
			Output    json.RawMessage `json:"output"`
			Summary   json.RawMessage `json:"summary"`
			Action    json.RawMessage `json:"action"`
			Tools     json.RawMessage `json:"tools"`
			Model     string          `json:"model"`
			Cwd       string          `json:"cwd"`
			Message   string          `json:"message"`
			Item      struct {
				Type    string          `json:"type"`
				Content json.RawMessage `json:"content"`
			} `json:"item"`
			Usage struct {
				Input     int `json:"input_tokens"`
				Cached    int `json:"cached_input_tokens"`
				Output    int `json:"output_tokens"`
				Reasoning int `json:"reasoning_output_tokens"`
			} `json:"usage"`
			Replacement []json.RawMessage `json:"replacement_history"`
		}
		json.Unmarshal(l.Payload, &p)
		at := l.Timestamp
		reply := func() *assistant {
			if open == nil {
				open = &assistant{}
			}
			open.end = at
			return open
		}
		switch l.Type {
		case "session_meta":
			b.conv.Dir = p.Cwd
		case "turn_context":
			b.model = p.Model
		case "token_usage_record":
			flush(usage(p.Usage.Input, p.Usage.Cached, p.Usage.Output, p.Usage.Reasoning), at)
		case "compacted":
			flush(nil, at)
			var rebuilt []Message
			for _, item := range p.Replacement {
				if m := codexInput(item); m != nil {
					rebuilt = append(rebuilt, m)
				}
			}
			b.compact(p.Message, rebuilt, at)
		case "event_msg":
			prompt := ""
			switch {
			case p.Type == "item_completed" && p.Item.Type == "UserMessage":
				prompt = text(raw(p.Item.Content))
			case p.Type == "user_message":
				prompt = p.Message
			case p.Type == "task_complete":
				flush(nil, at)
				b.end("ok", at)
			case p.Type == "turn_aborted":
				flush(nil, at)
				b.end("cancelled", at)
			}
			// 同一句话可能既有 user_message 又有 UserMessage（不同版本），本轮还没有请求时只算一次。
			if prompt = strings.TrimSpace(prompt); prompt != "" && !(prompt == lastPrompt && b.prompt && b.calls == 0) {
				flush(nil, at)
				b.begin(prompt, at)
				lastPrompt = prompt
			}
		case "response_item":
			switch p.Type {
			case "message":
				if p.Role == "assistant" {
					if t := text(raw(p.Content)); t != "" {
						reply().content = append(reply().content, strings.TrimSpace(t))
					}
					continue
				}
				flush(nil, time.Time{})
				role := "user"
				if p.Role == "developer" || p.Role == "system" {
					role = "system"
				}
				b.add(Message{"role": role, "content": text(raw(p.Content))}, at)
			case "reasoning":
				// 推理原文加密保存，记录里只有可选的摘要。
				if t := text(raw(p.Summary)); t != "" {
					reply().reasoning = append(reply().reasoning, t)
				} else {
					reply()
				}
			case "function_call":
				reply().calls = append(reply().calls, toolCall(p.CallID, p.Name, p.Arguments))
			case "custom_tool_call":
				reply().calls = append(reply().calls, toolCall(p.CallID, p.Name, p.Input))
			case "local_shell_call", "web_search_call", "tool_search_call", "image_generation_call":
				name := strings.TrimSuffix(p.Type, "_call")
				reply().calls = append(reply().calls, toolCall(p.CallID, name, string(p.Action)))
			case "function_call_output", "custom_tool_call_output", "local_shell_call_output", "tool_search_output":
				flush(nil, time.Time{})
				b.add(Message{"role": "tool", "tool_call_id": p.CallID, "content": text(raw(p.Output))}, at)
			case "additional_tools":
				b.tools = codexTools(raw(p.Tools))
			}
		}
	}
	flush(nil, time.Time{})
	return b.finish(), nil
}

// codexInput 把压缩后替换历史里的条目换成消息；工具调用之类的条目在摘要里已经体现，跳过。
func codexInput(item json.RawMessage) Message {
	var p struct {
		Type, Role string
		Content    json.RawMessage
	}
	if json.Unmarshal(item, &p) != nil || p.Type != "message" {
		return nil
	}
	role := map[string]string{"developer": "system", "system": "system", "assistant": "assistant"}[p.Role]
	if role == "" {
		role = "user"
	}
	return Message{"role": role, "content": text(raw(p.Content))}
}

// codexTools 把 Responses 格式的工具定义（可以按 namespace 嵌套，custom 工具用文法而不是 JSON schema）摊平成 function 列表。
func codexTools(v any) []any {
	var out []any
	list, _ := v.([]any)
	for _, item := range list {
		tool, _ := item.(map[string]any)
		if tool == nil {
			continue
		}
		if tool["type"] == "namespace" {
			out = append(out, codexTools(tool["tools"])...)
			continue
		}
		params := tool["parameters"]
		if params == nil {
			params = tool["format"]
		}
		name, _ := tool["name"].(string)
		if name == "" {
			name, _ = tool["type"].(string)
		}
		out = append(out, map[string]any{"type": "function", "function": map[string]any{"name": name, "description": tool["description"], "parameters": params}})
	}
	return out
}
