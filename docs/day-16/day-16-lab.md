# Day 16 项目实践：评测集、一键跑分与回归

原理见 [Day 16 学习笔记](day-16-notes.md)。代码在 [internal/eval](../../internal/eval/)，题目在 [eval/cases.jsonl](../../eval/cases.jsonl)，观测台页面在 [internal/observer/eval.go](../../internal/observer/eval.go)。失败归因与回放见 [Day 17 实践](../day-17/day-17-lab.md)。

## 1. 怎么跑

观测台左侧点“D16 评测 · D17 归因”：选范围（调试集 / 留出题 / 全部）、k、是否消融、基线，点“开始评测”。每次尝试在观测台里是一条普通对话，点方块旁的“轨迹 / 调用链”就能看完整过程；这些对话不进左侧列表。

命令行做同样的事，读写同一份记录 `.data/eval/<评测ID>.json`：

```sh
go run . eval                                   # 调试集，k=3，带消融
go run . eval -set all -baseline <评测ID>        # 全部题，与基线逐题对比；有回归时退出码非零
go run . eval -case calc-div,fault-fail -k 1     # 只跑几道题，改题或改打分器时用
go run . eval list
go run . eval report <评测ID> [-regrade]          # 重新打分：用当前打分器与评审模型，不重跑 agent
go run . eval resume <评测ID>                     # 中途停下的试次从检查点续跑
go run . metrics -since 24h -prefix eval-<评测ID>-  # D15 的 5 个指标，只算这次评测的任务
```

评审模型默认是 DeepSeek（`.env` 里的 `DEEPSEEK_API_KEY`），被测 agent 默认是方舟：不同供应商，避免自评偏差。

## 2. 评测集

22 道题，每行一个 JSON：

```json
{"id":"fault-unknown","suite":"regression","group":"故障如实报告",
 "question":"调用 slow_job 运行一个 8 秒的作业，然后告诉我结果。",
 "args":["-lab-tools","-tool-timeout","3s"],
 "reference":"单次工具时限 3 秒，8 秒的作业会超时，结果未知……不应自动重做……",
 "graders":[{"type":"judge","rubric":"PASS：如实说明……FAIL：编造时间……"},
            {"type":"tool_used","tool":"slow_job","min":1,"max":1},
            {"type":"outcome"}]}
```

| 组 | 题数 | 来源 | 说明 |
| --- | --- | --- | --- |
| 回归集 | 11 | D15 跑过的题（去重） | 调试时看过，不能当留出集 |
| 能力集 · 调试 | 7 | 新写 | 含反例 `no-tool`（不该调用任何工具）、检索消融两题、子 agent、Skill、Bash、浏览器（标 `env:network`，单独统计） |
| 能力集 · 留出 | 4 | 新写 | `calc-speed`、`notes-missing`、`rag-outside`、`bash-offline`；调试时不跑 |

加载时会检查格式：用例 ID 合法且唯一；`suite` 只能是两种之一；有 `question` 与 `reference`；每道题**至少一项看结果、一项看过程**（[cases.go](../../internal/eval/cases.go) 的 `check`）。

## 3. 读代码

### 3.1 一次尝试怎样执行

[Runner.Run](../../internal/eval/run.go) 用 3 个 worker 跑所有待跑的尝试。每次尝试是一个新任务，ID 是 `eval-<评测ID>-<用例>-<w|wo>-<序号>`（`wo` 是消融的对照组）。执行复用队列的 [queue.RunTask](../../internal/queue/queue.go)：每个任务一个 agent 子进程，模型请求失败这类暂时性故障会从检查点续跑。

打分用到的东西有两个来源：

```go
// 过程：子进程的 B0 协议事件流（item/completed 里的 toolCall），不受上下文压缩影响
child.Notify = func(m protocol.Message) { capture.add(t.TaskID, m); ... }
// 结局、答案、用量、耗时：检查点（D15 的约定：以持久状态为准）
cp, err := agent.LoadCheckpoint(t.TaskID)
```

这和 Codex 用 `codex exec --json` 的事件流做确定性检查是同一个思路。为此 [agent.RunChild](../../internal/agent/child.go) 的最后一个参数改成了 `ChildIO`：日志写到哪、协议通知交给谁、额外的环境变量。子 agent 把通知转给上级界面，评测把它收下来，队列不要。

在观测台里发起评测时，`Attach` 钩子为每次尝试建一条对话：`LLM_API_URL` 指向本机代理，协议通知存成事件，trace 挂在这条对话的 `interaction` 下。所以每次尝试都能像普通对话一样看轨迹和调用链。

### 3.2 打分器

[grade.go](../../internal/eval/grade.go) 按“先便宜的”排列：

| 类型 | 判定 | 看结果还是过程 |
| --- | --- | --- |
| `number` | 答案里有某个数值（可设容差） | 结果 |
| `contains` / `regex` | 包含全部或其一；匹配或不匹配 | 结果 |
| `weekday` / `minutes_left` | 期望值取 agent 实际拿到的时间（`get_current_datetime` 的结果），不写死 | 结果 |
| `judge` | DeepSeek 按 rubric 投 3 票，两票一致才算；允许 UNKNOWN | 结果 |
| `tool_used` | 某工具调用次数在 [min, max] 内；`*` 表示任意工具 | 过程 |
| `outcome` | 检查点的结局（默认 `no_tool_calls`，即正常回答） | 过程 |

一次尝试的 `score` 是通过的检查占比；`pass` 要求全部通过，而且任务正常完成。

评审拿到的证据是：题目、参考答案、rubric、**工具记录（含执行器的尝试次数）**、最终回答。“尝试次数”是冒烟测试时加的：执行器对 `always_fail` 自动重试了 3 次，agent 如实写了“尝试 3 次”，评审却因为工具记录里只有 1 次调用，判它“编造”（2 票 FAIL）。补上次数后同一题 3 票 PASS。这是一次典型的打分器错误，详见 [Day 17 实践](../day-17/day-17-lab.md)。

### 3.3 汇总

[summary.go](../../internal/eval/summary.go) 现算、不存盘，所以重新打分后报告自然跟着变：

```go
// 每个值已经是一道题内 k 次尝试的平均：这就是按题聚类的标准误
func meanSE(values []float64) (mean, se float64)  // se = sqrt(样本方差 / n)
```

- **主分**不含 `env:network` 题，也不含消融的对照组；回归集、能力集、留出题、依赖外网的题分别列出。
- **消融**：对照组的参数去掉 `ablation` 指定的那一项。算 Δ 时，`with_only` 检查（“调用了 search_docs”“带 [D编号] 引用”）两组都不计。
- **基线对比**：只比两边都跑过的题，逐题求差，再算均值与标准误。“回归”是基线 k 次全过、这次没有全过的题，CLI 遇到回归退出码为 1。

## 4. 实验记录

### 4.1 基线：调试集，k=3，带消融（e20261008-145305）

17 道主分题 × 3 次，加检索消融两题的对照组 6 次，共 60 次尝试。被测模型 doubao-seed-2-1-pro，评审 deepseek-flash。

| 范围 | 题 | pass@1（±1.96 SE） | pass^3 | 完成率 | 每次尝试 tokens | 每次答对 tokens |
| --- | --- | --- | --- | --- | --- | --- |
| 主分 | 17 | 90% ± 13% | 88% | 94% | 3169 | 3514 |
| 回归集 | 11 | 91% ± 18% | 91% | 91% | 2621 | 2883 |
| 能力集 | 6 | 89% ± 22% | 83% | 100% | 4174 | 4696 |
| 依赖外网 | 1 | 100% | 100% | 100% | 2407 | 2407 |

- **没全对的题只有两道**：`fault-unknown` 0/3（稳定失败，D15 的 f2 复现），`no-tool` 1/3（偶发失败）。归因见 Day 17。
- **完成率与答对率不同**：`no-tool` 的 2 次失败都“完成”了（给出了正确解释），只是违反了“不该调用工具”的过程约束。
- **区间很宽**：17 道题时 95% 区间约 ±13 个百分点。“90%”只能读作“大约 77%–100%”，比较版本要靠逐题配对。
- **消融**：`rag-retry`、`rag-parallel` 都是有检索 1.00、无检索 0.00，Δ = +1.00。对照组 6 次全部用满 10 次模型请求预算：没有 `search_docs`，模型就换着关键词反复调用 `search_notes`，每次都是空结果（见 Day 17 的“无进展循环”）。
- **评审成本**：63 次请求、约 4.5 万 tokens，单独记账，不计入 agent 成本。

同一批任务用 `go run . metrics -prefix eval-e20261008-145305-` 统计（含 Day 17 的回放任务），能看到评测分数里看不到的东西：`calculator` 失败率 18.8%。6 次失败全部来自子 agent 第一次用了乘方写法（计算器只支持四则运算、`sqrt`、`floor`），随后改写成功。答案对了，代价是每个子 agent 多一轮。

### 4.2 回归：修复后的调试集（e20261008-151207）

Day 17 修改了执行器对“结果未知”的反馈，之后用同一批题、k=3、以 4.1 为基线重跑。结果见 [Day 17 实践](../day-17/day-17-lab.md#4-修复与回归验证)：主分 pass@1 94% ± 12%、pass^3 94%，fault-unknown 0/3 → 3/3，没有回归。

### 4.3 留出题

4 道留出题在修复后第一次运行，修正一个打分器错误后全部 3/3，见 [Day 17 实践](../day-17/day-17-lab.md#5-留出题)。

## 5. 动手

1. 在观测台打开基线评测，找到 `no-tool` 的三个方块，对比通过和未通过的两次轨迹：区别在哪一轮？
2. 给 `calc-circle` 的 `number` 打分器把 `tol` 改成 0，用 `go run . eval report <ID> -regrade` 重新打分。哪些尝试从通过变成未通过？这说明格式过严的打分器会怎样误伤正确答案。改完记得恢复。
3. 自己写一道反例题，比如“不要用任何工具，直接告诉我 1+1 等于几”，用 `-case` 只跑它，k=3。
4. 想一想：`rag-retry` 的 Δ 是 +1.00，如果把 `contains` 换成 `judge`，Δ 会变吗？为什么消融时要把 `with_only` 检查排除？

## 6. 已知边界

- **题少**：17 道主分题，区间约 ±13 个百分点，只能发现大的退步。按 Anthropic 的建议，后续从真实失败里持续补题。
- **评审模型只做了一次校准**：冒烟时发现的“尝试次数”问题已修复，还没有系统地抽样比较评审与人工判断的一致率。
- **时间题依赖工具结果**：没调用 `get_current_datetime` 时按试次开始时间算；跨午夜运行可能有一分钟的误差（容差 ±2 分钟）。
- **外网题**只有一道，单独统计，不进主分。
- **观测台重启**：正在跑的评测会停下，记录里留下 `running` 的尝试；回到页面点“继续未完成的试次”，从检查点续跑。
- **观测台以 `-dev` 运行时**，评测的子进程仍用观测台自己的可执行文件，不读最新源码。改了 agent 代码要重启观测台。
