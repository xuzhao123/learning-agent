# 15 · 观测：先看清 Agent 发生了什么

观测回答“发生了什么”：模型看到了什么、调用了哪些工具、慢在哪、花了多少。“做得好不好”是评测的事，见 [18 · 评测与失败归因](18-evaluation-and-attribution.md)；界面怎样实时拿到这些过程，见 [19 · Agent 与界面的协议](19-agent-client-protocol.md)。三者需要关联，却不能互相代替。

[阅读目录](README.md) · [技术展开](#技术展开) · [基础自测](#自测)

## 先认识术语

| 术语 | 通俗解释 |
| --- | --- |
| Trace | 用 ID 串起任务、模型、工具与子任务的执行链 |
| Span | 链路里一项有起止时间的操作 |
| Log | 记录事件与状态的文字或结构化信息 |
| Metrics | 汇总很多次执行的数值 |
| Turn | 一次用户提问及其执行过程 |
| Step | 用户回合内的一次任务模型调用与工具执行 |

Turn、Step 是本书选用的展示语义，不保证所有产品使用同样的命名。

## 一条轨迹至少要关联什么

```text
用户回合
  ├─ 模型请求：实际输入、工具定义、输出、用量
  ├─ 工具调用：名称、参数、ID、尝试、状态与结果
  ├─ 摘要 / 记忆辅助请求：用途、输入、输出与成本
  ├─ 子任务：父子关系、自己的过程与交付
  └─ 结束：停止原因、完成结果、未确认动作
```

模型提出了工具请求，不代表已经执行。工具的成功、失败、未执行和结果未知都应被记录。

只代理模型请求能看到消息怎样往返，但任务在下一次请求前停止时，某些工具结果还没发给模型。完整观察还要结合终端或执行事件，不能把代理数据当作所有外部动作的完整审计。

## 事件与输入快照为什么不应混着数

假设同一个用户问题经历检索、计算、回答：

```text
Step 1 输入：system、user
Step 2 输入：上面的历史 + 检索请求 + 检索结果
Step 3 输入：上面的历史 + 计算请求 + 计算结果
```

Step 3 的输入可能有六条消息，但主表并没有发生六个新事件。旧消息是这一次重发的上下文，不是用户再次提问。

界面可以在主表展示新事件，在每个 Step 的详情显示实际完整输入。这样既能数清步骤，也能核查模型究竟看到了什么。

压缩请求应单独标用途，保留下来的旧用户原话不应被误认成新的 Turn。

## 原始输入输出有什么价值

摘要卡片适合快速看懂，原始数据适合核查字段与消息关系：工具定义是否存在、调用 ID 是否配对、响应是否截断、用量是否包括辅助调用。

展示中的“相关”“成功”“绕路”常由规则推断，应让人看到判断依据。存在引用不等于事实正确，输出了一段思考也不等于执行成功。

原始数据可能包含业务秘密。不要只去掉认证头就认为日志安全；消息、工具参数、结果与记忆也需要权限、脱敏和保留期限。

## 成本与延迟看哪一层

主请求少了一次，可能只是把工作移到摘要、记忆背景或子任务。应把相关辅助成本一起看。

向量请求、模型请求和外部服务可能使用不同计量单位，应分别记录后按规则计算，不盲目相加。

耗时要区分用户等待、模型等待、工具执行、队列等待和空闲。并行工具总耗时不能把各自时长简单累加。非流式只知道请求总耗时时，也不能凭空推断首 token 时间。

## 指标的数据从哪里取

| 来源 | 优点 | 缺点 |
| --- | --- | --- |
| 持久化任务状态（检查点） | 每个边界同步写，进程被杀也在；有最终结局 | 只有任务级信息 |
| 调用链（trace） | 每一步的耗时、父子关系、重试 | span 结束时才导出，崩溃时正在执行的部分丢失 |
| 界面事件 | 实时、结构化 | 只覆盖经过这个界面的任务 |
| 日志 | 最全 | 文本，统计前要解析 |

崩溃时丢的恰恰是失败的那部分。只从调用链统计成功率，崩溃的任务没有根 span，既不进分子也不进分母，成功率被高估。所以结局、耗时、成本以持久状态为准，调用链只用来回答“慢在哪一步、哪个工具在出错”。

## 技术展开

### 进阶术语：事件账本、输入快照、统计单位与指标基数

| 术语 | 含义 |
| --- | --- |
| 事件账本 | 按顺序记录真正发生的事件，而非重复历史 |
| 输入快照 | 本次提交的完整messages、tools和参数 |
| 统计单位 | 一次任务、一次请求或一次尝试等被计数对象 |
| 指标基数 | 某个指标标签组合的数量，任务ID等无限变化值会使它持续增长 |
| TTFT | 请求开始到首个模型token的时间，不能由非流式总耗时倒推 |

### 事件结构要能恢复因果关系

结构示意：

```go
type TraceEvent struct {
    Seq        int64
    RunID      string
    TaskID     string
    ParentTask string
    Turn       int
    Step       int
    CallID     string
    Attempt    int
    Kind       string
    Purpose    string
    Body       json.RawMessage
}
```

需要 encoding/json。TaskID关联父子执行，CallID关联调用/返回，Attempt区分重试，Purpose区分主任务、摘要与重排。

时间戳有助于算耗时，不能单独作为跨进程因果顺序。序号可以提供接收顺序，还要通过ID和父子关系解释真正依赖。Trace/Span的基础关系见 [OpenTelemetry traces](https://opentelemetry.io/docs/concepts/signals/traces/)。

指标标签适合用provider、工具名、状态等有限集合；不要直接把每个TaskID、问题全文放进指标标签。它们会产生高基数，增加存储与查询成本；精确身份应保留在轨迹和日志的关联字段中。

### 重发历史不应重复计作新事件

比较连续请求的消息前缀，可以辅助观察新增输入：

```go
func commonPrefix(a, b []json.RawMessage) int {
    n := min(len(a), len(b))
    for i := 0; i < n; i++ {
        if !bytes.Equal(a[i], b[i]) { return i }
    }
    return n
}
```

需要 encoding/json、bytes。该片段比较字节；要得到语义比较需规范化完整消息，不能只比较content而忽略tool_calls或其他字段。

“前缀一致”不是供应商缓存命中的证明；tools和请求配置还在messages之外。压缩重建后，前缀很短也不表示全部历史都是新用户事件。

展示归属应优先由start/continue、任务关系、模型/工具ID与压缩用途决定，完整输入独立显示为快照。

### 成本和延迟要按真实活动拆分

```text
总成本 = 主模型 + 摘要 + 记忆背景/重排 + 子任务
        + embedding + 其他计费服务
```

模型token、向量请求、计算资源可能不同单位，先分别计量，再按明确规则换算。总耗时还包括队列和等待，并行阶段取实际墙钟时间，不把各项span直接加起来。

非流式响应不能推断准确TTFT；第一次网络字节也未必就是第一段可见模型token。指标名称应对应实际能观测的事实。

### 调用链：span 树、上下文传播与两条采集路线

span 的最小字段是 `trace_id`、`span_id`、`parent_span_id`、名字、kind、起止时间、状态、属性和事件。同一进程内父节点放在 `context.Context` 里；跨进程把当前 span 编码成 W3C `traceparent`（`00-<32位trace_id>-<16位span_id>-<flags>`），经 HTTP 头、子进程环境变量 `TRACEPARENT` 或 MCP 的 `params._meta` 带过去，接收方取出后作为远程父节点。

```go
// 父进程：把当前 span 交给子进程
cmd.Env = append(os.Environ(), "TRACEPARENT="+carrier["traceparent"])
// 子进程：根 span 挂到父 span 下
ctx = propagator.Extract(ctx, propagation.MapCarrier{"traceparent": os.Getenv("TRACEPARENT")})
ctx, span := tracer.Start(ctx, "invoke_agent worker")
```

几个容易出错的窗口：

| 情况 | 现象 | 原因 |
| --- | --- | --- |
| 进程被强制杀死 | 子 span 找不到父节点 | SDK 只导出已结束的 span，根 span 还没结束 |
| 工具超时后执行器先返回 | 子 span 比父 span 结束得晚 | 工具 goroutine 仍在运行，正是“结果未知”的证据 |
| 并行工具 | 子 span 耗时之和大于父 span | 时间重叠 |
| Batch 导出未 Shutdown | 最后几个 span 丢失 | 缓冲区没有刷新 |

**代理与进程内打点**：代理只看得到模型请求的原文，工具耗时只能用前后两次请求的间隔去估；进程内 span 能看到工具、检索、子进程的结构，但默认不含原文。两者并存时用 `traceparent` 头把截获的请求和 chat span 对上。社区的 GenAI 语义约定统一了 `chat`、`execute_tool`、`invoke_agent` 等 span 名与 `gen_ai.*` 属性，内容属性需要显式开启。原理与算例见 [Day 13 笔记](../docs/day-13/day-13-notes.md)。

### 以持久状态为准：两种数据各管什么

```text
完成率、结局分布、耗时、成本  ← 持久化任务状态（检查点）：每个边界同步写，kill -9 也在
工具失败率、重试、慢在哪一步  ← trace：只导出已结束的 span，崩溃时正在执行的部分会丢
```

只从 trace 统计成功率会产生幸存者偏差：崩溃的任务没有根 span，既不进分子也不进分母。所以结局以持久状态为准，trace 只做归因，并在报告里注明工具层指标可能偏低（[Q19](../docs/deep-questions.md#q19)）。指标口径（完成与答对、分母、pass^k）见 [第 18 篇](18-evaluation-and-attribution.md#指标的分母与口径)，原理见 [Day 15 笔记](../docs/day-15/day-15-notes.md)。

### 对照已有实现

[observer.proxy、add、streamEvents](../internal/observer/server.go)记录请求、用途、任务与事件；[embedding请求](../internal/retrieval/embedding.go)另走向量接口，[检索实验](../internal/labs/rag_lab.go)提供现有固定对比。

Day 13 起，[internal/telemetry](../internal/telemetry/telemetry.go) 用 OpenTelemetry Go SDK 给模型请求、工具、检索、记忆、MCP、沙箱、浏览器、子 agent 和队列打 span，写到本地文件，可选 OTLP 导出；观测台每次交互一个 trace，“调用链”标签展示 span 树和由 span 汇总的次数、失败率、P50/P95。实现与实验见 [Day 13 实践](../docs/day-13/day-13-lab.md)。

Day 15 起，`go run . metrics`（[internal/metrics](../internal/metrics/metrics.go)）按上一节的分工统计完成率、异常结局与可恢复性、端到端延迟、成本和工具可靠性；检查点补记开始时间、累计用量与 trace_id。B0 起观测台读取 agent 的结构化协议通知，不再解析终端文本，见 [第 19 篇](19-agent-client-protocol.md)。

Day 16–17 的评测与归因见 [第 18 篇对照](18-evaluation-and-attribution.md#对照已有实现)。

Day 21 起另有审计日志（[audit.go](../internal/audit/audit.go)）：调用链用于排查耗时与错误，可采样、崩溃会丢 span；审计只记关键决定与有副作用的执行，只追加、带哈希链、写后落盘，并分清人、模型与程序。两者用 trace_id 与调用 ID 互相跳转，不互相替代，见 [17 · 权限、确认与审计](17-permissions-approval-audit.md#审计事后能说清谁批准了什么)。多租户补充后，调用链仍由平台共享，观测台只让租户看到自己对话的 trace。

仍然没有：采样、Metrics SDK 与告警、带 trace_id 的结构化日志（重试等事件目前挂在 span 上）。页面中的 LLM token 总量不能直接当作包含全部向量/资源的费用账单。上文的事件类型是技术学习内容，不表示都已自动采集。

### 进阶推演

1. 一个 trace 里有几个 span 没有父节点，却带着 parent_span_id，最可能是什么情况？  
   父 span 所在的进程还在运行（span 尚未结束），或者被强制杀死、没来得及导出。先看进程是否还活着，再看检查点或退出记录。

2. 只看代理截获的模型请求，能统计工具失败率吗？  
   不能准确统计。代理只看到模型请求的往返，工具结果要等下一次请求才出现；任务在下一次请求前停止时，最后的工具结果根本不会经过代理。要靠进程内的工具 span 或执行事件。

## 自测

1. 一次请求的 messages 有六条，是否等于本轮六个新事件？  
   不是。它是输入快照，可能包含此前已发生的多条历史。

2. 新策略少了两次主模型调用，一定更便宜吗？  
   不一定。要加上辅助请求、子任务、向量检索和其他外部成本。

3. 只从调用链统计任务成功率，为什么会偏高？  
   崩溃的任务没有根 span，既不进分子也不进分母；失败的样本恰好被漏掉。

## 延伸阅读

- Sigelman 等，[Dapper, a Large-Scale Distributed Systems Tracing Infrastructure](https://research.google/pubs/dapper-a-large-scale-distributed-systems-tracing-infrastructure/)（Google，2010）：trace、span、父子关系这套概念的源头。
- OpenTelemetry，[GenAI semantic conventions](https://github.com/open-telemetry/semantic-conventions-genai)：模型调用、工具执行、Agent 调用的 span 名与属性约定（已从主仓库迁出）。
- OpenTelemetry Blog，[AI Agent Observability – Evolving Standards and Best Practices](https://opentelemetry.io/blog/2025/ai-agent-observability/)（2025）：Agent 观测的现状与各框架的接入方式。
- Hamel Husain，[Your AI Product Needs Evals](https://hamel.dev/blog/posts/evals/)：前半部分讲为什么要先能看到每一条轨迹，才谈得上评测。

本项目课程：[观测台说明](../docs/observer/observer-notes.md) · [Day 13 笔记](../docs/day-13/day-13-notes.md) · [Day 13 实践](../docs/day-13/day-13-lab.md) · [Day 15 笔记 §6](../docs/day-15/day-15-notes.md)

[上一篇：权限、确认与审计](17-permissions-approval-audit.md) · [下一篇：评测与失败归因](18-evaluation-and-attribution.md) · [术语表](TERMS.md)
