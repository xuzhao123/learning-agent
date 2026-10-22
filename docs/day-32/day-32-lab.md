# Day 32 项目实践：工具声明与后台命令

先读 [学习笔记](day-32-notes.md)。对应能力：H09 工具接口设计（只读 / 破坏性标注）、H01 Agent 循环（只读并行、修改串行）、H07 Shell 与进程（后台命令）；相关的有 H10（权限分类）、H16（MCP 标注）。做的是评估里的 O2、O3 与自用任务 T4（见 [架构评估](../architecture-review.md) 第 4 节、[Day 22 实践](../day-22/day-22-lab.md#3-自用任务集)）。

## 改动一览

| 改动 | 代码 | 作用 |
| --- | --- | --- |
| 工具声明 | [toolspec.go](../../internal/agent/toolspec.go) | `Spec`：只读、串行、可重做、私有来源、外发、不可撤回、权限类别、时限；`specs` 每个内置工具一行，`specOf` 取值 |
| MCP 标注 | [mcp.go](../../internal/mcp/mcp.go) 的 `HintsOf` | 记下 server 声明的 `readOnlyHint`、`idempotentHint`，给调度和续跑用 |
| 读声明的地方 | [react.go](../../internal/agent/react.go)、[approval.go](../../internal/agent/approval.go)、[dispatch.go](../../internal/agent/dispatch.go)、[permission.go](../../internal/permission/permission.go) | 原来的 `repeatable`、`sequential` 名单、`outbound()`、`trackPrivate` 里的工具名、`runAttempt` 的时限、权限包里的工具分类，都改成查声明 |
| 允许列表前移 | [react.go](../../internal/agent/react.go) 的 `ExecuteBatch` | 菜单外的工具在 hook、权限、确认之前就被拒绝；续跑补齐中断批次的路径在 `runTool` 里仍查一次 |
| 后台命令 | [jobs.go](../../internal/sandbox/jobs.go)、[sandbox.go](../../internal/sandbox/sandbox.go) | `bash` 加 `background`；新工具 `bash_output`、`bash_kill`；`launch` 抽出建沙箱的部分，前台与后台共用；`Close` 先终止所有后台命令 |

## 1. 一张表

```go
var specs = map[string]Spec{
	"read_file":  {ReadOnly: true, Repeatable: true, Private: "workspace", Permission: "Read"},
	"edit_file":  {Serial: true, Permission: "Edit"},
	"bash":       {Serial: true, Private: "bash", Permission: "Bash"},
	"open_page":  {ReadOnly: true, Repeatable: true, Outbound: true, Permission: "WebFetch"},
	"forget_memory": {Irreversible: true, Permission: "Write"},
	"spawn_agent": {Repeatable: true, Private: "subagent"},
	…
}
```

谁读哪一项：

| 字段 | 读它的地方 | 以前在哪 |
| --- | --- | --- |
| `Serial` | `ExecuteBatch`：同一批里串行的工具按顺序一个接一个执行 | `sequential` 名单（只有 bash、edit_file、write_file） |
| `Repeatable` | 续跑补齐中断批次、结果未知时的停止判断、审计（不可重做的才记）、span 属性 | `repeatable` 名单 |
| `Private` | `trackPrivate`：成功后记私有来源；bash 与 `bash_output` 只在沙箱能读到用户文件时算，spawn_agent 只在子 agent 读过私有数据时算 | `trackPrivate` 里按工具名的 switch |
| `Outbound`、`Irreversible` | `risk`：读过私有数据后的外发、不可撤回的操作要确认 | `outbound()` 名单、写死的 `forget_memory` |
| `Permission` | `permission.Decide`：规则类别（Bash、Edit、Read、WebFetch、WebSearch、Write）与模式默认值 | 权限包里按工具名的 switch，以及写死的 `mcp_`、`remember_memory` |
| `Timeout` | `runAttempt` 的单次时限（spawn_agent、request_network_access 跟着命令行参数，取的时候填） | `runAttempt` 里的 switch |

没写的工具取零值：可以并行、不能重做、不算私有、不算外发、权限按工具名。实验工具（`slow_job`、`always_fail`）就是这样。

## 2. MCP 工具：效率参考标注，安全只收紧

```go
if strings.HasPrefix(name, "mcp_") {
	hints := mcp.HintsOf(name)
	return Spec{ReadOnly: hints.ReadOnly, Serial: !hints.ReadOnly, Repeatable: hints.ReadOnly || hints.Idempotent, Outbound: true, Permission: "Write"}
}
```

- 声明了只读的并行、可以重做；声明了幂等的可以重做；没有标注的按规范默认值处理：串行、不能重做。
- 外发和权限不看标注：一律算外发（读过私有数据后要确认），一律按“会写”的类别要权限。声明了 `destructiveHint` 的另外按破坏性确认（D20 原有的规则）。这是 D20 的原则：标注只收紧，不放宽。

## 3. 后台命令

```text
bash(command, background=true) ──▶ {job_id: "job-1", status: running}
bash_output(job_id, wait=5)     ──▶ 上次之后的新输出 · running / exited + exit_code
bash_kill(job_id)               ──▶ 按 cgroup 整体结束，交回剩下的输出
```

- 和前台命令走同一个 `launch`：同样的 bwrap、seccomp、cgroup 限制、断网与代理。启动在 bash 的锁里做，启动完就放锁，不占着它等命令结束。
- 每个后台命令留最多 1MB 输出，超出时丢掉最早的，读的时候说明丢了多少。`wait` 最多等 20 秒、不超过单条命令时限的一半，命令结束就提前返回。
- 最多同时 4 个。后台命令只活到这一轮结束：`main` 退出前调用 `sandbox.Close`，它先把还在跑的后台命令按 cgroup 终止，再关代理。

## 4. 验证

格式化、`go vet`、`go build` 通过。DeepSeek 官方 `deepseek-flash` 真实运行：

| 场景 | 结果 |
| --- | --- |
| 同一次回复里三次调用自己的 MCP 计算器（声明了只读） | 三个 `execute_tool` span 都从 0 ms 开始，并行 |
| 同一次回复里三次调用 everything 示例 server 的 `echo`（没有声明只读） | 依次从 0、11、17 ms 开始，串行 |
| 后台跑 8 秒的循环，`bash_output` 带 `wait=5` 读两次 | 第一次读到 7 行、running，第二次读到第 8 行、exited、退出码 0，不重复不遗漏 |
| 后台 `sleep 300` 后 `bash_kill` | `exited`，`stopped` 写明“被 bash_kill 停掉” |
| 后台 `sleep 200` 后直接结束回答 | 这一轮结束时被终止；之后没有残留的 bwrap 进程和 `la-sandbox-*` scope |
| 重构后的回归：计算器；临时 clone 里写文件（带 hooks）、bash 编译、bash 改文件；`-deny 'Edit(p.txt)'` 下写 p.txt | 计算器正常；三步都成功，bash 的改动被前后比对检测到；写 p.txt 被规则 `deny: Edit(p.txt)` 拒绝，文件没有建出来（权限类别从声明里取） |

允许列表前移没能用真实模型触发：模型服务不让调用请求里没声明的工具，提示它硬调也只是在回答里说明。检查本身只是挪了位置，续跑路径上原来的检查保留。

## 5. 已知边界

- 后台命令不跨轮：下一轮是新进程，读不到上一轮的 job，起的服务也随这一轮结束而停止。要跨轮得把状态写进检查点，并处理“上个进程死了、命令还在不在”。
- 后台命令改了工作区不会被 O15 的前后比对抓到（比对只包住一次 bash 调用的启动）。
- 后台命令被代理拒绝的连接，会混进下一条前台命令的 `network_denied`（拒绝记录按进程一份）。
- 没有 PTY：不能给后台命令写输入，交互式程序用不了。
- 权限规则仍按工具名或类别写；声明里的类别是程序写死的，用户不能给某个 MCP 工具改类别。
