package llm

import (
	"encoding/json"
	"errors"
	"strings"
)

// D33：Anthropic Messages 协议（DeepSeek 在 /anthropic 提供）与 chat/completions 的对应：
//
//	chat/completions                 Messages
//	messages[0]（system）             顶层 system
//	user 消息                         user 消息的内容块：text、image（base64）
//	assistant 的推理与 tool_calls     assistant 的内容块：thinking（带 signature）、text、tool_use
//	tool 消息                         下一条 user 消息里的 tool_result 块（tool_use_id 配对）
//	tools[].function                 tools[]：{name, description, input_schema}
//	reasoning_effort                 thinking + output_config.effort
//	max_completion_tokens            max_tokens（必填）
//
// 相邻的同角色消息合成一条（Messages 要求 user、assistant 交替）：工具结果和随后的提醒都进同一条 user 消息。
// 思考块的 signature 存在 Message.EncryptedContent 里，下一次请求原样传回（DeepSeek 不强制，Claude 要求）。

const messagesMaxTokens = 32768 // 没有给输出上限时用它：Messages 要求必填

// MessagesBody 把 chat/completions 的请求体换成 Messages 的请求体。
func MessagesBody(chat []byte) ([]byte, error) {
	var in struct {
		Model    string    `json:"model"`
		Messages []Message `json:"messages"`
		Tools    []struct {
			Function map[string]any `json:"function"`
		} `json:"tools"`
		ToolChoice      any    `json:"tool_choice"`
		ReasoningEffort string `json:"reasoning_effort"`
		MaxTokens       int    `json:"max_completion_tokens"`
	}
	if err := json.Unmarshal(chat, &in); err != nil {
		return nil, errors.New("请求体不是有效的 chat/completions 请求")
	}
	var system []string
	var messages []map[string]any
	add := func(role string, blocks ...any) {
		if len(blocks) == 0 {
			return
		}
		if n := len(messages); n > 0 && messages[n-1]["role"] == role {
			messages[n-1]["content"] = append(messages[n-1]["content"].([]any), blocks...)
			return
		}
		messages = append(messages, map[string]any{"role": role, "content": blocks})
	}
	for _, m := range in.Messages {
		switch m.Role {
		case "system":
			system = append(system, m.Content)
		case "tool":
			var content any = m.Content
			if len(m.Images) > 0 {
				content = messagesContent(m)
			}
			add("user", map[string]any{"type": "tool_result", "tool_use_id": m.ToolCallID, "content": content})
		case "assistant":
			var blocks []any
			if m.ReasoningContent != "" && m.EncryptedContent != "" {
				blocks = append(blocks, map[string]any{"type": "thinking", "thinking": m.ReasoningContent, "signature": m.EncryptedContent})
			}
			if strings.TrimSpace(m.Content) != "" {
				blocks = append(blocks, map[string]any{"type": "text", "text": m.Content})
			}
			for _, call := range m.ToolCalls {
				input := json.RawMessage(call.Function.Arguments)
				if !json.Valid(input) || !strings.HasPrefix(strings.TrimSpace(call.Function.Arguments), "{") {
					input = json.RawMessage("{}")
				}
				blocks = append(blocks, map[string]any{"type": "tool_use", "id": call.ID, "name": call.Function.Name, "input": input})
			}
			add("assistant", blocks...)
		default:
			add("user", messagesContent(m)...)
		}
	}
	maxTokens := messagesMaxTokens
	if in.MaxTokens > 0 {
		maxTokens = in.MaxTokens
	}
	out := map[string]any{"model": in.Model, "max_tokens": maxTokens, "messages": messages, "stream": true}
	if len(system) > 0 {
		out["system"] = strings.Join(system, "\n\n")
	}
	if len(in.Tools) > 0 {
		tools := make([]map[string]any, 0, len(in.Tools))
		for _, t := range in.Tools {
			tools = append(tools, map[string]any{"name": t.Function["name"], "description": t.Function["description"], "input_schema": t.Function["parameters"]})
		}
		out["tools"] = tools
		if in.ToolChoice == "none" {
			out["tool_choice"] = map[string]string{"type": "none"}
		}
	}
	if in.ReasoningEffort != "" {
		// budget_tokens 按 Anthropic 的要求填（至少 1024、小于 max_tokens）；DeepSeek 忽略它，只看 output_config.effort。
		out["thinking"] = map[string]any{"type": "enabled", "budget_tokens": min(4096, maxTokens-1)}
		out["output_config"] = map[string]string{"effort": in.ReasoningEffort}
	}
	return json.Marshal(out)
}

// messagesContent 把一条消息的文字和图片排成内容块，顺序和 chat 的数组一样：开标签、图、闭标签，最后是文字。
func messagesContent(m Message) []any {
	var blocks []any
	for _, img := range m.Images {
		media, data, ok := strings.Cut(strings.TrimPrefix(img.Ref, "data:"), ";base64,")
		if !ok || !strings.HasPrefix(img.Ref, "data:") {
			blocks = append(blocks, map[string]any{"type": "text", "text": img.OpenTag() + "\n图片无法发送：不是 data URL\n" + imageCloseTag})
			continue
		}
		blocks = append(blocks, map[string]any{"type": "text", "text": img.OpenTag()},
			map[string]any{"type": "image", "source": map[string]string{"type": "base64", "media_type": media, "data": data}},
			map[string]any{"type": "text", "text": imageCloseTag})
	}
	if strings.TrimSpace(m.Content) != "" {
		blocks = append(blocks, map[string]any{"type": "text", "text": m.Content})
	}
	return blocks
}

// ConvertBody 按上游协议把 chat 形式的请求体换过去：agent 直连时自己换，经观测台时由代理换，两处用这一份。
func ConvertBody(protocol string, chat []byte) ([]byte, error) {
	switch protocol {
	case "responses":
		return ResponsesBody(chat)
	case "messages":
		return MessagesBody(chat)
	}
	return chat, nil
}

// Messages 的流式事件：message_start（id、输入用量）→ 每个内容块 content_block_start / delta / stop →
// message_delta（stop_reason 与输出用量）→ message_stop。内容块按 index 区分：thinking、text、tool_use。
type messagesEvent struct {
	Type    string `json:"type"`
	Index   int    `json:"index"`
	Message *struct {
		ID    string         `json:"id"`
		Model string         `json:"model"`
		Usage *messagesUsage `json:"usage"`
	} `json:"message"`
	Block *struct {
		Type string `json:"type"`
		ID   string `json:"id"`
		Name string `json:"name"`
		Text string `json:"text"`
	} `json:"content_block"`
	Delta *struct {
		Type        string `json:"type"`
		Text        string `json:"text"`
		Thinking    string `json:"thinking"`
		Signature   string `json:"signature"`
		PartialJSON string `json:"partial_json"`
		StopReason  string `json:"stop_reason"`
	} `json:"delta"`
	Usage *messagesUsage `json:"usage"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// Messages 的 input_tokens 不含命中缓存与写入缓存的部分，三者相加才是 chat 里的 prompt_tokens。
type messagesUsage struct {
	Input         int `json:"input_tokens"`
	Output        int `json:"output_tokens"`
	CacheRead     int `json:"cache_read_input_tokens"`
	CacheCreation int `json:"cache_creation_input_tokens"`
}

func messagesEventType(t string) bool {
	return strings.HasPrefix(t, "message_") || strings.HasPrefix(t, "content_block_") || t == "ping"
}

func (s *Stream) addMessages(data string) (content, reasoning string, err error) {
	var e messagesEvent
	if err := json.Unmarshal([]byte(data), &e); err != nil {
		return "", "", errors.New("流式响应里有无效的 JSON 事件")
	}
	s.Chunks++
	m := &s.Message
	m.Role = "assistant"
	switch e.Type {
	case "message_start":
		if e.Message != nil {
			s.ID, s.Model = e.Message.ID, e.Message.Model
			s.setMessagesUsage(e.Message.Usage)
		}
	case "content_block_start":
		if e.Block == nil {
			break
		}
		switch e.Block.Type {
		case "tool_use":
			if s.toolAt == nil {
				s.toolAt = map[int]int{}
			}
			call := ToolCall{ID: e.Block.ID, Type: "function"}
			call.Function.Name = e.Block.Name
			s.toolAt[e.Index] = len(m.ToolCalls)
			m.ToolCalls = append(m.ToolCalls, call)
		case "text":
			m.Content += e.Block.Text
			return e.Block.Text, "", nil
		}
	case "content_block_delta":
		if e.Delta == nil {
			break
		}
		switch e.Delta.Type {
		case "text_delta":
			m.Content += e.Delta.Text
			return e.Delta.Text, "", nil
		case "thinking_delta":
			m.ReasoningContent += e.Delta.Thinking
			return "", e.Delta.Thinking, nil
		case "signature_delta":
			m.EncryptedContent += e.Delta.Signature
		case "input_json_delta":
			if i, ok := s.toolAt[e.Index]; ok {
				m.ToolCalls[i].Function.Arguments += e.Delta.PartialJSON
			}
		}
	case "message_delta":
		s.setMessagesUsage(e.Usage)
		if e.Delta != nil {
			// 换成 chat 的 finish_reason，截断检查与“有没有回答”的判断不用分协议。
			switch e.Delta.StopReason {
			case "tool_use":
				s.FinishReason = "tool_calls"
			case "max_tokens":
				s.FinishReason = "length"
			case "":
			default:
				s.FinishReason = "stop" // end_turn、stop_sequence
			}
		}
		for i := range m.ToolCalls {
			if strings.TrimSpace(m.ToolCalls[i].Function.Arguments) == "" {
				m.ToolCalls[i].Function.Arguments = "{}" // 没有参数的工具，DeepSeek 不发 input_json_delta
			}
		}
	case "message_stop":
		s.Done = true
	}
	return "", "", nil
}

// setMessagesUsage 记下用量（message_start 带输入，message_delta 带累计的输出），换成 chat 的字段名存档。
func (s *Stream) setMessagesUsage(u *messagesUsage) {
	if u == nil {
		return
	}
	prompt := u.Input + u.CacheRead + u.CacheCreation
	output := u.Output
	if s.Usage != nil {
		prompt, output = max(prompt, s.Usage.Prompt), max(output, s.Usage.Completion)
	}
	cached := u.CacheRead
	usage := Usage{Prompt: prompt, Completion: output}
	usage.Details.Cached = &cached
	s.Usage = &usage
	s.RawUsage, _ = json.Marshal(map[string]any{"prompt_tokens": prompt, "completion_tokens": output, "total_tokens": prompt + output,
		"prompt_tokens_details": map[string]int{"cached_tokens": cached, "cache_write_tokens": u.CacheCreation}})
}
