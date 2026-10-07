# Day 2 项目实践：给循环加上预算、重试与熔断

原理见 [Day 2 学习笔记](day-02-notes.md)。本文记录本项目的规则、代码位置、故障实验与实现范围。启动与模型配置见根目录 [README](../../README.md)。

## 1. 本项目的规则

| 护栏 | 参数与默认值 | 说明 |
| --- | --- | --- |
| 请求预算 | `-max-steps 10` | 统计模型请求；Day 3 起摘要与超窗重试也计入。耗尽后返回未完成与本地生成的步骤摘要，退出码 1 |
| 工具重试 | `-retries 2`，范围 0–5 | 首次执行后最多再试 2 次，等待 `200ms × 2^r`（200ms、400ms）；200ms 是便于观察的起点，不是生产标准；当前没有加抖动 |
| 重复熔断 | 固定阈值 3 | 第三次连续相同的工具与参数出现时，执行前拦下整批，退出码 1 |
| 模型请求超时 | 5 分钟 | 单次 HTTP 请求；尚未设置任务总时限与单个工具超时 |

## 2. 代码位置

| 位置 | 看什么 |
| --- | --- |
| [react.go](../../internal/agent/react.go) 的 `agent.Run` | 循环上限来自 maxSteps；执行前按调用顺序检查重复动作 |
| react.go 的 `runWithRetry` | 保留同一调用 ID，增加 attempts；退避用 timer + context，Ctrl+C 可中断等待 |
| react.go 的 `actionKey` | 工具名 + 规范化参数；忽略调用 ID、JSON 空白与键顺序 |
| react.go 的 `stopRun` | 打印 `Termination:` 原因，返回未完成与已执行步骤摘要，不再调用模型 |
| [dispatch.go](../../internal/agent/dispatch.go) 的 `runTool` | 在边界用 recover 把 panic 转为 error，进入重试与回填流程 |

## 3. 故障实验

实验工具需显式加 `-lab-tools`：`always_fail` 总是失败，`check_task_status` 总是返回 pending。普通任务不加载它们。

**正常参照**

```sh
go run . -question '请用 calculator 计算 floor(sqrt(1234*5678))，并查询当前日期和星期，两项独立请同轮调用。'
```

两个调用同轮提出，成功时不进入重试等待。

**一直失败的工具**

```sh
go run . -lab-tools -retries 2 -question '请调用 always_fail 完成一次故障操作。如果工具反馈失败，请说明任务未完成和错误原因，然后结束。'
```

观察首次执行加两次重试（等待 200ms、400ms）后，错误成为 tool 消息，由下一轮模型说明失败。三次执行不算三次模型请求。

**原地打转**

```sh
go run . -lab-tools -question '请持续使用 check_task_status 查询 task_id 为 day2-loop 的任务。每轮只查询一次；状态为 pending 就在下一轮用相同参数继续查询，直到状态变成 completed 才结束。'
```

第三次相同请求出现时被拦截，步骤摘要只包含实际执行过的两次。

**预算耗尽但有进度**

```sh
go run . -max-steps 1 -question '请先用 search_notes 查询循环职责，再用 calculator 计算职责数量乘以25。'
```

查询完成后以 max_steps 停止，不继续计算，也不额外请求模型写总结。

## 4. 实现范围

已实现：参数与协议的基础校验、请求预算、统一的工具重试与指数退避、错误回填、重复动作熔断、未完成摘要。模型 API 错误不重试，直接停止。

未实现（笔记中的知识扩展）：按错误类型分类重试、抖动、单个工具超时与任务总时限、业务幂等键、执行进度持久化与恢复、部分成功的分项处理。未来最小的演进方向是：`runTool` 返回少量可识别的错误类别，`runWithRetry` 据此决定是否原样重试，`agent.Run` 的结果回填保持不变。
