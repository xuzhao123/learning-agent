package llm

import (
	"encoding/json"
	"errors"
	"strings"
)

// Responses 协议（OpenAI Responses API，Codex 用的那一套）与 chat/completions 的对应：
//
//	chat/completions                 Responses
//	messages[0]（system）             instructions
//	user / assistant 消息             input 里的 {role, content}；图片是 input_image
//	assistant 的 tool_calls           每个调用一项 function_call（call_id、name、arguments）
//	tool 消息                         function_call_output（output 是字符串，带图时是数组）
//	tools[].function                 tools[] 摊平成 {type, name, description, parameters}
//	reasoning_effort                 reasoning.effort
//	max_completion_tokens            max_output_tokens
//
// agent 的消息、检查点和观测台存档都只有 chat 形式：发往 Responses 上游前由 ResponsesBody 换一次，
// 收到的流式事件由 Stream.Add 拼回同一个 Message。store 为 false，每次发完整历史，和 chat 的用法一样。
// 推理内容不回传：Responses 要回传的是带 id 或加密内容的 reasoning 项，这个服务只给摘要文字。

// ResponsesBody 把 chat/completions 的请求体换成 Responses 的请求体。agent 直连上游时自己换，
// 经观测台时由代理换（存档留的是换之前的 chat 形式），两处用这一份。
func ResponsesBody(chat []byte) ([]byte, error) {
	var in struct {
		Model    string    `json:"model"`
		Messages []Message `json:"messages"`
		Tools    []struct {
			Function map[string]any `json:"function"`
		} `json:"tools"`
		ToolChoice        any    `json:"tool_choice"`
		ParallelToolCalls *bool  `json:"parallel_tool_calls"`
		ReasoningEffort   string `json:"reasoning_effort"`
		MaxTokens         int    `json:"max_completion_tokens"`
	}
	if err := json.Unmarshal(chat, &in); err != nil {
		return nil, errors.New("请求体不是有效的 chat/completions 请求")
	}
	input := []any{}
	out := map[string]any{"model": in.Model, "stream": true, "store": false}
	for _, m := range in.Messages {
		switch m.Role {
		case "system":
			out["instructions"] = m.Content
		case "tool":
			input = append(input, map[string]any{"type": "function_call_output", "call_id": m.ToolCallID, "output": responsesContent(m)})
		default:
			if m.Content != "" || len(m.Images) > 0 {
				input = append(input, map[string]any{"role": m.Role, "content": responsesContent(m)})
			}
			for _, call := range m.ToolCalls {
				input = append(input, map[string]any{"type": "function_call", "call_id": call.ID, "name": call.Function.Name, "arguments": call.Function.Arguments})
			}
		}
	}
	out["input"] = input
	if len(in.Tools) > 0 {
		tools := make([]map[string]any, 0, len(in.Tools))
		for _, t := range in.Tools {
			tool := map[string]any{"type": "function"}
			for k, v := range t.Function {
				tool[k] = v
			}
			tools = append(tools, tool)
		}
		out["tools"] = tools
		if in.ToolChoice != nil {
			out["tool_choice"] = in.ToolChoice
		}
		if in.ParallelToolCalls != nil {
			out["parallel_tool_calls"] = *in.ParallelToolCalls
		}
	}
	if in.ReasoningEffort != "" {
		// summary 让支持的模型给出推理摘要，界面像显示 reasoning_content 一样显示它。
		out["reasoning"] = map[string]string{"effort": in.ReasoningEffort, "summary": "auto"}
	}
	if in.MaxTokens > 0 {
		out["max_output_tokens"] = in.MaxTokens
	}
	return json.Marshal(out)
}

// responsesContent：没有图就是字符串；有图时和 chat 的数组同样的顺序（开标签、图、闭标签，最后是文字）。
func responsesContent(m Message) any {
	if len(m.Images) == 0 {
		return m.Content
	}
	parts := make([]map[string]string, 0, 3*len(m.Images)+1)
	for _, img := range m.Images {
		parts = append(parts, map[string]string{"type": "input_text", "text": img.OpenTag()},
			map[string]string{"type": "input_image", "image_url": img.Ref},
			map[string]string{"type": "input_text", "text": imageCloseTag})
	}
	if m.Content != "" {
		parts = append(parts, map[string]string{"type": "input_text", "text": m.Content})
	}
	return parts
}

// Responses 的流式事件：每行 data 是一个带 type 的事件。正文与推理摘要按 delta 边收边显示；
// response.completed 带着完整的 output 与 usage，最终的回答以它为准（工具调用也从这里取，不拼参数片段）。
type responsesEvent struct {
	Type    string `json:"type"`
	Delta   string `json:"delta"`
	Message string `json:"message"` // type 为 error 时的说明（Responses）
	Error   *struct {
		Message string `json:"message"`
	} `json:"error"` // type 为 error 时的说明（Anthropic Messages 把它放在 error 对象里）
	Response *struct {
		ID     string `json:"id"`
		Model  string `json:"model"`
		Status string `json:"status"`
		Output []struct {
			Type    string `json:"type"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
			Summary []struct {
				Text string `json:"text"`
			} `json:"summary"`
			CallID    string `json:"call_id"`
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		} `json:"output"`
		Usage *struct {
			Input        int `json:"input_tokens"`
			Output       int `json:"output_tokens"`
			Total        int `json:"total_tokens"`
			InputDetails struct {
				Cached *int `json:"cached_tokens"`
			} `json:"input_tokens_details"`
			OutputDetails struct {
				Reasoning int `json:"reasoning_tokens"`
			} `json:"output_tokens_details"`
		} `json:"usage"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	} `json:"response"`
}

func (s *Stream) addResponses(data string) (content, reasoning string, err error) {
	var e responsesEvent
	if err := json.Unmarshal([]byte(data), &e); err != nil {
		return "", "", errors.New("流式响应里有无效的 JSON 事件")
	}
	s.Chunks++
	s.Message.Role = "assistant"
	if r := e.Response; r != nil {
		if r.ID != "" {
			s.ID = r.ID
		}
		if r.Model != "" {
			s.Model = r.Model
		}
	}
	switch e.Type {
	case "response.output_text.delta":
		s.Message.Content += e.Delta
		return e.Delta, "", nil
	case "response.reasoning_summary_text.delta", "response.reasoning_text.delta":
		s.Message.ReasoningContent += e.Delta
		return "", e.Delta, nil
	case "error":
		if e.Message == "" && e.Error != nil {
			e.Message = e.Error.Message
		}
		return "", "", errors.New("模型在流式响应中途报错：" + e.Message)
	case "response.failed":
		reason := "未知原因"
		if e.Response != nil && e.Response.Error != nil {
			reason = e.Response.Error.Message
		}
		return "", "", errors.New("模型在流式响应中途报错：" + reason)
	case "response.completed", "response.incomplete":
		if e.Response == nil {
			return "", "", nil
		}
		var texts, summaries []string
		var calls []ToolCall
		for _, item := range e.Response.Output {
			switch item.Type {
			case "message":
				for _, part := range item.Content {
					if part.Type == "output_text" {
						texts = append(texts, part.Text)
					}
				}
			case "reasoning":
				for _, part := range item.Summary {
					summaries = append(summaries, part.Text)
				}
			case "function_call":
				call := ToolCall{ID: item.CallID, Type: "function"}
				call.Function.Name, call.Function.Arguments = item.Name, item.Arguments
				calls = append(calls, call)
			}
		}
		// 有的实现在 completed 里不再带 output，这时保留按 delta 拼出的正文。
		if len(e.Response.Output) > 0 {
			s.Message.Content, s.Message.ToolCalls = strings.Join(texts, ""), calls
		}
		if len(summaries) > 0 {
			s.Message.ReasoningContent = strings.Join(summaries, "\n\n")
		}
		// 换成 chat 的 finish_reason，截断检查与“有没有回答”的判断不用分协议。
		switch {
		case e.Type == "response.incomplete":
			s.FinishReason = "length"
		case len(s.Message.ToolCalls) > 0:
			s.FinishReason = "tool_calls"
		default:
			s.FinishReason = "stop"
		}
		if u := e.Response.Usage; u != nil {
			usage := Usage{Prompt: u.Input, Completion: u.Output}
			usage.Details.Cached = u.InputDetails.Cached
			s.Usage = &usage
			// 存档与页面读的是 chat 的 usage 字段名，这里换成同样的结构。
			s.RawUsage, _ = json.Marshal(map[string]any{"prompt_tokens": u.Input, "completion_tokens": u.Output, "total_tokens": u.Total,
				"prompt_tokens_details": map[string]*int{"cached_tokens": u.InputDetails.Cached}, "completion_tokens_details": map[string]int{"reasoning_tokens": u.OutputDetails.Reasoning}})
		}
		s.Done = true
	}
	return "", "", nil
}
