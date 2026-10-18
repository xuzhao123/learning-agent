package transcript

import (
	"encoding/json"
	"regexp"
	"strings"
	"time"
)

// Claude Code 的会话 JSONL 每行一条记录，用 uuid / parentUuid 串成树（倒回重试会留下分支，压缩边界的 parentUuid 为空、
// 用 logicalParentUuid 接上）。从最后一条往回走得到实际生效的主干。一次回复拆成的多行、并行工具各自的结果会分出
// 旁支，所以按回复（message.id）判断：回复有一行在主干上，它的所有行和挂在这些行下面的工具结果都算。
// 最后按文件顺序（即时间顺序）处理。
//   - assistant：一次回复按内容块拆成多行，message.id 相同；usage 在 message 上
//   - user：用户原话、斜杠命令、工具结果（tool_result 块）、注入的上下文（isMeta）、压缩摘要（isCompactSummary）
//   - system：compact_boundary 等；子 agent（isSidechain，或另存在 subagents/ 下）不在主链上
func parseClaude(lines [][]byte) (*Conversation, error) {
	type record struct {
		Type          string          `json:"type"`
		Subtype       string          `json:"subtype"`
		UUID          string          `json:"uuid"`
		Parent        string          `json:"parentUuid"`
		LogicalParent string          `json:"logicalParentUuid"`
		Sidechain     bool            `json:"isSidechain"`
		Meta          bool            `json:"isMeta"`
		Summary       bool            `json:"isCompactSummary"`
		Cwd           string          `json:"cwd"`
		Timestamp     time.Time       `json:"timestamp"`
		Message       json.RawMessage `json:"message"`
		reply         string          // assistant 行所属回复的 message.id
	}
	byID, order := map[string]*record{}, []*record{}
	for _, line := range lines {
		r := &record{}
		if json.Unmarshal(line, r) != nil || r.UUID == "" || r.Sidechain {
			continue
		}
		byID[r.UUID] = r
		if r.Type == "assistant" {
			var m struct{ ID string }
			json.Unmarshal(r.Message, &m)
			r.reply = m.ID
		}
		if r.Type == "user" || r.Type == "assistant" || r.Type == "system" {
			order = append(order, r)
		}
	}
	trunk := map[string]bool{}
	if len(order) > 0 {
		for r := order[len(order)-1]; r != nil && !trunk[r.UUID]; {
			trunk[r.UUID] = true
			parent := r.Parent
			if parent == "" {
				parent = r.LogicalParent
			}
			r = byID[parent]
		}
	}
	replies := map[string]bool{}
	for id := range trunk {
		if r := byID[id]; r.reply != "" {
			replies[r.reply] = true
		}
	}
	var chain []*record
	for _, r := range order {
		parent := byID[r.Parent]
		toolResult := r.Type == "user" && parent != nil && replies[parent.reply] && strings.Contains(string(r.Message), `"tool_result"`)
		if trunk[r.UUID] || replies[r.reply] || toolResult {
			chain = append(chain, r)
		}
	}
	b := newBuilder("claude-code")
	var open *assistant
	type result struct {
		m  Message
		at time.Time
	}
	var results []result // 回复还在流式输出时就执行完的工具，结果等这次回复结束后再进历史
	cancelled := false
	flush := func() {
		if open != nil {
			b.call(open.message(), open.usage, open.end, false)
			open = nil
		}
		for _, r := range results {
			b.add(r.m, r.at)
		}
		results = nil
	}
	for _, r := range chain {
		if b.conv.Dir == "" {
			b.conv.Dir = r.Cwd
		}
		var m struct {
			ID      string          `json:"id"`
			Model   string          `json:"model"`
			Content json.RawMessage `json:"content"`
			Usage   struct {
				Input         int `json:"input_tokens"`
				CacheCreation int `json:"cache_creation_input_tokens"`
				CacheRead     int `json:"cache_read_input_tokens"`
				Output        int `json:"output_tokens"`
			} `json:"usage"`
		}
		json.Unmarshal(r.Message, &m)
		at := r.Timestamp
		switch r.Type {
		case "assistant":
			if m.Model == "<synthetic>" { // 客户端自己生成的提示（如 API 错误），不是模型回复
				continue
			}
			if open == nil || open.id != m.ID {
				flush()
			}
			if open == nil {
				open = &assistant{id: m.ID}
				b.model = m.Model
			}
			open.end = at
			in := m.Usage.Input + m.Usage.CacheCreation + m.Usage.CacheRead
			open.usage = usage(in, m.Usage.CacheRead, m.Usage.Output, 0)
			var blocks []map[string]any
			json.Unmarshal(m.Content, &blocks)
			for _, block := range blocks {
				switch block["type"] {
				case "text":
					open.content = append(open.content, text(block["text"]))
				case "thinking":
					if t := text(block["thinking"]); t != "" { // 新版本只留签名，正文为空
						open.reasoning = append(open.reasoning, t)
					}
				case "tool_use":
					args, _ := json.Marshal(block["input"])
					id, _ := block["id"].(string)
					name, _ := block["name"].(string)
					open.calls = append(open.calls, toolCall(id, name, string(args)))
				}
			}
		case "user":
			content := raw(m.Content)
			if blocks, ok := content.([]any); ok && len(blocks) > 0 && isToolResult(blocks[0]) {
				for _, item := range blocks {
					block, _ := item.(map[string]any)
					if !isToolResult(block) {
						continue
					}
					msg := Message{"role": "tool", "tool_call_id": block["tool_use_id"], "content": text(block["content"])}
					if block["is_error"] == true {
						msg["is_error"] = true
					}
					results = append(results, result{msg, at})
				}
				continue
			}
			flush()
			t := text(content)
			if r.Summary {
				b.compact(t, nil, at)
				continue
			}
			if prompt := claudePrompt(t); prompt != "" && !r.Meta {
				if cancelled {
					b.end("cancelled", at)
					cancelled = false
				}
				b.begin(prompt, at)
			}
			if strings.HasPrefix(t, "[Request interrupted by user") {
				cancelled = true
			}
			b.add(Message{"role": "user", "content": t}, at)
		}
	}
	flush()
	if cancelled {
		b.end("cancelled", b.last)
	}
	return b.finish(), nil
}

func isToolResult(v any) bool {
	block, _ := v.(map[string]any)
	return block != nil && block["type"] == "tool_result"
}

var commandName, commandArgs = regexp.MustCompile(`<command-name>([^<]*)</command-name>`), regexp.MustCompile(`<command-args>([^<]*)</command-args>`)

// claudePrompt 判断一条用户消息是不是人发起的一轮：原话、斜杠命令（/name args）、! 开头的 shell 命令；
// 命令输出、中断标记、后台通知等是上下文，不开新的一轮。
func claudePrompt(t string) string {
	t = strings.TrimSpace(t)
	switch {
	case t == "":
		return ""
	case strings.HasPrefix(t, "<command-name>") || strings.HasPrefix(t, "<command-message>"):
		name, args := commandName.FindStringSubmatch(t), commandArgs.FindStringSubmatch(t)
		if name == nil {
			return ""
		}
		if args != nil {
			return strings.TrimSpace(name[1] + " " + args[1])
		}
		return name[1]
	case strings.HasPrefix(t, "<bash-input>"):
		return "!" + strings.TrimSuffix(strings.TrimPrefix(t, "<bash-input>"), "</bash-input>")
	case strings.HasPrefix(t, "<"), strings.HasPrefix(t, "[Request interrupted"), strings.HasPrefix(t, "Caveat:"):
		return ""
	}
	return t
}
