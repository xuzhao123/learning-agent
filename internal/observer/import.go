package observer

import (
	"encoding/json"
	"io"
	"net/http"
	"time"

	"github.com/google/uuid"

	"learning-agent/internal/transcript"
)

// importRun 导入 Codex / Claude Code 的会话文件：换成和代理记下的同一种存档（request/response 成对），
// 放进当前租户的 runs 目录，之后和普通对话一样在列表里查看、回放、对比。导入的对话只读：没有检查点，不能续聊。
func (s *server) importRun(w http.ResponseWriter, req *http.Request) {
	u := current(req)
	data, err := io.ReadAll(http.MaxBytesReader(w, req.Body, 128<<20))
	if err != nil {
		http.Error(w, "文件读取失败或超过 128 MB："+err.Error(), http.StatusBadRequest)
		return
	}
	conv, err := transcript.Parse(data)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	events := importEvents(conv)
	if len(events) == 0 {
		http.Error(w, "文件里没有找到对话", http.StatusBadRequest)
		return
	}
	id := uuid.NewString()
	file, err := s.createRunFile(u.Tenant, id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer file.Close()
	r := &run{ID: id, Tenant: u.Tenant, Query: events[0].Text, Source: conv.Source, Done: true, changed: make(chan struct{})}
	for _, e := range events {
		line, _ := json.Marshal(e)
		if _, err := file.Write(append(line, '\n')); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		r.calls = max(r.calls, e.Call)
	}
	r.events = events
	s.mu.Lock()
	s.runs = append(s.runs, r)
	s.mu.Unlock()
	writeJSON(w, r.summary())
}

// importEvents 按时间顺序生成事件，时间用记录里的原始时间戳。每一轮以 start/continue 开始、exit 结束，
// 和观测台自己启动的对话一样；请求只带新增的消息（append），页面接在上一次请求后面还原完整输入。
func importEvents(conv *transcript.Conversation) []event {
	var events []event
	add := func(e event, at time.Time) {
		e.Seq, e.Time = len(events)+1, at.UTC()
		events = append(events, e)
	}
	n := 0
	for _, x := range conv.Events {
		switch {
		case x.Prompt != "":
			kind := "continue"
			if len(events) == 0 {
				kind = "start"
			}
			add(event{Kind: kind, Text: x.Prompt, Source: conv.Source, Provider: conv.Source, WorkDir: conv.Dir, Protocol: true}, x.Time)
		case x.End != "":
			if x.End == "cancelled" {
				body, _ := json.Marshal(map[string]string{"outcome": "cancelled"})
				add(event{Kind: "notify", Method: "turn/completed", Body: body}, x.Time)
				add(event{Kind: "exit", Text: "exit 1"}, x.Time)
			} else {
				add(event{Kind: "exit", Text: "exit 0"}, x.Time)
			}
		case x.Call != nil:
			c := x.Call
			n++
			purpose := "main"
			if c.Compact {
				purpose = "compact"
			}
			request := map[string]any{"model": c.Model, "messages": c.New}
			if !c.Reset {
				request["append"] = true
			}
			if c.Tools != nil {
				request["tools"] = c.Tools
			}
			body, _ := json.Marshal(request)
			add(event{Kind: "request", Call: n, Purpose: purpose, Body: body}, c.Start)
			finish := "stop"
			if c.Reply["tool_calls"] != nil {
				finish = "tool_calls"
			}
			response := map[string]any{"model": c.Model, "choices": []any{map[string]any{"index": 0, "finish_reason": finish, "message": c.Reply}}}
			if c.Usage != nil {
				response["usage"] = c.Usage
			}
			body, _ = json.Marshal(response)
			add(event{Kind: "response", Call: n, Status: 200, Millis: c.End.Sub(c.Start).Milliseconds(), Body: body}, c.End)
		}
	}
	return events
}
