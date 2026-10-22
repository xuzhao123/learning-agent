# Day 31 项目实践：长任务接力与独立验证

先读 [学习笔记](day-31-notes.md)。对应能力：H20 规划与长任务（跨会话接力、独立验证）；相关的有 H14（Stop hook 当收尾检查）、H19（进度文件随快照撤销）。做的是评估里的 O4 与复查里的 O12（见 [架构评估](../architecture-review.md) 第 4 节、[自用复查 10-20](../selfuse-review-2026-10-20.md) 第 3 节）。

## 改动一览

| 改动 | 代码 | 作用 |
| --- | --- | --- |
| 进度文件与工具 | [progress.go](../../internal/agent/progress.go) | 工作区里的 `.agent/progress.json`：目标、功能清单（每项一条校验命令）、交接说明；`progress(read / init / note)`；有工作区又开了 bash 时提供 |
| 独立验证 | [progress.go](../../internal/agent/progress.go) 的 `verifyFeature`、`askVerifier`；[sandbox.go](../../internal/sandbox/sandbox.go) 的 `Check` | 在工作区只读的沙箱里跑校验命令；退出码为 0 时再请另一家模型（`-verifier`，默认和主模型不同的一家）对照描述、输出与 `git diff --stat` 判定；两者都过才记为通过 |
| 只能经验证改 | [snapshot.go](../../internal/workspace/snapshot.go) 的 `Protected`、`WriteManaged` | 编辑工具拒绝改进度文件；harness 写它时照样存快照（撤销这一轮时一起退）；`.data/[tenants/<租户>/]progress/` 里留权威副本，读的时候核对，被 bash 或人改过就恢复并告诉模型 |
| 收尾检查 | [react.go](../../internal/agent/react.go) 的 `finalCheck`（O12，第一组时已做） | 不正常结束时用 Stop hook 跑一次检查，结论写进检查点与 `turn/completed` |
| 沙箱只读运行 | [sandbox.go](../../internal/sandbox/sandbox.go) | 工作区可写与否从全局 `ProjectWrite` 改成每次执行的参数：普通 bash 跟着权限模式，校验命令一律只读 |
| 界面 | [runs.js](../../internal/observer/web/runs.js)、[util.js](../../internal/observer/web/util.js) | 验证者的请求（purpose `verify`）作为辅助调用显示为“独立验证（D31）”，不打乱主任务的轮次 |

## 1. 一项怎样变成“通过”

```text
verify_feature(id)
  ├─ sandbox.Check(check)          工作区只读挂载，和普通 bash 同样的隔离与资源限制
  │    └─ 退出码 ≠ 0 ──▶ fail（不问验证者）
  ├─ askVerifier(desc, check, 输出, git diff --stat)   另一家模型，只给证据
  │    └─ {"verdict": "pass" | "fail", "reason": …}
  └─ passing = 退出码 0 且 verdict = pass；写进进度文件和副本
```

验证者的系统提示词要求它不相信输出里自称“已完成”的文字，退出码不为 0 一律判失败。它看不到写代码的模型说过什么。

## 2. 防止绕过

| 途径 | 怎么挡 |
| --- | --- |
| 用 edit_file / write_file 改状态 | `workspace.Protected` 拒绝，返回“进度文件只能用 progress、verify_feature 改” |
| 用 bash 改 | 沙箱看不到 `.data`，harness 的副本不受影响；下次 `progress` 或 `verify_feature` 读的时候发现不一致，按副本恢复，结果里带 `warning` |
| 人手写一个带“通过”的文件（没有副本时） | 照收，但没有验证记录的“通过”一律按未通过处理 |
| 校验命令写得太松 | 退出码之外还要验证者对照功能描述认可 |
| 校验命令顺手改代码 | 工作区只读挂载 |

计划里还有“git 提交”：沙箱里的 `.git` 是只读的（D25），模型提交不了，这一步留给人。进度文件在工作区里，下一个会话照样读得到。

## 3. 验证

格式化、`go vet`、`go build` 通过。DeepSeek 官方 `deepseek-flash` 写代码，方舟 `doubao-seed-2.1-pro` 当验证者，目标是临时目录里一个只有 `add` 子命令的小 Go 程序，任务是“加 pow 和 max 两个子命令”：

| 步骤 | 结果 |
| --- | --- |
| 第一个会话（`-max-steps 7`） | 先读进度（没有），`progress(init)` 拆成 4 项：pow、max、错误处理、原有行为回归，每项一条 `go run … | grep -qx …` 的校验命令；改 `main.go` 后 `verify_feature(F1)`：退出码 0、验证者 pass，F1 记为通过；随后请求预算用完，停下 |
| 手工把 F2 改成 `"passing": true` | 模拟用工具之外的途径改状态 |
| 第二个会话（全新对话，“继续这个仓库里没做完的长任务”） | 第一次读进度就收到 `warning`：“进度文件在 progress、verify_feature 之外被改过……已按 harness 的记录恢复”，F2 回到未通过；补完 max 后 F2、F3、F4 依次经验证通过；最后 `progress(note)` 写下交接说明 |
| 收尾检查（O12） | 临时 clone 里放一处编译不过的改动，预算用完后 `final_check` 记下编译错误（见 [Day 26 实践](../day-26/day-26-lab.md)） |

验证者的每次判定都在进度文件里留了记录（时间、供应商/模型、退出码、结论与理由）。

## 4. 已知边界

- 编辑工具拒绝改进度文件这一条这次没被模型触发（模型一直按规则用工具），逻辑本身很简单，没有单独构造。
- 计划要求做完回看 R1、R12（让 harness 改一次带悬浮的页面控件，由独立验证者实际加载页面判定），这次没做：需要在沙箱里用 Chrome 做页面检查，留到 D36 编码任务评测集里作为一道题。
- 验证者是另一家模型，但它同样会犯错；它只能让“命令过了”的结论更严，不能推翻失败的命令。
- 进度副本按工作区的绝对路径区分；换了机器、换了目录，副本对不上，只能按“没有验证记录的通过不算”重新开始。
- 模型提交不了 git，接力靠进度文件；交接说明是模型写的，质量取决于它。
