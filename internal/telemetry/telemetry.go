package telemetry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.41.0"
	"go.opentelemetry.io/otel/trace"
)

// D13 调用链追踪：用 OpenTelemetry Go SDK 创建 span，属性按 GenAI 语义约定（gen_ai.*）命名。
// 每个进程启动时调用 Start：span 结束时写进 .data/traces/<trace_id>.jsonl（观测台读它）；
// 设置了 OTEL_EXPORTER_OTLP_ENDPOINT 时再批量发给 Jaeger、Langfuse 等兼容 OTLP 的平台。
// 跨进程靠 W3C trace context：子进程用环境变量 TRACEPARENT，HTTP 用 traceparent 头，MCP 用 params._meta。

const Dir = ".data/traces"

var Tracer = otel.Tracer("learning-agent")

// 提示词、回答、工具参数与结果默认不进 span，只记耗时、token、状态这些元数据。
// 与社区一致：内容可能含敏感信息，要显式打开，并且截断。
var CaptureContent = os.Getenv("OTEL_INSTRUMENTATION_GENAI_CAPTURE_MESSAGE_CONTENT") == "true"

// 同一个对话的多次交互（每次一个 trace）用 gen_ai.conversation.id 归到一起。
// 观测台启动 agent 时设 AGENT_CONVERSATION_ID；子 agent 继承环境变量，因此属于同一个对话。
var ConversationID = os.Getenv("AGENT_CONVERSATION_ID")

// Start 安装 SDK 并返回关闭函数；关闭时把还在缓冲区里的 span 发出去。
func Start(service string) (shutdown func()) {
	res, _ := resource.Merge(resource.Default(), resource.NewSchemaless(semconv.ServiceName(service), semconv.ProcessPID(os.Getpid())))
	// 本地文件同步写：span 一结束就落盘，观测台能实时看到，进程被 kill -9 也只丢还没结束的 span。
	options := []sdktrace.TracerProviderOption{sdktrace.WithResource(res), sdktrace.WithSpanProcessor(sdktrace.NewSimpleSpanProcessor(&fileExporter{}))}
	if os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT") != "" || os.Getenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT") != "" {
		// 网络导出走批处理：后台攒一批再发，不在每个 span 结束时等网络。地址、协议等由标准 OTEL_* 环境变量决定。
		if exporter, err := otlptracehttp.New(context.Background()); err == nil {
			options = append(options, sdktrace.WithBatcher(exporter))
		} else {
			fmt.Fprintln(os.Stderr, "Trace: OTLP 导出不可用：", err)
		}
	}
	provider := sdktrace.NewTracerProvider(options...)
	otel.SetTracerProvider(provider)
	otel.SetTextMapPropagator(propagation.TraceContext{})
	return func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := provider.Shutdown(ctx); err != nil {
			fmt.Fprintln(os.Stderr, "Trace: 导出未完成：", err)
		}
	}
}

// FromEnv 读取父进程放在 TRACEPARENT 里的上下文，本进程的根 span 会挂到它下面；没有时开始新的 trace。
func FromEnv(ctx context.Context) context.Context {
	carrier := propagation.MapCarrier{"traceparent": os.Getenv("TRACEPARENT"), "tracestate": os.Getenv("TRACESTATE")}
	return otel.GetTextMapPropagator().Extract(ctx, carrier)
}

// Env 是传给子进程的环境变量：当前 span 作为子进程根 span 的父节点。
func Env(ctx context.Context) []string {
	carrier := propagation.MapCarrier{}
	otel.GetTextMapPropagator().Inject(ctx, carrier)
	env := []string{}
	for key, value := range carrier {
		env = append(env, strings.ToUpper(key)+"="+value)
	}
	return env
}

// Carrier 把当前 span 写成 {"traceparent": …}，用于 HTTP 头或 MCP 的 _meta。
func Carrier(ctx context.Context) map[string]string {
	carrier := propagation.MapCarrier{}
	otel.GetTextMapPropagator().Inject(ctx, carrier)
	return carrier
}

// Extract 从 MCP 请求的 _meta 等键值里取出远程父 span。
func Extract(ctx context.Context, values map[string]any) context.Context {
	carrier := propagation.MapCarrier{}
	for _, key := range []string{"traceparent", "tracestate"} {
		if value, ok := values[key].(string); ok {
			carrier[key] = value
		}
	}
	return otel.GetTextMapPropagator().Extract(ctx, carrier)
}

// Turn 与 Step 不是标准 span，作为属性挂在 span 上（agent.step），树不再多套一层。
type stepKey struct{}

func WithStep(ctx context.Context, step int) context.Context {
	return context.WithValue(ctx, stepKey{}, step)
}

func Begin(ctx context.Context, name string, kind trace.SpanKind, attrs ...attribute.KeyValue) (context.Context, trace.Span) {
	if step, ok := ctx.Value(stepKey{}).(int); ok {
		attrs = append(attrs, attribute.Int("agent.step", step))
	}
	return Tracer.Start(ctx, name, trace.WithSpanKind(kind), trace.WithAttributes(attrs...))
}

// End 结束 span；err 不为空时标成失败，error.type 记错误类别（低基数），描述记具体原因。
func End(span trace.Span, err error, errorType string) {
	if err != nil {
		if errorType == "" {
			errorType = "_OTHER"
		}
		span.SetAttributes(semconv.ErrorTypeKey.String(errorType))
		span.SetStatus(codes.Error, Clip(err.Error(), 300))
	}
	span.End()
}

// Content 在打开内容采集时附上一段截断的文本或 JSON。
func Content(span trace.Span, key attribute.Key, value any) {
	if !CaptureContent {
		return
	}
	text, ok := value.(string)
	if !ok {
		data, _ := json.Marshal(value)
		text = string(data)
	}
	span.SetAttributes(key.String(Clip(text, 8<<10)))
}

func Clip(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	return text[:cut] + fmt.Sprintf("…[截断，原%d字节]", len(text))
}

// 写进文件的一行就是一个 span，字段与 OTLP 的 Span 对应，只是改成便于阅读的 JSON。
type Record struct {
	TraceID    string         `json:"trace_id"`
	SpanID     string         `json:"span_id"`
	ParentID   string         `json:"parent_id,omitempty"`
	Name       string         `json:"name"`
	Kind       string         `json:"kind"`
	Service    string         `json:"service"`
	Start      time.Time      `json:"start"`
	End        time.Time      `json:"end"`
	Status     string         `json:"status"` // ok 或 error（未设置也算 ok）
	Error      string         `json:"error,omitempty"`
	Attributes map[string]any `json:"attributes,omitempty"`
	Events     []EventRecord  `json:"events,omitempty"`
}

type EventRecord struct {
	Name       string         `json:"name"`
	Time       time.Time      `json:"time"`
	Attributes map[string]any `json:"attributes,omitempty"`
}

var traceIDPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)

func ValidTraceID(id string) bool { return traceIDPattern.MatchString(id) }

func Path(traceID string) string { return filepath.Join(Dir, traceID+".jsonl") }

// fileExporter 实现 SDK 的 SpanExporter 接口：按 trace_id 分文件追加。
// 同一个 trace 的 span 来自好几个进程（观测台、agent、子 agent、MCP server），各自以 O_APPEND 写一整行，互不覆盖。
type fileExporter struct{ mu sync.Mutex }

func (e *fileExporter) ExportSpans(_ context.Context, spans []sdktrace.ReadOnlySpan) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := os.MkdirAll(Dir, 0o700); err != nil {
		return err
	}
	var errs []error
	for _, span := range spans {
		record := Record{
			TraceID: span.SpanContext().TraceID().String(), SpanID: span.SpanContext().SpanID().String(),
			Name: span.Name(), Kind: span.SpanKind().String(), Start: span.StartTime().UTC(), End: span.EndTime().UTC(),
			Status: "ok", Attributes: values(span.Attributes()),
		}
		if span.Parent().IsValid() {
			record.ParentID = span.Parent().SpanID().String()
		}
		if service, ok := span.Resource().Set().Value(semconv.ServiceNameKey); ok {
			record.Service = service.AsString()
		}
		if span.Status().Code == codes.Error {
			record.Status, record.Error = "error", span.Status().Description
		}
		for _, event := range span.Events() {
			record.Events = append(record.Events, EventRecord{Name: event.Name, Time: event.Time.UTC(), Attributes: values(event.Attributes)})
		}
		line, err := json.Marshal(record)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		file, err := os.OpenFile(Path(record.TraceID), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		_, err = file.Write(append(line, '\n'))
		errs = append(errs, err, file.Close())
	}
	return errors.Join(errs...)
}

func (e *fileExporter) Shutdown(context.Context) error { return nil }

func values(attrs []attribute.KeyValue) map[string]any {
	if len(attrs) == 0 {
		return nil
	}
	result := map[string]any{}
	for _, kv := range attrs {
		result[string(kv.Key)] = kv.Value.AsInterface()
	}
	return result
}
