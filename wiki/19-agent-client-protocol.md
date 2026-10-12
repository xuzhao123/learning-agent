# 19 · Agent 与界面的协议：过程怎样送到界面，人的决定怎样送回 Agent

前面各篇讲 Agent 内部怎样运行。本篇讲它和外界的那条线：命令行、网页、IDE 插件、父 Agent 怎样实时知道它在做什么，怎样在执行到一半时让人拍板，又怎样让它停下。

这条线也是第 17 篇“确认只能由人回答”的技术基础：确认请求走的是这条协议通道，模型和工具结果写不到这里。

[阅读目录](README.md) · [技术展开](#技术展开) · [基础自测](#自测)

## 先认识术语

| 术语 | 通俗解释 |
| --- | --- |
| 界面（client） | 展示 Agent 过程、接收人操作的一方：网页、命令行、IDE，或父 Agent |
| JSON-RPC 2.0 | 一种很小的消息格式：请求、响应、通知三种 |
| 请求 / 响应 | 带编号、需要对方回复的消息 / 用同一编号回复 |
| 通知 | 没有编号，只告诉对方一件事，不需要回复 |
| JSON Lines | 一行一条 JSON 消息的传输方式 |
| Thread / Turn / Item | 一段对话 / 用户的一次提问 / 一轮里的一个工作单元（一次模型调用、一次工具调用） |
| 反向请求 | Agent 主动向界面发请求并等待回复，如审批 |
| 兼容层 | 把协议上线前的旧记录换算成同样事件的代码，只对旧数据使用 |

## 为什么不让界面去读日志

最省事的做法是界面读 Agent 的终端输出，用正则匹配“开始调用工具”“任务ID是多少”。问题很快出现：

- **日志是给人看的**：措辞一改、多一个空格，界面就解析不出来；谁都不敢改日志。
- **只能单向**：Agent 想在执行中途问“允许访问这个网站吗”，日志做不到，只能先结束这一轮，等用户下一轮回答。
- **嵌套更乱**：子 Agent 的输出被父 Agent 转发时只能加前缀，界面再拆一次。

所以在 Agent 核心和界面之间定义一个**结构化、双向**的协议。OpenAI Codex 的 app-server 就是这样：命令行、编辑器插件、网页版共用同一个 Agent 核心，经同一个协议通信。

## 三种消息，一行一条

```text
界面 → Agent   {"jsonrpc":"2.0","id":1,"method":"turn/start","params":{"question":"上海出差补贴多少？"}}
Agent → 界面   {"jsonrpc":"2.0","id":1,"result":{}}
Agent → 界面   {"jsonrpc":"2.0","method":"item/started","params":{"item":{"id":"call_1","type":"toolCall"}}}
Agent → 界面   {"jsonrpc":"2.0","id":7,"method":"tool/requestApproval","params":{"tool":"open_page",…}}
界面 → Agent   {"jsonrpc":"2.0","id":7,"result":{"decision":"deny"}}
```

- **双方都能发请求**。编号只在发起方自己的空间里唯一，回复带回同一个编号。
- **一行一条**：读一行就是一条完整消息，管道、socket 都能传。
- **协议和日志分开**：stdout 只放协议，给人看的日志放 stderr。混在一起，一行日志就能破坏解析。
- **不认识的请求回错误，不认识的通知忽略**：旧界面遇到新通知不会出错，新旧版本可以共存。

## 会话分三层

```text
Thread（一段对话）：可以保存、恢复、续聊
 └ Turn（一次提问）：到 Agent 给出回答或停下为止，结局明确
    ├ Item：用户消息
    ├ Item：一次模型调用   started → completed（带用量）
    ├ Item：一次工具调用   started → completed（带结果或错误）
    └ Item：一次上下文压缩 started → completed
```

- **为什么 Item 有开始和完成**：只发完成，慢工具执行的几十秒里界面什么都不知道。开始事件让界面立刻显示“正在执行”。
- **Item 用稳定的 ID**：工具调用的 ID 和模型协议里的 `tool_call_id` 一致，界面才能把“模型请求了什么”和“工具做了什么”对上。
- **一轮的结局要明确**：完成、被中断、预算耗尽、熔断……写在 `turn/completed` 里，界面不用从最后几行日志猜。

## 反向请求：执行到一半问人

协议双向以后，Agent 可以暂停、向界面发请求、等回复再继续：

```text
Agent：读过私有数据后要打开 x.com/?d=…
  → 发请求 tool/requestApproval，在这里等待
界面：显示具体参数和“允许 / 拒绝”
  → 人点击 → 回复 {"decision":"deny"}
Agent：不执行，把“用户拒绝”回填给模型
```

设计要点：

- **谁能回答**：回复只能来自界面连接。模型只能“让程序发起请求”，不能“批准请求”。
- **等多久**：人可能不在。等待有上限，到时按“暂未答复”处理，不无限期卡住。
- **没人能回答**：命令行、后台队列里没有可以点按钮的人，如实回答“没有可确认的界面”，不假装批准。
- **中断**：等待期间用户按了停止，等待随之取消。

停止也是一个请求（`turn/interrupt`）：和 Ctrl+C 走同一条“先收尾、写好检查点、再退出”的路，而且能拿到“已收到”的确认；拿不到再退回发信号。

## 嵌套：父 Agent 是子 Agent 的界面

```text
网页 ⇄ 观测台 ⇄ 父 Agent ⇄ 子 Agent
```

- **事件向上转发**：子 Agent 的通知包一层（带子任务ID）交给上级，界面按同样的逻辑还原子 Agent 的状态。
- **请求向上转发，回复向下传回**：子 Agent 要审批，父 Agent 转给自己的界面，拿到回复再交回。链上任何一层没有界面，就由这一层如实回答“没人能批准”。

## 为什么不用 MCP

MCP 定义的是“模型调用外部工具”：一问一答。Agent 与界面之间需要的是长时间的会话、流式进度、执行中的审批、会话恢复，这些 MCP 装不下。两者不冲突：Agent **对外调用工具**用 MCP，**对界面汇报和接受控制**用自己的会话协议。

## 技术展开

### 进阶术语：等待表、背压、断线语义与版本

| 术语 | 含义 |
| --- | --- |
| 等待表（pending） | 发起方记录“编号 → 等待者”的表，收到响应时据此交还 |
| 读循环 | 唯一读取连接的 goroutine，按消息类型分发 |
| 背压 | 消费方处理不过来时，生产方被迫变慢或丢弃 |
| SIGPIPE | 向已关闭的管道写时进程收到的信号，默认会杀死进程 |
| 断线语义 | 连接断开时，正在进行的工作算中断、继续还是待接管 |
| 向后兼容 | 新增字段与新通知不影响旧界面；改字段含义需要版本或新方法名 |

### 一条连接，双向收发

两端共用同一个实现。读循环按行解析：**带编号、不带方法名的是响应**，交给等待中的调用；其余（请求和通知）交给处理函数。

```go
func (c *Conn) Call(ctx context.Context, method string, params, result any) error {
    c.mu.Lock()
    c.next++
    id := c.next
    ch := make(chan Message, 1) // 带缓冲：响应先到、调用方已放弃时，读循环不会卡住
    c.pending[id] = ch
    c.mu.Unlock()
    if err := c.write(Message{ID: &id, Method: method, Params: raw(params)}); err != nil {
        return err
    }
    select {
    case m := <-ch:
        return decode(m, result)
    case <-ctx.Done(): // 超时或任务被停止：从等待表里删掉
        c.mu.Lock(); delete(c.pending, id); c.mu.Unlock()
        return ctx.Err()
    case <-c.closed: // 对端断开
        return errClosed
    }
}
```

几个细节：

- **写要加锁**：多个 goroutine（并行工具、子 Agent 转发）同时发消息，一行没写完另一行插进来就坏了。
- **处理函数不能阻塞**：它跑在读循环里；阻塞了，后面的响应都读不到，等待中的 `Call` 全部卡死。需要等人的请求，要把编号记下来，之后在别的 goroutine 里回复。
- **单行上限**：读缓冲要有上限（例如 16 MB），超长的一行按错误处理，不能无限申请内存。

### Agent 一侧：stdout 归协议

```text
启动时（打印任何东西之前）
  协议写到原来的 stdout
  os.Stdout 换成 stderr      → 之后所有 fmt.Printf 都成了日志
  忽略 SIGPIPE               → 界面先退出时写失败，进程照常收尾，不被信号杀掉
  等 turn/start              → 问题与续聊上下文都在参数里
界面断开 = 中断：走和 Ctrl+C 相同的收尾
检查点保存启动参数时去掉 -app-server：之后在命令行续跑，不该变成协议模式
```

没有界面（命令行直接运行）时，全局连接为空，发通知的函数什么都不做，各处代码不需要判断模式；发请求则返回“没有界面”，调用方据此按拒绝处理。

### 确认请求的完整链路

```text
子 Agent         父 Agent              观测台                       人
 risk() 命中
 写审计 approval_request
 Call tool/requestApproval ─▶ relayRequest
                              加 task_id
                              Call ─────────▶ handle()
                                               记 toolWaits[call_id]
                                               （不阻塞读循环）
                                                                    ◀─ 页面显示参数
                                               decideTool ◀──────── 点“拒绝”
                                               runOf：核对租户
                                               取出并删除等待项
                                               写审计 user_decision
                                               Reply(rpc_id) ──┐
                              ◀────────────────────────────────┘
                              Reply ─▶
 写审计 approval_decision
 回填 denied
```

要点：

- **一次答复只用一次**：观测台取出等待项就删掉，重复点击、过期的点击返回冲突，不会把旧的“允许”用在新调用上。
- **先审计再回复**：观测台的审计写不进去，就把等待项放回去并报错，不把“允许”交给 Agent。
- **租户核对在答复入口**：别的租户连这条对话都找不到，自然答不了它的确认。
- **转发时补上子任务ID**：界面要知道是哪个子任务在请求，审计也记在子任务名下。
- **任何一层失败都向下回答“unavailable”**：父 Agent 自己没有界面、或向上转发出错，就这样回答子 Agent，子 Agent 按拒绝处理。

### 断线、重连与兼容

| 情况 | 现在的处理 | 更强的做法 |
| --- | --- | --- |
| 界面进程退出 | Agent 读到 EOF，按中断收尾，写好检查点 | 独立于界面存活的守护进程，界面重启后重新接管 |
| 界面重启后想看历史 | 读观测台保存的事件记录 | 事件先落盘、带序号，界面按序号补读 |
| 协议上线前的旧存档 | 兼容层把旧日志行换算成同样的通知，只对没有协议标记的存档使用 | 迁移后删除兼容层 |
| 字段变更 | 新增字段、新通知向后兼容 | 改含义时用新方法名或版本号 |

### 对照已有实现

| 机制 | 源码 | 实践 |
| --- | --- | --- |
| 连接、等待表、通知与请求 | [protocol.go](../internal/protocol/protocol.go) | [B0 实践](../docs/bonus-protocol/protocol-lab.md) |
| `-app-server`、stdout/stderr 分流、`turn/start`、`turn/interrupt` | [main.go](../main.go) | 同上 |
| 子 Agent 的事件与请求转发 | [RunChild、relayRequest](../internal/agent/child.go) | 同上 |
| 界面一侧：保存通知、记下等待项、答复 | [server.go](../internal/observer/server.go) 的 `handle`、`decideNetwork`、`decideTool`；页面 [index.html](../internal/observer/index.html) 的 `applyNotify` | 同上、[Day 20 实践](../docs/day-20/day-20-lab.md) |
| 联网审批与高风险确认 | [sandbox.go](../internal/sandbox/sandbox.go)、[approval.go](../internal/agent/approval.go) | [Day 20 实践](../docs/day-20/day-20-lab.md) |

评测与队列也用这套协议启动子进程，但上面没有界面：通知不转发，审批如实回答 unavailable。

当前边界：模型请求是非流式的，Item 只有开始和完成，没有增量；观测台重启后不能接管正在运行的 Agent；给人看的日志仍是文本；等待表按调用 ID 区分，依赖模型供应商给出的调用 ID 在一个对话里不重复。

### 进阶推演

1. 处理函数里直接等人点按钮，会发生什么？  
   读循环被卡住，之后的响应都读不到；Agent 侧其他正在等待的 `Call`（比如另一个子任务的请求）全部卡死。应记下编号、立即返回，人点击后再回复。

2. 父 Agent 没有界面（在队列里运行），子 Agent 发来确认请求，应该怎样回答？为什么不直接回答“deny”？  
   回答 unavailable。它如实说明“没人能确认”，与“人拒绝了”是不同的事实；审计和回填给模型的说明都不同，用户事后能区分。两者都不执行。

3. Agent 一边并行执行三个工具、一边转发子 Agent 的通知，为什么写协议必须加锁？  
   多个 goroutine 同时写同一个 stdout，一条消息可能写到一半被另一条插入，变成两行坏 JSON。

4. 界面在 Agent 等确认时崩溃了，Agent 会怎样？  
   连接关闭，等待中的 `Call` 返回错误，按 unavailable 处理、不执行；读到 EOF 后整轮按中断收尾，检查点保留，之后可以续跑。

## 自测

1. 协议消息和日志为什么必须分到不同的通道？  
   混在一起时任何一行日志都可能破坏解析；分开后日志可以随意改，不影响界面。

2. 双方都能发请求时，响应怎么找到对应的请求？  
   发起方给请求编号并登记等待者，回复带回同一编号，读循环据此交还。

3. Item 只发完成事件有什么问题？  
   慢操作执行期间界面什么都不知道，也无法展示“正在等待确认”这类中间状态。

4. 为什么审批请求不能让模型回答？  
   模型可能被读到的内容诱导；回复只能来自界面连接上的人，模型的输出写不到这条通道上。

5. 为什么 Agent 与界面的通信不用 MCP？  
   MCP 是一问一答的工具调用，缺少长会话、流式进度、执行中的审批和会话恢复。

## 延伸阅读

- JSON-RPC Working Group，[JSON-RPC 2.0 Specification](https://www.jsonrpc.org/specification)：整份规范很短，半小时能读完。
- OpenAI，[Codex app-server README](https://github.com/openai/codex/blob/main/codex-rs/app-server/README.md)：B0 参照的设计：Thread、Turn、Item 与审批请求。
- Zed 等，[Agent Client Protocol](https://agentclientprotocol.com)：编辑器与 coding agent 之间的开放协议，本地 Agent 作为编辑器的子进程、经 stdio 用 JSON-RPC 通信。
- Microsoft，[Language Server Protocol](https://microsoft.github.io/language-server-protocol/)：同类思路的成熟先例：一个语言服务、多个编辑器共用一套协议。

本项目课程：[结构化协议笔记](../docs/bonus-protocol/protocol-notes.md) · [结构化协议实践](../docs/bonus-protocol/protocol-lab.md) · [17 · 确认请求的规则](17-permissions-approval-audit.md) · [12 · 子 Agent](12-subagents.md)

[上一篇：评测与失败归因](18-evaluation-and-attribution.md) · [回学习目录](README.md) · [术语表](TERMS.md)
