package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"learning-agent/internal/llm"
	"learning-agent/internal/protocol"
)

// D39：子 agent 的控制面。原来 spawn_agent 只有一种用法：派出去、等它做完、拿回结论，父 agent 这段时间什么也做不了。
// 现在多了：
//   - 后台：spawn_agent 带 background=true，立刻返回 task_id，子 agent 在后台跑，父 agent 接着做别的；
//   - agent_wait：等某个（或任意一个）后台子 agent 做完，拿回它的结论；
//   - agent_send：给还在跑的子 agent 插一句话（它的协议连接上发 turn/steer，D34 的插话）；
//   - agent_stop：停掉它（turn/interrupt），它回填结果、写好检查点，之后可以用 task_id 续跑；
//   - 分叉：spawn_agent 带 fork=true，子 agent 从当前对话接着做（同样的上下文，换成它自己的系统提示词和工具），
//     适合“基于我们刚才讨论的这些，去试一种方案”；默认仍是全新的上下文。
// 后台子 agent 只活到这一轮结束：模型想结束而还有后台子 agent 在跑时提醒一次；这一轮结束时还在跑的统一停掉，结论可以下一轮续跑取回。

// turnCtx 是这一轮的上下文（Run 设置）：后台子 agent 跟着这一轮走，而不是跟着派出它的那次工具调用。
var turnCtx = context.Background()

// forkFrom 返回当前对话的视图（Run 设置）：分叉的子 agent 拿到的就是这份。
var forkFrom func() []llm.Message

type backgroundSub struct {
	id, callID, task string
	done             chan struct{}
	result           any
	err              error
	collected        bool // 结论已经经 agent_wait 交给模型

	mu   sync.Mutex
	conn *protocol.Conn
}

var (
	bgMu sync.Mutex
	bgs  []*backgroundSub
)

// forkHistory：当前视图去掉最后一条（就是发起这次 spawn_agent 的 assistant 消息，它的工具结果还没有），系统提示词留空让子 agent 换成自己的。
func forkHistory() *HistoryInput {
	if forkFrom == nil {
		return nil
	}
	view := forkFrom()
	if n := len(view); n > 1 && view[n-1].Role == "assistant" && len(view[n-1].ToolCalls) > 0 {
		view = view[:n-1]
	}
	if len(view) < 2 {
		return nil
	}
	messages := slices.Clone(view)
	messages[0] = llm.Message{Role: "system"}
	// 父对话里用户说的“派一个子 agent 去……”会被子 agent 当成对自己说的；先说清它的身份，任务紧跟在后面。
	messages = append(messages, llm.Reminder("以上是派出你的父 agent 的对话，供你参考。你是它派出的子 agent：只做下一条消息里的任务，用你自己的工具完成，不要再派子 agent，做完给出结论。"))
	return &HistoryInput{Messages: messages, WithTools: true}
}

func startBackground(call llm.ToolCall, id, task string, history *HistoryInput) (any, error) {
	bgMu.Lock()
	for _, b := range bgs {
		if b.id == id {
			bgMu.Unlock()
			return nil, llm.Permanent(fmt.Errorf("子任务 %s 已经在后台运行，用 agent_wait 等它", id))
		}
	}
	b := &backgroundSub{id: id, callID: call.ID, task: task, done: make(chan struct{})}
	bgs = append(bgs, b)
	bgMu.Unlock()
	go func() {
		defer close(b.done)
		b.result, b.err = runSub(turnCtx, call.ID, id, task, history, func(conn *protocol.Conn) {
			b.mu.Lock()
			b.conn = conn
			b.mu.Unlock()
		})
	}()
	return map[string]any{"task_id": id, "status": "running", "fork": history != nil,
		"note": "子 agent 在后台运行。用 agent_wait 等它的结论，agent_send 给它补充说明，agent_stop 停掉；这一轮结束时还在跑的会被停下（之后可以用 task_id 续跑）。"}, nil
}

func findBackground(id string) (*backgroundSub, error) {
	bgMu.Lock()
	defer bgMu.Unlock()
	for _, b := range bgs {
		if b.id == strings.TrimSpace(id) {
			return b, nil
		}
	}
	return nil, llm.Permanent(fmt.Errorf("没有这个后台子任务：%s（task_id 来自 spawn_agent 带 background=true 的返回，只在本轮有效）", id))
}

// outcome 是交给模型的结论：成功时就是 spawn_agent 的结果，失败时带上错误和 task_id。
func (b *backgroundSub) outcome() map[string]any {
	b.collected = true
	if b.err != nil {
		return map[string]any{"task_id": b.id, "status": "failed", "error": b.err.Error()}
	}
	result, _ := b.result.(map[string]any)
	out := map[string]any{"status": "done"}
	for k, v := range result {
		out[k] = v
	}
	return out
}

// agentWait 等一个后台子 agent 做完；不给 task_id 时等任意一个还没交过结论的。最多等 timeout_seconds（默认 60，最多 300）。
func agentWait(ctx context.Context, arguments string) (any, error) {
	var args struct {
		TaskID  string `json:"task_id"`
		Timeout int    `json:"timeout_seconds"`
	}
	if err := json.Unmarshal([]byte(arguments), &args); err != nil {
		return nil, llm.Permanent(err)
	}
	wait := time.Duration(min(max(args.Timeout, 1), 300)) * time.Second
	if args.Timeout == 0 {
		wait = time.Minute
	}
	var targets []*backgroundSub
	if args.TaskID != "" {
		b, err := findBackground(args.TaskID)
		if err != nil {
			return nil, err
		}
		targets = []*backgroundSub{b}
	} else {
		bgMu.Lock()
		for _, b := range bgs {
			if !b.collected {
				targets = append(targets, b)
			}
		}
		bgMu.Unlock()
		if len(targets) == 0 {
			return nil, llm.Permanent(errors.New("没有在等结论的后台子任务"))
		}
	}
	deadline := time.After(wait)
	for {
		for _, b := range targets {
			select {
			case <-b.done:
				return b.outcome(), nil
			default:
			}
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-deadline:
			ids := []string{}
			for _, b := range targets {
				ids = append(ids, b.id)
			}
			return map[string]any{"status": "running", "task_ids": ids, "note": fmt.Sprintf("等了 %s 还没做完；可以先做别的，稍后再等", wait)}, nil
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// agentControl 给后台子 agent 插话或停掉它：都经它自己的协议连接（它的父进程就是它的界面）。
func agentControl(ctx context.Context, name, arguments string) (any, error) {
	var args struct {
		TaskID  string `json:"task_id"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal([]byte(arguments), &args); err != nil {
		return nil, llm.Permanent(err)
	}
	b, err := findBackground(args.TaskID)
	if err != nil {
		return nil, err
	}
	select {
	case <-b.done:
		return map[string]any{"task_id": b.id, "status": "finished", "note": "它已经结束了，用 agent_wait 取结论"}, nil
	default:
	}
	b.mu.Lock()
	conn := b.conn
	b.mu.Unlock()
	if conn == nil {
		return nil, errors.New("子 agent 还在启动，稍后再试")
	}
	callCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if name == "agent_send" {
		if strings.TrimSpace(args.Message) == "" {
			return nil, llm.Permanent(errors.New("agent_send 需要 message"))
		}
		if err := conn.Call(callCtx, "turn/steer", map[string]string{"text": args.Message}, nil); err != nil {
			return nil, llm.Permanent(fmt.Errorf("没有送到（它可能刚做完）：%w", err))
		}
		return map[string]any{"task_id": b.id, "status": "sent", "note": "排进了它的下一次模型请求"}, nil
	}
	if err := conn.Call(callCtx, "turn/interrupt", nil, nil); err != nil {
		return nil, fmt.Errorf("停止请求没有送到：%w", err)
	}
	select {
	case <-b.done:
	case <-time.After(30 * time.Second):
	}
	return map[string]any{"task_id": b.id, "status": "stopped", "note": "已经停下并写好检查点；要接着做就用 spawn_agent 传这个 task_id"}, nil
}

// runningBackground 返回还在跑的后台子任务。
func runningBackground() []string {
	bgMu.Lock()
	defer bgMu.Unlock()
	var ids []string
	for _, b := range bgs {
		select {
		case <-b.done:
		default:
			ids = append(ids, b.id)
		}
	}
	return ids
}

// stopBackground 在这一轮结束时停掉还在跑的后台子 agent，等它们写好检查点（最多 30 秒）。
func stopBackground() {
	bgMu.Lock()
	list := slices.Clone(bgs)
	bgMu.Unlock()
	for _, b := range list {
		b.mu.Lock()
		conn := b.conn
		b.mu.Unlock()
		select {
		case <-b.done:
			continue
		default:
		}
		fmt.Printf("Subagent background stop: task_id=%s（这一轮结束）\n", b.id)
		if conn != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			conn.Call(ctx, "turn/interrupt", nil, nil)
			cancel()
		}
		select {
		case <-b.done:
		case <-time.After(30 * time.Second):
		}
	}
}

var ControlDefinitions = []map[string]any{
	{"name": "agent_wait", "description": "等后台子 agent（spawn_agent 带 background=true 派出的）做完，返回它的结论。给 task_id 等那一个；不给就等任意一个还没交过结论的。最多等 timeout_seconds 秒（默认 60，最多 300），到时还没做完会说明，可以先做别的再来等。",
		"parameters": map[string]any{"type": "object", "properties": map[string]any{
			"task_id":         map[string]string{"type": "string"},
			"timeout_seconds": map[string]string{"type": "integer"},
		}, "additionalProperties": false}},
	{"name": "agent_send", "description": "给还在后台运行的子 agent 补充一句说明或调整要求：不打断它正在做的事，排进它的下一次模型请求。",
		"parameters": map[string]any{"type": "object", "properties": map[string]any{
			"task_id": map[string]string{"type": "string"},
			"message": map[string]string{"type": "string"},
		}, "required": []string{"task_id", "message"}, "additionalProperties": false}},
	{"name": "agent_stop", "description": "停掉一个后台子 agent：它回填结果、写好检查点后退出，之后可以用 spawn_agent 传 task_id 续跑。方向错了、结论已经不需要时用。",
		"parameters": map[string]any{"type": "object", "properties": map[string]any{
			"task_id": map[string]string{"type": "string"},
		}, "required": []string{"task_id"}, "additionalProperties": false}},
}
