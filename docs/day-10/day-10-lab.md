# Day 10 项目实践：用队列跑 10 个 agent 任务，保证幂等

原理见 [Day 10 学习笔记](day-10-notes.md)。代码在 [queue.go](../../internal/queue/queue.go)（队列与 worker）和 [child.go](../../internal/agent/child.go)（`agent.RunChild`，队列与子 agent 共用）。

## 1. 实现概览

```sh
go run . queue [-workers 3] [-attempts 3] 任务文件.jsonl [-- 公共agent参数]
```

```text
queue/tasks.jsonl ──load──▶ 去重 ──▶ jobs channel ──▶ worker × 3 ──▶ agent.RunChild(id, question, args)
                                                                      │
                     .data/checkpoints/<id>.json  ◀── 读/写 ── agent 子进程（-task-id id 或 -resume id）
                     .data/checkpoints/<id>.log   ◀── 子进程的全部输出
```

| 设计点 | 本项目的选择 |
| --- | --- |
| 任务格式 | 每行一个 JSON：`id`（幂等键）、`question`、可选 `args`；空行和 `#` 开头的行跳过 |
| 幂等键 | 任务ID就是 Day 9 的检查点ID。不写 `id` 时用 `q-` + 问题与参数的 SHA-256 前 12 位 |
| 状态存储 | 没有单独的队列状态文件：任务状态就是检查点状态（不存在 / running / stopped / done） |
| 并发控制 | 固定数量的 worker（1–8），即同时运行的子进程数上限 |
| 执行方式 | 每个任务一个 agent 子进程：功能开关是包级变量、一个进程只跑一个任务，子进程正好互相隔离 |
| 独占执行 | 子进程运行期间持有检查点的 flock；同一ID的第二个进程直接报错退出 |
| 任务级重试 | 暂时性失败（模型请求失败、进程中途退出、被取消）退避 1s、2s 后用 `-resume` 续跑；确定性失败不重试 |
| 停止 | Ctrl+C 后不再派发；在跑的子进程收到一次 SIGINT，写好检查点再退出；10 秒不退出才强制结束 |
| 输出 | 终端只打印状态变化与汇总；每个任务的完整过程写进自己的 `.log` |

## 2. 读代码

### 2.1 `RunChild`：按检查点决定做什么

```go
cp, err = LoadCheckpoint(id)
switch {
case errors.Is(err, fs.ErrNotExist):
	args = append([]string{"-task-id", id, "-question", question}, args...) // 全新启动
case cp.Question != question:
	return cp, false, llm.Permanent(fmt.Errorf("任务ID %s 已用于另一个问题，不能复用", id))
case cp.Status == "done":
	return cp, true, nil // 已完成：直接返回存档答案，不启动进程
case !Resumable(cp):
	return cp, false, llm.Permanent(…) // 预算耗尽、熔断等：续跑结果也一样
default:
	args = []string{"-resume", id} // 续跑
}
```

这就是笔记第 4.1 节“没见过 / 已完成 / 正在执行”三种情况。“正在执行”由锁判断：`RunChild` 启动前先试一下锁，拿不到就返回 `ErrBusy`；子进程启动后会自己再加锁，所以试锁和启动之间即使有别的进程抢先，也不会两个都跑起来。

### 2.2 子进程的取消

```go
cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} // 终端的 Ctrl+C 只到调度方
cmd.Cancel = func() error { return cmd.Process.Signal(os.Interrupt) }
cmd.WaitDelay = 10 * time.Second
```

`exec.CommandContext` 默认在 ctx 取消时直接 SIGKILL；改成 SIGINT 后，子进程走 Day 8 的优雅停止。子进程放进自己的进程组，终端的 Ctrl+C 就不会直接发给它。这样它只收到调度方转发的那一次信号，不会因为第二次信号被立即结束（agent 在第一次信号后恢复了默认处理）。

### 2.3 worker 与派发

```go
for range *workers {
	wg.Go(func() {
		for i := range jobs {
			results[i] = runTask(ctx, tasks[i], extra, *attempts)
		}
	})
}
```

派发循环同时等 `jobs <- i` 和 `ctx.Done()`，收到 Ctrl+C 后就不再派发。每个 worker 只写自己负责的 `results[i]`，不需要加锁。

### 2.4 任务级重试：`runTask`

```go
case llm.IsPermanent(err) || attempt == attempts:
	result.status, result.detail = "failed", err.Error()
	return result
}
// 暂时性失败（模型请求失败、子进程中途退出）：退避后用 -resume 续跑，不从头再来。
```

Day 8 的错误分类在这里又用了一次：`RunChild` 返回的错误也带着“是否可以再试”的标记。

## 3. 动手

[queue/tasks.jsonl](../../queue/tasks.jsonl) 有 11 行任务：9 个带 `id`，第 11 行和 `t03` 完全相同，第 12 行没有 `id`。

### 3.1 跑 10 个任务

```sh
go run . queue -workers 3 queue/tasks.jsonl -- -reasoning-effort low -max-steps 4
```

真实输出（2026-10-07，节选）：

```text
Queue duplicate: id=t03 line=11 与第4行重复，只入队一次
Queue: tasks=10 workers=3 attempts=3 common_args=["-reasoning-effort" "low" "-max-steps" "4"] …
Queue retry: id=t01 error=任务 t01 未完成（model_error）：未完成：模型请求失败，请检查网络与超时 wait=1s
Queue start: id=t01 attempt=2/3
…
Queue summary: elapsed=1m4.7s
- t01        done        attempts=2 model_calls=3  计算结果为：**2647** …
- t02        done        attempts=1 model_calls=2  今天是2026年10月7日，星期三。
- t03        done        attempts=1 model_calls=2  根据学习笔记内容，循环职责共有以下四项：…
…
- q-3f313f0029e9 done    attempts=1 model_calls=2  在学习笔记文件 `docs/day-01/day-01-notes.md` 中，“ReAct”出现在…
Queue totals: done=10 replayed=0 failed=0 interrupted=0 not_started=0 model_calls=23
```

这次运行碰上了一次真实的模型请求失败。`t01` 的日志：

```text
===== … attempt 1 =====
Model request: 1/4 purpose=main input_est=240
Termination: model_error
===== … attempt 2 =====
Resume: id=t01 completed_rounds=0 model_calls=1/4 pending_calls=0
Model request: 2/4 purpose=main input_est=240
Model request: 3/4 purpose=main input_est=482
Termination: no_tool_calls
```

第二次是续跑：失败的那次请求也计入了预算，从 2/4 开始。

10 个任务、3 个 worker、平均每个任务约 15 秒，用了 65 秒。按笔记第 2.3 节估算约 50 秒，`t08` 用了 35 秒，最后拖了尾。

### 3.2 重跑整个队列：一个都不重复执行

```sh
go run . queue -workers 3 queue/tasks.jsonl -- -reasoning-effort low -max-steps 4
```

```text
Queue skip: id=t02 已完成，直接取存档答案（不重复执行）
…
Queue totals: done=0 replayed=10 failed=0 interrupted=0 not_started=0 model_calls=23
```

`model_calls=23` 是存档里的历史用量，这次一次模型请求都没有发。

### 3.3 中途 Ctrl+C，再跑一次

临时任务文件：两个各跑 20 秒 `slow_job` 的任务（`args: ["-lab-tools"]`，问题里写明“如果结果未知，不要重做，直接说明”），以及两个普通任务。2 个 worker，20 秒时发 SIGINT：

```text
Queue interrupted: id=i02
Queue interrupted: id=i01
- i01        interrupted attempts=1 model_calls=1  队列被中断；重新运行队列会从检查点续跑
- i02        interrupted attempts=1 model_calls=1  …
- i03        not_started attempts=0 model_calls=0  队列被中断前没有派发；重新运行队列即可
- i04        not_started attempts=0 model_calls=0  …
```

`i01.log` 里，子进程是被优雅停止的，不是被杀掉的：

```text
Slow job: interrupted after=13.958s
Observation […]: {"tool":"slow_job","status":"unknown","error":"结果未知：执行中被取消（context canceled），可能已经生效",…}
Termination: cancelled
```

再跑一次同一个队列：`i01`、`i02` 从第 2 轮续跑（`Resume: id=i01 completed_rounds=1 model_calls=1/4`），模型按要求说明作业结果未知、没有重做；`i03`、`i04` 全新执行。4 个都完成。

### 3.4 同一个任务，两个进程同时跑

见 [Day 9 实验 3.4](../day-09/day-09-lab.md)：第二个进程报 `这个任务正在另一个进程中运行`。队列里两个 worker 拿到同一个ID时也是同样的结果，不过 `load` 已经在入队时去重，正常情况下不会发生。

## 4. 已知边界

- 只适用于单机：锁是本机文件锁，检查点在本地目录。多台机器要换成数据库加租约（笔记第 3、7 节）。
- 任务文件本身就是队列，不支持运行中追加任务；没有优先级。
- 没有死信队列：超过 `-attempts` 的任务标记为 failed 后留在原处，下次运行队列时 `Resumable` 为真的仍会再试。
- 并发上限只按任务数算，没有按模型请求速率或 token 速率做全局限流。
- 任务级幂等只保证“同一个ID不被启动两次、完成后不再执行”；任务内部有副作用的工具调用，崩溃恢复时仍可能重复（Day 9 已知边界）。
- 一个 worker 每次只看一个任务的日志文件；终端汇总里的答案截断到 50 字，完整答案在检查点的 `answer` 字段。

