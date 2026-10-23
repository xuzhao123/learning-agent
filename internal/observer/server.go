package observer

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"crypto/subtle"
	"embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"math"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"learning-agent/internal/agent"
	"learning-agent/internal/audit"
	"learning-agent/internal/llm"
	"learning-agent/internal/protocol"
	"learning-agent/internal/telemetry"
	"learning-agent/internal/tenant"
	"learning-agent/internal/tools"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel/attribute"
	semconv "go.opentelemetry.io/otel/semconv/v1.41.0"
	"go.opentelemetry.io/otel/trace"
)

// 页面在 web/：外壳 index.html、样式 app.css 和按功能拆开的原生 ES 模块（入口 main.js），不打包，打进同一个程序。
//
//go:embed web
var webFS embed.FS

const contextLabTitle = "Day 3 上下文实验（33轮对话 + 大工具输出）"
const ragLabTitle = "Day 4 检索实验（10题无检索 / 有检索对比）"

// 观测系统不读取 agent 源码：它代理模型请求、转发终端输出，从协议数据还原过程。
type event struct {
	Seq        int             `json:"seq"`
	Time       time.Time       `json:"time"`
	Kind       string          `json:"kind"` // request、response、stdout、stderr、exit、notify、delta、rewind 等
	Call       int             `json:"call,omitempty"`
	Status     int             `json:"status,omitempty"`
	Millis     int64           `json:"ms,omitempty"`
	Body       json.RawMessage `json:"body,omitempty"`
	Text       string          `json:"text,omitempty"`
	Embedding  string          `json:"embedding,omitempty"`
	Purpose    string          `json:"purpose,omitempty"` // 请求用途：main、compact、memory_contextualize、memory_rerank
	Memory     bool            `json:"memory,omitempty"`
	MemoryTTL  string          `json:"memory_ttl,omitempty"`
	Skills     []string        `json:"skills,omitempty"` // 本对话启用的 skill 与 MCP server 快照，续聊沿用
	MCP        []mcpServer     `json:"mcp,omitempty"`
	Subagents  bool            `json:"subagents,omitempty"`        // 本对话允许 spawn_agent，续聊沿用
	AskUser    bool            `json:"ask_user,omitempty"`         // D34：本对话提供 ask_user，模型可以在运行中向人提问，续聊沿用
	Browser    bool            `json:"browser,omitempty"`          // 本对话启用浏览器工具，续聊沿用
	DSSearch   bool            `json:"ds_search,omitempty"`        // 本对话启用 DeepSeek 搜索工具，续聊沿用
	Disallowed []string        `json:"disallowed_tools,omitempty"` // 设置快照：关掉的单个工具（内置工具、web_search、open_page），续聊沿用；旧存档没有，表示都开
	Provider   string          `json:"provider,omitempty"`         // 本对话的模型供应商（-provider），续聊沿用
	Model      string          `json:"model,omitempty"`            // 本对话的具体模型（agent 的 -model）；空表示用这家供应商的默认值
	Bash       bool            `json:"bash,omitempty"`             // 本对话启用 bash 沙箱，续聊沿用
	RAG        bool            `json:"rag,omitempty"`              // 设置快照：本对话启用知识库检索，续聊沿用
	Workspace  string          `json:"workspace,omitempty"`        // 设置快照：本对话的工作区名字与开始时解析出的目录，续聊沿用
	WorkDir    string          `json:"workspace_dir,omitempty"`    //
	MaxSteps   int             `json:"max_steps,omitempty"`        // 设置快照：模型请求预算（-1 不限）、上下文窗口与工具输出上限；旧存档没有，按 agent 默认值
	Window     int             `json:"context_window,omitempty"`   //
	ToolOutput int             `json:"tool_output,omitempty"`      //
	Permission string          `json:"permission,omitempty"`       // D25：本轮的权限模式（续聊可以换）与权限规则快照
	Rules      []string        `json:"permission_rules,omitempty"`
	Hooks      string          `json:"hooks,omitempty"`          // D26：平台设置里的 hooks 配置原文快照，续聊沿用
	NetAllow   []string        `json:"net_allow,omitempty"`      // 本轮沙箱可访问的域名，记在 start/continue 上：start 上是设置里的白名单，continue 上再加上本对话批准过的
	Task       string          `json:"task,omitempty"`           // 发出这次请求的 agent 任务ID（X-Agent-Task）
	Sub        bool            `json:"sub,omitempty"`            // 请求来自子 agent：不进入父对话流、不参与续聊恢复
	Trace      string          `json:"trace,omitempty"`          // D13：start/continue 上记这次交互的 trace_id
	Span       string          `json:"span,omitempty"`           // D13：request 上记 agent 的 chat span ID（来自 traceparent 头）
	Method     string          `json:"method,omitempty"`         // B0：notify / approval 事件的协议方法名，参数在 Body
	RPC        int64           `json:"rpc,omitempty"`            // B0：agent 发来的请求ID（approval），回复时带回
	ASeq       int64           `json:"aseq,omitempty"`           // B0 接管：这条事件对应的协议消息在 agent 一侧的序号；重启后从最大的序号之后补齐
	Chunks     int             `json:"chunks,omitempty"`         // B1：流式响应的 chunk 数；Body 是按 chunk 拼回的完整响应
	FirstToken int64           `json:"first_token_ms,omitempty"` // B1：从转发请求到收到第一段正文或推理的毫秒数
	Protocol   bool            `json:"protocol,omitempty"`       // B0：start/continue 上标记这一轮经协议运行；之前的存档没有，页面据此启用旧存档兼容
	Eval       string          `json:"eval,omitempty"`           // D16：由评测发起的试次（评测ID · 用例 #序号），对话列表里不显示
	Source     string          `json:"source,omitempty"`         // 导入的对话：codex 或 claude-code，记在 start 上；只读，不能续聊
}

type run struct {
	ID, Query string
	Tenant    string // 对话所属的租户；空是平台。只有同一租户的账号能看到和操作它
	Eval      string // D16：评测发起的试次，见 event.Eval
	Source    string // 从 Codex / Claude Code 导入的对话，见 event.Source
	Done      bool

	mu         sync.Mutex
	chains     map[string]*chain // O20：每个任务最近一次主请求，代理据此把存档写成增量
	calls      int
	seq        int // 最后一个事件的序号：只增不减。delta 只在内存里也占序号，所以重启后要从存档里最大的序号接着编
	events     []event
	changed    chan struct{}
	file       *os.File
	proc       *os.Process // 正在运行的 agent 进程；强制结束时向它的进程组发 SIGKILL
	stopping   bool        // 已请求过一次停止；再按一次强制结束
	rootTask   string      // 本次启动的父 agent 任务ID：启动后第一个带任务ID的请求一定来自父 agent
	token      string      // 本次启动的 agent 进程访问模型代理用的令牌（AGENT_PROXY_TOKEN），每次启动重新生成
	after      int64       // 接管时已经存下的最后一条协议消息的序号：补发里不超过它的不再记一遍（见 live.go）
	exitStatus string      // agent 退出前用 process/exit 报的状态；接管的进程拿不到退出码，靠它
	// B0：与正在运行的 agent 之间的协议连接，以及它在等待答复的联网审批（域名 → 请求ID）。
	conn      *protocol.Conn
	approvals map[string][]int64
	toolWaits map[string]toolWait // D20：等待确认的高风险操作（调用ID → 请求）
	asks      map[string]int64    // D34：模型在等人回答的提问（ask_user 的调用ID → 请求ID）
}

type toolWait struct {
	rpc                                int64
	Task, Tool, Risk, Arguments, Scope string // Scope：外发类可以“本对话内都允许”的范围，由 agent 给出
}

type server struct {
	agentDir, addr string
	mu             sync.Mutex
	runs           []*run
	agentBin       string     // 为空表示 -dev（go run .）
	auth           *auth      // 账号与会话（见 auth.go）
	hubMu          sync.Mutex // 保护 .data/hub.json 的读改写
	evals          evals      // D16/D17 评测记录（见 eval.go）
}

// Run 启动观测台：go run . observe [-addr …] [-dev]，须在项目根目录运行。
// 观测台与内置 MCP server 在同一个进程；每次对话仍启动一个独立的 agent 子进程，
// 这样各次运行的全局状态、终端输出和故障互不影响，模型请求也照旧经过本机代理记录。
func Run(args []string, mcpHandler http.Handler) error {
	flags := flag.NewFlagSet("observe", flag.ContinueOnError)
	addr := flags.String("addr", "127.0.0.1:8090", "观测页面地址")
	dev := flags.Bool("dev", false, "每次对话用 go run . 启动 agent：改完 agent 代码不必重启观测台，代价是每次多一次编译")
	addUserName := flags.String("add-user", "", "添加或重设一个登录账号（密码从标准输入读）后退出")
	userTenant := flags.String("tenant", "", "配合 -add-user：账号所属的租户；不给时是平台账号")
	features := flags.String("features", "", "配合 -add-user -tenant：这个租户可用的功能，逗号分隔（"+strings.Join(allFeatures, ",")+"）；不给时不改")
	maxRunning := flags.Int("max-running", 0, "配合 -add-user -tenant：这个租户同时运行的对话上限；0 表示不改（默认 2）")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("observe 只接受 -addr、-dev，或 -add-user [-tenant -features -max-running]")
	}
	if *addUserName != "" {
		return addUser(*addUserName, *userTenant, *features, *maxRunning)
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
	authState, err := loadAuth()
	if err != nil {
		return err
	}
	hooksBase = dir
	s := &server{agentDir: dir, addr: *addr, agentBin: agentBin, auth: authState, evals: evals{jobs: map[string]*evalJob{}}}
	s.loadRuns()
	mux := http.NewServeMux()
	mux.Handle("GET /slides/", http.StripPrefix("/slides/", http.FileServer(http.Dir(filepath.Join(dir, "docs", "slides")))))
	web, _ := fs.Sub(webFS, "web")
	page, err := fs.ReadFile(web, "index.html")
	if err != nil {
		return err
	}
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		w.Write(page)
	})
	// 样式与模块：embed 的文件没有修改时间，浏览器拿不到 Last-Modified；no-cache 让它每次都来问，换了程序就拿到新文件。
	files := http.StripPrefix("/ui/", http.FileServerFS(web))
	mux.HandleFunc("GET /ui/", func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")
		files.ServeHTTP(w, req)
	})
	mux.HandleFunc("GET /login", s.loginPage)
	mux.HandleFunc("POST /login", s.loginPage)
	mux.HandleFunc("POST /logout", s.logoutUser)
	mux.HandleFunc("GET /me", s.me)
	mux.HandleFunc("GET /models", s.listModels)
	mux.HandleFunc("POST /attachments", s.uploadAttachment)
	mux.HandleFunc("GET /attachments/{name}", s.serveAttachment)
	mux.HandleFunc("GET /runs", s.listRuns)
	mux.HandleFunc("POST /runs", s.startRun)
	mux.HandleFunc("POST /runs/import", s.importRun)
	mux.HandleFunc("POST /runs/{id}/messages", s.continueRun)
	mux.HandleFunc("GET /runs/{id}/events", s.streamEvents)
	mux.HandleFunc("POST /runs/{id}/stop", s.stopRun)
	mux.HandleFunc("POST /runs/{id}/network", s.decideNetwork)
	mux.HandleFunc("POST /runs/{id}/approval", s.decideTool)
	mux.HandleFunc("POST /runs/{id}/answer", s.answerQuestion)
	mux.HandleFunc("POST /runs/{id}/steer", s.steerRun)
	mux.HandleFunc("POST /runs/{id}/rewind", s.rewindRun)
	mux.HandleFunc("GET /runs/{id}/files", s.listFiles)
	mux.HandleFunc("GET /runs/{id}/files/{path...}", s.downloadFile)
	mux.HandleFunc("DELETE /runs/{id}/files/{path...}", s.deleteFile)
	mux.HandleFunc("POST /runs/{id}/files", s.uploadFile)
	mux.HandleFunc("GET /audit", s.listAudit)
	mux.HandleFunc("POST /llm/{id}", s.proxy)
	mux.HandleFunc("GET /browser/{task}/{call}", s.browserFrame)
	mux.HandleFunc("GET /traces/{id}", s.traceSpans)
	mux.HandleFunc("GET /trace-metrics", s.traceMetrics)
	// 评测与归因读写平台空间（.data/eval、平台检查点），只给平台账号。
	mux.HandleFunc("GET /evals", platform(s.listEvals))
	mux.HandleFunc("GET /evals/cases", platform(s.evalCases))
	mux.HandleFunc("GET /evals/{id}", platform(s.getEval))
	mux.HandleFunc("POST /evals", platform(s.startEval))
	mux.HandleFunc("POST /evals/{id}/{action}", platform(s.evalAction))
	mux.HandleFunc("POST /runs/{id}/attribute", platform(s.attributeRun))
	mux.HandleFunc("GET /memory", s.listMemory)
	mux.HandleFunc("POST /memory/{id}/forget", s.forgetMemory)
	// Skills/MCP 是平台配置：所有账号可以查看、使用已开启的；只有平台账号能修改。
	mux.HandleFunc("GET /settings", s.getSettings)
	mux.HandleFunc("GET /commands", s.listCommands)
	mux.HandleFunc("PUT /settings", s.putSettings("user"))
	mux.HandleFunc("PUT /settings/platform", platform(s.putSettings("platform")))
	mux.HandleFunc("POST /settings/workspaces", platform(s.addWorkspace))
	mux.HandleFunc("DELETE /settings/workspaces/{name}", platform(s.deleteWorkspace))
	mux.HandleFunc("GET /hub", s.listHub)
	mux.HandleFunc("GET /hub/enabled", s.enabledHub)
	mux.HandleFunc("POST /hub/skills", platform(s.createSkill))
	mux.HandleFunc("DELETE /hub/skills/{name}", platform(s.deleteSkill))
	mux.HandleFunc("POST /hub/skills/{name}/enabled", platform(s.toggleSkill))
	mux.HandleFunc("POST /hub/mcp", platform(s.addMCP))
	mux.HandleFunc("DELETE /hub/mcp/{name}", platform(s.deleteMCP))
	mux.HandleFunc("POST /hub/mcp/{name}/enabled", platform(s.toggleMCP))
	mux.HandleFunc("POST /hub/mcp/{name}/test", platform(s.testMCP))
	// 内置远程 MCP：同一进程直接处理，不另开端口、不转发；同样只接受本机同源请求。
	mux.Handle("/mcp", sameOrigin(mcpHandler))
	mode := "直接执行当前程序（改了 agent 代码需重启观测台）"
	if *dev {
		mode = "go run .（每次读取最新源码）"
	}
	fmt.Printf("观测页面：http://%s\n远程 MCP：http://%s/mcp（内置 calculator，Streamable HTTP）\nAgent：项目根目录 · %s\n模型上游：由每个 agent 按 -provider 在请求头声明\n", *addr, *addr, mode)
	fmt.Printf("登录账号：%s（%d 个）\n", usersPath, len(authState.accounts))
	return http.ListenAndServe(*addr, s.protect(mux))
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
	// 只在当前账号的空间里找：别的租户的任务ID拼不出它的画面路径。
	http.ServeFile(w, req, filepath.Join(s.agentDir, tenant.Path(current(req).Tenant, "browser", task, call+".jpg")))
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
	// 普通对话只选模型和工作区，工具来自设置（见 settings.go）；实验仍按各自的固定参数单独运行。
	var input struct {
		Query              string      `json:"query"`
		ContextLab         bool        `json:"context_lab"`
		RAGLab             bool        `json:"rag_lab"`
		SecurityLab        string      `json:"security_lab"`
		SecurityProtection bool        `json:"security_protection"`
		Embedding          string      `json:"embedding"`
		Provider           string      `json:"provider"`
		Model              string      `json:"model"`      // 第二级下拉选的具体模型；空：用这家供应商的默认值
		Workspace          *string     `json:"workspace"`  // nil：用设置里的默认工作区；""：本对话不用工作区
		Permission         string      `json:"permission"` // 空：用设置里的默认权限模式
		Images             []attachRef `json:"images"`     // 附图：先经 POST /attachments 存好，这里只带引用
	}
	if json.NewDecoder(req.Body).Decode(&input) != nil {
		http.Error(w, "请求必须是JSON", http.StatusBadRequest)
		return
	}
	input.Query = strings.TrimSpace(input.Query)
	u := current(req)
	settings, _, err := s.effectiveFor(u)
	if err != nil {
		http.Error(w, "无法读取设置："+err.Error(), http.StatusInternalServerError)
		return
	}
	if input.Embedding == "" {
		input.Embedding = settings.Values["embedding"]
	}
	if input.Embedding != "ark" && input.Embedding != "local" {
		http.Error(w, "embedding须为ark或local", http.StatusBadRequest)
		return
	}
	if input.SecurityLab != "" && (!slices.Contains(tools.SecuritySampleIDs, input.SecurityLab) || input.ContextLab || input.RAGLab) {
		http.Error(w, "安全学习实验只允许 clean、answer、tool，且不能与其他实验组合", http.StatusBadRequest)
		return
	}
	lab := input.ContextLab || input.RAGLab || input.SecurityLab != ""
	if (!lab && input.Query == "") || (lab && input.Query != "") || (input.ContextLab && input.RAGLab) {
		http.Error(w, "普通问答需要query；实验请单独运行", http.StatusBadRequest)
		return
	}
	if input.Provider == "" {
		input.Provider = settings.Values["provider"]
	}
	// 具体模型同样有默认值：设置里的“默认具体模型”，空表示用这家供应商的默认值。
	// 页面传了就以页面为准；它跟其他启动参数一起写进检查点，续聊、续跑、回放与子 agent 沿用。
	// 设置里的模型不属于这次选的供应商时（页面换了供应商）不带过去，免得被下面的清单校验拒绝。
	if input.Model == "" {
		input.Model = modelFor(input.Provider, settings.Values["model"])
	}
	// 供应商名由 agent 校验（路由表在 llm 包）；这里只挡住不像名字的输入。
	if !providerPattern.MatchString(input.Provider) {
		http.Error(w, "provider 只能是小写字母", http.StatusBadRequest)
		return
	}
	// 具体模型只能是这家供应商清单里的一个：清单同样来自 llm.Providers，页面不做第二份。
	if input.Model != "" {
		p, ok := llm.FindProvider(input.Provider)
		if _, found := p.FindModel(input.Model); !ok || !found {
			http.Error(w, "模型不在 "+input.Provider+" 的可选清单里", http.StatusBadRequest)
			return
		}
	}
	// 附图：实验的工具范围与数据都是固定的，不带图；普通对话要模型能看图才收，
	// 与 agent 发请求前的处理一致（不能看图的模型会把图换成说明文字，那等于白附）。
	if len(input.Images) > 0 {
		if lab {
			http.Error(w, "实验不能带图", http.StatusBadRequest)
			return
		}
		if !s.checkImages(w, u.Tenant, input.Images) {
			return
		}
		if model, vision := visionModel(input.Provider, input.Model); !vision {
			http.Error(w, "模型 "+model+" 不能看图，换一个能看图的模型再附图", http.StatusBadRequest)
			return
		}
	}
	// 工作区只能从这个账号可选的列表里挑；网页给的是名字，目录由服务端按登记解析。
	workspace := settings.Values["workspace"]
	if input.Workspace != nil {
		workspace = *input.Workspace
	}
	var workDir string
	if workspace != "" && !lab {
		i := slices.IndexFunc(settings.Workspaces, func(w workspaceDir) bool { return w.Name == workspace })
		if i < 0 || !settings.on("coding") {
			http.Error(w, "不能使用工作区 "+workspace+"：没有登记、编码工具没打开，或租户没有开通", http.StatusBadRequest)
			return
		}
		workDir = settings.Workspaces[i].Path
	} else {
		workspace = ""
	}
	if lab && !u.platform() && !s.auth.policy(u.Tenant).allows("labs") {
		http.Error(w, "租户 "+u.Tenant+" 没有开通功能：labs", http.StatusForbidden)
		return
	}
	if !s.underQuota(w, u) {
		return
	}
	// 设置快照：这一刻打开的工具，写进 start 事件，续聊沿用。
	snap := event{Kind: "start", RAG: settings.on("rag"), Memory: settings.on("memory"), MemoryTTL: settings.Values["memory_ttl"], Subagents: settings.on("subagents"), AskUser: settings.on("ask_user") && !lab,
		Browser: settings.on("web_search") || settings.on("open_page"), DSSearch: settings.on("ds_search"), Provider: input.Provider, Model: input.Model, Bash: settings.on("bash"), Workspace: workspace, WorkDir: workDir,
		Permission: settings.Values["permission_mode"], Rules: ruleLines(settings.Values["permission_rules"])}
	for _, name := range singleTools {
		if !settings.on(name) {
			snap.Disallowed = append(snap.Disallowed, name)
		}
	}
	if input.Permission != "" {
		if valid("permission_mode", input.Permission, settingsFile{}) != nil {
			http.Error(w, "permission 取 plan、ask、edit 或 full", http.StatusBadRequest)
			return
		}
		snap.Permission = input.Permission
	}
	if snap.Hooks, err = hooksConfig(settings.Values["hooks"]); err != nil {
		http.Error(w, "平台设置里的 hooks 无法使用："+err.Error(), http.StatusInternalServerError)
		return
	}
	snap.MaxSteps, _ = strconv.Atoi(settings.Values["max_steps"])
	if snap.MaxSteps == 0 {
		snap.MaxSteps = -1 // 设置里 0 表示不限；快照里 0 留给“旧存档没有这项、按 agent 默认值”
	}
	snap.Window, _ = strconv.Atoi(settings.Values["context_window"])
	if snap.Window == 0 {
		// R5：设置里是“按模型”，取路由表里这个模型核对过的输入上限；不知道时用 262144（改成按模型之前的默认值）。
		snap.Window = cmp.Or(llm.WindowOf(input.Provider, input.Model), 262144)
	}
	snap.ToolOutput, _ = strconv.Atoi(settings.Values["tool_output"])
	if settings.on("skills") {
		snap.Skills = settings.Skills
	}
	if settings.on("mcp") {
		snap.MCP = settings.MCP
	}
	if !snap.RAG && !snap.Memory {
		input.Embedding = ""
	}
	if snap.Bash {
		snap.NetAllow = netDomains(settings.Values["net_allow"])
	}
	var args []string
	if !lab {
		snap.Embedding = input.Embedding
		args = snapshotArgs(snap, snap.NetAllow)
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
	if input.SecurityLab != "" {
		args = []string{"-security-lab", "-max-steps", "4", "-reasoning-effort", "minimal"}
		if input.SecurityProtection {
			args = append(args, "-security-protection")
		}
		input.Query = tools.SecurityQuestion(input.SecurityLab)
	}
	if input.RAGLab {
		args = append(args, "-embedding", input.Embedding)
	}
	if lab {
		args = append(args, "-provider", input.Provider)
		if input.Model != "" {
			args = append(args, "-model", input.Model)
		}
		snap = event{Kind: "start", Provider: input.Provider, Model: input.Model, Embedding: input.Embedding}
	}
	// 对话 ID：随机 UUID（v4），不含时间，与观测台在哪个时区启动无关。
	// 早期存档的 ID 是“20261006-213820.763”这样的本地时间，照常读回，不改名，旧链接仍能打开。
	id := uuid.NewString()
	file, err := s.createRunFile(u.Tenant, id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	r := &run{ID: id, Tenant: u.Tenant, Query: strings.TrimSpace(input.Query), changed: make(chan struct{}), file: file}
	s.mu.Lock()
	s.runs = append(s.runs, r)
	s.mu.Unlock()
	ctx, span := r.beginTurn(input.Query)
	snap.Text, snap.Trace, snap.Protocol = r.Query, span.SpanContext().TraceID().String(), true
	r.add(snap)
	var turn map[string]any
	if !input.ContextLab && !input.RAGLab {
		turn = map[string]any{"question": input.Query}
		if len(input.Images) > 0 {
			turn["images"] = input.Images
		}
	}
	s.launch(ctx, r, args, turn, u.Name)
	json.NewEncoder(w).Encode(r.summary())
}

// GET /models：输入框第二级下拉的候选项。路由表只存在 llm 包里，这里按它读出来给页面，不做第二份。
// default 是不选具体模型时 agent 实际会用的那个：.env 或环境变量里的 LLM_MODEL / DEEPSEEK_MODEL 覆盖路由表默认值，
// agent 子进程继承观测台的环境、在同一目录读 .env，所以按同样的规则解析。
// 每个模型带上能不能看图：页面据此禁用附图按钮，服务端收图时同样按这条规则判断。
// 供应商的下拉（输入框与设置页）也从这里来：label 是显示名，error 是本机没配好的原因（缺密钥或地址），页面据此灰掉。
func (s *server) listModels(w http.ResponseWriter, req *http.Request) {
	type providerModels struct {
		Name    string      `json:"name"`
		Label   string      `json:"label"`
		Default string      `json:"default"`
		Error   string      `json:"error,omitempty"`
		Models  []llm.Model `json:"models"`
	}
	out := []providerModels{}
	for _, p := range llm.Providers {
		def, problem := p.Model, ""
		config, err := llm.LoadConfig(p.Name)
		if config.Model != "" { // 没有密钥时仍会解析出模型
			def = config.Model
		}
		if err != nil {
			problem = err.Error()
		}
		out = append(out, providerModels{Name: p.Name, Label: p.Label, Default: def, Error: problem, Models: p.Models})
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(out)
}

// 对话存档按租户放：.data/tenants/<租户>/runs/<对话ID>.jsonl；平台是 .data/runs。
func (s *server) runsDir(space string) string {
	return filepath.Join(s.agentDir, tenant.Path(space, "runs"))
}

func (s *server) createRunFile(space, id string) (*os.File, error) {
	if err := os.MkdirAll(s.runsDir(space), 0o700); err != nil {
		return nil, err
	}
	return os.OpenFile(filepath.Join(s.runsDir(space), id+".jsonl"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
}

// 同一租户同时运行的对话数有上限；平台账号不限。到上限时返回 429，页面提示稍后再试。
func (s *server) underQuota(w http.ResponseWriter, u *user) bool {
	if u.platform() {
		return true
	}
	limit, running := s.auth.policy(u.Tenant).maxRunning(), 0
	s.mu.Lock()
	for _, r := range s.runs {
		r.mu.Lock()
		if r.Tenant == u.Tenant && r.proc != nil {
			running++
		}
		r.mu.Unlock()
	}
	s.mu.Unlock()
	if running >= limit {
		http.Error(w, fmt.Sprintf("租户 %s 已有 %d 个对话在运行（上限 %d），请等其中一个结束", u.Tenant, running, limit), http.StatusTooManyRequests)
		return false
	}
	return true
}

// runOf 取路径里的对话，只返回当前账号所属租户的：别的租户的对话一律当作不存在（404），不透露它是否存在。
func (s *server) runOf(w http.ResponseWriter, req *http.Request) *run {
	r := s.find(req.PathValue("id"))
	if r == nil || r.Tenant != current(req).Tenant {
		http.Error(w, "找不到这条对话", http.StatusNotFound)
		return nil
	}
	return r
}

// 同一条对话一次只处理一个提问；恢复历史不会重新执行已经完成的工具。
func (s *server) continueRun(w http.ResponseWriter, req *http.Request) {
	r := s.runOf(w, req)
	if r == nil {
		return
	}
	if !s.underQuota(w, current(req)) {
		return
	}
	var input struct {
		Query      string      `json:"query"`
		Permission string      `json:"permission"` // D25：权限模式可以每轮换（和 Codex 的 /approvals、Claude Code 的 Shift+Tab 一样）；工具与规则沿用快照
		Images     []attachRef `json:"images"`     // 附图：同新对话，只带 POST /attachments 得到的引用
	}
	if json.NewDecoder(req.Body).Decode(&input) != nil || strings.TrimSpace(input.Query) == "" {
		http.Error(w, "请填写续聊问题", http.StatusBadRequest)
		return
	}
	if input.Permission != "" && valid("permission_mode", input.Permission, settingsFile{}) != nil {
		http.Error(w, "permission 取 plan、ask、edit 或 full", http.StatusBadRequest)
		return
	}
	if len(input.Images) > 0 && !s.checkImages(w, r.Tenant, input.Images) {
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
	file, err := os.OpenFile(filepath.Join(s.runsDir(r.Tenant), r.ID+".jsonl"), os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		r.mu.Unlock()
		http.Error(w, "无法追加对话记录", http.StatusInternalServerError)
		return
	}
	// 续聊沿用这条对话开始时的设置快照：最近一次 start/continue 事件为准，之后改设置不影响这条对话。
	var snap event
	for _, e := range r.events {
		if e.Kind == "start" || e.Kind == "continue" {
			snap = e
		}
	}
	snap.Kind, snap.Text, snap.Embedding = "continue", strings.TrimSpace(input.Query), history.Embedding
	if input.Permission != "" {
		snap.Permission = input.Permission
	}
	snap.RAG = snap.RAG || history.RAG // B0 之前的存档没有 rag 字段，按上一次请求里有没有 search_docs 判断
	// 模型跟着检查点走：续聊用上一轮实际用的模型（agent 的 history.Model），换模型要新建对话。
	if history.Model != "" {
		snap.Model = history.Model
	}
	// 带图时按这条对话实际用的模型判断能不能看图（snap.Model 为空就是 snap.Provider 的默认值）。
	if len(input.Images) > 0 {
		if model, vision := visionModel(snap.Provider, snap.Model); !vision {
			file.Close()
			r.mu.Unlock()
			http.Error(w, "模型 "+model+" 不能看图，换一个能看图的模型再附图", http.StatusBadRequest)
			return
		}
	}
	snap.Trace, snap.Protocol, snap.Status, snap.Seq, snap.Time = "", true, 0, 0, time.Time{}
	r.file, r.Done = file, false
	r.mu.Unlock()
	snap.NetAllow = r.netAllow() // netAllow 自己加锁，要在放开 r.mu 之后调用
	ctx, span := r.beginTurn(snap.Text)
	snap.Trace = span.SpanContext().TraceID().String()
	r.add(snap)
	args := []string{}
	if history.Effort != "" {
		args = append(args, "-reasoning-effort", history.Effort)
	}
	args = append(args, snapshotArgs(snap, r.netAllow())...)
	// 问题、续聊上下文与续接关系放进 turn/start。每一轮都用新的检查点ID，旧检查点保持原样（“一个ID一个问题”），
	// 续接关系让模型可以按 task_id 续跑之前各轮的子任务。
	turn := map[string]any{"question": strings.TrimSpace(input.Query), "history": history, "continues": history.Continues}
	if len(input.Images) > 0 {
		turn["images"] = input.Images
	}
	s.launch(ctx, r, args, turn, current(req).Name)
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

// B0：agent 以 -app-server 启动，协议走 .data/[tenants/<租户>/]live/ 下的 Unix socket（见 live.go），给人看的终端输出也经协议的 log 通知送来。
// 观测台退出时 agent 照常运行，观测台重启后重新接管（adopt）。turn 为 nil 时（上下文、检索实验）由命令行参数决定要做什么，不发 turn/start。
//
// 多租户：租户对话的 agent 带 -tenant 启动，读写都落在租户目录下；发起人的用户名经 AGENT_USER 进审计。
// 每次启动生成一个代理令牌：模型代理只接受带着本对话当前令牌的请求，别的进程不能往这条对话里塞记录；重新接管时 agent 也要出示它。
func (s *server) launch(ctx context.Context, r *run, args []string, turn map[string]any, userName string) {
	span := trace.SpanFromContext(ctx)
	if r.Tenant != "" {
		args = append([]string{"-tenant", r.Tenant}, args...)
	}
	token := newToken()
	cmd := s.agentCommand(context.Background(), append([]string{"-app-server"}, args...)...)
	cmd.Env = append(os.Environ(), "LLM_API_URL=http://"+s.addr+"/llm/"+r.ID, "AGENT_CONVERSATION_ID="+r.ID, "AGENT_USER="+userName, "AGENT_PROXY_TOKEN="+token, "AGENT_APP_SOCKET="+livePath(r, ".sock"))
	cmd.Env = append(cmd.Env, telemetry.Env(ctx)...)
	os.MkdirAll(s.abs(filepath.Dir(livePath(r, ".sock"))), 0o700)
	s.cleanLive(r)
	// 文件描述符 2 接到文件上：观测台退出后 agent 往这里写也不会出错；正常日志走协议，这里只会有崩溃一类的输出。
	errFile, err := os.OpenFile(s.abs(livePath(r, ".err")), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		r.add(event{Kind: "exit", Text: err.Error()})
		telemetry.End(span, err, "launch_error")
		return
	}
	cmd.Stderr = errFile
	r.mu.Lock()
	r.approvals, r.toolWaits, r.asks, r.token, r.exitStatus = map[string][]int64{}, map[string]toolWait{}, map[string]int64{}, token, ""
	r.mu.Unlock()
	// 自己的进程组：停止时向整组发信号；观测台所在终端的 Ctrl-C 也不会传到 agent。-dev 模式下组里是 go run 和它编译出的 agent。
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	err = cmd.Start()
	errFile.Close()
	if err != nil {
		r.add(event{Kind: "exit", Text: err.Error()})
		telemetry.End(span, err, "launch_error")
		return
	}
	if info, err := json.Marshal(liveInfo{PID: cmd.Process.Pid, Token: token}); err == nil {
		os.WriteFile(s.abs(livePath(r, ".json")), info, 0o600)
	}
	r.mu.Lock()
	r.proc, r.stopping, r.rootTask = cmd.Process, false, ""
	r.mu.Unlock()
	exited := make(chan struct{})
	var waitErr error
	go func() {
		waitErr = cmd.Wait()
		close(exited)
	}()
	go func() {
		conn := s.connect(r, token, 0, exited, "")
		if conn != nil && turn != nil {
			if err := conn.Call(context.Background(), "turn/start", turn, nil); err != nil {
				r.add(event{Kind: "stdout", Text: "turn/start 失败：" + err.Error()})
			}
		}
		if conn != nil {
			<-conn.Closed()
		}
		<-exited
		text := "exit 0"
		if waitErr != nil {
			text = waitErr.Error()
		}
		// 先结束根 span 再记 exit：页面看到 exit 时，interaction 已经写进 trace 文件。
		span.SetAttributes(attribute.String("process.exit", text))
		telemetry.End(span, waitErr, "agent_exit")
		s.finish(r, text)
	}()
}

// snapshotArgs 把设置快照还原成 agent 的启动参数；首问和续聊共用，保证同一条对话每一轮的工具一致。
func snapshotArgs(e event, netAllow []string) []string {
	args := []string{}
	if e.RAG {
		args = append(args, "-rag")
	}
	if (e.RAG || e.Memory) && e.Embedding != "" {
		args = append(args, "-embedding", e.Embedding)
	}
	args = appendMemoryArgs(args, e.Memory, e.MemoryTTL)
	args = appendCapabilityArgs(args, e.Skills, e.MCP)
	if e.Subagents {
		args = append(args, "-subagents")
	}
	if e.AskUser {
		args = append(args, "-ask-user")
	}
	if e.Browser {
		args = append(args, "-browser")
	}
	if e.DSSearch {
		args = append(args, "-ds-search")
	}
	if len(e.Disallowed) > 0 {
		args = append(args, "-disallowed-tools", strings.Join(e.Disallowed, ","))
	}
	if e.Provider != "" {
		args = append(args, "-provider", e.Provider)
	}
	// 具体模型作为启动参数传：它跟其他参数一起写进检查点，中断续跑、归因回放和子 agent 都用同一个模型。
	if e.Model != "" {
		args = append(args, "-model", e.Model)
	}
	if e.Bash {
		args = append(args, "-bash")
		for _, domain := range netAllow {
			args = append(args, "-net-allow", domain)
		}
	}
	if e.WorkDir != "" {
		args = append(args, "-workspace", e.WorkDir)
	}
	if e.Permission != "" {
		args = append(args, "-permission-mode", e.Permission)
	}
	for _, line := range e.Rules {
		effect, rule, _ := strings.Cut(line, ":")
		args = append(args, "-"+strings.TrimSpace(effect), strings.TrimSpace(rule))
	}
	if e.Hooks != "" {
		args = append(args, "-hooks", e.Hooks)
	}
	if e.MaxSteps < 0 {
		args = append(args, "-max-steps", "0")
	}
	for flag, n := range map[string]int{"-max-steps": e.MaxSteps, "-context-window": e.Window, "-tool-output-tokens": e.ToolOutput} {
		if n > 0 {
			args = append(args, flag, strconv.Itoa(n))
		}
	}
	return args
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
func (s *server) listMemory(w http.ResponseWriter, req *http.Request) {
	space := current(req).Tenant
	data, err := os.ReadFile(filepath.Join(s.agentDir, tenant.Path(space, "memory.json")))
	if errors.Is(err, os.ErrNotExist) {
		data, err = []byte(`{"next":0,"memories":[]}`), nil
	}
	if err != nil || !json.Valid(data) {
		http.Error(w, "无法读取 .data/memory.json", http.StatusInternalServerError)
		return
	}
	index, err := os.ReadFile(filepath.Join(s.agentDir, tenant.Path(space, "memory-index", "contexts.json")))
	if err != nil || !json.Valid(index) {
		index = []byte(`{"chunks":{}}`)
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"path": tenant.Path(space, "memory.json"), "now": time.Now(), "file": json.RawMessage(data), "index": json.RawMessage(index)})
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
	u := current(req)
	args := []string{"-memory-forget", id}
	if u.Tenant != "" {
		args = append([]string{"-tenant", u.Tenant}, args...)
	}
	cmd := s.agentCommand(req.Context(), args...)
	cmd.Env = append(os.Environ(), "AGENT_USER="+u.Name)
	output, err := cmd.CombinedOutput()
	if err != nil {
		http.Error(w, strings.TrimSpace(string(output)), http.StatusConflict)
		return
	}
	w.Write(output)
}

// D29：撤销某一轮及之后各轮的文件改动，对话退回到它的上一轮。文件由 agent 的 rewind 命令恢复（和记忆删除一样，
// 改动用户数据的事交给 agent，观测台不自己写工作区）；这里核对这一轮属于这条对话、对话没有在运行，再把结果记进对话记录。
// dry_run 只预览：页面先列出会恢复、删除的文件和冲突，人确认后再真正执行。
func (s *server) rewindRun(w http.ResponseWriter, req *http.Request) {
	r := s.runOf(w, req)
	if r == nil {
		return
	}
	var input struct {
		Turn   string `json:"turn"`
		DryRun bool   `json:"dry_run"`
	}
	if json.NewDecoder(req.Body).Decode(&input) != nil || !taskPattern.MatchString(input.Turn) {
		http.Error(w, "需要这一轮的检查点ID", http.StatusBadRequest)
		return
	}
	r.mu.Lock()
	turns := r.turnIDs()
	running := !r.Done
	r.mu.Unlock()
	if !slices.Contains(turns, input.Turn) {
		http.Error(w, "这一轮不属于这条对话", http.StatusNotFound)
		return
	}
	if running {
		http.Error(w, "对话正在运行，停下之后再撤销", http.StatusConflict)
		return
	}
	u := current(req)
	args := []string{"rewind"}
	if r.Tenant != "" {
		args = append(args, "-tenant", r.Tenant)
	}
	if input.DryRun {
		args = append(args, "-dry-run")
	}
	cmd := s.agentCommand(req.Context(), append(args, input.Turn)...)
	cmd.Env = append(os.Environ(), "AGENT_USER="+u.Name, "AGENT_CONVERSATION_ID="+r.ID)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	output, err := cmd.Output()
	result := json.RawMessage(output)
	if !json.Valid(result) {
		result = nil
	}
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		json.NewEncoder(w).Encode(map[string]any{"error": strings.TrimPrefix(strings.TrimSpace(stderr.String()), "Error: "), "result": result})
		return
	}
	if !input.DryRun {
		// 对话已经结束，记录文件关了：临时以追加方式打开（同联网审批）。
		r.mu.Lock()
		file, err := os.OpenFile(filepath.Join(s.runsDir(r.Tenant), r.ID+".jsonl"), os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			r.mu.Unlock()
			http.Error(w, "文件已撤销，但无法写入对话记录", http.StatusInternalServerError)
			return
		}
		r.file = file
		r.mu.Unlock()
		r.add(event{Kind: "rewind", Text: input.Turn, Body: result})
		file.Close()
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(output)
}

// 这条对话各轮 agent 的检查点ID（turn/started 通知；子 agent 的包在 subagent/message 里，不会混进来）。调用方持有 r.mu。
func (r *run) turnIDs() []string {
	ids := []string{}
	for _, e := range r.events {
		var p struct {
			TurnID string `json:"turn_id"`
		}
		if e.Kind == "notify" && e.Method == "turn/started" && json.Unmarshal(e.Body, &p) == nil && p.TurnID != "" && !slices.Contains(ids, p.TurnID) {
			ids = append(ids, p.TurnID)
		}
	}
	return ids
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
// 上一轮中途停下（停止按钮、超时、出错）时，改从 agent 的检查点接着聊；
// D29 撤销之后同样走检查点：接着的是撤销点的上一轮，它的检查点就是那时模型看到的上下文，和恢复后的文件同一时刻。
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
	bodies := requestBodies(r.events) // O20：存档里的请求可能是增量，先还原成整份
	for i, e := range r.events[start:end] {
		// 记忆背景生成、重排与独立验证是辅助调用：它们的输入不是对话，输出也不是 assistant 的回答，续聊不能从它们恢复。
		// 子 agent 的请求属于另一个上下文，同样跳过。
		if auxPurpose(e.Purpose) || e.Sub {
			continue
		}
		if e.Kind == "request" {
			request.Messages, request.Tools = nil, nil
			if json.Unmarshal(bodies[start+i], &request) != nil || len(request.Messages) == 0 {
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
	data, err := os.ReadFile(filepath.Join(tenant.Path(r.Tenant, "checkpoints"), id+".json"))
	if err != nil {
		return nil, errors.New("找不到这次运行的检查点，无法接着聊：" + id)
	}
	var cp struct {
		Version  int               `json:"version"`
		Messages []json.RawMessage `json:"messages"`
	}
	if json.Unmarshal(data, &cp) != nil || len(cp.Messages) == 0 {
		return nil, errors.New("检查点无效：" + id)
	}
	if err := agent.CheckVersion(id, cp.Version); err != nil {
		return nil, err
	}
	result := &resumeInput{Messages: cp.Messages, WithTools: true}
	// 模型、推理强度与是否检索沿用这条对话最近一次主任务请求。
	bodies := requestBodies(r.events)
	for i := len(r.events) - 1; i >= 0; i-- {
		e := r.events[i]
		if e.Kind != "request" || e.Sub || auxPurpose(e.Purpose) {
			continue
		}
		var request struct {
			Tools  []struct{ Function struct{ Name string } } `json:"tools"`
			Model  string                                     `json:"model"`
			Effort string                                     `json:"reasoning_effort"`
		}
		if json.Unmarshal(bodies[i], &request) == nil {
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
// D29：最近一次是撤销时，返回撤销后对话接着的那一轮（空表示连第一轮也撤销了）。
func (r *run) lastCheckpoint() string {
	for i := len(r.events) - 1; i >= 0; i-- {
		e := r.events[i]
		if e.Kind == "start" || e.Kind == "continue" {
			return ""
		}
		if e.Kind == "rewind" {
			var p struct {
				Continues string `json:"continues"`
			}
			json.Unmarshal(e.Body, &p)
			return p.Continues
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
	// 课堂样本按独立任务对比，不把前一组的资料带进后一组。
	for _, sample := range tools.SecuritySampleIDs {
		if r.Query == tools.SecurityQuestion(sample) {
			return false
		}
	}
	// 用户的网络审批（allow/deny）与撤销（rewind）可能追加在 exit 之后，不算对话进程的事件。
	i, rewound := len(r.events)-1, false
	for i >= 0 && (r.events[i].Kind == "allow" || r.events[i].Kind == "deny" || r.events[i].Kind == "rewind") {
		rewound = rewound || r.events[i].Kind == "rewind"
		i--
	}
	if r.Query == ragLabTitle || r.Source != "" || !r.Done || i < 0 || r.events[i].Kind != "exit" {
		return false
	}
	// D29：撤销之后从撤销点的上一轮接着聊；连第一轮也撤销了，就没有可以接着的对话。
	if rewound {
		return r.lastCheckpoint() != ""
	}
	if r.events[i].Text == "exit 0" {
		return r.calls > 0
	}
	return r.lastCheckpoint() != ""
}

// 启动时读回 runs/*.jsonl，重启后仍能查看、回放和对比历史运行。
// 平台的存档在 .data/runs，各租户的在 .data/tenants/<租户>/runs；所属租户由目录决定。
func (s *server) loadRuns() {
	spaces := []string{""}
	dirs, _ := os.ReadDir(filepath.Join(s.agentDir, ".data", "tenants"))
	for _, d := range dirs {
		if d.IsDir() && tenant.Valid(d.Name()) {
			spaces = append(spaces, d.Name())
		}
	}
	for _, space := range spaces {
		s.loadSpace(space)
	}
}

func (s *server) loadSpace(space string) {
	paths, _ := filepath.Glob(filepath.Join(s.runsDir(space), "*.jsonl"))
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		r := &run{ID: strings.TrimSuffix(filepath.Base(path), ".jsonl"), Tenant: space, Done: true, changed: make(chan struct{})}
		for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
			var e event
			if json.Unmarshal([]byte(line), &e) != nil {
				continue
			}
			r.events = append(r.events, e)
			r.calls, r.seq = max(r.calls, e.Call), max(r.seq, e.Seq)
			if e.Kind == "start" {
				r.Query, r.Eval, r.Source = e.Text, e.Eval, e.Source
			}
			if e.Kind == "request" && r.Query == "" {
				r.Query = firstUserMessage(e.Body)
			}
		}
		s.runs = append(s.runs, r)
		s.adopt(r)
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
	summary := map[string]any{"id": r.ID, "query": r.Query, "done": r.Done, "can_continue": r.canContinue(), "eval": r.Eval, "source": r.Source}
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

// 转发请求与响应原文；Authorization 只转发，不记录。
func (s *server) proxy(w http.ResponseWriter, req *http.Request) {
	r := s.find(req.PathValue("id"))
	if r == nil {
		http.Error(w, "未知运行", http.StatusNotFound)
		return
	}
	// 模型代理不走登录：调用方是 agent 进程。凭启动时发给它的令牌确认是这条对话的 agent（子 agent 继承同一个环境变量）。
	r.mu.Lock()
	token := r.token
	r.mu.Unlock()
	if token == "" || subtle.ConstantTimeCompare([]byte(req.Header.Get("X-Agent-Token")), []byte(token)) != 1 {
		http.Error(w, "代理令牌无效", http.StatusForbidden)
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
	r.add(event{Kind: "request", Call: call, Body: raw(r.compactRequest(task, purpose, s.archiveImages(r.Tenant, body))), Purpose: purpose, Task: task, Sub: sub, Span: spanID})

	start := time.Now()
	// 模型路由在 agent 里：它在 X-Agent-Upstream 里声明这次请求的真实地址（方舟、DeepSeek 或 Coconut），代理照此转发。
	// 只接受 https；明文 http 只给不带密钥的请求（本机的 Coconut 不要密钥），密钥不经明文发出。
	// 浏览器里的网页设不了这个自定义头（跨域预检会被拒），能设它的只有本机进程，而本机进程本来就能直接访问外网。
	key := req.Header.Get("Authorization")
	target, err := url.Parse(req.Header.Get("X-Agent-Upstream"))
	if err == nil && ((target.Scheme != "https" && (target.Scheme != "http" || key != "")) || target.Host == "" || target.User != nil) {
		err = errors.New("X-Agent-Upstream 需要是 https 地址（不带密钥的请求可以是 http）")
	}
	// 上游说 Responses、Messages 协议时，转发前把 chat 形式的请求体换过去；存档留的仍是 chat 形式，页面、续聊和归因不用分协议。
	forward := body
	if err == nil {
		forward, err = llm.ConvertBody(req.Header.Get("X-Agent-Protocol"), body)
	}
	if err != nil {
		r.add(event{Kind: "response", Call: call, Status: http.StatusBadRequest, Text: err.Error(), Purpose: purpose, Task: task, Sub: sub})
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	upstream, err := http.NewRequestWithContext(req.Context(), http.MethodPost, target.String(), bytes.NewReader(forward))
	if err == nil {
		upstream.Header.Set("Content-Type", req.Header.Get("Content-Type"))
		if key != "" {
			upstream.Header.Set("Authorization", key)
		}
		var response *http.Response
		if response, err = http.DefaultClient.Do(upstream); err == nil {
			defer response.Body.Close()
			if response.StatusCode == http.StatusOK && strings.HasPrefix(response.Header.Get("Content-Type"), "text/event-stream") {
				s.relayStream(w, req, r, response, event{Kind: "response", Call: call, Status: response.StatusCode, Purpose: purpose, Task: task, Sub: sub}, start)
				return
			}
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

// B1：流式响应边收边转发，每行原样写给 agent 并立刻 flush；整段缓冲再转发，agent 就看不到流式了。
// 转发的同时用 agent 的同一份拼接逻辑（llm.Stream）还原完整响应：存档仍是 choices[0].message 的结构，
// 页面、续聊恢复和归因不必区分流式与非流式。逐个 chunk 的原文不留档，只记 chunk 数与首字延迟。
func (s *server) relayStream(w http.ResponseWriter, req *http.Request, r *run, response *http.Response, e event, start time.Time) {
	w.Header().Set("Content-Type", response.Header.Get("Content-Type"))
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(response.StatusCode)
	flusher, _ := w.(http.Flusher)
	var stream llm.Stream
	scanner := bufio.NewScanner(response.Body)
	scanner.Buffer(make([]byte, 64*1024), 2<<20)
	var err error
	for scanner.Scan() {
		line := scanner.Text()
		if _, err = io.WriteString(w, line+"\n"); err != nil {
			break // agent 断开了（停止按钮、超时）
		}
		if flusher != nil {
			flusher.Flush()
		}
		// 解析失败不影响转发：判断这次回复能不能用是 agent 的事，存档只是尽量还原。
		if content, reasoning, _ := stream.Add(line); (content != "" || reasoning != "") && e.FirstToken == 0 {
			e.FirstToken = time.Since(start).Milliseconds()
		}
	}
	if err == nil {
		err = scanner.Err()
	}
	e.Millis, e.Chunks = time.Since(start).Milliseconds(), stream.Chunks
	e.Body, _ = json.Marshal(stream.Response())
	if err != nil {
		// 流在中途断了：已转发的部分 agent 收到了，这里如实记下断开原因，499 同样表示 agent 先取消。
		e.Text = err.Error()
		if req.Context().Err() != nil {
			e.Status = 499
		}
	}
	r.add(e)
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
	r.seq++
	e.Seq, e.Time = r.seq, time.Now().UTC() // 统一存 UTC：换时区启动观测台、或别人打开页面，时间都不会错
	r.events = append(r.events, e)
	if line, err := json.Marshal(e); err == nil && e.Kind != "delta" {
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
	r := s.runOf(w, req)
	if r == nil {
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
// Day 12：沙箱网络白名单由用户审批。请求来自 agent 的 request_network_access（模型发起）或沙箱代理（命令连到不在白名单的域名）；
// 用户在页面上点“允许 / 拒绝”，这里记一条 allow/deny 事件（存进对话记录），下一轮续聊时作为 -net-allow 传给 agent。
// “始终允许”另外把域名加进这个账号的个人设置（net_allow），之后新开的对话都带上（同 Claude Code 的“Yes, and don't ask again”）。
// 模型自己无法调用这个接口：它在断网的沙箱里，也没有访问观测台的工具。
var domainPattern = regexp.MustCompile(`^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z]{2,63}$`)

func (s *server) decideNetwork(w http.ResponseWriter, req *http.Request) {
	r := s.runOf(w, req)
	if r == nil {
		return
	}
	var input struct {
		Domain string `json:"domain"`
		Allow  bool   `json:"allow"`
		Always bool   `json:"always"` // 只和 allow 一起用：同时写进个人设置
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
		file, err := os.OpenFile(filepath.Join(s.runsDir(r.Tenant), r.ID+".jsonl"), os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			r.mu.Unlock()
			http.Error(w, "无法写入对话记录", http.StatusInternalServerError)
			return
		}
		r.file = file
		defer file.Close()
	}
	r.mu.Unlock()
	always := input.Allow && input.Always
	detail := "观测台页面"
	if always {
		detail += "；始终允许：写入个人设置"
	}
	// D21：联网批准同样先写审计；之后的续聊会把它作为 -net-allow 传给 agent，agent 那边再记一条。
	if err := audit.Write(req.Context(), audit.Record{Actor: "user", User: current(req).Name, Tenant: r.Tenant, Event: "user_network_decision", Task: r.rootTask, Conversation: r.ID, Target: input.Domain, Risk: "network", Decision: kind, Detail: detail}); err != nil {
		http.Error(w, "审计日志写入失败："+err.Error(), http.StatusInternalServerError)
		return
	}
	var body json.RawMessage
	if always {
		if err := s.alwaysAllow(current(req), input.Domain); err != nil {
			http.Error(w, "无法写入个人设置："+err.Error(), http.StatusInternalServerError)
			return
		}
		body = json.RawMessage(`{"always":true}`)
	}
	r.add(event{Kind: kind, Text: input.Domain, Body: body})
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

// D20：高风险操作确认。agent 发来 tool/requestApproval 后暂停；用户在页面上点“允许 / 拒绝”，这里当场回复。
// 只对正在等待的调用有效：下一项操作要重新确认。外发类的请求带范围时，用户可以选“本对话内都允许”（allow_session），
// 由 agent 记下范围，之后同一范围不再询问。决定记进对话记录，页面重开后仍能看到。
// D34：回答模型的提问（ask_user）。回答是人在对话里说的话，和续聊的问题一样记进存档，不写审计（不是授权）。
func (s *server) answerQuestion(w http.ResponseWriter, req *http.Request) {
	r := s.runOf(w, req)
	if r == nil {
		return
	}
	var input struct {
		CallID string `json:"call_id"`
		Answer string `json:"answer"`
	}
	if json.NewDecoder(io.LimitReader(req.Body, 64<<10)).Decode(&input) != nil || input.CallID == "" || strings.TrimSpace(input.Answer) == "" {
		http.Error(w, "需要 call_id 和非空的 answer", http.StatusBadRequest)
		return
	}
	r.mu.Lock()
	conn := r.conn
	rpc, waiting := r.asks[input.CallID]
	delete(r.asks, input.CallID)
	r.mu.Unlock()
	if !waiting || conn == nil {
		http.Error(w, "agent 没有在等这个问题的回答（已经回答过、已超时或这一轮已结束）", http.StatusConflict)
		return
	}
	body, _ := json.Marshal(map[string]string{"answer": strings.TrimSpace(input.Answer)})
	r.add(event{Kind: "answer", Text: input.CallID, Body: body})
	conn.Reply(rpc, json.RawMessage(body), nil)
	w.WriteHeader(http.StatusNoContent)
}

// D34：运行中插话。agent 把它排进下一次模型请求；这一轮已经结束时返回 409，页面提示用户在结束后照常发送。
// 插话真正送进模型时 agent 会发 userMessage（steer=true）的通知，存档里以那条为准。
func (s *server) steerRun(w http.ResponseWriter, req *http.Request) {
	r := s.runOf(w, req)
	if r == nil {
		return
	}
	var input struct{ Text string }
	if json.NewDecoder(io.LimitReader(req.Body, 64<<10)).Decode(&input) != nil || strings.TrimSpace(input.Text) == "" {
		http.Error(w, "需要非空的 text", http.StatusBadRequest)
		return
	}
	r.mu.Lock()
	conn := r.conn
	r.mu.Unlock()
	if conn == nil {
		http.Error(w, "这条对话没有在运行", http.StatusConflict)
		return
	}
	ctx, cancel := context.WithTimeout(req.Context(), 5*time.Second)
	defer cancel()
	if err := conn.Call(ctx, "turn/steer", map[string]string{"text": strings.TrimSpace(input.Text)}, nil); err != nil {
		http.Error(w, "没有送到：这一轮已经结束（"+err.Error()+"），结束后照常发送即可", http.StatusConflict)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) decideTool(w http.ResponseWriter, req *http.Request) {
	// 确认请求只能由同一租户的账号回答：别的租户连这条对话都找不到。
	r := s.runOf(w, req)
	if r == nil {
		return
	}
	var input struct {
		CallID  string `json:"call_id"`
		Allow   bool   `json:"allow"`
		Session bool   `json:"session"` // 本对话内同一范围都允许；只对带范围的外发类请求有效
	}
	if json.NewDecoder(req.Body).Decode(&input) != nil || input.CallID == "" || len(input.CallID) > 256 {
		http.Error(w, "需要 call_id", http.StatusBadRequest)
		return
	}
	r.mu.Lock()
	conn := r.conn
	wait, waiting := r.toolWaits[input.CallID]
	delete(r.toolWaits, input.CallID)
	r.mu.Unlock()
	if !waiting || conn == nil {
		http.Error(w, "这项操作已不在等待确认（可能已超时或 agent 已结束）", http.StatusConflict)
		return
	}
	decision, detail := "deny", "观测台页面"
	if input.Allow {
		decision = "allow"
	}
	if input.Allow && input.Session {
		if wait.Risk != "outbound" || wait.Scope == "" {
			r.mu.Lock()
			r.toolWaits[input.CallID] = wait
			r.mu.Unlock()
			http.Error(w, "这项操作只能单次允许", http.StatusBadRequest)
			return
		}
		decision, detail = "allow_session", "观测台页面；本对话内都允许："+wait.Scope
	}
	// D21：观测台记下“人点了什么”，与 agent 那边的 approval_decision 互相对照；参数哈希应与 agent 的请求记录一致。
	// 先写审计再回复：审计写不进去，就不把“允许”交给 agent。
	record := audit.Record{Actor: "user", User: current(req).Name, Tenant: r.Tenant, Event: "user_decision", Task: wait.Task, Conversation: r.ID, CallID: input.CallID, Tool: wait.Tool, Risk: wait.Risk, Decision: decision, Detail: detail}
	record.Args(wait.Arguments)
	if err := audit.Write(req.Context(), record); err != nil {
		r.mu.Lock()
		r.toolWaits[input.CallID] = wait
		r.mu.Unlock()
		http.Error(w, "审计日志写入失败，没有回复 agent："+err.Error(), http.StatusInternalServerError)
		return
	}
	r.add(event{Kind: "decision", Text: input.CallID, Body: json.RawMessage(`{"decision":"` + decision + `"}`)})
	conn.Reply(wait.rpc, map[string]string{"decision": decision}, nil)
	json.NewEncoder(w).Encode(map[string]string{"decision": decision})
}

// D21：审计页只读。每次都从头校验整条哈希链，返回最近的 500 条（可按任务过滤）。
func (s *server) listAudit(w http.ResponseWriter, req *http.Request) {
	result, err := audit.Read(current(req).Tenant, strings.TrimSpace(req.URL.Query().Get("task")))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if n := len(result.Records); n > 500 {
		result.Records = result.Records[n-500:]
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(result)
}

// 当前允许的域名：从开始时设置里的白名单出发，按时间顺序处理 allow/deny，后一次决定覆盖前一次。
func (r *run) netAllow() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	list := []string{}
	for _, e := range r.events {
		if e.Kind == "start" {
			list = append(list, e.NetAllow...)
		}
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
	r := s.runOf(w, req)
	if r == nil {
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

func (s *server) listRuns(w http.ResponseWriter, req *http.Request) {
	space := current(req).Tenant
	s.mu.Lock()
	runs := slices.DeleteFunc(append([]*run(nil), s.runs...), func(r *run) bool { return r.Tenant != space })
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
	if !telemetry.ValidTraceID(id) || !s.ownsTrace(current(req).Tenant, id) {
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

// 调用链文件是平台数据（不按租户分），但每个 trace 都由某一次对话产生：只能看自己空间里对话的 trace。
func (s *server) ownsTrace(space, id string) bool {
	s.mu.Lock()
	runs := append([]*run(nil), s.runs...)
	s.mu.Unlock()
	for _, r := range runs {
		if r.Tenant != space {
			continue
		}
		r.mu.Lock()
		found := slices.ContainsFunc(r.events, func(e event) bool { return e.Trace == id })
		r.mu.Unlock()
		if found {
			return true
		}
	}
	return false
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
	// 不带对话时汇总全部 trace（含各租户），只给平台账号；租户账号只能看自己的某个对话。
	if u := current(req); !u.platform() {
		if r := s.find(conversation); r == nil || r.Tenant != u.Tenant {
			http.Error(w, "租户账号只能查看自己对话的指标", http.StatusForbidden)
			return
		}
	}
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
	// 接管时补发的消息里，序号不超过 after 的已经存过：请求只重新登记等待，不再记一遍。
	r.mu.Lock()
	seen := m.Seq > 0 && m.Seq <= r.after
	r.mu.Unlock()
	if m.IsRequest() {
		// agent 退出前报告状态：按顺序处理到这里，说明之前的消息都已记下，答复后它才断开退出。
		if m.Method == "process/exit" {
			var p struct{ Status string }
			json.Unmarshal(m.Params, &p)
			r.mu.Lock()
			r.exitStatus = p.Status
			conn := r.conn
			r.mu.Unlock()
			if conn != nil {
				conn.Reply(*m.ID, nil, nil)
			}
			return
		}
		// D34：模型的提问（ask_user）：记下等待，页面上的人回答后经 answerQuestion 答复。
		if m.Method == "user/requestInput" {
			var p struct {
				CallID string `json:"call_id"`
			}
			json.Unmarshal(m.Params, &p)
			r.mu.Lock()
			if r.asks == nil {
				r.asks = map[string]int64{}
			}
			r.asks[p.CallID] = *m.ID
			r.mu.Unlock()
			if !seen {
				r.add(event{Kind: "approval", Method: m.Method, RPC: *m.ID, Body: m.Params, ASeq: m.Seq})
			}
			return
		}
		if m.Method == "tool/requestApproval" {
			var params struct {
				CallID    string `json:"call_id"`
				Task      string `json:"task_id"` // 子 agent 的请求经父进程转发时带上
				Tool      string `json:"tool"`
				Risk      string `json:"risk"`
				Arguments string `json:"arguments"`
				Scope     string `json:"scope"`
			}
			json.Unmarshal(m.Params, &params)
			r.mu.Lock()
			if params.Task == "" {
				params.Task = r.rootTask
			}
			if r.toolWaits != nil {
				r.toolWaits[params.CallID] = toolWait{*m.ID, params.Task, params.Tool, params.Risk, params.Arguments, params.Scope}
			}
			r.mu.Unlock()
			if !seen {
				r.add(event{Kind: "approval", Method: m.Method, RPC: *m.ID, Body: m.Params, ASeq: m.Seq})
			}
			return
		}
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
		if !seen {
			r.add(event{Kind: "approval", Method: m.Method, RPC: *m.ID, Body: m.Params, ASeq: m.Seq})
		}
		return
	}
	if seen {
		return
	}
	switch m.Method {
	case "log": // 终端日志：和以前从 stderr 读到的一样记成 stdout
		var p struct{ Text string }
		json.Unmarshal(m.Params, &p)
		r.add(event{Kind: "stdout", Text: p.Text, ASeq: m.Seq})
		return
	}
	// B1：item/delta 只给正在看的页面实时显示，不写进运行记录：一次回答会有成百上千条，完整内容已在 response 事件里。
	kind := "notify"
	if protocol.IsDelta(m) {
		kind = "delta"
	}
	r.add(event{Kind: kind, Method: m.Method, Body: m.Params, ASeq: m.Seq})
}
