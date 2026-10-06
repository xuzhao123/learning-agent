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
	"strings"
	"sync"
	"time"
)

type modelUsage struct {
	Prompt     int `json:"prompt_tokens"`
	Completion int `json:"completion_tokens"`
	Details    struct {
		Cached *int `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
}
type modelReply struct {
	Message Message
	Usage   *modelUsage
}

var errContextLength = errors.New("模型服务报告上下文过长")
var errModelBudget = errors.New("模型请求次数预算已用完（包含摘要和超窗重试）")

// 普通请求和摘要共享模型、工具定义和推理配置；摘要额外通过tool_choice禁用工具。
func callModel(ctx context.Context, config modelConfig, history []Message, withTools bool, output int, purpose string) (modelReply, error) {
	endpoint, err := url.Parse(config.APIURL)
	local := endpoint != nil && endpoint.Scheme == "http" && endpoint.Hostname() == "127.0.0.1"
	if err != nil || (endpoint.Scheme != "https" && !local) || endpoint.Host == "" || endpoint.User != nil || endpoint.RawQuery != "" {
		return modelReply{}, errors.New("模型地址需要是完整的HTTPS URL，或本机http://127.0.0.1观测代理")
	}
	body := map[string]any{"model": config.Model, "messages": history, "stream": false, "reasoning_effort": config.Effort}
	// 0表示使用供应商默认值，不在客户端把推理与回答一起截在4096。
	if output > 0 {
		body["max_completion_tokens"] = output
	}
	if withTools {
		body["tools"], body["parallel_tool_calls"] = modelTools(), true
		// tool_choice只在提供工具时有意义；不少兼容接口会拒绝没有tools的tool_choice。
		if purpose == "compact" {
			body["tool_choice"] = "none"
		}
	}
	data, err := json.Marshal(body)
	if err != nil {
		return modelReply{}, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, config.APIURL, bytes.NewReader(data))
	if err != nil {
		return modelReply{}, errors.New("无法创建模型请求")
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+config.APIKey)
	if local {
		// 观测台按这个明确标记区分主任务、摘要和记忆辅助调用，不靠回答内容猜测；代理不转发给上游。
		request.Header.Set("X-Agent-Purpose", purpose)
	}
	client := &http.Client{Timeout: 5 * time.Minute, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return modelReply{}, ctx.Err()
		}
		return modelReply{}, errors.New("模型请求失败，请检查网络与超时")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		var failure struct {
			Error struct{ Code, Message string }
		}
		_ = json.NewDecoder(io.LimitReader(response.Body, 64<<10)).Decode(&failure)
		code, detail := strings.ToLower(failure.Error.Code), strings.ToLower(failure.Error.Message)
		// 只识别明确的上下文长度错误；401、429和普通400不会被误当作压缩信号。
		tooLong := code == "context_length_exceeded" || code == "contextwindowexceeded" || code == "prompttoolong" || code == "outofcontexterror" ||
			(strings.Contains(detail, "maximum context length") && strings.Contains(detail, "exceed")) ||
			(strings.Contains(detail, "context window") && strings.Contains(detail, "exceed"))
		if (response.StatusCode == 400 || response.StatusCode == 413) && tooLong {
			return modelReply{}, errContextLength
		}
		return modelReply{}, fmt.Errorf("模型API返回HTTP %d", response.StatusCode)
	}
	var result struct {
		Usage   *modelUsage `json:"usage"`
		Choices []struct {
			Message      Message `json:"message"`
			FinishReason string  `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 2<<20)).Decode(&result); err != nil {
		return modelReply{}, errors.New("模型响应不是有效JSON")
	}
	reply := modelReply{Usage: result.Usage}
	if len(result.Choices) == 0 {
		return reply, errors.New("模型没有返回消息")
	}
	choice := result.Choices[0]
	if choice.FinishReason != "stop" && choice.FinishReason != "tool_calls" {
		return reply, errors.New("模型响应被截断或未完成；请检查显式输出上限或服务端默认限制")
	}
	reply.Message = choice.Message
	if len(reply.Message.ToolCalls) == 0 && (strings.TrimSpace(reply.Message.Content) == "" || choice.FinishReason == "tool_calls") {
		return reply, errors.New("模型没有返回回答或工具调用")
	}
	if !withTools && len(reply.Message.ToolCalls) > 0 {
		return reply, errors.New("未提供工具的请求意外返回工具调用")
	}
	reply.Message.Role = "assistant"
	return reply, nil
}

func modelTools() []map[string]any {
	tools := make([]map[string]any, 0, len(toolDefinitions))
	for _, definition := range toolDefinitions {
		tools = append(tools, map[string]any{"type": "function", "function": definition})
	}
	return tools
}

// 只有请求计数器，不做另一层重试。所有实际请求（含摘要、超窗重试、记忆背景生成与重排）共享上限。
// 并行的 search_memory 也会请求模型，计数与用量统计由 mu 保护。
type modelClient struct {
	Config                                                   modelConfig
	Calls, Limit, Output                                     int
	SummaryCalls, Input, Completion, Cached, CacheKnownInput int
	SummaryInput, SummaryCached                              int
	AuxCalls, AuxInput, AuxCompletion                        int
	mu                                                       sync.Mutex
}

// 记忆辅助调用不需要主任务的高推理强度；固定较低强度以控制延迟和用量。
const memoryEffort = "low"

func (c *modelClient) call(ctx context.Context, history []Message, withTools bool, purpose string) (modelReply, error) {
	return c.callKeeping(ctx, history, withTools, purpose, 0)
}

// keep：发出这次请求后至少还要为主任务留下的请求次数；辅助调用用它保证主任务仍能回答。
func (c *modelClient) callKeeping(ctx context.Context, history []Message, withTools bool, purpose string, keep int) (modelReply, error) {
	if err := ctx.Err(); err != nil {
		return modelReply{}, err
	}
	if _, err := contextGroups(history); err != nil {
		return modelReply{}, err
	}
	aux := strings.HasPrefix(purpose, "memory_")
	c.mu.Lock()
	if c.Limit-c.Calls <= keep {
		c.mu.Unlock()
		return modelReply{}, errModelBudget
	}
	c.Calls++
	n := c.Calls
	if purpose == "compact" {
		c.SummaryCalls++
	}
	if aux {
		c.AuxCalls++
	}
	c.mu.Unlock()
	config := c.Config
	if aux {
		config.Effort = memoryEffort
	}
	fmt.Printf("Model request: %d/%d purpose=%s input_est=%d\n", n, c.Limit, purpose, contextTokens(history, withTools))
	reply, err := callModel(ctx, config, history, withTools, c.Output, purpose)
	c.mu.Lock()
	defer c.mu.Unlock()
	if u := reply.Usage; u != nil {
		c.Input += u.Prompt
		c.Completion += u.Completion
		cache := "unavailable"
		if u.Details.Cached != nil {
			c.Cached += *u.Details.Cached
			c.CacheKnownInput += u.Prompt
			cache = fmt.Sprint(*u.Details.Cached)
		}
		if purpose == "compact" {
			c.SummaryInput += u.Prompt
			if u.Details.Cached != nil {
				c.SummaryCached += *u.Details.Cached
			}
		}
		if aux {
			c.AuxInput += u.Prompt
			c.AuxCompletion += u.Completion
		}
		fmt.Printf("Usage: purpose=%s prompt_tokens=%d completion_tokens=%d cached_tokens=%s\n", purpose, u.Prompt, u.Completion, cache)
	} else {
		fmt.Printf("Usage: purpose=%s unavailable\n", purpose)
	}
	return reply, err
}
