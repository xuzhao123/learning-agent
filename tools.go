package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"math"
	"os"
	"strconv"
	"strings"
	"time"
)

var toolDefinitions = []map[string]any{
	{"name": "calculator", "description": "计算四则运算、括号、sqrt 和 floor，返回 result 数值。", "parameters": parameters("expression")},
	{"name": "get_current_datetime", "description": "返回 Asia/Shanghai 当前日期、时间和中文星期，无需参数。", "parameters": parameters("")},
	{"name": "search_notes", "description": "在项目实际学习笔记中检索关键词，返回最多5条匹配内容及来源。", "parameters": parameters("query")},
}

// 仅在显式 -lab-tools 时注册；故障行为是 Day 2 实验，模型调用仍由真实 API 生成。
var labToolsEnabled bool
var contextLabEnabled bool
var labToolDefinitions = []map[string]any{
	{"name": "always_fail", "description": "Day 2 故障实验：执行一次故障操作，可能返回错误，无需参数。", "parameters": parameters("")},
	{"name": "check_task_status", "description": "查询任务状态；pending 表示还未完成，需要使用相同 task_id 继续查询。", "parameters": parameters("task_id")},
}

func parameters(name string) map[string]any {
	properties := map[string]any{}
	required := []string{}
	if name != "" {
		properties[name] = map[string]string{"type": "string"}
		required = append(required, name)
	}
	return map[string]any{"type": "object", "properties": properties, "required": required}
}

func runTool(ctx context.Context, call ToolCall) (result any, err error) {
	// Go 的 panic 也在单次工具执行边界转为 error，交给重试与 Observation 处理。
	defer func() {
		if recovered := recover(); recovered != nil {
			result, err = nil, fmt.Errorf("工具 panic：%v", recovered)
		}
	}()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !json.Valid([]byte(call.Function.Arguments)) || !strings.HasPrefix(strings.TrimSpace(call.Function.Arguments), "{") {
		return nil, errors.New("工具参数需要是有效 JSON 对象")
	}
	switch call.Function.Name {
	case "search_docs":
		if !ragEnabled {
			return nil, errors.New("请使用 -rag 开启知识库检索")
		}
		args := struct {
			Query string `json:"query"`
			K     int    `json:"k"`
		}{K: 3}
		if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil {
			return nil, errors.New("query 需要是字符串，k 需要是整数")
		}
		return searchDocs(ctx, args.Query, args.K)
	case "search_memory", "forget_memory", "remember_memory":
		if memories == nil {
			return nil, errors.New("请使用 -memory 开启长期记忆")
		}
		var args struct{ Query, ID, Reason, Quote, Kind string }
		if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil {
			return nil, errors.New("参数需要是字符串")
		}
		if call.Function.Name == "remember_memory" {
			return memories.remember(args.Quote, args.Kind)
		}
		if call.Function.Name == "search_memory" {
			if strings.TrimSpace(args.Query) == "" {
				return nil, errors.New("query 需要是非空字符串")
			}
			return memories.search(ctx, args.Query)
		}
		if strings.TrimSpace(args.ID) == "" || strings.TrimSpace(args.Reason) == "" {
			return nil, errors.New("id 和 reason 需要是非空字符串")
		}
		return memories.forget(strings.TrimSpace(args.ID), args.Reason)
	case "read_day3_notes":
		if !contextLabEnabled {
			return nil, errors.New("上下文实验工具未启用")
		}
		data, err := os.ReadFile("docs/day-03/day-03-notes.md")
		if err != nil {
			return nil, errors.New("无法读取Day 3学习笔记")
		}
		return map[string]string{"file": "docs/day-03/day-03-notes.md", "content": string(data)}, nil
	case "always_fail", "check_task_status":
		if !labToolsEnabled {
			return nil, errors.New("故障实验工具未启用")
		}
		if call.Function.Name == "always_fail" {
			panic("Day 2 故障注入：工具始终失败")
		}
		var args struct {
			TaskID string `json:"task_id"`
		}
		if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil || strings.TrimSpace(args.TaskID) == "" {
			return nil, errors.New("task_id 需要是非空字符串")
		}
		// 捣乱工具故意永远 pending，用来观察真实模型重复调用时，循环是否强制熔断。
		return map[string]string{"task_id": args.TaskID, "status": "pending", "message": "尚未完成，请用相同 task_id 再次查询。"}, nil
	case "calculator":
		var args struct {
			Expression string `json:"expression"`
		}
		if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil {
			return nil, errors.New("expression 需要是字符串")
		}
		value, err := calculate(args.Expression)
		if err != nil {
			return nil, err
		}
		return map[string]any{"result": value}, nil
	case "get_current_datetime":
		now := time.Now().In(shanghai)
		weekdays := []string{"星期日", "星期一", "星期二", "星期三", "星期四", "星期五", "星期六"}
		return map[string]any{"datetime": now.Format(time.RFC3339), "date": now.Format("2006-01-02"), "weekday": weekdays[now.Weekday()]}, nil
	case "search_notes":
		var args struct {
			Query string `json:"query"`
		}
		if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil || strings.TrimSpace(args.Query) == "" {
			return nil, errors.New("query 需要是非空字符串")
		}
		const path = "docs/day-01/day-01-notes.md"
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, errors.New("无法读取项目学习笔记")
		}
		matches := []string{}
		for i, line := range strings.Split(string(data), "\n") {
			if strings.Contains(strings.ToLower(line), strings.ToLower(strings.TrimSpace(args.Query))) {
				matches = append(matches, fmt.Sprintf("第%d行：%s", i+1, line))
				if len(matches) == 5 {
					break
				}
			}
		}
		return map[string]any{"file": path, "matches": matches}, nil
	default:
		return nil, fmt.Errorf("未知工具：%s", call.Function.Name)
	}
}

// 只解释数学 AST；不会执行表达式里出现的 Go 代码。
func calculate(expression string) (float64, error) {
	if expression == "" || len(expression) > 1024 {
		return 0, errors.New("expression 需要是1到1024字节的数学表达式")
	}
	node, err := parser.ParseExpr(expression)
	if err != nil {
		return 0, errors.New("无法解析数学表达式")
	}
	value, err := evaluate(node)
	if err == nil && (math.IsNaN(value) || math.IsInf(value, 0)) {
		return 0, errors.New("结果不是有限数值")
	}
	return value, err
}

func evaluate(node ast.Expr) (float64, error) {
	switch n := node.(type) {
	case *ast.BasicLit:
		if n.Kind == token.INT || n.Kind == token.FLOAT {
			return strconv.ParseFloat(n.Value, 64)
		}
	case *ast.ParenExpr:
		return evaluate(n.X)
	case *ast.UnaryExpr:
		value, err := evaluate(n.X)
		if err != nil {
			return 0, err
		}
		if n.Op == token.ADD {
			return value, nil
		}
		if n.Op == token.SUB {
			return -value, nil
		}
	case *ast.BinaryExpr:
		left, err := evaluate(n.X)
		if err != nil {
			return 0, err
		}
		right, err := evaluate(n.Y)
		if err != nil {
			return 0, err
		}
		switch n.Op {
		case token.ADD:
			return left + right, nil
		case token.SUB:
			return left - right, nil
		case token.MUL:
			return left * right, nil
		case token.QUO:
			if right == 0 {
				return 0, errors.New("除数不能为0")
			}
			return left / right, nil
		}
	case *ast.CallExpr:
		name, ok := n.Fun.(*ast.Ident)
		if !ok || len(n.Args) != 1 || n.Ellipsis.IsValid() {
			break
		}
		value, err := evaluate(n.Args[0])
		if err != nil {
			return 0, err
		}
		if name.Name == "sqrt" {
			return math.Sqrt(value), nil
		}
		if name.Name == "floor" {
			return math.Floor(value), nil
		}
	}
	return 0, errors.New("只支持数值、四则运算、括号、sqrt 和 floor")
}
