# Day 26 项目实践：Hooks 与斜杠命令

先读 [学习笔记](day-26-notes.md)。对应能力：H14 Hooks，H15 斜杠命令，H13 配置与信任（hooks 只来自用户和平台）。

## 改动一览

```text
用户输入
 → 斜杠命令展开（/名字 参数）                        main.go → commands.Expand
 → UserPromptSubmit hook   拦下：不进记忆、不发模型   react.go Run 开头
 → 模型 ⇄ 工具
     工具调用 → PreToolUse hook（拒绝 / 询问 / 放行 / 改写）
              → 权限规则与模式（D25，用改写后的参数）→ 高风险确认（D20）→ 执行
              → PostToolUse hook（反馈附在结果的 hook_feedback 上）
 → 模型想结束 → Stop hook   拦下：原因作为 <system-reminder> 追加，接着请求模型
```

| 改动 | 代码 | 作用 |
| --- | --- | --- |
| hooks 包 | [hooks.go](../../internal/hooks/hooks.go) | 解析 Claude Code 格式的配置；按 matcher 依次运行命令（stdin 写 JSON，`sh -c`，默认 60 秒超时，只给 PATH、HOME 与两个目录变量）；退出码 2 或 JSON 的 block/deny 是拦截，其他非零码失败放行；每次运行发 `hook/completed` 通知，拦截写审计 `hook_block` |
| 执行器 | [react.go](../../internal/agent/react.go) | 四个事件的接入点；改写记审计 `hook_rewrite` 并在结果的 `note` 里告诉模型；hook 的 allow 只免掉模式默认的询问 |
| 命令包 | [commands.go](../../internal/commands/commands.go) | 个人级 `commands/*.md` 与项目级 `<工作区>/.agents/commands/*.md`（同名项目级优先）；`$ARGUMENTS`、`$1…$9`；未知命令报错 |
| 入口 | [main.go](../../main.go) | `-hooks`（JSON 原文或文件路径）；子 agent 继承；问题以 `/名字` 开头时先展开 |
| 观测台 | [settings.go](../../internal/observer/settings.go)、[server.go](../../internal/observer/server.go)、[pages.js](../../internal/observer/web/pages.js)、[composer.js](../../internal/observer/web/composer.js)、[chat.js](../../internal/observer/web/chat.js) | 平台设置 `hooks`（个人层改它返回 403），对话开始时读成原文拍进快照；`GET /commands` 列出命令，输入框打 `/` 弹出菜单；工具上方、回答下方显示 HOOK 卡片 |
| 配置与脚本 | [hooks/](../../hooks/hooks.json)、[commands/review.md](../../commands/review.md)、[.agents/commands/self-task.md](../../.agents/commands/self-task.md) | 见第 1 节 |

## 1. 本仓库实际使用的 hooks 与命令

`hooks/hooks.json` 用 `$AGENT_HARNESS_DIR` 引用脚本：hook 的工作目录（也是 `$AGENT_PROJECT_DIR`）是工作区（可能是别的仓库），脚本在 harness 目录。

没有工作区时，工作目录是本对话的沙箱目录（bash 里的 `/workspace`）。10-21 之前退回的是 harness 目录：一段只做搜索的自用对话结束时，Stop 的 `go-check.sh` 查到 harness 自己未提交、暂时编译不过的改动，把报错交给了一个看不到这份代码的模型，模型在空沙箱里找了两轮文件才说明做不了。现在沙箱目录里没有 `go.mod`，检查直接通过；`Hooks:` 启动行会打印实际目录。

| 事件 | 脚本 | 做什么 |
| --- | --- | --- |
| UserPromptSubmit | [secret-guard.sh](../../hooks/secret-guard.sh) | 问题里像有 API 密钥或私钥时退出码 2 拦下 |
| PreToolUse（edit_file、write_file） | [protect-generated.sh](../../hooks/protect-generated.sh) | 目标文件开头有 `// Code generated … DO NOT EDIT.` 时返回 `permissionDecision: deny` |
| PostToolUse（edit_file、write_file） | [gofmt.sh](../../hooks/gofmt.sh) | 改过的 `.go` 文件没格式化就直接 `gofmt -w`；`gofmt` 报错（语法错误）时 `decision: block` 把错误交给模型 |
| PostToolUse（edit_file、write_file） | [js-check.sh](../../hooks/js-check.sh) | 改过的 `.js`，或 `.html` 里的内联脚本有语法错误时 `decision: block`，把报错位置交给模型；页面 embed 进 Go 程序，`go build` 查不出脚本错误。宿主机没有 node 时跳过（10-19 补） |
| Stop | [go-check.sh](../../hooks/go-check.sh) | 工作区有未提交的 `.go` 改动时跑 `go build` 与 `go vet`，不过就退出码 2 |

不正常结束（预算用完、模型报错、超时、熔断，不含人按停止）时，harness 把 Stop hook 当作收尾检查再跑一次（10-21 起，`finalCheck`）：不再请求模型，结论（通过，或 hook 给出的原因）写进检查点的 `final_check` 和 `turn/completed`，页面的停止卡片上显示。实测：临时 clone 里放一处编译不过的改动，`-max-steps 1` 用完预算后，`final_check` 记下了 `go build` 的语法错误。

```bash
go run . -provider deepseek -workspace ../some-repo -bash -hooks hooks/hooks.json -question "/review 关注错误处理"
```

命令：`/review [关注点]`（个人级，评审未提交改动、不改文件）；`/self-task T1`（本仓库的项目级命令，做 [D22 自用任务集](../day-22/day-22-lab.md#3-自用任务集) 里的一项，D28 用）。

## 2. 读代码：执行前 hook 与权限的衔接

```go
hooked = hooks.Run(ctx, "PreToolUse", call.Function.Name, map[string]any{"tool_name": ..., "tool_input": rawArgs(call), "tool_use_id": call.ID})
if hooked.Block { return denied }                       // hook 拒绝：不执行
if hooked.Input != nil { call.Function.Arguments = string(hooked.Input) } // 改写，记审计
effect, why := permission.Decide(call.Function.Name, call.Function.Arguments) // 用改写后的参数
switch {
case hooked.Decision == "ask" && effect != "deny":                                // hook 只能收紧
	effect = "ask"
case hooked.Decision == "allow" && effect == "ask" && !strings.HasPrefix(why, "规则"): // 只免掉模式默认的询问
	effect = "allow"
}
```

Stop hook 在“回复没有工具调用”处插入：拦下时追加 `[hook Stop] 还不能结束：…` 提醒，`continue` 回到循环顶部；请求预算照常计数，所以不会无限继续。

## 3. 验证记录

DeepSeek 真实运行（方舟仍是 HTTP 403）。工作区是本仓库的临时 `git clone --local`，加了一个带 `DO NOT EDIT` 头的 `internal/gen/gen.go`；用完删除。

| 场景 | 观察 |
| --- | --- |
| 问题里贴了 `sk-…` | UserPromptSubmit 拦下，结局 `hook_blocked`，没有模型请求、没有记忆检索；审计有 `hook_block`。第一次运行发现检查点的启动参数里还留着问题原文，改为一起替换后复跑，检查点里搜不到密钥 |
| 让模型照抄一行没格式化、还调用了未定义函数的 Go 代码，“写完就结束” | PostToolUse 拦截并给出 gofmt 差异 → 模型改成 gofmt 格式 → 想结束时 Stop hook 拦下（`undefined: helperOffset`）→ 模型补上函数 → Stop 通过，结局 `no_tool_calls`，clone 里 `go build` 通过 |
| 改生成文件的常量 + 跑测试 | edit_file 被 protect-generated 拒绝，模型在回答里说明要改生成器；其余调用照常 |
| 临时加一个 bash 的 PreToolUse hook：`go test` 补上 `-count=1` | 命令被改写后执行，审计有 `hook_rewrite`，结果的 note 写明改写后的参数；同样的运行再加 `-deny "Bash(*-count=1*)"`，改写后的命令被规则拒绝 |
| `/nosuch` | 报错“没有 /nosuch 这个命令；可用的命令：/review、/self-task”，不请求模型 |
| `/review 重点看大小写处理`，plan 模式，clone 里删掉了 `host()` 的 `ToLower` | 第一版模板下模型评审过细（在 `/scratch` 写复现程序、重复查文档），20 次请求用完；模板加上“查到能下结论就停，不另写程序复现”后 10 次请求完成：指出大小写不同的 URL 能绕过 `WebFetch(domain:…)` 拒绝规则，并发现规则里的域名本身也没有转小写（原代码就有的问题，记入待办） |
| 观测台 | 平台设置 `hooks: hooks/hooks.json`，格式错误保存时报 400，租户账号改 hooks 返回 403；`GET /commands` 平台账号选了工作区时有项目级命令，租户账号（没开通工作区）只有个人级；格式化与 Stop 场景在观测台复跑，四个事件的 `hook/completed` 都带上了，工具级的带 `call_id`；截图确认 HOOK 卡片与 `/` 菜单显示正常 |

`gofmt -l` 无输出，`go vet` 与 `go build ./...` 通过，页面脚本 `node --check` 通过。临时账号、设置文件、clone、测试对话与检查点已删除。

## 4. 当前没有覆盖的范围

- **没有项目级 hooks 与信任确认**：只读用户（`-hooks`）和平台设置的配置；工作区里的配置不读。
- **失败放行**：hook 崩溃、超时都照常继续；安全检查不能只靠 hook。
- **hook 不在沙箱里**：以 harness 的身份在宿主机运行，环境变量已收窄，但文件系统和网络没有限制；Stop 的编译检查也在宿主机上跑。
- **事件不全**：没有会话开始、压缩前后、子 agent 结束、工具失败（PostToolUseFailure）等；续跑补齐中断前那一批时不跑 hook。
- **同一事件的 hook 顺序执行**（Claude Code 是并行）：一个拦截就不跑后面的。
- **斜杠命令只有模板**：没有内置命令（D34 的终端客户端里做恢复、分叉、压缩等命令）；不支持在模板里执行命令或引用文件；skill 不能用 `/名字` 触发。
- **展开后的模板就是用户消息**：记忆把它当作用户原话，长模板会占用 `remember_memory` 的引文来源。
