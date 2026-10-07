# 学习 agent：Day 1 → Day 7（Week 1）

想先看动画讲解，打开 [课件入口](docs/slides/index.html)：每节课一份可翻页、按步骤播放的 HTML 课件。

每天只读一篇笔记：[Day 1 工具调用与循环](docs/day-01/day-01-notes.md)、[Day 2 循环护栏](docs/day-02/day-02-notes.md)、[Day 3 上下文管理](docs/day-03/day-03-notes.md)、[Day 4 按需检索与引用](docs/day-04/day-04-notes.md)、[Day 5 长期记忆](docs/day-05/day-05-notes.md)、[Day 6 MCP client](docs/day-06/day-06-notes.md)、[Day 7 MCP server](docs/day-07/day-07-notes.md)、[Bonus Agent Skills](docs/bonus-skills/skills-notes.md)。笔记只讲知识本身（原理、通用例子、权衡与自测）；本项目的代码阅读、动手步骤与实验记录在同目录的项目实践：[Day 1](docs/day-01/day-01-lab.md)、[Day 2](docs/day-02/day-02-lab.md)、[Day 3](docs/day-03/day-03-lab.md)、[Day 4](docs/day-04/day-04-lab.md)、[Day 5](docs/day-05/day-05-lab.md)、[Day 6](docs/day-06/day-06-lab.md)、[Day 7](docs/day-07/day-07-lab.md)、[Skills](docs/bonus-skills/skills-lab.md)。第一周的总结与架构图见 [Week 1 复盘](docs/week-01-review.md)。

用 Go 手写最小 ReAct loop，重点是看懂流程：

`问题 → 选择本轮上下文 → 请求模型 → 读取 tool_calls → 并行执行 → tool 结果写回历史 → 下一轮或结束`

使用方舟原生 Tool Calling。普通模式的 system prompt 只说明工具助手角色；Day 4 的 `-rag` 模式补充检索、引用与资料不足规则；Day 5 的 `-memory` 模式补充记忆规则与开场调入的记忆块。回答仍可自由表达，工具定义通过 `tools` 提供，调用参数由模型生成。

## 运行

使用 Go 1.25.5 以上版本。以下命令统一在项目根目录（包含 `go.mod` 和 `main.go` 的目录）执行，文档中的文件路径均相对于项目根目录。

首次配置：复制 [.env.example](.env.example) 为 `.env`，填写自己的 `ARK_API_KEY`，然后运行：

```sh
go run .
```

也可以从命令行提供问题：

```sh
go run . -question '计算 (1234×5678) 开根号取整，并查询当前日期时间。两项独立，请同轮调用。'
go run . -question '先查学习笔记中的循环职责，再计算职责数量乘以25。'
```

当前参数：

| 参数 | 默认值 | 用途 |
| --- | --- | --- |
| `-question` | 交互输入 | 交给模型的任务 |
| `-history-stdin` | false | 从stdin接收上次上下文JSON，配合-question续聊；观测台自动处理 |
| `-parallel` | 4 | 工具执行并发，1到16 |
| `-max-steps` | 10 | 模型请求总预算，包含最终回答与上下文摘要 |
| `-retries` | 2 | 工具额外重试次数，0到5 |
| `-lab-tools` | false | 显式开启 Day 2 故障工具 |
| `-context-window` | 12288 | 配置的总窗口，学习实验值 |
| `-max-output-tokens` | 0 | 0省略API输出上限；大于0才传max_completion_tokens（含推理） |
| `-reasoning-reserve` | 1024 | 输入容量额外安全余量 |
| `-tool-output-tokens` | 2000 | 工具结果进入View时的粗估上限 |
| `-keep-groups` | 2 | 最近组超20%配额或挤占摘要空间时逐步减少，可到0 |
| `-reasoning-effort` | high | 所有请求统一的推理强度：minimal、low、medium、high |
| `-context-lab` | false | 33轮召回与约束保留，再运行真实大工具输出实验 |
| `-rag` | false | 增加 search_docs，项目知识回答先检索并引用来源 |
| `-embedding` | ark | 向量模型：ark线上方舟，local本地纯Go MiniLM；search_docs 与长期记忆共用 |
| `-min-score` | 0.40 | 检索相似度阈值，范围−1到1 |
| `-search-docs` | 空 | 只执行检索，不调用聊天模型；local无需密钥，ark使用同一ARK_API_KEY |
| `-k` | 3 | 配合 search-docs，最多返回1–6条候选 |
| `-rag-lab` | false | 10题直接检索检查，再做20次独立真实问答；max-steps对每题每组分别生效 |
| `-memory` | false | 跨进程长期记忆：开头经背景补充、两路召回、RRF、重排后调入，结尾写入原文与来源，增加 remember_memory、search_memory 与 forget_memory |
| `-memory-ttl` | 0 | 本轮“记住”写入的有效期，如 `1m`；0 不过期 |
| `-memory-limit` | 200 | 记忆总量上限，超出时淘汰重要性×新近度最低的一条 |
| `-memory-forget` | 空 | 只删除指定编号的记忆后退出，如 `M3`；不做容量淘汰，不需要密钥 |
| `-memory-search` | 空 | 只执行一次记忆检索，stdout 输出各阶段 JSON；配合 `-memory-mode` 对比 |
| `-memory-mode` | rerank | memory-search 的方式：keyword（旧关键词）、vector、bm25、hybrid（RRF）、rerank（完整流程） |
| `-memory-context-calls` | 4 | 本次运行最多几次背景生成；0 表示全部按原文检索 |
| `-memory-tokens` | 800 | 注入或返回的记忆估算 token 预算，另有最多5条的上限 |
| `-mcp-server` | 空 | MCP server：`http(s)://…` 地址连远程 server（Streamable HTTP），否则当作子进程命令（stdio，按空格切分，不经过shell）；可重复给多个；配合 `-question` 时把它们的工具注册为 `mcp_*`，连不上的 server 打印 `MCP error` 后跳过；`-mcp-list`/`-mcp-raw` 只接受一个 |
| `-mcp-list` | false | 只用 SDK 连接 `-mcp-server`，打印协议版本、capabilities 与工具列表，不请求模型 |
| `-mcp-raw` | 空 | 不用 SDK，手写 JSON-RPC：`legacy`（initialize 握手）或 `modern`（server/discover + 每请求 `_meta`），逐行打印收发 |
| `-mcp-call` / `-mcp-args` | 空 / `{}` | 配合 `-mcp-list` 或 `-mcp-raw` 再调用一次指定工具 |
| `-mcp-serve` | false | 作为 MCP server（stdio）运行，暴露 calculator；须单独使用 |
| `-mcp-http` | 空 | 配合 `-mcp-serve`，改用 Streamable HTTP 在该地址提供 `/mcp`（远程 MCP），如 `127.0.0.1:8091` |
| `-skills` | false | 扫描 `skills/*/SKILL.md`，system 中放索引，增加 load_skill |
| `-skill` | 空 | 配合 `-skills`，只启用这个名字的 skill（可重复）；不给则启用全部 |
| `-skills-list` | false | 只扫描 `skills/`，stdout 输出每个 skill 的元数据、正文与校验错误（JSON），不请求模型；须单独使用 |

工具失败按200ms起步指数退避，耗尽后将错误回填；连续第三次相同动作拦下整批。超限或熔断会输出未完成与已执行步骤摘要。详见 [Day 2 学习笔记](docs/day-02/day-02-notes.md)。

## 源码阅读

| 文件 | 阅读重点 |
| --- | --- |
| [main.go](main.go) | 输入问题、读取方舟配置、启动任务 |
| [llm.go](llm.go) | 发送消息历史与 tools，接收真实方舟模型消息 |
| [react.go](react.go) | 原生调用结构、并行调度、tool 结果回流、终止 |
| [tools.go](tools.go) | 工具 Schema、实际执行、实际笔记检索 |
| [context_manager.go](context_manager.go) | Transcript/View、usage用量、集中清理与摘要 |
| [context_lab.go](context_lab.go) | 真实33轮召回、约束与大工具输出观察 |
| [retrieval.go](retrieval.go) | search_docs定义、进程内索引、点积排序、低分过滤与文件缓存 |
| [rag_lab.go](rag_lab.go) | 10题真实检索与无检索/有检索对比；不写死模型回答 |
| [embedding.go](embedding.go) | 本地纯Go推理 / 线上方舟embedding |
| [memory.go](memory.go) | 长期记忆依据层：原文与来源上下文、文件锁事务、规则写入、过期与淘汰、删除 |
| [mcp.go](mcp.go) | MCP client（SDK 连接、工具注册与转发）、手写 JSON-RPC、calculator MCP server |
| [skills.go](skills.go) | frontmatter 解析、启动扫描与校验、skill 索引、load_skill |
| [memory_retrieval.go](memory_retrieval.go) | Contextual Retrieval：切块、背景生成与缓存、向量与BM25召回、RRF、LLM重排、预算与降级 |

默认三个工具是 calculator、get_current_datetime、search_notes；显式 `-lab-tools` 增加 always_fail 和 check_task_status 两个实验工具。学习笔记检索读取 `docs/day-01/day-01-notes.md`，工具选择和参数由模型生成。

## Day 3 上下文系统

```sh
go run . -context-lab -max-steps 60 -context-window 8192 -max-output-tokens 2048 -reasoning-effort minimal
```

完整Transcript保留在内存，模型读取View；写入时截断长工具结果，90%触发处理，40%为软目标；摘要后在60%内可接受，超过40%时提醒后继续。先尝试清理旧tool结果，收益不足再做原生前缀摘要。普通请求、摘要和超窗重试共用调用预算。实验统一minimal推理，所有回答与摘要均为真实调用，不保存实验日志。原理见 [Day 3笔记](docs/day-03/day-03-notes.md)。

旧的context-strategy、context-tokens和keep-recent已由统一策略、窗口与消息组配置替代。默认输入容量仍按12288−4096−1024=7168规划：未显式设输出上限时，4096只作为本地预算预留，不发送给API；显式设限时则按该值预留。省略参数仍受供应商默认限制，不能理解为无限输出。

## 观测台

`observer/` 是独立程序（有自己的 go.mod）。网页是类似 Codex 客户端的对话界面：左侧对话列表，主区显示用户消息、思考、工具调用和回答，底部输入框发送。顶栏切到“观测”有三个视图：轨迹（用户Turn → 模型Step的事件账本 + 瀑布时间轴）、迷宫（主路径、绕路、回退 + token与上下文压力数据轨）、对比（2–5次运行按请求序号对齐），并支持回放：

```sh
cd observer && go run .
# 打开 http://127.0.0.1:8090
```

回答结束后，在底部输入下一句话并按 Enter，会延续同一条对话；也可以先选择左侧的历史记录再续聊。点击“新对话”开始独立任务。运行中禁止重复发送，观测台重启后仍可从存档恢复。每个新问题开启下一个Turn并重新获得默认10次模型请求预算；Turn内Step从1编号，全局请求#N保持连续。摘要单独展示，不占任务Step，但仍计入请求预算。每个Step可查看全部messages和原始输入/输出。

点击左侧“Day 3 上下文实验”并确认，即可在对话流中看到33轮对话、压缩分隔线和大工具调用。它等价于 `go run . -context-lab -max-steps 60 -reasoning-effort minimal`，由观测台启动并接入代理，无需另开终端执行实验命令。切到“观测”，点击“终端输出”查看 `Context`、`Compact`、`Recall` 和 `Lab complete`；“迷宫”查看上下文压力与压缩位置。更新观测台源码后，需要重启观测台并刷新网页。

单独在项目根目录运行实验会直连方舟，不会自动出现在观测台；已经绕过代理的对话无法事后补录。通过观测台启动时，沿用观测台的 `observer/runs/` 存档；实验程序本身不另写日志。

左侧“Skills 中心”和“MCP 中心”类似 Codex 客户端的管理页。Skills 中心列出 `skills/` 下的 skill（数据来自 agent 的 `-skills-list`，与运行时同一套校验），可以查看正文、启停、删除，或填表新建一份 SKILL.md（写入后立即校验，不合规自动撤销）。MCP 中心添加 stdio server（名字 + 一行命令，相对项目根目录执行），可以启停、删除，“测试连接”调用 agent 的 `-mcp-list` 显示协商版本和工具列表。命令栏填 `http(s)://…` 地址即为远程 server。观测台自带一个远程 MCP server：启动时把 agent 编译到临时目录，以 `-mcp-serve -mcp-http 127.0.0.1:8091` 常驻运行，`http://127.0.0.1:8090/mcp` 转发过去（工具是 calculator）；在 MCP 中心用“快速填入 → observer”添加即可在对话中使用。Ctrl+C 退出观测台时它一起结束；`-mcp-addr ''` 可关闭。开关保存在 `.data/hub.json`。新对话勾选输入框里的 Skills / MCP 后，观测台把当时打开的 skill 与 server 作为快照写进 start 事件，转成 `-skills -skill …` 和多个 `-mcp-server`；续聊沿用快照，之后在中心里的改动只影响新对话。对话流在用户消息下方显示“✦”能力卡片（启用了哪些、注册了几个工具、哪些被跳过），`load_skill` 与 `mcp_*` 工具带 SKILL / MCP 标记。中心接口只接受本机 Host 且同源的请求，防止其他网页借浏览器添加命令。

观测台把 agent 的 `LLM_API_URL` 指向本机代理，从模型协议本身还原过程，agent 代码不含观测台专用埋点。摘要作为独立模型调用展示；视图重建在上下文压力轨中标记，工具结果按调用ID跨请求关联。详见 [观测台笔记](docs/observer/observer-notes.md)。

## Day 4：带来源的知识库问答

检索与Agent在同一个Go进程里，search_docs直接调用函数，无需启动8092服务。默认线上方舟向量模型，复用根目录.env里的ARK_API_KEY：

```sh
# 一条命令启动带检索的Agent：线上1024维向量
go run . -rag -embedding ark -question '本项目工具失败总共尝试几次？等待多久？'
# 或本地384维向量；问答仍调用方舟聊天模型
go run . -rag -embedding local -question '本项目工具失败总共尝试几次？等待多久？'
# 只检索，不调用聊天模型；local模式无需密钥
go run . -search-docs '本项目的工具重试次数与等待时间' -embedding local -k 3
# 10题真实对比，可用-embedding选择模型
go run . -rag-lab -max-steps 6 -reasoning-effort minimal
```

本地模式用Hugot纯Go后端加载官方ONNX权重，不运行Python或CGO；首次下载约470MB。线上请求使用方舟多模态embedding接口，只请求稠密向量（开启multi/sparse会让每次响应从约16KB变为约2MB）。本地模式单进程峰值内存约1.6GB。入库与查询分别使用压缩、检索指令。

网页使用时，只需一条命令：

```sh
go -C observer run .
# 打开 http://127.0.0.1:8090
```

**新对话 → 勾选“知识库 RAG” → 选择线上方舟或本地MiniLM → 提问**。观测台自动启动Agent，Agent自行初始化检索；无需另开检索进程。续聊自动沿用原模式和向量模型，重启观测台后也保留。左侧“Day 4 检索对比”可选择模型；实验记录不支持混成一段对话续聊。展开search_docs看结果，点击有效引用跳到对应片段。[学习课件](http://127.0.0.1:8090/slides/)也由观测台提供。

知识库为[24条FAQ](retrieval/corpus.md)，按标题切块，标题与正文一起embedding，来源作为元信息。仅启用RAG、检索检查或RAG实验时初始化向量模型；普通ReAct不加载。本地模型共享实例时串行推理，线上请求可并行。每个Agent进程初始化一次，退出时释放；新进程复用.cache/retrieval-go/磁盘缓存。修改语料或模型后，根据指纹选择或重建索引，不能混用不同模型的向量。

默认k=3，最多6；余弦相似度阈值0.40是教学起点，用-min-score调整，本地和线上分别校准。低分片段仅返回元信息，accepted=true仍需核对正文。BM25、rerank和LLM背景小抄讲清原理，基础实验实现向量召回。原理见[Day 4笔记](docs/day-04/day-04-notes.md)，本项目实现与10题实验见[Day 4项目实践](docs/day-04/day-04-lab.md)。`-search-docs`的stdout只有JSON，建索引日志写到stderr。

## Day 5：跨会话长期记忆

```sh
# 第一个进程：模型调用 remember_memory 登记原话，程序校验后在本轮结束时连同来源写进 .data/memory.json
go run . -memory -question '记住：错误码 ERR-4012 表示索引过期'
# 第二个进程：开头补背景 → 向量+BM25召回 → RRF → 重排，相关的写进 system，回答引用 [M编号]
go run . -memory -question 'ERR-4012 是什么意思？'
go run . -memory -embedding local -question 'ERR-4012 是什么意思？'
# 只看检索各阶段（对比旧关键词、纯向量、BM25、混合、加重排）
go run . -memory-search '我现在用什么编程语言？' -memory-mode hybrid
# TTL：本轮写入1分钟后过期；容量：上限2条时观察淘汰
go run . -memory -memory-ttl 1m -question '记住：今天下午三点在 3 号会议室开周会'
go run . -memory -memory-limit 2 -question '记住流程：代码评审先跑构建，再看逻辑'
# 人工删除
go run . -memory-forget M3
```

写入只来自两处：用户希望记住的原话，以及本轮重试耗尽仍失败的工具（经历，7天过期）。要不要记由模型判断：用户说“记住……”“别忘了……”“以后都……”等时，模型调用 `remember_memory(quote, kind)`；`quote` 必须逐字出自本次会话的用户消息，程序校验后再做长度与敏感信息检查（每轮最多3条），本轮结束时连同来源保存。模型不能写入用户没说过的内容或工具结果；用户明说“记住”而模型没有登记时，终端打印 `Memory hint`，不自动写入。新记忆同时保存来源上下文（本次会话的用户原话与实际工具结果）。

检索按 Anthropic《Introducing Contextual Retrieval》的流程实现：每个片段用"来源 + 片段"请求当前方舟模型生成背景说明，"背景 + 原文"同时建向量与 BM25 索引；查询时两路各取前10个片段，按记忆去重后 RRF 融合前20条，当前方舟模型重排，只注入相关的完整原文（最多5条、800估算token），允许0条。背景和向量缓存在 `.data/memory-index/`（派生数据，可删除重建）；旧记忆没有来源时按原文检索并标记。背景生成与重排计入 `-max-steps`，并给主任务至少留2次请求。模型、重排器和参数与 Anthropic 实验不同，差异与手动验证方法见 [Day 5 项目实践](docs/day-05/day-05-lab.md)。`.data/` 已排除提交。

观测台：新对话勾选“长期记忆”并选择向量模型，续聊沿用。回答前的“◎”卡片显示两路候选数、融合数、重排结果、选中数和降级原因，以及背景生成与重排两类辅助调用的原始输入输出；点记忆行查看原文、背景与来源。原理见 [Day 5 笔记](docs/day-05/day-05-notes.md)。

## Day 6–7：MCP client 与 server

```sh
go build -o bin/learning-agent .
GOBIN=$PWD/bin go install github.com/mark3labs/mcp-go/examples/everything@v1.1.1   # 现成的示例 server
# SDK client：连接 → tools/list → 调一次
./bin/learning-agent -mcp-server bin/everything -mcp-list -mcp-call add -mcp-args '{"a":1,"b":2}' 2>/dev/null
# 手写 JSON-RPC，对照两代协议
./bin/learning-agent -mcp-server bin/everything -mcp-raw legacy -mcp-call echo -mcp-args '{"message":"hi"}' 2>/dev/null
./bin/learning-agent -mcp-server bin/everything -mcp-raw modern 2>/dev/null
# Day 7 自举闭环：client → 自己的 server → calculate；再让 agent 通过 MCP 调用
./bin/learning-agent -mcp-server 'bin/learning-agent -mcp-serve' -mcp-list -mcp-call calculator -mcp-args '{"expression":"2*21"}'
./bin/learning-agent -mcp-server 'bin/learning-agent -mcp-serve' -question '请只用 MCP 提供的计算器算 (1234×5678) 开根号取整'
```

mcp-go v1.1.1 默认使用 2026-07-28 版协议：先发 `server/discover`，旧版 server 才退回 `initialize` 握手。MCP 工具加 `mcp_` 前缀，避免和本地工具重名。server 端用 schema、取参、calculate 三层校验，可纠正的错误以 `isError: true` 返回。注意：SDK 会把本进程的全部环境变量传给 server 子进程。详见 [Day 6 项目实践](docs/day-06/day-06-lab.md) 与 [Day 7 项目实践](docs/day-07/day-07-lab.md)。

## Bonus：知识型 skill

```sh
./bin/learning-agent -skills -question '帮我 review 这段 Go 代码：……'   # 主动 load_skill 并按 code-review 步骤评审
./bin/learning-agent -skills -question '北京今天天气怎么样？'            # 无关任务不加载
```

启动时打印 `Skills: loaded=N names=…`，坏 SKILL.md 打印 `Skill error` 并跳过。只读 Markdown 正文，不执行 skill 自带脚本。详见 [Skills 项目实践](docs/bonus-skills/skills-lab.md)。

## 配置与学习

默认使用 `https://ark.cn-beijing.volces.com/api/v3/chat/completions`、`doubao-seed-2-1-pro-260628` 和 `reasoning_effort: high`；模型请求等待上限为5分钟。普通请求和摘要共享推理配置及工具定义；摘要额外使用tool_choice: none禁止调用工具。输出上限默认省略，只有显式配置时才发送；可通过reasoning-effort显式调整整次运行的推理强度。

读取本地 `.env`，环境变量优先。支持 `ARK_API_KEY`（兼容 `LLM_API_KEY`）、`LLM_API_URL` 和 `LLM_MODEL`。密钥只放在本地私有配置，权限保持0600，`.gitignore` 已排除它。

自行在 `.env` 填写 `ARK_API_KEY`。修改模型上游地址后，重启观测台以读取新地址。

代码保持直接的函数分工，不编写 test 文件或脚本模型。通过格式化、编译和真实运行观察结果：

```sh
gofmt -w *.go
go build -o bin/learning-agent .
```

[Day 1 学习笔记](docs/day-01/day-01-notes.md)

项目目标与约定见 [PROJECT.md](PROJECT.md) 和 [AGENTS.md](AGENTS.md)。
