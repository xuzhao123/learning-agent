package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"learning-agent/internal/llm"
)

// D37 消融候选：上下文内的 todo 清单（Claude Code 的 TodoWrite 一类）。模型自己写、自己更新，harness 只存最新的一份并原样交回。
// D27 决定默认不做，由消融看它有没有用：-todo 打开，同时在系统提示词里加一句用法。
var Todo bool

const todoRules = "\n多步任务先用 todo_write 列出步骤，做完一项就更新状态；一两步就能做完的不用。"

var TodoDefinitions = []map[string]any{{
	"name":        "todo_write",
	"description": "写下或更新这次任务的步骤清单。每次传完整的清单：每项有 content 与 status（pending、in_progress、completed），同一时刻最多一项 in_progress。",
	"parameters": map[string]any{
		"type": "object",
		"properties": map[string]any{
			"todos": map[string]any{"type": "array", "items": map[string]any{
				"type":                 "object",
				"properties":           map[string]any{"content": map[string]string{"type": "string"}, "status": map[string]any{"type": "string", "enum": []string{"pending", "in_progress", "completed"}}},
				"required":             []string{"content", "status"},
				"additionalProperties": false,
			}},
		},
		"required":             []string{"todos"},
		"additionalProperties": false,
	},
}}

func todoWrite(arguments string) (any, error) {
	var args struct {
		Todos []struct{ Content, Status string } `json:"todos"`
	}
	if err := json.Unmarshal([]byte(arguments), &args); err != nil || len(args.Todos) == 0 {
		return nil, llm.Permanent(errors.New("todos 需要至少一项"))
	}
	running := 0
	for _, t := range args.Todos {
		if t.Content == "" || !slices.Contains([]string{"pending", "in_progress", "completed"}, t.Status) {
			return nil, llm.Permanent(errors.New("每项要有 content，status 取 pending、in_progress、completed"))
		}
		if t.Status == "in_progress" {
			running++
		}
	}
	if running > 1 {
		return nil, llm.Permanent(fmt.Errorf("同一时刻最多一项 in_progress，现在有 %d 项", running))
	}
	return map[string]any{"todos": args.Todos, "note": "清单已更新"}, nil
}
