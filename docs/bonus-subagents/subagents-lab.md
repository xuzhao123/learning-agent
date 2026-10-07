# 子 agent 项目实践：spawn_agent 建在 Day 8–10 的运行时之上

原理见 [子 agent 学习笔记](subagents-notes.md)。代码在 [subagent.go](../../internal/agent/subagent.go)（`spawn_agent`）和 [child.go](../../internal/agent/child.go)（`agent.RunChild`，与 Day 10 队列共用）。

## 1. 实现概览

```sh
go run . -subagents -question '……'
```

`-subagents` 给模型增加一个工具 `spawn_agent(task)`，并在 system prompt 里说明什么时候该用它。

```text
父 agent（检查点 P）
  Round 1：spawn_agent(task1)、spawn_agent(task2) ── ExecuteBatch 并行（受 -parallel 限制）
             │                            │
             ▼                            ▼
       RunChild(P-sub-<hash1>)      RunChild(P-sub-<hash2>)        ← Day 10 的执行器
       子进程：-task-id … -question task1 [继承的参数] -max-steps 6
       输出逐行加前缀转到父终端：  │ call_xxx Round 1 …
             │                            │
             └──── {answer, model_calls, replayed, task_id} 作为 tool 结果回到父 agent
  Round 2：模型汇总
```

| 设计点 | 本项目的选择 |
| --- | --- |
| 执行方式 | 每个子 agent 一个子进程，复用 `RunChild`：已完成直接取存档、可续跑就续跑、否则全新启动 |
| 子任务ID | 父检查点ID + `-sub-` + task 文本 SHA-256 的前 12 位。父任务重放这次调用，或续跑后模型用同样的 task 再派一次，都会落到同一个子任务 |
| 继承的参数 | 白名单：`-parallel -retries -lab-tools -reasoning-effort` 与上下文参数、`-rag -embedding -min-score -skills -skill -mcp-server -tool-timeout` |
| 不继承 | `-memory`（子 agent 的“用户”是父 agent）、`-subagents`（只允许一层）、`-timeout` 与续聊（由父进程的 ctx 管） |
| 预算与数量 | 每个子 agent `-subagent-steps`（默认6）次模型请求；每次运行最多 4 个不同的子任务 |
| 时限 | 单次尝试 10 分钟（`SubagentTimeout`），父任务的 Ctrl+C 和 `-timeout` 照常传下去 |
| 可重复 | `spawn_agent` 在 `repeatable` 表里：结果未知时可以重试，因为按ID续跑不会重复执行已完成的部分 |
| 失败 | 子任务确定性失败（预算耗尽、熔断）返回不可重试的错误；暂时性失败（模型请求失败、进程中途退出）由 `runWithRetry` 重试，重试时续跑 |

## 2. 读代码

### 2.1 父 agent 这一侧：只是一个工具

[dispatch.go](../../internal/agent/dispatch.go) 里 `spawn_agent` 和其他工具一样按名字分发。父循环不知道它是 agent：并行、重试、超时、检查点都沿用 Day 8–9 的通用逻辑。

### 2.2 子任务ID

```go
task := strings.TrimSpace(args.Task)
sum := sha256.Sum256([]byte(task))
id := CheckpointID + "-sub-" + hex.EncodeToString(sum[:6])
```

最初用的是模型给的调用ID（`call_xxx`）。kill -9 后的重放没有问题，因为写前日志里保存了原来的调用ID。但 Ctrl+C 之后，这批结果已经回填为“结果未知”，续跑时模型会发起一次**新的**调用，调用ID变了，子任务也就从头跑。改成按 task 内容哈希后，只要模型给出同样的 task，就能续上原来的子任务。代价是同一次运行里，同样的 task 只会执行一次。

实测发现模型续跑时常常改写 task（[Day 14 实践](../day-14/day-14-lab.md#41-观测台续聊后子任务没有续跑而是重跑)），所以又加了显式引用：失败和“结果未知”里写明 `task_id`，`spawn_agent` 可以只传 `task_id`，从检查点取原任务续跑。程序只做精确匹配，并且只接受本任务派出的子任务。观测台在停止后续聊时沿用上一轮的检查点 ID，子任务 ID 的前缀才不会变。

### 2.3 子 agent 的输出

```go
out := &prefixWriter{prefix: "  │ " + call.ID + " "}
```

子进程的 stdout/stderr 逐行加上调用ID前缀再写到父终端，几个子 agent 并行时也能分清是谁在输出。完整过程同时保存在子任务自己的检查点里。

## 3. 动手

### 3.1 三个子 agent 并行

```sh
./bin/learning-agent -subagents -reasoning-effort low -max-steps 5 -task-id sub-demo \
  -question '我要准备一份学习简报，包含三部分，彼此独立，请分给子 agent 并行完成：(1) 在学习笔记里查“循环职责”和“并行”两个主题，各用一两句话概括；(2) 计算 floor(sqrt(1234*5678)) 和 365*24*60；(3) 查当前日期与星期。最后汇总成一张表。'
```

真实输出（2026-10-07，节选）：

```text
Round 1
Action [call_wxp7…]: spawn_agent
Action Input: {"task": "请在学习笔记中分别检索“循环职责”和“并行”两个主题的相关内容，每个主题用1-2句话做简洁概括，返回结果时请明确标注每个主题对应的概括内容。"}
Action [call_c1b0…]: spawn_agent
Action Input: {"task": "请计算两个数学表达式的值：1. floor(sqrt(1234*5678))；2. 365*24*60。…"}
Action [call_a84o…]: spawn_agent
Action Input: {"task": "请查询Asia/Shanghai时区的当前日期（格式为YYYY年MM月DD日）和对应的中文星期，直接返回这两个结果即可。"}
Subagent start [call_a84o…]: task_id=… max_steps=6
…
  │ call_wxp7… Action […]: search_notes
  │ call_wxp7… Action Input: {"query": "循环职责"}
  │ call_a84o… Action […]: get_current_datetime
  │ call_wxp7… Action […]: search_notes
  │ call_wxp7… Action Input: {"query": "并行"}
  │ call_a84o… 2026年10月07日，星期三
Subagent done [call_a84o…]: model_calls=2 replayed=false
Subagent done [call_wxp7…]: model_calls=3 replayed=false
Subagent done [call_c1b0…]: model_calls=2 replayed=false

Round 2
Model request: 2/5 purpose=main input_est=1384
以下是为你汇总好的学习简报内容：…
```

几点观察：

- 父 agent 只用了 2 次请求，三个子 agent 共 7 次，整次运行一共 9 次请求。不拆分时一个 agent 大约 3–4 次请求就能做完：这类简单任务用子 agent 并不划算，这里只是为了演示机制。
- 三个 task 都是父模型改写过的完整说明（时区、格式、标注要求），没有照抄用户的原话。子 agent 看不到用户的对话，这正是笔记第 5.1 节说的任务说明。
- 子 agent 输出交错出现，说明它们确实在并行运行。

### 3.2 kill -9 父进程，再续跑

```sh
./bin/learning-agent -subagents -lab-tools -reasoning-effort low -max-steps 4 -task-id subkill \
  -question '请派两个子 agent 并行完成：(1) 调用 slow_job 执行 30 秒的作业，报告开始和结束时间；(2) 用计算器算 1234*5678。然后汇总。'
# 25 秒时 kill -9 父进程
```

父进程死后各检查点的状态：

```text
subkill.json                  running   calls 1   ← 末尾是两个 spawn_agent 调用，没有结果（写前日志）
subkill-sub-9c20d81b98cc.json done      calls 2   ← 计算子任务已经完成
subkill-sub-3fc5c4cf794b.json running   calls 1   ← slow_job 子任务也死了
```

slow_job 子 agent 本身没有被 kill，它死于**输出管道断开**：它的 stdout 接在父进程上，父进程死后，它下一次打印时收到 SIGPIPE 退出。效果上等于“父死子亡”，状态停在 `running`，可以续跑。

```sh
./bin/learning-agent -resume subkill
```

```text
Resume: id=subkill completed_rounds=0 model_calls=1/4 pending_calls=2
Round 1 (resume)
Subagent done [call_xzp4…]: model_calls=2 replayed=true                      ← 计算子任务：直接取存档，不再请求模型
  │ call_op02… Resume: id=subkill-sub-3fc5c4cf794b completed_rounds=0 model_calls=1/6 pending_calls=1
  │ call_op02… Replay […]: slow_job not_repeatable → unknown                  ← 子任务里的 slow_job：结果未知
  │ call_op02… Round 2
  │ call_op02… Action […]: slow_job                                           ← 子 agent 的模型决定重做
Subagent done [call_op02…]: model_calls=3 replayed=false
Round 2
以下是两个子任务的汇总结果：…
```

三层机制在这里同时起作用：父任务的写前日志找回了两个 `spawn_agent` 调用；子任务ID相同，一个直接取存档，一个从检查点续跑；子任务内部的写前日志又找回了中断的 `slow_job`。

但要注意最后一步：子 agent 的模型看到“结果未知”后，选择了**重做** `slow_job`。如果第一次作业其实已经完成，它就执行了两次。框架已经如实报告了“结果未知”，最终是否重做由模型决定。要真正避免重复，作业本身要支持幂等键或状态查询（Day 10 笔记第 4.5 节）。

### 3.3 Ctrl+C 传到子 agent

```sh
./bin/learning-agent -subagents -lab-tools -reasoning-effort low -max-steps 4 -task-id subint \
  -question '请派一个子 agent：调用 slow_job 执行 40 秒的作业并报告时间。'
# 25 秒时按 Ctrl+C（实验中用 timeout -s INT 25s）
```

```text
Observation [call_txos…]: {"tool":"spawn_agent","status":"unknown","error":"结果未知：执行中被取消（context canceled），可能已经生效",…}
  │ call_txos… Observation […]: {"tool":"slow_job","status":"unknown",…}
  │ call_txos… Slow job: interrupted after=11.783s
  │ call_txos… Termination: cancelled
Termination: cancelled
```

两个检查点都是 `stopped / cancelled`。子进程在自己的进程组里，终端的 Ctrl+C 只到父进程；父进程取消 ctx，`cmd.Cancel` 给子进程发一次 SIGINT，子进程优雅停止。

这次运行里，父进程的 `Observation` 打印在子进程最后几行之前：当时父的 `runAttempt` 在取消时立即返回，没有等子进程。之后在观测台测试时，真的出现了父进程先退出、子进程输出和检查点丢失的情况。所以 `runAttempt` 改成：`spawn_agent` 被取消时，继续等子进程收尾（最多 `WaitDelay` 10 秒）。重测的顺序变为：两个子 agent 先打印 `Termination: cancelled`，再是 `Subagent stop`，最后父 agent 回填结果；三个检查点都是 `stopped / cancelled`。

### 3.4 在观测台里看子 agent

```sh
go run . observe   # 新对话勾选“子 agent”
```

对话流里每个 `spawn_agent` 是一张卡片，点开后右侧侧栏显示该子 agent 的任务原文和完整过程；运行中按 ■ 停止，父子 agent 都按上面的顺序优雅停止。实现见 [观测台笔记](../observer/observer-notes.md) 的“Day 8 停止按钮与子 agent 侧栏”。实测（无头浏览器，1440 与 390 宽度、浅色与深色）：三个子 agent 的卡片与侧栏正常显示，父对话流只有父 agent 的 2 次请求，子 agent 的 6 次请求都在侧栏；续聊时新的父进程请求仍归入主对话。

## 4. 已知边界

- 父进程被 kill -9 后，子进程靠 SIGPIPE 间接退出，而不是被主动清理；在不打印的阶段（如等待模型响应），子进程会一直运行到下一次输出。Linux 可用 `Pdeathsig` 让内核在父进程死亡时发信号，macOS 没有对应机制，本项目没有使用。
- 子 agent 数量上限按进程内计数，续跑后重新计数。
- 子 agent 的请求不经过父 agent 的预算；整次运行的最大请求数约为 父 `-max-steps` + 4 × `-subagent-steps`。
- 观测台按任务ID区分父子请求；同一次对话里父 agent 的 Turn/Step 编号不含子 agent 的请求，但全局请求编号 #N 是共用的，所以父对话的编号会跳号。
- 同一次运行里，同样的 task 文本只执行一次（第二次直接取第一次的结论）。

