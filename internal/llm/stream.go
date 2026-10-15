package llm

import (
	"encoding/json"
	"errors"
	"strings"
)

// B1：流式响应（"stream": true）是 SSE：一个 chunk 一行 "data: {...}"，事件之间空一行，最后是 "data: [DONE]"。
// 每个 chunk 只带一小段增量（delta）：
//
//	正文、推理     字符串片段，按到达顺序拼接
//	tool_calls    按 index 归到同一个调用：第一段带 id、type、name，后面只带 arguments 的片段
//	finish_reason 只在最后一个有内容的 chunk 出现；没出现就是流被截断
//	usage         请求里带 stream_options.include_usage 时，[DONE] 前多一个 choices 为空、只有 usage 的 chunk
//
// agent 用它边收边拼出完整回答，观测台的代理用同一份逻辑把转发过的流还原成完整响应留档。
type Stream struct {
	ID, Model    string
	Message      Message
	FinishReason string
	Usage        *Usage
	RawUsage     json.RawMessage // 原样保留供应商的 usage（含 total_tokens、推理 token 等），观测台显示用
	Chunks       int
	Done         bool // 读到了 [DONE]
}

type streamChunk struct {
	ID      string `json:"id"`
	Model   string `json:"model"`
	Choices []struct {
		Delta struct {
			Role             string `json:"role"`
			Content          string `json:"content"`
			ReasoningContent string `json:"reasoning_content"`
			ToolCalls        []struct {
				Index    int    `json:"index"`
				ID       string `json:"id"`
				Type     string `json:"type"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage json.RawMessage `json:"usage"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// Add 处理一行 SSE，返回这一行带来的正文与推理增量。空行、注释（": ..."）和 event: 行不含数据，直接忽略。
func (s *Stream) Add(line string) (content, reasoning string, err error) {
	data, ok := strings.CutPrefix(line, "data:")
	if !ok {
		return "", "", nil
	}
	data = strings.TrimSpace(data)
	if data == "[DONE]" {
		s.Done = true
		return "", "", nil
	}
	var c streamChunk
	if err := json.Unmarshal([]byte(data), &c); err != nil {
		return "", "", errors.New("流式响应里有无效的 JSON chunk")
	}
	if c.Error != nil {
		// 有的供应商在流的中途用一个 error chunk 报告失败（此时 HTTP 状态早已是 200）。
		return "", "", errors.New("模型在流式响应中途报错：" + c.Error.Message)
	}
	s.Chunks++
	if c.ID != "" {
		s.ID = c.ID
	}
	if c.Model != "" {
		s.Model = c.Model
	}
	if len(c.Usage) > 0 && string(c.Usage) != "null" {
		var u Usage
		if json.Unmarshal(c.Usage, &u) == nil {
			s.Usage, s.RawUsage = &u, c.Usage
		}
	}
	if len(c.Choices) == 0 {
		return "", "", nil
	}
	choice := c.Choices[0]
	m := &s.Message
	if choice.Delta.Role != "" {
		m.Role = choice.Delta.Role
	}
	m.Content += choice.Delta.Content
	m.ReasoningContent += choice.Delta.ReasoningContent
	for _, d := range choice.Delta.ToolCalls {
		// index 是这次回复里第几个调用；新 index 出现就是新调用开始。
		for len(m.ToolCalls) <= d.Index {
			m.ToolCalls = append(m.ToolCalls, ToolCall{Type: "function"})
		}
		call := &m.ToolCalls[d.Index]
		if d.ID != "" {
			call.ID = d.ID
		}
		if d.Type != "" {
			call.Type = d.Type
		}
		if d.Function.Name != "" {
			call.Function.Name = d.Function.Name
		}
		call.Function.Arguments += d.Function.Arguments
	}
	if choice.FinishReason != "" {
		s.FinishReason = choice.FinishReason
	}
	return choice.Delta.Content, choice.Delta.ReasoningContent, nil
}

// Response 把收到的 chunk 拼回非流式响应的结构（choices[0].message、finish_reason、usage）。
// 观测台按这个结构留档，页面、续聊恢复和归因读取存档的代码不必区分流式与非流式。
func (s *Stream) Response() map[string]any {
	message := s.Message
	if message.Role == "" {
		message.Role = "assistant"
	}
	response := map[string]any{"id": s.ID, "object": "chat.completion", "model": s.Model,
		"choices": []map[string]any{{"index": 0, "message": message, "finish_reason": s.FinishReason}}}
	if s.RawUsage != nil {
		response["usage"] = s.RawUsage
	}
	return response
}
