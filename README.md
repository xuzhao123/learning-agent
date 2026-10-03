# 学习 agent：Day 1 → Day 2

每天只读一篇笔记：先学 [Day 1 工具调用与循环](docs/day-01/day-01-notes.md)，再学 [Day 2 循环护栏](docs/day-02/day-02-notes.md)。每篇包含原理、例子、关键代码和自己动手的练习。

用 Go 手写最小 ReAct loop，重点是看懂流程：

`问题 → 请求模型 → 读取 tool_calls → 并行执行 → tool 结果写回历史 → 下一轮或结束`

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
| `-parallel` | 4 | 工具执行并发，1到16 |
| `-max-steps` | 10 | 模型轮数预算，包含最终回答轮 |
| `-retries` | 2 | 工具额外重试次数，0到5 |
| `-lab-tools` | false | 显式开启 Day 2 故障工具 |

工具失败按200ms起步指数退避，耗尽后将错误回填；连续第三次相同动作拦下整批。超限或熔断会输出未完成与已执行步骤摘要。详见 [Day 2 学习笔记](docs/day-02/day-02-notes.md)。

## 四个源码文件

| 文件 | 阅读重点 |
| --- | --- |
| [main.go](main.go) | 输入问题、读取方舟配置、启动任务 |
| [llm.go](llm.go) | 发送消息历史与 tools，接收真实方舟模型消息 |
| [react.go](react.go) | 原生调用结构、并行调度、tool 结果回流、终止 |
| [tools.go](tools.go) | 工具 Schema、实际执行、实际笔记检索 |

默认三个工具是 calculator、get_current_datetime、search_notes；显式 `-lab-tools` 增加 always_fail 和 check_task_status 两个实验工具。学习笔记检索读取 `docs/day-01/day-01-notes.md`，工具选择和参数由模型生成。


## 配置与学习

默认使用 `https://ark.cn-beijing.volces.com/api/v3/chat/completions`、`doubao-seed-2-1-pro-260628` 和 `reasoning_effort: high`；模型请求等待上限为5分钟。

读取本地 `.env`，环境变量优先。支持 `ARK_API_KEY`（兼容 `LLM_API_KEY`）、`LLM_API_URL` 和 `LLM_MODEL`。密钥只放在本地私有配置，权限保持0600，`.gitignore` 已排除它。

自行在 `.env` 填写 `ARK_API_KEY`。

代码保持四个文件，不编写 test 文件或脚本模型。通过格式化、编译和真实运行观察结果：

```sh
gofmt -w *.go
go build -o /tmp/learning-agent .
```

[Day 1 学习笔记](docs/day-01/day-01-notes.md)
