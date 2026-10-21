# Day 21 项目实践：审计日志

先读 [学习笔记](day-21-notes.md)。

## 改动一览

agent 和观测台都写同一份只追加、带哈希链的审计日志 `.data/audit/audit.jsonl`；观测台的“审计日志”页和 `go run . audit` 只读并校验。

```text
agent 进程（执行方）  ──写──┐
  任务开始/结束、确认请求与程序决定、     │
  有副作用的执行、续跑取回与回填、         ├──▶ .data/audit/audit.jsonl
  联网请求与决定、记忆变更                 │     文件锁 · 哈希链 · 写后 fsync
观测台               ──写──┘
  人点了允许/拒绝（工具确认、联网审批）
                                  ▼
            观测台审计页 · go run . audit [-task ID] [-verify]
```

| 改动 | 代码 | 作用 |
| --- | --- | --- |
| 审计包 | [audit.go](../../internal/audit/audit.go) | 记录结构、加锁追加、哈希链、写后落盘；`Read` 从头校验；`Run` 是命令行入口 |
| 确认流程 | [approval.go](../../internal/agent/approval.go) | `approval_request` 写不进审计就不发出、不执行；答复写 `approval_decision`，人答的记为 user，没界面或超时记为 program |
| 执行与续跑 | [react.go](../../internal/agent/react.go) | 任务开始/结束；不可重复或经过确认的调用结束时写 `tool_call`；续跑写 `replay_journal`、`replay_unknown` |
| 联网审批 | [sandbox.go](../../internal/sandbox/sandbox.go)、[main.go](../../main.go) | `network_request` / `network_decision`；启动参数 `-net-allow` 每次运行记 `network_allow_flag` |
| 记忆变更 | [memory.go](../../internal/memory/memory.go)、[main.go](../../main.go) | 写入、刷新、删除、过期、淘汰各记一条，只记编号；`-memory-forget`（含观测台记忆页的删除按钮）先记 `memory_forget_command` |
| 观测台 | [server.go](../../internal/observer/server.go)、[sandbox.js](../../internal/observer/web/sandbox.js)、[pages.js](../../internal/observer/web/pages.js) | 人的点击写 `user_decision` / `user_network_decision`，先写审计再回复 agent；`GET /audit` 与审计页 |

## 1. 一条记录

```json
{"seq":17,"time":"…","actor":"user","event":"user_decision","task":"79405ca4-…","conversation":"…",
 "call_id":"call_00_…","tool":"open_page","risk":"outbound","decision":"allow","detail":"观测台页面",
 "args_sha256":"e63a7e0a66f0…","args_bytes":46,"prev":"…","hash":"…"}
```

- `actor`：`user` 人、`model` 模型提议（工具调用、子任务的启动）、`program` 程序规则或模块自己执行（确认请求、自动拒绝、记忆过期）。
- 参数只存 SHA-256 与字节数。原文在检查点（`messages` 里的 tool_calls）和观测台存档里，核对时重新计算哈希。
- `task` 用于从审计跳回检查点；`conversation` 是观测台对话 ID；`trace` 来自当前 span，能对回调用链。

哈希覆盖除 `hash` 以外的全部字段，包括 `seq` 与 `prev`。

## 2. 写入的规则

- **加锁追加**：观测台和多个 agent 进程会同时写。`Write` 对文件加排他锁，读出最后一条的 `seq` 与 `hash`，接上链写入，`fsync` 后解锁。检查点不 fsync（丢了可以重跑），审计要 fsync（已承认的决定不能丢）。
- **最后一条损坏就停**：不在坏记录后面继续接链，交给人检查。
- **写不进就不执行**：D20 的确认请求、联网审批请求、观测台把人的答复转给 agent 之前，都必须先写成功；否则操作不执行（agent 侧结果为 `denied`，原因 `audit_error`），观测台返回 500，不回复 agent。
- **其余记录**（执行结局、记忆变更、任务起止）写失败只打印 `Audit error`，不阻断任务。
- **范围**：只读工具（在可重复名单里的）不记；审计不是第二份调用链。

## 3. 查看与校验

```bash
go run . audit                 # 列出全部记录并校验
go run . audit -task <任务ID>   # 只列这个任务与它的子任务（校验仍覆盖整条链）
go run . audit -verify         # 只校验
```

观测台左侧“审计日志”：最新在上，显示链是否完整；点任务 ID 过滤，点“对话”打开对应的对话。

## 4. 验证记录

2026-10-11 使用 DeepSeek（方舟仍 HTTP 403）。代码已格式化、`go vet` 与编译通过，页面脚本语法检查通过；观测台审计页用无头浏览器截图确认表格与校验状态正常显示。

| 场景 | 审计里看到的 |
| --- | --- |
| 三个 agent 同时运行（外发、联网申请、slow_job） | 13 条交错写入，链完整；外发任务依次是 `task_start → tool_call bash → approval_request → approval_decision unavailable → tool_call open_page denied → task_end`（结束时带“私有数据来源：bash”）；联网申请 `network_request → network_decision unavailable` |
| 观测台批准外发 | `approval_request`（程序）→ `user_decision allow`（观测台记的人）→ `approval_decision allow`（agent 收到）→ `tool_call open_page ok`；四条的参数哈希相同 |
| 记忆写入与人工删除 | `tool_call remember_memory ok` → `memory_write M12`；`memory_forget_command M12`（人）→ `memory_forget M12`（程序） |
| 子 agent | 子任务 `task_start` 的 actor 为 model、带“私有数据来源：parent”；它的确认与拒绝记在子任务 ID 下，`-task` 用父任务 ID 能一起列出 |
| 篡改（在备份上操作后还原） | 改一条内容 → 该条 hash 不符；删一条 → 序号跳号；改一条并重算它的 hash → 下一条 prev 对不上。还原后校验完整 |

## 5. 当前没有覆盖的范围

- **整链重算**：能写文件的人从被改处开始重算全部哈希，本地校验发现不了。没有做锚定（定期把最新哈希存到别处）。
- **身份**：观测台登录后，观测台发起的操作记录 `user`（登录名）与 `tenant`，见 [Day 20 实践 §6](../day-20/day-20-lab.md#6-多租户与登录补充)；命令行运行的记录没有用户名。
- **保留与轮转**：单个文件一直增长，没有轮转、归档和保留期限；读取与校验每次从头扫描。
- **参数哈希**：没有加盐，短且可穷举的参数可以从哈希猜回原文。
- **范围**：只读调用、模型请求本身、MCP server 进程内部的动作不记；评测与队列的调度动作没有单独记录（它们启动的 agent 会记 `task_start`）。
- **告警**：没有定期校验与基于规则的告警。
