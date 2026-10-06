package main

import (
	"bufio"
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

//go:embed index.html
var page []byte

const contextLabTitle = "Day 3 上下文实验（33轮对话 + 大工具输出）"
const ragLabTitle = "Day 4 检索实验（10题无检索 / 有检索对比）"

// 观测系统不读取 agent 源码：它代理模型请求、转发终端输出，从协议数据还原过程。
type event struct {
	Seq       int             `json:"seq"`
	Time      time.Time       `json:"time"`
	Kind      string          `json:"kind"` // request、response、stdout、stderr、exit
	Call      int             `json:"call,omitempty"`
	Status    int             `json:"status,omitempty"`
	Millis    int64           `json:"ms,omitempty"`
	Body      json.RawMessage `json:"body,omitempty"`
	Text      string          `json:"text,omitempty"`
	Embedding string          `json:"embedding,omitempty"`
}

type run struct {
	ID, Query string
	Done      bool

	mu      sync.Mutex
	calls   int
	events  []event
	changed chan struct{}
	file    *os.File
}

type server struct {
	upstream, agentDir, addr string
	mu                       sync.Mutex
	runs                     []*run
}

func main() {
	addr := flag.String("addr", "127.0.0.1:8090", "观测页面地址")
	agentDir := flag.String("agent", "..", "agent 项目目录")
	flag.Parse()
	dir, err := filepath.Abs(*agentDir)
	if err == nil {
		err = os.MkdirAll("runs", 0o700)
	}
	upstream := upstreamURL(dir)
	if err == nil && upstream == "" {
		err = errors.New("请在环境变量或 agent 的 .env 中配置 LLM_API_URL")
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(1)
	}
	s := &server{upstream: upstream, agentDir: dir, addr: *addr}
	s.loadRuns()
	mux := http.NewServeMux()
	mux.Handle("GET /slides/", http.StripPrefix("/slides/", http.FileServer(http.Dir(filepath.Join(dir, "docs", "slides")))))
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(page)
	})
	mux.HandleFunc("GET /runs", s.listRuns)
	mux.HandleFunc("POST /runs", s.startRun)
	mux.HandleFunc("POST /runs/{id}/messages", s.continueRun)
	mux.HandleFunc("GET /runs/{id}/events", s.streamEvents)
	mux.HandleFunc("POST /llm/{id}", s.proxy)
	fmt.Printf("观测页面：http://%s\nagent 目录：%s\n模型上游：%s\n", *addr, dir, upstream)
	if err := http.ListenAndServe(*addr, mux); err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(1)
	}
}

// 真实上游地址：环境变量优先，其次是 agent 目录的 .env。
func upstreamURL(agentDir string) string {
	if value := strings.TrimSpace(os.Getenv("LLM_API_URL")); value != "" {
		return value
	}
	data, _ := os.ReadFile(filepath.Join(agentDir, ".env"))
	for _, line := range strings.Split(string(data), "\n") {
		name, value, _ := strings.Cut(strings.TrimSpace(line), "=")
		if strings.TrimSpace(name) == "LLM_API_URL" {
			return strings.Trim(strings.TrimSpace(value), "\"'")
		}
	}
	return ""
}

// 每次运行都用 go run 启动 agent，agent 代码更新后无需改动观测系统。
func (s *server) startRun(w http.ResponseWriter, req *http.Request) {
	var input struct {
		Query      string `json:"query"`
		ContextLab bool   `json:"context_lab"`
		RAG        bool   `json:"rag"`
		RAGLab     bool   `json:"rag_lab"`
		Embedding  string `json:"embedding"`
	}
	if json.NewDecoder(req.Body).Decode(&input) != nil {
		http.Error(w, "请求必须是JSON", http.StatusBadRequest)
		return
	}
	input.Query = strings.TrimSpace(input.Query)
	if input.Embedding == "" {
		input.Embedding = "ark"
	}
	if input.Embedding != "ark" && input.Embedding != "local" {
		http.Error(w, "embedding须为ark或local", http.StatusBadRequest)
		return
	}
	lab := input.ContextLab || input.RAGLab
	if (!lab && input.Query == "") || (lab && input.Query != "") || (input.ContextLab && input.RAGLab) || (lab && input.RAG) {
		http.Error(w, "普通问答需要query；实验请单独运行", http.StatusBadRequest)
		return
	}
	args := []string{"run", ".", "-question", input.Query}
	if input.RAG {
		args = append(args, "-rag")
	}
	if input.ContextLab {
		// 使用同一个真实实验入口；所有模型请求仍经过本次运行的代理。
		args = []string{"run", ".", "-context-lab", "-max-steps", "60", "-reasoning-effort", "minimal"}
		input.Query = contextLabTitle
	}
	if input.RAGLab {
		args = []string{"run", ".", "-rag-lab", "-max-steps", "6", "-reasoning-effort", "minimal"}
		input.Query = ragLabTitle
	}
	if input.RAG || input.RAGLab {
		args = append(args, "-embedding", input.Embedding)
	}
	id := time.Now().Format("20060102-150405.000")
	file, err := os.OpenFile(filepath.Join("runs", id+".jsonl"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	r := &run{ID: id, Query: strings.TrimSpace(input.Query), changed: make(chan struct{}), file: file}
	s.mu.Lock()
	s.runs = append(s.runs, r)
	s.mu.Unlock()
	r.add(event{Kind: "start", Text: r.Query, Embedding: input.Embedding})
	s.launch(r, args, nil)
	json.NewEncoder(w).Encode(r.summary())
}

// 同一条对话一次只处理一个提问；恢复历史不会重新执行已经完成的工具。
func (s *server) continueRun(w http.ResponseWriter, req *http.Request) {
	r := s.find(req.PathValue("id"))
	if r == nil {
		http.Error(w, "找不到这条对话", http.StatusNotFound)
		return
	}
	var input struct {
		Query string `json:"query"`
	}
	if json.NewDecoder(req.Body).Decode(&input) != nil || strings.TrimSpace(input.Query) == "" {
		http.Error(w, "请填写续聊问题", http.StatusBadRequest)
		return
	}
	r.mu.Lock()
	if !r.canContinue() {
		r.mu.Unlock()
		http.Error(w, "对话正在运行或尚未正常完成，暂不能续聊", http.StatusConflict)
		return
	}
	history, err := r.resumeHistory()
	if err != nil {
		r.mu.Unlock()
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	file, err := os.OpenFile(filepath.Join("runs", r.ID+".jsonl"), os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		r.mu.Unlock()
		http.Error(w, "无法追加对话记录", http.StatusInternalServerError)
		return
	}
	r.file, r.Done = file, false
	r.mu.Unlock()
	r.add(event{Kind: "continue", Text: strings.TrimSpace(input.Query), Embedding: history.Embedding})
	args := []string{"run", ".", "-history-stdin", "-question", strings.TrimSpace(input.Query)}
	if history.Effort != "" {
		args = append(args, "-reasoning-effort", history.Effort)
	}
	if history.RAG {
		args = append(args, "-rag", "-embedding", history.Embedding)
	}
	s.launch(r, args, history)
	json.NewEncoder(w).Encode(r.summary())
}

func (s *server) launch(r *run, args []string, history *resumeInput) {
	cmd := exec.Command("go", args...)
	cmd.Dir = s.agentDir
	cmd.Env = append(os.Environ(), "LLM_API_URL=http://"+s.addr+"/llm/"+r.ID)
	if history != nil {
		data, err := json.Marshal(history)
		if err != nil {
			r.add(event{Kind: "exit", Text: "无法编码续聊历史"})
			return
		}
		cmd.Stdin = bytes.NewReader(data)
		cmd.Env = append(cmd.Env, "LLM_MODEL="+history.Model)
	}
	stdout, _ := cmd.StdoutPipe()
	stderr, _ := cmd.StderrPipe()
	if err := cmd.Start(); err != nil {
		r.add(event{Kind: "exit", Text: err.Error()})
	} else {
		var wg sync.WaitGroup
		wg.Add(2)
		go r.copyLines(&wg, "stdout", stdout)
		go r.copyLines(&wg, "stderr", stderr)
		go func() {
			wg.Wait()
			err := cmd.Wait()
			text := "exit 0"
			if err != nil {
				text = err.Error()
			}
			r.add(event{Kind: "exit", Text: text})
		}()
	}
}

type resumeInput struct {
	Messages  []json.RawMessage `json:"messages"`
	Usage     json.RawMessage   `json:"usage,omitempty"`
	WithTools bool              `json:"with_tools"`
	Model     string            `json:"-"`
	Effort    string            `json:"-"`
	RAG       bool              `json:"-"`
	Embedding string            `json:"-"`
}

// 调用方持有r.mu。请求里的View已经包含摘要，直接延续它，不重新拼接所有旧请求。
func (r *run) resumeHistory() (*resumeInput, error) {
	start, end := 0, len(r.events)
	for i, e := range r.events {
		if e.Kind == "continue" {
			start = i + 1
		}
	}
	if start == 0 && r.Query == contextLabTitle {
		// Day 3后半段是独立的大工具场景；续聊接在33轮学习对话之后。
		for i, e := range r.events {
			if e.Kind == "stdout" && strings.HasPrefix(e.Text, "Recall complete:") {
				end = i
				break
			}
		}
	}
	var request struct {
		Messages []json.RawMessage `json:"messages"`
		Tools    []json.RawMessage `json:"tools"`
		Model    string            `json:"model"`
		Effort   string            `json:"reasoning_effort"`
	}
	call, compact := 0, false
	var result *resumeInput
	for _, e := range r.events[start:end] {
		if e.Kind == "request" {
			request.Messages, request.Tools = nil, nil
			if json.Unmarshal(e.Body, &request) != nil || len(request.Messages) == 0 {
				return nil, errors.New("存档缺少有效的模型请求")
			}
			var last struct{ Content string }
			json.Unmarshal(request.Messages[len(request.Messages)-1], &last)
			call, compact = e.Call, strings.HasPrefix(last.Content, "[上下文压缩请求]")
			if !compact {
				result = nil
			}
		}
		if e.Kind != "response" || e.Call != call || compact || e.Status != http.StatusOK {
			continue
		}
		var response struct {
			Usage   json.RawMessage `json:"usage"`
			Choices []struct {
				Message      json.RawMessage `json:"message"`
				FinishReason string          `json:"finish_reason"`
			} `json:"choices"`
		}
		if json.Unmarshal(e.Body, &response) != nil || len(response.Choices) == 0 || response.Choices[0].FinishReason != "stop" {
			continue
		}
		choice := response.Choices[0]
		var reply struct {
			Role, Content string
			ToolCalls     []json.RawMessage `json:"tool_calls"`
		}
		if json.Unmarshal(choice.Message, &reply) != nil || reply.Role != "assistant" || strings.TrimSpace(reply.Content) == "" || len(reply.ToolCalls) > 0 {
			continue
		}
		messages := append(append([]json.RawMessage(nil), request.Messages...), choice.Message)
		result = &resumeInput{Messages: messages, Usage: response.Usage, WithTools: len(request.Tools) > 0, Model: request.Model, Effort: request.Effort}
		for _, rawTool := range request.Tools {
			var tool struct{ Function struct{ Name string } }
			if json.Unmarshal(rawTool, &tool) == nil && tool.Function.Name == "search_docs" {
				result.RAG = true
			}
		}
	}
	if result == nil {
		return nil, errors.New("存档没有完整的回答，无法恢复续聊上下文")
	}
	result.Embedding = "ark"
	// 新存档明确记录选择；旧存档从实际检索结果恢复backend，无法确定时沿用默认ark。
	for i := len(r.events) - 1; i >= 0; i-- {
		e := r.events[i]
		if e.Embedding == "ark" || e.Embedding == "local" {
			result.Embedding = e.Embedding
			return result, nil
		}
	}
	for _, raw := range result.Messages {
		var m struct{ Role, Content string }
		if json.Unmarshal(raw, &m) != nil || m.Role != "tool" {
			continue
		}
		var observation struct {
			Tool   string
			Result struct{ Backend string }
		}
		if json.Unmarshal([]byte(m.Content), &observation) == nil && observation.Tool == "search_docs" && (observation.Result.Backend == "ark" || observation.Result.Backend == "local") {
			result.Embedding = observation.Result.Backend
		}
	}
	return result, nil
}

// 调用方持有r.mu；失败或中断的任务不能被当作已完成对话重放。
func (r *run) canContinue() bool {
	return r.Query != ragLabTitle && r.Done && r.calls > 0 && len(r.events) > 0 && r.events[len(r.events)-1].Kind == "exit" && r.events[len(r.events)-1].Text == "exit 0"
}

// 启动时读回 runs/*.jsonl，重启后仍能查看、回放和对比历史运行。
func (s *server) loadRuns() {
	paths, _ := filepath.Glob(filepath.Join("runs", "*.jsonl"))
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		r := &run{ID: strings.TrimSuffix(filepath.Base(path), ".jsonl"), Done: true, changed: make(chan struct{})}
		for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
			var e event
			if json.Unmarshal([]byte(line), &e) != nil {
				continue
			}
			r.events = append(r.events, e)
			r.calls = max(r.calls, e.Call)
			if e.Kind == "start" {
				r.Query = e.Text
			}
			if e.Kind == "request" && r.Query == "" {
				r.Query = firstUserMessage(e.Body)
			}
		}
		s.runs = append(s.runs, r)
	}
}

// 早期存档没有 start 事件，从第一次请求的 user 消息取问题。
func firstUserMessage(body json.RawMessage) string {
	var request struct {
		Messages []struct{ Role, Content string }
	}
	json.Unmarshal(body, &request)
	for _, m := range request.Messages {
		if m.Role == "user" {
			return m.Content
		}
	}
	return ""
}

func (r *run) summary() map[string]any {
	r.mu.Lock()
	defer r.mu.Unlock()
	return map[string]any{"id": r.ID, "query": r.Query, "done": r.Done, "can_continue": r.canContinue()}
}

func (r *run) copyLines(wg *sync.WaitGroup, kind string, pipe io.Reader) {
	defer wg.Done()
	scanner := bufio.NewScanner(pipe)
	scanner.Buffer(make([]byte, 64*1024), 4<<20)
	for scanner.Scan() {
		r.add(event{Kind: kind, Text: scanner.Text()})
	}
}

// 转发请求与响应原文；Authorization 只转发，不记录。
func (s *server) proxy(w http.ResponseWriter, req *http.Request) {
	r := s.find(req.PathValue("id"))
	if r == nil {
		http.Error(w, "未知运行", http.StatusNotFound)
		return
	}
	body, err := io.ReadAll(req.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	r.mu.Lock()
	r.calls++
	call := r.calls
	r.mu.Unlock()
	r.add(event{Kind: "request", Call: call, Body: raw(body)})

	start := time.Now()
	upstream, err := http.NewRequestWithContext(req.Context(), http.MethodPost, s.upstream, bytes.NewReader(body))
	if err == nil {
		upstream.Header.Set("Content-Type", req.Header.Get("Content-Type"))
		upstream.Header.Set("Authorization", req.Header.Get("Authorization"))
		var response *http.Response
		if response, err = http.DefaultClient.Do(upstream); err == nil {
			defer response.Body.Close()
			data, readErr := io.ReadAll(response.Body)
			r.add(event{Kind: "response", Call: call, Status: response.StatusCode, Millis: time.Since(start).Milliseconds(), Body: raw(data)})
			if readErr == nil {
				w.Header().Set("Content-Type", response.Header.Get("Content-Type"))
				w.WriteHeader(response.StatusCode)
				w.Write(data)
				return
			}
			err = readErr
		}
	}
	r.add(event{Kind: "response", Call: call, Status: http.StatusBadGateway, Millis: time.Since(start).Milliseconds(), Text: err.Error()})
	http.Error(w, err.Error(), http.StatusBadGateway)
}

func raw(data []byte) json.RawMessage {
	if json.Valid(data) {
		return data
	}
	text, _ := json.Marshal(string(data))
	return text
}

// 事件写入内存供页面实时读取，同时追加到 runs/<id>.jsonl 留档。
func (r *run) add(e event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e.Seq, e.Time = len(r.events)+1, time.Now()
	r.events = append(r.events, e)
	if line, err := json.Marshal(e); err == nil {
		r.file.Write(append(line, '\n'))
	}
	if e.Kind == "exit" {
		r.Done = true
		r.file.Close()
	}
	close(r.changed)
	r.changed = make(chan struct{})
}

// SSE：先回放已有事件，再推送新事件，运行结束后关闭。
func (s *server) streamEvents(w http.ResponseWriter, req *http.Request) {
	r := s.find(req.PathValue("id"))
	if r == nil {
		http.Error(w, "未知运行", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	flusher, _ := w.(http.Flusher)
	for sent := 0; ; {
		r.mu.Lock()
		pending, changed, done := r.events[sent:], r.changed, r.Done
		r.mu.Unlock()
		for _, e := range pending {
			data, _ := json.Marshal(e)
			fmt.Fprintf(w, "data: %s\n\n", data)
		}
		sent += len(pending)
		if flusher != nil {
			flusher.Flush()
		}
		if done {
			fmt.Fprint(w, "event: end\ndata: {}\n\n")
			return
		}
		select {
		case <-changed:
		case <-req.Context().Done():
			return
		}
	}
}

func (s *server) listRuns(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	runs := append([]*run(nil), s.runs...)
	s.mu.Unlock()
	list := []map[string]any{}
	for _, r := range runs {
		list = append(list, r.summary())
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(list)
}

func (s *server) find(id string) *run {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range s.runs {
		if r.ID == id {
			return r
		}
	}
	return nil
}
