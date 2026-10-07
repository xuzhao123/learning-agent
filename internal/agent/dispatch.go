package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"learning-agent/internal/llm"
	"learning-agent/internal/mcp"
	"learning-agent/internal/memory"
	"learning-agent/internal/retrieval"
	"learning-agent/internal/skills"
	"learning-agent/internal/tools"
)

// 按工具名分发：检索、记忆、skill 与 MCP 在各自的包里；内置工具和 Day 2/3 实验工具交给 tools.Run。
func runTool(ctx context.Context, call llm.ToolCall) (result any, err error) {
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
		if !retrieval.Enabled {
			return nil, errors.New("请使用 -rag 开启知识库检索")
		}
		args := struct {
			Query string `json:"query"`
			K     int    `json:"k"`
		}{K: 3}
		if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil {
			return nil, errors.New("query 需要是字符串，k 需要是整数")
		}
		return retrieval.Search(ctx, args.Query, args.K)
	case "search_memory", "forget_memory", "remember_memory":
		if memory.Active == nil {
			return nil, errors.New("请使用 -memory 开启长期记忆")
		}
		var args struct{ Query, ID, Reason, Quote, Kind string }
		if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil {
			return nil, errors.New("参数需要是字符串")
		}
		if call.Function.Name == "remember_memory" {
			return memory.Active.Remember(args.Quote, args.Kind)
		}
		if call.Function.Name == "search_memory" {
			if strings.TrimSpace(args.Query) == "" {
				return nil, errors.New("query 需要是非空字符串")
			}
			return memory.Active.Search(ctx, args.Query)
		}
		if strings.TrimSpace(args.ID) == "" || strings.TrimSpace(args.Reason) == "" {
			return nil, errors.New("id 和 reason 需要是非空字符串")
		}
		return memory.Active.Forget(strings.TrimSpace(args.ID), args.Reason)
	case "load_skill":
		if skills.Index == nil {
			return nil, errors.New("请使用 -skills 开启 skill")
		}
		var args struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil {
			return nil, errors.New("name 需要是字符串")
		}
		return skills.Load(strings.TrimSpace(args.Name))
	}
	for _, conn := range mcp.Conns {
		if conn.Owns(call.Function.Name) {
			return conn.Call(ctx, call.Function.Name, call.Function.Arguments)
		}
	}
	return tools.Run(ctx, call.Function.Name, call.Function.Arguments)
}
