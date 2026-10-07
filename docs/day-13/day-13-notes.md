# Day 13：可观测性——调用链追踪、指标与日志

## 学习目标

读完本文，应能回答以下问题：

1. 一个多步、跨进程的 agent 出了问题，为什么只看日志不够？
2. trace、span、metrics、logs 各是什么，彼此怎样关联？
3. 一个 span 由哪些字段组成？父子关系怎样跨越进程和网络传下去？
4. OpenTelemetry 的 API、SDK、处理器、导出器、OTLP 各管什么？
5. GenAI 语义约定规定了哪些 span 和属性？为什么提示词默认不采集？
6. 截获请求的代理和进程内打点各能看到什么？指标怎样从 span 汇总？

## 1. 为什么 agent 需要调用链

一次 agent 任务往往是这样的：用户提一个问题，agent 请求模型三四次，中间并行调用几个工具；某个工具又启动一个子 agent（另一个进程），子 agent 再调用一个远程 MCP server。最后用户只看到“花了 30 秒”和一个答案。

排查时想知道的是：

- **时间花在哪**：模型推理、工具执行、排队、重试退避，各占多少？
- **哪一步失败、为什么**：失败发生在哪个进程、哪次尝试，是超时、被取消还是参数错误？
- **谁调用了谁**：并行的三个子 agent 各自挂在哪一次工具调用下面？

纯文本日志能记下“发生了什么”，但每个进程各写各的，并行时行与行交错，无法可靠地还原因果关系。调用链追踪（distributed tracing）给每一段工作一个 ID 并记录它的父节点，把所有进程里的记录连成一棵树。

## 2. 三类信号

OpenTelemetry（OTel）把可观测数据分成三类信号：

| 信号 | 是什么 | 适合回答 | 代价 |
| --- | --- | --- | --- |
| **Traces**（调用链） | 一次请求经过的所有工作单元，组成一棵树 | 这一次为什么慢、在哪失败 | 每次请求都有记录，量大，常需要采样 |
| **Metrics**（指标） | 按时间聚合的数值：次数、耗时分布、token 数 | 整体趋势、失败率、P95 延迟、告警 | 聚合后体积小，但看不到单次细节 |
| **Logs**（日志） | 带时间戳的离散记录 | 某个时刻发生了什么的细节 | 文本格式难以统计；需要结构化 |

三者不是互相替代，而是互相关联：日志和指标的样本带上 trace_id，就能从“P95 突然升高”的指标跳到一条慢 trace，再从 trace 里的某个 span 找到当时的日志。

span 上还可以挂**事件**（span event）：带时间戳的一条小记录，比如“第 2 次重试，等待 400ms”。它本质上是关联到 span 的结构化日志。2026 年 OTel 宣布弃用 Span Events API：同一件事有“span 事件”和“Logs API 事件”两种写法，造成分裂，新代码应改用与当前 span 关联的日志事件；已有数据和后端里的展示继续可用。

## 3. Trace 与 span 的数据结构

**trace** 是一次端到端请求，用 16 字节的 `trace_id` 标识。**span** 是 trace 里的一个工作单元，用 8 字节的 `span_id` 标识。一个 span 主要包含：

| 字段 | 作用 | 例子 |
| --- | --- | --- |
| trace_id | 属于哪个 trace；同一 trace 的 span 共用 | `4bf92f3577b34da6a3ce929d0e0e4736` |
| span_id、parent_span_id | 自己是谁、父节点是谁；根 span 没有父节点 | `00f067aa0ba902b7` |
| name | 操作名，低基数（不放每次都变的值） | `execute_tool calculator` |
| kind | 和外部的关系：SERVER 接请求，CLIENT 发请求，INTERNAL 进程内 | 调模型 API 是 CLIENT |
| start、end | 起止时间；耗时 = end − start | |
| status | 未设置、成功或失败，失败时带描述 | `error: 单次执行超过 30s` |
| attributes | 键值属性，用于筛选和聚合 | `gen_ai.usage.input_tokens=1302` |
| events | span 内带时间戳的事件 | `retry {attempt=1, wait_ms=200}` |
| links | 指向其他 trace 的 span，表示“相关但不是父子” | 批处理任务关联多个来源请求 |

一个典型 agent 任务的 trace：

```text
trace 4bf92f…  “查资料并计算”                         总耗时 21.4s
└─ invoke_agent research-agent       INTERNAL          21.4s
   ├─ chat model-x       step=1      CLIENT  in=1.2k   1.9s
   ├─ execute_tool search_docs                          0.4s
   │   ├─ embeddings embed-model     CLIENT             0.3s
   │   └─ retrieval docs  top_k=5                       0.01s
   ├─ execute_tool spawn_agent                         14.0s   ← 子进程
   │   └─ invoke_agent research-agent  (另一个进程)      13.8s
   │       ├─ chat model-x
   │       └─ execute_tool run_shell  (重试 1 次)        3.1s
   └─ chat model-x       step=3                         2.5s
```

**父 span 的耗时不等于子 span 之和**：并行的子 span 时间重叠；父 span 自己也有处理时间（解析参数、排队、写检查点）。树上两个子 span 之间的空白，正是“谁也没有记录的时间”，常常就是问题所在。

**span 只在结束时才完整**：大多数 SDK 在 span 结束后才把它交给导出器。进程被强制杀死时，还没结束的 span（往往包括根 span）就丢了，已经结束的子 span 会因为父节点缺失而成为“孤儿”。

## 4. 上下文传播：父子关系怎样跨进程

同一进程内，当前 span 放在请求上下文里（Go 的 `context.Context`、Python 的 contextvar），新 span 自动以它为父。跨进程时要把“当前 span”编码后带过去，接收方解码后作为远程父节点。这就是**上下文传播**。

W3C Trace Context 规定了标准格式 `traceparent`：

```text
traceparent: 00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01
             │  │                                │                └ flags：01 表示已采样
             │  └ trace_id（32 位十六进制）       └ 父 span_id（16 位十六进制）
             └ 版本
```

`tracestate` 可以再带一些厂商自定义的键值。不同通道的载体不同：

| 通道 | 载体 | 例子 |
| --- | --- | --- |
| HTTP | 请求头 `traceparent` | 调用下游服务 |
| 子进程 | 环境变量 `TRACEPARENT` | 启动工具进程、子 agent，Claude Code 给 Bash 子进程设置的就是它 |
| 消息队列 | 消息头或属性 | Kafka header |
| MCP | 请求的 `params._meta.traceparent`（MCP SEP-414） | 调用 MCP server 的 `tools/call` |

```go
// 发送方：把当前 span 写进载体（这里是一个 map，可以变成请求头或环境变量）
carrier := propagation.MapCarrier{}
otel.GetTextMapPropagator().Inject(ctx, carrier) // carrier["traceparent"] = "00-…-…-01"

// 接收方：从载体取出远程父 span，新 span 自动挂在它下面
ctx = otel.GetTextMapPropagator().Extract(ctx, propagation.MapCarrier{"traceparent": value})
ctx, span := tracer.Start(ctx, "invoke_agent research-agent")
defer span.End()
```

接收方如果不认识这个字段（比如不支持追踪的第三方 MCP server），会直接忽略，调用照常进行，只是树在这里断开。

## 5. OpenTelemetry 的组成

```text
业务代码 ──调用──▶ API（Tracer.Start / span.End）
                     │  只定义接口；没有安装 SDK 时是空操作
                     ▼
                  SDK：TracerProvider
                     ├─ Resource：这个进程是谁（service.name、process.pid）
                     ├─ Sampler：要不要记录这条 trace
                     └─ SpanProcessor ──▶ Exporter ──▶ 文件 / OTLP / 控制台
                         ├─ Simple：span 一结束就同步导出
                         └─ Batch：攒一批后在后台导出
                                              │ OTLP（gRPC 或 HTTP/protobuf）
                                              ▼
                              Collector（可选：过滤、采样、转发）
                                              ▼
                              后端：Jaeger、Tempo、Langfuse、Phoenix……
```

- **API 与 SDK 分离**：库只依赖 API，不决定数据去哪；应用安装 SDK 后才真正记录。这样一个库被不同应用使用时不会强加导出方式。
- **Simple 与 Batch**：Simple 在 span 结束时同步导出，延迟低、实现简单，但每个 span 都要等一次 I/O；Batch 在后台按批发送，不拖慢业务，代价是进程异常退出时会丢掉缓冲区里的 span，正常退出时必须调用 `Shutdown` 刷新。本地文件适合 Simple，网络导出通常用 Batch。
- **OTLP** 是 OTel 自己的传输协议。各家后端都能接收，所以换后端只需改环境变量：`OTEL_EXPORTER_OTLP_ENDPOINT`、`OTEL_SERVICE_NAME` 等由 SDK 自动读取。
- **Resource** 描述产生数据的进程。同一个 trace 里的 span 可能来自好几个 service，后端按 `service.name` 区分它们。

## 6. GenAI 语义约定

不同框架如果各自命名，同一个后端就无法统一展示“模型调用”和“工具调用”。OTel 的 GenAI 语义约定给出了统一名字。截至 2026 年，它仍处于 **Development** 状态，版本之间可能改名，规范已迁到独立仓库 `semantic-conventions-genai`。

**操作与 span 名**：span 名是“操作名 + 对象”。

| 操作 `gen_ai.operation.name` | span 名 | kind |
| --- | --- | --- |
| `chat` | `chat {模型}` | CLIENT |
| `embeddings` | `embeddings {模型}` | CLIENT（远程）或 INTERNAL（本地推理） |
| `invoke_agent` | `invoke_agent {agent 名}` | 进程内 INTERNAL；远程 agent 服务 CLIENT |
| `execute_tool` | `execute_tool {工具名}` | INTERNAL |
| `retrieval` | `retrieval {数据源}` | |
| `invoke_workflow` | `invoke_workflow {流程名}` | |

**常用属性**：`gen_ai.provider.name`、`gen_ai.request.model`、`gen_ai.usage.input_tokens` / `output_tokens` / `cache_read.input_tokens`、`gen_ai.conversation.id`、`gen_ai.agent.name` / `id`、`gen_ai.tool.name` / `call.id` / `type`，失败时加通用的 `error.type`。

**内容默认不采集**：`gen_ai.input.messages`、`gen_ai.output.messages`、`gen_ai.tool.call.arguments` / `result` 都是需要显式开启的属性。提示词和工具结果可能含个人信息、密钥或公司数据，而追踪系统常被更多人访问、保存更久。开启时还应截断。

**标准指标**：`gen_ai.client.operation.duration`（耗时直方图）、`gen_ai.client.token.usage`（token 直方图），按操作、模型、`error.type` 分组。

**会话与交互**：社区普遍把“一次用户交互”作为一个 trace，把同一段对话的多个 trace 用 `gen_ai.conversation.id` 归到一起。一个对话可能持续几小时、跨多次进程重启，把它塞进一个 trace 会让 trace 过长、根 span 迟迟不能结束。

**基数**：`error.type`、span 名这类用于分组的值应当只有少数几种（低基数）。把错误原文、用户问题放进 span 名，会让后端产生无数个“操作”，统计失去意义。具体原因放在状态描述或属性里。

## 7. 两条路线：代理与进程内打点

| | 代理 / 网关 | 进程内打点 |
| --- | --- | --- |
| 做法 | 把模型的 base URL 指向代理，代理记录请求和响应 | 代码里创建 span，SDK 在后台导出 |
| 能看到 | 模型调用的完整原文、状态码、耗时 | 模型、工具、检索、子进程的结构和耗时 |
| 看不到 | 工具执行、进程内步骤、父子关系 | 默认不含原文（需开启） |
| 改动 | 只改一个地址 | 改代码 |
| 代表 | Helicone | Langfuse、LangSmith、Phoenix、OpenAI Agents SDK |

只用代理时，工具耗时只能用“两次模型请求之间的间隔”来估，其中还混着 agent 自己的处理时间。两者可以并存：span 负责结构和耗时；代理保留原始请求，用于回放和逐字核对。并存时要用 `traceparent` 头把两边对上。

社区实现举例：

- **OpenAI Agents SDK** 默认开启追踪，span 类型有 Agent、Generation、Function、Handoff、Guardrail，父子关系靠 contextvar 自动维护，后台批量上传。
- **Claude Code** 直接导出 OTLP：span 树是“一次交互 → 模型请求、工具 → 工具执行、等用户审批、子 agent”；给 Bash 子进程设 `TRACEPARENT`；也读取传入的 `TRACEPARENT`，挂到调用方的 trace 下；提示词、工具参数、工具输出、原始请求体各有开关，默认关闭。
- **Langfuse、Phoenix** 的数据模型是 Session（对话）→ Trace（一次请求）→ Observation（span、模型调用、事件），可以给 trace 打评分，连接评测。

## 8. 从 span 汇总指标

不另起一套采集，直接从 span 计算：

```text
失败率 = 状态为 error 的 span 数 / 同组 span 总数
P95   = 把同组耗时从小到大排序，取第 ceil(0.95 × n) 个（最近秩法）
```

**算例**：某工具 6 次调用的耗时（毫秒）排序后是 `[2, 3, 3, 4, 9, 950]`。

- P50：ceil(0.5 × 6) = 3，取第 3 个，3ms；
- P95：ceil(0.95 × 6) = 6，取第 6 个，950ms；
- 平均值约 162ms，被一次慢调用拉高，既不代表典型情况也不代表最坏情况。

样本只有 6 个时，P95 就是最大值，读数一定要和次数一起看。生产系统通常用直方图（按桶计数）在服务端聚合，不保存每个样本。

## 9. 边界与取舍

- **采样**：高流量时只保留一部分 trace。头部采样在开始时决定，便宜但可能漏掉恰好失败的那次；尾部采样等 trace 结束后再决定（比如保留所有失败和慢请求），需要 Collector 先缓存整条 trace。
- **强制退出**：还没结束的 span 不会导出。长任务可以定期写检查点式的事件，或者在关键阶段拆出较短的 span。
- **隐私与体积**：内容采集默认关闭；开启时截断；追踪数据也需要保留期限和访问控制。
- **时钟**：不同机器的时钟有偏差，跨机器的 span 起止时间可能“子比父早”。同一台机器上的多个进程共用时钟，问题较小。
- **约定变化**：GenAI 约定仍在开发中，属性名可能改；只依赖核心的少数属性，并固定所用的约定版本。

## 10. 自测

**1. 为什么 agent 的排查需要 trace，而不是多打一些日志？**
日志分散在多个进程，并行时交错，难以还原谁调用了谁。trace 用 trace_id 把所有进程的记录串起来，用 parent_span_id 还原父子关系，并精确给出每一段的起止时间。

**2. 父 span 的耗时为什么不等于子 span 耗时之和？**
并行的子 span 时间重叠，总和会大于父 span；父 span 自身也有处理时间，子 span 之间的空白就是没有被记录的时间。

**3. 子进程怎样挂到父进程的 span 下？**
父进程把当前 span 编码成 `traceparent`，通过环境变量 `TRACEPARENT` 传给子进程；子进程启动时取出它，作为自己根 span 的远程父节点。

**4. 为什么网络导出用 Batch，本地文件可以用 Simple？**
网络发送慢且可能失败，同步等待会拖慢业务；Batch 在后台按批发送，代价是异常退出时会丢缓冲区，正常退出要 Shutdown。本地追加一行很快，同步写也不影响业务，还能让查看工具立即读到。

**5. 为什么提示词和工具结果默认不进 span？**
它们可能含敏感数据；追踪系统通常访问面更广、保存更久，而且大文本会显著增加存储成本。需要时显式开启并截断。

**6. 进程被强制杀死后，trace 会是什么样？**
还没结束的 span（通常包括根 span 和正在执行的工具）没有导出，已经结束的子 span 找不到父节点，成为孤儿。查看工具应该把它们单独标出来。

**7. 为什么一个对话的多次交互不放进同一个 trace？**
对话可能跨越很长时间和多次进程重启，单个 trace 会过长、根 span 迟迟不能结束；每次交互一个 trace，再用 `gen_ai.conversation.id` 归组，查询和展示都更清楚。

**8. 6 个样本的 P95 怎样读？**
按最近秩法，第 ceil(0.95×6)=6 个，也就是最大值。样本少时 P95 不稳定，要同时报告次数。

## 参考

- OpenTelemetry，[Observability primer](https://opentelemetry.io/docs/concepts/observability-primer/)、[Traces](https://opentelemetry.io/docs/concepts/signals/traces/)、[Context propagation](https://opentelemetry.io/docs/concepts/context-propagation/)
- OpenTelemetry Go，[Getting started](https://opentelemetry.io/docs/languages/go/getting-started/)；[go.opentelemetry.io/otel](https://pkg.go.dev/go.opentelemetry.io/otel)
- W3C，[Trace Context](https://www.w3.org/TR/trace-context/)
- OpenTelemetry，[GenAI semantic conventions 仓库](https://github.com/open-telemetry/semantic-conventions-genai)；[MCP 语义约定](https://opentelemetry.io/docs/specs/semconv/gen-ai/mcp)
- Dash0，[OpenTelemetry GenAI semantic conventions explained](https://www.dash0.com/knowledge/opentelemetry-genai-semantic-conventions-explained)，2026
- Model Context Protocol，[SEP-414：在 _meta 中传播 trace context](https://modelcontextprotocol.io/seps/414-request-meta)
- Anthropic，[Claude Code Monitoring](https://code.claude.com/docs/en/monitoring-usage)
- OpenAI，[Agents SDK Tracing](https://openai.github.io/openai-agents-python/tracing/)
- Langfuse，[Glossary](https://langfuse.com/docs/glossary)、[OpenTelemetry 接入](https://langfuse.com/docs/opentelemetry/introduction)
- Morph，[Langfuse vs Helicone](https://www.morphllm.com/comparisons/langfuse-vs-helicone)（代理与 SDK 两条路线对比）
- OpenTelemetry，[Deprecating Span Events API](https://opentelemetry.io/blog/2026/deprecating-span-events/)，2026
- Jaeger，[jaegertracing.io](https://www.jaegertracing.io/docs/)

本课程的代码实现、动手步骤与实验记录见 [Day 13 项目实践](day-13-lab.md)。
