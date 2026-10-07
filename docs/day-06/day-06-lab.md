# Day 6 项目实践：写一个 MCP client，把 MCP 工具接进 Agent

原理见 [Day 6 学习笔记](day-06-notes.md)。本文记录本项目怎样实现、怎样动手观察，以及真实运行结果。代码在 [mcp.go](../../mcp.go)，参数入口在 [main.go](../../main.go)。

## 1. 实现概览

```text
                     ┌──────────── learning-agent 进程 ────────────┐
-mcp-list   ───────▶ │ connectMCP：SDK 起子进程 → discover/initialize │──stdio──▶ MCP server 子进程
                     │            → tools/list →（可选）tools/call    │◀─────────  （stderr 原样转到本进程 stderr）
-mcp-raw    ───────▶ │ rawMCP：自己拼 JSON-RPC，逐行打印 → / ←        │
-mcp-server ───────▶ │ connectMCP → definitions：MCP tool → function │
 + -question         │ 模型 tool_calls(mcp_xxx) → runTool → call()  │──tools/call──▶
                     └──────────────────────────────────────────────┘
```

| 设计点 | 本项目的选择 |
| --- | --- |
| SDK | [mcp-go](https://github.com/mark3labs/mcp-go) v1.1.1（用户任务指定）。它要求 Go ≥ 1.25.5，`go.mod` 随之从 1.25.0 升到 1.25.5，本机 Go 版本较低时由 Go 工具链自动下载 |
| 协议版本 | SDK 默认先发 `server/discover` 试探 2026-07-28；对方是旧版就退回 `initialize`（2025-11-25）。日志 `MCP initialize: protocol=…` 打印实际协商结果 |
| 传输 | 只做 stdio。`-mcp-server` 的值按空格切成命令和参数，不经过 shell，所以引号、管道、`~` 都不展开 |
| 工具命名 | 统一加 `mcp_` 前缀，`.` 等字符替换成 `_`，最长 64；和本地工具重名或超长就跳过并打印 `MCP skip`。`-mcp-server` 可以重复给多个，后连的 server 与已注册工具重名同样跳过，所以前缀不含 server 名；某个 server 连不上只打印 `MCP error` 并跳过它 |
| Schema | `inputSchema` 原样作为 function 的 `parameters` |
| 结果 | 文本内容拼成 `content` 数组回填；有 `structuredContent` 时一起放进 `structured`；非文本内容（图片等）只写一行说明，不转给模型 |
| 错误 | JSON-RPC 错误记为“MCP 协议错误”，`isError: true` 记为“MCP 工具执行错误”，两种都作为 tool 结果回填给模型 |
| 超时 | 连接和每次调用各 1 分钟，同时受 agent 自身的取消信号控制 |
| 生命周期 | 启动时连接一次、拉一次工具列表；server 子进程活到本次运行结束，退出时关闭 stdin 并等待它退出 |

## 2. 读代码

建议按调用顺序读 [mcp.go](../../mcp.go)：

1. **`connectMCP`**：`client.NewStdioMCPClientWithOptions` 启动子进程，`WithCommandStderrWriter(os.Stderr)` 把 server 日志转到本进程 stderr；`c.Initialize` 内部先试 discover 再退回 initialize；然后检查 server 是否声明了 `tools` 能力，没有就不必 `tools/list`。这里用的就是 capabilities 协商的结果。
2. **`definitions`**：MCP tool 转 function 定义。先收集已有工具名，避免和本地 `calculator` 冲突；`m.tools` 记住“agent 侧名字 → server 原名”，调用时还原。
3. **`call`**：模型给的参数是 JSON 字符串，解析成 map 后作为 `arguments` 发出；分开处理 `err`（协议错误）和 `result.IsError`（执行错误）。
4. **`rawMCP`**：不依赖 SDK。用 `exec.Cmd` 拿到 stdin/stdout 管道，一个 goroutine 按行读 stdout 写进 channel；`send` 写一行 JSON 加 `\n`；`receive(id)` 等待同一 `id` 的响应（期间收到的通知也打印），10 秒超时。`legacy` 和 `modern` 两种模式的区别只在两处：开场发 `initialize` + `notifications/initialized` 还是 `server/discover`，以及每个请求要不要附带 `_meta`。

接入 agent 的改动很小：[main.go](../../main.go) 在 `runAgent` 之前连接并追加工具定义；[tools.go](../../tools.go) 的 `runTool` 在 `default` 分支里先查 `mcpConn.tools`，命中就转发。并行、重试、熔断、上下文管理都沿用原有逻辑，MCP 工具和本地工具走同一条路径。

## 3. 动手

先构建，并把 mcp-go 自带的示例 server 装到 `bin/`（已忽略提交）：

```sh
go build -o bin/learning-agent .
GOBIN=$PWD/bin go install github.com/mark3labs/mcp-go/examples/everything@v1.1.1
```

### 3.1 SDK client：连接、列工具、调一次

```sh
./bin/learning-agent -mcp-server bin/everything -mcp-list -mcp-call add -mcp-args '{"a":12.5,"b":30}' 2>/dev/null
```

`2>/dev/null` 是为了屏蔽示例 server 打到 stderr 的大量钩子日志。真实输出（工具列表省略了 inputSchema 行）：

```text
MCP connect: command=bin/everything
MCP initialize: protocol=2026-07-28 server=example-servers/everything/1.0.0 capabilities={"logging":{},"prompts":{"listChanged":true},"resources":{"subscribe":true,"listChanged":true},"tools":{"listChanged":true},"completions":{}}
MCP tools/list: count=6
  1. add — Adds two numbers
  2. echo — Echoes back the input
  3. getTinyImage — Returns the MCP_TINY_IMAGE
  4. get_resource_link — Returns a resource link example
  5. longRunningOperation — Demonstrates a long running operation with progress updates
  6. notify —
MCP tools/call: name=add isError=false result={"_meta":{"io.modelcontextprotocol/serverInfo":{"name":"example-servers/everything","version":"1.0.0"}},"content":[{"type":"text","text":"The sum of 12.500000 and 30.000000 is 42.500000."}],"resultType":"complete"}
```

注意 `protocol=2026-07-28`：SDK 和示例 server 都是新版，所以这次连接**没有发生 initialize 握手**。示例 server 的 stderr 日志里能看到第一条请求是 `server/discover`。

### 3.2 手写 JSON-RPC：亲眼看两代协议

```sh
./bin/learning-agent -mcp-server bin/everything -mcp-raw legacy -mcp-call echo -mcp-args '{"message":"你好 MCP"}' 2>/dev/null
./bin/learning-agent -mcp-server bin/everything -mcp-raw modern -mcp-call echo -mcp-args '{"message":"你好 MCP"}' 2>/dev/null
```

legacy（tools/list 的响应截断显示）：

```text
→ {"id":1,"jsonrpc":"2.0","method":"initialize","params":{"capabilities":{},"clientInfo":{"name":"learning-agent-raw","version":"0.6.0"},"protocolVersion":"2025-11-25"}}
← {"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-11-25","capabilities":{"logging":{},"prompts":{"listChanged":true},"resources":{"subscribe":true,"listChanged":true},"tools":{"listChanged":true},"completions":{}},"serverInfo":{"name":"example-servers/everything","version":"1.0.0"}}}
→ {"jsonrpc":"2.0","method":"notifications/initialized"}
→ {"id":2,"jsonrpc":"2.0","method":"tools/list","params":{}}
← {"jsonrpc":"2.0","id":2,"result":{"tools":[{"annotations":{"readOnlyHint":false,"destructiveHint":true,…},"description":"Adds two numbers",…
→ {"id":3,"jsonrpc":"2.0","method":"tools/call","params":{"arguments":{"message":"你好 MCP"},"name":"echo"}}
← {"jsonrpc":"2.0","id":3,"result":{"content":[{"type":"text","text":"Echo: 你好 MCP"}]}}
```

modern：

```text
→ {"id":1,"jsonrpc":"2.0","method":"server/discover","params":{"_meta":{"io.modelcontextprotocol/clientCapabilities":{},"io.modelcontextprotocol/clientInfo":{"name":"learning-agent-raw","version":"0.6.0"},"io.modelcontextprotocol/protocolVersion":"2026-07-28"}}}
← {"jsonrpc":"2.0","id":1,"result":{"_meta":{"io.modelcontextprotocol/serverInfo":{"name":"example-servers/everything","version":"1.0.0"}},"resultType":"complete","ttlMs":0,"cacheScope":"private","supportedVersions":["2026-07-28","2025-11-25","2025-06-18","2025-03-26","2024-11-05"],"capabilities":{…同上…}}}
→ {"id":2,"jsonrpc":"2.0","method":"tools/list","params":{"_meta":{…同上三项…}}}
→ {"id":3,"jsonrpc":"2.0","method":"tools/call","params":{"_meta":{…同上三项…},"arguments":{"message":"你好 MCP"},"name":"echo"}}
← {"jsonrpc":"2.0","id":3,"result":{"_meta":{"io.modelcontextprotocol/serverInfo":{…}},"content":[{"type":"text","text":"Echo: 你好 MCP"}],"resultType":"complete"}}
```

对照两段日志，可以看出：

- legacy 的版本和能力只在 `initialize` 里出现一次，后面的请求 `params` 是空的，server 靠“这条连接已经握过手”来记住它们；
- modern 没有握手，同样的三项信息出现在**每一个请求**的 `_meta` 里；结果多了 `resultType`、`ttlMs`、`cacheScope` 等新版字段；
- `notifications/initialized` 没有 `id`，也没有任何响应；
- 示例 server 两代都支持（dual-era），所以两种方式都能用。

### 3.3 接进 agent

```sh
./bin/learning-agent -mcp-server bin/everything -max-steps 5 \
  -question '请用 MCP server 提供的加法工具计算 12.5 加 30，并用它的 echo 工具回显“MCP 已接通”。两项独立，可以同轮调用。' 2>/dev/null
```

真实运行（方舟 doubao-seed-2-1-pro，省略 Context 行）：

```text
MCP register: add → mcp_add
MCP register: echo → mcp_echo
…（其余 4 个工具）
Round 1
Action [call_5ydb…]: mcp_add
Action Input: {"a": 12.5, "b": 30}
Action [call_kneh…]: mcp_echo
Action Input: {"message": "MCP 已接通"}
Observation [call_5ydb…]: {…"tool":"mcp_add","result":{"content":["The sum of 12.500000 and 30.000000 is 42.500000."]},"attempts":1}
Observation [call_kneh…]: {…"tool":"mcp_echo","result":{"content":["Echo: MCP 已接通"]},"attempts":1}
Round 2
两项工具调用的结果如下：
1.  加法计算：12.5 + 30 = 42.5
2.  回显结果：成功返回「Echo: MCP 已接通」
Termination: no_tool_calls
```

模型在同一轮并行调用了两个 MCP 工具。第一次请求的实际输入是 1010 token，其中大部分是 3 个本地工具加 6 个 MCP 工具的定义，问题本身只有几十 token。作为量级参考：Day 7 的运行只多注册了 1 个 MCP 工具，问题也不同，首轮输入是 739 token。工具越多，每一轮都要多付这部分成本。

## 4. 昨日回顾：本项目“程序决定能不能记”的三道检查

`remember_memory` 被调用时，[memory.go](../../memory.go) 的 `remember` 依次检查：

1. **出处**：引文必须逐字出自本次会话的用户消息（不含摘要、模型回答、工具结果）；
2. **内容**：`rejectReason` 检查长度（≤500 字）和疑似密钥；
3. **频率**：每轮最多登记 3 条。

三道检查都由代码执行，模型只能提出请求。Day 7 的 MCP server 参数校验用的是同一个思路。

## 5. 已知边界

- 只支持一个 MCP server、只支持 stdio；没有处理工具列表变化通知，启动后的工具列表不再更新。
- SDK 启动子进程时会把本进程的**全部环境变量**传给 server（mcp-go 的实现是 `os.Environ()` 加上额外变量）。如果 `ARK_API_KEY` 设在环境变量里，第三方 server 也能读到。这违反最小权限原则；`.env` 里的密钥只在本进程内读取，不会进入环境变量。
- 工具执行错误会走原有的重试逻辑（默认额外重试 2 次）。参数错误是确定性的，重试不会成功，属于 Day 2 提到的“错误分类后再决定是否重试”的缺口。
- 工具结果直接进入模型上下文，只受原有的工具输出长度上限约束，没有专门防范结果里的提示注入。
- 观测台没有接入 `-mcp-server`。
