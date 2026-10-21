# Day 25 项目实践：权限规则、模式与工作区沙箱

先读 [学习笔记](day-25-notes.md)。对应能力：H10 权限策略，H11 OS 沙箱与网络，H13 配置（观测台设置里的默认模式与规则）。

## 改动一览

```text
工具调用
 → 允许列表（D19）
 → 用户规则  deny › ask › allow            -deny / -ask / -allow，或观测台设置
 → 权限模式默认  plan · ask · edit · full   -permission-mode，或输入框旁的“权限”
 → D20 高风险规则                          任何模式下都照常
 → 确认（tool/requestApproval，只由界面上的人回答）
 → 执行（bash 在沙箱里，工作区按模式挂成可写或只读）
```

| 改动 | 代码 | 作用 |
| --- | --- | --- |
| 权限包 | [permission.go](../../internal/permission/permission.go) | 规则解析与匹配（Bash 按命令切段，Edit/Read 按路径，WebFetch 按域名）；四种模式的默认；ask 模式下只读命令（ls、cat、git diff/log/status……）不问；`Describe` 写进环境信息 |
| 执行器 | [react.go](../../internal/agent/react.go) | 拒绝：不执行，记 `permission_deny` 审计；询问：并入 D20 的确认路径，风险标 `permission` |
| 入口 | [main.go](../../main.go) | `-permission-mode`、`-allow`、`-ask`、`-deny`；子 agent 继承模式与规则 |
| 沙箱 | [sandbox.go](../../internal/sandbox/sandbox.go) | 有工作区时 `/workspace` 挂仓库（plan 只读），`.git` 只读，`.env` 换成 `/dev/null`，`.data`、`.cache` 换空目录；本对话目录挂 `/scratch`；Go 工具链与模块缓存只读挂入，`GOPROXY=off`，编译缓存放 `.data/gocache`（按租户）；宿主机有 Node、Chrome 时也只读挂入（不在 `/usr` 下的挂到 `/opt/node`、`/opt/chrome`），提示词告诉模型改了网页要用 `node --check` 和无头 Chrome `--dump-dom` 实际检查一次（10-19 补：对话 review 发现改观测台页面三轮都没法验证）；bash 的工具描述也按有没有工作区写 `/workspace`、`/scratch`（10-20 补：原来一直是没有工作区的版本，说上传的文件在 `/workspace`） |
| 环境信息 | [context.go](../../internal/workspace/context.go) | 加一行“权限：模式与规则”，模型据此不去尝试会被拒的操作 |
| 观测台 | [settings.go](../../internal/observer/settings.go)、[server.go](../../internal/observer/server.go)、[index.html](../../internal/observer/index.html) | 设置里的默认权限模式与规则；输入框旁“权限”每轮可换（续聊带 `permission`）；规则拍进快照；确认卡片的风险显示“权限设置要求确认” |

## 1. 规则怎样匹配

```go
for _, effect := range []string{"deny", "ask", "allow"} {
	for _, r := range rules {
		if r.effect == effect && r.matches(category, name, value) {
			return effect, "规则 " + effect + ": " + r.text
		}
	}
}
return modeDefault(category, name, value), "权限模式 " + Mode
```

Bash 命令先按 `&&`、`||`、`;`、`|`、换行切段：允许规则要求每一段都命中，拒绝规则只要一段命中。所以 `-allow "Bash(go vet *)"` 放行 `go vet ./...`，不放行 `go vet ./... && curl …`；`-deny "Bash(git *)"` 挡住 `go vet ./... && git log`。

## 2. 验证记录

DeepSeek 真实运行（方舟仍是 HTTP 403）。目标仓库是本仓库的临时 worktree 或 `git clone --local`，用完删除。

| 场景 | 观察 |
| --- | --- |
| edit 模式，改代码后在沙箱编译 | `edit_file` 直接成功；模型在 bash 里 `go build ./internal/...` 与 `go vet ./internal/netguard` 都通过（离线，冷缓存约 60 秒）；改动只落在 worktree |
| 沙箱边界（clone 里放了假的 `.env` 和 `.data`） | `cat /workspace/.env` 读不到内容；`/workspace/.data` 是空目录；`git log` 能用；`git commit` 失败：`.git/index.lock: Read-only file system`；`/workspace` 与 `/scratch` 都能写 |
| plan 模式 | 环境信息里写着“plan（只读）”，模型没有尝试编辑，读完代码给出计划；文件未改 |
| ask 模式 + `-allow "Bash(go vet *)"` + `-deny "Bash(git *)"`（命令行，没有界面） | `go vet` 直接执行；`go vet … && git log` 被规则拒绝；`ls` 和 `edit_file` 进入确认，没有界面所以按拒绝处理；审计里依次有 `permission_deny`、`approval_request`、`approval_decision`（unavailable） |
| 观测台：ask 模式 + 设置规则 `deny: Bash(git push *)` | 编辑前出现确认请求（风险 permission），经确认接口允许后修改落盘；agent 启动参数带 `-permission-mode ask -deny Bash(git push *)` |
| 观测台：同一对话下一轮切到 plan | 续聊的启动参数变成 `-permission-mode plan`，规则沿用；模型给出计划，没有改文件 |
| ask 模式的只读命令 | 上一个观测台运行里，模型想用 `git diff` 复核也要确认，等到超时；加入只读命令集后，`git -C /workspace diff --stat`、`git log -1 && git status --short` 直接执行，`git commit` 仍要确认 |

`gofmt -l` 无输出，`go vet` 与 `go build ./...` 通过。临时账号、设置文件、clone、测试对话与检查点已删除；`.data/gocache`（约 300MB）是沙箱的编译缓存，保留以免下次再冷编译。

## 3. 当前没有覆盖的范围

- **规则只看文本**：切段不理解引号、`sh -c`、变量拼接；`Read(.env)` 管不住 `grep -r` 搜全仓库。可靠的边界是沙箱（`.env` 在沙箱里已换成空文件）。
- **文件工具不在沙箱里**：read_file 等由 `os.Root` 限定在工作区内，但能读工作区里的 `.env`；需要时用 `-deny "Read(.env)"`。
- **没有分类器审批**：Claude Code 的 auto 模式、Codex 的自动审查是另一个模型先筛一遍；这里只有人。
- **没有执行前 hook**：管线里的 hook 在 D26 加入。
- **没有项目级权限文件**：规则来自命令行或观测台设置，仓库里不能放一份共享的权限配置；配置分层只有内置默认 › 平台 › 个人。
- **沙箱资源**：内存 512MB、256 个进程（10-19 从 64 放宽：无头 Chrome 64 时建不了线程直接崩），编译大项目可能不够；编译缓存放在 `.data/gocache`，没有清理。
- **工作区可写时 bash 改了文件**：读后才写的账本会发现（D24 提醒、D23 拒绝过期写入），但改动本身没有快照，回退靠 git（D29 文件快照）。
