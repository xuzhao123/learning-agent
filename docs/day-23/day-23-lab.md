# Day 23 项目实践：编码工具集

先读 [学习笔记](day-23-notes.md)。对应能力：H06 编码工具（读、搜、改三项），H09 工具接口设计。

## 改动一览

用 `-workspace <目录>` 指定一个仓库后，agent 多出 5 个工具，只在这个目录里读写：

| 工具 | 要点 |
| --- | --- |
| `read_file` | 每行带行号（`%6d` + 制表符）；`offset`/`limit` 分页，默认 200 行、最多 2000 行；单行超过 2000 字符截断（原为 300，10-19 放宽：观测台页面一行 800 多字符，截断后 edit_file 没法原样复制，模型只能绕去 bash 看原文）；超过 2MB 或二进制拒绝；结果里有 `total_lines`，没读完时给 `next_offset`。读到工具输出上限（`-tool-output-tokens`）的八成就提前停下，按写进观测 JSON 后的长度算（`<`、`>` 会转义成 6 个字节），所以结果不会被转存给 `read_output`（10-20 前会，行号印两遍） |
| `glob` | `*`、`?` 不跨目录，`**` 跨任意层；模式里没有 `/` 时只匹配文件名；跳过 `.git`、`.data`、`node_modules`；最多 200 个 |
| `grep` | Go 正则（RE2），可限定 `path`、`glob`、`ignore_case`；返回 `路径:行号:内容`，最多 100 处；跳过二进制和超过 2MB 的文件 |
| `edit_file` | `old_string` → `new_string`；没找到、出现多处（报次数和行号）、与新内容相同都拒绝；`replace_all` 替换全部；必须先读过、读后没被改过；返回改动片段 |
| `write_file` | 新建文件（自动建上级目录），或整体覆盖一个读过且没被改过的文件 |

| 改动 | 代码 | 作用 |
| --- | --- | --- |
| 工具实现 | [workspace.go](../../internal/workspace/workspace.go) | 5 个工具；所有路径经 `os.Root`；`seen` 记录每个文件最近一次读到或写入的内容哈希；`Seen`、`RestoreSeen` 让这张表能随检查点进出 |
| 分发 | [dispatch.go](../../internal/agent/dispatch.go) | 5 个工具交给 `workspace.Run` |
| 执行器 | [react.go](../../internal/agent/react.go) | `read_file`、`glob`、`grep` 列入可重复（只读）；`edit_file`、`write_file` 和 bash 一样按调用顺序串行；读到工作区内容记为私有来源；系统提示词加入工作区说明；每次写检查点时带上账本，开局按续跑/续聊恢复 |
| 检查点 | [checkpoint.go](../../internal/agent/checkpoint.go) | `Seen` 字段存账本；`ForkSnapshot` 不继承它 |
| 入口 | [main.go](../../main.go) | `-workspace` 参数；子 agent 继承；不与 security-lab、实验模式组合 |

没有单独做权限：只有显式给了 `-workspace` 才有写工具，目录内的写入直接执行。写入不在可重复名单里，所以沿用 D21 的规则，每次结束写一条审计（只存参数哈希），续跑遇到结果未知时先问人。统一的权限模式与规则在 D25。

## 1. 核对与写入

编辑的核心只有几行：持锁、重新读、核对、计数、替换、写回、更新账本。

```go
mu.Lock()
defer mu.Unlock()
data, err := read(path)          // 经 os.Root；越界、二进制、超大都在这里拒绝
if err := fresh(path, data); err != nil {
	return nil, err              // 没读过，或读过之后被改过
}
count := strings.Count(text, oldString)
// count == 0：没找到；count > 1 且没有 replace_all：报次数与行号
...
root.WriteFile(path, []byte(updated), 0o644)
seen[path] = hash([]byte(updated)) // 自己的写入更新账本，接着改同一文件不用重读
```

越界检查完全交给 `os.Root`：`../../.env` 和指向工作区外的符号链接，在解析时就被拒绝，工具再把标准库的 `path escapes from parent` 翻译成“超出工作区”。

## 2. 账本随检查点走

账本只在进程内存里时，续聊换进程就丢：上一轮读过的文件，下一轮被当成没读过。修法是把账本存进检查点，开局再恢复。

```go
save := func() error {            // 每次写检查点
	...
	cp.Seen = workspace.Seen()    // 返回副本：序列化时工具可能还在往里写
	return cp.write()
}
// 开局：续跑用自己检查点里的，续聊用上一轮的，全新任务从空账本开始
ledger := cp.Seen
if resume == nil && Continues != "" {
	if prev, err := LoadCheckpoint(Continues); err == nil {
		ledger = prev.Seen
	}
}
workspace.RestoreSeen(ledger)
```

恢复的只是“读过这个文件”和当时的哈希，`fresh` 并不因此放行：编辑前照常重读磁盘、比对当前内容的哈希，两轮之间被改过照样拒绝，要求重新读。

两条边界：

- **子 agent 不继承**。它是另一个进程（`RunChild` 启动），账本从它自己的检查点恢复。父 agent 的上下文它看不到，凭父的记录改文件等于改一个自己没看过的版本。
- **回放分叉从空账本开始**。`ForkCheckpoint`、`ForkSnapshot` 的 View 被截断过，来源检查点的账本里可能有截断点之后才读的文件；从空的开始，代价最多是重新读一次，方向是安全的一侧。

验证方式：跑一轮读文件并修改，把上下文序列化成历史，用新检查点ID加 `-continues` 续聊，直接让模型改同一文件（不用再读）——改动应成功且检查点文件里有 `seen`；撤掉 `RestoreSeen` 后同样的续聊应被拒“需要先用 read_file 读过它”。另做一次反向检查：两轮之间用另一个进程改动该文件，续聊里的编辑应被拒“在你上次读取之后被改过”。子 agent 与回放分叉则应要求自行重读。

## 3. 动手

在本仓库的一个 git worktree 里验证，不碰主仓库，也不动 [Day 22 的自用任务集](../day-22/day-22-lab.md#3-自用任务集)：

```bash
git worktree add --detach .data/worktrees/d23 HEAD
go run . -provider deepseek -workspace .data/worktrees/d23 -max-steps 15 \
  -context-window 64000 -tool-output-tokens 6000 \
  -question "找到判断 IP 是否属于公网全局单播地址的 Go 函数，读懂后在它的文档注释最后补一行……"
git -C .data/worktrees/d23 diff
git worktree remove --force .data/worktrees/d23   # 用完删除
```

默认的 `-context-window 12288`、`-tool-output-tokens 2000` 是第 3 天为观察压缩设的小值，读源码时一页就会被截断，所以这里调大。

## 4. 验证记录

DeepSeek 真实运行 6 次（方舟仍是 HTTP 403），全部在 worktree 里：

| 任务 | 观察到的行为 |
| --- | --- |
| 找函数并补注释 | 先并行两次 `grep`，读目标文件，再用 `grep` 找调用方并读一处，`edit_file` 一次成功，最后按行号范围读回确认；`git diff` 只有一行新增；审计里有一条 `edit_file` 的 `tool_call` |
| 边界检查 | `old_string="return true"` 被拒：出现 3 次（第 50、54、58 行）；模型按提示带上下文改中第一处。`../../../.env` 和指向它的符号链接 `env-link` 都被拒绝；`glob env*` 列不出这个链接（只列普通文件） |
| 外部修改 | 读文件 → `slow_job` 期间另一个进程往文件末尾追加一行 → `edit_file` 被拒“在你上次读取之后被改过” → 模型重新读、再改成功；最终文件同时保留外部追加和模型的修改 |
| 目标文件不在快照里 | 第一次“同一批两处编辑”指向了一个只在主仓库未提交改动里的文件；模型读失败后用 `glob`、`grep` 搜了几轮，如实报告找不到，没有编造内容 |
| 同一批两处编辑 | 两个 `edit_file` 在同一次回复里发出，都成功，两处改动都在。模型先猜错了缩进（以为是两个制表符），两处都“没找到”，随后用 `grep` 数空格再改对 |
| 依赖前一处的编辑 | 第二处的原文是第一处的结果；模型自己分成两轮，没有放在同一批，所以同一批的顺序执行这次没有被真实运行覆盖，只按代码确认 |

`gofmt -l` 无输出，`go vet` 与 `go build ./...` 通过。验证用的 worktree 与符号链接已删除；检查点和审计记录留在 `.data/`（不提交）。

两个值得记下的现象：

- **缩进是字符串替换最常见的失败**。读工具的格式是“行号 + 制表符 + 原文”，原文本身又以制表符开头，模型数错了。错误信息提示了“检查空格与缩进”，模型能自己纠正，但多花了两轮。D37 做编辑格式对比时，这是要量化的一项。
- **模型会主动读回确认**。成功修改的 5 次里有 4 次改完又读了一遍，可能与系统提示词要求“改完说明改了哪几处”有关；这也让“自己的写入更新账本”这条路径很少被用到。

## 5. 当前没有覆盖的范围

- **bash 还看不到工作区**：模型能改代码，但还不能在沙箱里 `go build` 验证。按 D22 的约定放到 D24–D25。
- **观测台不能指定工作区**：只在命令行可用。
- **账本只跟着续接链走**：续跑、续聊能接上，子 agent 与回放分叉从空账本开始，改之前要重新读。这是安全的方向（多读一次），但会多花轮次；账本按相对路径记，换了工作区目录续聊时，只有内容也完全相同时哈希才对得上（哈希是内容哈希，不区分目录）。
- **同一批里既局部修改又整体重写同一文件**：两者按顺序执行，整体重写会盖掉前面的局部修改，见笔记第 4 节。
- **没有文件快照**：改坏了只能靠 git 回退。按步回退在 D29。
- **没有代码智能**：改完看不到编译错误和类型诊断，要等 bash 能在工作区里编译。
- **大仓库性能**：glob、grep 每次全量遍历，没有索引，也不读 `.gitignore`。

## 6. 自用任务集状态

T1–T6 都没有动。T4（按工具标注调度）的范围随本日调整：bash、`edit_file`、`write_file` 已在写死的顺序名单里，T4 要做的是用工具标注代替这份名单，并让 MCP 工具按 server 声明的标注调度。
