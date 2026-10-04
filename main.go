package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
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
	flag.Parse()
	if flag.NArg() != 0 || *parallel < 1 || *parallel > 16 {
		return errors.New("使用 -question 提供问题，-parallel 范围为 1 到 16")
	}
	if *maxSteps < 1 || *retries < 0 || *retries > 5 {
		return errors.New("max-steps 至少为1，retries 范围为0到5")
	}
	options := contextOptions{Window: *window, Output: *output, ReasoningReserve: *reserve, ToolOutput: *toolOutput, KeepGroups: *keepGroups}
	if *effort != "minimal" && *effort != "low" && *effort != "medium" && *effort != "high" {
		return errors.New("reasoning-effort取值不支持")
	}
	if *output < 0 || *reserve < 0 || *toolOutput < 64 || *keepGroups < 0 || options.capacity() < 256 {
		return errors.New("输出上限、推理预留和keep-groups须非负、tool-output-tokens至少64；扣除预留后的输入容量至少256")
	}
	if *contextLab && (*maxSteps < 36 || *question != "" || labToolsEnabled) {
		return errors.New("context-lab单独使用，至少36次请求，建议 -max-steps 60 为摘要保留额度")
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
	if !*contextLab && strings.TrimSpace(*question) == "" {
		fmt.Print("请输入任务： ")
		*question, _ = bufio.NewReader(os.Stdin).ReadString('\n')
	}
	*question = strings.TrimSpace(*question)
	if !*contextLab && *question == "" {
		return errors.New("问题不能为空")
	}
	config, err := loadConfig()
	if err != nil {
		return err
	}
	config.Effort = *effort
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if *contextLab {
		return runContextLab(ctx, config, options, *maxSteps)
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
