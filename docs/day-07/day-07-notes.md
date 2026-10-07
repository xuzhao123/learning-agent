# Day 7：MCP 协议（下）——写一个 MCP server

## 学习目标

读完本文，应能回答以下问题：

1. MCP server 收到 `tools/list` 和 `tools/call` 时各要做什么，`CallToolResult` 由哪些字段组成。
2. 什么情况返回 JSON-RPC 错误，什么情况返回 `isError: true`，为什么参数校验失败属于后者。
3. 工具粒度怎样取舍：一个万能工具，还是多个小工具。
4. 为什么 MCP server 应尽量无状态；确实需要状态时，新版协议推荐怎样做。
5. 为什么 server 端必须重新校验参数，即使 client 已经按 schema 检查过。
6. 一个 server 的参数校验通常分几层，每层拦什么。
7. 运行 server 时的最小权限原则具体指什么。

## 1. 昨日回顾

**三大原语**：
- **Tools**：模型决定何时调用的函数，参数用 JSON Schema 描述；
- **Resources**：应用决定读取的只读数据，用 URI 寻址；
- **Prompts**：用户主动选择的预置提示模板。

**`tools/list` 与 `tools/call`**：两者都是 client 调 server。
- `tools/list` 返回工具定义数组（name、description、inputSchema 等），可分页；
- `tools/call` 带上 name 和 arguments，返回 `content` 数组、可选的 `structuredContent`，以及表示是否失败的 `isError`。

## 2. server 的职责

一个只提供工具的 server，核心工作只有三件事：

```text
声明能力 ──▶ 回答 tools/list ──▶ 处理 tools/call
（capabilities.tools）  （返回工具定义）     （校验 → 执行 → 包装结果）
```

### 2.1 声明能力

server 在 `initialize` 的响应（旧版）或 `server/discover` 的结果（新版）里声明 `capabilities.tools`。如果工具列表会在运行中变化，再声明 `listChanged: true`，并在变化时发通知。只提供工具的 server 不要声明 resources、prompts，否则 client 会来请求，而 server 无法回答。

### 2.2 回答 `tools/list`

返回每个工具的定义。写好定义比写好实现更影响效果，因为模型只能看到定义：

- **name**：简短、稳定、见名知义，例如 `search_issues` 比 `tool1` 好；
- **description**：写清做什么、什么时候用、返回什么，必要时写上限制（“只读”、“最多返回 50 条”）；
- **inputSchema**：类型、必填项、取值范围、长度上限都写进去；不接受额外字段时写 `additionalProperties: false`；
- **顺序**：新版规范要求每次以确定的顺序返回，方便 client 缓存，也有利于模型侧的提示缓存。

### 2.3 处理 `tools/call`

结果的结构：

```json
{
  "content": [{"type": "text", "text": "42"}],
  "structuredContent": {"result": 42},
  "isError": false
}
```

- `content`：给模型看的内容，可以有多项，类型包括文本、图片、音频、资源链接、嵌入资源；
- `structuredContent`：给程序用的结构化结果。工具声明了 `outputSchema` 时必须提供并符合它；为了兼容，同时在 `content` 里放一份序列化文本；
- `isError`：工具执行失败时为 `true`，`content` 写明原因。

### 2.4 两种错误，选哪一种

| 情况 | 返回方式 |
| --- | --- |
| 工具名不存在 | JSON-RPC 错误，code -32602 |
| 请求结构不对（例如 `params` 不是对象） | JSON-RPC 错误，code -32602 或 -32600 |
| server 内部崩溃、无法处理 | JSON-RPC 错误，code -32603 |
| 参数类型错、缺必填项、多了未知字段 | `isError: true` |
| 参数值不合法（格式错、超出范围） | `isError: true` |
| 下游 API 失败、业务规则不允许 | `isError: true` |

有些教程说“参数无效走 -32602”，这是 2025-11-25 版之前常见的写法。该版本根据 SEP-1303 明确改为：**参数校验失败用 `isError: true` 返回**。理由是 JSON-RPC 错误通常只进日志，模型看不到；放进结果里，模型才能读到“expression 应为字符串”并改正参数。判断标准是：**模型调整参数后有没有可能成功**。有可能，就返回执行错误；连调用本身都不成立，才返回协议错误。

## 3. 设计取舍

### 3.1 工具粒度

| | 一个万能工具（如“执行任意命令”） | 多个小工具（如 `read_file`、`list_dir`、`search`） |
| --- | --- | --- |
| 灵活性 | 高，什么都能做 | 只能做定义好的事 |
| 模型用起来 | 难：要自己组织命令、处理各种输出格式 | 容易：每个工具的 schema 都是使用说明 |
| 校验 | 几乎做不了，参数是任意字符串 | 每个参数都能校验类型和范围 |
| 权限与审计 | 难：日志里只有一串命令，无法按操作授权 | 容易：可以按工具开放、按工具确认、按工具统计 |
| 开发量 | 少 | 多 |

这和 Day 2 讲过的“协议越自由，模型发挥空间越大；协议越严格，行为越可靠”是同一个取舍。决定粒度的应当是**调用方的认知负担**：调用方是模型，它没法事先读完你的源码，每个工具都要能单靠定义就用对。经验做法是：按用户的任务划分工具，而不是照搬底层 API。高风险操作单独成工具，便于单独确认；常常连续调用的几步，可以合成一个工具，减少往返和出错机会。

工具也不是越多越好。每个定义都占输入 token，工具太多时模型选错的概率也会上升。几十个以上时，应该考虑分组、按需暴露，或者拆成多个 server。

### 3.2 无状态与有状态

**纯函数工具最适合入门**：计算器、格式转换、校验器，输入相同、输出就相同，不依赖历史，可以随意重试、并发、多实例部署。

2026-07-28 版协议本身已经无状态：没有握手，也没有会话 ID，任何请求都可以发给任何一个 server 实例。需要跨调用保存状态时（购物车、浏览器页面、数据库事务），规范建议使用**显式句柄**：

```text
create_basket()                     → {"basket_id": "bsk_a1b2c3"}
add_item(basket_id, sku)            → 按 basket_id 找到购物车
checkout(basket_id)
```

设计句柄要注意：

- 不透明：用随机 ID，不要把内部结构编码进去，免得被猜测或解析；
- 有权限：句柄只是名字，每次调用都要重新检查调用者能不能操作它；
- 有期限：在创建工具的 description 里写明过期时间；
- 过期可恢复：句柄失效时返回执行错误并说明原因，让模型重新创建。

stdio server 通常是“一个 host 一个进程”，本来就天然隔离。HTTP server 同时服务多个用户，状态必须按用户（按凭据）隔离。

### 3.3 输入校验：server 是信任边界

**信任边界**是数据从不可控的一方进入可控的一方的那条线，过线的数据必须重新检查。对 MCP server 来说，`tools/call` 的参数就是从边界外进来的：

1. **client 不一定守规矩**：协议只是约定，任何人都可以手写一条 JSON-RPC 消息发过来；HTTP server 更是暴露在网络上；
2. **守规矩的 client 也可能被骗**：参数由模型生成，而模型可能受到提示注入影响，读了一篇恶意网页后生成“删除全部文件”的参数；
3. **client 的校验可能不完整**：不同 client 实现的 JSON Schema 关键字不同，有的根本不校验；
4. **只有 server 知道执行意味着什么**：schema 只能表达类型和格式，路径是否越界、金额是否超限、用户有没有权限，只有执行方的代码能判断。

所以 client 端的校验只是提前反馈、节省往返，server 端的校验才是安全保证。规范把“server 必须校验所有工具输入”写成了 MUST。

这和长期记忆的写入规则是同一个思想：“模型决定要不要记，程序决定能不能记”。提出请求的一方可以有自己的判断，**放不放行由执行方的代码决定**。

#### 校验的三层

| 层 | 检查什么 | 拦下的例子 | 怎样实现 |
| --- | --- | --- | --- |
| 结构 | 按 inputSchema：类型、必填项、长度、未知字段 | `{"expression": 42}`、`{}`、多出 `"shell"` 字段 | 用 JSON Schema 校验器统一检查 |
| 取参 | handler 取参时确认参数存在且类型正确；非空等约束需另写 | schema 校验被关闭，或 schema 写漏了约束 | 显式取参，不直接把整个 map 传下去 |
| 语义 | 业务含义是否允许 | 表达式里调用了不允许的函数；路径跳出了允许目录；金额超出上限 | 白名单、解析后再判断、按权限检查 |

第一层能挡住大部分格式问题，但它只看形状，不懂含义：`"os.Exit(1)"` 是一个长度合法的字符串，schema 完全无法拦它。真正的防线在第三层，而且第三层应当用白名单（只允许已知安全的东西），不要用黑名单（列出已知危险的东西），因为危险的写法列不完。

### 3.4 安全伏笔：最小权限

server 是真正执行代码、访问数据的地方，一旦被滥用，造成的破坏以它拥有的权限为上限。所以：

- **只给需要的权限**：只读就不给写；只需要一个目录，就不给整个家目录；只需要一个数据库表，就用只读账号连这张表；
- **不继承多余的凭据**：子进程默认继承父进程的全部环境变量，里面可能有模型 API 密钥、云服务凭据。启动 server 时应只传入它需要的变量；
- **不用高权限用户运行**：不用 root，不放在能访问生产环境的机器上调试；
- **限制资源**：超时、输入长度上限、调用频率限制（规范要求 server 做 rate limit），防止一个调用耗尽机器；
- **输出也要处理**：返回结果前去掉不该外泄的信息（内部路径、密钥、其他用户的数据）。规范要求 server 对输出做清理。

更彻底的隔离（容器、沙箱、资源配额）是 Week 2 的内容。

## 4. stdio server 的实现要点

- **stdout 只写协议消息**：任何调试打印都会破坏协议流，日志写 stderr；
- **读到 stdin EOF 就退出**：这是 client 通知关闭的标准方式；
- **一条消息一行**：JSON 序列化时不能有换行，大多数库的默认输出就满足；
- **不要因为一个请求崩溃**：handler 里的 panic 或异常要捕获并转成错误结果，否则整个 server 退出，所有工具都不可用；
- **并发处理请求**：同一条连接上可能同时有多个请求在途，不要假设请求严格串行。

## 5. 自测

**1. 一个 server 只提供 tools，却声明了 resources 能力，会发生什么？**
client 会认为它支持资源，发来 `resources/list`，server 只能报错或不回复。能力声明应当和实际实现一致。

**2. 模型调用 `transfer(amount: -100)`，server 应返回什么？**
工具执行错误：`isError: true`，`content` 说明金额必须为正数。模型改正参数后可能成功，所以不该用 JSON-RPC 错误。

**3. 为什么说“执行任意 shell 命令”这样的万能工具难以审计？**
所有操作都是同一个工具、同一个字符串参数，无法按操作授权、确认或统计；参数是任意文本，也无法做结构化校验。

**4. 一个浏览器自动化 server 需要记住打开的页面，按新版协议应怎样设计？**
`open_page` 返回不透明的页面句柄，后续 `click`、`read` 都把句柄作为参数传入；server 每次检查句柄是否存在、调用者是否有权访问，并在 description 里写明过期时间。

**5. client 已经按 inputSchema 校验过参数，server 为什么还要再校验？**
server 是信任边界：请求可能来自手写或恶意的 client，参数可能由被注入的模型生成，client 的校验实现也可能不完整；而且 schema 只能检查形状，业务含义只有 server 能判断。

**6. schema 校验已经开启，handler 里的取参检查还有必要吗？**
有必要。schema 校验通常是可以关掉的选项，schema 本身也可能写漏约束；handler 显式取参保证即使第一层失效，错误类型的值也不会流进业务逻辑。

**7. 一个只读文件的 server，以开发者本人身份运行，继承了全部环境变量，有什么问题？**
它能读到开发者能读的所有文件，以及环境变量里的所有凭据，远超“读几个文件”所需的权限。一旦被注入或本身有漏洞，泄露范围就是这些。应限定可访问的目录，只传入必要的环境变量。

## 参考

- Model Context Protocol, [Tools（2026-07-28）](https://modelcontextprotocol.io/specification/2026-07-28/server/tools)：结果格式、错误处理、Stateful Tools 与安全要求
- Model Context Protocol, [Key Changes（2025-11-25）](https://modelcontextprotocol.io/specification/2025-11-25/changelog)：SEP-1303，参数校验错误作为工具执行错误返回
- Model Context Protocol, [Security Best Practices](https://modelcontextprotocol.io/specification/draft/basic/security_best_practices)
- Model Context Protocol, [stdio transport（2026-07-28）](https://modelcontextprotocol.io/specification/2026-07-28/basic/transports/stdio)
- [JSON-RPC 2.0 Specification](https://www.jsonrpc.org/specification)
- [mcp-go](https://github.com/mark3labs/mcp-go)：`server.NewMCPServer`、`server.ServeStdio`、`WithInputSchemaValidation`
- Saltzer & Schroeder, [The Protection of Information in Computer Systems](https://www.cs.virginia.edu/~evans/cs551/saltzer/), 1975（最小权限原则的出处）

本课程的代码实现、动手步骤与实验记录见 [Day 7 项目实践](day-07-lab.md)。
