# Day 24 项目实践：项目指令、环境信息与系统提醒

先读 [学习笔记](day-24-notes.md)。对应能力：H03 提示词组装。

## 改动一览

只在用了 `-workspace` 时生效；没有工作区的对话和以前一样。

```text
系统提示词（View[0]，一个对话内不变）
  基本规则 + 各工具规则 + 工作区说明 + 根目录项目指令（AGENTS.md，没有时读 CLAUDE.md，最多 32KB）
  + 记忆召回块 + Skills 索引
<system-reminder> 环境信息：工作区、bash 里的工作区路径、分支与 HEAD、未提交改动数、日期、系统      ← 新对话的第一条
用户问题
……模型回答、工具调用与结果……
<system-reminder> 子目录项目指令 / 读过的文件被改了                           ← 每次请求模型之前，有就追加
……
（续聊）<system-reminder> 环境和上一轮不同 / 根目录项目指令改过               ← 变了才追加
（续聊，旧环境提醒已被压缩进摘要）<system-reminder> 环境信息                    ← 无从比较，重新给一份，不说“变了”
```

| 改动 | 代码 | 作用 |
| --- | --- | --- |
| 提醒消息 | [protocol.go](../../internal/llm/protocol.go) | `llm.Reminder` 生成带 `<system-reminder>` 的用户角色消息；`llm.FromUser` 判断是不是用户原话（排除摘要和提醒） |
| 项目指令与环境 | [context.go](../../internal/workspace/context.go) | `Open` 时读根目录项目指令；读、改、写文件后检查各级子目录有没有自己的项目指令，没进过上下文就排进提醒；`Reminders` 取走提醒，并检查读过的文件有没有被别人改过；`Environment` 用 git 取分支、HEAD 与改动数，并在 `BashMount` 非空时写出 bash 里的工作区路径 |
| 组装 | [react.go](../../internal/agent/react.go) | 系统提示词加入项目指令；新对话在问题前放环境信息，续聊对比上一份、变了才追加；每次请求模型之前追加提醒 |
| 排除提醒 | [memory.go](../../internal/memory/memory.go)、[context_manager.go](../../internal/agent/context_manager.go) | 记忆的“用户原话”和压缩时保留的原话都不含提醒 |
| 观测台 | [util.js](../../internal/observer/web/util.js) | 原始消息里的提醒标为“harness 提醒” |

“被改过”的提醒不更新读后才写的账本：模型要改那个文件，仍然得先重新读（D23 的核对照旧生效）。同一次改动只提醒一次。

## 1. 子目录项目指令按需加载

`note` 在读、改、写一个文件之后，从文件所在目录往上走到根，把还没进过上下文的各级目录找出来，从上往下检查有没有 AGENTS.md：

```go
for d := filepath.Dir(path); d != "." && d != "/" && !loaded[d]; d = filepath.Dir(d) {
	dirs = append([]string{d}, dirs...)
}
for _, d := range dirs {
	loaded[d] = true
	if name, text := doc(d); text != "" {
		pending = append(pending, fmt.Sprintf("[项目指令 dir=%s]\n…%s", d, text))
	}
}
```

提醒的第一行 `[项目指令 dir=…]` 是标记：`Prime` 按当前视图里的提醒重算 `loaded`。续聊是新进程，靠它不重复发送；压缩会把提醒并进摘要、原文离开视图，所以每次压缩后也重算一次，模型再碰那个目录时重新发一次原文。只在进程里记“发过”而不看视图，压缩之后约定就只剩摘要里的转述。

## 2. bash 里的工作区路径

文件工具按宿主机的绝对路径算相对路径，bash 里看到的却是挂载点常量 `/workspace`。两个路径不一致时，模型常拿环境信息里的宿主机路径去 `cd`，在沙箱里必然失败，白掉一轮。所以环境信息里多写一行：

```go
var BashMount string // main 在“开了 bash、而且工作区挂了进去”时设成 /workspace

if BashMount != "" {
	fmt.Fprintf(&b, "bash 里的工作区：%s（bash 命令里 cd、grep 都用这个路径；read_file、edit_file、write_file 等文件工具的 path 仍写相对工作区根目录的路径，不要带 %s 前缀）\n", BashMount, BashMount)
}
```

变量由 main 设置，而不是让 workspace 去 import sandbox：文件工具不该为了 bash 的挂载点依赖沙箱包，两者都由 main 组装（main 本来就在这里设置 `sandbox.Project`）。没开 bash 时这个变量为空，环境信息里少这一行，与以前一致。

验证方式：新对话同时开 `-workspace` 与 `-bash`，看第一条环境信息里有没有“bash 里的工作区”这一行；让模型先用 bash 在 `/workspace` 里 `git status`，再用 `read_file` 读同一目录下的文件，两条路径都要成功。只开 `-workspace` 时这一行不出现。

## 3. 验证记录

在本仓库的 git worktree 里，用 DeepSeek 真实运行 3 次（方舟仍是 HTTP 403）。根目录的 AGENTS.md 就是本仓库的协作规则（约 13KB，中文）；为了观察按需加载，在 worktree 的 `internal/netguard/` 下临时放了一份 AGENTS.md，要求“本目录的注释一律用英文写”。

| 运行 | 观察 |
| --- | --- |
| 补一句注释 | 模型先 `grep`，再读 `netguard.go`；读完之后、下一次请求之前出现提醒 `[项目指令 dir=internal/netguard]`。模型随后写的注释是英文，遵守了子目录约定（根目录约定是中文） |
| 等待期间外部修改 | 读文件 → `slow_job` 期间另一个进程往文件末尾追加一行 → 下一次请求前出现“在你上次读取之后被改过”的提醒 → 模型先重新读、再编辑，一次成功。D23 同样的场景是先编辑失败、再重读 |
| 续聊 | 把第一次运行的上下文作为历史续聊，期间工作区多了改动、根目录 AGENTS.md 临时加了一条“回答末尾说明遵守了哪条约定”。新进程追加了“环境和上一轮不同”“根目录的项目指令改过”两条提醒；模型据环境信息说出“2 个已跟踪文件有改动，其中一个是我改的”，回答末尾也照新加的约定写了说明 |

提示缓存（DeepSeek 返回的 `cached_tokens`）：

| 请求 | prompt_tokens | cached_tokens |
| --- | --- | --- |
| 第一次运行第 1 次 | 4698 | 0 |
| 第 2 次 | 4861 | 4608 |
| 第 3 次（读了文件，新增约 1.2k） | 6126 | 4864 |
| 第 4 次 | 6693 | 6400 |
| 续聊第 1 次（追加了两条提醒，含约 13KB 的项目指令全文） | 10206 | 6784 |

续聊是新进程，但前缀和上一轮一字不差，服务端照样命中了上一轮缓存的部分。

`gofmt -l` 无输出，`go vet` 与 `go build ./...` 通过。worktree、临时 AGENTS.md 和临时二进制已删除。

## 4. 当前没有覆盖的范围

- **没有全局（个人）项目指令**，也没有 `AGENTS.override.md`。
- **项目信任**：工作区由平台账号登记，登记即视为信任；没有“首次打开先确认”的流程。权限边界在 D25。
- **根目录指令改过时整份重发**：项目指令很长时，续聊多一大段输入；没有按差异发送。
- **环境信息只在对话开始和续聊时比较**：一轮里面切分支不会提醒；未提交改动数随模型自己的修改变化，所以续聊几乎每次都会追加一份新的环境信息。
- **文件变化靠逐个重读比对**：读过的文件多时每步都要读一遍磁盘；没有用文件系统通知。
- **bash 仍看不到工作区**：模型用 bash 改文件的情形要等 D25 挂载后才会出现；本日的“被改过”是另一个进程改的。
- **系统提示词的其他部分**（记忆召回、Skills 索引）沿用原来的位置，没有重新整理顺序；工具定义的顺序由启动参数决定，同一对话内不变。
