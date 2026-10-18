# 扩展实践：在观测台导入 Codex 与 Claude Code 的对话

先读 [学习笔记](import-notes.md)。对应能力：H24 可观测性（同一套视图看不同 harness）、H18 会话记录与恢复（读懂别家的记录格式）。

## 1. 用法

观测台左侧点“导入对话”，选一个文件：

| 来源 | 文件位置 |
| --- | --- |
| Codex | `~/.codex/sessions/<年>/<月>/<日>/rollout-*.jsonl` |
| Claude Code | `~/.claude/projects/<目录名>/<会话ID>.jsonl` |

导入后它和普通对话一样出现在列表里（标题前有来源标记），对话流、轨迹、迷宫、对比、原始输入输出都能用。导入的对话只读：输入框禁用，没有检查点，不能续聊，也不能归因回放。存档放在当前租户的 `runs` 目录，和观测台自己的对话一样不提交；记录里可能有路径、账号 ID 和代码，导入前自己确认。

## 2. 实现

| 部分 | 代码 | 做法 |
| --- | --- | --- |
| 解析与归一化 | [transcript](../../internal/transcript/transcript.go)、[codex.go](../../internal/transcript/codex.go)、[claude.go](../../internal/transcript/claude.go) | 按第一条记录认格式；输出按时间排列的“一轮开始 / 一次请求 / 一轮结束” |
| 导入接口 | [import.go](../../internal/observer/import.go) | `POST /runs/import`，最大 128 MB；换成 start/continue、request/response、exit 事件，时间用记录里的原始时间戳；start 带 `source` |
| 页面 | [index.html](../../internal/observer/index.html) | 请求带 `append` 时接在上一次请求后面还原完整输入；导入的对话隐藏模型、工作区、权限选择 |

切分请求的规则：

- **Codex**：输出条目（assistant 消息、reasoning、各种工具调用）累积成一次回复，遇到 `token_usage_record` 就结束这次请求；输入条目（developer/user 消息、工具输出）进历史。`UserMessage` 或 `user_message` 事件开新的一轮，`task_complete` / `turn_aborted` 结束一轮；插话直接开新的一轮。
- **Claude Code**：先从最后一条沿父指针（压缩边界用 `logicalParentUuid`）走出主干，再按 message.id 收回同一次回复的所有行和挂在下面的工具结果，按文件顺序处理。工具在回复还在流式输出时就执行完的，结果先暂存，等这次回复的所有行都到了再进历史。`[Request interrupted by user…]` 让这一轮以“已取消”结束；没有请求模型的斜杠命令不算一轮。

存档只存新增消息：

```go
c.New = b.history[from:len(b.history):len(b.history)] // 压缩后 from=0，存完整列表，页面不接前文
```

页面侧先比引用再比内容，工具结果按调用 ID 用一个 Map 对上，长会话才不会在每次请求时把整段历史序列化一遍。

## 3. 验证

都是本机真实文件，导入到常驻 `test` 账号，记录保留。

| 检查 | Codex（Codex Desktop，13 轮） | Claude Code（本项目的一次会话） |
| --- | --- | --- |
| 源文件 | 1.6 MB，342 行 | 77 MB（含大量客户端内部记录） |
| 请求数 | 37 = 用量记录 37 条 | 703 = 不同 message.id 700 个 + 压缩 3 次 |
| 工具调用 | 24 次，全部有结果 | 709 次 = 源文件 tool_use 块 709 个；没有结果的 2 次是导入时还在执行的最后一步 |
| 轮次 | 13（含一次插话“写完了吗”） | 61；`/clear` 这类本地命令不算；1 轮因中断显示“已取消” |
| 还原的请求 | 每次请求末尾都不是 assistant；耗时没有负数 | 同左 |
| 存档 | 0.85 MB | 4.4 MB；若每次请求存完整输入约 250 MB |
| 页面 | 对话流与轨迹正常；第 2 轮开头显示“系统规则 / 工具定义变化”（Codex 追加了 skills 说明） | 703 次请求加载约 3.5 秒 |

另外用一个 Markdown 文件试了导入，返回“认不出这个文件”。

## 4. 当前没有覆盖的范围

- **子 agent**：Claude Code 的子 agent 记录在会话目录的 `subagents/` 下，Codex 的子线程在各自的 rollout 里；只上传一个文件时看不到它们的过程。
- **Codex 的编排工具**：`exec` 一次脚本里可以调用搜索、shell、MCP 等多个工具，记录里另有这些内部调用的界面事件；现在只显示外层的 `exec` 和它的输出。
- **系统提示词与工具定义**：Claude Code 的记录里没有；推理原文两家都看不到。
- **耗时**：按时间戳估算，没有首字延迟；调用链、指标统计、归因回放都依赖本项目自己的 trace 与检查点，导入的对话不参与。
- **重复导入**：同一个文件导入两次会得到两条对话；观测台也还没有删除对话的入口。
