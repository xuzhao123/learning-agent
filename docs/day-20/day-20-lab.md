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
  → bash（能读到用户文件时）/ 记忆 / 子 agent 成功返回 → 标记本任务含私有数据
```

| 改动 | 代码 | 作用 |
| --- | --- | --- |
| 风险规则与确认 | [approval.go](../../internal/agent/approval.go) | `risk` 判断四类风险，`approve` 发 `tool/requestApproval` 并等待；没有界面按 unavailable 处理，结果 `denied` |
| 私有数据标记 | [approval.go](../../internal/agent/approval.go)、[react.go](../../internal/agent/react.go) | bash 成功且能读到用户文件（`sandbox.SeesUserFiles`：挂了工作区或项目，或本对话目录里已有文件；10-21 起，空沙箱里的 `echo ok` 不再算）、记忆召回或 search_memory 成功、子 agent 含私有数据 → 记来源；之后浏览器与 MCP 工具要确认（full 模式由程序放行，见 [Day 25 §6](../day-25/day-25-notes.md#6-和高风险确认的关系)）。存进检查点，续跑、续聊延续 |
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

外发类卡片多一个“本对话内 … 都允许”（10-19 补）。起因是一次自用对话的 review：有工作区的对话几乎每轮都读私有数据，之后每次查公开资料都要人点一次。范围由 agent 按程序给出，不由模型写：

| 工具 | 范围 |
| --- | --- |
| `web_search`、`ds_search`、`mcp_*` | 工具名：搜索词都发给同一个服务 |
| `open_page`、`ds_fetch` | 工具名 + 主机，如 `ds_fetch · go.dev`；网址解析不出主机时只能单次允许 |

选了之后 agent 回复 `allow_session`，把范围记进检查点的 `granted`：同一范围之后直接放行，但照样先写一条 `approval_decision`（`decision=allow_session`，`actor=program`，写不进审计就不执行）；续跑、续聊沿用，子 agent 和回放分叉不继承，仍要问。代价是这个范围之后的参数不再逐条过目——搜索词里带上私有内容也不会再问，所以只给外发类，不可撤回、破坏性、重做和权限询问仍然只能单次允许。

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

“本对话内都允许”（10-19，DeepSeek，临时平台账号在另起的观测台上运行，验证后删除账号，对话记录保留）：

| 场景 | 结果 |
| --- | --- |
| 同一轮两次搜索 | bash 之后第一次 `ds_search` 请求确认、带 `scope=ds_search`；接口回复 `session: true` 得到 `allow_session`，第二次 `ds_search` 终端打印 `Approval skipped … scope=ds_search`，没有再发确认 |
| 续聊沿用 | 检查点里 `granted: ["ds_search"]`；重启观测台后续聊，新一轮的 `ds_search` 同样直接放行 |
| 页面按钮 | chromedp 登录后打开对话：卡片有“允许 / 本对话内 ds_fetch · go.dev 都允许 / 拒绝”，点中间那个后显示“已允许，本对话内 ds_fetch · go.dev 不再询问”，agent 收到 `allow_session` |
| 审计 | 请求、人的点击（观测台记 `user_decision`）与之后的放行（`actor=program`）都在链上，`go run . audit` 校验完整 |

## 5. 当前没有覆盖的范围

- **粗粒度**：标记的是“任务接触过私有数据”，不追踪具体值；含私有数据后，查一个与它无关的公开网页也要确认。
- **白名单内的外发**：bash 仍可向用户批准过的域名发送数据（Day 12 的白名单不区分路径与内容），这里没有额外确认。
- **MCP 注解**：不可信 server 可以不声明 `destructiveHint`，破坏性工具因此不需确认；它只在含私有数据后受外发检查约束。
- **审计**：D21 起确认请求与决定写进独立的审计日志（哈希链），见 [Day 21 实践](../day-21/day-21-lab.md)。
- **多租户**：已补做，见下面第 6 节；那里另列了多租户部分没有覆盖的范围。
- **结果日志窗口**：工具已生效、日志还没写下时被 kill，仍是结果未知；续跑重新执行可重复工具时不再询问。
- **repeatable 名单**：内置工具是否可重复仍按工具名写死，误标仍会静默重做；结果日志降低了它的触发机会。

## 6. 多租户与登录（补充）

最初只在笔记里讲了多租户概念，之后按用户要求补做：观测台加真正的登录，数据按租户隔离，功能与并发按租户限制。

```text
浏览器 ──登录（PBKDF2 校验）──▶ 观测台：会话 Cookie（HttpOnly、SameSite=Strict）
   │ 每个请求：protect 查会话 → 改状态的请求再查同源 → runOf 核对对话属于本租户（否则 404）
   ▼
观测台启动 agent：-tenant <租户> + AGENT_USER（进审计）+ AGENT_PROXY_TOKEN（模型代理只认这个进程）
   ▼
agent 所有读写落在 .data/tenants/<租户>/：checkpoints · memory.json · sandbox · browser · runs · audit
   └ 子 agent、续跑、续聊沿用同一个租户；别的租户的任务ID、子任务、记忆编号在这里找不到
```

| 改动 | 代码 | 作用 |
| --- | --- | --- |
| 数据空间 | [tenant.go](../../internal/tenant/tenant.go) | `tenant.Path(租户, …)`：平台是 `.data/…`，租户是 `.data/tenants/<租户>/…`；agent 用 `-tenant` 确定本进程的空间 |
| agent 各模块 | [checkpoint.go](../../internal/agent/checkpoint.go)、[child.go](../../internal/agent/child.go)、[memory.go](../../internal/memory/memory.go)、[main.go](../../main.go) | 检查点、结果日志、记忆与索引、沙箱和浏览器目录都按空间取路径；`RunChild` 按 `ChildIO.Tenant` 读写并给子进程带上 `-tenant`；`-resume` 可与 `-tenant` 一起用 |
| 审计 | [audit.go](../../internal/audit/audit.go) | 每个空间一条链；记录增加 `user`、`tenant`；`go run . audit -tenant 租户` |
| 登录 | [auth.go](../../internal/observer/auth.go) | 账号在 `.data/users.json`（PBKDF2-SHA256 60 万次、每账号随机盐）；会话令牌只存哈希、12 小时过期；连续 5 次失败锁定 1 分钟；未知用户名也算一遍哈希；登录、失败与退出写审计 |
| 观测台 | [server.go](../../internal/observer/server.go)、[eval.go](../../internal/observer/eval.go)、[index.html](../../internal/observer/index.html) | 对话带租户；列表、事件流、续聊、停止、确认、联网审批、画面、调用链都只对本租户可见；评测、归因、Skills/MCP 修改与全局指标只给平台账号；页面显示账号、可退出，隐藏没开通的功能 |
| 租户策略与配额 | [auth.go](../../internal/observer/auth.go)、[queue.go](../../internal/queue/queue.go) | `tenants` 里的 `features` 与 `max_running`（默认全部功能、2 个）；队列任务可带 `tenant`，`-per-tenant` 限制每个租户同时占用的 worker |
| 模型代理 | [llm.go](../../internal/llm/llm.go)、[server.go](../../internal/observer/server.go) | 每次启动 agent 生成令牌，经 `X-Agent-Token` 核对；没有令牌的本机进程不能往别人的对话里写请求记录 |

### 账号与启动

```bash
go run . observe -add-user admin                                  # 平台账号：看到原有的 .data/ 数据，可用评测与归因
go run . observe -add-user alice -tenant acme -max-running 1      # 租户账号，并设置租户策略
go run . observe -add-user bob -tenant globex -features memory    # 只开通长期记忆
go run . observe                                                   # 没有账号时拒绝启动
```

密码从标准输入读（终端里不回显，也可以用管道传入），不出现在命令行和 shell 历史里。改账号后重启观测台生效。

队列任务加 `"tenant": "acme"` 即在该租户空间里执行：

```bash
go run . queue -workers 3 -per-tenant 1 tasks.jsonl
go run . metrics -tenant acme
```

### 验证记录

2026-10-11 使用 DeepSeek，三个测试账号（平台 admin、acme 的 alice、globex 的 bob），验证后已删除测试账号与租户数据。

| 场景 | 结果 |
| --- | --- |
| 登录 | 未登录访问首页 303 跳登录页、接口 401；错误密码 401；Cookie 带 HttpOnly；跨站 Origin 的 POST 403；账号文件里只有盐和 PBKDF2 哈希；密码少于 8 位拒绝保存 |
| 记忆隔离 | alice 记住“内部项目代号是蓝鲸”，写进 `tenants/acme/memory.json`；bob 开记忆问同一问题，召回 0 条，模型回答“不知道” |
| 越权访问 | bob 访问 alice 的对话：列表里没有；事件流、续聊、确认、停止、调用链全部 404；按对话看指标 403；alice 自己访问 200。平台 admin 的列表里也没有租户对话，读事件流同样 404 |
| 平台功能 | bob 访问评测、修改 Skills、全局指标 403；admin 访问评测 200 |
| 功能策略与配额 | bob 发起带 bash 的对话 403（未开通）；alice 运行中再发起一个，429（上限 1） |
| 模型代理 | 不带令牌或令牌错误调用 `/llm/<对话>`，403 |
| 命令行跨租户 | `-tenant globex -resume <acme 的任务>`：globex 目录下找不到检查点；`-tenant acme` 才取回存档 |
| 租户内的确认与子 agent | alice 的对话里子 agent 外发请求确认；bob 抢答 404，alice 允许后执行；子任务的检查点、浏览器画面、沙箱目录都在 acme 下；acme 的审计链记录 `user=alice`，globex 的链里只有 bob 的记录 |
| 队列 | acme 两个任务、globex 一个，`-per-tenant 1`：acme 第二个在第一个结束后才开始，globex 与之并行 |
| 页面 | bob 登录后侧栏显示“bob · 租户 globex”和退出按钮；评测、实验入口与未开通的开关都不显示 |

### 多租户部分没有覆盖的范围

- **单机、单进程观测台**：会话存在内存里，观测台重启要重新登录；没有多实例共享会话。
- **没有 HTTPS**：只监听本机，Cookie 没有 `Secure`；部署到网络上必须先加 TLS。
- **账号管理**：只有命令行添加与重设密码，没有删除、改名、自助改密码，也没有多因素认证；角色只有“平台/租户”两种。
- **共享的部分**：模型密钥与配额、调用链文件、Skills/MCP 配置、知识库索引由所有租户共用；平台账号的全局指标会汇总所有租户的 trace（不含内容）。
- **文件系统层面**：隔离靠路径，所有租户的数据仍属同一个系统用户，能登录服务器的人可以直接读；没有按租户加密或分系统用户。
- **令牌传递**：第三方 stdio MCP server 继承 agent 的环境变量，会拿到模型代理令牌（沙箱里的 bash 不会，bwrap 清空了环境）。
- **资源配额**：只限制同时运行的对话数，没有按租户限制 token 用量、磁盘与模型费用。

