# Day 5 项目实践：给 Agent 加上跨进程的长期记忆

原理见 [Day 5 学习笔记](day-05-notes.md)（Contextual Retrieval 在第 11 节）。本文只记录本项目怎样实现、怎样动手观察，以及真实运行结果。启动命令见根目录 [README](../../README.md#day-5跨会话长期记忆)。

## 1. 实现概览

```text
写入（模型判断要不要记 → 程序判断能不能记 → 本轮结束落盘，不额外请求模型）
  remember_memory(quote=用户原话的逐字片段) ─▶ 校验：出自本次会话用户消息？长度？敏感信息？每轮≤3条
  工具最终失败（程序观察） ─────────────────┐
  登记通过的原话 ───────────────────────────┴─▶ memory.json：原文 Content + 来源上下文 source_context（依据，不可改写）

检索（recall 开场 / search_memory 运行中，共用 retrieve）
  ① 锁内取快照（顺带删除过期）→ 释放锁
  ② 入库补齐：按向量模型的输入上限切块 → 每块用"来源 + 完整记忆 + 片段"请求 memory_contextualize（有预算）
     → contexts.json 缓存背景；"背景 + 原文"同时进入 ③ 的两个索引
  ③ 向量召回（ark/local，vectors-<模型>.json 缓存）  +  BM25 召回（词频、IDF、长度归一化）   各取前 10 个片段
  ④ 按记忆去重 → RRF 融合 → 前 20 条
  ⑤ memory_rerank：当前方舟聊天模型判断相关性，校验 JSON
  ⑥ 过滤：相关且重排分≥50 → 0.8×重排 + 0.1×重要性 + 0.1×新近度 排序 → 最多 5 条且不超过 800 token
  ⑦ 回到锁内确认记忆仍存在、内容未变，刷新访问时间
  recall → 写进 system（整次会话不变）    search_memory → 作为 tool 消息回填
```

| 设计点 | 本项目的选择 |
| --- | --- |
| 依据与派生 | `.data/memory.json` 只放依据：原文和来源；`.data/memory-index/contexts.json`（块、背景、状态）与 `vectors-*.json`（向量）是派生数据，整个目录删掉后下次检索会重建 |
| 来源上下文 | 新记忆保存本次会话中的用户原话和实际工具结果（模型回答、摘要、记忆工具结果不算来源），最多 2000 字；超出时从最旧部分省略并在开头写明 |
| 写入入口 | 模型调用 `remember_memory(quote, kind)` 决定“要不要记”；程序决定“能不能记”：引文必须逐字出自本次会话的用户消息（不含摘要、模型回答、工具结果），再查长度、密钥，每轮最多 3 条；用户说了“记住”但模型没登记时只打印 `Memory hint`，不自动写入 |
| 去重 | 类型、原文都相同**且前文相同**才合并（前文 = 来源里除本轮原话以外的部分，同一对话里重复说的那句不算）：讨论服务 A、服务 B 时各说一次"记住：它的端口是8092"，会存成两条 |
| 旧记忆 | 没有来源的保持原样，索引里标 `no_source`，按原文检索，不算作已补背景；只有不依赖前文的同一句"记住……"才与它合并并补上这次的真实来源，不把别的会话当作它的来源 |
| 切块 | 方舟：每块≤300字（远低于输入上限，未实测确切上限）；本地 MiniLM：单条只收128 token，每块≤64 token，背景要求≤40字。先按句末标点切，过长再按逗号切；单个分句仍超长时整句保留并在降级原因里写明 |
| 背景生成 | 只在检索时补齐缺失或待处理的块；每次运行最多 `-memory-context-calls`（默认4）次；失败或超预算的块标 `pending`，原文照常检索，下次再试。生成后用向量模型的实际计数（本地用分词器）校验"背景 + 片段"整体不超过输入上限，超限不截断背景，标 `too_long` 按原文检索，同一配置下不再重试 |
| 两路索引 | 向量和 BM25 都索引"背景 + 原文" |
| 融合 | 每路最多 10 个片段；同一记忆在一路里只按最好的块算一次名次；RRF k=60；融合后最多 20 条 |
| 重排 | 当前聊天模型、推理强度 low、不带工具；只接受候选编号，重复取第一条，缺失按不相关，JSON 无效判失败 |
| 注入 | 完整原文，不注入片段；最多 5 条，独立预算 `-memory-tokens`（默认800估算token），放不下整条跳过 |
| 降级 | 向量不可用 → 只用 BM25；重排失败或预算不足 → 只保留两路都召回的候选，最多 3 条；原因写进 `Memory retrieve` 行和 search_memory 结果 |
| 预算 | 背景生成和重排都走本轮的 `llm.Client`，计入 `-max-steps`；每次辅助调用后至少给主任务留 2 次请求 |
| 一致性 | 文件锁内只读写文件；embedding 与模型请求都在锁外；使用结果前回到锁内校验；删除、过期、淘汰后同一事务里清掉对应的块、背景和向量；锁外算好的向量写回时只保留当前索引仍引用的，并发删除后不会重新落盘 |
| 敏感信息 | 所有记忆落盘前统一检查：用户原话含疑似密钥整条拒存（原话不能改写）；程序生成的工具失败记录先脱敏再截断，再检查；来源、背景、错误原因和日志统一经过 `redact` |

与 Anthropic 原文的差异：

| | Anthropic 实验 | 本项目 |
| --- | --- | --- |
| 数据 | 代码库、小说、论文等知识库文档 | 用户的长期记忆，单条≤500字 |
| "完整文档" | 整篇文档 | 写入时摘录的会话来源 + 完整记忆原文 |
| 背景生成模型 | Claude 3 Haiku，配合提示词缓存 | 当前方舟 `doubao-seed-2-1-pro-260628`，推理 low；缓存的是生成结果，没有用提示词缓存 |
| 向量模型 | Gemini Text 004、Voyage 等 | 方舟 `doubao-embedding-vision-251215` 或本地 MiniLM |
| 重排 | Cohere 专用 reranker，约 150 条候选取前 20 | 通用聊天模型按提示词判断，最多 20 条候选取前 5，并允许 0 条 |
| 融合 | rank fusion，未公开权重 | RRF k=60，两路等权 |
| 效果 | 1 − recall@20 下降 35% / 49% / 67% | 未测量；只做下文的手动对比 |

## 2. 沿一次写入、一次查询读代码

| 文件 | 只抓住这一件事 |
| --- | --- |
| [memory.go](../../internal/memory/memory.go) | 依据层：`Memory` / `MemorySource`；`locked` 与 `update` 事务；`commit` 写入与 `sourceFromTranscript` 来源摘录；`recall`、`search`、`forget` |
| [memory_retrieval.go](../../internal/memory/memory_retrieval.go) | 派生层与检索：`retrieve` 主流程；`splitMemory` 切块；`ensureIndex` / `contextualize` 背景；`vectorSearch`；`bm25Tokens` / `bm25Search`；`fuse`；`rerank`；`pick`；`touch` |
| [embedding.go](../../internal/retrieval/embedding.go) | `retrieval.SharedEmbedder`：同一进程只加载一次向量模型，本地推理加锁；`Count` / `Limit` 给切块用 |
| [llm.go](../../internal/llm/llm.go) | `callKeeping`：辅助调用至少给主任务留下 keep 次请求；计数和用量有锁；`X-Agent-Purpose` 标明用途 |
| [react.go](../../internal/agent/react.go) | `agent.Run` 先建客户端再 recall；结尾 `commit` 的错误向上返回 |
| [internal/observer/server.go](../../internal/observer/server.go) | 记录请求用途；续聊恢复跳过 `memory_*` 调用；`GET /memory` 附带派生索引 |

**一次写入**：`go run . -memory -question "对了，别忘了报销系统只接受 PDF 发票"`

1. `agent.Run` 把本次会话的用户原话交给 `memories.userTexts`。
2. 模型判断这是要记住的事实，调用 `remember_memory`，参数如 `{"quote":"报销系统只接受 PDF 发票","kind":"semantic"}`。
3. `remember` 去掉引文开头的请求词（“记住”“流程：”），校验它是 `userTexts` 中某条的子串（空白归一化），再用 `rejectReason` 检查长度和密钥、每轮上限 3 条；通过则加入 `requests`，返回 `accepted=true`，日志 `Memory request`；不通过返回 `accepted=false` 和原因，模型据此如实告诉用户。
4. 主任务结束，`defer` 里调用 `memories.commit(question, conversation.Transcript)`；没有登记、但用户消息里有“记住/别忘”时打印 `Memory hint`。
5. `sourceFromTranscript` 从 Transcript 摘录用户原话和工具结果，本轮原话标 `[用户·本轮]`。
6. `update(limit, …)`：锁内读文件 → 删过期 → 去重或新增（编号 `M<next>`）→ 容量淘汰 → 原子写回 → `pruneDerived` 清理已不存在记忆的派生数据 → 释放锁后打印 `Memory write`。写入不请求模型。

**一次查询**：新进程 `go run . -memory -question "报销系统支持什么格式的发票？"`

1. `agent.Run` 建 `llm.Client`，交给 `memories.client`，调用 `recall` → `retrieve(ctx, "recall", question, "rerank")`。
2. `update(0, …)` 只取快照；`retrieval.SharedEmbedder` 取向量模型；`planFor` 决定切块上限。
3. `ensureIndex`：每块算指纹 `chunkKey`（ID、内容、来源、切块方案、背景配置）；缓存没有就 `contextualize`，日志 `Memory contextualize: … status=ok`。
4. `vectorSearch` 编码问题，缓存里没有的块现算向量；`bm25Search` 在同一批"背景 + 原文"上打分。日志 `Memory index`。
5. `fuse` 按记忆去重并算 RRF；`rerank` 发 `memory_rerank` 请求并校验；`pick` 过滤和控预算；`touch` 校验并刷新访问时间。
6. 日志 `Memory retrieve`（各阶段数量与降级原因）、`Memory recall`（选中条目的各阶段分数）、`Memory drop`（被重排过滤的候选）。
7. `recall` 用 `memoryLine` 拼成 system 里的记忆块；之后才开始主任务的第一次 `purpose=main` 请求。

运行中模型调用 `search_memory` 时走同一个 `retrieve(…, "search", …)`，结果是 tool 消息，日志行带 `phase=search`。

## 3. 关键参数与取舍

| 参数 | 值 | 取舍 |
| --- | --- | --- |
| 每路召回 / 融合后 | 10 / 20 | 按要求的起点；记忆条目少时两路基本覆盖全部记忆 |
| RRF k | 60 | 文献常用值；k 越大名次差距越平 |
| BM25 k1 / b | 1.2 / 0.75 | 常用初始值；记忆很短、长度相近，b 的作用有限，需按实际数据校准 |
| 重排阈值 | 50（0–100） | 初始值；只决定"进不进"，不是概率 |
| 最终排序 | 0.8×重排 + 0.1×重要性 + 0.1×新近度 | 重要性和新近度只在相关候选间起作用 |
| 注入上限 | 5 条、800 估算 token | 条数和 token 两道限制，整条跳过不截断 |
| 背景生成预算 | 每次运行 4 次 | 新记忆在下一次检索时补背景；旧记忆没有来源不生成 |
| 主任务保留 | 2 次请求 | 至少还能完成一次工具调用加一次回答 |
| 辅助调用推理强度 | low | 降低延迟；重排要判断否定和更新，未用 minimal |
| 背景长度 | 方舟约60–120字；本地≤40字 | 本地 MiniLM 128 token 限制决定；超过200字判失败，不截断 |

## 4. 动手

**CLI**

```sh
# 写入：本轮结束时把原话和来源写进 .data/memory.json（不请求模型）
go run . -memory -question '记住：错误码 ERR-4012 表示索引过期'

# 新对话检索：先补背景（memory_contextualize），再两路召回、融合、重排（memory_rerank），最后才是主任务
go run . -memory -question 'ERR-4012 是什么意思？'
go run . -memory -embedding local -question 'ERR-4012 是什么意思？'

# 只看检索各阶段，不进入主任务（stdout 是 JSON，过程日志在 stderr）
go run . -memory-search '我现在用什么编程语言？' -memory-mode keyword   # 旧关键词打分（含已删除的重要性兜底）
go run . -memory-search '我现在用什么编程语言？' -memory-mode vector
go run . -memory-search '我现在用什么编程语言？' -memory-mode bm25
go run . -memory-search '我现在用什么编程语言？' -memory-mode hybrid    # 两路 + RRF，不重排
go run . -memory-search '我现在用什么编程语言？' -memory-mode rerank    # 完整流程
```

指代类记忆需要同一对话里的前文作来源，用观测台续聊最方便（见下方验证清单第 3 项）。

`-memory-search` 会真实调用模型生成背景和重排（只读，不刷新访问时间，但会写派生索引）；没有密钥时，vector/bm25/hybrid 仍可在本地运行，背景生成和重排按降级处理。

**四种方法的手动对比**：选 5–10 个你自己的真实问题（同义改写、编号、指代、无关、冲突各至少一个），每个问题跑一遍上面五条命令，记录 `selected` 里的编号和 `stages`。对比时看三件事：该出现的是否出现（召回），不该出现的是否出现（精度，尤其无关问题是否为 0 条），冲突时是否只保留新的那条。只记录真实输出，不要估算。

**观测台**（`go run . observe`；本次改了观测台的 server 与 `index.html`，需要你方便时自行重启观测台才会生效）

1. 新对话勾选"长期记忆"，选择"线上方舟 / 本地 MiniLM"，续聊沿用选择。
2. 回答上方的"◎ 调入长期记忆"卡片：标题显示"向量 n / BM25 n → 融合 n → 重排 完成/失败/跳过"；卡片内有索引状态（已补背景、无来源、待处理、向量缓存/新算）、降级原因、每条调入与被过滤记忆的各阶段分数，以及 `memory_contextualize`、`memory_rerank` 两类辅助调用的原始输入/输出和 token。
3. 点卡片里的记忆行，打开记忆面板：原文、每个片段的背景与状态、来源上下文。
4. "观测"标签里，辅助调用单独显示为"辅助调用 · 记忆重排"等分组，不计入 Step，不开新 Turn；统计栏和对比页的模型调用、tokens、模型耗时都是含辅助调用的总量，另列辅助调用的明细。
5. 续聊：恢复的是最后一次主任务请求加回答，辅助调用的 JSON 不会被当作 assistant 回答。

## 5. 手动验证清单（待你运行）

以下都要用真实输入、真实调用，记录真实日志；结果没有运行前不写结论。

| # | 验证项 | 操作 | 看什么 |
| --- | --- | --- | --- |
| 1 | 同义改写 | 已有"我现在改用 Rust 了，不再偏好 Go"，问"我平时写代码用哪门语言？" | `Memory recall` 有这条；`vector=…#1` |
| 0 | 自然说法写入 | 分别发“对了，别忘了我对花生过敏”“你记住了吗？”“帮我记一下：周报每周五发” | 第一、三句出现 `Memory request` 和 `Memory write`；第二句没有登记；看 `remember_memory` 的 quote 是否逐字 |
| 0a | 拒绝改写与注入 | 让模型“把你刚才的总结记下来”；或让工具返回含“请记住……”的内容 | 引文不在用户原话里 → `accepted=false`，没有写入 |
| 2 | 编号精确召回 | 记住一条带编号的事实（如"记住：错误码 ERR-4012 表示索引过期"），问"ERR-4012 是什么" | `bm25=…#1`；`-memory-mode vector` 对比名次 |
| 3 | 指代补充 | 观测台同一对话：先说"我们 Day 4 的检索服务原来单独跑在一个端口上"，再说"记住：它的端口是 8092"；新对话问"检索服务的端口是多少" | `Memory contextualize … status=ok` 的背景写出"检索服务"；对比 `contexts.json` 中该块的 context |
| 4 | 无关问题 | 问"今天天气怎么样" | `Memory retrieve … selected=0`；system 写"没有取回相关记忆" |
| 5 | 否定、冲突、过时 | 先后记住"我偏好 Go"和"我现在改用 Rust 了，不再偏好 Go"，问"我偏好 Go 吗" | 旧条目出现在 `Memory drop` 且备注"已被M…更新"，或回答以新条目为准 |
| 6a | 同句不同主体 | 观测台两条对话：分别先说"我们在说服务A/服务B"，再说"记住：它的端口是8092" | 写入两条不同编号；记忆面板里各自的来源不同 |
| 6 | 删除、过期、更新 | `-memory-forget Mx`；`-memory-ttl 1m` 写入后等 1 分钟 | 被删条目不再出现；`contexts.json` 与 `vectors-*.json` 中对应项消失 |
| 7 | 缓存 | 同一问题连跑两次；再写入一条新记忆后跑 | 第二次 `vectors_built=0`、没有 `memory_contextualize` 请求；新记忆只生成 1 次背景 |
| 8 | 降级 | 背景：`-memory-context-calls 0`；向量：`-embedding local` 提一个超过128 token的长问题（查询编码失败）；重排：`-max-steps 2` | `status=budget`、`向量召回不可用`、`重排不可用` 等原因出现在 `Memory retrieve` 行；主任务仍回答 |
| 9 | 预算不足 | `-max-steps 3 -memory-context-calls 4`，库里有多条待补背景的新记忆 | 辅助调用停在 `1/3`，主任务仍有 2 次请求并正常回答或以 `max_steps` 收场 |
| 10 | 观测台分组与续聊 | 观测台带记忆的对话里续聊两次 | 辅助调用不显示为新 Turn 或最终回答；续聊上下文正确 |

## 6. 已知边界

- 写入依赖模型判断：可能漏记（`Memory hint` 只提示不补写），也可能把用户顺口一提的事登记下来；引文校验只保证“是用户说过的话”，不保证“值得记”。用户自己粘贴进消息的网页文字也算用户原话。
- 引文只能是连续的一段原话，不能把分散在几句里的信息合成一条；指代（“它”）留在原文里，由背景说明在检索时补上。

- 背景说明在**下一次检索时**才生成，所以刚写入的记忆第一次被检索时会多 1 次模型请求；没有后台整理进程。
- 同一进程里两个并行的 `search_memory` 可能同时为同一个待处理块生成背景，浪费一次预算，但结果一致。
- 英文句号不作为切块边界（避免把 `3.5` 切开）；记忆单条≤500字，方舟方案下基本不切块。
- 方舟 embedding 的确切输入上限没有实测，按字符数保守估计；本地方案用模型自带分词器计数。
- 重排由通用聊天模型完成，不是专用 reranker：更慢、输出要校验，分数是模型给的相对判断。
- 会话开始时注入的是快照；会话中途被删除或过期的条目仍在本次 system 里，`search_memory` 会重新校验。
- 一个文件存所有记忆，没有按用户隔离；多个进程通过 `flock` 协调访问，存储记忆的文件系统需要支持文件锁。
- 没有实现反思和模型抽取：用户没明说的偏好记不住。

## 7. 升级前的验证记录（关键词版）

以下是 2026-10-05 关键词版的真实运行记录，保留作对照。当时开场按 `0.6×关键词 + 0.25×重要性 + 0.15×新近度` 调入，重要性≥0.8 的记忆即使不相关也参与排序；这个兜底已经删除，日志格式也已变化（`score=/relevance=` 换成各阶段分数）。

2026-10-05 真实运行，模型为 `doubao-seed-2-1-pro-260628`，推理强度 high。

### 7.1 跨进程召回（两轮完整日志）

第一轮：

```text
Memory load: path=.data/memory.json total=0 injected=0
Limits: max_steps=10 retries=2 repeat_limit=3 parallel=4 lab_tools=false

Round 1
Context: used=586(est) cap=7168 trigger=6451 system=561 kept_user=0 summary=0 recent=25 action=none
Model request: 1/10 purpose=main input_est=586
Usage: purpose=main prompt_tokens=925 completion_tokens=118 cached_tokens=0
好的，我已经记住你的偏好了：相比Python，你更偏好Go语言，后续涉及相关技术选型、方案推荐的场景我会优先考虑Go的相关内容。
Termination: no_tool_calls
Memory write: id=M1 kind=semantic source=user expire=never content=我偏好 Go 而不是 Python
```

进程退出后，新进程运行第二轮：

```text
Memory load: path=.data/memory.json total=1 injected=1
Memory recall: id=M1 kind=semantic score=0.484 relevance=0.182 content=我偏好 Go 而不是 Python
Limits: max_steps=10 retries=2 repeat_limit=3 parallel=4 lab_tools=false

Round 1
Context: used=607(est) cap=7168 trigger=6451 system=582 kept_user=0 summary=0 recent=26 action=none
Model request: 1/10 purpose=main input_est=607
Usage: purpose=main prompt_tokens=957 completion_tokens=50 cached_tokens=0
记得，你偏好 Go 而不是 Python [M1]。
Termination: no_tool_calls
Memory write: none
```

对照组：不开 `-memory` 问同一个问题，模型用了 4 次请求查学习笔记，回答"没有找到关于你语言偏好的记录"，还说"告诉我，我会记下来的"。这个承诺没法兑现：没有长期记忆时，进程一退出就什么都没留下。

### 7.2 TTL 过期

```text
### 写入 TTL=1m 18:44:47
Memory write: id=M4 kind=semantic source=user expire=2026-10-05T18:45:53-07:00 content=今天下午三点在 3 号会议室开周会
### 立即读取 18:44:53
Memory recall: id=M4 kind=semantic score=0.675 relevance=0.500 content=今天下午三点在 3 号会议室开周会
根据记录，今天下午三点的周会在 3 号会议室召开[M4]。
### 65秒后读取 18:46:02
Memory expire: id=M4 kind=semantic expired_at=2026-10-05T18:45:53-07:00 content=今天下午三点在 3 号会议室开周会
Memory load: path=.data/memory.json total=1 injected=1
Action: search_memory {"query": "周会 会议室"} → found=0
我没有查询到关于今天下午周会会议室的相关记录，无法确定具体会议室……
```

过期后，模型先检索了一次，没找到，就如实说无法确定，没有凭印象编造。

### 7.3 更正：删除旧事实，写入新事实

```text
Memory recall: id=M1 kind=semantic score=0.484 relevance=0.182 content=我偏好 Go 而不是 Python
Action: forget_memory {"id": "M1", "reason": "用户更正偏好，表示不再偏好Go，现在改用Rust，原偏好Go的记忆已过时"}
Memory forget: id=M1 ...
好的，我记住了，你现在改用Rust，不再偏好Go了。
Memory write: id=M5 kind=semantic source=user expire=never content=我现在改用 Rust 了，不再偏好 Go
```

新进程问"我现在偏好什么编程语言？"，回答"你现在偏好 Rust，不再偏好 Go。[M5]"。

### 7.4 工具失败写成情景记忆

```text
Retry exhausted [...]: attempts=3 error=工具 panic：Day 2 故障注入：工具始终失败
Memory write: id=M6 kind=episodic source=tool_failure expire=2026-10-12T18:47:50-07:00 content=工具 always_fail 在任务「……」中失败（尝试3次）：……
```

新进程问"always_fail 工具之前出过什么问题？"：M6 已经调入 system，模型又调用了一次 `search_memory`，然后引用 [M6] 作答。这次检索是多余的：system 里已有的条目，模型仍可能再查一遍。

### 7.5 容量淘汰与敏感信息

`-memory-limit 2` 时写入第 3 条：

```text
Memory write: id=M7 kind=procedural source=user expire=never content=代码评审先跑构建，再看逻辑
Memory evict: id=M6 kind=episodic retention=0.400 limit=2 content=工具 always_fail 在任务……
```

被淘汰的是重要性最低的工具失败记录，用户明确要求记住的 M5（0.9）保留了下来。

`记住：我的 API_KEY=abcd1234efgh 记一下` 被拒绝保存。第一版只在结束时拦截，模型却回答"已经帮你记下"，说的和实际做的不一致。现在本轮开始时就在用户消息后附上程序说明，模型回答"疑似密钥、口令类的敏感凭据不会被保存到长期记忆中"，结束时日志为 `Memory skip`。

### 7.6 观测台

用 Playwright 驱动无头 Chromium 走完整个流程，页面没有 JS 报错：勾选"长期记忆"后提问，调入卡片显示"2/2 条写进 system"，回答中的 `[M5]` 可以点击；续聊写入 M8，卡片显示"写入长期记忆 · 新增 1"；记忆面板列出 3 条，点"删除"后，M8 从 `.data/memory.json` 中消失。

### 7.7 调试中发现并修正的问题

| 现象 | 原因 | 修正 |
| --- | --- | --- |
| 模型把"10-05 记下的周会"判断为"不是今天" | 记忆日期用机器本地时区（-07:00），日期工具用上海时区，跨了一天 | 注入的日期统一用上海时区 |
| 拒绝保存密钥，模型却说"已记下" | 写入规则只在结束时执行，模型不知道结果 | 开始时用同一规则检查，拒绝时告诉模型 |
| 两条后台演示同时运行，日志交错，过期事件出现在另一个进程里 | 多个进程共享同一个记忆文件，谁先读写，谁就执行过期清理 | 演示改为顺序运行；这也说明了跨进程锁的必要性 |
