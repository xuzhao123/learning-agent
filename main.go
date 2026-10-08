package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"

	"learning-agent/internal/agent"
	"learning-agent/internal/browser"
	"learning-agent/internal/eval"
	"learning-agent/internal/labs"
	"learning-agent/internal/llm"
	"learning-agent/internal/mcp"
	"learning-agent/internal/memory"
	"learning-agent/internal/metrics"
	"learning-agent/internal/observer"
	"learning-agent/internal/protocol"
	"learning-agent/internal/queue"
	"learning-agent/internal/retrieval"
	"learning-agent/internal/sandbox"
	"learning-agent/internal/skills"
	"learning-agent/internal/telemetry"
	"learning-agent/internal/tools"

	"go.opentelemetry.io/otel/trace"
)

// 唯一入口：go run . observe 启动观测台（含内置远程 MCP）；go run . queue 运行任务队列；go run . metrics 统计 D15 指标；go run . eval 跑 D16 评测与 D17 归因；其余用法都是 agent 本身。
func main() {
	run := run
	if len(os.Args) > 1 && os.Args[1] == "observe" {
		run = func() error { return observer.Run(os.Args[2:], mcp.Handler()) }
	}
	// 沙箱里的第一个程序（见 internal/sandbox）：转发代理端口后运行命令。不解析其他参数、不读配置。
	if len(os.Args) > 1 && os.Args[1] == "sandbox-init" {
		os.Exit(sandbox.Init(os.Args[2:]))
	}
	service := "agent"
	if len(os.Args) > 1 && os.Args[1] == "queue" {
		run, service = func() error { return queue.Run(os.Args[2:]) }, "queue"
	}
	if len(os.Args) > 1 && os.Args[1] == "observe" {
		service = "observer"
	}
	// D16：评测集与回归；D17：归因卡片与回放验证。
	if len(os.Args) > 1 && os.Args[1] == "eval" {
		run, service = func() error { return eval.Run(os.Args[2:]) }, "eval"
	}
	// D15：只读检查点和 trace，统计 5 个核心指标；不请求模型，也不创建 span。
	if len(os.Args) > 1 && os.Args[1] == "metrics" {
		if err := metrics.Run(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "Error:", err)
			os.Exit(1)
		}
		return
	}
	if slices.Contains(os.Args, "-mcp-serve") {
		service = "mcp-calculator"
	}
	// D13：每个进程各自安装 OpenTelemetry SDK；退出前 shutdown，把缓冲的 span 发完。
	shutdown := telemetry.Start(service)
	err := run()
	shutdown()
	if err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(1)
	}
}

func run() error {
	question := flag.String("question", "", "要交给 agent 的问题")
	historyStdin := flag.Bool("history-stdin", false, "从stdin读取上次模型上下文JSON，配合-question续聊")
	appServer := flag.Bool("app-server", false, "B0：以结构化协议与界面通信（JSON-RPC，一行一条）：stdout 只走协议，日志改走 stderr；问题与续聊上下文由 turn/start 给出")
	flag.StringVar(&agent.Continues, "continues", "", "续聊时上一轮的检查点ID：本轮用新ID，记下续接关系，子任务可按 task_id 引用之前各轮派出的子任务")
	parallel := flag.Int("parallel", 4, "同时执行的工具数，1 到 16")
	maxSteps := flag.Int("max-steps", 10, "模型请求总次数，包含最终回答和上下文摘要")
	retries := flag.Int("retries", 2, "工具失败后额外重试次数，0 到 5")
	flag.BoolVar(&tools.LabEnabled, "lab-tools", false, "启用 Day 2 故障实验工具")
	provider := flag.String("provider", "ark", "模型路由：ark（豆包，方舟）或 deepseek；地址、模型与密钥见 README")
	effort := flag.String("reasoning-effort", "high", "所有请求统一的推理强度：minimal、low、medium或high")
	window := flag.Int("context-window", 12288, "上下文总窗口（学习实验值），输入容量扣除输出预算预留与推理余量")
	output := flag.Int("max-output-tokens", 0, "可选输出上限（含推理）；0不向API传限制，输入预算仍预留4096")
	reserve := flag.Int("reasoning-reserve", 1024, "输入容量额外预留的安全余量")
	toolOutput := flag.Int("tool-output-tokens", 2000, "工具结果写入视图的估算token上限，至少64")
	keepGroups := flag.Int("keep-groups", 2, "压缩后最多保留的最近完整消息组数")
	contextLab := flag.Bool("context-lab", false, "Day 3：33轮真实对话和真实大工具输出实验，建议 -max-steps 60")
	flag.BoolVar(&retrieval.Enabled, "rag", false, "Day 4：启用本地知识库检索工具与来源引用")
	ragLab := flag.Bool("rag-lab", false, "Day 4：10题检索检查与无检索/有检索真实模型对比")
	embeddingMode := flag.String("embedding", "ark", "向量模型：ark（线上方舟）或local（本地纯Go推理）")
	threshold := flag.Float64("min-score", retrieval.MinScore, "检索余弦相似度阈值，-1到1")
	searchQuery := flag.String("search-docs", "", "只执行检索；不调用聊天模型，local模式无需密钥")
	searchK := flag.Int("k", 3, "search-docs 返回的候选数，1到6")
	memoryEnabled := flag.Bool("memory", false, "Day 5：启用跨进程长期记忆（.data/memory.json）")
	memoryTTL := flag.Duration("memory-ttl", 0, "本轮“记住”写入的有效期，如 1m；0 表示不过期")
	memoryLimit := flag.Int("memory-limit", 200, "长期记忆总量上限；超出时淘汰重要性×新近度最低的一条")
	memoryForget := flag.String("memory-forget", "", "只删除指定编号的长期记忆后退出（人工修正）")
	memorySearch := flag.String("memory-search", "", "只执行一次长期记忆检索并输出各阶段JSON，用于对比；会真实调用模型生成背景与重排")
	memoryMode := flag.String("memory-mode", "rerank", "memory-search 的检索方式：keyword（旧关键词）、vector、bm25、hybrid（RRF）、rerank（完整流程）")
	memoryContextCalls := flag.Int("memory-context-calls", 4, "本次运行最多发起几次记忆背景生成，0 表示不生成（全部按原文检索）")
	memoryTokens := flag.Int("memory-tokens", 800, "开场注入或 search_memory 返回的记忆估算 token 预算")
	var mcpCommands, skillNames stringList
	flag.Var(&mcpCommands, "mcp-server", "Day 6：MCP server 子进程命令（按空格切分，可重复给多个）；配合-question时把它们的tools注册为agent工具")
	mcpList := flag.Bool("mcp-list", false, "Day 6：用SDK连接-mcp-server，打印协议版本、capabilities与工具列表后退出，不请求模型")
	mcpRaw := flag.String("mcp-raw", "", "Day 6：不用SDK，手写JSON-RPC走stdio：legacy（initialize握手）或modern（server/discover+每请求_meta）")
	mcpCall := flag.String("mcp-call", "", "配合-mcp-list或-mcp-raw：再调用一次该工具")
	mcpArgs := flag.String("mcp-args", "{}", "mcp-call的参数JSON")
	mcpServe := flag.Bool("mcp-serve", false, "Day 7：作为MCP server运行（stdio），暴露calculator；须单独使用")
	mcpHTTP := flag.String("mcp-http", "", "配合-mcp-serve：改用 Streamable HTTP（远程 MCP）在该地址提供 /mcp，如 127.0.0.1:8091")
	flag.BoolVar(&skills.Enabled, "skills", false, "Bonus：扫描skills/*/SKILL.md，system中放索引，增加load_skill工具")
	flag.Var(&skillNames, "skill", "配合-skills：只启用这个名字的skill（可重复）；不给则启用全部")
	skillsList := flag.Bool("skills-list", false, "只扫描skills/并以JSON输出每个skill的元数据、正文与错误，不请求模型；须单独使用")
	timeout := flag.Duration("timeout", 0, "Day 8：整次运行的时限，如 2m；0 表示不限（仍可 Ctrl+C 取消）")
	flag.DurationVar(&agent.ToolTimeout, "tool-timeout", agent.ToolTimeout, "Day 8：单次工具尝试的时限；超时记为结果未知")
	taskID := flag.String("task-id", "", "Day 9：检查点ID（字母、数字和 ._-）；不给则生成随机 UUID")
	resume := flag.String("resume", "", "Day 9：从这个ID的检查点续跑；配置取自检查点，须单独使用")
	flag.BoolVar(&browser.Enabled, "browser", false, "Day 11：增加 web_search 与 open_page，用无头 Chrome 查资料（CHROME_PATH 可指定浏览器）")
	flag.BoolVar(&sandbox.Enabled, "bash", false, "Day 12：增加 bash 与 request_network_access，在 bubblewrap 沙箱里执行命令（仅 Linux）")
	flag.BoolVar(&sandbox.ProjectRO, "bash-project-ro", false, "配合 -bash：把项目目录只读挂到 /project（.env、.data、.git 除外）")
	var netAllow stringList
	flag.Var(&netAllow, "net-allow", "配合 -bash：沙箱可经代理访问的域名（含子域名，可重复）；由用户批准后添加")
	subagents := flag.Bool("subagents", false, "增加 spawn_agent：把独立子任务交给全新上下文的子 agent（子进程）")
	flag.IntVar(&agent.SubagentSteps, "subagent-steps", agent.SubagentSteps, "每个子 agent 的模型请求预算，1到20")
	flag.Parse()
	if *resume != "" {
		if flag.NFlag() != 1+boolInt(*appServer) || flag.NArg() != 0 {
			return errors.New("resume须单独使用（可以再加 -app-server）：配置取自检查点里保存的启动参数")
		}
		cp, err := agent.LoadCheckpoint(*resume)
		if err != nil {
			return err
		}
		if cp.Status == "done" {
			// 幂等：已完成的任务再续跑，只返回存档答案，不请求模型、不执行工具。
			fmt.Printf("Resume: id=%s status=done，直接返回存档答案（不重复执行）\n%s\n", cp.ID, cp.Answer)
			return nil
		}
		if !agent.Resumable(cp) {
			return fmt.Errorf("任务 %s 因 %s 停止，续跑结果也一样；要重做请换一个任务ID", cp.ID, cp.Reason)
		}
		// 用首次启动的参数重新解析：工具开关、预算、超时都和中断前一致。通信方式以本次命令行为准。
		wantAppServer := *appServer
		if err := flag.CommandLine.Parse(cp.Args); err != nil {
			return err
		}
		*question, *taskID, *historyStdin, *appServer = cp.Question, cp.ID, false, wantAppServer
		agent.Resume = cp
	}
	// B0：-app-server 时 stdout 归协议所有，在任何打印之前分流。
	var history *agent.HistoryInput
	starts := make(chan protocol.Message, 1)
	interrupted := make(chan struct{})
	var interruptOnce sync.Once
	interrupt := func() { interruptOnce.Do(func() { close(interrupted) }) }
	if *appServer {
		out := os.Stdout
		os.Stdout = os.Stderr
		// 界面先退出时写协议会失败；忽略 SIGPIPE，让进程照常走第一段收尾，而不是被信号直接杀掉。
		signal.Ignore(syscall.SIGPIPE)
		protocol.Client = protocol.NewConn(os.Stdin, out, func(m protocol.Message) {
			switch m.Method {
			case "turn/start":
				select {
				case starts <- m:
				default:
					protocol.Client.Reply(*m.ID, nil, &protocol.Error{Code: -32600, Message: "一个 agent 进程只处理一轮"})
				}
			case "turn/interrupt":
				interrupt()
				protocol.Client.Reply(*m.ID, nil, nil)
			default:
				if m.IsRequest() {
					protocol.Client.Reply(*m.ID, nil, &protocol.Error{Code: -32601, Message: "未知方法 " + m.Method})
				}
			}
		})
		// 界面断开等同于中断：收尾、写好检查点再退出。
		go func() { <-protocol.Client.Closed(); interrupt() }()
		if *question == "" && agent.Resume == nil && !*contextLab && !*ragLab {
			var m protocol.Message
			select {
			case m = <-starts:
			case <-protocol.Client.Closed():
				return errors.New("界面在发起 turn/start 之前断开")
			}
			// 问题、续聊上下文和续接关系都由界面在 turn/start 里给出，不再分别经命令行和 stdin 传入。
			var params struct {
				Question  string              `json:"question"`
				History   *agent.HistoryInput `json:"history"`
				Continues string              `json:"continues"`
			}
			if err := json.Unmarshal(m.Params, &params); err != nil || strings.TrimSpace(params.Question) == "" {
				protocol.Client.Reply(*m.ID, nil, &protocol.Error{Code: -32602, Message: "turn/start 需要非空的 question"})
				return errors.New("turn/start 参数无效")
			}
			*question, history, agent.Continues = params.Question, params.History, params.Continues
			protocol.Client.Reply(*m.ID, nil, nil)
		}
	}
	if *mcpServe {
		// stdout 归协议所有，在任何打印之前分流。
		if flag.NFlag() != 1+min(len(*mcpHTTP), 1) || flag.NArg() != 0 {
			return errors.New("mcp-serve须单独使用，只能再加-mcp-http")
		}
		return mcp.Serve(*mcpHTTP)
	}
	if *mcpHTTP != "" {
		return errors.New("mcp-http需要配合-mcp-serve")
	}
	if *skillsList {
		if flag.NFlag() != 1 || flag.NArg() != 0 {
			return errors.New("skills-list须单独使用")
		}
		encoder := json.NewEncoder(os.Stdout)
		encoder.SetIndent("", "  ")
		encoder.SetEscapeHTML(false)
		return encoder.Encode(skills.List("skills"))
	}
	if len(skillNames) > 0 && !skills.Enabled {
		return errors.New("skill需要配合-skills")
	}
	if *mcpRaw != "" && len(mcpCommands) == 1 && mcp.IsURL(mcpCommands[0]) {
		return errors.New("mcp-raw只演示stdio；远程server请用-mcp-list")
	}
	if *mcpList || *mcpRaw != "" {
		allowed := map[string]bool{"mcp-server": true, "mcp-list": true, "mcp-raw": true, "mcp-call": true, "mcp-args": true}
		var extra error
		flag.Visit(func(f *flag.Flag) {
			if !allowed[f.Name] {
				extra = fmt.Errorf("mcp-list/mcp-raw只与mcp-server、mcp-call、mcp-args组合，不接受-%s", f.Name)
			}
		})
		if extra != nil || flag.NArg() != 0 {
			return errors.Join(extra, errors.New("用法：-mcp-server '命令' -mcp-list|-mcp-raw legacy|modern [-mcp-call 工具 -mcp-args JSON]"))
		}
		if len(mcpCommands) != 1 || (*mcpList && *mcpRaw != "") || (*mcpRaw != "" && *mcpRaw != "legacy" && *mcpRaw != "modern") {
			return errors.New("需要且只能给一个-mcp-server；-mcp-list与-mcp-raw二选一，mcp-raw取legacy或modern")
		}
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
		defer stop()
		if *mcpList {
			return mcp.Inspect(ctx, mcpCommands[0], *mcpCall, *mcpArgs)
		}
		return mcp.Raw(ctx, mcpCommands[0], *mcpRaw, *mcpCall, *mcpArgs)
	}
	if *mcpCall != "" {
		return errors.New("mcp-call需要配合-mcp-list或-mcp-raw")
	}
	if flag.NArg() != 0 || *parallel < 1 || *parallel > 16 {
		return errors.New("使用 -question 提供问题，-parallel 范围为 1 到 16")
	}
	if *maxSteps < 1 || *retries < 0 || *retries > 5 {
		return errors.New("max-steps 至少为1，retries 范围为0到5")
	}
	if (*embeddingMode != "ark" && *embeddingMode != "local") || math.IsNaN(*threshold) || *threshold < -1 || *threshold > 1 {
		return errors.New("embedding取ark或local，min-score范围为-1到1")
	}
	if *ragLab && (*contextLab || *question != "" || *historyStdin || tools.LabEnabled || *searchQuery != "") {
		return errors.New("rag-lab请单独运行，不与其他实验、问题或续聊参数组合")
	}
	if *searchQuery != "" {
		if *question != "" || *contextLab || *historyStdin || tools.LabEnabled || retrieval.Enabled {
			return errors.New("search-docs请单独使用，可配合-k")
		}
		if *searchK < 1 || *searchK > 6 {
			return errors.New("k范围为1到6")
		}
	}
	if *memoryTTL < 0 || *memoryLimit < 1 || *memoryLimit > 10000 || *memoryContextCalls < 0 || *memoryTokens < 100 {
		return errors.New("memory-ttl与memory-context-calls不能为负，memory-limit范围为1到10000，memory-tokens至少100")
	}
	if !slices.Contains([]string{"keyword", "vector", "bm25", "hybrid", "rerank"}, *memoryMode) {
		return errors.New("memory-mode取keyword、vector、bm25、hybrid或rerank")
	}
	if (len(mcpCommands) > 0 || skills.Enabled) && (*contextLab || *ragLab || *searchQuery != "" || *memoryForget != "" || *memorySearch != "") {
		return errors.New("mcp-server与skills用于普通问答与续聊，不与实验、search-docs、memory-search或memory-forget组合")
	}
	if (*taskID != "" || *subagents || browser.Enabled || *timeout != 0) && (*contextLab || *ragLab || *searchQuery != "" || *memoryForget != "" || *memorySearch != "") {
		return errors.New("task-id、subagents、browser与timeout用于普通问答与续聊，不与实验、search-docs、memory-search或memory-forget组合")
	}
	if *timeout < 0 || agent.ToolTimeout <= 0 || agent.SubagentSteps < 1 || agent.SubagentSteps > 20 {
		return errors.New("timeout不能为负，tool-timeout须大于0，subagent-steps范围为1到20")
	}
	if *memoryEnabled && (*contextLab || *ragLab || *searchQuery != "" || *memoryForget != "" || *memorySearch != "") {
		return errors.New("memory用于普通问答与续聊，不与实验、search-docs、memory-search或memory-forget组合")
	}
	if *memoryEnabled || *memoryForget != "" || *memorySearch != "" {
		memory.Active = memory.New(*memoryLimit, *memoryTTL, *embeddingMode, *memoryTokens, *memoryContextCalls)
	}
	// 向量模型按需加载、进程内共享；退出时统一释放。
	defer retrieval.CloseEmbedders()
	if *memoryForget != "" || *memorySearch != "" {
		if *question != "" || *historyStdin || *contextLab || *ragLab || *searchQuery != "" || retrieval.Enabled || (*memoryForget != "" && *memorySearch != "") {
			return errors.New("memory-forget和memory-search请单独使用")
		}
		if *memoryForget != "" {
			// 删除不做容量淘汰：一次性命令的默认上限不能顺带删掉其他记忆。
			_, err := memory.Active.Forget(*memoryForget, "人工删除")
			return err
		}
		if strings.TrimSpace(*memorySearch) == "" {
			return errors.New("memory-search需要非空问题")
		}
		// 没有密钥时 vector/bm25/hybrid 仍可在本地对比；背景生成与重排会按降级处理并写明原因。
		if config, err := llm.LoadConfig(*provider); err == nil {
			config.Effort = *effort
			memory.Active.Client = &llm.Client{Config: config, Limit: *maxSteps}
		} else {
			fmt.Fprintln(os.Stderr, "Memory search: model=unavailable reason="+err.Error())
		}
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
		defer stop()
		// 与 -search-docs 一样：过程日志改到 stderr，stdout 只有 JSON 结果。
		stdout := os.Stdout
		os.Stdout = os.Stderr
		result, err := memory.Active.Retrieve(ctx, "cli", strings.TrimSpace(*memorySearch), *memoryMode)
		os.Stdout = stdout
		if err != nil {
			return err
		}
		encoder := json.NewEncoder(os.Stdout)
		encoder.SetIndent("", "  ")
		encoder.SetEscapeHTML(false)
		return encoder.Encode(result.Report())
	}
	options := agent.ContextOptions{Window: *window, Output: *output, ReasoningReserve: *reserve, ToolOutput: *toolOutput, KeepGroups: *keepGroups}
	if *effort != "minimal" && *effort != "low" && *effort != "medium" && *effort != "high" {
		return errors.New("reasoning-effort取值不支持")
	}
	if *output < 0 || *reserve < 0 || *toolOutput < 64 || *keepGroups < 0 || options.Capacity() < 256 {
		return errors.New("输出上限、推理预留和keep-groups须非负、tool-output-tokens至少64；扣除预留后的输入容量至少256")
	}
	if *contextLab && (*maxSteps < 36 || *question != "" || tools.LabEnabled || retrieval.Enabled) {
		return errors.New("context-lab单独使用，至少36次请求，建议 -max-steps 60 为摘要保留额度")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	// 第一次 Ctrl+C 取消 ctx：在跑的工具被打断、结果回填、检查点写好再退出。之后恢复默认处理，再按一次立即结束。
	context.AfterFunc(ctx, stop)
	// turn/interrupt 走同一条路：和第一次 Ctrl+C 一样取消 ctx，开始第一段收尾。
	go func() {
		select {
		case <-interrupted:
			fmt.Println("Interrupt: turn/interrupt")
			stop()
		case <-ctx.Done():
		}
	}()
	if *timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, *timeout)
		defer cancel()
	}
	// D13：一次运行一个根 span invoke_agent。父进程（观测台、队列、父 agent）通过 TRACEPARENT 把它挂到自己的 span 下；
	// 启动阶段的检索建库、MCP 连接也在它下面。结局与任务ID由 agent.Run 补上。
	ctx, root := telemetry.Begin(telemetry.FromEnv(ctx), "invoke_agent learning-agent", trace.SpanKindInternal, agent.RootAttributes()...)
	defer root.End()
	// 普通ReAct任务不加载向量模型；只有检索模式初始化，并在当前进程退出时释放。
	if retrieval.Enabled || *ragLab || *searchQuery != "" {
		if *searchQuery != "" {
			retrieval.IndexLog = os.Stderr
		}
		var err error
		retrieval.Docs, err = retrieval.New(ctx, *embeddingMode, *threshold)
		if err != nil {
			return err
		}
	}
	if *searchQuery != "" {
		result, err := retrieval.Search(ctx, *searchQuery, *searchK)
		if err != nil {
			return err
		}
		encoder := json.NewEncoder(os.Stdout)
		encoder.SetIndent("", "  ")
		return encoder.Encode(result)
	}
	if *historyStdin {
		if *contextLab || strings.TrimSpace(*question) == "" {
			return errors.New("history-stdin需要-question，不能与context-lab一起使用")
		}
		history = &agent.HistoryInput{}
		if err := json.NewDecoder(io.LimitReader(os.Stdin, 8<<20)).Decode(history); err != nil {
			return fmt.Errorf("无法读取续聊上下文：%w", err)
		}
	}
	// 本次运行发给模型的工具：先放内置工具，再按开关追加。复制一份，不改动 tools.Definitions。
	llm.Tools = append([]map[string]any(nil), tools.Definitions...)
	if tools.LabEnabled {
		llm.Tools = append(llm.Tools, tools.LabDefinitions...)
	}
	if retrieval.Enabled {
		llm.Tools = append(llm.Tools, retrieval.SearchDefinition)
	}
	if memory.Active != nil {
		llm.Tools = append(llm.Tools, memory.ToolDefinitions...)
	}
	if browser.Enabled {
		llm.Tools = append(llm.Tools, browser.Definitions...)
		defer browser.Close()
	}
	if (sandbox.ProjectRO || len(netAllow) > 0) && !sandbox.Enabled {
		return errors.New("bash-project-ro与net-allow需要配合-bash")
	}
	for _, domain := range netAllow {
		if !sandbox.ValidDomain(strings.ToLower(domain)) {
			return fmt.Errorf("net-allow 需要是小写域名（不含协议、路径和 IP）：%s", domain)
		}
		sandbox.Allow = append(sandbox.Allow, strings.ToLower(domain))
	}
	if sandbox.Enabled {
		llm.Tools = append(llm.Tools, sandbox.Definitions...)
		defer sandbox.Close()
	}
	if !*contextLab && !*ragLab && strings.TrimSpace(*question) == "" {
		fmt.Print("请输入任务： ")
		*question, _ = bufio.NewReader(os.Stdin).ReadString('\n')
	}
	*question = strings.TrimSpace(*question)
	if !*contextLab && !*ragLab && *question == "" {
		return errors.New("问题不能为空")
	}
	config, err := llm.LoadConfig(*provider)
	if err != nil {
		return err
	}
	config.Effort = *effort
	fmt.Printf("Model: provider=%s model=%s upstream=%s\n", config.Provider, config.Model, config.Upstream)
	// B0：一轮开始。界面从这里拿任务ID（检查点、浏览器画面目录都按它存放）、输入容量和 trace_id，不再从日志里解析。
	turnStarted := func(turn string) {
		thread := telemetry.ConversationID
		if thread == "" {
			thread = turn
		}
		protocol.Notify("turn/started", map[string]any{"thread_id": thread, "turn_id": turn, "continues": agent.Continues, "resume": agent.Resume != nil,
			"trace_id": root.SpanContext().TraceID().String(), "input_capacity": options.Capacity(), "provider": config.Provider, "model": config.Model, "pid": os.Getpid()})
	}
	if *contextLab || *ragLab {
		turnStarted("")
	}
	if *contextLab {
		return labs.RunContext(ctx, config, options, *maxSteps)
	}
	if *ragLab {
		return labs.RunRAG(ctx, config, options, *maxSteps)
	}
	if skills.Enabled {
		skills.Index = skills.Scan("skills", skillNames)
		if len(skills.Index) > 0 {
			llm.Tools = append(llm.Tools, skills.LoadDefinition)
		}
	}
	// MCP 工具在启动时一次性拉取并注册；server 子进程活到本次运行结束。
	// 多个 server 依次连接，后连的 server 与已注册工具重名时跳过（definitions 打印原因）。
	// 某个 server 连不上只跳过它：和坏 skill 一样，外部工具是可选增强，不拖垮整次运行。
	for _, command := range mcpCommands {
		conn, listed, err := mcp.Connect(ctx, command)
		if err != nil {
			protocol.Event("capability/event", "mcp_error", fmt.Sprintf("MCP error: command=%s error=%v", command, err))
			continue
		}
		defer conn.Close()
		llm.Tools = append(llm.Tools, conn.Definitions(listed)...)
		mcp.Conns = append(mcp.Conns, conn)
	}
	if *subagents {
		// 子 agent 继承工具开关和运行参数；不继承长期记忆（子 agent 的“用户”是父 agent，不是真人）、
		// 不继承 -subagents（只允许一层）、不继承续聊与整次时限（由父进程的 ctx 管）。
		share := map[string]bool{"parallel": true, "retries": true, "lab-tools": true, "reasoning-effort": true, "context-window": true, "max-output-tokens": true,
			"reasoning-reserve": true, "tool-output-tokens": true, "keep-groups": true, "rag": true, "embedding": true, "min-score": true, "skills": true, "tool-timeout": true, "browser": true, "provider": true, "bash": true, "bash-project-ro": true}
		agent.SubagentArgs = []string{}
		flag.Visit(func(f *flag.Flag) {
			if share[f.Name] {
				agent.SubagentArgs = append(agent.SubagentArgs, "-"+f.Name+"="+f.Value.String())
			}
		})
		for _, command := range mcpCommands {
			agent.SubagentArgs = append(agent.SubagentArgs, "-mcp-server", command)
		}
		for _, name := range skillNames {
			agent.SubagentArgs = append(agent.SubagentArgs, "-skill", name)
		}
		for _, domain := range sandbox.Allow {
			agent.SubagentArgs = append(agent.SubagentArgs, "-net-allow", domain)
		}
		llm.Tools = append(llm.Tools, agent.SpawnDefinition)
	}
	// D9：每次普通运行都写检查点，并在整个运行期间持有它的锁，同一任务不会被两个进程同时执行。
	// 通信方式不存进检查点：之后在命令行续跑时，不该变成协议模式。
	agent.CheckpointID, agent.CheckpointArgs = *taskID, slices.DeleteFunc(slices.Clone(os.Args[1:]), func(a string) bool { return a == "-app-server" || a == "--app-server" })
	if agent.CheckpointID == "" {
		agent.CheckpointID = agent.NewCheckpointID()
	}
	if agent.Resume != nil {
		agent.CheckpointArgs = agent.Resume.Args
	}
	if !agent.ValidID(agent.CheckpointID) {
		return errors.New("task-id只能包含字母、数字和 ._-，最长128")
	}
	if agent.Continues != "" && agent.Resume == nil {
		if _, err := agent.LoadCheckpoint(agent.Continues); err != nil || history == nil {
			return fmt.Errorf("continues 需要配合续聊上下文，且上一轮的检查点 %s 要存在", agent.Continues)
		}
	}
	unlock, err := agent.LockCheckpoint(agent.CheckpointID)
	if err != nil {
		return err
	}
	defer unlock()
	if _, err := os.Stat(agent.CheckpointPath(agent.CheckpointID)); agent.Resume == nil && err == nil {
		return fmt.Errorf("任务ID %s 已有检查点：续跑用 -resume %s，重做请换一个ID", agent.CheckpointID, agent.CheckpointID)
	}
	llm.TaskID = agent.CheckpointID
	if browser.Enabled {
		browser.FrameDir = filepath.Join(".data", "browser", agent.CheckpointID)
	}
	// 沙箱工作目录按任务ID分开：同一任务的多次调用、续跑共用；子 agent 有自己的目录。
	sandbox.Dir = filepath.Join(".data", "sandbox", agent.CheckpointID)
	fmt.Printf("Checkpoint: id=%s file=%s（中断后用 -resume %s 续跑）\n", agent.CheckpointID, agent.CheckpointPath(agent.CheckpointID), agent.CheckpointID)
	turnStarted(agent.CheckpointID)
	return agent.Run(ctx, config, *question, *parallel, *maxSteps, *retries, options, history)
}

// 可重复的命令行参数，如 -mcp-server a -mcp-server b。
type stringList []string

func (l *stringList) String() string { return strings.Join(*l, ",") }

func (l *stringList) Set(value string) error {
	if strings.TrimSpace(value) == "" {
		return errors.New("不能为空")
	}
	*l = append(*l, strings.TrimSpace(value))
	return nil
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
