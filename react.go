package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
)

type Message struct {
	Role             string     `json:"role"`
	Content          string     `json:"content"`
	EncryptedContent string     `json:"encrypted_content,omitempty"`
	ReasoningContent string     `json:"reasoning_content,omitempty"`
	ToolCalls        []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID       string     `json:"tool_call_id,omitempty"`
}

type ToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type Observation struct {
	ID     string `json:"id"`
	Tool   string `json:"tool"`
	Result any    `json:"result,omitempty"`
	Error  string `json:"error,omitempty"`
}

// 主线：请求模型 → 读取 tool_calls → 执行工具 → 保存结果 → 下一轮。
func runAgent(ctx context.Context, config modelConfig, question string, parallel int) error {
	history := []Message{{Role: "system", Content: systemPrompt()}, {Role: "user", Content: question}}
	for step := 1; step <= 8; step++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		fmt.Printf("\nRound %d\n", step)
		reply, err := callModel(ctx, config, history)
		if err != nil {
			return err
		}
		history = append(history, reply)
		if reply.Content != "" {
			fmt.Println(reply.Content)
		}
		if len(reply.ToolCalls) == 0 {
			fmt.Println("Termination: no_tool_calls")
			return nil
		}
		if len(reply.ToolCalls) > 16 {
			return errors.New("每轮最多执行16项工具调用")
		}
		ids := map[string]bool{}
		for _, call := range reply.ToolCalls {
			if call.ID == "" || ids[call.ID] || call.Type != "function" || call.Function.Name == "" {
				return errors.New("模型返回了无效的工具调用")
			}
			ids[call.ID] = true
			fmt.Printf("Action [%s]: %s\nAction Input: %s\n", call.ID, call.Function.Name, call.Function.Arguments)
		}
		observations := executeBatch(ctx, reply.ToolCalls, parallel)
		if err := ctx.Err(); err != nil {
			return err
		}
		for _, observation := range observations {
			data, err := json.Marshal(observation)
			if err != nil {
				return err
			}
			fmt.Printf("Observation [%s]: %s\n", observation.ID, data)
			history = append(history, Message{Role: "tool", ToolCallID: observation.ID, Content: string(data)})
		}
	}
	return errors.New("达到 8 轮上限，任务未完成")
}

// 每个 goroutine 只写自己的结果；历史由主循环在整批结束后追加。
func executeBatch(ctx context.Context, calls []ToolCall, parallel int) []Observation {
	results := make([]Observation, len(calls))
	slots := make(chan struct{}, parallel)
	var wg sync.WaitGroup
	for i, call := range calls {
		wg.Add(1)
		go func(i int, call ToolCall) {
			defer wg.Done()
			result := Observation{ID: call.ID, Tool: call.Function.Name}
			select {
			case slots <- struct{}{}:
				defer func() { <-slots }()
				value, err := runTool(ctx, call)
				if err != nil {
					result.Error = err.Error()
				} else {
					result.Result = value
				}
			case <-ctx.Done():
				result.Error = ctx.Err().Error()
			}
			results[i] = result
		}(i, call)
	}
	wg.Wait()
	return results
}

func systemPrompt() string {
	return "你是一个工具助手，按需使用工具帮助用户，并用中文回答。"
}
