# Day 36 项目实践：编码任务评测集

先读 [学习笔记](day-36-notes.md)。对应能力：H25 评测与迭代（编码任务评测）；相关的有 H20（完成判定：自评与校验分开）、H11（校验命令在沙箱里只读运行）。复查对照：[自用任务集](../day-22/day-22-lab.md#3-自用任务集) T1、T3、T5，[架构评估](../architecture-review.md) 第 5 节的 R1、R12。

## 改动一览

| 改动 | 代码 | 作用 |
| --- | --- | --- |
| 编码题格式 | [cases.go](../../internal/eval/cases.go)、[coding.go](../../internal/eval/coding.go) | 用例多一个 `repo`（`commit`：本仓库的提交；`fixture`：`eval/coding/` 下的小仓库；可选 `setup` 补丁、`checks` 检查脚本目录），`suite` 为 `coding`；新打分器 `command`（`run` + `label`） |
| 每个试次一份拷贝 | [coding.go](../../internal/eval/coding.go) 的 `prepareRepo`、[run.go](../../internal/eval/run.go) 的 `execute` | `.data/eval/work/<任务ID>/`：commit 用 `git clone --no-hardlinks` 再检出（不用 worktree：沙箱里要有完整的 `.git`），fixture 复制后 `git init`；提交一个“起点”，作为 `-workspace` 交给 agent 并打开 `-bash`；跑完存 diff、删拷贝 |
| 校验命令 | [main.go](../../main.go) 的 `-check`；[coding.go](../../internal/eval/coding.go) 的 `runChecks` | 本程序以 `-workspace <拷贝> -bash -check '<命令>'` 运行：不请求模型，在工作区只读挂载的沙箱里执行（和 `verify_feature` 一样），最后一行输出 JSON 的退出码与输出；题目的 `checks` 脚本先放进这次的 `/scratch` |
| 自评与校验 | [grade.go](../../internal/eval/grade.go)、[summary.go](../../internal/eval/summary.go)、[cli.go](../../internal/eval/cli.go)、观测台 [eval.js](../../internal/observer/web/eval.js) | 试次记 `claimed`（正常结束）、`commands`（校验结果，重新打分时沿用）、`diff`；报告多一行“自称完成 / 校验通过 / 误报 / 没收尾但通过” |
| 题目 | [coding-cases.jsonl](../../eval/coding-cases.jsonl)、[eval/coding/](../../eval/coding/) | 5 道题，见第 1 节 |

## 1. 题目

| 用例 | 起点 | 来源 | 校验命令检查什么 |
| --- | --- | --- | --- |
| `code-calc-pow` | 本仓库 `ee91d23` | 自用 T1 | gofmt、vet；calculator 的描述里写了乘方；`tools.go` 的改动只在描述行 |
| `code-mcp-env` | 本仓库 `ee91d23` | 自用 T3 | 编译出来的程序用 `-mcp-list` 连一个把环境变量写进文件的 stdio server：拿得到 `PATH`，拿不到 `ARK_API_KEY`、`DEEPSEEK_API_KEY` |
| `code-checkpoint-version` | 本仓库 `ee91d23` | 自用 T5 | 在临时目录放两个检查点：版本 999 的续跑报错且说明是版本问题；没有版本号的旧检查点不报版本错误 |
| `code-calc-subcommands` | `eval/coding/calc` | D31 的接力实验 | add、pow、max 的输出，错误参数的退出码 |
| `code-ring-hover` | `eval/coding/ring` | R1、R12：悬浮没效果、没验证就说好了 | [hover.mjs](../../eval/coding/checks/ring/hover.mjs) 在沙箱里启动无头 Chrome，用 DevTools 协议把鼠标真的移到 `#ctx-ring` 上，只看“看得见的文字”（元素与伪元素的 display、visibility、opacity 都算）：悬浮时有 18%、82%、151.2k、828k，移开消失，没有脚本错误 |

T2（metrics 首字延迟）、T4（按标注调度）、T6（观测台删除）没有收进来：前两者要真实运行数据或模型才能检查行为，T6 要起一个带登录的观测台。它们留给人工验收。

**题目自证**：每道题的校验命令先在起点上跑（必须失败），再在参考解上跑（必须通过）。参考解：T1 只改描述的一行；T3、T5 用当前工作区里已经修好的代码；两个小仓库各写了一份参考实现（没有提交，只用来验证）。悬浮题另加一个反例：只用原生 `title` 的写法——它在真实浏览器里也看不到浮层，校验判失败。8 次加 3 次结果都符合预期。

## 2. 跑一次

```bash
go run . eval -cases eval/coding-cases.jsonl -k 2 -workers 3 -- -provider deepseek -model deepseek-flash
```

每道题的 `args` 里给了预算（30 或 40 次请求）与窗口（262144）。试次在 `.data/eval/work/` 里，跑完就删；diff 在 `.data/eval/<评测ID>/<任务ID>.diff`。观测台的“评测与归因”页面能看同一份记录（发起新评测仍只用问答题集）。

## 3. 结果

DeepSeek 官方 `deepseek-flash`，每题 2 次（评测 `e20261022-032018`，当时本仓库的题预算是 40 次请求）：

| 用例 | 结果 | 请求数 | 说明 |
| --- | --- | --- | --- |
| `code-calc-pow` | 2/2 | 6、13 | 只改了描述行 |
| `code-mcp-env` | 0/2 | 40、40（预算用完） | 校验命令两次都通过 |
| `code-checkpoint-version` | 0/2 | 40、40（预算用完） | 校验命令两次都通过 |
| `code-calc-subcommands` | 2/2 | 8、11 | |
| `code-ring-hover` | 2/2 | 20、33 | 两次都做成了真正的浮层，悬浮检查通过 |

自评与校验：10 个试次里模型自称完成 6 次，6 次校验都通过，**没有误报**；另有 4 次没正常结束（预算用完）但校验通过。

逐个看这 4 次：代码在第 10–16 次请求就改好了，之后的请求全花在改文档上——README、PROJECT、wiki、各天的 lab。这是本仓库 AGENTS.md 的规则（“修改实现时同步修改 docs”），系统提示词里就有，模型照做了。所以这是预算定得不对，不是 agent 的毛病：本仓库的题按“代码加文档”估预算，改成 80 次请求（D37 的基线按 80 重跑）。

悬浮题在 10-19 的自用里是失败的（R1：原生 title 不弹、模型没法验证），这次两次都通过：D25 起沙箱里有 Node 和 Chrome，提示词要求改了网页要在无头浏览器里检查，模型照做了。


## 4. 已知边界

- 题目少（5 道），k=2：结论只能看大的差别，消融的分差要配合逐题看。
- “自称完成”按这一轮正常结束算，没区分“说做完了”和“说做不了”；答案里明确说没做成的，由人工备注区分。
- 编码题中途停下再续跑时，拷贝会从起点重建，检查点里的历史和磁盘对不上；这种试次应作废重跑。D17 的回放分叉也不支持编码题（拷贝已经删了）。
- 校验命令在沙箱里断网运行：要联网的检查做不了；Go 依赖都得在模块缓存里（本仓库的历史提交满足）。
