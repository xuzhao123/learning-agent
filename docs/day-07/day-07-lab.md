# Day 7 项目实践：把 calculate 暴露成 MCP server，跑通自举闭环

原理见 [Day 7 学习笔记](day-07-notes.md)。Day 6 的 client 见 [Day 6 项目实践](../day-06/day-06-lab.md)。代码在 [mcp.go](../../internal/mcp/mcp.go) 的 `mcp.Serve`。

## 1. 实现概览

server 不是一个单独的程序，而是 agent 的一种运行模式：`learning-agent -mcp-serve`。这样可以直接复用 [tools.go](../../internal/tools/tools.go) 里现有的 `tools.Calculate`，不需要为了跨程序共享而把它挪进单独的包。

```text
learning-agent（client 模式）                     learning-agent -mcp-serve（server 子进程）
  模型 → tool_calls: mcp_calculator                 stdin ─▶ mcp-go ServeStdio
  runTool → conn.Call ───tools/call──stdio──▶      ① schema 校验（类型/必填/长度/多余字段）
                                                     ② handler: RequireString("expression")
                                                     ③ calculate：只解释数学 AST
  ◀── content: "2647", structured: {result: 2647} ◀── stdout；日志写 stderr
```

| 设计点 | 本项目的选择 |
| --- | --- |
| 工具 | `calculator`，参数 `expression`（必填字符串，最长 1024），描述与本地工具一致 |
| 能力 | 只声明 `tools`，`listChanged: false`（工具固定不变） |
| 校验 | `WithInputSchemaValidation()` 开启 schema 校验；`WithStrictInputSchemaDefault()` 自动加 `additionalProperties: false`；handler 显式取参；`tools.Calculate` 白名单解释 |
| 结果 | `NewToolResultStructured`：`structuredContent: {"result": 值}`，同时 `content` 里放文本 |
| 错误 | 参数和表达式错误都用 `NewToolResultError` 返回 `isError: true`；未知工具由 SDK 返回 JSON-RPC -32602 |
| 崩溃保护 | `WithRecovery()`：handler panic 不会让 server 退出 |
| stdout | `-mcp-serve` 在 `flag.Parse()` 之后立刻分流，之前没有任何打印；且必须单独使用，不读 `.env`，不初始化模型 |
| 日志 | 每次调用在 stderr 打印一行 `[mcp-serve] calculator ok/error/rejected`；client 把它原样转到自己的 stderr，所以在 agent 终端里能看到 server 侧发生了什么 |
| 状态 | 纯函数，无状态；每个 client 启动自己的子进程 |

## 2. 读代码

`mcp.Serve` 一共 30 多行，按顺序看：

1. `server.NewMCPServer(名字, 版本, 选项...)`：名字和版本会出现在 discover/initialize 的 serverInfo 里；
2. `mcp.NewTool("calculator", mcp.WithString("expression", mcp.Required(), mcp.MaxLength(1024), …))`：用 Go 代码生成 inputSchema，`tools/list` 返回的就是它；
3. `s.AddTool(tool, handler)`：handler 收到 `CallToolRequest`，返回 `*CallToolResult`；**返回 Go error 会变成 JSON-RPC 错误**，所以可让模型改正的错误都用 `NewToolResultError` 包成结果返回；
4. `server.ServeStdio(s, …)`：阻塞读 stdin，直到 EOF 或收到 SIGTERM/SIGINT。

## 3. 动手

```sh
go build -o bin/learning-agent .
```

### 3.1 自举闭环：Day 6 的 client → 自己的 server

```sh
./bin/learning-agent -mcp-server 'bin/learning-agent -mcp-serve' -mcp-list \
  -mcp-call calculator -mcp-args '{"expression":"floor(sqrt(1234*5678))"}'
```

真实输出（stdout 与 server 的 stderr 交错显示）：

```text
MCP connect: command=bin/learning-agent -mcp-serve
[mcp-serve] 11:54:48 serving calculator over stdio
MCP initialize: protocol=2026-07-28 server=learning-agent-calculator/0.7.0 capabilities={"tools":{}}
MCP tools/list: count=1
  1. calculator — 计算四则运算、括号、sqrt 和 floor，返回 result 数值。
     inputSchema={"additionalProperties":false,"properties":{"expression":{"description":"数学表达式，例如 (1234*5678) 或 floor(sqrt(2))","maxLength":1024,"type":"string"}},"required":["expression"],"type":"object"}
[mcp-serve] 11:54:48 calculator ok: expression="floor(sqrt(1234*5678))" result=2647
MCP tools/call: name=calculator isError=false result={"_meta":{"io.modelcontextprotocol/serverInfo":{"name":"learning-agent-calculator","version":"0.7.0"}},"content":[{"type":"text","text":"2647"}],"resultType":"complete","structuredContent":{"result":2647}}
```

1234 × 5678 = 7006652，开方约 2647.008，取整 2647，结果正确。inputSchema 里的 `additionalProperties: false` 是 `WithStrictInputSchemaDefault` 自动加上的。

### 3.2 信任边界：故意发错误的参数

模型生成的参数通常是合法的。要观察 server 端校验，需要绕过模型，用 Day 6 的手写 client 直接发请求。以下参数都从命令行传入：

```sh
S='bin/learning-agent -mcp-serve'
./bin/learning-agent -mcp-server "$S" -mcp-raw modern -mcp-call calculator -mcp-args '{"expression":42}'
./bin/learning-agent -mcp-server "$S" -mcp-raw modern -mcp-call calculator -mcp-args '{"expression":"1+1","shell":"rm -rf /"}'
./bin/learning-agent -mcp-server "$S" -mcp-raw modern -mcp-call calculator -mcp-args '{"expression":"os.Exit(1)"}'
./bin/learning-agent -mcp-server "$S" -mcp-raw modern -mcp-call calculator -mcp-args '{}'
./bin/learning-agent -mcp-server "$S" -mcp-raw modern -mcp-call rm_rf -mcp-args '{}'
```

真实结果（只保留 id=3 的响应文本）：

| 参数 | 被哪一层拦下 | server 返回 |
| --- | --- | --- |
| `{"expression":42}` | ① schema | `isError: true`，`input schema validation failed: /expression: &{Got:number Want:[string]}` |
| `{"expression":"1+1","shell":"rm -rf /"}` | ① schema（多余字段） | `isError: true`，`input schema validation failed: <root>: &{Properties:[shell]}` |
| `{"expression":"os.Exit(1)"}` | ③ calculate | `isError: true`，`只支持数值、四则运算、括号、sqrt 和 floor`；server 日志 `calculator error: expression="os.Exit(1)"` |
| `{}` | ① schema（缺必填） | `isError: true`，`input schema validation failed: <root>: &{Missing:[expression]}` |
| 工具名 `rm_rf` | SDK 路由 | JSON-RPC `error`：`{"code":-32602,"message":"tool 'rm_rf' not found: tool not found"}` |

从这张表可以看到：

- 前四种都是**工具执行错误**，模型改正参数后有机会成功；只有工具不存在是**协议错误**。这正是 SEP-1303 的划分；
- `os.Exit(1)` 是长度合法的字符串，schema 拦不住，只有懂语义的第三层能拦。所以只靠 schema 不够；
- 这次实验里第二层（handler 取参）没有触发，因为第一层已经挡住了。它的作用是在 schema 校验被关闭或 schema 写漏约束时兜底；
- mcp-go 的 schema 错误信息是 Go 结构体格式（`&{Got:number Want:[string]}`），模型大致能看懂，但不够友好。

### 3.3 终极验证：agent → 自己的 MCP server → calculate

```sh
./bin/learning-agent -mcp-server 'bin/learning-agent -mcp-serve' -max-steps 5 \
  -question '请只用 MCP server 提供的计算器（不要用本地 calculator）计算 (1234×5678) 开根号再取整。'
```

真实运行（方舟 doubao-seed-2-1-pro，省略 Context 行）：

```text
MCP connect: command=bin/learning-agent -mcp-serve
[mcp-serve] 11:55:06 serving calculator over stdio
MCP initialize: protocol=2026-07-28 server=learning-agent-calculator/0.7.0 capabilities={"tools":{}}
MCP tools/list: count=1
MCP register: calculator → mcp_calculator

Round 1
Usage: purpose=main prompt_tokens=739 completion_tokens=173 cached_tokens=0
Action [call_oohu…]: mcp_calculator
Action Input: {"expression": "floor(sqrt(1234*5678))"}
[mcp-serve] 11:55:11 calculator ok: expression="floor(sqrt(1234*5678))" result=2647
Observation [call_oohu…]: {…"tool":"mcp_calculator","result":{"content":["2647"],"structured":{"result":2647}},"attempts":1}

Round 2
计算结果为 **2647**。
Termination: no_tool_calls
```

`[mcp-serve]` 那一行来自 server 子进程，证明这次计算发生在另一个进程里，经过了完整的 MCP 往返。问题里明确要求不用本地 calculator：本地和 MCP 的计算器同时存在时，模型可能选任何一个，这时 `mcp_` 前缀起到了区分作用。

### 3.4 第二层取参校验的对照实验

2026-10-07 补充：在临时副本中只移除 `server.WithInputSchemaValidation()`，保留工具 schema、`WithStrictInputSchemaDefault()` 与 handler 的 `RequireString("expression")`。分别编译原实现和该副本，用真实 SDK client 经 stdio 调用同一个 calculator，未请求模型，实验结束后删除临时副本。项目原实现仍开启 schema 校验。

每个版本运行三种输入，共六次调用：

| arguments | schema 开启 | schema 关闭 |
| --- | --- | --- |
| `{"expression":123}` | 第一层返回 `input schema validation failed`，无 handler 的 rejected 日志 | 第二层返回 `argument "expression" is not a string`，有 `calculator rejected` 日志 |
| `{}` | 第一层返回缺少 expression，无 handler 的 rejected 日志 | 第二层返回 `required argument "expression" not found`，有 `calculator rejected` 日志 |
| `{"expression":"1+1"}` | 返回 2，`isError=false` | 返回 2，`isError=false` |

类型错误在关闭 schema 校验后的实际 server 日志：

```text
[mcp-serve] calculator rejected: argument "expression" is not a string
```

这个输入是合法 JSON，但 expression 的类型错误，因此能区分 schema 拦截和 handler 拦截。开启正确的 schema 校验时，两层的存在性和类型检查有重叠；第二层的价值是 handler 对自己的输入要求做明确检查，校验选项关闭或 schema 不完整时仍能兜底。`RequireString` 只检查存在与字符串类型，空字符串不会在这一层被拒绝；它也不替代 schema 的长度和未知字段检查。

## 4. 已知边界

- 第 2 层的存在性和类型检查与正确开启的 schema 校验重叠；现已用关闭 schema 校验的对照实验实际触发，见 3.4。它不是完整的 schema 校验器。
- 没有频率限制和调用超时：`tools.Calculate` 本身很快，输入也有长度上限，暂不需要。规范要求的 rate limit 留到 Week 2。
- server 以当前用户身份运行，并继承 client 的全部环境变量（见 Day 6 已知边界）。`tools.Calculate` 不读文件、不访问网络，所以眼下没有实际风险，但这不是一个好习惯。

## 补充：同一个 server 换成远程传输

server 的定义只有一份（[mcp.go](../../internal/mcp/mcp.go) 的 `newServer`），传输有三种用法：

- `-mcp-serve`：stdio，由 client 启动为子进程。
- `-mcp-serve -mcp-http 127.0.0.1:8091`：Streamable HTTP，独立端口常驻；client 用 `-mcp-server http://127.0.0.1:8091/mcp` 连接（`mcp.IsURL` 分支）。HTTP 模式下 stdout 不再承载协议，日志仍写 stderr。
- 观测台：`mcp.Handler()` 直接挂在观测台的 `/mcp` 上，同一进程、不另开端口，见 [观测台笔记](../observer/observer-notes.md)。
