# 扩展实践：模型流式输出（B1）

原理见 [流式输出笔记](streaming-notes.md)，来由见 [进阶计划 B1](../../plan-advanced.md)。拼接逻辑在 [internal/llm/stream.go](../../internal/llm/stream.go)，请求在 [llm.go](../../internal/llm/llm.go) 的 `callModel`，观测台转发在 [observer/server.go](../../internal/observer/server.go) 的 `relayStream`，页面在 [index.html](../../internal/observer/index.html) 的 `applyNotify` 与 `renderCall`。

## 1. 改了什么

| 以前 | 现在 |
| --- | --- |
| 请求 `"stream": false`，等整段回复 | `"stream": true` 并带 `stream_options.include_usage`；所有模型请求（主任务、摘要、记忆辅助、评审、归因）都走流式 |
| 终端在回复结束后整段打印正文 | 主任务的正文边收边打印 |
| 协议里的 modelCall Item 只有 started、completed | 主任务与摘要多了 `item/delta`（`item_id`、`field` 为 `content` 或 `reasoning`、`delta`）；completed 带 `first_token_ms` |
| 观测台代理整段读完上游再转给 agent | 是流式时逐行转发并 flush；同时用同一份 `llm.Stream` 拼回完整响应留档，记 `chunks` 与 `first_token_ms` |
| 页面在响应返回前只显示“思考中…” | 推理与正文随 `item/delta` 实时显示；响应到达后换成正式渲染 |
| 观测详情写“非流式，无 TTFT 拆分” | 显示首字延迟与生成时间、chunk 数 |

没有改变的：工具仍在整次回复结束后执行；截断、空回答、意外工具调用的检查规则不变，只是改为对拼好的回复做；检查点、D15 指标、续聊恢复、D16–17 评测与归因读的仍是完整响应。

## 2. 读代码

### 2.1 一份拼接逻辑，两处使用

[`llm.Stream.Add`](../../internal/llm/stream.go) 每次处理一行 SSE，返回这一行带来的正文与推理增量：

```go
for _, d := range choice.Delta.ToolCalls {
	for len(m.ToolCalls) <= d.Index { // 新 index：新调用开始
		m.ToolCalls = append(m.ToolCalls, ToolCall{Type: "function"})
	}
	call := &m.ToolCalls[d.Index]
	if d.ID != "" { call.ID = d.ID }
	if d.Function.Name != "" { call.Function.Name = d.Function.Name }
	call.Function.Arguments += d.Function.Arguments // 参数是一截一截的 JSON 字符串
}
```

agent 用它得到完整回复；观测台代理用它把转发过的流还原成 `choices[0].message` 结构留档。两边共用一份，存档与 agent 实际收到的内容不会各拼各的。

### 2.2 agent：流结束后再检查

`callModel` 按行读，流结束后检查：

```go
if stream.FinishReason != "stop" && stream.FinishReason != "tool_calls" {
	// 没有 finish_reason：流在中途断了，按“截断或未完成”处理
}
```

流中途出现 `error` chunk、JSON 坏掉、读超时或被取消，都在这一层返回错误，和非流式时一样交给 ReAct 循环处理。

`CallKeeping` 只给 `main` 与 `compact` 传 `onDelta`：发 `item/delta`，`main` 的正文同时打印到终端。记忆辅助、评审、归因的输出是给程序读的 JSON，不发增量。

### 2.3 观测台：转发不缓冲，增量不落盘

`relayStream` 每读到一行就写给 agent 并 `Flush`。整段读完再写，agent 就收不到流式了。agent 取消时写入失败，代理停止读上游，记 499。

`run.handle` 把 `item/delta`（包括子 agent 经 `subagent/message` 转上来的）记成 `delta` 事件：留在内存里供正在看的页面实时显示，**不写进** `runs/<id>.jsonl`。重新打开已结束的对话时，页面直接用 response 事件里的完整回答。

### 2.4 页面：增量挂在最后一次请求上

主任务的模型请求一次只有一个在进行，所以 `item/delta` 拼到 `r.calls.at(-1)` 的 `live` 上。请求已经有响应时，晚到的增量直接丢弃；响应到达时清空 `live`，改按完整响应渲染。

## 3. 验证

格式化、编译后用 DeepSeek 实际运行（`-provider deepseek`）：

1. **命令行**：`go run . -provider deepseek -question "用计算工具算 (37*41)+19，再解释怎么算的"`。
   - 第一次回复的工具参数由多个片段拼成，并执行成功；
   - 第二次回复的正文逐段打印；
   - 每次请求都有 `Usage:` 行，说明 `include_usage` 生效。
2. **协议**：`go run . -app-server -provider deepseek -question "…" > out.jsonl`。stdout 里有大量 `item/delta`，把 `content` 片段拼起来与最终回答一致；modelCall 的 completed 带 `first_token_ms`。
3. **观测台**：发起一条带工具调用的对话。
   - 实时事件里的 delta 先于 response 到达，页面逐段显示推理与正文；
   - 运行记录文件里只有 request/response，没有 delta；
   - response 的 body 是拼好的完整响应，`tool_calls` 完整，带 `chunks` 与 `first_token_ms`；
   - 续聊能从存档还原上下文并正确接着回答。
4. **中途停止**：让模型写长文，生成中按停止。代理记 499，本轮结局为 cancelled。

## 4. 当前边界

- 存档是按 chunk 还原的完整响应，不是逐字原文，SSE 注释与心跳行不留。
- 增量不落盘：观测台在生成中途重启，页面看不到已经生成的半段，只能等运行结束的结果。
- 只解析 OpenAI 兼容的 chat/completions 流式格式；其他协议（如 Responses API 的事件类型）需要另写适配。
- 首字延迟以第一段非空正文或推理为准；记在 modelCall 的 completed 与 chat span 的 `agent.response.first_token_ms` 上，`go run . metrics` 还没有汇总它。
