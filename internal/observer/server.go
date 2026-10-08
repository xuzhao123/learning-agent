package observer

import (
	"bufio"
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"learning-agent/internal/protocol"
	"learning-agent/internal/telemetry"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel/attribute"
	semconv "go.opentelemetry.io/otel/semconv/v1.41.0"
	"go.opentelemetry.io/otel/trace"
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
	Purpose   string          `json:"purpose,omitempty"` // 请求用途：main、compact、memory_contextualize、memory_rerank
	Memory    bool            `json:"memory,omitempty"`
	MemoryTTL string          `json:"memory_ttl,omitempty"`
	Skills    []string        `json:"skills,omitempty"` // 本对话启用的 skill 与 MCP server 快照，续聊沿用
	MCP       []mcpServer     `json:"mcp,omitempty"`
	Subagents bool            `json:"subagents,omitempty"` // 本对话允许 spawn_agent，续聊沿用
	Browser   bool            `json:"browser,omitempty"`   // 本对话启用浏览器工具，续聊沿用
	Provider  string          `json:"provider,omitempty"`  // 本对话的模型供应商（-provider），续聊沿用
	Bash      bool            `json:"bash,omitempty"`      // 本对话启用 bash 沙箱，续聊沿用
	NetAllow  []string        `json:"net_allow,omitempty"` // 本轮沙箱可访问的域名（用户批准过的），记在 start/continue 上
	Task      string          `json:"task,omitempty"`      // 发出这次请求的 agent 任务ID（X-Agent-Task）
	Sub       bool            `json:"sub,omitempty"`       // 请求来自子 agent：不进入父对话流、不参与续聊恢复
	Trace     string          `json:"trace,omitempty"`     // D13：start/continue 上记这次交互的 trace_id
	Span      string          `json:"span,omitempty"`      // D13：request 上记 agent 的 chat span ID（来自 traceparent 头）
	Method    string          `json:"method,omitempty"`    // B0：notify / approval 事件的协议方法名，参数在 Body
	RPC       int64           `json:"rpc,omitempty"`       // B0：agent 发来的请求ID（approval），回复时带回
	Protocol  bool            `json:"protocol,omitempty"`  // B0：start/continue 上标记这一轮经协议运行；之前的存档没有，页面据此启用旧存档兼容
	Eval      string          `json:"eval,omitempty"`      // D16：由评测发起的试次（评测ID · 用例 #序号），对话列表里不显示
}

type run struct {
	ID, Query string
	Eval      string // D16：评测发起的试次，见 event.Eval
	Done      bool

	mu       sync.Mutex
	calls    int
	events   []event
	changed  chan struct{}
	file     *os.File
	proc     *os.Process // 正在运行的 agent 进程；强制结束时向它的进程组发 SIGKILL
	stopping bool        // 已请求过一次停止；再按一次强制结束
	rootTask string      // 本次启动的父 agent 任务ID：启动后第一个带任务ID的请求一定来自父 agent
	// B0：与正在运行的 agent 之间的协议连接，以及它在等待答复的联网审批（域名 → 请求ID）。
	conn      *protocol.Conn
	approvals map[string][]int64
}

type server struct {
	agentDir, addr    string
	mu                sync.Mutex
	runs              []*run
	runsDir, agentBin string     // 存档目录 .data/runs；agentBin 为空表示 -dev（go run .）
	hubMu             sync.Mutex // 保护 .data/hub.json 的读改写
	evals             evals      // D16/D17 评测记录（见 eval.go）
}

// Run 启动观测台：go run . observe [-addr …] [-dev]，须在项目根目录运行。
// 观测台与内置 MCP server 在同一个进程；每次对话仍启动一个独立的 agent 子进程，
// 这样各次运行的全局状态、终端输出和故障互不影响，模型请求也照旧经过本机代理记录。
func Run(args []string, mcpHandler http.Handler) error {
	flags := flag.NewFlagSet("observe", flag.ContinueOnError)
	addr := flags.String("addr", "127.0.0.1:8090", "观测页面地址")
	dev := flags.Bool("dev", false, "每次对话用 go run . 启动 agent：改完 agent 代码不必重启观测台，代价是每次多一次编译")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("observe 只接受 -addr 与 -dev")
	}
	// agent 读 .env、skills/、docs/、.data/ 都相对项目根目录，所以观测台以当前目录为准。
	dir, err := os.Getwd()
	if err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(dir, "go.mod")); err != nil {
		return errors.New("请在项目根目录运行 go run . observe")
	}
	agentBin := ""
	if !*dev {
		if agentBin, err = os.Executable(); err != nil {
			return err
		}
	}
	runsDir := filepath.Join(dir, ".data", "runs")
	if err := os.MkdirAll(runsDir, 0o700); err != nil {
		return err
	}
	s := &server{agentDir: dir, addr: *addr, runsDir: runsDir, agentBin: agentBin, evals: evals{jobs: map[string]*evalJob{}}}
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
	mux.HandleFunc("POST /runs/{id}/stop", s.stopRun)
	mux.Handle("POST /runs/{id}/network", sameOrigin(http.HandlerFunc(s.decideNetwork)))
	mux.HandleFunc("POST /llm/{id}", s.proxy)
	mux.HandleFunc("GET /browser/{task}/{call}", s.browserFrame)
	mux.HandleFunc("GET /traces/{id}", s.traceSpans)
	mux.HandleFunc("GET /trace-metrics", s.traceMetrics)
	mux.HandleFunc("GET /evals", s.listEvals)
	mux.HandleFunc("GET /evals/cases", s.evalCases)
	mux.HandleFunc("GET /evals/{id}", s.getEval)
	mux.Handle("POST /evals", sameOrigin(http.HandlerFunc(s.startEval)))
	mux.Handle("POST /evals/{id}/{action}", sameOrigin(http.HandlerFunc(s.evalAction)))
	mux.Handle("POST /runs/{id}/attribute", sameOrigin(http.HandlerFunc(s.attributeRun)))
	mux.HandleFunc("GET /memory", s.listMemory)
	mux.HandleFunc("POST /memory/{id}/forget", s.forgetMemory)
	hub := func(pattern string, handler http.HandlerFunc) { mux.Handle(pattern, sameOrigin(handler)) }
	hub("GET /hub", s.listHub)
	hub("GET /hub/enabled", s.enabledHub)
	hub("POST /hub/skills", s.createSkill)
	hub("DELETE /hub/skills/{name}", s.deleteSkill)
	hub("POST /hub/skills/{name}/enabled", s.toggleSkill)
	hub("POST /hub/mcp", s.addMCP)
	hub("DELETE /hub/mcp/{name}", s.deleteMCP)
	hub("POST /hub/mcp/{name}/enabled", s.toggleMCP)
	hub("POST /hub/mcp/{name}/test", s.testMCP)
	// 内置远程 MCP：同一进程直接处理，不另开端口、不转发；同样只接受本机同源请求。
	mux.Handle("/mcp", sameOrigin(mcpHandler))
	mode := "直接执行当前程序（改了 agent 代码需重启观测台）"
	if *dev {
		mode = "go run .（每次读取最新源码）"
	}
	fmt.Printf("观测页面：http://%s\n远程 MCP：http://%s/mcp（内置 calculator，Streamable HTTP）\nAgent：项目根目录 · %s\n模型上游：由每个 agent 按 -provider 在请求头声明\n", *addr, *addr, mode)
	return http.ListenAndServe(*addr, mux)
}

// agent 子进程：默认执行当前程序本身，与观测台同一份代码、不需要编译、在哪台机器运行就是哪台机器的格式；
// -dev 时改用 go run .，读取最新源码。
// Day 11：agent 浏览时把画面写到 .data/browser/<任务ID>/<调用ID>.jpg（screencast 帧不断覆盖，结束时是最终截图）。
// 页面运行中反复来取这张图，就是实时画面；调用结束后它就是这一步的快照。
var providerPattern = regexp.MustCompile(`^[a-z]{1,32}$`)

var frameName = regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`)

func (s *server) browserFrame(w http.ResponseWriter, req *http.Request) {
	task, call := req.PathValue("task"), strings.TrimSuffix(req.PathValue("call"), ".jpg")
	if !frameName.MatchString(task) || !frameName.MatchString(call) || strings.HasPrefix(task, ".") || strings.Contains(call, ".") {
		http.NotFound(w, req)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	http.ServeFile(w, req, filepath.Join(s.agentDir, ".data", "browser", task, call+".jpg"))
}

func (s *server) agentCommand(ctx context.Context, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, s.agentBin, args...)
	if s.agentBin == "" {
		cmd = exec.CommandContext(ctx, "go", append([]string{"run", "."}, args...)...)
	}
	cmd.Dir = s.agentDir
	return cmd
}

// 每次运行都启动一个独立的 agent 子进程（见 agentCommand），各次运行的状态与输出互不影响。
func (s *server) startRun(w http.ResponseWriter, req *http.Request) {
	var input struct {
		Query      string `json:"query"`
		ContextLab bool   `json:"context_lab"`
		RAG        bool   `json:"rag"`
		RAGLab     bool   `json:"rag_lab"`
		Embedding  string `json:"embedding"`
		Memory     bool   `json:"memory"`
		MemoryTTL  string `json:"memory_ttl"`
		Skills     bool   `json:"skills"`
		MCP        bool   `json:"mcp"`
		Subagents  bool   `json:"subagents"`
		Browser    bool   `json:"browser"`
		Provider   string `json:"provider"`
		Bash       bool   `json:"bash"`
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
	if (!lab && input.Query == "") || (lab && input.Query != "") || (input.ContextLab && input.RAGLab) || (lab && (input.RAG || input.Memory || input.Skills || input.MCP || input.Subagents || input.Browser || input.Bash)) {
		http.Error(w, "普通问答需要query；实验请单独运行", http.StatusBadRequest)
		return
	}
	// 供应商名由 agent 校验（路由表在 llm 包）；这里只挡住不像名字的输入。
	if input.Provider != "" && !providerPattern.MatchString(input.Provider) {
		http.Error(w, "provider 只能是小写字母", http.StatusBadRequest)
		return
	}
	if ttl, err := time.ParseDuration(input.MemoryTTL); input.MemoryTTL != "" && (err != nil || ttl <= 0 || !input.Memory) {
		http.Error(w, "memory_ttl须为正的时长（如1m），且需开启长期记忆", http.StatusBadRequest)
		return
	}
	args := []string{}
	if input.RAG {
		args = append(args, "-rag")
	}
	args = appendMemoryArgs(args, input.Memory, input.MemoryTTL)
	var skills []string
	var servers []mcpServer
	if input.Skills || input.MCP {
		enabledSkills, enabledServers, err := s.enabledCapabilities()
		if err != nil {
			http.Error(w, "无法读取 .data/hub.json："+err.Error(), http.StatusInternalServerError)
			return
		}
		if input.Skills {
			skills = enabledSkills
		}
		if input.MCP {
			servers = enabledServers
		}
	}
	args = appendCapabilityArgs(args, skills, servers)
	if input.Subagents {
		args = append(args, "-subagents")
	}
	if input.Browser {
		args = append(args, "-browser")
	}
	if input.Provider != "" {
		args = append(args, "-provider", input.Provider)
	}
	if input.Bash {
		args = append(args, "-bash")
	}
	if input.ContextLab {
		// 使用同一个真实实验入口；所有模型请求仍经过本次运行的代理。
		args = []string{"-context-lab", "-max-steps", "60", "-reasoning-effort", "minimal"}
		input.Query = contextLabTitle
	}
	if input.RAGLab {
		args = []string{"-rag-lab", "-max-steps", "6", "-reasoning-effort", "minimal"}
		input.Query = ragLabTitle
	}
	if input.RAG || input.RAGLab || input.Memory {
		args = append(args, "-embedding", input.Embedding)
	}
	// 对话 ID：随机 UUID（v4），不含时间，与观测台在哪个时区启动无关。
	// 早期存档的 ID 是“20261006-213820.763”这样的本地时间，照常读回，不改名，旧链接仍能打开。
	id := uuid.NewString()
	file, err := os.OpenFile(filepath.Join(s.runsDir, id+".jsonl"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	r := &run{ID: id, Query: strings.TrimSpace(input.Query), changed: make(chan struct{}), file: file}
	s.mu.Lock()
	s.runs = append(s.runs, r)
	s.mu.Unlock()
	ctx, span := r.beginTurn(input.Query)
	r.add(event{Kind: "start", Text: r.Query, Embedding: input.Embedding, Memory: input.Memory, MemoryTTL: input.MemoryTTL, Skills: skills, MCP: servers, Subagents: input.Subagents, Browser: input.Browser, Provider: input.Provider, Bash: input.Bash, Trace: span.SpanContext().TraceID().String(), Protocol: true})
	var turn map[string]any
	if !input.ContextLab && !input.RAGLab {
		turn = map[string]any{"question": input.Query}
	}
	s.launch(ctx, r, args, turn, "")
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
	file, err := os.OpenFile(filepath.Join(s.runsDir, r.ID+".jsonl"), os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		r.mu.Unlock()
		http.Error(w, "无法追加对话记录", http.StatusInternalServerError)
		return
	}
	// 续聊沿用这条对话开始时的记忆、skill 与 MCP 设置：最近一次 start/continue 事件为准。
	memory, ttl, subagents, browser, provider, bash := false, "", false, false, "", false
	var skills []string
	var servers []mcpServer
	for _, e := range r.events {
		if e.Kind == "start" || e.Kind == "continue" {
			memory, ttl, skills, servers, subagents, browser, provider, bash = e.Memory, e.MemoryTTL, e.Skills, e.MCP, e.Subagents, e.Browser, e.Provider, e.Bash
		}
	}
	r.file, r.Done = file, false
	r.mu.Unlock()
	ctx, span := r.beginTurn(strings.TrimSpace(input.Query))
	r.add(event{Kind: "continue", Text: strings.TrimSpace(input.Query), Embedding: history.Embedding, Memory: memory, MemoryTTL: ttl, Skills: skills, MCP: servers, Subagents: subagents, Browser: browser, Provider: provider, Bash: bash, NetAllow: r.netAllow(), Trace: span.SpanContext().TraceID().String(), Protocol: true})
	args := []string{}
	if history.Effort != "" {
		args = append(args, "-reasoning-effort", history.Effort)
	}
	if history.RAG {
		args = append(args, "-rag")
	}
	if history.RAG || memory {
		args = append(args, "-embedding", history.Embedding)
	}
	args = appendMemoryArgs(args, memory, ttl)
	args = appendCapabilityArgs(args, skills, servers)
	if subagents {
		args = append(args, "-subagents")
	}
	if browser {
		args = append(args, "-browser")
	}
	if provider != "" {
		args = append(args, "-provider", provider)
	}
	if bash {
		args = append(args, "-bash")
		for _, domain := range r.netAllow() {
			args = append(args, "-net-allow", domain)
		}
	}
	// 问题、续聊上下文与续接关系放进 turn/start。每一轮都用新的检查点ID，旧检查点保持原样（“一个ID一个问题”），
	// 续接关系让模型可以按 task_id 续跑之前各轮的子任务。
	turn := map[string]any{"question": strings.TrimSpace(input.Query), "history": history, "continues": history.Continues}
	s.launch(ctx, r, args, turn, history.Model)
	json.NewEncoder(w).Encode(r.summary())
}

// D13：每次交互（首问或续聊）一个 trace，根 span 是观测台的 interaction；agent 进程的 invoke_agent 挂在它下面。
// 同一个对话的多个 trace 用 gen_ai.conversation.id（对话ID）归到一起。
func (r *run) beginTurn(query string) (context.Context, trace.Span) {
	r.mu.Lock()
	turn := 1
	for _, e := range r.events {
		if e.Kind == "start" || e.Kind == "continue" {
			turn++
		}
	}
	r.mu.Unlock()
	ctx, span := telemetry.Begin(context.Background(), "interaction", trace.SpanKindServer, semconv.GenAIConversationID(r.ID), attribute.Int("agent.turn", turn))
	telemetry.Content(span, "agent.question", query)
	return ctx, span
}

// B0：agent 以 -app-server 启动，stdout 是协议，stderr 是给人看的终端输出。
// turn 为 nil 时（上下文、检索实验）由命令行参数决定要做什么，不发 turn/start。
func (s *server) launch(ctx context.Context, r *run, args []string, turn map[string]any, model string) {
	span := trace.SpanFromContext(ctx)
	cmd := s.agentCommand(context.Background(), append([]string{"-app-server"}, args...)...)
	cmd.Env = append(os.Environ(), "LLM_API_URL=http://"+s.addr+"/llm/"+r.ID, "AGENT_CONVERSATION_ID="+r.ID)
	cmd.Env = append(cmd.Env, telemetry.Env(ctx)...)
	if model != "" {
		cmd.Env = append(cmd.Env, "LLM_MODEL="+model)
	}
	stdin, _ := cmd.StdinPipe()
	stdout, _ := cmd.StdoutPipe()
	stderr, _ := cmd.StderrPipe()
	conn := protocol.NewConn(stdout, stdin, r.handle)
	r.mu.Lock()
	r.conn, r.approvals = conn, map[string][]int64{}
	r.mu.Unlock()
	// 自己的进程组：停止时向整组发信号。-dev 模式下组里是 go run 和它编译出的 agent，两者都能收到。
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		r.add(event{Kind: "exit", Text: err.Error()})
		telemetry.End(span, err, "launch_error")
	} else {
		r.mu.Lock()
		r.proc, r.stopping, r.rootTask = cmd.Process, false, ""
		r.mu.Unlock()
		if turn != nil {
			go func() {
				if err := conn.Call(context.Background(), "turn/start", turn, nil); err != nil {
					r.add(event{Kind: "stdout", Text: "turn/start 失败：" + err.Error()})
				}
			}()
		}
		var wg sync.WaitGroup
		wg.Add(1)
		// 终端输出只有人读；页面需要的结构化信息都来自协议通知。
		go r.copyLines(&wg, "stdout", stderr)
		go func() {
			wg.Wait()
			<-conn.Closed()
			err := cmd.Wait()
			r.mu.Lock()
			r.proc, r.conn, r.approvals = nil, nil, nil
			r.mu.Unlock()
			text := "exit 0"
			if err != nil {
				text = err.Error()
			}
			// 先结束根 span 再记 exit：页面看到 exit 时，interaction 已经写进 trace 文件。
			span.SetAttributes(attribute.String("process.exit", text))
			telemetry.End(span, err, "agent_exit")
			r.add(event{Kind: "exit", Text: text})
		}()
	}
}

func appendMemoryArgs(args []string, memory bool, ttl string) []string {
	if memory {
		args = append(args, "-memory")
	}
	if memory && ttl != "" {
		args = append(args, "-memory-ttl", ttl)
	}
	return args
}

// 记忆文件由 agent 写入；观测台只读展示。删除走 agent 的 -memory-forget，与 agent 共用文件锁和事务。
// 派生索引（背景说明、块状态）只用来展示，缺失或损坏时显示为空，不影响原始记忆。
func (s *server) listMemory(w http.ResponseWriter, _ *http.Request) {
	data, err := os.ReadFile(filepath.Join(s.agentDir, ".data", "memory.json"))
	if errors.Is(err, os.ErrNotExist) {
		data, err = []byte(`{"next":0,"memories":[]}`), nil
	}
	if err != nil || !json.Valid(data) {
		http.Error(w, "无法读取 .data/memory.json", http.StatusInternalServerError)
		return
	}
	index, err := os.ReadFile(filepath.Join(s.agentDir, ".data", "memory-index", "contexts.json"))
	if err != nil || !json.Valid(index) {
		index = []byte(`{"chunks":{}}`)
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"path": ".data/memory.json", "now": time.Now(), "file": json.RawMessage(data), "index": json.RawMessage(index)})
}

var memoryID = regexp.MustCompile(`^M[0-9]{1,9}$`)
var purposePattern = regexp.MustCompile(`^[a-z_]{1,32}$`)
var taskPattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`)

func (s *server) forgetMemory(w http.ResponseWriter, req *http.Request) {
	id := req.PathValue("id")
	if !memoryID.MatchString(id) {
		http.Error(w, "记忆编号格式为M加数字", http.StatusBadRequest)
		return
	}
	cmd := s.agentCommand(req.Context(), "-memory-forget", id)
	output, err := cmd.CombinedOutput()
	if err != nil {
		http.Error(w, strings.TrimSpace(string(output)), http.StatusConflict)
		return
	}
	w.Write(output)
}

type resumeInput struct {
	Messages  []json.RawMessage `json:"messages"`
	Usage     json.RawMessage   `json:"usage,omitempty"`
	WithTools bool              `json:"with_tools"`
	Model     string            `json:"-"`
	Effort    string            `json:"-"`
	RAG       bool              `json:"-"`
	Embedding string            `json:"-"`
	Continues string            `json:"-"` // 上一轮的检查点ID：本轮用新ID并记下续接关系，子任务可以按 task_id 引用之前各轮的子任务
}

// 调用方持有r.mu。请求里的View已经包含摘要，直接延续它，不重新拼接所有旧请求。
// 上一轮中途停下（停止按钮、超时、出错）时，改从 agent 的检查点接着聊。
func (r *run) resumeHistory() (*resumeInput, error) {
	if id := r.lastCheckpoint(); id != "" && r.events[len(r.events)-1].Text != "exit 0" {
		result, err := r.checkpointHistory(id)
		if err != nil {
			return nil, err
		}
		result.Continues = id
		return r.withEmbedding(result), nil
	}
	start, end := 0, len(r.events)
	for i, e := range r.events {
		if e.Kind == "continue" {
			start = i + 1
		}
	}
	if start == 0 && r.Query == contextLabTitle {
		// Day 3后半段是独立的大工具场景；续聊接在33轮学习对话之后。
		for i, e := range r.events {
			// B0 起是 lab/phase 通知；之前的存档只有日志行。
			if (e.Kind == "notify" && e.Method == "lab/phase" && strings.Contains(string(e.Body), "recall_complete")) ||
				(e.Kind == "stdout" && strings.HasPrefix(e.Text, "Recall complete:")) {
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
		// 记忆背景生成与重排是辅助调用：它们的输入不是对话，输出也不是 assistant 的回答，续聊不能从它们恢复。
		// 子 agent 的请求属于另一个上下文，同样跳过。
		if strings.HasPrefix(e.Purpose, "memory_") || e.Sub {
			continue
		}
		if e.Kind == "request" {
			request.Messages, request.Tools = nil, nil
			if json.Unmarshal(e.Body, &request) != nil || len(request.Messages) == 0 {
				return nil, errors.New("存档缺少有效的模型请求")
			}
			var last struct{ Content string }
			json.Unmarshal(request.Messages[len(request.Messages)-1], &last)
			call, compact = e.Call, e.Purpose == "compact" || strings.HasPrefix(last.Content, "[上下文压缩请求]")
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
	result.Continues = r.lastCheckpoint()
	return r.withEmbedding(result), nil
}

// 中途停下的对话从 agent 的检查点接着聊：检查点里有被打断那一轮的完整记录，
// 包括写成“未执行”“结果未知”的工具结果；存档里只能找到最后一个完整回答之前的请求。
func (r *run) checkpointHistory(id string) (*resumeInput, error) {
	data, err := os.ReadFile(filepath.Join(".data", "checkpoints", id+".json"))
	if err != nil {
		return nil, errors.New("找不到这次运行的检查点，无法接着聊：" + id)
	}
	var cp struct {
		Messages []json.RawMessage `json:"messages"`
	}
	if json.Unmarshal(data, &cp) != nil || len(cp.Messages) == 0 {
		return nil, errors.New("检查点无效：" + id)
	}
	result := &resumeInput{Messages: cp.Messages, WithTools: true}
	// 模型、推理强度与是否检索沿用这条对话最近一次主任务请求。
	for i := len(r.events) - 1; i >= 0; i-- {
		e := r.events[i]
		if e.Kind != "request" || e.Sub || strings.HasPrefix(e.Purpose, "memory_") {
			continue
		}
		var request struct {
			Tools  []struct{ Function struct{ Name string } } `json:"tools"`
			Model  string                                     `json:"model"`
			Effort string                                     `json:"reasoning_effort"`
		}
		if json.Unmarshal(e.Body, &request) == nil {
			result.Model, result.Effort = request.Model, request.Effort
			for _, tool := range request.Tools {
				result.RAG = result.RAG || tool.Function.Name == "search_docs"
			}
		}
		break
	}
	return result, nil
}

// 最近一轮 agent 的检查点ID：来自它的 turn/started 通知（子 agent 的通知包在 subagent/message 里，不会混进来）。
// B0 之前的存档没有通知，退回到当时打印的 “Checkpoint: id=…” 行。
func (r *run) lastCheckpoint() string {
	for i := len(r.events) - 1; i >= 0; i-- {
		e := r.events[i]
		if e.Kind == "start" || e.Kind == "continue" {
			return ""
		}
		if e.Kind == "notify" && e.Method == "turn/started" {
			var p struct {
				TurnID string `json:"turn_id"`
			}
			if json.Unmarshal(e.Body, &p) == nil && p.TurnID != "" {
				return p.TurnID
			}
		}
		if m := legacyCheckpointLine.FindStringSubmatch(e.Text); e.Kind == "stdout" && m != nil {
			return m[1]
		}
	}
	return ""
}

var legacyCheckpointLine = regexp.MustCompile(`^Checkpoint: id=([A-Za-z0-9._-]{1,128}) `)

func (r *run) withEmbedding(result *resumeInput) *resumeInput {
	result.Embedding = "ark"
	// 新存档明确记录选择；旧存档从实际检索结果恢复backend，无法确定时沿用默认ark。
	for i := len(r.events) - 1; i >= 0; i-- {
		e := r.events[i]
		if e.Embedding == "ark" || e.Embedding == "local" {
			result.Embedding = e.Embedding
			return result
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
	return result
}

// 调用方持有r.mu。正常结束的对话从最后一个完整回答接着聊；
// 中途停下的对话（停止按钮、超时、出错）只要 agent 写过检查点，就从检查点接着聊。
func (r *run) canContinue() bool {
	// 用户的网络审批（allow/deny）可能追加在 exit 之后，不算对话进程的事件。
	i := len(r.events) - 1
	for i >= 0 && (r.events[i].Kind == "allow" || r.events[i].Kind == "deny") {
		i--
	}
	if r.Query == ragLabTitle || !r.Done || i < 0 || r.events[i].Kind != "exit" {
		return false
	}
	if r.events[i].Text == "exit 0" {
		return r.calls > 0
	}
	return r.lastCheckpoint() != ""
}

// 启动时读回 runs/*.jsonl，重启后仍能查看、回放和对比历史运行。
func (s *server) loadRuns() {
	paths, _ := filepath.Glob(filepath.Join(s.runsDir, "*.jsonl"))
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
				r.Query, r.Eval = e.Text, e.Eval
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
	summary := map[string]any{"id": r.ID, "query": r.Query, "done": r.Done, "can_continue": r.canContinue(), "eval": r.Eval}
	// 时间给 Unix 毫秒时间戳，由页面按看的人所在的时区显示。
	if len(r.events) > 0 {
		summary["created_at"], summary["updated_at"] = r.events[0].Time.UnixMilli(), r.events[len(r.events)-1].Time.UnixMilli()
	}
	return summary
}

// 最后一次活动的时间，列表按它排序。对话 ID 是随机的 UUID（早期存档是本地时间字符串），不表示先后。
func (r *run) updated() time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.events) == 0 {
		return time.Time{}
	}
	return r.events[len(r.events)-1].Time
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
	// 用途与任务ID由 agent 在请求头明确标记，只记录、不转发给上游；旧版本 agent 没有这两个头。
	purpose := req.Header.Get("X-Agent-Purpose")
	if !purposePattern.MatchString(purpose) {
		purpose = ""
	}
	task := req.Header.Get("X-Agent-Task")
	if !taskPattern.MatchString(task) {
		task = ""
	}
	r.mu.Lock()
	r.calls++
	call := r.calls
	if r.rootTask == "" {
		r.rootTask = task
	}
	sub := task != "" && task != r.rootTask
	r.mu.Unlock()
	// traceparent 是 00-<trace_id>-<span_id>-<flags>；只记 span_id，页面据此把调用链里的 chat span 和这条原始请求对上。
	spanID := ""
	if parts := strings.Split(req.Header.Get("traceparent"), "-"); len(parts) == 4 && spanPattern.MatchString(parts[2]) {
		spanID = parts[2]
	}
	r.add(event{Kind: "request", Call: call, Body: raw(body), Purpose: purpose, Task: task, Sub: sub, Span: spanID})

	start := time.Now()
	// 模型路由在 agent 里：它在 X-Agent-Upstream 里声明这次请求的真实地址（方舟或 DeepSeek），代理照此转发。
	// 只接受 https。浏览器里的网页设不了这个自定义头（跨域预检会被拒），能设它的只有本机进程，而本机进程本来就能直接访问外网。
	target, err := url.Parse(req.Header.Get("X-Agent-Upstream"))
	if err == nil && (target.Scheme != "https" || target.Host == "" || target.User != nil) {
		err = errors.New("X-Agent-Upstream 需要是 https 地址")
	}
	if err != nil {
		r.add(event{Kind: "response", Call: call, Status: http.StatusBadRequest, Text: err.Error(), Purpose: purpose, Task: task, Sub: sub})
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	upstream, err := http.NewRequestWithContext(req.Context(), http.MethodPost, target.String(), bytes.NewReader(body))
	if err == nil {
		upstream.Header.Set("Content-Type", req.Header.Get("Content-Type"))
		upstream.Header.Set("Authorization", req.Header.Get("Authorization"))
		var response *http.Response
		if response, err = http.DefaultClient.Do(upstream); err == nil {
			defer response.Body.Close()
			data, readErr := io.ReadAll(response.Body)
			r.add(event{Kind: "response", Call: call, Status: response.StatusCode, Millis: time.Since(start).Milliseconds(), Body: raw(data), Purpose: purpose, Task: task, Sub: sub})
			if readErr == nil {
				w.Header().Set("Content-Type", response.Header.Get("Content-Type"))
				w.WriteHeader(response.StatusCode)
				w.Write(data)
				return
			}
			err = readErr
		}
	}
	// agent 取消了请求（停止按钮、超时）时记为 499（客户端关闭请求），与上游故障的 502 区分开。
	status := http.StatusBadGateway
	if req.Context().Err() != nil {
		status = 499
	}
	r.add(event{Kind: "response", Call: call, Status: status, Millis: time.Since(start).Milliseconds(), Text: err.Error(), Purpose: purpose, Task: task, Sub: sub})
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
	e.Seq, e.Time = len(r.events)+1, time.Now().UTC() // 统一存 UTC：换时区启动观测台、或别人打开页面，时间都不会错
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

// 停止按钮：第一次向 agent 进程组发 SIGINT，agent 回填结果、写好检查点再退出（Day 8 的优雅停止）；
// 第二次发 SIGKILL 强制结束。子 agent 在各自的进程组里，由父 agent 收到 SIGINT 后转发。
// Day 12：沙箱网络白名单由用户审批。agent 的 request_network_access 只打印一行请求；
// 用户在页面上点“允许 / 拒绝”，这里记一条 allow/deny 事件（存进对话记录），下一轮续聊时作为 -net-allow 传给 agent。
// 模型自己无法调用这个接口：它在断网的沙箱里，也没有访问观测台的工具。
var domainPattern = regexp.MustCompile(`^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z]{2,63}$`)

func (s *server) decideNetwork(w http.ResponseWriter, req *http.Request) {
	r := s.find(req.PathValue("id"))
	if r == nil {
		http.Error(w, "找不到这条对话", http.StatusNotFound)
		return
	}
	var input struct {
		Domain string `json:"domain"`
		Allow  bool   `json:"allow"`
	}
	if json.NewDecoder(req.Body).Decode(&input) != nil || len(input.Domain) > 253 || !domainPattern.MatchString(input.Domain) {
		http.Error(w, "domain 需要是小写域名", http.StatusBadRequest)
		return
	}
	kind := "deny"
	if input.Allow {
		kind = "allow"
	}
	r.mu.Lock()
	if r.Done {
		// 已结束的对话，记录文件已经关了：临时以追加方式打开，写完再关，保证重启后审批仍在。
		file, err := os.OpenFile(filepath.Join(s.runsDir, r.ID+".jsonl"), os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			r.mu.Unlock()
			http.Error(w, "无法写入对话记录", http.StatusInternalServerError)
			return
		}
		r.file = file
		defer file.Close()
	}
	r.mu.Unlock()
	r.add(event{Kind: kind, Text: input.Domain})
	// B0：如果 agent 正在等这个域名的审批，当场回复；它会在本轮接着执行。否则只记下来，下一轮续聊时作为 -net-allow 传入。
	r.mu.Lock()
	conn, waiting := r.conn, r.approvals[input.Domain]
	delete(r.approvals, input.Domain)
	r.mu.Unlock()
	for _, id := range waiting {
		if conn != nil {
			conn.Reply(id, map[string]string{"decision": kind}, nil)
		}
	}
	json.NewEncoder(w).Encode(map[string]any{"net_allow": r.netAllow(), "answered": len(waiting)})
}

// 当前允许的域名：按时间顺序处理 allow/deny，后一次决定覆盖前一次。
func (r *run) netAllow() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	list := []string{}
	for _, e := range r.events {
		if e.Kind == "allow" && !slices.Contains(list, e.Text) {
			list = append(list, e.Text)
		}
		if e.Kind == "deny" {
			list = slices.DeleteFunc(list, func(d string) bool { return d == e.Text })
		}
	}
	return list
}

func (s *server) stopRun(w http.ResponseWriter, req *http.Request) {
	r := s.find(req.PathValue("id"))
	if r == nil {
		http.Error(w, "找不到这条对话", http.StatusNotFound)
		return
	}
	r.mu.Lock()
	proc, force := r.proc, r.stopping
	r.stopping = true
	r.mu.Unlock()
	if proc == nil {
		http.Error(w, "这条对话没有在运行", http.StatusConflict)
		return
	}
	if force {
		if err := syscall.Kill(-proc.Pid, syscall.SIGKILL); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		r.add(event{Kind: "stop", Text: "强制结束：向 agent 发送 SIGKILL"})
		w.WriteHeader(http.StatusNoContent)
		return
	}
	// 第一次：经协议发 turn/interrupt，agent 回填结果、写好检查点再退出（第一段）；协议不通时退回 SIGINT。
	r.mu.Lock()
	conn := r.conn
	r.mu.Unlock()
	text := "已请求停止：经协议发送 turn/interrupt，等待 agent 回填结果、写好检查点后退出"
	ctx, cancel := context.WithTimeout(req.Context(), 3*time.Second)
	defer cancel()
	if conn == nil || conn.Call(ctx, "turn/interrupt", nil, nil) != nil {
		if err := syscall.Kill(-proc.Pid, syscall.SIGINT); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		text = "已请求停止：向 agent 发送 SIGINT，等待它回填结果、写好检查点后退出"
	}
	r.add(event{Kind: "stop", Text: text})
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) listRuns(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	runs := append([]*run(nil), s.runs...)
	s.mu.Unlock()
	// 从旧到新（页面再倒过来显示）：最近有活动的对话在最上面，续聊过的旧对话也会回到顶部。
	slices.SortStableFunc(runs, func(a, b *run) int { return a.updated().Compare(b.updated()) })
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

var spanPattern = regexp.MustCompile(`^[0-9a-f]{16}$`)

// D13：读一个 trace 的全部 span（.data/traces/<trace_id>.jsonl，由各进程的 SDK 导出器写入）。
// 运行中也可以读：span 结束一个写一行，还没结束的 span（包括根 span）暂时不在文件里。
func (s *server) traceSpans(w http.ResponseWriter, req *http.Request) {
	id := req.PathValue("id")
	if !telemetry.ValidTraceID(id) {
		http.NotFound(w, req)
		return
	}
	spans, err := readSpans(filepath.Join(s.agentDir, telemetry.Path(id)))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(spans)
}

func readSpans(path string) ([]telemetry.Record, error) {
	data, err := os.ReadFile(path)
	spans := []telemetry.Record{}
	for _, line := range strings.Split(string(data), "\n") {
		var record telemetry.Record
		if json.Unmarshal([]byte(line), &record) == nil && record.SpanID != "" {
			spans = append(spans, record)
		}
	}
	return spans, err
}

// D13 指标：不另起一套采集，从已结束的 span 汇总。名字沿用 GenAI 约定的指标：
// gen_ai.client.operation.duration（模型、向量化）、gen_ai.client.token.usage、execute_tool 的耗时与失败率，以及任务结局。
// conversation 参数只统计这个对话的 trace；不给时统计全部。
func (s *server) traceMetrics(w http.ResponseWriter, req *http.Request) {
	conversation := req.URL.Query().Get("conversation")
	paths, _ := filepath.Glob(filepath.Join(s.agentDir, telemetry.Dir, "*.jsonl"))
	type series struct {
		Name     string  `json:"name"`
		Count    int     `json:"count"`
		Errors   int     `json:"errors"`
		P50      float64 `json:"p50_ms"`
		P95      float64 `json:"p95_ms"`
		Input    int     `json:"input_tokens,omitempty"`
		Output   int     `json:"output_tokens,omitempty"`
		Cached   int     `json:"cached_tokens,omitempty"`
		duration []float64
	}
	groups := map[string]map[string]*series{"gen_ai.client.operation.duration": {}, "execute_tool.duration": {}, "agent.outcome": {}}
	traces := 0
	number := func(v any) int { f, _ := v.(float64); return int(f) }
	for _, path := range paths {
		spans, err := readSpans(path)
		if err != nil {
			continue
		}
		if conversation != "" && !slices.ContainsFunc(spans, func(r telemetry.Record) bool { return r.Attributes["gen_ai.conversation.id"] == conversation }) {
			continue
		}
		traces++
		for _, span := range spans {
			op, _ := span.Attributes["gen_ai.operation.name"].(string)
			group, name := "", ""
			switch op {
			case "chat", "embeddings":
				model, _ := span.Attributes["gen_ai.request.model"].(string)
				group, name = "gen_ai.client.operation.duration", op+" "+model
				if purpose, ok := span.Attributes["agent.purpose"].(string); ok {
					name += " · " + purpose
				}
			case "execute_tool":
				tool, _ := span.Attributes["gen_ai.tool.name"].(string)
				group, name = "execute_tool.duration", tool
			case "invoke_agent":
				outcome, _ := span.Attributes["agent.outcome"].(string)
				group, name = "agent.outcome", outcome
			}
			if group == "" {
				continue
			}
			item := groups[group][name]
			if item == nil {
				item = &series{Name: name}
				groups[group][name] = item
			}
			item.Count++
			if span.Status == "error" {
				item.Errors++
			}
			item.duration = append(item.duration, float64(span.End.Sub(span.Start).Microseconds())/1000)
			item.Input += number(span.Attributes["gen_ai.usage.input_tokens"])
			item.Output += number(span.Attributes["gen_ai.usage.output_tokens"])
			item.Cached += number(span.Attributes["gen_ai.usage.cache_read.input_tokens"])
		}
	}
	result := map[string]any{"traces": traces}
	for group, items := range groups {
		list := []*series{}
		for _, item := range items {
			slices.Sort(item.duration)
			// 最近秩法：第 ceil(p×n) 个值。样本少时 P95 就是最大值附近，读数要结合 count。
			rank := func(p float64) float64 { return item.duration[max(0, int(math.Ceil(p*float64(len(item.duration))))-1)] }
			item.P50, item.P95 = rank(0.5), rank(0.95)
			list = append(list, item)
		}
		slices.SortFunc(list, func(a, b *series) int { return b.Count - a.Count })
		result[group] = list
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(result)
}

// B0：agent 发来的协议消息。通知原样存成 notify 事件（页面按方法名还原过程）；
// 联网审批请求存成 approval 事件，并记下请求ID，等用户在页面上点“允许 / 拒绝”时回复。
func (r *run) handle(m protocol.Message) {
	if m.IsRequest() {
		if m.Method != "network/requestApproval" {
			r.mu.Lock()
			conn := r.conn
			r.mu.Unlock()
			if conn != nil {
				conn.Reply(*m.ID, nil, &protocol.Error{Code: -32601, Message: "观测台不支持 " + m.Method})
			}
			return
		}
		var params struct {
			Domain string `json:"domain"`
		}
		json.Unmarshal(m.Params, &params)
		r.mu.Lock()
		if r.approvals != nil {
			r.approvals[params.Domain] = append(r.approvals[params.Domain], *m.ID)
		}
		r.mu.Unlock()
		r.add(event{Kind: "approval", Method: m.Method, RPC: *m.ID, Body: m.Params})
		return
	}
	r.add(event{Kind: "notify", Method: m.Method, Body: m.Params})
}
