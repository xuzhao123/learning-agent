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
	"slices"
	"strings"

	"learning-agent/internal/agent"
	"learning-agent/internal/labs"
	"learning-agent/internal/llm"
	"learning-agent/internal/mcp"
	"learning-agent/internal/memory"
	"learning-agent/internal/observer"
	"learning-agent/internal/retrieval"
	"learning-agent/internal/skills"
	"learning-agent/internal/tools"
)

// 唯一入口：go run . observe 启动观测台（含内置远程 MCP）；其余用法都是 agent 本身。
func main() {
	run := run
	if len(os.Args) > 1 && os.Args[1] == "observe" {
		run = func() error { return observer.Run(os.Args[2:], mcp.Handler()) }
	}
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(1)
	}
}

func run() error {
	question := flag.String("question", "", "要交给 agent 的问题")
	historyStdin := flag.Bool("history-stdin", false, "从stdin读取上次模型上下文JSON，配合-question续聊")
	parallel := flag.Int("parallel", 4, "同时执行的工具数，1 到 16")
	maxSteps := flag.Int("max-steps", 10, "模型请求总次数，包含最终回答和上下文摘要")
	retries := flag.Int("retries", 2, "工具失败后额外重试次数，0 到 5")
	flag.BoolVar(&tools.LabEnabled, "lab-tools", false, "启用 Day 2 故障实验工具")
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
	flag.Parse()
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
		if config, err := llm.LoadConfig(); err == nil {
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
	var history *agent.HistoryInput
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
	if !*contextLab && !*ragLab && strings.TrimSpace(*question) == "" {
		fmt.Print("请输入任务： ")
		*question, _ = bufio.NewReader(os.Stdin).ReadString('\n')
	}
	*question = strings.TrimSpace(*question)
	if !*contextLab && !*ragLab && *question == "" {
		return errors.New("问题不能为空")
	}
	config, err := llm.LoadConfig()
	if err != nil {
		return err
	}
	config.Effort = *effort
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
			fmt.Printf("MCP error: command=%s error=%v\n", command, err)
			continue
		}
		defer conn.Close()
		llm.Tools = append(llm.Tools, conn.Definitions(listed)...)
		mcp.Conns = append(mcp.Conns, conn)
	}
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
