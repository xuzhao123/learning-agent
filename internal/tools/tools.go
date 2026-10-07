package tools

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

	"learning-agent/internal/llm"
)

var Definitions = []map[string]any{
	{"name": "calculator", "description": "计算四则运算、括号、sqrt 和 floor，返回 result 数值。", "parameters": llm.Parameters("expression")},
	{"name": "get_current_datetime", "description": "返回 Asia/Shanghai 当前日期、时间和中文星期，无需参数。", "parameters": llm.Parameters("")},
	{"name": "search_notes", "description": "在项目实际学习笔记中检索关键词，返回最多5条匹配内容及来源。", "parameters": llm.Parameters("query")},
}

// 仅在显式 -lab-tools 时注册；故障行为是 Day 2 实验，模型调用仍由真实 API 生成。
var LabEnabled bool

// Day 3 上下文实验的大输出工具 read_day3_notes，只在 -context-lab 中启用。
var ContextLabEnabled bool

var LabDefinitions = []map[string]any{
	{"name": "always_fail", "description": "Day 2 故障实验：执行一次故障操作，可能返回错误，无需参数。", "parameters": llm.Parameters("")},
	{"name": "check_task_status", "description": "查询任务状态；pending 表示还未完成，需要使用相同 task_id 继续查询。", "parameters": llm.Parameters("task_id")},
	{"name": "slow_job", "description": "执行一个耗时 seconds 秒（1到600）的后台作业，完成后返回开始与结束时间。作业有副作用：每执行一次就算一次。", "parameters": llm.Parameters("seconds")},
}

// Run 执行内置工具与 Day 2/3 实验工具；未知名字返回错误。
// 调用方（agent）负责 panic 恢复、参数是否为 JSON 对象的检查与重试。
func Run(ctx context.Context, name, arguments string) (any, error) {
	switch name {
	case "read_day3_notes":
		if !ContextLabEnabled {
			return nil, llm.Permanent(errors.New("上下文实验工具未启用"))
		}
		data, err := os.ReadFile("docs/day-03/day-03-notes.md")
		if err != nil {
			return nil, errors.New("无法读取Day 3学习笔记")
		}
		return map[string]string{"file": "docs/day-03/day-03-notes.md", "content": string(data)}, nil
	case "always_fail", "check_task_status", "slow_job":
		if !LabEnabled {
			return nil, llm.Permanent(errors.New("故障实验工具未启用"))
		}
		if name == "always_fail" {
			panic("Day 2 故障注入：工具始终失败")
		}
		if name == "slow_job" {
			return slowJob(ctx, arguments)
		}
		var args struct {
			TaskID string `json:"task_id"`
		}
		if err := json.Unmarshal([]byte(arguments), &args); err != nil || strings.TrimSpace(args.TaskID) == "" {
			return nil, llm.Permanent(errors.New("task_id 需要是非空字符串"))
		}
		// 捣乱工具故意永远 pending，用来观察真实模型重复调用时，循环是否强制熔断。
		return map[string]string{"task_id": args.TaskID, "status": "pending", "message": "尚未完成，请用相同 task_id 再次查询。"}, nil
	case "calculator":
		var args struct {
			Expression string `json:"expression"`
		}
		if err := json.Unmarshal([]byte(arguments), &args); err != nil {
			return nil, llm.Permanent(errors.New("expression 需要是字符串"))
		}
		value, err := Calculate(args.Expression)
		if err != nil {
			return nil, llm.Permanent(err) // 表达式本身有问题，重算也一样
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
		if err := json.Unmarshal([]byte(arguments), &args); err != nil || strings.TrimSpace(args.Query) == "" {
			return nil, llm.Permanent(errors.New("query 需要是非空字符串"))
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
		return nil, llm.Permanent(fmt.Errorf("未知工具：%s", name))
	}
}

// Day 8 实验：真的等待指定秒数，并在等待期间响应取消。用来观察单次超时、整次运行超时、Ctrl+C 与 kill -9。
func slowJob(ctx context.Context, arguments string) (any, error) {
	var args struct {
		Seconds string `json:"seconds"`
	}
	_ = json.Unmarshal([]byte(arguments), &args)
	seconds, err := strconv.Atoi(strings.TrimSpace(args.Seconds))
	if err != nil || seconds < 1 || seconds > 600 {
		return nil, llm.Permanent(errors.New("seconds 需要是1到600的整数字符串"))
	}
	started := time.Now().In(shanghai)
	fmt.Printf("Slow job: start seconds=%d\n", seconds)
	timer := time.NewTimer(time.Duration(seconds) * time.Second)
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-ctx.Done():
		fmt.Printf("Slow job: interrupted after=%s\n", time.Since(started).Round(time.Millisecond))
		return nil, ctx.Err()
	}
	return map[string]any{"seconds": seconds, "started": started.Format(time.RFC3339), "finished": time.Now().In(shanghai).Format(time.RFC3339)}, nil
}

// 只解释数学 AST；不会执行表达式里出现的 Go 代码。
func Calculate(expression string) (float64, error) {
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

// 与记忆模块相同的固定东八区；各自定义，工具包不依赖记忆包。
var shanghai = time.FixedZone("Asia/Shanghai", 8*60*60)
