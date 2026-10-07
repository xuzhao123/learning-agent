# Day 13 项目实践：OpenTelemetry 调用链

原理见 [Day 13 学习笔记](day-13-notes.md)。核心代码在 [internal/telemetry](../../internal/telemetry/telemetry.go)；打点分布在模型请求、执行循环、子进程、队列、MCP、检索、记忆、沙箱、浏览器和观测台里，下文逐一列出。

## 1. 实现概览

```sh
go run . -question '现在几点？再算 1234*5678'                 # span 写到 .data/traces/<trace_id>.jsonl
go run . observe                                            # 观测 → “调用链”标签
go run . queue queue/tasks.jsonl                            # 一次队列运行 = 一个 trace

# 同时发给兼容 OTLP 的平台（Jaeger、Langfuse、Phoenix 等），见第 4 节
OTEL_EXPORTER_OTLP_ENDPOINT=http://127.0.0.1:4318 go run . -question '……'
# 打开内容采集：提示词、回答、工具参数与结果写进 span（截断到 8KB）
OTEL_INSTRUMENTATION_GENAI_CAPTURE_MESSAGE_CONTENT=true go run . -question '……'
```

在观测台里，一次交互对应的 trace 是这样的（真实运行，子 agent + MCP）：

```text
interaction                               observer  SERVER   ← 观测台：每次提问 / 续聊一个
└ invoke_agent learning-agent             agent              ← agent 进程（TRACEPARENT）
  ├ initialize calculator                 agent     CLIENT   ← MCP 握手 + tools/list
  ├ chat doubao-…            step=1       agent     CLIENT
  ├ execute_tool spawn_agent step=1       agent
  │ └ invoke_agent learning-agent         agent              ← 子 agent 进程（TRACEPARENT）
  │   ├ chat …
  │   └ execute_tool mcp_calculator
  │     └ tools/call calculator           agent     CLIENT
  │       └ tools/call calculator         mcp-calculator SERVER ← MCP server 进程（_meta.traceparent）
  ├ execute_tool spawn_agent step=1       agent              ← 并行的第二个子 agent
  │ └ …
  └ chat doubao-…            step=2       agent     CLIENT
```

| 设计点 | 本项目的选择 |
| --- | --- |
| 库 | 官方 OpenTelemetry Go SDK v1.46（兼容 Go 1.25）；属性名用 `semconv/v1.41.0` 的 GenAI 常量 |
| trace 粒度 | 一次交互一个 trace：观测台的首问、续聊；命令行的一次运行；`-resume` 续跑也是新的 trace。同一对话的 trace 共用 `gen_ai.conversation.id`（观测台是对话ID，命令行是任务ID）。一次队列运行是一个 trace |
| 根 span | 观测台：`interaction`；命令行：`invoke_agent learning-agent`；队列：`invoke_workflow queue` |
| Turn / Step | 不单独建 span，`agent.step` 作为属性挂在本轮的 chat、execute_tool 上；`agent.turn` 在 interaction 上 |
| 跨进程 | 子 agent、队列任务、观测台启动的 agent：环境变量 `TRACEPARENT`；模型请求经观测台代理时：`traceparent` 头；MCP：`params._meta.traceparent`；Bash 沙箱：`--setenv TRACEPARENT` |
| 导出 | 本地文件 `.data/traces/<trace_id>.jsonl`，Simple 处理器同步写；设置 `OTEL_EXPORTER_OTLP_ENDPOINT` 时再加一路 OTLP/HTTP，Batch 处理器 |
| 内容 | 默认只记元数据；`OTEL_INSTRUMENTATION_GENAI_CAPTURE_MESSAGE_CONTENT=true` 时写 `gen_ai.input/output.messages`、`gen_ai.tool.call.arguments/result` 等，截断到 8KB |
| 结构化事件 | 重试、不重试原因、重试耗尽、服务端超窗、拿到并发槽位、拿到沙箱锁：span 事件 |
| 指标 | 观测台从 span 汇总：模型与向量化的次数、失败率、P50/P95、token；各工具的耗时与失败率；任务结局 |
| 观测台 | 保留代理角色（原始请求、回放、续聊），新增“调用链”标签读 span；截获的请求按 `traceparent` 对上 chat span |

## 2. 读代码

### 2.1 安装 SDK 与本地导出器

[`telemetry.Start`](../../internal/telemetry/telemetry.go) 在每个进程启动时调用一次（[main.go](../../main.go) 按模式给出 service 名：agent、observer、queue、mcp-calculator）：

```go
options := []sdktrace.TracerProviderOption{sdktrace.WithResource(res),
	sdktrace.WithSpanProcessor(sdktrace.NewSimpleSpanProcessor(&fileExporter{}))}
if os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT") != "" || os.Getenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT") != "" {
	if exporter, err := otlptracehttp.New(context.Background()); err == nil {
		options = append(options, sdktrace.WithBatcher(exporter))
	}
}
```

- 同一个 span 同时交给两个处理器：本地文件同步写（span 一结束就落盘，观测台能实时读到；进程被强杀只丢没结束的 span）；OTLP 批量发送（不在每个 span 结束时等网络）。
- `fileExporter` 只实现 SDK 的 `SpanExporter` 接口（`ExportSpans`、`Shutdown`），按 trace_id 分文件追加一行 JSON。同一个 trace 的 span 来自好几个进程，各自以 `O_APPEND` 写整行。SDK 自带的 `stdouttrace` 只能写到一个 writer，不能按 trace 分文件。
- `main` 在 `run()` 返回后调用 shutdown，把 Batch 缓冲区里的 span 发完。

### 2.2 一次运行的根 span

[main.go](../../main.go) 在建好取消上下文之后创建根 span，父节点来自 `TRACEPARENT`：

```go
ctx, root := telemetry.Begin(telemetry.FromEnv(ctx), "invoke_agent learning-agent", trace.SpanKindInternal, agent.RootAttributes()...)
defer root.End()
```

放在 main 而不是 `agent.Run` 里，是为了让启动阶段的 MCP 连接、检索建库也在同一个 trace 下；否则命令行运行一次会产生好几个互不相连的 trace。[`agent.Run`](../../internal/agent/react.go) 再用 `trace.SpanFromContext(ctx)` 补上任务ID、模型、`agent.resume`，并在退出时写 `agent.outcome`（`no_tool_calls`、`max_steps`、`timeout`、`cancelled`、`repeated_action` 等，与检查点的停止原因一致）。

### 2.3 模型请求、工具与压缩

- **chat**：[`Client.CallKeeping`](../../internal/llm/llm.go) 包住一次 `callModel`，记 `agent.purpose`（main、compact、memory_contextualize、memory_rerank）、请求序号、输入估算与实际 usage。`error.type` 只分 cancelled、context_length_exceeded、http_error、model_error 几类。
- **execute_tool**：[`ExecuteBatch`](../../internal/agent/react.go) 的每个逻辑调用一个 span，包括排队等并发槽位、所有尝试和退避；`runWithRetry` 往当前 span 加 `retry`、`no_retry`、`retry_exhausted` 事件。`endTool` 把 Observation 的四种状态映射到 span：`ok` 成功，`error`、`unknown`、`not_run` 失败且 `error.type` 就是状态。续跑时没有执行、直接回填“结果未知”的调用也留一个 span，带 `agent.tool.replayed=true`。
- **compact_context**：[`Context.Prepare`](../../internal/agent/context_manager.go) 触发压缩时一个 span，记压缩前后的估算量；摘要请求（purpose=compact 的 chat）挂在它下面。

单次尝试在自己的 goroutine 里执行（Day 8）。超时后执行器不再等，但工具 goroutine 仍持有同一个上下文：它内部的 span 会在父 span 结束之后才结束。调用链上看到“子 span 比父 span 结束得晚”，就是“结果未知、后台可能还在跑”的直接证据。

### 2.4 跨进程

```go
// internal/agent/child.go：子 agent 与队列任务
cmd.Env = append(os.Environ(), telemetry.Env(ctx)...) // TRACEPARENT=00-…-<当前 span>-01

// internal/mcp/mcp.go：client 一侧
request.Params.Meta = sdk.NewMetaFromMap(meta) // {"traceparent": "00-…"}

// server 一侧（calculator handler）
ctx = telemetry.Extract(ctx, request.Params.Meta.AdditionalFields)
```

- `os.Environ()` 里可能已经有父进程传来的 `TRACEPARENT`；Go 的 `exec.Cmd` 遇到同名变量以最后一个为准，所以追加的新值会覆盖它。
- 观测台启动 agent 时还设 `AGENT_CONVERSATION_ID`；子 agent 继承环境变量，因此和父 agent 属于同一个对话。
- 模型请求的 `traceparent` 头只发给本机观测代理，不发给方舟或 DeepSeek（第三方用不上，Claude Code 默认也只对自家 API 发送）。

### 2.5 工具内部

| span | 位置 | 主要属性 |
| --- | --- | --- |
| `retrieval docs` | [retrieval.Search](../../internal/retrieval/retrieval.go) | top_k、hits、accepted、status |
| `embeddings {模型}` | [Embedder.Embed](../../internal/retrieval/embedding.go) | provider（ark 为 CLIENT，local 为 INTERNAL）、是否查询向量、输入字数 |
| `search_memory` | [Store.Retrieve](../../internal/memory/memory_retrieval.go) | phase、mode、total、selected、rerank 结果；背景生成与重排的 chat 是子 span |
| `upsert_memory` | `agent.Run` 结束时的 Commit | 成功或失败 |
| `initialize {server}`、`tools/call {工具}` | [mcp.go](../../internal/mcp/mcp.go) | mcp.method.name、network.transport（pipe/tcp）、协议版本 |
| `sandbox.exec` | [sandbox.execute](../../internal/sandbox/sandbox.go) | 是否有 cgroup、退出码、输出字节、被拒主机数；`lock_acquired` 事件 |
| `browser.tab` | [browser.inTab](../../internal/browser/browser.go) | url.full、被拦截的请求数 |

### 2.6 观测台

[server.go](../../internal/observer/server.go)：

- `beginTurn` 在 start/continue 时创建 `interaction` 根 span，trace_id 记进 start/continue 事件的 `trace` 字段；agent 进程退出后先结束 span，再记 exit 事件，页面看到 exit 时根 span 已经在文件里。
- `proxy` 从 `traceparent` 头取出 span_id，记进 request 事件的 `span` 字段。
- `GET /traces/{id}` 读一个 trace 的全部 span；`GET /trace-metrics[?conversation=对话ID]` 汇总指标。

页面的“调用链”标签（[index.html](../../internal/observer/index.html) 的 `renderSpans`）：上方是指标表，下方每个 Turn 一棵 span 树和瀑布条；点击 span 在右侧显示属性和事件；chat span 有“在轨迹中查看原始请求”按钮。找不到父节点的 span 标黄。

## 3. 实验记录

均用真实方舟模型运行，工具真实执行。

### 3.1 命令行

“现在几点？顺便算一下 1234*5678”：一个 trace，`invoke_agent` 下两次 chat（step 1、2），step 1 的两个 execute_tool 并行。根 span 记 `outcome=no_tool_calls`、输入 1539 tokens，与终端 Usage 行之和一致。

### 3.2 观测台：子 agent + MCP

勾选子 agent 和 MCP，要求“并行派两个子 agent，一个查时间，一个用 MCP 计算器算 (1234*5678)+99”：

- 一个 trace、25 个 span，来自 4 种进程：observer、agent、两个子 agent、mcp-calculator；
- 两个 `execute_tool spawn_agent` 时间重叠（各约 9s），父 agent 的 `invoke_agent` 29.5s，与观测台的 `interaction` 基本相同；
- `tools/call calculator` 的 client span（5ms）下面是 server 进程的 span（0ms），说明 `_meta.traceparent` 生效；
- 截获的 6 条模型请求都带 span_id，6/6 对上 chat span；
- `/trace-metrics`：chat 6 次，P50 3.2s、P95 8.2s；spawn_agent 2 次；结局 `no_tool_calls` 3 次（父 + 2 个子）。

每个 agent 进程启动时连接 MCP 中心里启用的 3 个 server，第一个（用 `go run` 从模块源码拉起的 everything 示例）握手用了 2.9–4.0s。以前只能看到“第一次模型请求前有一段空白”，现在调用链直接显示是 `initialize` 占用的。

### 3.3 队列

3 个任务、2 个 worker：一个 trace、22 个 span。`invoke_workflow queue` 27.5s，下面 3 个 `queue_task`，各自挂着子进程的 `invoke_agent`。可以直接看出总时长由最慢的 d13-q1 决定：模型分 5 步逐步验算开平方。

### 3.4 kill -9 与续跑

`-lab-tools` 下让模型同时调用 `slow_job(20)` 和查时间，执行中 `kill -9`：

- 第一个 trace 只有 2 个 span：已经结束的 chat 和 get_current_datetime。根 `invoke_agent` 和正在执行的 `slow_job` 都没有导出，两个 span 因父节点缺失成为孤儿，页面标黄；
- `-resume` 产生第二个 trace（`agent.resume=true`，conversation.id 相同）：`execute_tool slow_job` 0ms、`status=unknown`、`agent.tool.replayed=true`，get_current_datetime 重新执行；
- 之后模型连续用 `check_task_status` 核对，被重复动作熔断停下：根 span 失败，`agent.outcome=repeated_action`。

### 3.5 外部平台

从官方 GitHub release 下载 Jaeger v2.22.0（linux-arm64）；release 附带的校验文件列的是解压后二进制的 sha256，解压后用 `sha256sum -c` 核对，再直接运行单个二进制。设置 `OTEL_EXPORTER_OTLP_ENDPOINT=http://127.0.0.1:4318` 运行 agent：Jaeger 的 `/api/traces/<trace_id>` 返回同一个 trace 的 5 个 span，service 为 agent；同时打开内容采集后，chat 与 execute_tool 带上了消息与参数属性。本地文件照常写入。

### 3.6 页面

用无头 Chrome 打开 3.2 的对话，切到“调用链”：25 行 span、指标表正常，点击 chat span 后右侧显示属性和跳转按钮，页面没有 JS 异常。

## 4. 接入外部平台

只需设置 OTel 的标准环境变量，代码不用改。子进程（子 agent、队列任务、MCP server）继承环境变量，也会导出。

**Jaeger**（只看 trace，最轻）：

```sh
# 从 https://github.com/jaegertracing/jaeger/releases 下载对应平台的 jaeger-<版本>-<os>-<arch>.tar.gz 和 .sha256sum.txt
tar xzf jaeger-<版本>-<os>-<arch>.tar.gz && sha256sum -c jaeger-<版本>-<os>-<arch>.sha256sum.txt   # 校验的是解压出的二进制
./jaeger-<版本>-<os>-<arch>/jaeger --config=file:config.yaml   # OTLP: 4317 (gRPC) / 4318 (HTTP)；界面: http://127.0.0.1:16686
OTEL_EXPORTER_OTLP_ENDPOINT=http://127.0.0.1:4318 go run . observe
```

不带 `--config` 时用内置配置，它没给查询界面指定地址，界面会监听所有网卡。内置配置在源码的 `cmd/jaeger/internal/all-in-one.yaml`，复制一份后精简：只保留 `otlp` 接收、内存存储，并给 `extensions.jaeger_query.http.endpoint`、`grpc.endpoint` 和各接收端口都写上 `127.0.0.1`，可以先用 `jaeger validate --config=file:config.yaml` 检查。内容采集打开时尤其要只在本机运行。数据存在内存里，退出即丢失。

**Langfuse / Phoenix**（偏 LLM 场景，有会话、评分）：自托管或使用云服务后，把 endpoint 指向它们的 OTLP 地址，鉴权放在 `OTEL_EXPORTER_OTLP_HEADERS`（例如 `Authorization=Basic <base64(公钥:私钥)>`），具体地址和头以各平台文档为准。密钥只放环境变量或 `.env` 之外的本地私有配置，不写进仓库。

## 5. 动手

1. 用 3.1 的命令运行一次，`ls -t .data/traces | head -1` 找到最新的 trace，用 `jq` 按 `parent_id` 自己画出树。
2. 在观测台勾选子 agent 提一个需要并行子任务的问题，在“调用链”里找出两个 `spawn_agent` 的重叠区间，再点 chat span 跳到原始请求。
3. 用 `-lab-tools` 让模型调用 `always_fail`，在 execute_tool 的事件里数 `retry` 次数，对照 `-retries`。
4. 把 `-tool-timeout 3s` 和 `slow_job(10)` 组合，观察 `slow_job` 的 span 状态和它结束的时间点。
5. 打开内容采集，比较同一个 chat span 前后的属性大小。

## 6. 已知边界

- **强杀丢 span**：只导出已结束的 span。根 span 和执行中的工具在 kill -9 后没有记录；检查点仍是恢复的依据，trace 只用于排查。
- **事件用的是 span event**：重试等结构化事件挂在 span 上，终端输出仍是原来的文本行（观测台的对话流、记忆和子 agent 面板依赖这些行）。OTel 已宣布弃用 Span Events API，改为与 span 关联的日志事件；把终端输出改成带 trace_id 的结构化日志并经 Logs API 导出，留作后续改进。
- **指标只在观测台汇总**：没有使用 OTel Metrics SDK，也不导出到 Prometheus；Jaeger 只收 trace。每次请求都读取全部 trace 文件，适合学习规模。
- **没有采样、没有清理**：每次运行都记录；`.data/traces/` 不会自动删除。
- **约定仍在变**：GenAI 约定处于 Development，固定使用 v1.41.0 的常量；记忆操作、MCP 的部分属性名是按约定草案手写的字符串。
- **内容开关只管 span**：观测台作为代理本来就保存完整请求体，与内容开关无关。
- **第三方 MCP server** 不认识 `_meta.traceparent` 时，树在 client 一侧的 `tools/call` 处结束。
- **D13 之前的对话**没有 trace_id，“调用链”标签显示为空。
