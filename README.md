# 学习 agent：Day 1 → Day 2 → Day 3

想先看动画讲解，打开 [课件入口](docs/slides/index.html)：每节课一份可翻页、按步骤播放的 HTML 课件。

每天只读一篇笔记：先学 [Day 1 工具调用与循环](docs/day-01/day-01-notes.md)，再学 [Day 2 循环护栏](docs/day-02/day-02-notes.md) 和 [Day 3 上下文管理](docs/day-03/day-03-notes.md)。每篇包含原理、例子、关键代码和自己动手的练习。

用 Go 手写最小 ReAct loop，重点是看懂流程：

`问题 → 选择本轮上下文 → 请求模型 → 读取 tool_calls → 并行执行 → tool 结果写回历史 → 下一轮或结束`

使用方舟原生 Tool Calling。system prompt 只说明工具助手角色，回答可以自由表达；工具定义通过 `tools` 提供，调用参数由模型生成，不要求 Thought / Actions / Final Answer 文本格式。

## 运行

在项目根目录运行（Mac：`/Users/bytedance/workspace-vm/learning-agent`；VM：`/home/dev/workspace/learning-agent`）。首次配置参考 [.env.example](.env.example)，将自己的 ARK_API_KEY 填入 .env，然后输入问题：

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

默认三个工具是 calculator、get_current_datetime、search_notes；显式 `-lab-tools` 增加 always_fail 和 check_task_status 两个实验工具。学习笔记检索读取 `docs/day-01/day-01-notes.md`，工具选择和参数由模型生成。

## Day 3 上下文系统

```sh
go run . -context-lab -max-steps 60 -context-window 8192 -max-output-tokens 2048 -reasoning-effort minimal
```

完整Transcript保留在内存，模型读取View；写入时截断长工具结果，90%触发处理，40%为软目标；摘要后在60%内可接受，超过40%时提醒后继续。先尝试清理旧tool结果，收益不足再做原生前缀摘要。普通请求、摘要和超窗重试共用调用预算。实验统一minimal推理，所有回答与摘要均为真实调用，不保存实验日志。原理见 [Day 3笔记](docs/day-03/day-03-notes.md)。

旧的context-strategy、context-tokens和keep-recent已由统一策略、窗口与消息组配置替代。默认输入容量仍按12288−4096−1024=7168规划：未显式设输出上限时，4096只作为本地预算预留，不发送给API；显式设限时则按该值预留。省略参数仍受供应商默认限制，不能理解为无限输出。

## 观测台

`observer/` 是独立程序（有自己的 go.mod）。网页是类似 Codex 客户端的对话界面：左侧对话列表，主区显示用户消息、思考、工具调用和回答，底部输入框发送。顶栏切到“观测”有三个视图：轨迹（事件账本 + 瀑布时间轴）、迷宫（主路径、绕路、回退 + token 与上下文压力数据轨）、对比（2–5 次运行按轮次对齐），并支持回放：

```sh
cd observer && go run .
# 打开 http://127.0.0.1:8090
```

回答结束后，在底部输入下一句话并按 Enter，会延续同一条对话；也可以先选择左侧的历史记录再续聊。点击“新对话”开始独立任务。运行中禁止重复发送，观测台重启后仍可从存档恢复。每个新问题重新获得默认10次模型请求预算，页面Turn编号则在整条对话中持续递增。

点击左侧“Day 3 上下文实验”并确认，即可在对话流中看到33轮对话、压缩分隔线和大工具调用。它等价于 `go run . -context-lab -max-steps 60 -reasoning-effort minimal`，由观测台启动并接入代理，无需另开终端执行实验命令。切到“观测”，点击“终端输出”查看 `Context`、`Compact`、`Recall` 和 `Lab complete`；“迷宫”查看上下文压力与压缩位置。更新观测台源码后，需要重启观测台并刷新网页。

单独在项目根目录运行实验会直连方舟，不会自动出现在观测台；已经绕过代理的对话无法事后补录。通过观测台启动时，沿用观测台的 `observer/runs/` 存档；实验程序本身不另写日志。

观测台把 agent 的 `LLM_API_URL` 指向本机代理，从模型协议本身还原过程，agent 代码不含观测台专用埋点。摘要作为独立模型调用展示；视图重建在上下文压力轨中标记，工具结果按调用ID跨请求关联。详见 [观测台笔记](docs/observer/observer-notes.md)。

## 配置与学习

默认使用 `https://ark.cn-beijing.volces.com/api/v3/chat/completions`、`doubao-seed-2-1-pro-260628` 和 `reasoning_effort: high`；模型请求等待上限为5分钟。普通请求和摘要共享推理配置及工具定义；摘要额外使用tool_choice: none禁止调用工具。输出上限默认省略，只有显式配置时才发送；可通过reasoning-effort显式调整整次运行的推理强度。

读取本地 `.env`，环境变量优先。支持 `ARK_API_KEY`（兼容 `LLM_API_KEY`）、`LLM_API_URL` 和 `LLM_MODEL`。密钥只放在本地私有配置，权限保持0600，`.gitignore` 已排除它。

自行在 `.env` 填写 `ARK_API_KEY`。修改模型上游地址后，重启观测台以读取新地址。

代码保持直接的函数分工，不编写 test 文件或脚本模型。通过格式化、编译和真实运行观察结果：

```sh
gofmt -w *.go
go build -o /tmp/learning-agent .
```

[Day 1 学习笔记](docs/day-01/day-01-notes.md)

项目目标与约定见 [PROJECT.md](PROJECT.md) 和 [AGENTS.md](AGENTS.md)。
