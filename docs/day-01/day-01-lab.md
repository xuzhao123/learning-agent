# Day 1 项目实践：用 Go 实现原生工具调用循环

原理见 [Day 1 学习笔记](day-01-notes.md)。本文记录本项目的实现、代码阅读顺序与动手步骤。启动与模型配置见根目录 [README](../../README.md)。

## 1. 实现概览

| 设计点 | 本项目的选择 |
| --- | --- |
| 协议 | 方舟 Chat Completions 原生 Tool Calling：`tools` 发送定义，`tool_calls` 接收请求，`role=tool` + `tool_call_id` 回填结果 |
| system prompt | 只有一句："你是一个工具助手，按需使用工具帮助用户，并用中文回答。"不要求 Thought / Action 文本格式 |
| 默认工具 | `calculator`（解析数学 AST，不执行代码）、`get_current_datetime`、`search_notes`（按行检索本篇学习笔记） |
| 并发 | `-parallel` 默认 4，范围 1–16；每轮最多 16 项调用 |
| 终止 | 没有工具调用且有正常回答时结束；Day 2 加入预算与熔断 |
| 历史 | 保存在当前进程内存；Day 3 起由上下文管理器维护 |

## 2. 沿一次任务走一遍

问题："计算 `floor(sqrt(1234*5678))`，并查询今天星期几。"

1. 第 1 次请求：`system + user`，附带三个工具定义。
2. 模型同轮返回两个调用：`calculator(expression="floor(sqrt(1234*5678))")` 与 `get_current_datetime()`，各带 ID。
3. 执行器并行运行两者，calculator 返回 2647。
4. 两条 tool 消息按原顺序写回，`tool_call_id` 各自对应。
5. 第 2 次请求带上全部历史，模型据结果回答。

依赖任务的例子："先查学习笔记中的循环职责，再计算职责数量乘以 25"。第 1 轮调用 `search_notes` 查到四项职责；第 2 轮才能提出 `4*25`；第 3 轮回答 100。

## 3. 关键代码

参数解析（[tools.go](../../tools.go)）：

```go
var args struct {
    Expression string `json:"expression"`
}
err := json.Unmarshal([]byte(call.Function.Arguments), &args)
```

历史的两处追加（[react.go](../../react.go)，Day 3 后经上下文管理器写入）：

```go
conversation.RecordReply(response)  // 模型要求做什么（原样保存assistant消息）
conversation.Append(Message{Role: "tool", ToolCallID: observation.ID, Content: string(data)})  // 实际做成什么
```

当前 tool 消息的 content 除结果外还包含工具名、调用 ID 和尝试次数，便于观察。

## 4. 代码阅读顺序

| 位置 | 带着什么问题读 |
| --- | --- |
| [react.go](../../react.go) 的 `runAgent` | 哪一行请求模型？哪两处更新历史？何时结束？ |
| [tools.go](../../tools.go) 的 `toolDefinitions`、`runTool` | 定义和函数怎样对应？参数在哪里解析和校验？ |
| react.go 的 `executeBatch` | 并发名额怎么控制？结果为什么不会串到别的调用上？ |
| [llm.go](../../llm.go) 的 `callModel` | messages 和 tools 怎么发送？响应从哪里读出？ |
| [main.go](../../main.go) | 问题、配置和运行参数如何进入循环？ |

`executeBatch` 的结构：goroutine 发起执行，带缓冲 channel 控制名额，每项写入自己的 `results[i]`，WaitGroup 等整批结束后再由主循环更新历史。

## 5. 动手

在项目根目录的 `.env` 填写 `ARK_API_KEY` 后运行：

```sh
go run . -question '请用 calculator 计算 floor(sqrt(1234*5678))，并查询当前日期和星期，两项可以同轮调用。'
go run . -question '请先用 search_notes 查询循环职责，再用 calculator 计算职责数量乘以25。'
go run . -parallel 1 -question '请用 calculator 分别计算 1024*7 和 243*9，两项同轮调用。'
```

- 第一题：观察两个独立调用如何进入同一批，结果如何对应各自的 ID。
- 第二题：观察历史怎样增长，模型在拿到查询结果之后才使用数量 4 继续计算。
- 第三题：并发为 1 时两个调用不再重叠，但模型轮数不变。

也可以在观测台（`go -C observer run .`）中提问，在"观测"标签里查看每次请求的完整 messages。
