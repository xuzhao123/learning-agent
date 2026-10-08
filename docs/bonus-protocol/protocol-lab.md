# 扩展实践：仿 app-server 的结构化协议（B0）

原理见 [结构化协议笔记](protocol-notes.md)，来由见 [进阶计划 B0](../../plan-advanced.md)。协议层在 [internal/protocol](../../internal/protocol/protocol.go)，agent 一侧在 [main.go](../../main.go) 的 `-app-server`，界面一侧在 [observer/server.go](../../internal/observer/server.go) 与 [index.html](../../internal/observer/index.html) 的 `applyNotify`。

## 1. 改了什么

| 以前的别扭做法 | 现在 |
| --- | --- |
| 观测台和页面用十几个正则解析 agent 的终端文本：`Checkpoint: id=`、`Context: … cap=`、`Memory …`、`Skills:`/`MCP …`、`Browser [..]`、`Subagent start/done/stop`、`  │ call_id`、`Termination:`、`RAG case:`、`Recall complete:` | agent 发结构化通知；页面按方法名处理。这些正则只留在“旧存档兼容层”里，只对 B0 之前的存档生效 |
| 联网审批：agent 打印一行就结束本轮，用户批准后**下一轮**续聊才生效 | `network/requestApproval` 是 agent 发给界面的请求：agent 暂停等待，用户点“允许”后**本轮**接着执行 |
| 续聊上下文经 `-history-stdin` 从 stdin 传入，问题经 `-question` 传入 | 都放进 `turn/start` 的参数 |
| 停止只能发 SIGINT | 第一次停止发 `turn/interrupt` 请求，拿不到确认才退回 SIGINT；第二次仍是 SIGKILL |
| 子 agent 的输出加 `  │ call_id` 前缀转发，页面再拆前缀 | 子 agent 也以 `-app-server` 启动，父进程的 RunChild 就是它的界面：通知包成 `subagent/message` 上报，日志行另发 `subagent/log`，审批请求逐级转发 |

命令行直接运行时不带 `-app-server`，输出和以前完全一样；队列里的任务以协议模式运行，但上面没有界面，通知不再转发，审批如实回答“没人能批准”。

## 2. 消息一览

| 方向 | 方法 | 类型 | 内容 |
| --- | --- | --- | --- |
| 界面 → agent | `turn/start` | 请求 | `question`、`history`（续聊上下文）、`continues`（上一轮的检查点ID） |
| 界面 → agent | `turn/interrupt` | 请求 | 开始第一段停止 |
| agent → 界面 | `turn/started` | 通知 | `turn_id`（检查点ID）、`thread_id`、`continues`、`resume`、`trace_id`、`input_capacity`、模型与 `pid` |
| agent → 界面 | `item/started`、`item/completed` | 通知 | Item：`userMessage`、`modelCall`（用途、序号、用量）、`toolCall`（参数，完成时带 Observation）、`compaction`（前后估算量） |
| agent → 界面 | `turn/completed` | 通知 | 结局、答案或错误首行、模型请求数、用量 |
| agent → 界面 | `memory/event`、`capability/event` | 通知 | 记忆读写、skill 与 MCP 连接：`kind` 与给人看的 `text` |
| agent → 界面 | `browser/navigate` | 通知 | 调用ID与地址，页面据此找画面 |
| agent → 界面 | `subagent/started`、`subagent/completed`、`subagent/log`、`subagent/message` | 通知 | 子任务ID、结局；子 agent 的日志行；子 agent 自己的通知（原样包一层） |
| agent → 界面 | `lab/case`、`lab/phase` | 通知 | Day 3/4 实验的分组与阶段 |
| agent → 界面 | `network/requestApproval` | 请求 | `domain`、`reason`、`call_id`（子 agent 发起时另有 `task_id`）；回复 `{"decision":"allow" \| "deny" \| "unavailable"}` |

## 3. 读代码

### 3.1 一条连接，双向收发

[`protocol.Conn`](../../internal/protocol/protocol.go) 是两端共用的实现：读循环按行解析消息，**带 ID 不带方法名的是响应**，交给等待中的 `Call`；其余（请求和通知）交给 `handle`。

```go
if m.ID != nil && m.Method == "" {
	ch := c.pending[*m.ID] // Call 发出请求时登记的等待者
	delete(c.pending, *m.ID)
	ch <- m
	continue
}
handle(m)
```

agent 进程里有一个全局的 `protocol.Client`；命令行直接运行时它是 nil，`protocol.Notify` 什么都不做，所以各处发事件的代码不用判断模式。

### 3.2 agent 一侧

[main.go](../../main.go) 在 `-app-server` 时：

1. 把协议输出留给原来的 stdout，再把 `os.Stdout` 换成 stderr，之后所有 `fmt.Printf` 都成了给人看的日志；
2. 忽略 SIGPIPE：界面先退出时写协议会失败，进程照常收尾，而不是被信号杀掉；界面断开等同于中断；
3. 没有从命令行拿到问题时，等 `turn/start`；
4. `turn/interrupt` 和第一次 Ctrl+C 走同一个函数，取消运行上下文。

检查点保存启动参数时去掉 `-app-server`：之后在命令行 `-resume`，不该变成协议模式。

### 3.3 审批：暂停、等待、降级

[sandbox.Run](../../internal/sandbox/sandbox.go) 的 `request_network_access`：

```go
waitCtx, cancel := context.WithTimeout(ctx, ApprovalWait) // 4 分钟
err := protocol.Call(waitCtx, "network/requestApproval", …, &answer)
switch {
case ctx.Err() != nil:            // 整轮被中断 → 结果未知
case answer.Decision == "allow":  // AllowDomain(domain)，本轮立即生效
case answer.Decision == "deny":
case 没有界面 / unavailable:      // 命令行、队列：保持原来的 pending + -net-allow 提示
default:                          // 用户暂未答复：pending，下一轮再说
}
```

执行器给这个工具的单次时限是 `ApprovalWait + 30s`，工具会先于执行器到时返回 pending，不会被记成“结果未知”。白名单在运行中会变，代理的检查和追加用同一把锁。

### 3.4 子 agent：父进程就是界面

[RunChild](../../internal/agent/child.go) 用两对 `os.Pipe` 连接子进程的 stdin/stdout，stderr 仍写给人看的日志。子进程的通知包成 `subagent/message`（带调用ID和子任务ID）发给上级；子进程的审批请求由 `relayRequest` 转给上级界面，批准时父进程自己的白名单也加上这个域名，再把回复交回子进程。队列调用 RunChild 时调用ID为空，不转发通知，审批回答 unavailable。

### 3.5 界面一侧

[server.go](../../internal/observer/server.go) 的 `launch` 以 `-app-server` 启动 agent，`turn/start` 带上问题与续聊上下文；`handle` 把通知存成 `notify` 事件、把审批请求存成 `approval` 事件并记下请求ID；`decideNetwork` 记下用户的决定，如果 agent 正在等这个域名，当场回复。检查点ID改从 `turn/started` 读取。

页面的 `applyNotify` 按方法名更新运行状态。子 agent 转发上来的 `subagent/message` 原样递归应用到这个子 agent 的状态上，所以任务ID、浏览器地址、结局的处理和父 agent 完全一样。start/continue 事件带 `protocol: true`；没有这个标记的旧存档，日志行先经过 `legacyLine` 换算成同样的通知。

## 4. 实验记录

均使用真实方舟模型。

### 4.1 管道冒烟

不经过观测台，直接 `(printf '{"jsonrpc":"2.0","id":1,"method":"turn/start",…}'; sleep 40) | ./agent -app-server`：stdout 依次是 turn/start 的响应、`turn/started`、用户消息、两次 `modelCall`、两个并行的 `toolCall` 的开始与完成、`turn/completed`（带答案），日志全部在 stderr。

### 4.2 审批当轮生效

勾选 Bash 沙箱，让模型用 curl 查 PyPI 上 requests 的最新版本：

```text
approval  network/requestApproval  {"domain":"pypi.org", "reason":"需要访问 pypi.org 获取 requests 包的最新版本信息…"}
allow     pypi.org                 ← 用户点“允许”，观测台当场回复 {"decision":"allow"}
Network approved [call_…]: domain=pypi.org
Bash [call_…]: curl -sS https://pypi.org/pypi/requests/json | python3 -c …
Sandbox net: allow host=pypi.org port=443
turn/completed  答案：requests 包在 PyPI 上的最新版本号是 2.34.2
```

以前这需要两轮：第一轮申请、结束；用户批准后续聊，第二轮才能访问。

### 4.3 停止与续聊

派两个子 agent 浏览网页，执行中按停止：停止记为“经协议发送 turn/interrupt”，父 agent 日志出现 `Interrupt: turn/interrupt`，两个子任务 `subagent/completed state=stopped`，一轮结局 `cancelled`。续聊后见 [Day 14 实践](../day-14/day-14-lab.md) 的续接链：新的一轮用新ID，模型用 `task_id` 引用上一轮的两个子任务，二者都从检查点续跑。

### 4.4 队列与页面

- 队列：两个任务的子进程以协议模式运行，都正常完成；没有界面时不转发通知。
- 页面（无头 Chrome 读取页面状态，无 JS 异常）：新运行还原出检查点、输入容量、每个子 agent 的状态、日志、检查点、浏览器地址（2、5、2 个）和停止原因；B0 之前的演练存档经兼容层还原，结果与改动前一致。
- 发现并修复：最初没有把子 agent 的 `browser/navigate` 拆开，子 agent 面板拿不到画面地址；改为递归应用 `subagent/message` 后正常。只传 `task_id` 续跑的子 agent 卡片原来显示一串ID，改为显示 `subagent/started` 带来的任务原文。

## 5. 动手

1. 用 4.1 的管道方式自己当界面，在 agent 执行工具时再发一行 `{"jsonrpc":"2.0","id":2,"method":"turn/interrupt"}`，观察它怎样收尾。
2. 在观测台勾选 Bash 沙箱让模型申请联网，先不点按钮，观察 agent 等待时页面上的提示；再分别试“允许”和“拒绝”。
3. 用队列跑一个需要联网的 Bash 任务，看审批怎样被如实回答为“没有可以当场批准的界面”。
4. 在浏览器开发者工具里看 `/runs/<id>/events` 的事件流，找出 `notify` 与 `approval` 两类事件。

## 6. 已知边界

- **进程绑定连接**：观测台重启后，正在运行的 agent 失去协议连接（会按中断收尾），观测台不能重新接管；这需要 daemon 或事件先落盘再补读，留在进阶计划的 B2/B3。
- **没有 delta**：模型请求仍是非流式的，Item 只有开始和完成两个阶段；流式见 B1。
- **日志仍是文本**：给人看的日志没有结构化，终端输出照旧；结构化日志经 OTel Logs API 导出留在进阶计划 C6。
- **兼容层**：旧存档仍靠正则还原，只在没有 `protocol` 标记的存档上运行。
- **审批只有联网一种**：高风险操作确认（Week 3 D20）可以复用同一个请求通道。
