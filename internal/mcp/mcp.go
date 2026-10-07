package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	"learning-agent/internal/llm"
	"learning-agent/internal/tools"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/client/transport"
	sdk "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// Day 6：-mcp-server 启动的 MCP 连接，每个 server 一个；空表示未启用。
var Conns []*Connection

type Connection struct {
	client *client.Client
	tools  map[string]string // agent 侧工具名 → server 上的原名
}

// -mcp-server 的值是 http(s) 地址时连远程 server（Streamable HTTP），否则当作 stdio 子进程命令。
func IsURL(value string) bool {
	return strings.HasPrefix(value, "http://") || strings.HasPrefix(value, "https://")
}

// 建立传输后完成连接：mcp-go 先发 server/discover（2026-07-28 起的无状态协议），
// 服务端不认识时退回 initialize 握手；两种方式交换的都是协议版本与 capabilities。
// 两种传输之后的协议消息完全相同，区别只在消息怎么送达：stdio 写子进程 stdin，HTTP 每条消息一个 POST。
func Connect(ctx context.Context, command string) (*Connection, []sdk.Tool, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	var c *client.Client
	var err error
	if IsURL(command) {
		fmt.Printf("MCP connect: url=%s\n", command)
		// 远程 server 不是本进程启动的，Close 只断开连接，不会结束对方。
		if c, err = client.NewStreamableHttpClient(command); err == nil {
			err = c.Start(ctx)
		}
		if err != nil {
			return nil, nil, fmt.Errorf("无法连接远程 MCP server：%w", err)
		}
	} else {
		fields := strings.Fields(command)
		if len(fields) == 0 {
			return nil, nil, errors.New("mcp-server 需要一条命令或 http(s) 地址")
		}
		fmt.Printf("MCP connect: command=%s\n", command)
		// server 的 stderr 是它的日志通道，原样转到本进程 stderr；stdout 只承载协议消息。
		c, err = client.NewStdioMCPClientWithOptions(fields[0], nil, fields[1:], transport.WithCommandStderrWriter(os.Stderr))
		if err != nil {
			return nil, nil, fmt.Errorf("无法启动 MCP server：%w", err)
		}
	}
	request := sdk.InitializeRequest{}
	request.Params.ProtocolVersion = sdk.LATEST_PROTOCOL_VERSION
	request.Params.ClientInfo = sdk.Implementation{Name: "learning-agent", Version: "0.6.0"}
	info, err := c.Initialize(ctx, request)
	if err != nil {
		c.Close()
		return nil, nil, fmt.Errorf("MCP 连接失败：%w", err)
	}
	capabilities, _ := json.Marshal(info.Capabilities)
	fmt.Printf("MCP initialize: protocol=%s server=%s/%s capabilities=%s\n", c.ProtocolVersion(), info.ServerInfo.Name, info.ServerInfo.Version, capabilities)
	if info.Capabilities.Tools == nil {
		c.Close()
		return nil, nil, errors.New("MCP server 没有声明 tools capability")
	}
	listed, err := c.ListTools(ctx, sdk.ListToolsRequest{})
	if err != nil {
		c.Close()
		return nil, nil, fmt.Errorf("tools/list 失败：%w", err)
	}
	fmt.Printf("MCP tools/list: count=%d\n", len(listed.Tools))
	return &Connection{client: c, tools: map[string]string{}}, listed.Tools, nil
}

var invalidToolChars = regexp.MustCompile(`[^A-Za-z0-9_-]`)

// MCP tool → 方舟 function 定义：name 加 mcp_ 前缀避免和本地工具重名（本地也有 calculator），
// inputSchema 本身就是 JSON Schema，原样作为 parameters。
func (m *Connection) Definitions(tools []sdk.Tool) []map[string]any {
	taken := map[string]bool{}
	for _, definition := range llm.Tools {
		taken[definition["name"].(string)] = true
	}
	definitions := []map[string]any{}
	for _, tool := range tools {
		// 方舟函数名只允许字母、数字、_、-，最长64；MCP 还允许“.”，替换掉。
		name := "mcp_" + invalidToolChars.ReplaceAllString(tool.Name, "_")
		if len(name) > 64 || taken[name] {
			fmt.Printf("MCP skip: tool=%s reason=名称过长或与已有工具重名\n", tool.Name)
			continue
		}
		data, err := json.Marshal(tool)
		var decoded struct {
			InputSchema map[string]any `json:"inputSchema"`
		}
		if err != nil || json.Unmarshal(data, &decoded) != nil || decoded.InputSchema == nil {
			fmt.Printf("MCP skip: tool=%s reason=inputSchema 无效\n", tool.Name)
			continue
		}
		taken[name] = true
		m.tools[name] = tool.Name
		definitions = append(definitions, map[string]any{"name": name, "description": tool.Description, "parameters": decoded.InputSchema})
		fmt.Printf("MCP register: %s → %s\n", tool.Name, name)
	}
	return definitions
}

// tools/call 的两种失败分开：协议错误（JSON-RPC error，如未知工具）与工具执行错误（isError: true）。
// 两者都回填给模型；执行错误里通常写着怎么改参数。
func (m *Connection) Call(ctx context.Context, name, arguments string) (any, error) {
	var args map[string]any
	if err := json.Unmarshal([]byte(arguments), &args); err != nil {
		return nil, errors.New("工具参数需要是 JSON 对象")
	}
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	request := sdk.CallToolRequest{}
	request.Params.Name, request.Params.Arguments = m.tools[name], args
	result, err := m.client.CallTool(ctx, request)
	if err != nil {
		return nil, fmt.Errorf("MCP 协议错误：%w", err)
	}
	texts := []string{}
	for _, content := range result.Content {
		if text, ok := sdk.AsTextContent(content); ok {
			texts = append(texts, text.Text)
		} else {
			texts = append(texts, fmt.Sprintf("[非文本内容 %T 未转给模型]", content))
		}
	}
	if result.IsError {
		// server 已经明确拒绝（多为参数错误），原样再发一次结果相同，不重试。
		return nil, llm.Permanent(errors.New("MCP 工具执行错误：" + strings.Join(texts, "\n")))
	}
	output := map[string]any{"content": texts}
	if result.StructuredContent != nil {
		output["structured"] = result.StructuredContent
	}
	return output, nil
}

// Owns 报告 agent 侧工具名是否属于这个 server。
func (m *Connection) Owns(name string) bool { return m.tools[name] != "" }

func (m *Connection) Close() {
	// stdio：Close 关闭子进程 stdin 并等待退出，正常关闭信号就是 EOF；HTTP：只取消进行中的请求。
	if err := m.client.Close(); err != nil {
		fmt.Fprintln(os.Stderr, "MCP close:", err)
	}
}

// Day 6 第2步：只用 SDK 走 连接 → tools/list →（可选）tools/call，不请求模型。
func Inspect(ctx context.Context, command, callName, callArgs string) error {
	conn, tools, err := Connect(ctx, command)
	if err != nil {
		return err
	}
	defer conn.Close()
	for i, tool := range tools {
		schema, _ := json.Marshal(tool.InputSchema)
		fmt.Printf("  %d. %s — %s\n     inputSchema=%s\n", i+1, tool.Name, tool.Description, schema)
	}
	if callName == "" {
		return nil
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(callArgs), &args); err != nil {
		return errors.New("mcp-args 需要是 JSON 对象")
	}
	request := sdk.CallToolRequest{}
	request.Params.Name, request.Params.Arguments = callName, args
	result, err := conn.client.CallTool(ctx, request)
	if err != nil {
		return fmt.Errorf("tools/call 协议错误：%w", err)
	}
	data, _ := json.Marshal(result)
	fmt.Printf("MCP tools/call: name=%s isError=%t result=%s\n", callName, result.IsError, data)
	return nil
}

// Day 6 进阶：不用 SDK，自己拼 JSON-RPC 写进子进程 stdin、逐行读 stdout，看清线上的每一条消息。
// legacy：initialize → notifications/initialized → 请求不带 _meta；
// modern：server/discover，之后每个请求的 _meta 都带协议版本、客户端身份与 capabilities。
func Raw(ctx context.Context, command, era, callName, callArgs string) error {
	fields := strings.Fields(command)
	if len(fields) == 0 {
		return errors.New("mcp-server 需要一条命令")
	}
	if callName != "" && !json.Valid([]byte(callArgs)) {
		return errors.New("mcp-args 需要是有效 JSON（可以故意传错类型，观察 server 端校验）")
	}
	cmd := exec.CommandContext(ctx, fields[0], fields[1:]...)
	cmd.Stderr = os.Stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("无法启动 MCP server：%w", err)
	}
	lines := make(chan string)
	go func() {
		scanner := bufio.NewScanner(stdout)
		scanner.Buffer(make([]byte, 64<<10), 8<<20)
		for scanner.Scan() {
			lines <- scanner.Text()
		}
		close(lines)
	}()
	send := func(message map[string]any) error {
		message["jsonrpc"] = "2.0"
		data, err := json.Marshal(message)
		if err != nil {
			return err
		}
		fmt.Printf("→ %s\n", data)
		// 换行就是 stdio 的消息分隔符，所以单条 JSON 内部不能有换行（Marshal 不会产生）。
		_, err = stdin.Write(append(data, '\n'))
		return err
	}
	// 等待指定 id 的响应；途中收到的通知也打印出来。
	receive := func(id int) (map[string]json.RawMessage, error) {
		timeout := time.After(10 * time.Second)
		for {
			select {
			case line, ok := <-lines:
				if !ok {
					return nil, errors.New("server 关闭了 stdout")
				}
				fmt.Printf("← %s\n", line)
				var message map[string]json.RawMessage
				if json.Unmarshal([]byte(line), &message) != nil {
					return nil, errors.New("server 在 stdout 写了非 JSON-RPC 内容")
				}
				if string(message["id"]) == strconv.Itoa(id) {
					return message, nil
				}
			case <-timeout:
				return nil, fmt.Errorf("等待 id=%d 的响应超时", id)
			}
		}
	}
	request := func(id int, method string, params map[string]any) error {
		if era == "modern" {
			params["_meta"] = map[string]any{
				"io.modelcontextprotocol/protocolVersion":    sdk.ProtocolVersion20260728,
				"io.modelcontextprotocol/clientInfo":         map[string]string{"name": "learning-agent-raw", "version": "0.6.0"},
				"io.modelcontextprotocol/clientCapabilities": map[string]any{},
			}
		}
		if err := send(map[string]any{"id": id, "method": method, "params": params}); err != nil {
			return err
		}
		response, err := receive(id)
		if err == nil && response["error"] != nil {
			fmt.Printf("   ↳ JSON-RPC error：%s\n", response["error"])
		}
		return err
	}
	run := func() error {
		if era == "modern" {
			if err := request(1, "server/discover", map[string]any{}); err != nil {
				return err
			}
		} else {
			// 握手只做一次：客户端报上自己想用的版本和能力，服务端回它选定的版本和能力。
			if err := request(1, "initialize", map[string]any{
				"protocolVersion": sdk.ProtocolVersion20251125,
				"capabilities":    map[string]any{},
				"clientInfo":      map[string]string{"name": "learning-agent-raw", "version": "0.6.0"},
			}); err != nil {
				return err
			}
			// 通知没有 id，也不会有响应。
			if err := send(map[string]any{"method": "notifications/initialized"}); err != nil {
				return err
			}
		}
		if err := request(2, "tools/list", map[string]any{}); err != nil {
			return err
		}
		if callName != "" {
			return request(3, "tools/call", map[string]any{"name": callName, "arguments": json.RawMessage(callArgs)})
		}
		return nil
	}
	runErr := run()
	stdin.Close()
	waitErr := cmd.Wait()
	if runErr != nil {
		return runErr
	}
	if waitErr != nil && ctx.Err() == nil {
		return fmt.Errorf("MCP server 退出异常：%w", waitErr)
	}
	return nil
}

// Day 7：把 calculate 暴露成 calculator 的 MCP server。server 与传输分开：
// Serve 用 stdio 或独立 HTTP 端口运行；Handler 让观测台在自己的 /mcp 上直接提供（同一进程，不转发）。
func newServer(logger *log.Logger) *server.MCPServer {
	s := server.NewMCPServer("learning-agent-calculator", "0.7.0",
		server.WithToolCapabilities(false),
		// 第一道：按 inputSchema 校验（类型、必填、长度、多余字段），失败返回 isError 结果。
		server.WithInputSchemaValidation(),
		server.WithStrictInputSchemaDefault(),
		server.WithRecovery(),
	)
	tool := sdk.NewTool("calculator",
		sdk.WithDescription("计算四则运算、括号、sqrt 和 floor，返回 result 数值。"),
		sdk.WithString("expression", sdk.Required(), sdk.MaxLength(1024), sdk.Description("数学表达式，例如 (1234*5678) 或 floor(sqrt(2))")),
	)
	s.AddTool(tool, func(ctx context.Context, request sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		// 第二道：handler 自己取参。schema 校验是可关的选项，不能当成唯一防线。
		expression, err := request.RequireString("expression")
		if err != nil {
			logger.Printf("calculator rejected: %v", err)
			return sdk.NewToolResultError(err.Error()), nil
		}
		// 第三道：Calculate 只解释数学 AST，限制长度与可用函数。
		value, err := tools.Calculate(expression)
		if err != nil {
			logger.Printf("calculator error: expression=%q error=%v", expression, err)
			return sdk.NewToolResultError(err.Error()), nil
		}
		logger.Printf("calculator ok: expression=%q result=%v", expression, value)
		return sdk.NewToolResultStructured(map[string]any{"result": value}, strconv.FormatFloat(value, 'g', -1, 64)), nil
	})
	return s
}

// -mcp-serve：stdio 模式下 stdout 被协议独占，这条路径上不能有任何 fmt.Print，日志只写 stderr。
// httpAddr 非空时改用 Streamable HTTP，在独立端口提供 /mcp。
func Serve(httpAddr string) error {
	logger := log.New(os.Stderr, "[mcp-serve] ", log.Ltime)
	s := newServer(logger)
	if httpAddr != "" {
		// SDK 默认拒绝“本机连接但 Host 不是本机”的请求（防 DNS 重绑定）。
		logger.Printf("serving calculator over streamable HTTP at http://%s/mcp", httpAddr)
		return server.NewStreamableHTTPServer(s).Start(httpAddr)
	}
	logger.Printf("serving calculator over stdio")
	return server.ServeStdio(s, server.WithErrorLogger(logger))
}

// Handler 是同一个 server 的 Streamable HTTP 入口，观测台挂在自己的 /mcp 上；日志打印到观测台终端。
func Handler() http.Handler {
	return server.NewStreamableHTTPServer(newServer(log.New(os.Stdout, "[mcp] ", log.Ltime)))
}
