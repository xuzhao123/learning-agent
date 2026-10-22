# Day 39 项目实践：子 agent 的控制面

先读 [学习笔记](day-39-notes.md)。对应能力：H21 子 agent（fork、后台运行、发消息、等待、中断）。

## 改动一览

| 改动 | 代码 | 作用 |
| --- | --- | --- |
| 后台派出 | [subagent.go](../../internal/agent/subagent.go) 的 `spawnAgent`、[subcontrol.go](../../internal/agent/subcontrol.go) 的 `startBackground` | `spawn_agent` 带 `background=true`：在自己的 goroutine 里跑（跟着这一轮的上下文，而不是这次工具调用的），立刻返回 `task_id` |
| 等待 | `agent_wait` | 等指定的或任意一个还没交过结论的后台子 agent，最多 `timeout_seconds`（默认 60，最多 300），到时返回“还在跑” |
| 插话、停止 | `agent_send`、`agent_stop` | 经子进程的协议连接发 `turn/steer`、`turn/interrupt`（`RunChild` 新增 `Started` 回调把连接交出来）；停下的子任务写好检查点，可以用 `task_id` 续跑 |
| 分叉 | `forkHistory`、[child.go](../../internal/agent/child.go) 的 `History`、[context_manager.go](../../internal/agent/context_manager.go) 的 `rebuildContext` | `fork=true`：当前视图（去掉发起这次调用的那条 assistant 消息）经 `turn/start` 交给子进程，系统提示词留空、由子 agent 换成自己的，末尾加一条提醒说清它是被派出的子 agent |
| 生命周期 | [react.go](../../internal/agent/react.go) | 模型想结束而还有后台子 agent 在跑：提醒一次（和 Stop hook 同一个位置）；这一轮结束时 `stopBackground` 停掉还在跑的，每个最多等 30 秒写检查点 |
| 工具声明 | [toolspec.go](../../internal/agent/toolspec.go) | `agent_wait` 可重做、读到子 agent 的私有来源时同样标记（D20）；`agent_send` 串行；`agent_stop` 可重做 |

## 1. 验证

格式化、`go vet`、`go build` 通过。DeepSeek 余额用完之后，改用方舟 `doubao-seed-2.1-pro`，命令行 `-subagents`：

| 场景 | 结果 |
| --- | --- |
| 派两个后台子 agent 各算一个乘法，自己先算 2+2，再取回 | 两个 `spawn_agent` 并行派出、立刻返回；父 agent 自己调计算器；两次 `agent_wait` 分别取回 56088、9468，汇总正确 |
| 先约定“数字写成中文大写”，再 `fork=true` 派一个子 agent 算 12*12 | 第一次：子 agent 读到父对话里“现在用 spawn_agent 派一个子 agent”，以为是对自己说的，先试着派子 agent（它没有这个工具），再自己算；加上“你是被派出的子 agent”的提醒后重跑：子 agent 直接用计算器，答“壹佰肆拾肆”——它看得到父对话里的约定 |
| 后台派一个子 agent 依次算 1*1 到 6*6，派出后立刻 `agent_send`“只算到 3*3” | 子 agent 的日志里有 `Steer:` 这句话；它算完 3*3 就停了，结论只有三项 |
| 后台派出后要求“不要等它，直接回答已经派出” | 模型想结束时收到提醒（`Reminder: background subagents running=1`），随后 `agent_wait` 取回结论再回答 |

这一轮结束时强制停掉后台子 agent 的路径（模型不理提醒）没有专门构造；它和 `agent_stop` 是同一个 `turn/interrupt`。

## 2. 已知边界

- 后台子 agent 不跨轮：父进程一轮一个，这一轮结束就停下；下一轮要用 `task_id` 续跑取回结论。
- 分叉的子 agent 上下文和父 agent 一样大，按它的预算算；没有做“分叉时先压缩”。
- 观测台的子 agent 面板照常显示后台子 agent（按子任务ID），但没有“插话”“停止”按钮——这些是模型用的工具。
