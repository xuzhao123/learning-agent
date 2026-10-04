package main

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"time"
)

// 用户题目预设；所有回答、工具决定和交接摘要均来自真实模型。
func runContextLab(ctx context.Context, config modelConfig, options contextOptions, maxCalls int) error {
	started := time.Now()
	client := &modelClient{Config: config, Limit: maxCalls, Output: options.Output}
	conversation := newContext(func() string {
		return "你是中文学习助手。只回答当前问题，无法从现有上下文确定的信息请明确说不知道。不要主动复述用户个人信息。"
	}, options, false)
	topics := []string{"完整记录与模型视图的区别", "输入容量与输出预留", "保留完整工具消息组", "用户原话与交接摘要", "真实usage加增量估算", "前缀缓存与集中压缩"}
	for turn := 1; turn <= 33; turn++ {
		question := fmt.Sprintf("第%d轮，围绕%s写约150个汉字，给一个例子。讨论材料：上下文是模型本轮实际读到的输入；完整记录与视图有不同用途。视图变短不等于原文被删除。一次工具调用可能产生多个结果，应完整保留调用和结果的关系。设计应说明信息从哪里来，何时追加，何时变成摘要，失败后还剩哪些事实。学习时同时观察原始记录长度和发送内容大小，并区别估算量与API返回用量。请只解释本轮概念，不复述其他轮次。", turn, topics[(turn-1)%len(topics)])
		if turn == 1 {
			question = "我的账号是 ACC-8837，后面会再问。现在只确认收到，不复述账号。"
		}
		if turn == 2 {
			question = "学习范围限定为Day 3，不讲Day 1或Day 2。现在只确认这条约束。"
		}
		if turn == 33 {
			question = "我最早告知的账号是什么？我限定的学习范围是什么？只依据已有信息，不知道就说不知道。"
		}
		conversation.Append(Message{Role: "user", Content: question})
		before := append([]Message(nil), conversation.Transcript...)
		oldView, compacts := append([]Message(nil), conversation.View...), conversation.CompactCount
		fmt.Printf("\nLab: turn=%d/33 transcript=%d view=%d\n", turn, len(before), len(oldView))
		reply, err := conversation.Next(ctx, client)
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(before, conversation.Transcript) {
			return fmt.Errorf("压缩修改了Transcript")
		}
		if compacts == conversation.CompactCount && !reflect.DeepEqual(oldView, conversation.View) {
			return fmt.Errorf("未压缩时视图发生改写")
		}
		if turn == 33 {
			data, _ := json.Marshal(conversation.View)
			fmt.Printf("Recall: account_in_view=%t answer=%s\n", strings.Contains(string(data), "ACC-8837"), reply.Message.Content)
		}
		conversation.RecordReply(reply)
	}
	fmt.Printf("Recall complete: compacts=%d transcript=%d view=%d\n", conversation.CompactCount, len(conversation.Transcript), len(conversation.View))

	// 第二个场景实际读取本地笔记，形成真实大输出；它不参与前面的账号召回实验。
	contextLabEnabled = true
	toolDefinitions = append(toolDefinitions, map[string]any{"name": "read_day3_notes", "description": "完整读取本地Day 3学习笔记，返回实际文件内容。", "parameters": parameters("")})
	toolsContext := newContext(systemPrompt, options, true)
	toolsContext.Append(Message{Role: "user", Content: "请调用read_day3_notes读取Day 3笔记，然后用两句话概括上下文管理的核心。"})
	for round := 0; round < 4; round++ {
		reply, err := toolsContext.Next(ctx, client)
		if err != nil {
			return err
		}
		toolsContext.RecordReply(reply)
		if len(reply.Message.ToolCalls) == 0 {
			fmt.Println("Large tool answer:", reply.Message.Content)
			break
		}
		for _, result := range executeBatch(ctx, reply.Message.ToolCalls, 4, 0) {
			data, err := json.Marshal(result)
			if err != nil {
				return err
			}
			toolsContext.Append(Message{Role: "tool", ToolCallID: result.ID, Content: string(data)})
			full, view := toolsContext.Transcript[len(toolsContext.Transcript)-1], toolsContext.View[len(toolsContext.View)-1]
			fmt.Printf("Large tool: original_tokens=%d view_tokens=%d truncated=%t transcript_intact=%t\n", textTokens(full.Content), textTokens(view.Content), full.Content != view.Content, full.Content == string(data))
		}
		if round == 3 {
			return fmt.Errorf("大工具实验未在4轮内完成")
		}
	}
	fmt.Printf("Lab complete: calls=%d/%d summaries=%d input_tokens=%d completion_tokens=%d cached_tokens=%d cache_known_input=%d summary_input=%d summary_cached=%d elapsed=%s\n", client.Calls, client.Limit, client.SummaryCalls, client.Input, client.Completion, client.Cached, client.CacheKnownInput, client.SummaryInput, client.SummaryCached, time.Since(started).Round(time.Second))
	return nil
}
