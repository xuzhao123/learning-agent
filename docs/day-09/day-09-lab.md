# Day 9 项目实践：检查点、写前日志与 -resume

原理见 [Day 9 学习笔记](day-09-notes.md)。代码在 [checkpoint.go](../../internal/agent/checkpoint.go)、[react.go](../../internal/agent/react.go) 的 `Run` 与 `replayBatch`，以及 [main.go](../../main.go) 的 `-resume` 分支。

## 1. 实现概览

每次普通运行（`-question`、交互输入、观测台续聊）都会写检查点；Day 3/4 实验不写。

```text
.data/checkpoints/<id>.json   检查点（JSON，可直接打开看）
.data/checkpoints/<id>.lock   运行期间一直持有的 flock
```

| 字段 | 作用 |
| --- | --- |
| `args`、`question` | 首次启动的命令行参数与问题；续跑时原样恢复工具开关、预算、时限 |
| `status`、`reason`、`answer`、`error` | `running` / `stopped` / `done`；停止原因与终端 `Termination` 一致 |
| `step`、`calls` | 已完整结束的轮次；已用的模型请求数（预算跨续跑累计） |
| `messages` | 模型看到的 View。末尾若是带 `tool_calls` 的 assistant 而没有结果，就是一批中断的调用 |
| `summary`、`last_action`、`repeated` | 执行记录与重复动作计数，续跑后熔断照常生效 |

写入时机，对应笔记第 3 节的两个边界：

| 时机 | 写入后的 `messages` 末尾 |
| --- | --- |
| 问题写入后 | user |
| 模型决定调用工具后、执行前（写前日志） | assistant + tool_calls，没有结果 |
| 这批结果全部写回后 | tool 结果 |
| 运行结束（`defer`） | 结局：`done` 带答案，`stopped` 带原因 |

续跑：

```sh
go run . -resume <id>
```

- `status=done`：直接打印存档答案，不请求模型、不执行工具；
- 可续跑（`running`，或因 `cancelled`、`timeout`、`model_error` 停止）：用检查点里的 `args` 重新解析参数，从下一轮接着跑；
- 其他原因（`max_steps`、`repeated_action` 等）：拒绝续跑，并说明原因。

## 2. 读代码

### 2.1 写前日志

```go
calls = reply.ToolCalls
// 写前日志：先把“要执行哪些调用”落盘，再执行。进程若死在执行途中，续跑时知道是哪几项结果未知。
if err := save(); err != nil {
	return stopRun("checkpoint_error", err.Error(), summary)
}
```

`save` 每次把整份状态写到临时文件再 `os.Rename`（`Checkpoint.write`）。写不出检查点时停止运行：一个无法恢复的长任务，不如早点报错。

### 2.2 续跑时补齐中断的那一批

`resumeContext` 把末尾“有调用、没结果”的 assistant 消息单独取出来，作为 `pending`。主循环的第一轮看到 `pending` 就不请求模型，直接交给 `replayBatch`：

```go
if repeatable[call.Function.Name] {
	again, index = append(again, call), append(index, i) // 只读或带幂等键：重新执行
	continue
}
results[i] = llm.Observation{…, Status: "unknown", Error: "上一个进程在执行这项调用时中断，结果未知：可能已经执行过。需要时先核对，再决定是否重做"}
```

### 2.3 预算与配置

```go
// 预算跨续跑累计：否则“崩溃 → 续跑”就能绕过 max_steps 无限循环。
client.Calls = resume.Calls
```

`main.go` 里 `-resume` 必须单独使用，然后用 `flag.CommandLine.Parse(cp.Args)` 重新解析一遍首次启动的参数。这样续跑时不可能“忘了加 -lab-tools”。

### 2.4 一个任务一个执行者

`LockCheckpoint` 用非阻塞的 `flock(LOCK_EX|LOCK_NB)`，持有到进程退出。第二个进程拿不到锁就报 `这个任务正在另一个进程中运行`。进程被 `kill -9` 时内核释放锁，所以“状态是 `running`、锁却空着”就是“上个进程死了”。

## 3. 动手

### 3.1 已完成的任务再续跑：只返回存档

```sh
./bin/learning-agent -reasoning-effort low -question '计算 (1234×5678) 开根号取整，并查询当前日期时间。两项独立，请同轮调用。'
# Checkpoint: id=20261006-190900.418 …
./bin/learning-agent -resume 20261006-190900.418
```

```text
Resume: id=20261006-190900.418 status=done，直接返回存档答案（不重复执行）
1. 计算结果：(1234×5678) 开根号取整后的值为 **2647**。
…
```

### 3.2 Ctrl+C 之后续跑

接 [Day 8 实验 3.2](../day-08/day-08-lab.md)：`d8-sigint` 在第 1 轮被 Ctrl+C 打断，检查点为 `stopped / cancelled`，`step=1`、`calls=1`。

```sh
./bin/learning-agent -resume d8-sigint
```

```text
Resume: restored_view=5 summary=false
Resume: id=d8-sigint completed_rounds=1 model_calls=1/4 pending_calls=0
Limits: max_steps=4 … lab_tools=true …

Round 2
Model request: 2/4 purpose=main …
```

从第 2 轮、第 2 次请求接着跑；没有在命令行写 `-lab-tools`，它来自检查点的 `args`。

### 3.3 kill -9：写前日志派上用场

```sh
./bin/learning-agent -lab-tools -max-steps 5 -reasoning-effort low -task-id d9-kill \
  -question '请同一轮并行调用：slow_job 执行 40 秒的作业，同时用 calculator 算 1234*5678。都完成后汇报。'
# 作业开始后，从另一个终端 kill -9 这个进程（实验中用 timeout -s KILL 15s）
```

进程死后的检查点：`status=running`，`step=0`、`calls=1`，`messages` 末尾是 assistant 的两个调用 `slow_job`、`calculator`，没有结果。

```sh
./bin/learning-agent -resume d9-kill
```

```text
Resume: id=d9-kill completed_rounds=0 model_calls=1/5 pending_calls=2

Round 1 (resume)
Action […flfj…]: slow_job
Action […f5sc…]: calculator
Replay […flfj…]: slow_job not_repeatable → unknown
…
- 第1轮 slow_job({"seconds": "40"})，上一个进程在执行这项调用时中断，结果未知：可能已经执行过。需要时先核对，再决定是否重做
- 第1轮 calculator({"expression": "1234*5678"})，尝试1次，成功
```

`Round 1 (resume)` 没有请求模型：模型的决定已经在检查点里了。`calculator` 是只读的，重新执行；`slow_job` 有副作用，回填结果未知。

之后模型又拿 `check_task_status` 去核对，被熔断拦下。原因与 Day 8 实验 3.1 相同：这个实验里没有真实的核对接口。

### 3.4 同一个任务，两个进程同时跑

```sh
./bin/learning-agent -lab-tools … -task-id busy1 -question '调用 slow_job 执行 5 秒的作业后告诉我结果。' &
./bin/learning-agent -lab-tools … -task-id busy1 -question '调用 slow_job 执行 5 秒的作业后告诉我结果。'
```

```text
Error: 这个任务正在另一个进程中运行：busy1
Checkpoint: id=busy1 …
Termination: no_tool_calls
```

## 4. 已知边界

- 保存的是 View，不是完整 Transcript：被 Day 3 压缩掉的原文续跑后不能再读（与观测台续聊相同）。续跑后的第一次请求按粗估计算上下文用量，之后恢复使用真实 usage。
- 没有 `fsync`：进程崩溃不丢数据，整机断电可能丢最后一次写入。
- 写前日志只有“这一批”的粒度：一批里先完成的调用，续跑时也按“结果未知”处理（只读的会重新执行）。
- 长期记忆：续跑沿用检查点里 system 中的记忆块，不重新召回；`Commit` 在每个进程结束时各执行一次，同内容只刷新不重复。
- MCP server、子进程等外部连接在续跑时按 `args` 重新建立，外部状态可能已经变化。
- 检查点里没有记录程序版本。用改过的代码续跑旧检查点可能出现不一致，学习项目里先接受这个风险。
- 检查点文件留在 `.data/checkpoints/`，没有自动清理。

