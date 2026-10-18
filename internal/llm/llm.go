package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"learning-agent/internal/protocol"
	"learning-agent/internal/telemetry"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
	semconv "go.opentelemetry.io/otel/semconv/v1.41.0"
	"go.opentelemetry.io/otel/trace"
)

type Usage struct {
	Prompt     int `json:"prompt_tokens"`
	Completion int `json:"completion_tokens"`
	Details    struct {
		Cached *int `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
}
type Reply struct {
	Message    Message
	Usage      *Usage
	FirstToken time.Duration // B1：从发出请求到收到第一段正文或推理的时间（首字延迟）；没有收到为0
}

var ErrContextLength = errors.New("模型服务报告上下文过长")
var ErrModelBudget = errors.New("模型请求次数预算已用完（包含摘要和超窗重试）")

// Unlimited 是 -max-steps 0（不限请求次数）在内部的取值：足够大，预算的加减比较照常成立，不用到处判断“不限”。
// 防止无限循环靠重复动作与无进展熔断，以及人按停止。
const Unlimited = 1 << 30

// LimitText 用于日志：不限时不打印那个大数。
func LimitText(limit int) string {
	if limit >= Unlimited {
		return "不限"
	}
	return strconv.Itoa(limit)
}

// 普通请求和摘要共享模型、工具定义和推理配置；摘要额外通过tool_choice禁用工具。
// B1：请求一律流式，onDelta 收到每一段正文与推理增量（可以为 nil）；返回的仍是拼好的完整回复，
// 是否截断、有没有回答等检查都等流结束后对完整回复做，和非流式时一样。
func callModel(ctx context.Context, config Config, history []Message, withTools bool, output int, purpose string, onDelta func(content, reasoning string)) (Reply, error) {
	endpoint, err := url.Parse(config.APIURL)
	local := endpoint != nil && endpoint.Scheme == "http" && endpoint.Hostname() == "127.0.0.1"
	if err != nil || (endpoint.Scheme != "https" && !local) || endpoint.Host == "" || endpoint.User != nil || endpoint.RawQuery != "" {
		return Reply{}, errors.New("模型地址需要是完整的HTTPS URL，或本机http://127.0.0.1观测代理")
	}
	body := map[string]any{"model": config.Model, "messages": history, "stream": true, "reasoning_effort": config.Effort,
		// 流式默认不返回用量；要了它，[DONE] 前会多一个只有 usage 的 chunk，检查点与 D15 的成本才有数据。
		"stream_options": map[string]bool{"include_usage": true}}
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
		// 多租户：观测台启动 agent 时发的令牌，代理据此确认请求来自这条对话的 agent。
		if token := os.Getenv("AGENT_PROXY_TOKEN"); token != "" {
			request.Header.Set("X-Agent-Token", token)
		}
		// D13：W3C traceparent 头写着这次 chat span 的ID，观测台把截获的原始请求和 span 对上。
		// 只发给本机代理，不发给供应商：第三方 API 用不上它（Claude Code 默认也只对自家 API 发送）。
		otel.GetTextMapPropagator().Inject(ctx, propagation.HeaderCarrier(request.Header))
	}
	start := time.Now()
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
	// 上限按原始流算：每个 chunk 都带一层 JSON 外壳，同样的回答比非流式响应大很多。
	scanner := bufio.NewScanner(io.LimitReader(response.Body, 32<<20))
	scanner.Buffer(make([]byte, 64*1024), 2<<20)
	var stream Stream
	reply := Reply{}
	for scanner.Scan() {
		content, reasoning, err := stream.Add(scanner.Text())
		if err != nil {
			return Reply{Usage: stream.Usage}, err
		}
		if content == "" && reasoning == "" {
			continue
		}
		if reply.FirstToken == 0 {
			reply.FirstToken = time.Since(start)
		}
		if onDelta != nil {
			onDelta(content, reasoning)
		}
	}
	reply.Usage = stream.Usage
	if err := scanner.Err(); err != nil {
		if ctx.Err() != nil {
			return reply, ctx.Err()
		}
		return reply, errors.New("模型流式响应读取中断")
	}
	if stream.Chunks == 0 {
		return reply, errors.New("模型没有返回流式数据")
	}
	// 流结束后再检查：finish_reason 只在最后出现，没出现说明流在中途断了。
	if stream.FinishReason != "stop" && stream.FinishReason != "tool_calls" {
		return reply, errors.New("模型响应被截断或未完成；请检查显式输出上限或服务端默认限制")
	}
	reply.Message = stream.Message
	if len(reply.Message.ToolCalls) == 0 && (strings.TrimSpace(reply.Message.Content) == "" || stream.FinishReason == "tool_calls") {
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
	fmt.Printf("Model request: %d/%s purpose=%s input_est=%d\n", n, LimitText(c.Limit), purpose, ContextTokens(history, withTools))
	// D13：一次模型请求一个 chat span（CLIENT：调用进程外的服务）。用途区分主任务、摘要和记忆辅助调用。
	ctx, span := telemetry.Begin(ctx, "chat "+config.Model, trace.SpanKindClient, semconv.GenAIOperationNameChat, semconv.GenAIProviderNameKey.String(config.Provider),
		semconv.GenAIRequestModel(config.Model), attribute.String("agent.purpose", purpose), attribute.Int("agent.request.number", n),
		attribute.Int("agent.request.input_estimate", ContextTokens(history, withTools)), attribute.String("gen_ai.request.reasoning_effort", config.Effort))
	telemetry.Content(span, semconv.GenAIInputMessagesKey, history)
	// B0：一次模型调用是一个 modelCall Item。完整的请求与响应原文仍由观测台的代理记录，这里只给结构化摘要。
	item := protocol.Item{ID: fmt.Sprintf("model-%d", n), Type: "modelCall", Data: map[string]any{"purpose": purpose, "number": n, "input_estimate": ContextTokens(history, withTools)}}
	protocol.ItemStarted(TaskID, item)
	// B1：主任务与摘要的增量实时发给界面（item/delta，只用于显示，权威结果仍是 item/completed 与检查点）；
	// 主任务的正文同时边收边打印到终端。记忆辅助、评审这类调用的输出是给程序读的，不发增量。
	var onDelta func(content, reasoning string)
	printed := false
	if purpose == "main" || purpose == "compact" {
		onDelta = func(content, reasoning string) {
			if reasoning != "" {
				protocol.Notify("item/delta", map[string]any{"turn_id": TaskID, "item_id": item.ID, "purpose": purpose, "field": "reasoning", "delta": reasoning})
			}
			if content != "" {
				protocol.Notify("item/delta", map[string]any{"turn_id": TaskID, "item_id": item.ID, "purpose": purpose, "field": "content", "delta": content})
			}
			if purpose == "main" && content != "" {
				fmt.Print(content)
				printed = true
			}
		}
	}
	reply, err := callModel(ctx, config, history, withTools, c.Output, purpose, onDelta)
	if printed {
		fmt.Println()
	}
	done := map[string]any{"purpose": purpose, "number": n, "tool_calls": len(reply.Message.ToolCalls)}
	if reply.FirstToken > 0 {
		done["first_token_ms"] = reply.FirstToken.Milliseconds()
		span.SetAttributes(attribute.Int64("agent.response.first_token_ms", reply.FirstToken.Milliseconds()))
	}
	if u := reply.Usage; u != nil {
		done["usage"] = u
	}
	if err != nil {
		done["error"] = err.Error()
	}
	item.Data = done
	protocol.ItemCompleted(TaskID, item)
	if u := reply.Usage; u != nil {
		span.SetAttributes(semconv.GenAIUsageInputTokens(u.Prompt), semconv.GenAIUsageOutputTokens(u.Completion))
		if u.Details.Cached != nil {
			span.SetAttributes(semconv.GenAIUsageCacheReadInputTokens(*u.Details.Cached))
		}
	}
	if err == nil {
		span.SetAttributes(attribute.Int("agent.response.tool_calls", len(reply.Message.ToolCalls)))
		telemetry.Content(span, semconv.GenAIOutputMessagesKey, reply.Message)
	}
	telemetry.End(span, err, errorType(ctx, err))
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

// error.type 用少数几类，便于按类别统计失败率；具体原因在 span 状态描述里。
func errorType(ctx context.Context, err error) string {
	switch {
	case err == nil:
		return ""
	case ctx.Err() != nil:
		return "cancelled"
	case errors.Is(err, ErrContextLength):
		return "context_length_exceeded"
	case strings.HasPrefix(err.Error(), "模型API返回HTTP"):
		return "http_error"
	}
	return "model_error"
}
