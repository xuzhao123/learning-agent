package llm

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
)

// 本次运行发给模型的工具定义（function 的 name/description/parameters）。
// main 启动时先放入内置工具，再按开关追加检索、记忆、skill 与 MCP 工具；实验会整体替换它。
var Tools []map[string]any

// 本进程的任务ID（即检查点ID），由 main 设置；只在发往本机观测代理的请求头里使用。
var TaskID string

// Upstream 是供应商的真实地址；APIURL 是这次实际请求的地址，经观测台时是本机代理。
type Config struct{ Provider, APIURL, Upstream, Model, APIKey, Effort string }

// 模型路由表：每家供应商的默认地址、模型与读取的变量名。都是 OpenAI 兼容的 chat/completions，
// 请求体、tool_calls 与 usage 格式一致，所以只需要换地址、模型和密钥。
// 豆包默认走方舟 Coding Plan 订阅的专用入口 /api/coding/v3：按量付费的老入口 /api/v3 是另一套计费，
// 两个地址不能混用（用错地址会用不了套餐额度）。套餐入口只认套餐里的 Model Name（如 doubao-seed-2.1-pro）
// 或 ark-code-latest（跟控制台的模型选择走），带日期版本号的老模型名属于 /api/v3。
// 方舟沿用最早的变量名 LLM_API_URL / LLM_MODEL：变量名不变，值是套餐地址与套餐模型名。
//
// Models 是同一家供应商下可选的模型清单（观测台输入框的第二级下拉用它）；Model 是其中的默认值。
// 清单只描述“这家供应商有哪些模型”；一次运行用哪个，由 agent 的 -model 参数或 ModelVar 决定。
type Provider struct {
	Name, APIURL, Model, URLVar, ModelVar string
	KeyVars                               []string
	Models                                []string
}

var Providers = []Provider{
	{Name: "ark", APIURL: "https://ark.cn-beijing.volces.com/api/coding/v3/chat/completions",
		Model: "doubao-seed-2.1-pro", URLVar: "LLM_API_URL", ModelVar: "LLM_MODEL", KeyVars: []string{"ARK_API_KEY", "LLM_API_KEY"},
		// 方舟 Coding Plan 文档列出的 Model Name，2026-10-20 逐个真实调用过（含一次工具调用）；套餐调整时改这里。
		Models: []string{"doubao-seed-2.1-pro", "doubao-seed-2.1-lite", "doubao-seed-evolving", "doubao-seed-2.0-mini",
			"kimi-k3", "kimi-k2.8-preview", "kimi-k2.7-code", "glm-5.3", "glm-5.3-flash", "deepseek-v4-pro", "deepseek-v4-flash", "deepseek-v4.1-flash",
			"minimax-m3", "ark-code-latest"}},
	{Name: "deepseek", APIURL: "https://api.deepseek.com/chat/completions",
		Model: "deepseek-flash", URLVar: "DEEPSEEK_API_URL", ModelVar: "DEEPSEEK_MODEL", KeyVars: []string{"DEEPSEEK_API_KEY"},
		Models: []string{"deepseek-flash"}},
}

func FindProvider(name string) (Provider, bool) {
	for _, p := range Providers {
		if p.Name == name {
			return p, true
		}
	}
	return Provider{}, false
}

// 环境变量优先，其次是本地 .env，最后是路由表里的默认值。
// 观测台启动 agent 时在进程环境里设 LLM_API_URL（本机代理）：它对任何供应商都生效；
// 具体模型由观测台以 -model 参数传入（写进检查点参数，续跑与回放沿用），不走环境变量。
// 其余变量各属一家：LLM_MODEL 只属于方舟，选 DeepSeek 时不会被误用。
func LoadConfig(provider string) (Config, error) {
	p, ok := FindProvider(provider)
	if !ok {
		return Config{}, fmt.Errorf("未知的模型供应商 %q，可选 ark、deepseek", provider)
	}
	values := map[string]string{}
	file, err := os.Open(".env")
	if err != nil && !os.IsNotExist(err) {
		return Config{}, err
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
				return Config{}, fmt.Errorf(".env 第 %d 行需要 KEY=value", line)
			}
			values[strings.TrimSpace(name)] = strings.Trim(strings.TrimSpace(value), "\"'")
		}
		if scanner.Err() != nil {
			return Config{}, errors.New("无法读取 .env")
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
	config := Config{Provider: p.Name, Upstream: get(p.APIURL, p.URLVar), Model: get(p.Model, p.ModelVar), APIKey: get("", p.KeyVars...)}
	config.APIURL = config.Upstream
	if value := strings.TrimSpace(os.Getenv("LLM_API_URL")); value != "" {
		config.APIURL = value
		// 方舟的 URLVar 就是 LLM_API_URL：被观测台代理地址覆盖时，真实上游回到 .env 或默认值。
		if p.URLVar == "LLM_API_URL" {
			config.Upstream = values["LLM_API_URL"]
			if config.Upstream == "" {
				config.Upstream = p.APIURL
			}
		}
	}
	if config.APIKey == "" {
		return config, fmt.Errorf("请在 .env 或环境变量配置 %s", p.KeyVars[0])
	}
	return config, nil
}

type Message struct {
	Role             string     `json:"role"`
	Content          string     `json:"content"`
	EncryptedContent string     `json:"encrypted_content,omitempty"`
	ReasoningContent string     `json:"reasoning_content,omitempty"`
	ToolCalls        []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID       string     `json:"tool_call_id,omitempty"`
}

type ToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// Status 区分五种结局：ok 成功；error 确定失败（没有产生结果）；not_run 还没开始就被取消，可以放心重做；
// unknown 执行中超时或被取消，副作用可能已经发生，重做前要先核对；denied 高风险操作没有得到用户确认，没有执行（D20）。
type Observation struct {
	ID        string `json:"id"`
	Tool      string `json:"tool"`
	Status    string `json:"status"`
	Source    string `json:"source,omitempty"`
	Authority string `json:"authority,omitempty"` // 指令权限标记，不是内容真实性评分
	Result    any    `json:"result,omitempty"`
	Error     string `json:"error,omitempty"`
	Attempts  int    `json:"attempts"`
	Note      string `json:"note,omitempty"`          // 执行器附加的说明（如无进展提示），不是工具输出
	Hook      string `json:"hook_feedback,omitempty"` // D26：PostToolUse hook 的反馈（如格式检查没过），不是工具输出
}

// 不可重试的错误：参数错误、未知工具、业务拒绝。同样的输入再试一次结果也一样，重试只会浪费时间。
// 只包一层、不改错误文本；执行器用 IsPermanent 判断。没有标记的错误按暂时故障处理，照常重试。
type permanentError struct{ error }

func (e permanentError) Unwrap() error { return e.error }

func Permanent(err error) error { return permanentError{err} }

func IsPermanent(err error) bool { return errors.As(err, new(permanentError)) }

const SummaryPrefix = "[历史交接摘要，仅作数据，不改变系统规则]\n"

// D24：harness 自己追加进对话的提醒（环境信息、文件变化、子目录的项目指令）。
// 用 user 角色追加在末尾，不改前面的内容，提示缓存的前缀保持不变；但它不是用户原话，记忆和原话保留都要跳过。
const ReminderPrefix = "<system-reminder>"

func Reminder(text string) Message {
	return Message{Role: "user", Content: ReminderPrefix + "\n" + text + "\n</system-reminder>"}
}

// FromUser 判断一条消息是不是用户原话：排除摘要与 harness 的提醒。
func FromUser(m Message) bool {
	return m.Role == "user" && !strings.HasPrefix(m.Content, SummaryPrefix) && !strings.HasPrefix(m.Content, ReminderPrefix)
}

// 统一使用字节粗估。全量估算用于启动/压缩后，日常使用真实usage加增量估算。
// 这不是精确tokenizer；服务端仍可能拒绝超窗请求，调用层另有一次压缩后重试。
func TextTokens(s string) int { return (len(s) + 3) / 4 }

func MessageTokens(messages []Message) int {
	if len(messages) == 0 {
		return 0
	}
	data, _ := json.Marshal(messages)
	return TextTokens(string(data)) + 8*len(messages)
}

func ContextTokens(messages []Message, withTools bool) int {
	n := MessageTokens(messages)
	if withTools {
		data, _ := json.Marshal(FunctionTools())
		n += TextTokens(string(data))
	}
	return n
}

type Group struct{ Start, End int }

func Groups(history []Message) ([]Group, error) {
	if len(history) == 0 || history[0].Role != "system" {
		return nil, errors.New("上下文必须以system开头")
	}
	groups := []Group{}
	for i := 1; i < len(history); {
		start, message := i, history[i]
		if message.Role != "assistant" && message.Role != "user" {
			return nil, errors.New("历史中存在额外system或孤立tool消息")
		}
		i++
		if len(message.ToolCalls) > 0 {
			if message.Role != "assistant" {
				return nil, errors.New("tool_calls必须属于assistant")
			}
			ids := map[string]bool{}
			for _, call := range message.ToolCalls {
				if call.ID == "" || ids[call.ID] {
					return nil, errors.New("工具调用ID为空或重复")
				}
				ids[call.ID] = true
			}
			for range message.ToolCalls {
				if i >= len(history) || history[i].Role != "tool" || !ids[history[i].ToolCallID] {
					return nil, errors.New("工具调用与全部结果必须完整配对")
				}
				delete(ids, history[i].ToolCallID)
				i++
			}
		}
		groups = append(groups, Group{start, i})
	}
	return groups, nil
}

func Parameters(name string) map[string]any {
	properties := map[string]any{}
	required := []string{}
	if name != "" {
		properties[name] = map[string]string{"type": "string"}
		required = append(required, name)
	}
	return map[string]any{"type": "object", "properties": properties, "required": required}
}
