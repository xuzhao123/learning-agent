package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// 工具定义通过 tools 提供，模型通过 tool_calls 返回调用请求。
func callModel(ctx context.Context, config modelConfig, history []Message) (Message, error) {
	endpoint, err := url.Parse(config.APIURL)
	// 本机 http 地址留给观测代理使用，其余地址必须是 HTTPS。
	local := endpoint != nil && endpoint.Scheme == "http" && endpoint.Hostname() == "127.0.0.1"
	if err != nil || (endpoint.Scheme != "https" && !local) || endpoint.Host == "" || endpoint.User != nil || endpoint.RawQuery != "" {
		return Message{}, errors.New("模型地址需要是完整的 HTTPS URL，或本机 http://127.0.0.1 观测代理")
	}
	tools := make([]map[string]any, 0, len(toolDefinitions))
	for _, definition := range toolDefinitions {
		tools = append(tools, map[string]any{"type": "function", "function": definition})
	}
	data, err := json.Marshal(map[string]any{
		"model": config.Model, "messages": history, "tools": tools,
		"parallel_tool_calls": true, "stream": false,
		"reasoning_effort": "high",
	})
	if err != nil {
		return Message{}, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, config.APIURL, bytes.NewReader(data))
	if err != nil {
		return Message{}, errors.New("无法创建模型请求")
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+config.APIKey)
	client := &http.Client{Timeout: 5 * time.Minute, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return Message{}, ctx.Err()
		}
		return Message{}, errors.New("模型请求失败，请检查网络与超时")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return Message{}, fmt.Errorf("模型 API 返回 HTTP %d", response.StatusCode)
	}
	var result struct {
		Choices []struct {
			Message      Message `json:"message"`
			FinishReason string  `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 2<<20)).Decode(&result); err != nil {
		return Message{}, errors.New("模型响应不是有效 JSON")
	}
	if len(result.Choices) == 0 {
		return Message{}, errors.New("模型没有返回消息")
	}
	choice := result.Choices[0]
	if choice.FinishReason != "stop" && choice.FinishReason != "tool_calls" {
		return Message{}, errors.New("模型响应被截断或未完成")
	}
	message := choice.Message
	if len(message.ToolCalls) == 0 && (message.Content == "" || choice.FinishReason == "tool_calls") {
		return Message{}, errors.New("模型没有返回回答或工具调用")
	}
	message.Role = "assistant"
	return message, nil
}
