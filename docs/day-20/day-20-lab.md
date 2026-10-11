# Day 20 项目实践：高风险操作确认、外发检查、中断恢复与循环检测

先读 [学习笔记](day-20-notes.md)。

## 改动一览

这次把三个遗留问题（跨工具外发、副作用恢复、循环检测）和 D20 的“高风险操作需人工确认”合在一处：执行器在执行前按程序规则判断风险，命中就经 B0 协议暂停问人。

```text
模型提出一批工具调用
  → 滑动窗口：最近 6 个动作里同一动作第 3 次 → 熔断 repeated_action
  → 每项调用：本轮允许列表（D19）→ 风险规则（D20）
        命中 → tool/requestApproval → 界面上的人：允许 / 拒绝
        没有界面、超时、拒绝 → status=denied，不执行
  → 执行；每项结束立即写结果日志
  → 无进展：整轮结果都见过 → 第 2 轮附提示，第 3 轮熔断 no_progress
  → bash / 记忆 / 子 agent 成功返回 → 标记本任务含私有数据
```

| 改动 | 代码 | 作用 |
| --- | --- | --- |
| 风险规则与确认 | [approval.go](../../internal/agent/approval.go) | `risk` 判断四类风险，`approve` 发 `tool/requestApproval` 并等待；没有界面按 unavailable 处理，结果 `denied` |
| 私有数据标记 | [approval.go](../../internal/agent/approval.go)、[react.go](../../internal/agent/react.go) | bash 成功、记忆召回或 search_memory 成功、子 agent 含私有数据 → 记来源；之后浏览器与 MCP 工具要确认。存进检查点，续跑、续聊延续 |
| 子 agent 继承 | [subagent.go](../../internal/agent/subagent.go)、[main.go](../../main.go) | 父任务含私有数据时，子 agent 带 `-private-input` 启动；子任务结果返回 `private`，父任务据此标记 |
| MCP 注解 | [mcp.go](../../internal/mcp/mcp.go) | server 声明 `destructiveHint=true` 的工具要确认；注解只收紧不放宽。自带的 calculator server 如实声明只读 |
| 结果日志 | [approval.go](../../internal/agent/approval.go) | 每项调用结束写 `.data/checkpoints/<任务ID>.calls/<调用ID哈希>.json`；续跑先查日志，有记录就取回，不重做 |
| 中断重做 | [react.go](../../internal/agent/react.go) | 续跑时不可重复、又无记录的调用先问人；不允许就回填“结果未知”，说明与超时的一致（不重做、不构造标识） |
| 循环检测 | [approval.go](../../internal/agent/approval.go)、[checkpoint.go](../../internal/agent/checkpoint.go) | 相邻计数改为 6 个动作的窗口；新增结果指纹与无进展轮数；状态存检查点 `progress`，分叉回放时重算 |
| 界面 | [server.go](../../internal/observer/server.go)、[index.html](../../internal/observer/index.html)、[child.go](../../internal/agent/child.go) | 观测台接收确认请求，对话里出现确认卡片，`POST /runs/{id}/approval` 当场回复；子 agent 的请求经父进程逐级转发 |

## 1. 风险规则

```go
switch {
case name == "forget_memory":
    return "irreversible", "删除一条长期记忆，删除后无法恢复"
case strings.HasPrefix(name, "mcp_") && mcp.Destructive(name):
    return "destructive", "MCP server 声明这个工具会做破坏性修改"
case outbound(name) && len(privateSources) > 0:
    return "outbound", "本任务已读入私有数据（…），这次调用会把参数发到外部"
}
```

外发类工具是 `web_search`、`open_page` 和全部 `mcp_*`。第四类 `redo` 只在续跑补齐中断批次时出现。

确认放在占用并发槽位之前，等人的时间不算进单次工具时限（`-tool-timeout`）；最长等待沿用联网审批的 4 分钟。一次确认只放行这一项调用，不留到下一轮；联网白名单仍是 Day 12 的另一套机制（用户批准后本对话持续有效）。

为什么 bash 本身不需要确认：它在断网、只可写工作目录的沙箱里，联网要经用户批准的白名单。它是私有数据的**来源**，外发要经过浏览器、MCP 或白名单里的域名。

## 2. 在观测台里操作

```bash
go run . observe
```

勾选 Bash 沙箱和浏览器，让 agent 先用 bash 生成一个文件、再打开带文件内容的网址。bash 成功后，open_page 前会出现“需要你确认 · 可能外发私有数据”卡片，显示工具和完整参数。点“允许”后本轮继续执行；点“拒绝”时模型收到 `denied`。卡片和决定记在对话记录里，页面重开仍能看到；agent 结束时还没答复的，显示为未答复。

命令行没有界面，同样的请求如实拒绝：

```bash
go run . -provider deepseek -bash -browser -question "先用 bash 执行 echo build-7731 > /workspace/tag.txt && cat /workspace/tag.txt，然后用 open_page 打开 https://example.com/?tag= 加上文件内容"
```

终端依次出现 `Private data: source=bash`、`Approval request … risk=outbound`、`Approval decision …: unavailable`，Observation 的 `status` 为 `denied`。

## 3. 中断恢复

```bash
go run . -provider deepseek -lab-tools -tool-timeout 120s -task-id j1 \
  -question "在同一轮里同时调用两次 slow_job：一个 2 秒，一个 60 秒。"
# 两个作业都开始后等 5 秒，在另一个终端 kill -9 这个进程
go run . -resume j1
```

续跑时 2 秒那项来自结果日志（`Replay …: slow_job journal → ok`，结果带 `note` 说明没有重做）；60 秒那项没有记录，先发 `risk=redo` 的确认，命令行下没有界面，回填“结果未知”。

## 4. 验证记录

2026-10-11 使用 DeepSeek（deepseek-flash）。方舟仍返回 HTTP 403，没有模型回答，不计入结果。代码已格式化、`go vet` 与编译通过，观测页面脚本语法检查通过。

| 场景 | 结果 |
| --- | --- |
| 跨工具外发（命令行） | bash 后 open_page 被拒（outbound，unavailable）；模型如实说明需要确认后结束 |
| 跨工具外发（观测台 HTTP 接口） | 确认请求进入对话记录；接口回复 allow 后 open_page 实际执行并返回页面；`decision` 事件已记录 |
| 子 agent 继承 | 父任务 bash 后派出子 agent，子进程启动即标记 `parent`，它的 open_page 被拒；随后父 agent 想自己打开同一网址，同样被拒（标记 `bash、subagent`）。模型没有完全遵守“不要换工具重试”，拦截靠的是程序规则 |
| 删除记忆 | forget_memory 被拒（irreversible）；记忆文件里该条仍在 |
| kill -9 后续跑 | 已完成的 slow_job 从结果日志取回，没有重做；未完成的经 redo 确认（unavailable）后回填结果未知；模型分别如实报告 |
| 无进展（D17 检索消融题） | 4 次运行都自行在第 4–5 轮回答，无进展轮数最多 1，没有误触发；D17 的方舟运行用满 6 次预算，这次没能在方舟上复现 |
| 无进展（换关键词查不存在的主题） | 2 次运行都在第 3 轮附加提示，模型第 4 轮回答（预算 8 次），没有走到熔断 |

没有在真实模型上复现 A/B 交替调用，滑动窗口按代码阅读确认覆盖了连续重复（第 3 次相同动作在窗口内）。

## 5. 当前没有覆盖的范围

- **粗粒度**：标记的是“任务接触过私有数据”，不追踪具体值；含私有数据后，查一个与它无关的公开网页也要确认。
- **白名单内的外发**：bash 仍可向用户批准过的域名发送数据（Day 12 的白名单不区分路径与内容），这里没有额外确认。
- **MCP 注解**：不可信 server 可以不声明 `destructiveHint`，破坏性工具因此不需确认；它只在含私有数据后受外发检查约束。
- **审计**：确认请求与决定记在观测台的对话记录里，还没有独立、不可篡改的审计日志（D21）。
- **多租户**：本项目是单用户本地运行，没有租户身份、按租户的数据隔离和配额；只在笔记中学习概念。
- **结果日志窗口**：工具已生效、日志还没写下时被 kill，仍是结果未知；续跑重新执行可重复工具时不再询问。
- **repeatable 名单**：内置工具是否可重复仍按工具名写死，误标仍会静默重做；结果日志降低了它的触发机会。
