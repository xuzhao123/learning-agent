# 07 · MCP：让 Agent 接上不同来源的工具服务

MCP 统一工具服务的接入方式。先分清应用、连接端和服务端，就容易看懂一个工具究竟由谁执行。

[阅读目录](README.md) · [技术展开](#技术展开) · [基础自测](#自测)

## 先认识术语

| 术语 | 通俗解释 |
| --- | --- |
| MCP | Model Context Protocol，模型上下文协议 |
| Host | 承载用户、模型与工具连接的应用 |
| Client | 代表应用与某个 server 交换协议请求的连接端 |
| Server | 提供工具、资源等能力的程序 |
| Capability | 协议功能的能力声明 |
| JSON-RPC | 用 JSON 表示方法、参数、请求 ID、结果或错误的格式 |
| stdio | 父子进程之间用标准输入输出通信 |
| Streamable HTTP | 用 HTTP 传送 MCP 消息的方式 |
| _meta | 请求中携带的协议元信息，不是工具业务参数 |

## MCP 和模型的工具调用不是同一层

```text
工具服务 ── 返回工具定义 ──→ MCP client
                                ↓ 转成模型可用接口
用户问题 ──────────────→ 模型生成工具调用
                                ↓
执行器 ── tools/call ──→ MCP server ──→ 实际函数或外部系统
                                ↑ 返回结果
模型 ←── 工具结果写回历史 ──────┘
```

模型通常只看到工具定义，不需要自己说 JSON-RPC。应用将协议工具转换成模型接口，再把模型提出的调用交给正确的 server。

MCP 也定义资源与提示模板等能力；本章先围绕可以产生行动的工具理解链路。

## 谁执行 calculator

stdio 模式：应用启动一个 server 子进程，实际 handler 在子进程里运行。父进程给 stdin 写请求，从 stdout 读结果。

HTTP 模式：server 可以是独立服务，也可以作为现有应用中的一个处理器。请求到哪里，handler 就在相应服务端执行；不是每次连接都必须新开进程。

server 的 handler 又可能调用另一个服务。若想知道最终计算或写入发生在哪里，要继续追踪 handler，而不能只看模型输出里的工具名字。

协议角色不等于固定进程划分，更不等于固定部署机器。

## 从发现到调用

常见链路包含能力发现或初始化、tools/list 和 tools/call。工具列表提供名字、描述与 inputSchema（参数结构约束）；调用时提供具体名字和 arguments（业务参数）。

协议版本会影响初始化方式：

| 版本范围 | 基本方式 |
| --- | --- |
| 2025-11-25 及更早的传统方式 | initialize 握手建立协议状态 |
| 2026-07-28 方式 | 每个请求携带版本、client 信息与能力元数据；server/discover 查询服务端信息 |

新版减少对握手状态的依赖，也增加每次请求的重复元数据。并不是把 server 能力也凭空放进 client 声明。详见 [官方生命周期规范](https://modelcontextprotocol.io/specification/2026-07-28/basic/lifecycle)。

实现必须使用双方支持的协议版本；兼容两代的 client 还要处理探测和退回路径。协议格式变化不改变“server 接到请求后实际运行工具”的核心分工。

## 请求 ID 别混在一起

| ID | 关联什么 |
| --- | --- |
| 模型的 tool_call_id | 一次工具请求与返回模型的结果 |
| JSON-RPC id | 一次协议请求与协议响应 |
| 业务操作 ID / 幂等键 | 同一个外部业务意图及其重复提交 |
| 任务 ID | 整个任务、进度与恢复记录 |

这几个可以建立映射，但含义不同。新生成的 RPC ID 不应该导致同一个报销提交被当作新的业务操作。

## 校验与错误

服务端必须自己检查输入：

- Schema 检查结构。
- handler 检查取参和关键边界。
- 业务函数检查语义与权限。

重复检查不总是冗余。有人可能关闭 Schema 校验、绕过原 client 或直接访问服务端；取参层至少不能把错误类型强行当成有效值。

协议错误表示方法、报文等出了问题；工具执行错误表示方法接通了，但业务动作没完成。两种错误应明确返回，不让 client 把“RPC 成功收到响应”理解成“工具动作成功”。工具协议见 [官方 tools 规范](https://modelcontextprotocol.io/specification/2026-07-28/server/tools)。

stdio server 的 stdout 是协议通道，日志应放到 stderr，避免普通打印破坏消息解析。

## 协议不授予权限

拿到 tools/list 不等于获得执行全部动作的授权。server 仍需检查身份与操作范围。

启动第三方 stdio server 时，它能读到什么环境变量、文件和网络，取决于进程权限设置。MCP 的格式不会自动去掉密钥，也不会隔离它的系统调用。

## 技术展开

### 进阶术语：报文、适配器与业务失败

| 术语 | 含义 |
| --- | --- |
| Wire message | 实际在传输通道中发送的协议报文 |
| Adapter | 在模型工具格式与MCP格式间转换的程序 |
| Transport error | 未拿到可靠协议响应的连接/传输问题 |
| Protocol error | 方法、报文或请求参数不符合协议 |
| Tool error | 协议请求接通，但工具业务执行失败 |

### 一次 tools/call 的完整 JSON-RPC 示例

下例展示 2026-07-28 风格的 JSON-RPC 部分；HTTP 传输还有版本和身份相关头部要求，不能把它当成完整 HTTP 请求：

```json
{
  "jsonrpc":"2.0",
  "id":7,
  "method":"tools/call",
  "params":{
    "name":"calculator",
    "arguments":{"expression":"3*180"},
    "_meta":{
      "io.modelcontextprotocol/protocolVersion":"2026-07-28",
      "io.modelcontextprotocol/clientInfo":{
        "name":"example-client","version":"1.0"
      },
      "io.modelcontextprotocol/clientCapabilities":{}
    }
  }
}
```

_meta 属于请求的 params 元信息，arguments 才是工具业务输入。不要把它们一起当作 calculator 的参数做 schema 校验。

这里 arguments 是对象；常见聊天模型返回的 function.arguments 是 JSON 字符串。适配器需要先解析再构造 MCP 参数，返回时再转成模型的 tool observation。

工具结果示意：

```json
{
  "jsonrpc":"2.0","id":7,
  "result":{
    "resultType":"complete",
    "content":[{"type":"text","text":"540"}],
    "isError":false
  }
}
```

工具结果可含多种内容块，不保证只有一个字符串。应用自行简化后得到的 Observation，也不能冒充原始 MCP 报文。

### 三类错误为什么要分别处理

| 层次 | 例子 | 是否可直接重试 |
| --- | --- | --- |
| 传输 | 发出请求后连接断开 | 可能未知；需看副作用和幂等 |
| 协议 | 参数类型不合法、方法不存在 | 同输入通常无效，应修正 |
| 业务 | 除零、对象无权限、操作部分失败 | 依据具体执行语义 |

JSON-RPC 错误示意：

```json
{
  "jsonrpc":"2.0","id":7,
  "error":{"code":-32602,"message":"Invalid params"}
}
```

与之不同，业务失败可以通过工具结果中的 isError 表达。RPC 有响应不等于业务成功；isError=true 也不能统一当成瞬时抖动反复调用。

请求/响应、内容块及校验规则见 [官方工具规范](https://modelcontextprotocol.io/specification/2026-07-28/server/tools)。

### Schema 开关怎样改变失败位置

以 expression 必须为字符串的工具为例：

| 输入 | 开启schema校验 | 关闭schema校验，仅RequireString | 语义层 |
| --- | --- | --- | --- |
| expression为数字 | 结构层拒绝 | 取参层拒绝类型 | 不进入计算 |
| 没有expression | 必填校验拒绝 | 取参层拒绝缺失 | 不进入计算 |
| expression为空串 | 是否拒绝看长度约束 | 字符串取参可通过 | 计算层仍要拒绝无效表达式 |
| expression为1/0 | 类型通常合法 | 取参可通过 | 业务层拒绝除零 |

因此 RequireString 不应被解释成“非空、长度与权限全校验”。是否重复校验，要按实际入口和已强制的前置条件判断。

### stateless 的收益和成本落在哪

现代每请求元信息减少对已握手会话状态的依赖，连接切换或路由到另一实例更容易理解。但每次带相同客户端信息会增加报文与校验开销。

server/discover 仍可能发生一次网络往返；并非所有实现都把第一次请求缩减为“一步”。长连接、很多微小调用、服务端需要应用会话状态时，重复元数据或自行维护状态可能更不划算。

无协议会话状态不能消除业务状态。报销单、文件和长任务仍需身份、幂等与持久化。

### 多server与权限需要独立映射

适配器应保留“模型工具名 → 连接实例 → 服务端原始工具名”的映射，而不只拼一个前缀。多个 server 可以有同名 calculator，重新连接或动态列表变化也要更新映射。

clientCapabilities 声明功能支持，不证明当前用户有操作权限。stdio 的环境继承、HTTP 的鉴权、server 的参数/对象权限都是各自的信任边界。

### 对照已有实现

[Connect、Definitions、Call 与手写Raw调用](../internal/mcp/mcp.go)展示两种传输、名称适配、_meta 注入和返回处理。传统握手与 modern 方式都有对应实验入口。

动态 list_changed、通用远程鉴权与第三方进程环境最小化仍是现有边界。新的协议版本或 SDK 内容块应按实际协商结果处理。

### 进阶推演

1. 为什么在 schema 校验关闭后，数字 expression 能让第二层真正触发？  
   第一层不再拦截，RequireString 才会看到非字符串；这能区分两层各自覆盖的条件。

2. server/discover 返回支持 tools，是否表示某用户可以调用所有工具？  
   不是。功能能力、可见工具与实际对象/操作授权是不同契约。


## 自测

1. 模型返回 mcp_calculator，计算在哪里执行？  
   要看连接映射。通常在所连接 server 的 handler，或它调用的下游，不能从名字推断。

2. 一个 HTTP server 同时服务两个 client，为什么不能只信任上游参数校验？  
   其他入口或配置可能不同，服务端本身就是执行边界。

3. 去掉握手后，有副作用的工具就能自动安全重试吗？  
   不能。协议状态与业务幂等是不同问题，仍要确认外部动作的结果。

## 延伸阅读

- [Day 6 client](../docs/day-06/day-06-notes.md)
- [Day 7 server](../docs/day-07/day-07-notes.md)
- [MCP 动手与校验](../docs/day-07/day-07-lab.md)

[上一篇：长期记忆](06-memory.md) · [下一篇：Skills 知识与流程](08-skills.md) · [术语表](TERMS.md)
