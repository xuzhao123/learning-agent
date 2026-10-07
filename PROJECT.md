# 学习 agent 项目说明

## 目标

以用户提供的 Manus「Agent 全栈工程师」岗位要求为长期学习目标，使用 Go 逐步实现 agent。完整能力对照见 [JOB_REQUIREMENTS.md](JOB_REQUIREMENTS.md)。

当前完成 Week 1（Day 1–7）、Bonus Skills，以及 Week 2 的 Day 8–10 与子 agent，优先帮助用户理解 agent 流程。用户每天给出任务，每一步围绕当天知识点提供讲解、例子、代码阅读和练习。

## 工作约定

- 只实现当天任务，保持代码精简、直接，优先使用 Go 标准库。
- 不写 test 文件，不搭建测试框架，不增加脚本模型或写死的工具决策。
- 不为生产系统、测试注入或未来需求引入通用接口、复杂分层和大量配置。
- 使用真实方舟模型和实际工具执行验证行为；学习目录只保留当天的一篇笔记，运行输出按需在终端查看。
- 修改代码时同步学习笔记；运行通过与用户掌握情况分开记录。
- 保留用户已有修改。密钥只放环境变量或本地私有配置，不写进源码、文档或日志。

## 当前实现

- 路径约定：所有启动命令从项目根目录执行；源码、文档、缓存和运行数据使用项目相对路径。
- 代码结构：一个 Go 模块、一个入口 `main.go`（`go run .` 是 agent，`go run . observe` 是观测台）。按功能分包在 `internal/`：`llm`（模型请求、Message/ToolCall/Observation、`llm.Tools` 工具注册表、token 粗估、消息组校验、`.env` 配置）、`agent`（`react.go` 循环、`dispatch.go` 工具分发、`context_manager.go`）、`tools`（内置与 Day 2/3 实验工具）、`retrieval`（Day 4 检索与向量模型）、`memory`（Day 5）、`mcp`（Day 6–7 client、手写 JSON-RPC、calculator server）、`skills`（Bonus）、`queue`（Day 10 任务队列）、`labs`（Day 3/4 实验）、`observer`（观测台）；`agent` 另有 `checkpoint.go`（Day 9）、`child.go`（按任务ID执行 agent 子进程）、`subagent.go`（spawn_agent）。各功能开关是包级变量，由 main 按参数设置；一个 agent 进程只跑一个任务。
- 方舟原生 Tool Calling：通过 `tools` 提供定义，从 `tool_calls` 接收请求，普通回答不限制文本格式。
- 支持独立工具同批并行，默认并发4，每批最多16项；依赖结果的调用放在下一轮。
- 模型请求默认最多10次，可通过 `-max-steps` 配置，Day 3 摘要请求也计入；保存原始 assistant 消息，每项结果用 `role: "tool"` 与 `tool_call_id` 写回历史；无调用且有正常回答时结束。
- 工具默认额外重试2次，按200ms、400ms退避，耗尽后回填错误；第三次连续相同动作触发熔断。超限和熔断均返回未完成及已执行步骤摘要。Day 8 起只重试暂时性错误（见下）。
- `-lab-tools` 显式启用 Day 2 故障实验，默认关闭。
- Day 3 按用户提供的上下文系统设计重构：内存Transcript与View分离，写入时截断，usage加增量估算，90%触发、40%软目标与60%接受上限，用户原话与完整最近组配额，原生前缀摘要；摘要保留工具定义并以tool_choice: none禁用工具（仅在带工具时发送）；压缩布局按真实/粗估比例换算容量；摘要输入超窗时先删保留区之后的旧组；只剩1次请求时不摘要；API输出上限默认省略，本地输出预留与请求限制分开。
- calculator 实际计算；日期工具读取当前时间；search_notes 检索实际 docs 学习笔记。
- Day 4：`-rag`注册search_docs，检索与Agent在同一Go进程直接调用；-embedding选择本地纯Go MiniLM或线上方舟Doubao，-min-score调整阈值，缓存沿用.cache/retrieval-go。普通任务不初始化向量模型。24条FAQ按标题切块，k默认3、范围1–6；低分正文不注入。-search-docs独立检查召回（local无需密钥），-rag-lab比较10题真实回答。CLI用go run .；网页用go run . observe，选择向量模型后直接提问，续聊与重启恢复选择，无8092检索服务。
- Day 5：`-memory` 启用跨进程长期记忆 `.data/memory.json`（已忽略提交）。写入只来自用户原话和本轮重试耗尽的工具失败（episodic 0.4，7天过期）：模型调用 remember_memory(quote, kind) 登记（semantic 0.9 / procedural 0.8），程序校验引文逐字出自本次会话用户消息（不含摘要、回答、工具结果）、长度与密钥、每轮≤3条，被拒时工具直接返回原因；本轮结束时写入、同内容同前文只刷新；未登记但含“记住/别忘”时只打印 Memory hint；疑似密钥或超500字拒存。新记忆另存来源上下文（用户原话与实际工具结果，≤2000字，模型回答/摘要/记忆工具结果不算）。检索升级为Contextual Retrieval：按向量模型输入上限切块（方舟≤300字；本地MiniLM≤64 token、背景≤40字），检索时为缺失块请求当前方舟模型生成背景（`memory_contextualize`，每次运行≤`-memory-context-calls`默认4次），“背景+原文”同时建向量与BM25（k1=1.2、b=0.75，汉字两字组+编号整词）索引；两路各10片段→按记忆去重RRF（k=60）前20→`memory_rerank`当前聊天模型重排（推理low，校验JSON）→相关且≥50分按0.8重排+0.1重要性+0.1新近度排序→最多5条、`-memory-tokens`默认800；删除了“重要性高即使不相关也注入”的兜底，允许0条。重排失败只保留两路都召回的最多3条。辅助调用计入-max-steps并给主任务留2次。派生数据在 `.data/memory-index/`（contexts.json、vectors-<模型>.json），按指纹失效，删除/过期/淘汰同事务清理；旧记忆无来源标no_source按原文检索。recall与search_memory共用 `retrieve`；开场结果写进system，整次会话不变，续聊沿用首轮system。`-memory-search`+`-memory-mode`（keyword/vector/bm25/hybrid/rerank）对比各阶段。模型只有search_memory（按需检索）与forget_memory（更正删除），不能新增记忆。每次读写在sync.Mutex+flock内完成读、删过期、修改、超量淘汰（重要性×0.5^(天/7)最低者，只在写入时）和原子写回，日志在落盘后打印、保存失败向上返回；embedding与模型请求都在锁外，使用前回锁内校验；-memory-ttl、-memory-limit（默认200）、-memory-forget（不做淘汰）。向量模型进程内共享，本地推理加锁；modelClient计数与用量加锁，请求头 `X-Agent-Purpose` 标明用途。注入日期统一上海时区。
- Day 6：引入 mcp-go v1.1.1（用户任务指定；要求 Go≥1.25.5，go.mod 由1.25.0升至1.25.5）。`-mcp-server` 以空格切分命令启动 stdio 子进程（不经shell），SDK 先发 `server/discover`（2026-07-28 无状态协议），旧版 server 才退回 initialize（2025-11-25）；检查 tools capability 后 tools/list，工具名加 `mcp_` 前缀、非法字符换 `_`、≤64、与本地重名跳过，inputSchema 原样作 parameters；runTool 的 default 分支转发 tools/call，协议错误与 isError 分别标注后回填；连接与每次调用各1分钟超时；server stderr 转到本进程 stderr。`-mcp-list`（SDK 连接+列工具+可选 `-mcp-call`/`-mcp-args`）与 `-mcp-raw legacy|modern`（手写 JSON-RPC，逐行打印 →/←）不请求模型。示例 server 用 `go install github.com/mark3labs/mcp-go/examples/everything@v1.1.1` 装到 bin/。
- Day 7：`-mcp-serve` 在 flag.Parse 后立即分流（stdout 归协议），须单独使用；mcp-go server 暴露 calculator（expression 必填、≤1024），WithInputSchemaValidation + WithStrictInputSchemaDefault（additionalProperties:false）+ handler RequireString + calculate AST 白名单三层校验，错误以 NewToolResultError（isError）返回，未知工具由 SDK 返回 -32602；WithRecovery；结果为 structuredContent {result} + 文本；每次调用在 stderr 记一行。
- Bonus Skills：`-skills` 扫描 `skills/*/SKILL.md`，手写 frontmatter 解析（顶层 key: value、去成对引号、缩进行跳过、多行值/重复键/缺分隔线报错），校验 name（≤64、`^[a-z0-9]+(-[a-z0-9]+)*$`、等于目录名）、description（≤1024字符）、compatibility（≤500）、文件≤64KB；坏文件打印 Skill error 跳过，打印 Skills: loaded=N names=…；有可用 skill 时 system 追加索引并注册 load_skill(name)，按索引查表读文件并重新校验，未知名字返回结构化结果（不触发重试）；不读 references、不执行 scripts。
- 观测台在 `internal/observer/`，由 `go run . observe` 启动（须在项目根目录）：每次对话启动一个 agent 子进程（默认执行当前程序本身，`-dev` 时用 `go run .`），本机代理记录每次模型请求与响应，事件存档于 `.data/runs/`（已忽略提交），启动时读回；网页是类似 Codex 客户端的对话界面（对话流从请求/响应还原用户消息、思考、工具与回答，压缩显示为分隔线），“观测”标签提供轨迹、迷宫、对比三个视图和回放；可新建对话、从存档恢复View续聊，或运行Day 3上下文实验；能直接查看终端输出，并从实际Context输出读取输入容量。agent 只额外接受 `http://127.0.0.1` 模型地址。Day 5：新对话可勾选长期记忆与TTL，续聊沿用；对话流显示开场调入与结尾写入卡片，[M编号]引用可点击；左侧记忆面板只读 `.data/memory.json`，删除调用agent的 `-memory-forget`。Contextual Retrieval 后：记忆模式可选ark/local并续聊沿用；代理按请求头记录purpose，`memory_*` 辅助调用单独列出（原始输入输出与token），不开Turn、不占Step，续聊恢复跳过；调入卡片显示两路候选、融合、重排、选中数与降级原因，面板显示背景、块状态与来源。Skills / MCP 中心：配置在 `.data/hub.json`（skill 默认启用、只记停用名单；MCP server 名字+命令+开关），skill 列表与新建校验调用 agent 的 `-skills-list`，测试连接调用 `-mcp-list`；新对话勾选 Skills/MCP 时把当时打开的项作为快照写入 start 事件并转成 `-skills -skill …` 与多个 `-mcp-server`，续聊沿用快照；对话流显示能力卡片，工具带 SKILL/MCP 标记；`/hub` 接口校验本机 Host 与同源 Origin。agent 侧 `-mcp-server` 可重复、连不上的 server 跳过，新增 `-skill`（可重复）与 `-skills-list`。远程 MCP：`-mcp-server` 值为 http(s) 地址时用 Streamable HTTP client；`-mcp-serve -mcp-http addr` 以 Streamable HTTP 提供 calculator；观测台进程内直接把 `mcp.Handler()` 挂在 `/mcp`（与 `-mcp-serve` 同一个 server 定义；本机同源检查）。
- Day 8：`-timeout` 整次运行时限（包在信号上下文外，停止原因 `timeout`）；`-tool-timeout`（默认30s）为每次工具尝试派生子上下文，`spawn_agent` 用10分钟；`runAttempt` 让工具在独立 goroutine 里运行、执行器到时即返回（带缓冲通道防泄漏）。`llm.Observation.Status` 分 ok / error / not_run（排队或首次尝试前取消）/ unknown（执行中超时或取消）；summary 与 tool 结果逐项写出，修复被取消调用消失的问题。`llm.Permanent` 标记确定性错误（参数、未知工具、功能未开、Calculate 错误、MCP isError），不重试；unknown 只对 `repeatable` 表内的只读或带幂等键工具重试；退避可被取消打断。第一次 Ctrl+C 取消 ctx，`context.AfterFunc` 恢复默认处理，第二次立即退出。`-lab-tools` 新增 `slow_job(seconds)`：真实等待、响应取消、描述为有副作用。停止原因通过 `stopError` 返回。
- Day 9：每次普通运行写 `.data/checkpoints/<id>.json`（`-task-id` 指定或按启动时间生成；ID 仅 `[A-Za-z0-9._-]`、≤128），实验不写。字段：args、question、status（running/stopped/done）、reason、answer、error、step、calls、View 消息、summary、熔断计数。写入时机：问题写入后、模型决定调用工具后执行前（写前日志）、结果写回后、结束时（defer）。临时文件 + rename 原子替换，不 fsync。运行全程持有 `<id>.lock` 非阻塞 flock，第二个进程报 ErrBusy。`-resume id` 须单独使用：done 直接打印存档答案；running 或因 cancelled/timeout/model_error/checkpoint_error 停止的，用保存的 args 重新解析参数，恢复 client.Calls 与熔断计数，`resumeContext` 取出末尾无结果的 tool_calls，第一轮不请求模型而由 `replayBatch` 补齐（repeatable 工具重新执行，其余回填 unknown）；其他停止原因拒绝续跑。新 ID 已有检查点时拒绝启动。`restoreContext` 拆出 `rebuildContext` 供续聊与续跑共用。续跑跳过记忆召回。
- Day 10：`go run . queue [-workers 3] [-attempts 3] 文件.jsonl [-- 公共参数]`。任务行 `{id, question, args}`，缺 id 时用 `q-`+SHA-256(问题与参数)前12位，文件内重复 ID 只入队一次。任务ID即检查点ID，队列不另存状态。固定 worker（1–8）从 channel 取任务，`agent.RunChild` 执行：无检查点则 `-task-id -question` 全新启动，问题不一致报错，done 直接返回（replayed），可续跑则 `-resume`，否则不可重试错误；启动前试锁。子进程 Setpgid、取消时发 SIGINT、WaitDelay 10s，输出追加到 `.data/checkpoints/<id>.log`。暂时性失败退避1s、2s后续跑，确定性失败不重试；Ctrl+C 后停止派发并汇总 done/replayed/failed/interrupted/not_started。示例任务 `queue/tasks.jsonl`。
- 子 agent：`-subagents` 注册 `spawn_agent(task)` 并在 system 追加使用规则；子任务ID = 父检查点ID + `-sub-` + task 的 SHA-256 前12位，经 `RunChild` 以子进程运行，`-max-steps` 取 `-subagent-steps`（默认6），每次运行最多4个不同子任务；子进程输出逐行加 `│ call_id` 前缀转到父终端；返回 {task_id, answer, model_calls, replayed}。子 agent 按白名单继承工具开关与运行参数（含 `-mcp-server`、`-skill`），不继承 `-memory`、`-subagents`、`-timeout` 与续聊。`spawn_agent` 列入 repeatable。
- 默认方舟地址为 `https://ark.cn-beijing.volces.com/api/v3/chat/completions`，模型为 `doubao-seed-2-1-pro-260628`，请求使用 `reasoning_effort: high`，等待上限5分钟。
- 本地 `.env` 自动读取，保留指定方舟地址、模型与认证，权限0600并已加入提交忽略规则。

## 进度记录

| 步骤 | 状态 | 产物 |
| --- | --- | --- |
| 项目准备 | 已完成 | AGENTS.md、PROJECT.md、岗位能力对照 |
| Day 8：取消、超时与重试 | 已编译、go vet 通过并真实运行；理解待反馈 | slow_job 10s、单次时限3s：unknown、No retry（unknown_not_repeatable）；并行 slow_job+calculator 时 SIGINT：slow_job unknown、calculator ok，summary 两项都在，检查点 stopped/cancelled；`-timeout 15s`：工具实际只得到约8.6s，Termination: timeout；calculator 1/0 一次即停（permanent），always_fail 仍重试3次。day-08-notes/lab |
| Day 9：检查点与恢复 | 已编译并真实运行；理解待反馈 | done 任务 `-resume` 直接返回存档答案；Ctrl+C 后续跑从第2轮、请求2/4开始，`-lab-tools` 取自检查点；kill -9 于工具执行中：检查点末尾为两个无结果调用，续跑 Round 1 (resume) 不请求模型，calculator 重新执行、slow_job 回填 unknown；同 ID 两进程并发，第二个报 ErrBusy。day-09-notes/lab |
| Day 10：任务队列与幂等 | 已编译并真实运行；理解待反馈 | 10个任务3个worker 65秒全部完成，23次模型请求；t01 遇真实 model_error，第2次尝试以 `-resume` 续跑（预算从2/4继续）；重复 ID 只入队一次；重跑队列 replayed=10、无新请求；4任务2 worker 20秒时 SIGINT：2个 interrupted（子进程 Termination: cancelled）、2个 not_started，再跑全部完成且中断的从第2轮续跑。day-10-notes/lab |
| 中断后从检查点续聊 | 已编译、go vet 通过并真实运行；理解待反馈 | `canContinue`：exit 0 照旧；非 0 时只要本次运行打印过 `Checkpoint: id=` 就允许续聊，`resumeHistory` 改读 `.data/checkpoints/<id>.json` 的 messages（模型、推理强度、是否检索取自最近一次主任务请求）。agent `restoreContext` 接受以用户问题或工具结果结尾的历史，末尾无结果的 tool_calls 补成 unknown（与 replayBatch 共用文案）。实测：被停止的子 agent 对话续聊，模型请求含原问题、两个 spawn_agent 调用与两条“结果未知”再加新消息，模型按要求只重派计算任务；子 agent 因 `^` 不受支持耗尽预算后，父 agent 自己算完，exit 0；正常结束的对话续聊不受影响。页面在新一轮前显示中断标记，停止卡片提示可继续提问 |
| 观测台停止按钮与子 agent 侧栏 | 已编译、go vet 通过；真实方舟运行与无头浏览器验证；理解待反馈 | 运行中发送键变 ■：`POST /runs/{id}/stop` 向 agent 进程组（Setpgid）发 SIGINT，再按一次 SIGKILL；被取消的代理请求记 499。agent 在本机代理请求头加 `X-Agent-Task`，代理以每次启动后的首个任务ID为父、其余标 `sub`，续聊恢复跳过 sub。页面：spawn_agent 卡片 + 右侧侧栏（子 agent 列表、任务原文、完整过程、终端输出），新对话“子 agent”开关。实测：三子 agent 对话父2次/子6次请求分开显示；子 agent 运行中点停止，父子三个检查点均 stopped/cancelled；续聊后新父进程请求仍归主对话。修复：stopcard 遇空 stdout 行报错；spawn_agent 取消时父进程先退出导致子进程输出与检查点丢失（改为等待子进程收尾）。1440/390 宽度、浅色/深色截图无报错、无横向滚动 |
| 子 agent（spawn_agent） | 已编译并真实运行；理解待反馈 | 三个子 agent 并行（父2次请求、子共7次）后汇总；kill -9 父进程：计算子任务 done、slow_job 子任务因 SIGPIPE 停在 running，父续跑时重放两个 spawn_agent：前者 replayed=true，后者续跑、内部 slow_job 回填 unknown 后被子模型重做（如实记录为重复执行风险）；SIGINT 传给子进程一次，父子检查点都为 stopped/cancelled。子任务ID由调用ID改为 task 哈希，以便取消后同 task 再派可续上。subagents-notes/lab；观测台未适配 |
| MCP 协议与校验追问 | 已核对现行规范并真实运行取参对照；理解待反馈 | 协议无状态的收益、元数据与兼容成本、业务状态及重放安全已补入 Day 6 笔记；Day 7 临时副本只关闭 schema 校验，数字、缺参、正常输入在两个版本中共六次真实 stdio 调用，第二层的 rejected 日志与错误结果已确认；实验记录见 lab 3.4。项目代码未改，Day 8 错误分类仅讨论策略，尚未实现 |
| 文档与启动说明统一 | 已完成；主工程与观测台 build、go vet通过；文档链接与脚本语法检查通过 | 启动命令统一从项目根目录执行，示例使用相对路径；删除个人路径与部署环境叙述；观测台启动提示不显示项目绝对目录，内置 MCP 的编译和启动诊断使用通用路径名称；约定同步至 AGENTS.md |
| Day 6：MCP client | 已编译、go vet通过并真实运行；理解待反馈 | mcp-go SDK 连接 everything 示例 server：协商 2026-07-28、6个工具、add 返回42.5；手写 JSON-RPC 对照 legacy（initialize/initialized、请求无_meta）与 modern（server/discover、每请求_meta）；agent 同轮并行调用 mcp_add 与 mcp_echo 后正确回答。day-06-notes 讲 JSON-RPC、传输、三原语、旧版握手与capabilities协商、2026-07-28 去握手与 discover、工具定义转换、两种错误；lab 记录代码阅读与真实日志 |
| Day 7：MCP server 与周复盘 | 已编译并真实运行；周复盘已写；理解待反馈 | `-mcp-serve` 暴露 calculator；client→自己的 server 得 floor(sqrt(1234*5678))=2647；手写 client 发错误参数：类型错/多余字段/缺必填被 schema 层以 isError 拦下，os.Exit(1) 被 calculate 拦下，未知工具返回 -32602；agent→自己的 server→calculate 闭环正确（server stderr 日志为证）。docs/week-01-review.md：Day1–7 一句话、含6模块（另加skills）的 mermaid 架构图、W2 三件事（建议草稿，待用户确认） |
| Bonus：知识型 Skills | 已编译并真实运行；理解待反馈 | skills/code-review/SKILL.md（由 Day 5 procedural 记忆改写）；三个临时坏文件各有明确报错且不 crash（运行后删除）；天气问题未调用 load_skill；代码评审问题主动 load_skill 并按“构建→分级问题→结论”输出，且未假装运行命令；unknown_skill 分支仅代码阅读；observer 可选项未做 |
| 单一入口与按功能分包 | 已编译、go vet通过并真实运行；理解待反馈 | 先在分支 single-entry-refactor 提交检查点；删除 observer/go.mod 与转发用的 mcp_host.go，代码移入 internal/ 九个包，存档迁到 .data/runs（47条读回）。CLI：并行 calculator+日期得2647；`-mcp-server "go run . -mcp-serve" -mcp-list`、`-skills-list`、local `-search-docs`（D05 第一）正常。`go run . observe`：单进程、无8091，/mcp 跨站403，经 /mcp 的 -mcp-list 与中心“测试连接”成功；真实方舟对话（skills+everything+observer）同轮调用 mcp_calculator 与 mcp_echo，续聊沿用快照并 load_skill 评审；RAG(local)+记忆对话引用 D05 并写入记忆（测试记忆随后删除）；`-dev` 模式与错误参数、非根目录启动的报错已核对。未运行 Day 3/Day 4 实验（只编译） |
| 观测台内置远程 MCP | 已编译、go vet通过并真实运行；理解待反馈 | agent 直连 HTTP server：协商2026-07-28、tools/call 得2647；-mcp-raw 遇到 URL、-mcp-http 单独使用均明确报错。观测台启动后后台编译并运行内置 server，经 :8097/mcp 测试连接成功（1个工具）；跨站 Origin 与伪造 Host 均403；只启用 observer 远程 server 的真实方舟问答注册 mcp_calculator 并得2647，观测台终端有 calculator ok 日志；SIGINT 后子进程结束、临时目录删除、内部端口关闭 |
| 观测台 Skills 中心与 MCP 中心 | 已编译、go vet通过并真实运行；页面视觉待浏览器确认；理解待反馈 | 接口实测：添加 everything/calculator/坏命令三个 server，重名被拒，跨站 Origin 返回403；测试连接显示 calculator 协商 2026-07-28 与1个工具，坏命令显示启动失败；新建 skill 的非法名字、多行 description、重名、超1024字符 description（由 agent 校验拦下并撤销）均被拒，合法 skill 可启停与删除（临时 skill 已删）。真实方舟问答：快照为 code-review + 3个server，注册 mcp_add 等6个与 mcp_calculator，坏 server 打印 MCP error 跳过；模型同轮调用 mcp_calculator 与 load_skill，得2647并按评审流程输出；停用坏 server 后续聊仍沿用快照，调用 mcp_add 得42.5。页面已做 JS 语法检查，视觉与交互待确认。MCP 预设统一使用 `go run . -mcp-serve` 与 `go run …/everything@v1.1.1` 从源码启动，已验证分别连接1个、6个工具；启动失败时页面提示使用源码预设或重新构建 |
| 观测台对话 ID 与链接定位 | 已编译并用真实存档在独立预览中验证；理解待反馈 | 顶栏显示并复制已有对话 ID；URL 的 run 参数跟随对话。浏览器核对切换、刷新恢复、前进后退、新对话清除参数，以及对话/观测切换保持 ID；无 JS 报错，未新增模型请求 |
| Day 1 Tool Calling / ReAct | 已实现，理解待用户反馈 | 真实模型调用、并行工具执行、消息历史与终止 |
| 学习方式调整 | 清理与真实运行已完成 | 删除 test、demo、脚本模型及假笔记字典；核心代码由936行精简至466行，真实两道任务通过 |
| 并行链路讲解 | 笔记已补充，理解待反馈 | 区分工具目录加载、模型批量调用与执行并发；补充 Codex、Claude Code 的公开执行机制、程序编排与结果关联 |
| 简化 system prompt | 已完成，理解待反馈 | 原生工具协议、普通回答、依赖调用与错误回流已实际通过；删除 parseDecision，已移除固定文本格式，原理见 Day 1 学习笔记 |
| 可视化观测台 | 已实现并真实运行，理解待反馈 | 独立 observer 程序：协议层代理、SSE 网页时间线、JSONL 存档；agent 仅放行本机 http 地址（441行）；页面视觉待用户在浏览器确认 |
| 观测台Turn/Step事件分组 | 已编译并用真实存档在浏览器验证；理解待反馈 | 参考DeepSeek Harness：用户回合Turn内按任务模型Step分组；SYSTEM/USER/ASSISTANT/TOOL/COMPACTED/EXIT按事件展示，摘要不占任务Step，原始输入输出与全部messages保留在请求详情。最新检索计算为1 Turn、3 Step、1 USER、2 TOOL；hello为8 Turn且摘要独立，Day 3为34回合，Day 4为20组；并行、原始开关、折叠、回放、迷宫、对比、详情标签与聊天共用归属已核对，无新增模型请求 |
| 观测台逐 Turn 原始输入/输出 | 已实现并通过真实存档与回放核对；理解待反馈 | 轨迹 Turn 与对话模型调用共用开关；完整请求/响应双栏卡片、JSON 着色、行号、复制；等待响应时显示占位，返回后更新；普通、工具和压缩调用均已核对，完整输入/输出与存档一致，复制粘贴、标签切换保留展开、JSON布局和无页面报错通过；仅增强 observer/index.html，不新增模型请求 |
| 观测台统一模型输入/输出（初版） | 已由Turn/Step事件分组承接，理解待反馈 | 全部messages按请求顺序编号和角色展示，工具定义在独立tools字段；原来的输入/输出轨迹行改为Step内详情入口。最近检索计算任务三次请求仍为2/4/6条输入，属于同一个Turn，完整输入与响应保持可查看 |
| 观测台流程节点视图 | 已实现并真实运行，视觉待确认 | 卡片墙改为流程节点：模型/工具/回答/进程节点，并行批次横排，状态着色，点击节点在右侧看详情；仅改 observer/index.html |
| 观测台 Trajectory / 迷宫 / 对比 | 已实现并真实运行，视觉待确认 | 参考 DeepSeek Harness 官方 Trajectory 视图与 dsh-maze / trace compare：事件账本 + 瀑布时间轴、确定性的绕路与回退判定、token 与上下文数据轨、多运行对比、回放；观测台读回历史 runs；agent 未改 |
| 方舟模型配置切换 | 已编译并实际调用 | 公共 API、Seed 2.1 Pro、high 推理已通过 Day 2 四个真实场景 |
| Day 2：循环护栏 | 实现与四场景验收完成，理解待反馈 | 预算、指数退避、重复动作熔断；原理、示例与练习集中在 Day 2 学习笔记 |
| Day 2：生产失败类型扩展 | 笔记已补充，理解待反馈 | 分类重试、超时与取消、结果未知与幂等、并行部分成功、状态恢复和权限边界；补充工具封装、执行器与模型的恢复分工及工单案例；仅知识讲解，未新增实现 |
| 检索合并到Agent进程 | 实现、编译与真实运行已完成；理解待反馈 | 删除8092 HTTP客户端和独立检索程序，向量推理、缓存、排序直接由Agent执行；根模块Go1.25、Hugot纯Go后端，本地/线上可选，普通任务不加载模型。8092停止后两种直接检索均命中D05；网页本地问答与重启后续聊答出600ms并保持local，线上问答答出默认预算10且引用D04，均exit 0；普通计算156通过。学习笔记、课件和网页选择已同步；未提交Git |
| Day 4：检索注入与来源问答 | 代码、文档、观测台、课件均已验证；理解待反馈 | Go工具、对比入口与检索模块（后续已并入Agent）；CGO_ENABLED=0编译，本地MiniLM / 线上Doubao两种模式各24条FAQ、8/8可答题hit@3；线上20次问答中RAG答对8/8并有效引用、库外2/2拒答，无检索0/8可答、未观察到编造；32次聊天请求、12次检索，记录195200.394；对比报告见day-04-lab.md，观测台引用跳转与续聊已验证，12页动画经浏览器核对。未提交Git |
| Day 4：评审后完善与笔记重写 | 已编译并真实运行；笔记待用户阅读反馈，理解待反馈 | 线上embedding只请求稠密向量：响应约16KB对2.18MB、耗时约减半，与原稠密向量余弦0.9986；重建索引24次请求后单次查询1.67s（原3.39s），10题hit@3仍8/8、库外2/2全部过滤。-search-docs的stdout只有JSON；方舟embedding报错带出原因与截断响应体；-rag-lab单题失败记录后继续（-max-steps 1实测20题跑完、10题按预期记失败、退出码1）；观测台把未取回或低于阈值的[Dxx]标红。day-04-notes.md按用户要求改为只讲RAG知识（结构、Agent中的位置、切块、相似度、阈值、BM25/RRF/重排、引用与安全、分层评估、失败模式），项目实现、动手步骤与实验记录移到day-04-lab.md；AGENTS.md笔记约定同步。CLI端到端：检索D05后答600ms |
| Day 5：长期记忆 | 已编译并真实运行，观测台无头浏览器验证；理解待反馈 | memory.go 规则写入、开场调入、search/forget 工具、TTL与加权淘汰、文件锁事务；真实运行：两进程召回“偏好Go”并引用[M1]（不开记忆对照组答不出）、1分钟TTL后删除且模型不编造、“改用Rust”时模型调用forget_memory删M1并写M5、always_fail失败写成episodic并被新进程答出、上限2时淘汰0.4的失败记录、密钥拒存且模型如实告知；调试中修正时区不一致与“嘴上说已记下”；观测台调入/写入卡片、[M5]引用、续聊写入M8、面板删除M8均通过且无JS错误。day-05-notes/lab、12页课件、README同步；未提交Git |
| Day 5：Contextual Retrieval 升级 | 已编译、go vet通过；真实模型验证待用户按清单运行；理解待反馈 | memory_retrieval.go：来源上下文、按embedding上限切块、背景生成与缓存、Contextual向量+BM25、RRF、LLM重排、过滤与token预算、降级与失效；recall/search_memory共用；观测台purpose分组；notes第11节、lab（数据流、参数、10项手动验证、与Anthropic差异）、课件16页、README同步。仅使用临时数据核对本地检索、BM25与删除失效，未调用模型；评审后修正：失败记录先脱敏再落盘并统一检查、去重加入前文（同句不同主体分存）、本地按分词器校验“背景+片段”超限记too_long、并发删除后向量不再写回、观测台总量含辅助调用（均已做定向核对）；观测台与课件的视觉和交互待确认；未提交Git |
| Day 1–3 笔记重写 | 文档已完成，待用户阅读反馈；理解待反馈 | 按用户要求将day-01/02/03-notes改为只讲知识的专业讲义：Day 1 工具调用协议、ReAct、消息历史约束、循环职责、并行与执行器安全；Day 2 预算、退避与抖动、多层重试放大、重复检测、错误分类、超时与结果未知、幂等、职责分工；Day 3 上下文工程、记录与视图、容量与用量测量、压缩策略谱系、消息组、摘要设计、前缀缓存、停止边界。项目参数、代码阅读、故障实验、真实压缩案例与方舟实测移到各自的day-0X-lab.md。保留search_notes依赖的"循环职责"原句；Day 3笔记仍足够长，供read_day3_notes观察截断；知识库D12来源改指day-03-lab.md |
| Day 3首版：上下文窗口管理（已重构） | 已编译并通过真实模型对比，理解待反馈 | 两组各33轮：Trim未召回账号，Summarize递归18次后召回；共84次请求，每轮system与最近消息检查通过；工具任务压缩后继续完成，摘要计入4次请求上限并按时停止；输入预算不足时拒绝请求；长tool清理分支已代码检查 |
| Day 3：按上下文系统设计重构 | 实现与主要真实场景验证完成，理解待反馈 | 33轮保留账号和Day 3约束，37次请求含2次摘要；大工具原文保留、视图3402→2000估算token；10步工具链中途压缩后仍引用早期数字；摘要缓存6768/9252；清理收益门槛、连续压缩熔断及上游超窗重试做代码检查 |
| 分步动画课件 | Day 3重排为14页并完成本地浏览器核对，理解待反馈 | docs/slides：Day 1（12页）、Day 2（14页）、Day 3（14页），共用 deck.css/deck.js。Day 3按基本概念、压后组成与曲线、Codex流程、案例和项目附录重排；第7–11页保留Codex动画，第13页集中说明当前Go参数。去掉固定40%谷底，修正“压到绝对最小”和缓存必然命中/全部失效的误解；图表与说明分区。新版目录、分步字幕与卡片布局已浏览器核对，保留原话动画衔接正常。未修改Go压缩逻辑，旧私有发布版未同步 |
| Day 3：输出与压缩容错优化 | 已实现并真实验证请求与K缩减，理解证据见本轮评审 | 请求体确认默认省略输出上限、摘要tool_choice=none且保留工具定义；15步任务18次请求完成，两次K从2减到1；40%到60%的接受分支已代码检查；本次短链路缓存为0，未证明缓存等价 |
| Day 3：观测台实验入口 | 已编译并通过真实浏览器运行，理解待反馈 | 页面直接启动context-lab，经代理记录36次请求含1次摘要；压缩后召回账号与Day 3约束，大工具原文保留；普通问答与参数冲突拒绝通过；终端输出与输入容量显示已检查 |
| 观测台续聊 | 已编译并通过真实模型验证，理解待反馈 | 同一记录连续提问，恢复View、摘要和工具历史；真实计算323→330，服务重启后继续到660，调用编号1到6连续；压缩实验续聊仍召回账号与范围；并发重复提交仅一个成功，另一个409 |
| Day 3：评审后完善 | 已编译并真实运行，理解待反馈 | 摘要输入超窗先删保留区之后的旧组（真实运行见`drop_group=3..5 protected=3`）；压缩布局按真实/粗估比例换算容量；清理与摘要共用keep-groups；仅带工具时发送tool_choice；省略标记不再暗示可读Transcript；剩1次请求时不摘要（真实运行在第5次前以max_steps停止）。33轮实验36次请求含1次摘要、账号仍在View；8步工具链压缩4次完成，正常窗口下答案全对。实测方舟：1个工具定义约430 token；tool_choice=none时不渲染工具定义；推理仅在当前工具循环内计入输入 |
| 观测台改为对话界面 | 已编译并通过真实模型与无头浏览器运行，视觉待用户确认 | 参考 Codex 客户端：左侧对话列表，主区对话流，底部输入框（Enter发送、输入法选词不误发）；原三个视图移入“观测”标签；仅改 observer/index.html。真实新对话两次工具调用后回答4×25=100；续聊2647×3=7941；33轮实验存档显示34条用户消息、1条压缩分隔线且无重复；浅色、深色、390px手机宽度截图检查无横向滚动、无页面报错；失败运行的停止卡片未在浏览器验证（存档中没有失败记录） |
| 观测台消息角色与历史展示（旧版） | 已由统一模型输入/输出替代 | 旧版拆分system、history与当前输入，容易让展示数量与messages长度产生歧义；现展示本次请求的全部messages，聊天页仍保持用户消息角色和新问题识别 |
| Codex压缩策略复核 | 官方文档与2026-10-04源码afb436d已核对，理解待反馈 | 区分本地文本摘要、远端v2和公开Responses API；本地摘要副本报超窗才裁剪，用户原话从会话历史独立收集，预算20k；远端保留消息预算64k；本项目10%/40%/60%为自身选择。allo案例约3237用户文本token可放入Codex本地原话配额，仅源码推演，未运行Codex对照实验 |

## 已知问题

2026-10-05 代码评审时发现，暂不修复；Day 5 引入记忆注入时一并考虑。

| 问题 | 位置 | 现象 | 改进方向 |
| --- | --- | --- | --- |
| 熔断器只识别相邻重复 | `react.go` 重复动作检查 | 计数跨轮累积，但只和上一个动作比较：A,A,A 会触发；A,B,A,B… 每次都重置为1，永不触发，只能靠 max_steps 兜底 | 保留执行前的相邻重复检查（在副作用前拦截）；执行后按滑动窗口统计 (动作, 结果哈希)，最近N步内同一对出现≥3次即判定无进展循环，结果在变的轮询不误杀 |
| 低分片段仍暴露标题和来源 | `retrieval.go` searchDocs 低分分支 | accepted=false 时只清空 text，id/title/source/score 仍进入上下文；分数略低于阈值且标题与问题高度相关时，模型可能按标题和通用知识补写答案，甚至引用该 [Dxx] | 低分片段同时去掉 title/source，或只返回 status 与最高分 |
| 资料不足后的重搜次数无约束 | `react.go` systemPrompt | status=insufficient 后，prompt 未规定能否改写查询重搜；每次换说法参数都不同，熔断器拦不住，可能一直耗到 max_steps | prompt 规定最多改写重搜1次，仍不足就说明资料不足 |
| 库外问题的回答方式有歧义 | `react.go` systemPrompt | "区分通用知识与项目事实"与"只说明资料不足"冲突：对天气等库外问题，模型可以用通用知识回答，也可以只说资料不足 | 在 prompt 中明确选择其一 |
| 已注入的记忆仍被重复检索 | `memory.go` 记忆规则；`react.go` systemPrompt | Day 5 实测：M6 已在 system 记忆块中，模型仍调用 search_memory 再查一次，多一次请求 | 观察更多样本后再决定是否调整提示；不为单次现象改规则 |
| ~~被取消的调用从 summary 中消失~~（Day 8 已修复） | `react.go` | 已按 Status 区分成功 / 失败 / 未执行 / 结果未知，summary 与 tool 结果逐项写出 | 详见 [深度问题 Q3–Q4](docs/deep-questions.md) 与 [Day 8 项目实践](docs/day-08/day-08-lab.md) |

补充（2026-10-06，Day 6–7 / Bonus 引入）：

| 问题 | 位置 | 现象 | 改进方向 |
| --- | --- | --- | --- |
| MCP 子进程继承全部环境变量 | `mcp.go` connectMCP（mcp-go 用 os.Environ()+额外变量） | 环境变量里的 ARK_API_KEY 等凭据会被第三方 server 读到，违反最小权限 | 用 transport 的自定义命令函数只传入白名单变量；Week 2 沙箱时一并处理 |
| ~~MCP 工具执行错误也被重试~~（Day 8 已修复） | `mcp.go` Call | isError 以 `llm.Permanent` 返回，不再重试 | — |
| MCP 只支持单 server、启动时一次性拉取 | `main.go`、`mcp.go` | 不处理 list_changed；多 server 时前缀需含 server 标识 | 需要多 server 时再扩展 |
| schema 校验错误信息不友好 | mcp-go WithInputSchemaValidation | 返回 `&{Got:number Want:[string]}` 这类 Go 结构体格式 | 必要时在 handler 中自行校验并给出中文说明 |
| skill 触发准确率未评估 | `skills.go`、system 索引 | 只观察了1个相关、1个无关问题 | 纳入 Week 3 评测集 |

补充（2026-10-07，Day 8–10 / 子 agent 引入）：

| 问题 | 位置 | 现象 | 改进方向 |
| --- | --- | --- | --- |
| 放弃等待后工具仍在后台运行 | `react.go` runAttempt | 不配合取消的工具会继续执行到返回，副作用照样发生 | Day 12 把这类工具放进可整体杀掉的独立进程或容器 |
| 模型请求失败不在 agent 内重试 | `react.go`、`llm.go` | 一次 model_error 就停止，失败请求也占预算；依赖队列在任务层续跑 | 对 429/5xx 做有限次退避重试，并区分是否计入预算 |
| 父进程被 kill -9 后子进程靠 SIGPIPE 间接退出 | `child.go` | 不打印的阶段子进程会继续运行并持有锁，父续跑时可能报 ErrBusy | Linux 可用 Pdeathsig；或父进程续跑时等待锁释放 |
| 检查点不记录程序版本、不 fsync、不自动清理 | `checkpoint.go` | 改代码后续跑旧检查点可能不一致；断电可能丢最后一次写入；文件持续累积 | 需要时加版本号与清理命令 |
| ~~子 agent 未接入观测台~~（已完成） | observer | 按 `X-Agent-Task` 区分父子请求，子 agent 在右侧侧栏显示 | — |
| ~~停止后不能在页面续聊~~（已完成） | observer、`context_manager.go` | 中途停下的对话从检查点续聊，被打断的调用以“未执行/结果未知”交给模型 | — |
| unknown 后模型可能重做有副作用的工具 | 子 agent 实验 3.2 | 框架如实报告结果未知，模型仍选择重做 slow_job | 有副作用的工具提供幂等键或状态查询接口；或高风险操作需人工确认（Week 3） |

## 学习记录

用户提出的有深度的问题集中记录在 [值得反复琢磨的问题](docs/deep-questions.md)，新问题持续追加。

Day 5：[Day 5 学习笔记](docs/day-05/day-05-notes.md) 讲记忆分层、写读忘的取舍、遗忘与记忆污染；实现、两轮完整日志与TTL/更正/淘汰实验见 [Day 5 项目实践](docs/day-05/day-05-lab.md)，[课件](docs/slides/day-05.html) 20页（第9–16页为Contextual Retrieval：片段缺上下文、流程动画、向量与BM25原理、BM25算例、min-max与RRF对比、交叉编码器与大模型重排、缓存键与锁外计算时间线、预算降级与原文数字；字幕按笔记第8、11节重写）。笔记第8节按机制重写工程问题（原子重命名、丢失更新时间线、进程锁与flock、慢操作不持锁、依据与派生数据）；第11节扩为12小节，讲召回—重排漏斗与recall@k、切块与静默截断、背景补充及提示词缓存成本算例、双编码器与余弦、BM25逐项拆解与完整算例（加背景前1.05:0.91，加背景后3.13:0.51）、min-max与RRF对比算例、交叉编码器与大模型重排三种方式、缓存键、乐观并发时间线、预算与降级；用户反馈旧版“太泛”，理解待复述确认；lab第5节是10项手动验证清单，结果待用户真实运行。真实运行不等于用户已掌握；“短期与长期记忆的分工”“为什么遗忘是必需的”待用户复述确认。

Day 6：[Day 6 学习笔记](docs/day-06/day-06-notes.md) 讲 MCP 要解决的问题、JSON-RPC 与两种传输、三原语、旧版握手交换的协议版本与 capabilities 及协商的意义、2026-07-28 版去握手（每请求 `_meta` + `server/discover`）与新旧共存、工具定义转换、协议错误与执行错误；实现与真实日志见 [Day 6 项目实践](docs/day-06/day-06-lab.md)。用户任务描述以“initialize 握手”为主线，现行规范已改，笔记两代都讲。“握手交换哪两样东西、为什么需要 capabilities 协商”待用户复述确认。

Day 7：[Day 7 学习笔记](docs/day-07/day-07-notes.md) 讲 server 职责、CallToolResult、错误选择（按 SEP-1303 参数校验错误用 isError，纠正任务描述中“参数无效走 -32602”的旧说法）、工具粒度、无状态与显式句柄、信任边界与三层校验、最小权限；实验见 [Day 7 项目实践](docs/day-07/day-07-lab.md)。周复盘见 [Week 1 复盘](docs/week-01-review.md)。“为什么 server 端必须重做参数校验”待用户复述确认。

Bonus：[Skills 学习笔记](docs/bonus-skills/skills-notes.md) 讲 skill / MCP tool / 程序性记忆分工、三级渐进式加载、frontmatter 规范、触发方式、供应链风险与“只做知识型”的边界（任务中“约9%为critical”的出处未找到，改引 Snyk 13.4% 与 0.52%–46.8% 的方法差异）；实现见 [Skills 项目实践](docs/bonus-skills/skills-lab.md)。三者分工与“为什么不执行脚本”待用户复述确认。

Day 8–10 与子 agent：[Day 8 笔记](docs/day-08/day-08-notes.md) 讲协作式取消与上下文树、超时层次与时限传递、四种结局、重试决策与错误分类、优雅停止；[Day 9 笔记](docs/day-09/day-09-notes.md) 讲恢复所需状态、写前日志与崩溃窗口、原子替换与持久性、中断调用的处理、快照式与事件重放式、单一执行者（flock、租约与防护令牌）；[Day 10 笔记](docs/day-10/day-10-notes.md) 讲队列、并发上限与利特尔法则、三种投递语义、幂等键、任务级失败与死信队列、进程隔离；[子 agent 笔记](docs/bonus-subagents/subagents-notes.md) 讲上下文隔离、代价、适用场景、任务说明、权限继承与运行时的关系。各自的 lab 记录了真实运行。“结果未知为什么不能当失败”“写前日志解决哪个崩溃窗口”“恰好一次如何实现”“子 agent 首要解决什么问题”待用户复述确认。

每日推送的阅读材料按天记录在 [阅读材料](docs/reading-list.md)，新链接持续追加。

Day 1 的工具定义、消息历史和并行调用见 [Day 1 学习笔记](docs/day-01/day-01-notes.md)，本项目实现与动手见 [Day 1 项目实践](docs/day-01/day-01-lab.md)。启动与模型配置见 [README](README.md)。

建议先读 `agent.Run` 的两次 history append，再读 `agent.ExecuteBatch` 的结果槽与 WaitGroup。理解情况仍待用户实际解释或练习反馈。

观测台原理与验证见 [观测台笔记](docs/observer/observer-notes.md)。

Day 2：[Day 2 学习笔记](docs/day-02/day-02-notes.md)，故障实验见 [Day 2 项目实践](docs/day-02/day-02-lab.md)。用户掌握情况仍待复述和追问反馈。

Day 3：[Day 3 学习笔记](docs/day-03/day-03-notes.md)讲上下文管理原理；真实压缩实例（旧原文、摘要和新请求的4条消息）、方舟实测与参数见 [Day 3 项目实践](docs/day-03/day-03-lab.md)。用户反馈压缩流程仍未理解，已补充逐步讲解与信息损失示例，理解情况待复述确认。另有[上下文系统设计](docs/day-03/context-system-design.md)：先调研 Codex 源码与 Claude API / Claude Code 文档，再给出本项目方案（完整记录与视图分离、写入时截断、90%触发、保留用户原话和最近消息组的摘要压缩），已据此重构，真实验证进度见上表。Codex 的完整调研见 [Codex 上下文管理调研](docs/day-03/codex-context-research.md)。

Day 4：[Day 4学习笔记](docs/day-04/day-04-notes.md)，实现与10题实验见 [Day 4 项目实践](docs/day-04/day-04-lab.md)。从窗口外知识到role=tool回流，结合分数与正文判断依据，再核对引用。真实运行不等于用户已掌握，理解仍待复述或练习。
