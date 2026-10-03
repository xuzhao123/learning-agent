package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
)

type modelConfig struct{ APIURL, Model, APIKey string }

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(1)
	}
}

func run() error {
	question := flag.String("question", "", "要交给 agent 的问题")
	parallel := flag.Int("parallel", 4, "同时执行的工具数，1 到 16")
	flag.Parse()
	if flag.NArg() != 0 || *parallel < 1 || *parallel > 16 {
		return errors.New("使用 -question 提供问题，-parallel 范围为 1 到 16")
	}
	if strings.TrimSpace(*question) == "" {
		fmt.Print("请输入任务： ")
		*question, _ = bufio.NewReader(os.Stdin).ReadString('\n')
	}
	*question = strings.TrimSpace(*question)
	if *question == "" {
		return errors.New("问题不能为空")
	}
	config, err := loadConfig()
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	return runAgent(ctx, config, *question, *parallel)
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
