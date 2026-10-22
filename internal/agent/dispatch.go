package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"learning-agent/internal/browser"
	"learning-agent/internal/llm"
	"learning-agent/internal/mcp"
	"learning-agent/internal/memory"
	"learning-agent/internal/retrieval"
	"learning-agent/internal/sandbox"
	"learning-agent/internal/skills"
	"learning-agent/internal/tools"
	"learning-agent/internal/websearch"
	"learning-agent/internal/workspace"
)

// 按工具名分发：检索、记忆、skill、浏览器与 MCP 在各自的包里；内置工具和 Day 2/3 实验工具交给 tools.Run。
// offered 报告这个工具在不在本轮发给模型的工具列表里。
func offered(name string) bool {
	for _, definition := range llm.Tools {
		if definition["name"] == name {
			return true
		}
	}
	return false
}

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
	// D19：tools 不只是给模型看的菜单，也必须是执行器的允许列表。ExecuteBatch 一开始就查过；续跑补齐中断批次时
	// 不经过它，这里再守一次。外部文字、模型输出和旧检查点都不能给未提供的工具增加执行权限。
	if !offered(call.Function.Name) {
		return nil, llm.Permanent(fmt.Errorf("工具 %s 未列入本轮允许列表，已拒绝执行；请按用户任务使用本轮提供的工具", call.Function.Name))
	}
	if !json.Valid([]byte(call.Function.Arguments)) || !strings.HasPrefix(strings.TrimSpace(call.Function.Arguments), "{") {
		return nil, llm.Permanent(errors.New("工具参数需要是有效 JSON 对象"))
	}
	switch call.Function.Name {
	case "search_docs":
		if !retrieval.Enabled {
			return nil, llm.Permanent(errors.New("请使用 -rag 开启知识库检索"))
		}
		args := struct {
			Query string `json:"query"`
			K     int    `json:"k"`
		}{K: 3}
		if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil {
			return nil, llm.Permanent(errors.New("query 需要是字符串，k 需要是整数"))
		}
		return retrieval.Search(ctx, args.Query, args.K)
	case "search_memory", "forget_memory", "remember_memory":
		if memory.Active == nil {
			return nil, llm.Permanent(errors.New("请使用 -memory 开启长期记忆"))
		}
		var args struct{ Query, ID, Reason, Quote, Kind string }
		if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil {
			return nil, llm.Permanent(errors.New("参数需要是字符串"))
		}
		if call.Function.Name == "remember_memory" {
			return memory.Active.Remember(args.Quote, args.Kind)
		}
		if call.Function.Name == "search_memory" {
			if strings.TrimSpace(args.Query) == "" {
				return nil, llm.Permanent(errors.New("query 需要是非空字符串"))
			}
			return memory.Active.Search(ctx, args.Query)
		}
		if strings.TrimSpace(args.ID) == "" || strings.TrimSpace(args.Reason) == "" {
			return nil, llm.Permanent(errors.New("id 和 reason 需要是非空字符串"))
		}
		return memory.Active.Forget(strings.TrimSpace(args.ID), args.Reason)
	case "load_skill":
		if skills.Index == nil {
			return nil, llm.Permanent(errors.New("请使用 -skills 开启 skill"))
		}
		var args struct {
			Name string `json:"name"`
			File string `json:"file"`
		}
		if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil {
			return nil, llm.Permanent(errors.New("name、file 需要是字符串"))
		}
		return skills.Load(strings.TrimSpace(args.Name), strings.TrimSpace(args.File))
	case "bash":
		if !sandbox.ProjectWrite {
			return sandbox.Run(ctx, call.ID, call.Function.Name, call.Function.Arguments)
		}
		// O15：工作区可写地挂在沙箱里时，前后比一次文件，bash 改了哪些如实告诉模型并记进本轮清单。
		before := workspace.Stamps()
		result, err := sandbox.Run(ctx, call.ID, call.Function.Name, call.Function.Arguments)
		changed := workspace.Changed(before, workspace.Stamps())
		if len(changed) == 0 {
			return result, err
		}
		workspace.NoteBash(changed)
		fmt.Printf("Bash [%s]: workspace_changes=%d\n", call.ID, len(changed))
		if out, ok := result.(map[string]any); ok {
			shown := changed
			if len(shown) > 30 {
				shown = shown[:30]
			}
			out["workspace_changes"] = shown
			out["workspace_note"] = fmt.Sprintf("这条 bash 改了工作区里 %d 个文件。bash 的改动没有快照，撤销这一轮时不会还原，之后再用编码工具改它们，撤销会因冲突被拒；也没经过编辑工具的 hook。改源码请用 edit_file、write_file，bash 用来构建、测试和查看。", len(changed))
		}
		return result, err
	case "request_network_access", "bash_output", "bash_kill":
		return sandbox.Run(ctx, call.ID, call.Function.Name, call.Function.Arguments)
	case "read_file", "glob", "grep", "edit_file", "write_file":
		return workspace.Run(ctx, call.Function.Name, call.Function.Arguments)
	case "web_search", "open_page":
		return browser.Run(ctx, call.ID, call.Function.Name, call.Function.Arguments)
	case "ds_search", "ds_fetch":
		return websearch.Run(ctx, call.Function.Name, call.Function.Arguments)
	case "read_output":
		return readOutput(call.Function.Arguments)
	case "view_image":
		return viewImage(call.Function.Arguments)
	case "progress":
		return progressTool(call.Function.Arguments)
	case "verify_feature":
		return verifyFeature(ctx, call)
	case "ask_user":
		return askUser(ctx, call)
	case "todo_write":
		return todoWrite(call.Function.Arguments)
	case "agent_wait", "agent_send", "agent_stop":
		if SubagentArgs == nil {
			return nil, llm.Permanent(errors.New("请使用 -subagents 开启子 agent"))
		}
		if call.Function.Name == "agent_wait" {
			return agentWait(ctx, call.Function.Arguments)
		}
		return agentControl(ctx, call.Function.Name, call.Function.Arguments)
	case "spawn_agent":
		if SubagentArgs == nil {
			return nil, llm.Permanent(errors.New("请使用 -subagents 开启子 agent"))
		}
		return spawnAgent(ctx, call)
	}
	for _, conn := range mcp.Conns {
		if conn.Owns(call.Function.Name) {
			return conn.Call(ctx, call.Function.Name, call.Function.Arguments)
		}
	}
	return tools.Run(ctx, call.Function.Name, call.Function.Arguments)
}
