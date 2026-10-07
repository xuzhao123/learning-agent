# Day 6：MCP 协议（上）——client 怎样连上工具

## 学习目标

读完本文，应能回答以下问题：

1. MCP 要解决什么问题，为什么常拿 LSP 和 USB-C 作类比。
2. host、client、server 三个角色各做什么；JSON-RPC 2.0 的请求、响应、通知、错误长什么样。
3. stdio 和 Streamable HTTP 两种传输各适合什么场景，stdio 的分帧规则是什么。
4. Tools、Resources、Prompts 三个原语分别由谁控制、用来做什么。
5. 旧版（2025-11-25 及以前）的 initialize 握手交换了哪两样东西，为什么需要 capabilities 协商。
6. 2026-07-28 版为什么去掉了握手，协议版本和 capabilities 改由什么携带，`server/discover` 起什么作用。
7. `tools/list`、`tools/call` 是谁调谁、返回什么；MCP 工具定义怎样变成模型能用的 function 定义。
8. 协议错误和工具执行错误有什么区别，各自该交给谁处理。

## 1. 问题：M 个应用 × N 个工具

假设有 3 个 Agent 应用（聊天助手、IDE 插件、自动化平台），每个都想接 GitHub、数据库、日历这 3 个系统。如果各写各的对接代码，就要写 3 × 3 = 9 份；应用和工具一多，工作量按乘法增长。每家模型的 function calling 格式还略有不同，同一个工具要为不同模型再适配一次。

**Model Context Protocol（MCP）** 是 Anthropic 在 2024 年 11 月发布的开放协议，现在由社区治理。它的办法是加一层统一的接口：

- 工具一方实现一次 **MCP server**，按协议暴露自己的能力；
- 应用一方实现一次 **MCP client**，按协议调用任何 server。

工作量从 M × N 变成 M + N。两个常见类比：

- **LSP（Language Server Protocol）**：编辑器不必为每种语言重写补全、跳转，语言方写一个 language server，所有支持 LSP 的编辑器都能用。MCP 的消息格式本身就借鉴了 LSP，二者都建立在 JSON-RPC 2.0 之上。
- **USB-C**：设备和电脑只需各自支持同一个接口。

要注意 MCP **不取代** function calling。模型仍然通过各家 API 的 function calling 决定调用哪个工具；MCP 规定的是"应用怎样从外部发现工具、怎样把调用转给工具所在的进程"。一次完整调用里两套协议各管一段：

```text
模型 ──function calling──▶ 应用（MCP client） ──MCP tools/call──▶ MCP server ──▶ 真实系统
```

## 2. 角色与消息格式

### 2.1 三个角色

| 角色 | 是什么 | 例子 |
| --- | --- | --- |
| host | 用户直接使用的应用，管理模型、对话和权限 | 桌面聊天客户端、IDE、自己写的 Agent |
| client | host 内部的连接器，**一个 client 对应一个 server 连接** | host 连了 3 个 server，就有 3 个 client |
| server | 暴露工具、数据或提示模板的程序 | 文件系统 server、GitHub server |

host 是安全决策的地方：哪些 server 可以连、哪些工具可以给模型、哪些调用要用户确认，都由 host 决定，server 无权替 host 做主。

### 2.2 JSON-RPC 2.0

MCP 的每条消息都是一个 JSON-RPC 2.0 对象，共三种：

```json
{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"get_weather","arguments":{"city":"杭州"}}}
{"jsonrpc":"2.0","id":7,"result":{"content":[{"type":"text","text":"多云，22℃"}]}}
{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":7}}
```

- **请求**有 `id`，对方必须回一条 `id` 相同的响应；
- **响应**带 `result` 或 `error`，二者只有一个；
- **通知**没有 `id`，对方不回复。

靠 `id` 关联请求和响应，所以同一条连接上可以同时有多个请求在途，响应顺序不必和请求顺序一致。错误响应的 `code` 有一组标准值：

| code | 含义 |
| --- | --- |
| -32700 | 解析错误：不是合法 JSON |
| -32600 | 无效请求：不是合法的 JSON-RPC 对象 |
| -32601 | 方法不存在 |
| -32602 | 参数无效 |
| -32603 | 内部错误 |

MCP 在此基础上补充了自己的错误码，例如 2026-07-28 版的 `-32022`（不支持的协议版本）。

## 3. 传输：消息怎么送过去

### 3.1 stdio

host 把 server 作为**子进程**启动，通过它的标准输入输出通信：

- client 把消息写进 server 的 stdin，server 把消息写到 stdout；
- **每条消息占一行**，用换行分隔，所以单条 JSON 内部不能有换行；
- server 的 stdout 只能写合法的 MCP 消息，日志一律写 stderr；
- 关闭时 client 先关掉 server 的 stdin，server 读到 EOF 后退出；超时不退再发 SIGTERM、SIGKILL。

stdout 被协议独占是最常见的坑：server 里随手打印一行调试信息，client 就会读到一行不是 JSON 的内容，连接随即出错。

### 3.2 Streamable HTTP

server 作为独立的网络服务运行，client 用 HTTP POST 发消息；响应可以是普通 JSON，也可以是 SSE 流（用于进度通知等）。适合远程、多用户、需要统一部署和鉴权的场景。

| | stdio | Streamable HTTP |
| --- | --- | --- |
| 部署 | 零部署，host 本机起子进程 | 需要单独运行和维护服务 |
| 范围 | 只能本机 | 可远程、可多租户 |
| 鉴权 | 继承本机用户权限，通常不另做鉴权 | 必须做（规范定义了基于 OAuth 的授权） |
| 生命周期 | 跟随 host，一个 host 一个进程 | 独立于 client |
| 典型用途 | 本地文件、本地命令行工具、开发调试 | SaaS 服务、团队共享的工具服务 |

早期的 HTTP+SSE 传输（2024-11-05 版）已被 Streamable HTTP 取代，处于弃用状态。

## 4. 三个原语

server 可以暴露三类能力，区别在于**由谁决定使用**：

| 原语 | 是什么 | 由谁控制 | 类比 | 例子 |
| --- | --- | --- | --- | --- |
| Tools | 可执行的函数，用 JSON Schema 描述参数 | 模型决定何时调用 | API | `create_issue`、`run_query` |
| Resources | 只读数据，用 URI 寻址 | 应用决定读哪些、放进上下文 | 文件系统 | `file:///docs/api.md`、`db://orders/schema` |
| Prompts | 预置的提示模板，可带参数 | 用户主动选择 | 快捷指令 | "生成周报"、"按模板评审 PR" |

Tools 是用得最多的原语，它的定义和 function calling 的工具定义是同一套思想：名字、描述、参数的 JSON Schema。下面是一个典型的工具定义：

```json
{
  "name": "get_weather",
  "description": "查询城市当前天气",
  "inputSchema": {
    "type": "object",
    "properties": {"city": {"type": "string", "description": "城市名"}},
    "required": ["city"]
  }
}
```

client 一侧也有几项能力，例如 sampling（server 反过来请 host 的模型生成内容）、roots（告诉 server 可以访问哪些目录）、elicitation（server 向用户要补充信息）。2026-07-28 版已把 roots、sampling、logging 标为弃用，建议改用工具参数、直接调用模型 API、写 stderr 或 OpenTelemetry。

## 5. 生命周期（一）：旧版的 initialize 握手

2025-11-25 及以前的版本，连接建立后必须先握手，再做别的事：

```text
client                                   server
  │── initialize(请求) ───────────────────▶│  我想用的协议版本、我支持的能力、我是谁
  │◀──────────────────────── 响应 ─────────│  我选定的协议版本、我支持的能力、我是谁
  │── notifications/initialized(通知) ───▶│  握手完成，可以开始了
  │── tools/list ─────────────────────────▶│
  │── tools/call ─────────────────────────▶│
```

一次真实的握手如下（为便于阅读做了换行，实际传输时每条消息占一行）：

```json
→ {"jsonrpc":"2.0","id":1,"method":"initialize","params":{
     "protocolVersion":"2025-11-25",
     "capabilities":{},
     "clientInfo":{"name":"demo-client","version":"1.0.0"}}}
← {"jsonrpc":"2.0","id":1,"result":{
     "protocolVersion":"2025-11-25",
     "capabilities":{"tools":{"listChanged":true},"resources":{"subscribe":true},"prompts":{}},
     "serverInfo":{"name":"demo-server","version":"1.0.0"}}}
→ {"jsonrpc":"2.0","method":"notifications/initialized"}
```

握手交换的核心是两样东西：

1. **协议版本**。client 报上自己支持的最新版本；server 支持就原样返回，不支持就返回自己支持的版本，client 不接受就断开。版本号是日期字符串，可以直接按字符串比较新旧。
2. **capabilities（能力）**。双方各自声明支持哪些可选功能：server 声明有没有 tools、resources、prompts，工具列表变化时会不会发通知（`listChanged`）；client 声明支持 sampling、roots、elicitation 中的哪些。

`clientInfo` / `serverInfo` 只是自报身份，用于显示和日志，不能用来做安全决策。

### 为什么需要 capabilities 协商

协议里有很多**可选**功能，双方的实现进度也不一样。没有协商，就只能"发出去试试"：

- client 给一个只有工具的 server 发 `resources/list`，只能等一个"方法不存在"的错误，或者干脆收不到回复；
- server 想在工具列表变化时通知 client，却不知道 client 会不会处理这条通知；
- server 想请 host 的模型做一次 sampling，可 host 根本没有开放模型。

协商把"能不能做"提前说清楚，好处有三点：

- **不发对方不支持的请求**：client 看到 server 没有 `resources` 能力，就不会去列资源；
- **功能可以渐进演进**：新功能以新能力的形式加入，老实现不声明就不会被用到，协议升级不需要所有实现同时更新；
- **双向的权限边界**：client 不声明 sampling，server 就不能借用 host 的模型；能力声明本身也是一种授权范围。

## 6. 生命周期（二）：2026-07-28 版去掉了握手

2026 年 7 月 28 日发布的新版协议做了一次大改动：**协议变成无状态**。

- 删除 `initialize` / `notifications/initialized`，也删除了 Streamable HTTP 上的会话 ID；
- **每个请求**都在 `params._meta` 里带上协议版本和 client 的 capabilities，并建议带上 client 身份；
- server 在每个结果的 `_meta` 里报上自己的身份；
- 新增 `server/discover`，server 必须实现：client 可以先调用它，一次拿到 server 支持的版本列表、capabilities 和身份。

```json
→ {"jsonrpc":"2.0","id":1,"method":"server/discover","params":{"_meta":{
     "io.modelcontextprotocol/protocolVersion":"2026-07-28",
     "io.modelcontextprotocol/clientInfo":{"name":"demo-client","version":"1.0.0"},
     "io.modelcontextprotocol/clientCapabilities":{}}}}
← {"jsonrpc":"2.0","id":1,"result":{
     "resultType":"complete",
     "supportedVersions":["2026-07-28","2025-11-25"],
     "capabilities":{"tools":{}},
     "_meta":{"io.modelcontextprotocol/serverInfo":{"name":"demo-server","version":"1.0.0"}}}}
→ {"jsonrpc":"2.0","id":2,"method":"tools/list","params":{"_meta":{ …同上三项… }}}
```

交换的仍然是**协议版本和 capabilities 这两样东西**。准确地说，每个请求自带协议版本和客户端能力，服务端能力通过 `server/discover` 获取。这样省掉的是强制初始化和协议会话状态；如果客户端先做 discovery，启动时仍有一次发现请求的往返，不保证首个工具调用更快。为什么这样改：

- **水平扩展**：依赖协议会话状态时，需要会话亲和路由或共享会话存储；请求自描述后，实例无需记住先前的协商信息。业务状态仍需单独处理，不能由此推导出所有有状态工具都能任意切换实例。
- **故障恢复**：server 重启后，新请求不再依赖旧协议会话。已经发出的工具调用是否可以重发，仍取决于能否确认未执行、工具是否幂等或是否有去重机制；无状态不保证重放安全。
- **语义清晰**：每个请求都自带完整上下文，不再有"这条连接当初协商了什么"的隐含状态。

代价是每个请求重复传输并处理 `_meta`，报文大小取决于声明的能力和身份信息。这通常增加的是协议字节数，而非直接增加模型输入 token。兼容旧版还需要探测与回退，失败或超时的探测可能增加启动延迟。确实需要跨调用保存状态的 server（购物车、浏览器会话、数据库事务），改为由一个工具返回显式的句柄（handle），后续调用把句柄当作普通参数传入，并处理句柄的授权、生命周期和状态存储。

按机制推导，长期复用同一个 stdio 进程、频繁执行很短的调用时，旧握手成本已摊销，新版增加重复元数据却没有水平扩展收益；只支持旧版的 server 会增加兼容成本；原先依赖隐式会话的有状态工具会增加迁移工作。这些是设计取舍，不是本项目测得的性能结论。

版本不匹配时，server 返回 `-32022` 错误，并在 `data.supported` 里列出自己支持的版本，client 从中选一个重试。

### 新旧两代怎样共存

规范把 2026-07-28 及以后称为 modern，之前的称为 legacy，两者都支持的称为 dual-era。dual-era client 在 stdio 上的做法是：

1. 先发 `server/discover` 试探（带上想用的新版本）；
2. 收到 `DiscoverResult`，说明对方是新版 server，继续用新协议；
3. 收到"不支持的协议版本"这类新版错误码，说明对方也是新版，只是版本不同，换版本重试，**不要**退回握手；
4. 收到其他错误或超时，说明对方是旧版 server，退回 `initialize` 握手。

规范特意要求第 4 步不能只看某一个错误码，因为旧版 server 对未知方法的反应各不相同：有的回 -32601，有的回 -32602，有的根本不回。所以试探必须设超时。

主流 SDK 已按这个流程实现。用新版 SDK 连接时，日志里可能看不到 `initialize`，而是一条 `server/discover`。这不是 SDK 漏了握手，而是协议本身改了。

## 7. 发现与调用工具

| 方法 | 方向 | 参数 | 返回 |
| --- | --- | --- | --- |
| `tools/list` | client → server | 可选的分页游标 `cursor` | `tools` 数组（每项含 name、description、inputSchema 等）；可能带 `nextCursor` |
| `tools/call` | client → server | `name` + `arguments` | `content` 数组（文本、图片、资源链接等）、可选的 `structuredContent`、`isError` 标记 |

新版规范要求 `tools/list` 按确定的顺序返回，便于 client 缓存，也能提高模型侧的提示缓存命中率（工具定义通常放在提示前部）。server 的工具列表变化时可以通知 client，client 重新拉取。

### 从 MCP 工具定义到模型的 function 定义

host 拿到 `tools/list` 后，要把每个工具转换成所用模型 API 的格式。以 OpenAI 兼容格式为例，几乎是字段改名：

```text
MCP Tool                       function 定义
name         ──────────────▶   function.name（可能需要改写）
description  ──────────────▶   function.description
inputSchema  ──────────────▶   function.parameters（同为 JSON Schema，原样传）
```

真正需要处理的是边界情况：

- **重名**：两个 server 都有 `search`，或者 MCP 工具和本地工具同名。常见做法是加前缀，例如 `mcp__github__search`。规范提醒 `serverInfo.name` 不保证唯一，不宜单独用来消歧。
- **命名字符集**：MCP 建议工具名用字母、数字、`_`、`-`、`.`，最长 128；部分模型 API 不允许 `.`，或限制最长 64，要做转换并记住映射关系，调用时还原成原名。
- **Schema 方言**：MCP 默认 JSON Schema 2020-12，模型 API 可能只支持其中一部分关键字。
- **成本**：每个工具定义都会占用模型的输入 token。连上十几个 server、上百个工具后，光工具定义就可能有上万 token，而且工具太多也会降低模型选对工具的概率。所以 host 往往只暴露当前任务需要的工具。

## 8. 两种错误

调用工具可能以两种方式失败，规范要求区分开：

| | 协议错误 | 工具执行错误 |
| --- | --- | --- |
| 例子 | 工具不存在；请求不符合 `tools/call` 的结构；server 内部崩溃 | 下游 API 失败；参数值不合法（日期格式错、超出范围）；业务规则不允许 |
| 形式 | JSON-RPC `error`（如 -32602） | 正常的 `result`，`isError: true`，`content` 里写原因 |
| 模型能不能改正 | 通常不能 | 通常能：错误信息告诉它该怎么改参数 |
| client 怎么做 | 可以告诉模型，但更多是记日志、提示配置问题 | 应当回填给模型，让它自我纠正 |

2025-11-25 版专门澄清：**参数校验失败属于工具执行错误**，应该用 `isError: true` 返回，而不是 JSON-RPC 的 -32602，目的是让模型看到具体原因并改正参数。只有"连调用本身都不成立"的情况（例如工具名不存在）才用协议错误。

```json
{"jsonrpc":"2.0","id":4,"result":{
  "content":[{"type":"text","text":"date 必须是未来日期，今天是 2025-08-08"}],
  "isError":true}}
```

## 9. 设计取舍

**stdio 还是 HTTP**：见第 3 节的表。经验法则是：工具只在用户本机有意义、只服务一个用户，用 stdio；工具需要共享、需要集中管理凭据或远程访问，用 HTTP。

**参数校验放在哪一端**：两端都可以做，但只有 server 端是必须的。

- client 端校验（按 inputSchema 预检）可以更早发现错误、少一次往返，属于体验优化；
- server 端校验是安全要求：server 无法确认请求来自一个守规矩的 client，请求也可能是手写的、被篡改的，或由被提示注入操纵的模型生成的。**server 是信任边界**，执行任何操作前都必须自己校验。规范把"server 必须校验所有工具输入"写成了 MUST。

这和长期记忆里"模型决定要不要记，程序决定能不能记"是同一个原则：提出请求的一方可以有判断，但放行与否由执行方的代码决定。

**工具定义可信吗**：不完全可信。工具的 description 和 annotations（如 `readOnlyHint`、`destructiveHint`）都是 server 自报的。规范要求 client 把来自不可信 server 的 annotations 视为不可信。恶意 server 可以在 description 里藏指令（工具投毒），工具返回的内容也可能带提示注入。host 应当只连接可信的 server，敏感操作要让用户确认，并把工具结果当作数据而不是指令。

## 10. 昨日回顾：MemGPT 一句话

MemGPT 把 **LLM 当作操作系统的处理器**，把**上下文窗口当作内存**（容量小、访问快），外部存储当作磁盘。paging 靠**模型自己调用函数**：上下文接近上限时系统发出"内存压力"提示，模型调用记忆管理函数，把内容换出到外部存储，或者从外部存储检索换入。和操作系统由硬件和内核自动换页不同，MemGPT 的换页决策由模型做出。

"模型决定要不要记、程序决定能不能记"在本项目中的三道检查，见项目实践。

## 11. 自测

**1. 为什么说 MCP 不取代 function calling？**
function calling 是模型 API 的能力，负责让模型表达"我要调用哪个工具、参数是什么"；MCP 负责应用和工具进程之间的发现与调用。host 把 MCP 工具转换成 function 定义交给模型，模型返回调用请求后，host 再转成 `tools/call` 发给 server。

**2. 一个 host 连了 GitHub 和 Postgres 两个 server，有几个 client？**
两个。client 和 server 是一对一的连接，host 可以持有多个 client。

**3. 为什么 stdio server 不能在 stdout 打印日志？**
stdout 是协议通道，client 把每一行都当作一条 JSON-RPC 消息解析。混进一行日志，解析就会失败。日志应写到 stderr。

**4. 旧版握手交换了哪两样东西？没有 capabilities 协商会怎样？**
协议版本和 capabilities。没有协商，双方只能盲发请求，靠报错或超时才知道对方不支持；新功能也无法渐进加入，因为发送方无法判断对方是否理解。

**5. 2026-07-28 版删除了握手，版本和 capabilities 去了哪里？**
放进每个请求的 `_meta`：`io.modelcontextprotocol/protocolVersion` 和 `io.modelcontextprotocol/clientCapabilities`。server 的能力通过 `server/discover` 获取。

**6. 新版 client 发 `server/discover`，旧版 server 回了 -32601，client 该怎么做？**
退回 `initialize` 握手。-32601 不是新版定义的错误码，说明对方是旧版 server。但 client 不能只认 -32601：其他错误或超时同样表示对方是旧版。

**7. 模型把日期参数写成了 "下周三"，server 该返回什么？**
工具执行错误：`result` 里 `isError: true`，`content` 写明需要的格式和当前日期，让模型改正后重试。

**8. 模型调用了一个 server 上不存在的工具名，server 该返回什么？**
协议错误：JSON-RPC `error`，code -32602，消息说明工具不存在。

**9. 一个工具的 annotations 写着 `readOnlyHint: true`，host 可以据此跳过用户确认吗？**
只有在 server 可信时才可以。annotations 是 server 自报的，恶意 server 可以把删除操作标成只读。

## 参考

- Model Context Protocol, [Specification 2026-07-28](https://modelcontextprotocol.io/specification/2026-07-28)：[Key Changes](https://modelcontextprotocol.io/specification/2026-07-28/changelog)、[Versioning and Compatibility](https://modelcontextprotocol.io/specification/2026-07-28/basic/lifecycle)、[Discovery](https://modelcontextprotocol.io/specification/2026-07-28/server/discover)、[stdio](https://modelcontextprotocol.io/specification/2026-07-28/basic/transports/stdio)、[Tools](https://modelcontextprotocol.io/specification/2026-07-28/server/tools)
- Model Context Protocol, [Specification 2025-11-25](https://modelcontextprotocol.io/specification/2025-11-25)（最后一个使用 initialize 握手的版本）及其 [Key Changes](https://modelcontextprotocol.io/specification/2025-11-25/changelog)（SEP-1303：参数校验错误作为工具执行错误返回）
- Anthropic, [Introducing the Model Context Protocol](https://www.anthropic.com/news/model-context-protocol), 2024
- [JSON-RPC 2.0 Specification](https://www.jsonrpc.org/specification)
- Microsoft, [Language Server Protocol](https://microsoft.github.io/language-server-protocol/)
- [mcp-go](https://github.com/mark3labs/mcp-go)（Go SDK，同时支持新旧两代协议）
- Packer et al., [MemGPT: Towards LLMs as Operating Systems](https://arxiv.org/abs/2310.08560), 2023

本课程的代码实现、动手步骤与实验记录见 [Day 6 项目实践](day-06-lab.md)。
