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
)

type modelConfig struct{ APIURL, Model, APIKey, Effort string }

func main() {
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
	flag.BoolVar(&labToolsEnabled, "lab-tools", false, "启用 Day 2 故障实验工具")
	effort := flag.String("reasoning-effort", "high", "所有请求统一的推理强度：minimal、low、medium或high")
	window := flag.Int("context-window", 12288, "上下文总窗口（学习实验值），输入容量扣除输出预算预留与推理余量")
	output := flag.Int("max-output-tokens", 0, "可选输出上限（含推理）；0不向API传限制，输入预算仍预留4096")
	reserve := flag.Int("reasoning-reserve", 1024, "输入容量额外预留的安全余量")
	toolOutput := flag.Int("tool-output-tokens", 2000, "工具结果写入视图的估算token上限，至少64")
	keepGroups := flag.Int("keep-groups", 2, "压缩后最多保留的最近完整消息组数")
	contextLab := flag.Bool("context-lab", false, "Day 3：33轮真实对话和真实大工具输出实验，建议 -max-steps 60")
	flag.BoolVar(&ragEnabled, "rag", false, "Day 4：启用本地知识库检索工具与来源引用")
	ragLab := flag.Bool("rag-lab", false, "Day 4：10题检索检查与无检索/有检索真实模型对比")
	embeddingMode := flag.String("embedding", "ark", "向量模型：ark（线上方舟）或local（本地纯Go推理）")
	threshold := flag.Float64("min-score", minScore, "检索余弦相似度阈值，-1到1")
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
	flag.Parse()
	if flag.NArg() != 0 || *parallel < 1 || *parallel > 16 {
		return errors.New("使用 -question 提供问题，-parallel 范围为 1 到 16")
	}
	if *maxSteps < 1 || *retries < 0 || *retries > 5 {
		return errors.New("max-steps 至少为1，retries 范围为0到5")
	}
	if (*embeddingMode != "ark" && *embeddingMode != "local") || math.IsNaN(*threshold) || *threshold < -1 || *threshold > 1 {
		return errors.New("embedding取ark或local，min-score范围为-1到1")
	}
	if *ragLab && (*contextLab || *question != "" || *historyStdin || labToolsEnabled || *searchQuery != "") {
		return errors.New("rag-lab请单独运行，不与其他实验、问题或续聊参数组合")
	}
	if *searchQuery != "" {
		if *question != "" || *contextLab || *historyStdin || labToolsEnabled || ragEnabled {
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
	if *memoryEnabled && (*contextLab || *ragLab || *searchQuery != "" || *memoryForget != "" || *memorySearch != "") {
		return errors.New("memory用于普通问答与续聊，不与实验、search-docs、memory-search或memory-forget组合")
	}
	if *memoryEnabled || *memoryForget != "" || *memorySearch != "" {
		memories = &memoryStore{limit: *memoryLimit, ttl: *memoryTTL, provider: *embeddingMode, tokens: *memoryTokens}
		memories.contextLeft.Store(int64(*memoryContextCalls))
	}
	// 向量模型按需加载、进程内共享；退出时统一释放。
	defer closeEmbedders()
	if *memoryForget != "" || *memorySearch != "" {
		if *question != "" || *historyStdin || *contextLab || *ragLab || *searchQuery != "" || ragEnabled || (*memoryForget != "" && *memorySearch != "") {
			return errors.New("memory-forget和memory-search请单独使用")
		}
		if *memoryForget != "" {
			// 删除不做容量淘汰：一次性命令的默认上限不能顺带删掉其他记忆。
			_, err := memories.forget(*memoryForget, "人工删除")
			return err
		}
		if strings.TrimSpace(*memorySearch) == "" {
			return errors.New("memory-search需要非空问题")
		}
		// 没有密钥时 vector/bm25/hybrid 仍可在本地对比；背景生成与重排会按降级处理并写明原因。
		if config, err := loadConfig(); err == nil {
			config.Effort = *effort
			memories.client = &modelClient{Config: config, Limit: *maxSteps}
		} else {
			fmt.Fprintln(os.Stderr, "Memory search: model=unavailable reason="+err.Error())
		}
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
		defer stop()
		// 与 -search-docs 一样：过程日志改到 stderr，stdout 只有 JSON 结果。
		stdout := os.Stdout
		os.Stdout = os.Stderr
		result, err := memories.retrieve(ctx, "cli", strings.TrimSpace(*memorySearch), *memoryMode)
		os.Stdout = stdout
		if err != nil {
			return err
		}
		encoder := json.NewEncoder(os.Stdout)
		encoder.SetIndent("", "  ")
		encoder.SetEscapeHTML(false)
		return encoder.Encode(result.report())
	}
	options := contextOptions{Window: *window, Output: *output, ReasoningReserve: *reserve, ToolOutput: *toolOutput, KeepGroups: *keepGroups}
	if *effort != "minimal" && *effort != "low" && *effort != "medium" && *effort != "high" {
		return errors.New("reasoning-effort取值不支持")
	}
	if *output < 0 || *reserve < 0 || *toolOutput < 64 || *keepGroups < 0 || options.capacity() < 256 {
		return errors.New("输出上限、推理预留和keep-groups须非负、tool-output-tokens至少64；扣除预留后的输入容量至少256")
	}
	if *contextLab && (*maxSteps < 36 || *question != "" || labToolsEnabled || ragEnabled) {
		return errors.New("context-lab单独使用，至少36次请求，建议 -max-steps 60 为摘要保留额度")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	// 普通ReAct任务不加载向量模型；只有检索模式初始化，并在当前进程退出时释放。
	if ragEnabled || *ragLab || *searchQuery != "" {
		if *searchQuery != "" {
			indexLog = os.Stderr
		}
		var err error
		docsRetriever, err = newRetriever(ctx, *embeddingMode, *threshold)
		if err != nil {
			return err
		}
	}
	if *searchQuery != "" {
		result, err := searchDocs(ctx, *searchQuery, *searchK)
		if err != nil {
			return err
		}
		encoder := json.NewEncoder(os.Stdout)
		encoder.SetIndent("", "  ")
		return encoder.Encode(result)
	}
	var history *historyInput
	if *historyStdin {
		if *contextLab || strings.TrimSpace(*question) == "" {
			return errors.New("history-stdin需要-question，不能与context-lab一起使用")
		}
		history = &historyInput{}
		if err := json.NewDecoder(io.LimitReader(os.Stdin, 8<<20)).Decode(history); err != nil {
			return fmt.Errorf("无法读取续聊上下文：%w", err)
		}
	}
	if labToolsEnabled {
		toolDefinitions = append(toolDefinitions, labToolDefinitions...)
	}
	if ragEnabled {
		toolDefinitions = append(toolDefinitions, searchDocsDefinition)
	}
	if memories != nil {
		toolDefinitions = append(toolDefinitions, memoryToolDefinitions...)
	}
	if !*contextLab && !*ragLab && strings.TrimSpace(*question) == "" {
		fmt.Print("请输入任务： ")
		*question, _ = bufio.NewReader(os.Stdin).ReadString('\n')
	}
	*question = strings.TrimSpace(*question)
	if !*contextLab && !*ragLab && *question == "" {
		return errors.New("问题不能为空")
	}
	config, err := loadConfig()
	if err != nil {
		return err
	}
	config.Effort = *effort
	if *contextLab {
		return runContextLab(ctx, config, options, *maxSteps)
	}
	if *ragLab {
		return runRAGLab(ctx, config, options, *maxSteps)
	}
	return runAgent(ctx, config, *question, *parallel, *maxSteps, *retries, options, history)
}

// 环境变量优先，其次是本地 .env，最后是地址与模型的默认值。
func loadConfig() (modelConfig, error) {
	values := map[string]string{}
	file, err := os.Open(".env")
	if err != nil && !os.IsNotExist(err) {
		return modelConfig{}, err
	}
	if file != nil {
		defer file.Close()
		scanner := bufio.NewScanner(file)
		for line := 1; scanner.Scan(); line++ {
			text := strings.TrimSpace(scanner.Text())
			if text == "" || strings.HasPrefix(text, "#") {
				continue
			}
			name, value, ok := strings.Cut(text, "=")
			if !ok {
				return modelConfig{}, fmt.Errorf(".env 第 %d 行需要 KEY=value", line)
			}
			values[strings.TrimSpace(name)] = strings.Trim(strings.TrimSpace(value), "\"'")
		}
		if scanner.Err() != nil {
			return modelConfig{}, errors.New("无法读取 .env")
		}
	}
	get := func(fallback string, names ...string) string {
		for _, name := range names {
			if value := strings.TrimSpace(os.Getenv(name)); value != "" {
				return value
			}
		}
		for _, name := range names {
			if value := values[name]; value != "" {
				return value
			}
		}
		return fallback
	}
	config := modelConfig{
		APIURL: get("https://ark.cn-beijing.volces.com/api/v3/chat/completions", "LLM_API_URL"),
		Model:  get("doubao-seed-2-1-pro-260628", "LLM_MODEL"),
		APIKey: get("", "ARK_API_KEY", "LLM_API_KEY"),
	}
	if config.APIKey == "" {
		return config, errors.New("请在 .env 或环境变量配置 ARK_API_KEY")
	}
	return config, nil
}
