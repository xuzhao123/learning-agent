# 用 Go 学习 Agent

从最小工具调用循环出发，逐步学习上下文、检索、记忆、MCP、运行时和安全边界。长期目标见 [岗位能力对照](JOB_REQUIREMENTS.md)，当前进度见 [PROJECT.md](PROJECT.md)。

**按模块系统学习，从 [Agent Wiki](wiki/README.md) 开始。** Wiki 分基础理解与技术展开两层，提供术语、数据结构、算法算例、Go片段、故障推演和源码对照；每日 notes 与 lab 保留按天学习和动手路径，见下方学习目录。

`用户问题 → 组织上下文 → 请求模型 → tool_calls → 执行工具 → 结果回填 → 下一轮或结束`

## 快速开始

需要 Go 1.25.5 或以上版本。所有命令从项目根目录执行，文档中的文件路径相对于项目根目录。

```sh
cp .env.example .env
# 在 .env 中填写自己的 ARK_API_KEY
go run . -question '计算 1231+23123'
```

不带 `-question` 时从终端交互输入。启动观测台：

```sh
go run . observe
# 打开 http://127.0.0.1:8090
```

观测台支持新对话、续聊、停止，以及每个模型 Step 的完整输入输出。开发时用 `go run . observe -dev`，每次对话从当前源码启动 agent；默认使用当前程序，修改 agent 后需要重启观测台。使用方法见 [观测台说明](docs/observer/observer-notes.md)。

需要可执行文件时：

```sh
go build -o bin/learning-agent .
./bin/learning-agent -question '先查学习笔记中的循环职责，再计算职责数量乘以25。'
```

## 模型与工具配置

配置读取根目录 `.env`，同名环境变量优先。密钥只写入本地私有配置，保持文件权限为 0600；`.env` 已忽略提交。

| 模式 | 配置项 | 默认值 |
| --- | --- | --- |
| 方舟聊天模型 | `ARK_API_KEY`（兼容 `LLM_API_KEY`）、`LLM_API_URL`、`LLM_MODEL` | `-provider ark`；`https://ark.cn-beijing.volces.com/api/v3/chat/completions`；`doubao-seed-2-1-pro-260628` |
| DeepSeek 聊天模型 | `DEEPSEEK_API_KEY`、`DEEPSEEK_API_URL`、`DEEPSEEK_MODEL` | `-provider deepseek`；`https://api.deepseek.com/chat/completions`；`deepseek-flash` |
| 线上向量模型 | 与方舟共用 `ARK_API_KEY` | `-embedding ark` |
| 本地向量模型 | 不需要 embedding API 密钥；首次使用会下载模型 | `-embedding local`；聊天请求仍使用选定的线上模型 |
| 浏览器 | 本机安装 Chrome/Chromium；`CHROME_PATH` 可指定可执行文件 | `-browser`；仅环境变量读取浏览器配置 |
| Bash 沙箱 | Linux、bubblewrap；cgroup 资源限制依赖 systemd 用户会话 | `-bash`；缺少资源限制时的行为见 [Day 12 实践](docs/day-12/day-12-lab.md) |
| 调用链追踪 | 无需配置，span 写入 `.data/traces/`；`OTEL_EXPORTER_OTLP_ENDPOINT` 另发给 Jaeger 等 OTLP 平台；`OTEL_INSTRUMENTATION_GENAI_CAPTURE_MESSAGE_CONTENT=true` 采集提示词与工具内容 | 默认只记元数据；接入步骤见 [Day 13 实践](docs/day-13/day-13-lab.md#4-接入外部平台) |

默认使用原生 Tool Calling：工具定义在请求的 `tools` 字段，模型返回 `tool_calls`，回答不限制文本格式。普通任务只启用 calculator、get_current_datetime、search_notes；检索、记忆、MCP、Skills、子 agent、浏览器和 Bash 按需开启。

推理强度默认 `high`。API 输出上限默认省略，显式设置 `-max-output-tokens` 才发送；本地上下文规划仍保留输出空间，省略 API 参数不代表无限输出。向量检索直接在 agent 进程内执行。

## 学习目录

推荐顺序：**笔记理解原理 → 实践阅读源码与动手 → 课件复习流程 → 自测复述**。每天的笔记讲通用知识，实践讲本项目实现；下表的“—”表示尚无该节课件。

| 课程 | 主题 | 学习笔记 | 项目实践 | 动画课件 |
| --- | --- | --- | --- | --- |
| Day 1 | 工具调用与 ReAct loop | [笔记](docs/day-01/day-01-notes.md) | [实践](docs/day-01/day-01-lab.md) | [课件](docs/slides/day-01.html) |
| Day 2 | 循环护栏与错误反馈 | [笔记](docs/day-02/day-02-notes.md) | [实践](docs/day-02/day-02-lab.md) | [课件](docs/slides/day-02.html) |
| Day 3 | 上下文窗口与压缩 | [笔记](docs/day-03/day-03-notes.md) | [实践](docs/day-03/day-03-lab.md) | [课件](docs/slides/day-03.html) |
| Day 4 | 按需检索、证据与引用 | [笔记](docs/day-04/day-04-notes.md) | [实践](docs/day-04/day-04-lab.md) | [课件](docs/slides/day-04.html) |
| Day 5 | 长期记忆与记忆检索 | [笔记](docs/day-05/day-05-notes.md) | [实践](docs/day-05/day-05-lab.md) | [课件](docs/slides/day-05.html) |
| Day 6 | MCP client | [笔记](docs/day-06/day-06-notes.md) | [实践](docs/day-06/day-06-lab.md) | — |
| Day 7 | MCP server | [笔记](docs/day-07/day-07-notes.md) | [实践](docs/day-07/day-07-lab.md) | — |
| Day 8 | 取消、超时与分类重试 | [笔记](docs/day-08/day-08-notes.md) | [实践](docs/day-08/day-08-lab.md) | — |
| Day 9 | 检查点与恢复 | [笔记](docs/day-09/day-09-notes.md) | [实践](docs/day-09/day-09-lab.md) | — |
| Day 10 | 任务队列与幂等 | [笔记](docs/day-10/day-10-notes.md) | [实践](docs/day-10/day-10-lab.md) | — |
| Day 11 | 浏览器自动化 | [笔记](docs/day-11/day-11-notes.md) | [实践](docs/day-11/day-11-lab.md) | — |
| Day 12 | 代码执行沙箱 | [笔记](docs/day-12/day-12-notes.md) | [实践](docs/day-12/day-12-lab.md) | — |
| Day 13 | 可观测性：调用链追踪 | [笔记](docs/day-13/day-13-notes.md) | [实践](docs/day-13/day-13-lab.md) | — |
| Day 14 | 第二周综合与复盘 | [复盘](docs/week-02-review.md) | [实践](docs/day-14/day-14-lab.md) | — |
| Day 15 | 评测指标：成功、可靠、延迟、成本 | [笔记](docs/day-15/day-15-notes.md) | [实践](docs/day-15/day-15-lab.md) | — |
| 扩展 | Agent Skills | [笔记](docs/bonus-skills/skills-notes.md) | [实践](docs/bonus-skills/skills-lab.md) | — |
| 扩展 | 子 agent | [笔记](docs/bonus-subagents/subagents-notes.md) | [实践](docs/bonus-subagents/subagents-lab.md) | — |
| 扩展 | 模型路由 | [笔记](docs/bonus-routing/routing-notes.md) | [实践](docs/bonus-routing/routing-lab.md) | — |
| 扩展 | agent 与界面的结构化协议（B0） | [笔记](docs/bonus-protocol/protocol-notes.md) | [实践](docs/bonus-protocol/protocol-lab.md) | — |

补充阅读：

- [28 天学习计划](plan.md)：课程安排与长期目标；[进阶计划](plan-advanced.md)：从单机 agent 到生产级服务的改造清单；[第一周复盘](docs/week-01-review.md)：执行循环各模块的关系；[第二周复盘](docs/week-02-review.md)：一次工具调用怎样穿过运行时、检查点、队列、沙箱与调用链。
- [深度问题](docs/deep-questions.md)：按主题复习设计取舍；[阅读材料](docs/reading-list.md)：原始资料与各课参考入口。
- [Agent 工程面试题库](wiki/16-interview-preparation.md)：126 题，含参考回答、追问、系统设计、Go 编码与 GitHub 资料选择。
- [Day 3 设计](docs/day-03/context-system-design.md)：本项目的上下文方案；[Codex 调研](docs/day-03/codex-context-research.md)：带版本范围的外部实现研究。
- [课件入口](docs/slides/index.html)：当前覆盖 Day 1–5。

## 源码阅读

项目只有一个 Go 模块、一个入口 `main.go`。先看入口怎样组装工具，再按当天课程进入对应模块，完整源码不复制进学习文档。

| 阅读顺序 | 文件或模块 | 关注点 |
| --- | --- | --- |
| 入口 | [main.go](main.go) | 参数、工具开关、agent / observe / queue 的入口 |
| 模型协议 | [internal/llm](internal/llm/) | messages、tools、tool_calls、请求预算与用量 |
| 执行循环 | [internal/agent/react.go](internal/agent/react.go)、[工具分发](internal/agent/dispatch.go) | 结果回填、并发、分类重试与终止 |
| 上下文与恢复 | [上下文管理](internal/agent/context_manager.go)、[检查点](internal/agent/checkpoint.go) | Transcript/View、压缩、写前日志、续跑 |
| 知识与记忆 | [retrieval](internal/retrieval/)、[memory](internal/memory/) | 向量检索、规则写入、两路召回与重排 |
| 工具扩展 | [tools](internal/tools/)、[mcp](internal/mcp/)、[skills](internal/skills/) | 本地工具、协议服务、按需加载知识 |
| 多任务执行 | [queue](internal/queue/)、[子进程](internal/agent/child.go)、[子 agent](internal/agent/subagent.go) | worker、任务 ID、独立上下文与权限继承 |
| 外部执行 | [browser](internal/browser/)、[sandbox](internal/sandbox/)、[netguard](internal/netguard/) | 页面读取、进程隔离、网络与资源边界、出口地址判断 |
| 观测与实验 | [observer](internal/observer/)、[protocol](internal/protocol/)、[telemetry](internal/telemetry/)、[metrics](internal/metrics/)、[labs](internal/labs/) | 模型请求代理、agent 与界面的结构化协议、OpenTelemetry span 与导出、D15 指标、上下文与检索实验 |

一个 agent 进程运行一个任务，功能开关由入口设置。观测台、任务队列和子 agent 用独立子进程执行任务；检索等工具直接在该 agent 进程内调用。具体运行边界见 [PROJECT.md](PROJECT.md)。

## 参数速查

| 常用参数 | 默认值 | 作用 |
| --- | --- | --- |
| `-question` | 交互输入 | 本轮任务 |
| `-provider` | ark | 选择方舟或 DeepSeek |
| `-max-steps` | 10 | 模型请求预算，含摘要与辅助调用 |
| `-parallel` | 4 | 独立工具并发上限 |
| `-rag` / `-memory` | false | 按需启用知识检索或长期记忆 |
| `-resume` | 空 | 按检查点 ID 续跑，配置沿用检查点 |

<details>
<summary>展开全部 agent 参数</summary>

| 参数 | 默认值 | 用途 |
| --- | --- | --- |
| `-question` | 交互输入 | 交给模型的任务 |
| `-history-stdin` | false | 从stdin接收上次上下文JSON，配合-question续聊（命令行用；观测台改经 `turn/start` 传入） |
| `-continues` | 空 | 续聊时上一轮的检查点ID：本轮用新ID并记下续接关系，子 agent 可按 task_id 引用之前各轮的子任务；观测台自动处理 |
| `-app-server` | false | B0：以结构化协议与界面通信（JSON-RPC，一行一条）：stdout 只走协议，日志改走 stderr；观测台与父 agent 启动子进程时使用 |
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
| `-bash` | false | Day 12：增加 bash 与 request_network_access，命令在 bubblewrap 沙箱里执行（仅 Linux） |
| `-bash-project-ro` | false | 配合 `-bash`：项目只读挂到 `/project`（`.env`、`.data`、`.git`、`bin` 除外） |
| `-net-allow` | 空 | 配合 `-bash`：沙箱可经代理访问的域名（含子域名，可重复），由用户批准后添加 |
| `-browser` | false | Day 11：增加 web_search 与 open_page，用无头 Chrome 查资料；环境变量 `CHROME_PATH` 指定浏览器，系统不支持 Chrome 沙箱时设 `CHROME_NO_SANDBOX=1` |

</details>

观测台参数见 [启动说明](docs/observer/observer-notes.md#1-启动)，队列参数与用法见 [Day 10 实践](docs/day-10/day-10-lab.md)。`go run . metrics [-since 168h] [-json]` 统计 D15 的 5 个核心指标，见 [Day 15 实践](docs/day-15/day-15-lab.md)。协作与文档维护规则见 [AGENTS.md](AGENTS.md)。
