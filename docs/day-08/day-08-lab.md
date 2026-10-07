# Day 8 项目实践：可取消的执行上下文、分层超时与按错误类型重试

原理见 [Day 8 学习笔记](day-08-notes.md)。代码在 [react.go](../../internal/agent/react.go)（`runWithRetry`、`runAttempt`、`ExecuteBatch`）和 [main.go](../../main.go)（信号与整次时限）。

## 1. 实现概览

| 改动 | 位置 | 说明 |
| --- | --- | --- |
| 整次运行时限 | `main.go` `-timeout` | `context.WithTimeout` 包在信号上下文外面；到时停止原因记为 `timeout` |
| 单次工具时限 | `-tool-timeout`（默认30s）；`agent.ToolTimeout` | 每次尝试一个子上下文；`spawn_agent` 单独用 10 分钟 |
| 不等不配合的工具 | `runAttempt` | 工具在自己的 goroutine 里跑，执行器同时等结果和时限；到时直接返回，记为结果未知 |
| 四种结局 | `llm.Observation.Status` | `ok`、`error`、`not_run`、`unknown`；tool 消息和 summary 都写出来 |
| 错误分类 | `llm.Permanent` / `llm.IsPermanent` | 参数错误、未知工具、计算错误、MCP `isError` 打上“不可重试”；没有标记的按暂时故障重试 |
| 结果未知能否重做 | `repeatable` 表 | 只读或带幂等键的工具才在超时后重试；`slow_job` 和全部 MCP 工具不在表里 |
| 优雅停止 | `main.go` `context.AfterFunc(ctx, stop)` | 第一次 Ctrl+C 取消上下文，回填结果、写检查点再退出；之后恢复默认处理，第二次 Ctrl+C 立即结束 |
| 实验工具 | `slow_job`（`-lab-tools`） | 真的等待指定秒数，等待期间响应取消；描述里写明“有副作用” |

这一天同时修掉了 Week 1 记下的两个问题：被取消的调用不再从 summary 里消失（[深度问题 Q3、Q4](../deep-questions.md)），MCP 工具的执行错误不再被重试。

## 2. 读代码

### 2.1 单次尝试：`runAttempt`

```go
attemptCtx, cancel := context.WithTimeout(ctx, timeout)
defer cancel()
done := make(chan outcome, 1) // 带缓冲：执行器先走了，工具 goroutine 之后也能写完退出
go func() {
	value, err := runTool(attemptCtx, call)
	done <- outcome{value, err}
}()
select {
case o := <-done:
	if o.err == nil || attemptCtx.Err() == nil {
		return o.value, o.err
	}
case <-attemptCtx.Done():
}
// 已经开始执行，却在完成前被打断：无法确定副作用有没有发生。
```

三个细节：

- `attemptCtx` 从 `ctx` 派生，整次运行的时限和 Ctrl+C 都会传到工具里；
- 通道带一个缓冲：执行器放弃等待后，工具 goroutine 之后写结果时不会永远阻塞（否则就是 goroutine 泄漏）；
- 工具返回了错误、而此时 `attemptCtx` 已经结束，这个错误多半就是“被取消”，同样记为结果未知。

### 2.2 重试决策：`runWithRetry`

```go
switch {
case ctx.Err() != nil:
	return result // 整次运行被取消或到时，不再重试
case llm.IsPermanent(err):
	reason = "permanent"
case result.Status == "unknown" && !repeatable[call.Function.Name]:
	reason = "unknown_not_repeatable"
}
```

顺序就是笔记第 5 节的决策树。退避等待用 `select` 同时等计时器和 `ctx.Done()`，取消能打断等待。

### 2.3 排队时被取消：`ExecuteBatch`

```go
case <-ctx.Done():
	// 还在排队就被取消：一次都没开始，可以放心重做。
	result.Status, result.Error = "not_run", ctx.Err().Error()
```

拿到并发槽之后、第一次尝试之前被取消，`runWithRetry` 也返回 `not_run`。summary 不再过滤 `Attempts == 0` 的调用，每项都按 `describe` 写成“成功 / 失败 / 未执行（已取消） / 结果未知”。

### 2.4 打标记的位置

`llm.Permanent(err)` 只包一层，不改错误文本：

- [dispatch.go](../../internal/agent/dispatch.go)：参数不是 JSON 对象、字段类型不对、功能未开启；
- [tools.go](../../internal/tools/tools.go)：未知工具、参数错误、`Calculate` 返回的错误（除以 0、不支持的语法）；
- [mcp.go](../../internal/mcp/mcp.go)：server 返回 `isError: true`。

`always_fail` 的 panic 没有标记，仍按暂时故障重试 3 次，Day 2 的故障实验行为不变。

## 3. 动手

```sh
go build -o bin/learning-agent .
```

### 3.1 单次超时：结果未知，不自动重做

```sh
./bin/learning-agent -lab-tools -tool-timeout 3s -max-steps 4 -reasoning-effort low \
  -question '请调用 slow_job 执行一个 10 秒的作业，然后告诉我结果。'
```

真实输出（2026-10-07）：

```text
Action [call_g9n5…]: slow_job
Action Input: {"seconds": "10"}
Slow job: start seconds=10
No retry [call_g9n5…]: reason=unknown_not_repeatable attempts=1 error=结果未知：单次执行超过 3s
Slow job: interrupted after=3.001s
Observation [call_g9n5…]: {"tool":"slow_job","status":"unknown","error":"结果未知：单次执行超过 3s","attempts":1}
```

`No retry` 在 `Slow job: interrupted` 之前打印：执行器到时就返回了，工具 goroutine 稍后才收到取消退出。

接下来模型用 `check_task_status` 去“核对”作业状态。这是 Day 2 的陷阱工具，永远返回 pending，连续两次后被熔断拦下（`repeated_action`）。这个行为本身说明一件事：要核对“结果未知”，必须有一个**真实的**查询接口；本实验的 `slow_job` 没有。

### 3.2 Ctrl+C：在途调用记为未知，已完成的保留

```sh
./bin/learning-agent -lab-tools -max-steps 4 -reasoning-effort low -task-id d8-sigint \
  -question '请同一轮并行调用：slow_job 执行 30 秒的作业，同时用 calculator 算 2*21。'
# 作业开始后按一次 Ctrl+C（实验中用 timeout -s INT 12s 发送）
```

```text
Slow job: interrupted after=6.279s
Observation […]: {"tool":"slow_job","status":"unknown","error":"结果未知：执行中被取消（context canceled）","attempts":1}
Observation […]: {"tool":"calculator","status":"ok","result":{"result":42},"attempts":1}
Termination: cancelled
已执行步骤摘要：
- 第1轮 slow_job({"seconds": "30"})，尝试1次，结果未知：执行中被取消（context canceled）
- 第1轮 calculator({"expression": "2*21"})，尝试1次，成功
```

对比 Week 1：这两项原来会在 summary 里写成“失败：context canceled”，或者整项消失。检查点里的状态是 `stopped / cancelled`，可以用 `-resume d8-sigint` 续跑（Day 9）。

续跑后，模型把未知的作业说成“作业未成功完成”。这比工具返回的信息说得更肯定，所以之后把错误文本改成了“执行中被取消（…），可能已经生效”。

### 3.3 整次运行时限

```sh
./bin/learning-agent -lab-tools -timeout 15s -max-steps 3 -reasoning-effort low -task-id d8-deadline \
  -question '调用 slow_job 执行 60 秒的作业。'
```

```text
Slow job: interrupted after=8.618s
Observation […]: {"tool":"slow_job","status":"unknown","error":"结果未知：执行中被取消（context deadline exceeded），可能已经生效","attempts":1}
Termination: timeout
```

单次时限是 30 秒，但整次运行只剩约 8.6 秒，工具实际拿到的就是这 8.6 秒：子上下文的截止时间不会晚于父上下文。

### 3.4 确定性错误不重试，暂时故障照常重试

```sh
./bin/learning-agent -max-steps 3 -reasoning-effort low -question '请直接用 calculator 计算表达式 1/0，不要改写表达式，把工具返回原样告诉我。'
./bin/learning-agent -lab-tools -max-steps 3 -reasoning-effort low -question '调用一次 always_fail，然后告诉我结果。'
```

```text
No retry [call_u71x…]: reason=permanent attempts=1 error=除数不能为0

Retry [call_rjvg…]: attempt 1/3 failed: 工具 panic：Day 2 故障注入：工具始终失败; wait 200ms
Retry [call_rjvg…]: attempt 2/3 failed: 工具 panic：Day 2 故障注入：工具始终失败; wait 400ms
Retry exhausted [call_rjvg…]: attempts=3 error=工具 panic：Day 2 故障注入：工具始终失败
```

除以 0 原来会白白重试两次、多等 600ms；现在一次就把错误交给模型。

## 4. 已知边界

- 执行器放弃等待后，不配合取消的工具仍在后台运行，直到它自己返回。本项目的内置工具都很快或会响应取消；真实系统应把这类工具放进独立进程，超时后可以整体杀掉（Day 12 沙箱）。
- `repeatable` 是手写的表。MCP 的工具定义里有 `readOnlyHint`、`idempotentHint` 注解，可以据此自动判断；本项目没有读取，MCP 工具超时后一律不自动重做。注解来自 server，只能作为提示，不能当作安全保证。
- 模型请求失败没有在 agent 内部重试（仍是 Week 1 的做法：直接以 `model_error` 停止），交给 Day 10 的队列在任务层续跑。失败的请求也计入 `-max-steps`。
- 重试预算是每个调用各自的 1+k 次，没有全局的重试比例限制。

