# Day 29 项目实践：文件快照与回退

先读 [学习笔记](day-29-notes.md)。对应能力：H19 文件检查点与回退；相关的有 H18 会话恢复与分叉（续接链）、H06 编码工具（读后才写）。

## 改动一览

有工作区（`-workspace`）的普通运行，`edit_file`、`write_file` 写入之前先存快照；`go run . rewind <轮次ID>` 或观测台上的“撤销到这一轮之前”把这一轮及之后各轮的文件放回去，对话退回它的上一轮。

| 改动 | 代码 | 作用 |
| --- | --- | --- |
| 快照与撤销计划 | [snapshot.go](../../internal/workspace/snapshot.go) | `snapshot` 写入前存“之前”、`written` 写入后记“之后”；`PlanRewind` 合并几轮的清单并核对当前内容；`Apply` 有冲突就一个也不动 |
| 写入路径 | [workspace.go](../../internal/workspace/workspace.go) | `editFile`、`writeFile` 在 `root.WriteFile` 前后各调用一次；快照写不进去就不改文件 |
| 按轮撤销 | [rewind.go](../../internal/agent/rewind.go) | 沿续接链往后找到最新一轮；给每轮加锁；先写审计再恢复；最后写 `rewound.json` 标记；`RewindCommand` 是命令行入口 |
| 入口 | [main.go](../../main.go) | `go run . rewind`；设置本轮的快照目录（子 agent 经环境变量 `AGENT_FILES_TURN` 写进父 agent 那一轮）；已撤销的轮次不能 `-resume`、不能 `-continues` |
| 观测台 | [server.go](../../internal/observer/server.go)、[index.html](../../internal/observer/index.html) | `POST /runs/{id}/rewind`（`dry_run` 预览）；调用 agent 的 rewind 命令，结果记成对话里的 `rewind` 事件；之后续聊从撤销点的上一轮的检查点接着；页面上被撤销的轮次淡化显示 |

## 1. 快照放在哪里

快照和检查点放在一起，一轮一个目录：

```text
.data/checkpoints/                     （租户在 .data/tenants/<租户>/checkpoints/）
  <轮次ID>.json                        对话检查点
  <轮次ID>.files/
    changes/<轮次ID>.json              父 agent 在这一轮的清单
    changes/<轮次ID>-sub-<哈希>.json   子 agent 在这一轮的清单（各写各的，两个进程不抢一个文件）
    blobs/<sha256>                     改之前的内容
    rewound.json                       撤销后才有
```

清单里每个文件只记第一次改之前（`before` 或 `created`）和最后一次改之后（`after`），外加两次的时间。几份清单改过同一个文件时，按时间取最早的“之前”、最晚的“之后”。

写入顺序（[workspace.go](../../internal/workspace/workspace.go) 的 `editFile`）：

```go
if err := snapshot(path, data, true); err != nil {   // 1. 存“之前”，清空“之后”
	return nil, fmt.Errorf("写入前的快照没存下来，%s 没有改：%w", path, err)
}
if err := root.WriteFile(path, []byte(updated), 0o644); err != nil { // 2. 真正写
	return nil, fmt.Errorf("写入 %s 失败：%w", path, err)
}
written(path, []byte(updated))                        // 3. 记“之后”
```

第 1 步先把 `after` 清空：进程死在第 2 步时，清单里是“之前有、之后空”，撤销时照样恢复，但不拿一个不确定的哈希去核对。

子 agent 为什么要写进父 agent 那一轮：子 agent 的任务ID以派出它的那一轮开头，但 `spawn_agent(task_id)` 可以在后面的轮次里续跑同一个子任务。按子任务ID存，就会把后一轮的改动记到前一轮名下。父进程启动时把自己的轮次ID放进环境变量，子进程继承它，所以子 agent 的改动总是记在“当时正在进行的那一轮”。

## 2. 撤销的核对

[snapshot.go](../../internal/workspace/snapshot.go) 的 `PlanRewind` 对每个文件只判断三种情况：

```go
switch {
case exists == !m.first.Created && current == original:   // 已经是原来的样子：跳过
	plan.Unchanged = append(plan.Unchanged, path)
case m.last.After != "" && (!exists || current != m.last.After): // 被别人改过：冲突
	plan.Conflicts = append(plan.Conflicts, path)
}
// 其余：新建的删除，改过的写回 blobs 里的原内容
```

[rewind.go](../../internal/agent/rewind.go) 的 `Rewind` 把它串起来：

1. 从目标轮次沿 `continues` 往后找没撤销的下一轮；有两个以上时拒绝（不知道磁盘上是哪一支）。
2. 给这几轮的检查点加锁，正在运行的轮次拿不到锁，直接报错。
3. `dry-run` 到这里就返回计划。
4. 有冲突：返回计划和错误，不改任何文件。
5. 写审计 `rewind_request`（写不进去就不做）→ 恢复 → 每轮写 `rewound.json` → 审计 `rewind`。

撤销是人发起的动作，不经过权限模式：plan 模式下也能撤销 agent 之前的改动。

## 3. 动手

命令行（检查点在平台空间；租户加 `-tenant`）：

```bash
# 两轮对话：第二轮用 -continues 接着第一轮（上下文取第一轮的检查点）
go run . -workspace <仓库> -task-id r1 -question "……"
jq '{messages: .messages, with_tools: true}' .data/checkpoints/r1.json \
  | go run . -workspace <仓库> -task-id r2 -continues r1 -history-stdin -question "……"

go run . rewind -dry-run r2   # 预览：turns、continues、restore、delete、unchanged、conflicts
go run . rewind r2            # 撤销第二轮；之后应接着 r1 聊
go run . audit | grep rewind  # 审计里的 rewind_request 与 rewind
```

观测台：选了工作区的对话，每条用户消息下面有“撤销到这一轮之前”。点击后先预览，列出会恢复、删除的文件；有冲突时只提示，不执行；确认后执行，页面重新加载，被撤销的轮次淡化，下一条消息从上一轮接着。

可以自己观察：

- 撤销之前手动改一下 agent 改过的文件，看冲突怎样报告、文件有没有被动；
- 撤销之后再问模型“到目前为止你改过哪些文件”，它的回答应该只包含没被撤销的轮次；
- 对已撤销的轮次用 `-continues` 或再撤销一次，都会被拒绝。

## 4. 验证记录

2026-10-19，DeepSeek，临时 clone 里进行，用完删除。

| 检查 | 结果 |
| --- | --- |
| 第一轮新建 `queue/README.md`、改 `go.mod` | 清单记下 `go.mod` 的原内容和 README 的 `created`；blob 只有一份 |
| 第二轮改 README、新建 `EXAMPLE.md`，撤销前手动改 README | 报冲突 `queue/README.md`，`EXAMPLE.md` 也没删，没有写 `rewound.json` |
| 去掉手动修改后撤销第二轮 | README 回到第一轮结束时，`EXAMPLE.md` 删除，`go.mod` 保持第一轮的改动；`continues` 指向第一轮 |
| 已撤销的轮次 | 再撤销、`-continues` 接着它都被拒绝 |
| 第三轮接着第一轮，派子 agent 新建 `SUB.md`，自己改 README | 子 agent 的清单写在第三轮的目录里 |
| 撤销第一轮 | 自动找到第三轮、跳过已撤销的第二轮；三处改动加子 agent 的文件全部恢复，`git status` 干净；审计两条记录 |
| 观测台两轮对话，撤销第二轮 | 别的对话的轮次返回 404；预览与执行结果一致；续聊的 `turn/started` 里 `continues` 是第一轮；模型回答“只新建了 NOTES.md”，与文件一致；页面显示撤销横幅、第二轮淡化 |

## 5. 当前没有覆盖的范围

- **bash 的改动**：沙箱里的命令能改工作区，那些改动没有快照。改到 agent 也改过的文件时，撤销会按冲突拒绝；只被 bash 改过的文件不会恢复，新建的也不会删除。
- **只退文件或只退对话**：现在只有“一起退”。只退对话已经可以用续聊接旧检查点做到（命令行），观测台没有单独的按钮。
- **保留期限**：快照随检查点一直保留，没有清理策略（和 `.data/` 其他运行数据一样，见 PROJECT 的“运行数据”待办）。
- **回放分叉**：D17 的回放分叉从请求快照启动新任务，如果开了工作区会改真实仓库；它和撤销互不知道对方。
- **文件权限与符号链接**：恢复只写回内容，不恢复权限位；agent 的文件工具不会写出符号链接，遇到时按冲突处理。
