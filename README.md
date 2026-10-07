# 学习 agent：Day 1 → Day 11（Week 1–2）

想先看动画讲解，打开 [课件入口](docs/slides/index.html)：每节课一份可翻页、按步骤播放的 HTML 课件。

每天只读一篇笔记：[Day 1 工具调用与循环](docs/day-01/day-01-notes.md)、[Day 2 循环护栏](docs/day-02/day-02-notes.md)、[Day 3 上下文管理](docs/day-03/day-03-notes.md)、[Day 4 按需检索与引用](docs/day-04/day-04-notes.md)、[Day 5 长期记忆](docs/day-05/day-05-notes.md)、[Day 6 MCP client](docs/day-06/day-06-notes.md)、[Day 7 MCP server](docs/day-07/day-07-notes.md)、[Bonus Agent Skills](docs/bonus-skills/skills-notes.md)、[Day 8 取消超时与重试](docs/day-08/day-08-notes.md)、[Day 9 检查点与恢复](docs/day-09/day-09-notes.md)、[Day 10 任务调度与幂等](docs/day-10/day-10-notes.md)、[子 agent](docs/bonus-subagents/subagents-notes.md)、[Day 11 浏览器自动化](docs/day-11/day-11-notes.md)、[模型路由](docs/bonus-routing/routing-notes.md)。笔记只讲知识本身（原理、通用例子、权衡与自测）；本项目的代码阅读、动手步骤与实验记录在同目录的项目实践：[Day 1](docs/day-01/day-01-lab.md)、[Day 2](docs/day-02/day-02-lab.md)、[Day 3](docs/day-03/day-03-lab.md)、[Day 4](docs/day-04/day-04-lab.md)、[Day 5](docs/day-05/day-05-lab.md)、[Day 6](docs/day-06/day-06-lab.md)、[Day 7](docs/day-07/day-07-lab.md)、[Skills](docs/bonus-skills/skills-lab.md)、[Day 8](docs/day-08/day-08-lab.md)、[Day 9](docs/day-09/day-09-lab.md)、[Day 10](docs/day-10/day-10-lab.md)、[子 agent](docs/bonus-subagents/subagents-lab.md)、[Day 11](docs/day-11/day-11-lab.md)、[模型路由](docs/bonus-routing/routing-lab.md)。第一周的总结与架构图见 [Week 1 复盘](docs/week-01-review.md)。

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
| `-lab-tools` | false | 显式开启故障实验工具：Day 2 的 always_fail、check_task_status，Day 8 的 slow_job |
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
| `-timeout` | 0 | Day 8：整次运行的时限，如 `2m`；0 不限，仍可 Ctrl+C |
| `-tool-timeout` | 30s | Day 8：单次工具尝试的时限；到时记为结果未知，只读工具才重试 |
| `-task-id` | 随机 UUID | Day 9：检查点ID（字母、数字和 `._-`）；已存在时拒绝启动，提示改用 `-resume` |
| `-resume` | 空 | Day 9：从该ID的检查点续跑，配置取自检查点；须单独使用；已完成的直接返回存档答案 |
| `-subagents` | false | 增加 spawn_agent：把独立子任务交给全新上下文的子 agent（子进程），每次运行最多4个 |
| `-subagent-steps` | 6 | 每个子 agent 的模型请求预算，1到20 |
| `-provider` | ark | 模型路由：`ark`（豆包，方舟）或 `deepseek`；子 agent 继承，续跑沿用 |
| `-browser` | false | Day 11：增加 web_search 与 open_page，用无头 Chrome 查资料；环境变量 `CHROME_PATH` 指定浏览器，系统不支持 Chrome 沙箱时设 `CHROME_NO_SANDBOX=1` |

工具失败按200ms起步指数退避，耗尽后将错误回填；连续第三次相同动作拦下整批。超限或熔断会输出未完成与已执行步骤摘要。详见 [Day 2 学习笔记](docs/day-02/day-02-notes.md)。Day 8 起按错误类型重试：参数错误等确定性失败不重试；单次超时或执行中被取消记为结果未知，只读工具才重试；每项调用的结局（成功 / 失败 / 未执行 / 结果未知）都写进 tool 结果和摘要。

## 源码阅读

整个项目只有一个 Go 模块、一个入口：`go run .` 是 agent 本身，`go run . observe` 启动观测台。代码按功能分在 `internal/` 下（`internal` 里的包只能被本项目引用），依赖方向：

```
llm ← tools、retrieval、skills
      tools ← mcp；retrieval ← memory
      llm ← browser
      全部功能包 ← agent ← labs、queue
observer 不引用任何内部包：它只通过命令行启动 agent；main.go 把 mcp.Handler() 交给它挂在 /mcp
```

| 包 / 文件 | 阅读重点 |
| --- | --- |
| [main.go](main.go) | 唯一入口：解析参数、按开关组装工具、启动任务；`observe` 子命令转到观测台 |
| [internal/llm](internal/llm/) | [llm.go](internal/llm/llm.go) 发送消息历史与 tools、请求计数与用量；[protocol.go](internal/llm/protocol.go) Message/ToolCall/Observation、本次运行的工具定义 `llm.Tools`、token 粗估、消息组校验、`.env` 配置 |
| [internal/agent](internal/agent/) | [react.go](internal/agent/react.go) ReAct 循环、并行调度、按错误类型重试、单次超时、终止与 system prompt；[checkpoint.go](internal/agent/checkpoint.go) Day 9 检查点、原子写、flock、续跑；[child.go](internal/agent/child.go) 按任务ID幂等执行一个 agent 子进程（队列与子 agent 共用）；[subagent.go](internal/agent/subagent.go) spawn_agent；[dispatch.go](internal/agent/dispatch.go) 按工具名分发（检索、记忆、skill、浏览器、MCP、内置）；[context_manager.go](internal/agent/context_manager.go) Transcript/View、usage、集中清理与摘要 |
| [internal/tools](internal/tools/tools.go) | 内置工具 calculator、get_current_datetime、search_notes 与 Day 2/3 实验工具 |
| [internal/retrieval](internal/retrieval/) | [retrieval.go](internal/retrieval/retrieval.go) search_docs、进程内索引、点积排序与低分过滤；[embedding.go](internal/retrieval/embedding.go) 本地纯Go推理 / 线上方舟embedding |
| [internal/memory](internal/memory/) | [memory.go](internal/memory/memory.go) 依据层：原文与来源、文件锁事务、规则写入、过期淘汰、删除；[memory_retrieval.go](internal/memory/memory_retrieval.go) Contextual Retrieval：切块、背景、两路召回、RRF、重排 |
| [internal/mcp](internal/mcp/mcp.go) | MCP client（stdio 与 Streamable HTTP）、手写 JSON-RPC、calculator MCP server（stdio / 独立端口 / 观测台 `/mcp`） |
| [internal/skills](internal/skills/skills.go) | frontmatter 解析、启动扫描与校验、skill 索引、load_skill |
| [internal/browser](internal/browser/browser.go) | Day 11：chromedp 启动无头 Chrome，web_search / open_page，每次调用一个标签页，等待内容、提取正文、内网地址拦截，screencast 画面写入 `.data/browser/` |
| [internal/queue](internal/queue/queue.go) | Day 10 任务队列：任务文件、去重、固定数量 worker、任务级重试与汇总 |
| [internal/labs](internal/labs/) | [context_lab.go](internal/labs/context_lab.go) Day 3 真实33轮召回与大工具输出；[rag_lab.go](internal/labs/rag_lab.go) Day 4 10题有无检索对比 |
| [internal/observer](internal/observer/) | [server.go](internal/observer/server.go) 代理、存档、SSE、启动 agent 子进程、浏览器画面；[hub.go](internal/observer/hub.go) Skills/MCP 中心；[index.html](internal/observer/index.html) 页面 |

各功能的启用状态是包级变量（如 `retrieval.Enabled`、`memory.Active`、`skills.Index`、`mcp.Conns`），由 `main.go` 按命令行参数设置；一个 agent 进程只跑一个任务，所以这样足够，观测台的每次对话也都是独立子进程。

默认三个工具是 calculator、get_current_datetime、search_notes；显式 `-lab-tools` 增加 always_fail、check_task_status 与 slow_job 三个实验工具。学习笔记检索读取 `docs/day-01/day-01-notes.md`，工具选择和参数由模型生成。

## Day 3 上下文系统

```sh
go run . -context-lab -max-steps 60 -context-window 8192 -max-output-tokens 2048 -reasoning-effort minimal
```

完整Transcript保留在内存，模型读取View；写入时截断长工具结果，90%触发处理，40%为软目标；摘要后在60%内可接受，超过40%时提醒后继续。先尝试清理旧tool结果，收益不足再做原生前缀摘要。普通请求、摘要和超窗重试共用调用预算。实验统一minimal推理，所有回答与摘要均为真实调用，不保存实验日志。原理见 [Day 3笔记](docs/day-03/day-03-notes.md)。

旧的context-strategy、context-tokens和keep-recent已由统一策略、窗口与消息组配置替代。默认输入容量仍按12288−4096−1024=7168规划：未显式设输出上限时，4096只作为本地预算预留，不发送给API；显式设限时则按该值预留。省略参数仍受供应商默认限制，不能理解为无限输出。

## 观测台

观测台和 agent 是同一个程序：`go run . observe` 启动观测台，内置远程 MCP 也在这个进程里；每次对话由观测台再启动一个 agent 子进程。网页是类似 Codex 客户端的对话界面：左侧对话列表，主区显示用户消息、思考、工具调用和回答，底部输入框发送。顶栏切到“观测”有三个视图：轨迹（用户Turn → 模型Step的事件账本 + 瀑布时间轴）、迷宫（主路径、绕路、回退 + token与上下文压力数据轨）、对比（2–5次运行按请求序号对齐），并支持回放：

```sh
go run . observe          # 在项目根目录运行；打开 http://127.0.0.1:8090
go run . observe -dev     # 每次对话用 go run . 启动 agent，改完 agent 代码不必重启观测台
```

默认情况下，观测台启动 agent 时直接执行当前程序本身：不用编译、与观测台代码版本一致、在哪台机器上运行就是哪台机器的格式；代价是改了 agent 代码要重启观测台。`-dev` 反过来，每次对话多一次编译。

回答结束后，在底部输入下一句话并按 Enter，会延续同一条对话；也可以先选择左侧的历史记录再续聊。点击“新对话”开始独立任务。运行中禁止重复发送，观测台重启后仍可从存档恢复。每个新问题开启下一个Turn并重新获得默认10次模型请求预算；Turn内Step从1编号，全局请求#N保持连续。摘要单独展示，不占任务Step，但仍计入请求预算。每个Step可查看全部messages和原始输入/输出。

点击左侧“Day 3 上下文实验”并确认，即可在对话流中看到33轮对话、压缩分隔线和大工具调用。它等价于 `go run . -context-lab -max-steps 60 -reasoning-effort minimal`，由观测台启动并接入代理，无需另开终端执行实验命令。切到“观测”，点击“终端输出”查看 `Context`、`Compact`、`Recall` 和 `Lab complete`；“迷宫”查看上下文压力与压缩位置。更新观测台源码后，需要重启观测台并刷新网页。

单独在项目根目录运行实验会直连方舟，不会自动出现在观测台；已经绕过代理的对话无法事后补录。通过观测台启动时，沿用观测台的 `.data/runs/` 存档；实验程序本身不另写日志。

运行中，输入框的发送键变成 ■：第一次点击向 agent 发 SIGINT，agent 回填结果、写好检查点后退出；再点一次强制结束。停止（或超时、出错）后可以直接继续提问：观测台从检查点接着聊，模型会看到被打断的调用是“未执行”还是“结果未知”。新对话勾选“子 agent”后，对话流里每个 `spawn_agent` 显示为一张卡片，点开在右侧侧栏查看该子 agent 的任务原文与完整过程（参考 Codex 客户端）。

左侧“Skills 中心”和“MCP 中心”类似 Codex 客户端的管理页。Skills 中心列出 `skills/` 下的 skill（数据来自 agent 的 `-skills-list`，与运行时同一套校验），可以查看正文、启停、删除，或填表新建一份 SKILL.md（写入后立即校验，不合规自动撤销）。MCP 中心添加 stdio server（名字 + 一行命令，相对项目根目录执行），可以启停、删除，“测试连接”调用 agent 的 `-mcp-list` 显示协商版本和工具列表。命令栏填 `http(s)://…` 地址即为远程 server。观测台自带一个远程 MCP server：`http://127.0.0.1:8090/mcp`，由观测台进程直接处理（工具是 calculator，与 `-mcp-serve` 同一个 server 定义），在 MCP 中心用“快速填入 → observer”添加即可在对话中使用。中心接口只接受本机 Host 且同源的请求，防止其他网页借浏览器添加命令。

观测台把 agent 的 `LLM_API_URL` 指向本机代理，agent 在请求头 `X-Agent-Upstream` 里声明真实上游（方舟或 DeepSeek），代理照此转发；从模型协议本身还原过程，agent 代码不含观测台专用埋点。摘要作为独立模型调用展示；视图重建在上下文压力轨中标记，工具结果按调用ID跨请求关联。详见 [观测台笔记](docs/observer/observer-notes.md)。

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
go run . observe
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

## Day 8–10：运行时、检查点与任务队列

```sh
go build -o bin/learning-agent .
# Day 8：单次超时 → 结果未知，有副作用的工具不自动重做；按一次 Ctrl+C 优雅停止，再按一次立即退出
./bin/learning-agent -lab-tools -tool-timeout 3s -question '请调用 slow_job 执行一个 10 秒的作业，然后告诉我结果。'
./bin/learning-agent -lab-tools -timeout 15s -question '调用 slow_job 执行 60 秒的作业。'
# Day 9：每次运行打印 Checkpoint: id=…；中断（Ctrl+C、kill -9、模型请求失败）后续跑
./bin/learning-agent -lab-tools -task-id demo -question '请同一轮并行调用：slow_job 执行 40 秒的作业，同时用 calculator 算 1234*5678。'
./bin/learning-agent -resume demo
# Day 10：10 个任务、3 个 worker；再跑一次全部直接取存档；中途 Ctrl+C 后再跑会续跑
go run . queue -workers 3 queue/tasks.jsonl -- -reasoning-effort low -max-steps 4
```

检查点在 `.data/checkpoints/<id>.json`（已忽略提交），运行期间持有同名 `.lock` 的 flock，同一任务不会被两个进程同时执行。模型决定调用工具后、执行前先写一次（写前日志），续跑时末尾没有结果的调用：只读工具重新执行，其余回填“结果未知”。预算、熔断计数和启动参数都在检查点里，续跑不重置预算、不需要重新输入参数。

队列的任务ID就是检查点ID：已完成的不再执行，中断的用 `-resume` 续跑，同一文件里重复的ID只入队一次，不写 `id` 时按问题内容哈希。每个任务的完整输出在 `.data/checkpoints/<id>.log`。详见 [Day 8](docs/day-08/day-08-lab.md)、[Day 9](docs/day-09/day-09-lab.md)、[Day 10](docs/day-10/day-10-lab.md) 项目实践。

## Bonus：子 agent

```sh
./bin/learning-agent -subagents -question '我要准备一份学习简报，包含三部分，彼此独立，请分给子 agent 并行完成：……最后汇总成一张表。'
```

模型调用 `spawn_agent(task)` 时，启动一个只拿到 task 的子 agent 进程（复用队列的执行器），输出加 `│ call_id` 前缀转到当前终端，结论作为 tool 结果交回。子 agent 继承工具开关，不继承长期记忆与 `-subagents`；子任务ID = 父检查点ID + task 哈希，父任务续跑时已完成的子任务直接取回结论。详见 [子 agent 项目实践](docs/bonus-subagents/subagents-lab.md)。

## Day 11：浏览器

```sh
# 需要本机有 Chrome/Chromium；不在 PATH 里时用 CHROME_PATH 指定
./bin/learning-agent -browser -question '用浏览器查一下 chromedp 最新发布的版本号和发布日期，给出来源链接。'
# 观测台：新对话勾选“浏览器”，右侧实时显示 agent 的浏览器画面
go run . observe
```

每次 `web_search` / `open_page` 开一个新标签页，返回可见正文（最多3000字，可用 `find` 只取含关键词的段落），调用结束或超时就关闭。不允许打开本机和内网地址；不伪装 User-Agent，遇到验证码如实告诉模型。画面保存在 `.data/browser/<任务ID>/<调用ID>.jpg`。详见 [Day 11 项目实践](docs/day-11/day-11-lab.md)。

## 配置与学习

默认使用 `https://ark.cn-beijing.volces.com/api/v3/chat/completions`、`doubao-seed-2-1-pro-260628` 和 `reasoning_effort: high`；模型请求等待上限为5分钟。普通请求和摘要共享推理配置及工具定义；摘要额外使用tool_choice: none禁止调用工具。输出上限默认省略，只有显式配置时才发送；可通过reasoning-effort显式调整整次运行的推理强度。

读取本地 `.env`，环境变量优先。方舟：`ARK_API_KEY`（兼容 `LLM_API_KEY`）、`LLM_API_URL`、`LLM_MODEL`；DeepSeek（`-provider deepseek`，默认 `https://api.deepseek.com/chat/completions`、`deepseek-flash`）：`DEEPSEEK_API_KEY`、`DEEPSEEK_API_URL`、`DEEPSEEK_MODEL`。向量检索只用方舟。密钥只放在本地私有配置，权限保持0600，`.gitignore` 已排除它。

自行在 `.env` 填写 `ARK_API_KEY`，要用 DeepSeek 再填 `DEEPSEEK_API_KEY`。观测台新对话可在输入框左下角选模型，一段对话固定一个模型。详见 [模型路由项目实践](docs/bonus-routing/routing-lab.md)。

代码保持直接的函数分工，不编写 test 文件或脚本模型。通过格式化、编译和真实运行观察结果：

```sh
gofmt -w *.go
go build -o bin/learning-agent .
```

[Day 1 学习笔记](docs/day-01/day-01-notes.md)

项目目标与约定见 [PROJECT.md](PROJECT.md) 和 [AGENTS.md](AGENTS.md)。
