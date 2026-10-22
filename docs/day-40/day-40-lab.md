# Day 40 项目实践：只追加的会话记录

先读 [学习笔记](day-40-notes.md)。对应能力：H18 会话记录与恢复、H22 客户端协议（断线补读）。做的是 [架构评估](../architecture-review.md) 的 O9 的第一步：会话记录与检查点对上序号、观测台按它补读；“检查点完全由记录重放得出”没有做。

## 改动一览

| 改动 | 代码 | 作用 |
| --- | --- | --- |
| 会话记录 | [journal.go](../../internal/agent/journal.go) | `.data/[tenants/<租户>/]sessions/<对话第一轮的检查点ID>.jsonl`：每行 `seq`（对话内序号，各轮接着编）、`turn`（检查点ID）、`aseq`（协议序号，观测台的可以脱离的连接上才有）、`time`、`method`、`params`；从 `turn/started` 起写，命令行、队列、评测里没有界面也写；子 agent 的消息经 `subagent/message` 记在父对话里 |
| 写入点 | [protocol.go](../../internal/protocol/protocol.go) 的 `send`、`Notify`、`Journal` | agent 发出的通知与请求在发给界面的同时、同一把锁下交给 `Journal`；item/delta、`log` 行与响应不记 |
| 快照位置 | [checkpoint.go](../../internal/agent/checkpoint.go) 的 `LogSeq` | 检查点记下写这份快照时会话记录到了第几条 |
| 补读 | [live.go](../../internal/observer/live.go) 的 `recoverJournal` | 观测台启动接管时，agent 进程已经不在：从会话记录里取这一轮 `aseq` 大于存档里最大值的事件补进存档（都记成通知，审批已经没人能答），退出状态从 `process/exit` 取 |

## 1. 一条记录

```json
{"seq":1,"turn":"7bdf230a-…","aseq":48,"time":"2026-10-22T03:41:23Z","method":"turn/started","params":{"continues":"","input_capacity":257024,"model":"doubao-seed-2.1-pro",…}}
```

## 2. 验证

格式化、`go vet`、`go build` 通过。方舟 `doubao-seed-2.1-pro`：

| 场景 | 结果 |
| --- | --- |
| 观测台里让 agent `sleep 6` 的 bash；bash 开始后冻结（SIGSTOP）观测台，12 秒后 `kill -9` agent 和观测台，再重启观测台 | 存档停在协议第 64 条（bash 开始）；新观测台发现 agent 已经不在，从会话记录补进 2 条：bash 的 `item/completed`（含结果）和下一次模型调用的 `item/started`，记一行“从会话记录 … 补齐 2 条协议消息”，再记 exit“agent 在观测台停止期间结束……以检查点为准”；live 文件清掉 |
| 同一次的会话记录与检查点 | 记录 8 条；检查点版本 2、`log_seq` 7：检查点在这批工具结果写回时保存，第 8 条（下一次模型调用开始）在它之后 |
| 终端客户端两轮 | 日志 `Journal: … from_seq=1`、`from_seq=7`；第一轮 1–6 条，第二轮 7–12 条，同一个文件接着编 |

另一种时机（agent 在 `process/exit` 之前被杀、观测台冻结在最终回答之后）没有构造成功：方舟回答很快，冻结总晚一步；逻辑和上面同一条路径。

## 3. 已知边界

- 只是 O9 的第一步：检查点仍是完整快照，不是“从记录重放得出”；终端日志行不在记录里，补读后看不到那段终端输出。
- 同一段对话被两个进程同时接着聊（命令行上分叉）时序号可能重复，按 `turn` 与时间区分。
- 记录随事件数增长，没有轮转与保留期限（同 PROJECT“运行数据”）。
- 名字上要和 D20 的“结果日志”（`.data/checkpoints/<任务ID>.calls/`，每项工具调用的结局）区分：那是续跑判断“做没做过”用的，这是给界面和人看过程用的。
