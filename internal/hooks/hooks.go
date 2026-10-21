// Package hooks 是 D26 的生命周期 hooks：harness 在固定的时刻运行用户配置的外部命令，
// 把当时的情况以 JSON 写进命令的 stdin，按退出码和 stdout 的 JSON 决定放行、拦截还是改写。
// 配置格式沿用 Claude Code 的 settings.json 里的 hooks 段：
//
//	{"hooks": {"PreToolUse": [{"matcher": "edit_file|write_file", "hooks": [{"type": "command", "command": "...", "timeout": 30}]}]}}
//
// 事件  UserPromptSubmit（用户提交）· PreToolUse（工具执行前）· PostToolUse（工具成功后）· Stop（模型准备结束）
// 退出码 0 = 成功，读 stdout 的 JSON（UserPromptSubmit 的纯文本 stdout 作为补充上下文）；
//
//	2 = 拦截，stderr 是原因；其他 = hook 自己出错，记一条日志后照常继续（不阻断）。
//
// hooks 只能由用户给（-hooks，或观测台的平台设置）：它们在宿主机上以 harness 的身份运行，不在沙箱里，
// 所以不读工作区里的 hooks 配置——没有“信任这个仓库”的确认，打开一个陌生仓库就等于执行它带的命令。
package hooks

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"learning-agent/internal/audit"
	"learning-agent/internal/protocol"
)

var Events = []string{"UserPromptSubmit", "PreToolUse", "PostToolUse", "Stop"}

type command struct {
	Type    string `json:"type"`
	Command string `json:"command"`
	Timeout int    `json:"timeout"` // 秒，默认 60
}

type group struct {
	Matcher string    `json:"matcher"` // 工具名的正则（整名匹配），空或 * 匹配全部；只用于 PreToolUse、PostToolUse
	Hooks   []command `json:"hooks"`
	match   *regexp.Regexp
}

var (
	config map[string][]group
	Text   string // 原始配置，子 agent 照原样继承
	Dir    string // hook 命令的工作目录：工作区，没有工作区时是本对话的沙箱目录
	// Harness 是本项目目录，hook 命令用 $AGENT_HARNESS_DIR 引用这里的脚本（工作目录可能是别的仓库）。
	Harness string
	// Session 是对话ID，写进每次输入，hook 可以据此把同一对话的几次调用关联起来。
	Session string
)

// Load 读取配置：以 { 开头的是 JSON 原文，否则是文件路径。
func Load(value string) error {
	text := strings.TrimSpace(value)
	if !strings.HasPrefix(text, "{") {
		data, err := os.ReadFile(text)
		if err != nil {
			return fmt.Errorf("读取 hooks 配置：%w", err)
		}
		text = string(data)
	}
	parsed, err := Parse(text)
	if err != nil {
		return err
	}
	config, Text = parsed, text
	return nil
}

// Parse 校验配置；观测台保存设置时也用它，格式错误在保存时就报出来，不等到对话开始。
func Parse(text string) (map[string][]group, error) {
	var file struct {
		Hooks map[string][]group `json:"hooks"`
	}
	decoder := json.NewDecoder(strings.NewReader(text))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&file); err != nil {
		return nil, fmt.Errorf("hooks 配置不是合法的 JSON：%w", err)
	}
	for event, groups := range file.Hooks {
		known := false
		for _, e := range Events {
			known = known || e == event
		}
		if !known {
			return nil, fmt.Errorf("不支持的 hook 事件 %s，可用：%s", event, strings.Join(Events, "、"))
		}
		for i := range groups {
			g := &groups[i]
			if g.Matcher != "" && g.Matcher != "*" {
				if event == "UserPromptSubmit" || event == "Stop" {
					return nil, fmt.Errorf("%s 不按工具匹配，去掉 matcher", event)
				}
				re, err := regexp.Compile("^(?:" + g.Matcher + ")$")
				if err != nil {
					return nil, fmt.Errorf("%s 的 matcher %q 不是合法的正则：%w", event, g.Matcher, err)
				}
				g.match = re
			}
			for _, h := range g.Hooks {
				if h.Type != "command" || strings.TrimSpace(h.Command) == "" {
					return nil, fmt.Errorf("%s 的 hook 需要 type: command 和非空的 command", event)
				}
				if h.Timeout < 0 || h.Timeout > 600 {
					return nil, fmt.Errorf("hook 的 timeout 范围为 0 到 600 秒")
				}
			}
		}
	}
	return file.Hooks, nil
}

// Has 表示这个事件配了 hook；没配时调用方不必准备输入。
func Has(event string) bool { return len(config[event]) > 0 }

// Result 是一个事件上所有 hook 的合并结论。
type Result struct {
	Block    bool            // 拦截：exit 2、decision=block，或 PreToolUse 的 permissionDecision=deny
	Reason   string          // 拦截原因（给模型或给人看）
	Decision string          // PreToolUse：allow、ask、deny；空表示 hook 没有意见
	Input    json.RawMessage // PreToolUse 改写后的工具参数；nil 表示没改
	Context  string          // 补充上下文：追加给模型看
}

type output struct {
	Decision string `json:"decision"`
	Reason   string `json:"reason"`
	Specific struct {
		PermissionDecision string          `json:"permissionDecision"`
		DecisionReason     string          `json:"permissionDecisionReason"`
		UpdatedInput       json.RawMessage `json:"updatedInput"`
		AdditionalContext  string          `json:"additionalContext"`
	} `json:"hookSpecificOutput"`
}

// Run 依次运行这个事件上匹配的 hook（按配置顺序，一个拦截就不再运行后面的）。
// input 是事件特有的字段；公共字段（事件名、对话ID、工作目录）在这里补上。
// PreToolUse 改写过参数后，后面的 hook 看到的是改写后的参数。
func Run(ctx context.Context, event, tool string, input map[string]any) Result {
	var result Result
	for _, g := range config[event] {
		if g.match != nil && !g.match.MatchString(tool) {
			continue
		}
		for _, h := range g.Hooks {
			input["hook_event_name"], input["session_id"], input["cwd"] = event, Session, Dir
			if result.Input != nil {
				input["tool_input"] = result.Input
			}
			out, outcome, reason := run(ctx, h, input)
			callID, _ := input["tool_use_id"].(string)
			if outcome == "ok" && out != nil && (out.Decision == "block" || out.Specific.PermissionDecision == "deny") {
				outcome, reason = "block", firstNonEmpty(out.Specific.DecisionReason, out.Reason, "hook 拦截了这项操作")
			}
			report(ctx, event, tool, callID, h.Command, outcome, firstNonEmpty(reason, out.text()))
			switch outcome {
			case "block":
				result.Block, result.Reason, result.Decision = true, reason, "deny"
				result.Context = join(result.Context, out.context())
				return result
			case "error":
				continue
			}
			if out == nil {
				// 纯文本 stdout：只有 UserPromptSubmit 把它当作补充上下文（和 Claude Code 一致），其他事件只进日志。
				if event == "UserPromptSubmit" && reason != "" {
					result.Context = join(result.Context, reason)
				}
				continue
			}
			s := out.Specific
			result.Context = join(result.Context, s.AdditionalContext)
			if event == "PreToolUse" {
				// 几个 hook 意见不同时取更严的：ask 压过 allow。
				if s.PermissionDecision == "ask" || (s.PermissionDecision == "allow" && result.Decision == "") {
					result.Decision, result.Reason = s.PermissionDecision, s.DecisionReason
				}
				if len(s.UpdatedInput) > 0 && string(s.UpdatedInput) != "null" {
					result.Input = s.UpdatedInput
				}
			}
		}
	}
	return result
}

// text 是给界面看的摘要：hook 的意见和补充上下文。
func (o *output) text() string {
	if o == nil {
		return ""
	}
	return strings.TrimSpace(strings.Join([]string{o.Specific.PermissionDecision, o.Specific.DecisionReason, o.Specific.AdditionalContext, string(o.Specific.UpdatedInput)}, " "))
}

func (o *output) context() string {
	if o == nil {
		return ""
	}
	return o.Specific.AdditionalContext
}

// run 执行一个 hook 命令，返回解析出的 JSON（没有时为 nil）、结局（ok、block、error）和文本（原因或 stdout）。
func run(ctx context.Context, h command, input map[string]any) (*output, string, string) {
	timeout := time.Duration(h.Timeout) * time.Second
	if timeout == 0 {
		timeout = 60 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	data, _ := json.Marshal(input)
	cmd := exec.CommandContext(ctx, "sh", "-c", h.Command)
	cmd.Dir, cmd.Stdin = Dir, bytes.NewReader(data)
	// 只给最少的环境变量：harness 进程里有模型密钥，hook 用不到也不该拿到。
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME"), "LANG=C.UTF-8", "AGENT_PROJECT_DIR=" + Dir, "AGENT_HARNESS_DIR=" + Harness}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	var exit *exec.ExitError
	switch {
	case ctx.Err() != nil:
		return nil, "error", fmt.Sprintf("超时（%s）", timeout)
	case errors.As(err, &exit) && exit.ExitCode() == 2:
		return nil, "block", firstNonEmpty(strings.TrimSpace(stderr.String()), "hook 以退出码 2 拦截")
	case err != nil:
		return nil, "error", firstNonEmpty(strings.TrimSpace(stderr.String()), err.Error())
	}
	text := strings.TrimSpace(stdout.String())
	if strings.HasPrefix(text, "{") {
		var out output
		if err := json.Unmarshal([]byte(text), &out); err != nil {
			return nil, "error", "stdout 不是合法的 JSON：" + err.Error()
		}
		return &out, "ok", ""
	}
	return nil, "ok", text
}

// report 把每次 hook 运行告诉界面；拦截写进审计（这是程序之外的一方做的决定，事后要查得到）。
func report(ctx context.Context, event, tool, callID, command, outcome, text string) {
	line := fmt.Sprintf("Hook %s", event)
	if tool != "" {
		line += "(" + tool + ")"
	}
	line += fmt.Sprintf(": %s command=%s", outcome, command)
	if text != "" {
		line += " " + firstLine(text)
	}
	protocol.Event("hook/completed", outcome, line, "event", event, "tool", tool, "call_id", callID, "command", command, "output", text)
	if outcome == "block" {
		audit.Log(ctx, audit.Record{Actor: "program", Event: "hook_block", CallID: callID, Tool: tool, Decision: "block", Detail: event + "：" + firstLine(text)})
	}
}

func join(a, b string) string {
	if a == "" || b == "" {
		return a + b
	}
	return a + "\n" + b
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return line
}
