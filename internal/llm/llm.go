package llm

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

type Usage struct {
	Prompt     int `json:"prompt_tokens"`
	Completion int `json:"completion_tokens"`
	Details    struct {
		Cached *int `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
}
type Reply struct {
	Message Message
	Usage   *Usage
}

var ErrContextLength = errors.New("模型服务报告上下文过长")
var ErrModelBudget = errors.New("模型请求次数预算已用完（包含摘要和超窗重试）")

// 普通请求和摘要共享模型、工具定义和推理配置；摘要额外通过tool_choice禁用工具。
func callModel(ctx context.Context, config Config, history []Message, withTools bool, output int, purpose string) (Reply, error) {
	endpoint, err := url.Parse(config.APIURL)
	local := endpoint != nil && endpoint.Scheme == "http" && endpoint.Hostname() == "127.0.0.1"
	if err != nil || (endpoint.Scheme != "https" && !local) || endpoint.Host == "" || endpoint.User != nil || endpoint.RawQuery != "" {
		return Reply{}, errors.New("模型地址需要是完整的HTTPS URL，或本机http://127.0.0.1观测代理")
	}
	body := map[string]any{"model": config.Model, "messages": history, "stream": false, "reasoning_effort": config.Effort}
	// 0表示使用供应商默认值，不在客户端把推理与回答一起截在4096。
	if output > 0 {
		body["max_completion_tokens"] = output
	}
	if withTools {
		body["tools"], body["parallel_tool_calls"] = FunctionTools(), true
		// tool_choice只在提供工具时有意义；不少兼容接口会拒绝没有tools的tool_choice。
		if purpose == "compact" {
			body["tool_choice"] = "none"
		}
	}
	data, err := json.Marshal(body)
	if err != nil {
		return Reply{}, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, config.APIURL, bytes.NewReader(data))
	if err != nil {
		return Reply{}, errors.New("无法创建模型请求")
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+config.APIKey)
	if local {
		// 观测台按这个明确标记区分主任务、摘要和记忆辅助调用，不靠回答内容猜测；代理不转发给上游。
		request.Header.Set("X-Agent-Purpose", purpose)
		// 任务ID区分父 agent 与子 agent：子进程继承同一个代理地址，靠它才能分开显示。
		if TaskID != "" {
			request.Header.Set("X-Agent-Task", TaskID)
		}
		// 模型路由：告诉代理这次请求本该发往哪家供应商；代理按它转发，观测台不必知道路由表。
		request.Header.Set("X-Agent-Upstream", config.Upstream)
	}
	client := &http.Client{Timeout: 5 * time.Minute, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return Reply{}, ctx.Err()
		}
		return Reply{}, errors.New("模型请求失败，请检查网络与超时")
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
			return Reply{}, ErrContextLength
		}
		return Reply{}, fmt.Errorf("模型API返回HTTP %d", response.StatusCode)
	}
	var result struct {
		Usage   *Usage `json:"usage"`
		Choices []struct {
			Message      Message `json:"message"`
			FinishReason string  `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 2<<20)).Decode(&result); err != nil {
		return Reply{}, errors.New("模型响应不是有效JSON")
	}
	reply := Reply{Usage: result.Usage}
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

func FunctionTools() []map[string]any {
	tools := make([]map[string]any, 0, len(Tools))
	for _, definition := range Tools {
		tools = append(tools, map[string]any{"type": "function", "function": definition})
	}
	return tools
}

// 只有请求计数器，不做另一层重试。所有实际请求（含摘要、超窗重试、记忆背景生成与重排）共享上限。
// 并行的 search_memory 也会请求模型，计数与用量统计由 mu 保护。
type Client struct {
	Config                                                   Config
	Calls, Limit, Output                                     int
	SummaryCalls, Input, Completion, Cached, CacheKnownInput int
	SummaryInput, SummaryCached                              int
	AuxCalls, AuxInput, AuxCompletion                        int
	mu                                                       sync.Mutex
}

// 记忆辅助调用不需要主任务的高推理强度；固定较低强度以控制延迟和用量。
const MemoryEffort = "low"

func (c *Client) Call(ctx context.Context, history []Message, withTools bool, purpose string) (Reply, error) {
	return c.CallKeeping(ctx, history, withTools, purpose, 0)
}

// keep：发出这次请求后至少还要为主任务留下的请求次数；辅助调用用它保证主任务仍能回答。
func (c *Client) CallKeeping(ctx context.Context, history []Message, withTools bool, purpose string, keep int) (Reply, error) {
	if err := ctx.Err(); err != nil {
		return Reply{}, err
	}
	if _, err := Groups(history); err != nil {
		return Reply{}, err
	}
	aux := strings.HasPrefix(purpose, "memory_")
	c.mu.Lock()
	if c.Limit-c.Calls <= keep {
		c.mu.Unlock()
		return Reply{}, ErrModelBudget
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
		config.Effort = MemoryEffort
	}
	fmt.Printf("Model request: %d/%d purpose=%s input_est=%d\n", n, c.Limit, purpose, ContextTokens(history, withTools))
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
