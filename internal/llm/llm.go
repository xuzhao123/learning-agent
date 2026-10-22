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
	"syscall"
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
	Model      string        // 响应里的 model：请求 ark-code-latest 这类跟随控制台的名字时，实际是哪个模型回答的
}

var ErrContextLength = errors.New("模型服务报告上下文过长")

// transient 标记可以退避重试的失败：限流（429）、服务端错误（5xx）、连接失败、流在中途断开。
// after 是服务端在 Retry-After 里要求的等待；0 表示按退避表。参数错误、鉴权失败、额度用完、上下文过长、输出被截断都不算。
type transient struct {
	error
	after     time.Duration
	proxyDown bool // 连不上观测台的模型代理（观测台在重启）：请求根本没发出去
}

func (t *transient) Unwrap() error { return t.error }

// 重试最多几次、单次最多等多久：Retry-After 要求等得更久（比如额度按小时重置）就不重试，直接报错。
const (
	modelRetries = 3
	maxRetryWait = 60 * time.Second
)

func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return line
}

// retryAfter 解析 Retry-After：秒数或 HTTP 日期；没有或看不懂时为 0。
func retryAfter(value string) time.Duration {
	value = strings.TrimSpace(value)
	if seconds, err := strconv.Atoi(value); err == nil && seconds >= 0 {
		return time.Duration(seconds) * time.Second
	}
	if at, err := http.ParseTime(value); err == nil {
		return max(0, time.Until(at))
	}
	return 0
}

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
	// 明文 HTTP 只用于本机观测代理和不要密钥的上游（本机的 Coconut）：密钥不能经明文发出去。
	plain := endpoint != nil && endpoint.Scheme == "http" && (local || config.APIKey == "")
	if err != nil || (endpoint.Scheme != "https" && !plain) || endpoint.Host == "" || endpoint.User != nil || endpoint.RawQuery != "" {
		return Reply{}, errors.New("模型地址需要是完整的HTTPS URL；明文HTTP只用于本机观测代理和不要密钥的上游")
	}
	body := map[string]any{"model": config.Model, "messages": prepareHistory(history, config.Vision()), "stream": true, "reasoning_effort": config.Effort,
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
	// Responses、Messages 上游：直连时在这里换请求体；经观测台时发 chat 形式，由代理换（存档仍是 chat 形式）。
	if !local {
		if data, err = ConvertBody(config.Protocol, data); err != nil {
			return Reply{}, err
		}
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, config.APIURL, bytes.NewReader(data))
	if err != nil {
		return Reply{}, errors.New("无法创建模型请求")
	}
	request.Header.Set("Content-Type", "application/json")
	if config.APIKey != "" {
		request.Header.Set("Authorization", "Bearer "+config.APIKey)
	}
	if local {
		// 上游说 Responses、Messages 协议时告诉代理，代理转发前换请求体。
		if config.Protocol != "" {
			request.Header.Set("X-Agent-Protocol", config.Protocol)
		}
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
		down := os.Getenv("AGENT_PROXY_TOKEN") != "" && errors.Is(err, syscall.ECONNREFUSED)
		return Reply{}, &transient{error: errors.New("模型请求失败，请检查网络与超时"), proxyDown: down}
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
		// 带上上游的错误码与说明（压成一行、截短）：额度用完、限流、鉴权失败光看状态码分不清，
		// 额度的重置时间也在说明里。前缀不变，errorType 按它归类。
		err := fmt.Errorf("模型API返回HTTP %d", response.StatusCode)
		if reason := strings.Join(strings.Fields(failure.Error.Code+" "+failure.Error.Message), " "); reason != "" {
			err = fmt.Errorf("模型API返回HTTP %d：%s", response.StatusCode, telemetry.Clip(reason, 400))
		}
		// O1：限流与服务端错误退避重试；额度用完也是 429，但要等到额度重置，重试没有用。
		quota := strings.Contains(code, "quota") || strings.Contains(detail, "quota")
		if (response.StatusCode == http.StatusTooManyRequests || response.StatusCode >= 500) && !quota {
			return Reply{}, &transient{error: err, after: retryAfter(response.Header.Get("Retry-After"))}
		}
		return Reply{}, err
	}
	// 上限按原始流算：每个 chunk 都带一层 JSON 外壳，同样的回答比非流式响应大很多。
	scanner := bufio.NewScanner(io.LimitReader(response.Body, 32<<20))
	scanner.Buffer(make([]byte, 64*1024), 2<<20)
	var stream Stream
	reply := Reply{}
	for scanner.Scan() {
		content, reasoning, err := stream.Add(scanner.Text())
		if err != nil {
			// 供应商在流中途报错（此时 HTTP 状态早已是 200）：多是过载一类的暂时故障，按可重试处理。
			return Reply{Usage: stream.Usage}, &transient{error: err}
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
	reply.Usage, reply.Model = stream.Usage, stream.Model
	if err := scanner.Err(); err != nil {
		if ctx.Err() != nil {
			return reply, ctx.Err()
		}
		return reply, &transient{error: errors.New("模型流式响应读取中断")}
	}
	if stream.Chunks == 0 {
		return reply, &transient{error: errors.New("模型没有返回流式数据")}
	}
	// 流结束后再检查：finish_reason 只在最后出现，没出现说明流在中途断了（可以重试）；是 length 说明输出到了上限（重试也一样）。
	if stream.FinishReason == "" {
		return reply, &transient{error: errors.New("模型流式响应在中途结束，没有收到结束标记")}
	}
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
	// O1：暂时故障退避重试（1、2、4 秒，或服务端 Retry-After 要求的时间）；重试不另占请求预算，在 span 上记一个 retry 事件。
	// 流已经输出了一半再重试时，通知界面清掉这次请求已经显示的部分（item/delta 的 reset），免得同一段话出现两遍。
	// 经观测台代理时，代理连不上说明观测台在重启：等它回来（最多 protocol.DetachGrace），不算重试次数。
	var reply Reply
	var err error
	var down time.Time
	for attempt := 0; ; attempt++ {
		reply, err = callModel(ctx, config, history, withTools, c.Output, purpose, onDelta)
		var t *transient
		if errors.As(err, &t) && t.proxyDown && ctx.Err() == nil {
			if down.IsZero() {
				down = time.Now()
				fmt.Printf("\nModel proxy unreachable: purpose=%s waiting for the observer (up to %s)\n", purpose, protocol.DetachGrace)
			}
			if time.Since(down) < protocol.DetachGrace {
				attempt--
				select {
				case <-time.After(2 * time.Second):
				case <-ctx.Done():
				}
				continue
			}
		}
		if err == nil || !errors.As(err, &t) || attempt == modelRetries || ctx.Err() != nil {
			break
		}
		wait := t.after
		if wait == 0 {
			wait = time.Second << attempt
		}
		if wait > maxRetryWait {
			break
		}
		fmt.Printf("\nModel retry: purpose=%s attempt=%d wait=%s reason=%s\n", purpose, attempt+1, wait, firstLine(err.Error()))
		span.AddEvent("retry", trace.WithAttributes(attribute.Int("agent.retry.attempt", attempt+1), attribute.String("agent.retry.reason", firstLine(err.Error())), attribute.Int64("agent.retry.wait_ms", wait.Milliseconds())))
		if onDelta != nil {
			protocol.Notify("item/delta", map[string]any{"turn_id": TaskID, "item_id": item.ID, "purpose": purpose, "field": "reset", "delta": ""})
		}
		select {
		case <-time.After(wait):
		case <-ctx.Done():
		}
		if ctx.Err() != nil {
			err = ctx.Err()
			break
		}
	}
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
	if reply.Model != "" {
		done["response_model"] = reply.Model
		span.SetAttributes(semconv.GenAIResponseModel(reply.Model))
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
