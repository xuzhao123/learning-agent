// Package permission 是 D25 的统一权限策略：每次工具调用执行前，先按用户写的规则、再按权限模式，决定允许、询问还是拒绝。
//
//	规则  Bash(go build *)、Edit(internal/agent/**)、Read(.env)、WebFetch(domain:github.com)、mcp_xxx
//	顺序  deny › ask › allow：先看拒绝，再看询问，最后看允许；命中第一条就定，具体程度不改变顺序（Claude Code 同样如此）
//	模式  plan（只读）· ask（改动都问）· edit（工作区内的编辑和沙箱里的 bash 直接做，默认）· full（不问，deny、ask 规则照常）
//
// 规则和模式只能由用户给（命令行参数或观测台设置），模型、工具结果和项目指令都改不了它们。
// D20 的高风险确认是程序写死的另一道关：权限放行之后照样检查。full 模式和 Claude Code 的 bypassPermissions、
// Codex 的完全访问一样连它也不问，由程序放行并写审计；用户写的 deny 规则照样拒绝，ask 规则照样询问。
package permission

import (
	"encoding/json"
	"fmt"
	"path"
	"regexp"
	"strings"
)

var Modes = []string{"plan", "ask", "edit", "full"}

var (
	Mode  = "edit"
	rules []rule
)

type rule struct {
	effect  string // allow、ask、deny
	tool    string // Bash、Edit、Read、WebFetch，或具体工具名
	pattern string // 括号里的内容；空表示这个工具的所有调用
	text    string // 原文，用于说明原因
}

var ruleSyntax = regexp.MustCompile(`^([A-Za-z_][A-Za-z0-9_]*)(?:\((.*)\))?$`)

// Add 加一条规则，如 Add("deny", "Bash(git push *)")。
func Add(effect, text string) error {
	m := ruleSyntax.FindStringSubmatch(strings.TrimSpace(text))
	if m == nil {
		return fmt.Errorf("规则格式是 工具名 或 工具名(模式)，如 Bash(go build *)、Edit(internal/**)：%q", text)
	}
	pattern := strings.TrimSpace(m[2])
	// 调用里的域名由 host() 转成小写，规则里的也要转，否则 WebFetch(domain:GitHub.com) 永远匹配不到。
	if domain, ok := strings.CutPrefix(pattern, "domain:"); ok {
		pattern = "domain:" + strings.ToLower(domain)
	}
	rules = append(rules, rule{effect: effect, tool: m[1], pattern: pattern, text: strings.TrimSpace(text)})
	return nil
}

// 工具分类：规则里写的类别名对应哪些工具，以及这个调用要拿来匹配的值（命令、路径或域名）。
func subject(name, arguments string) (category, value string) {
	var args struct {
		Command string `json:"command"`
		Path    string `json:"path"`
		URL     string `json:"url"`
		Query   string `json:"query"`
	}
	_ = json.Unmarshal([]byte(arguments), &args)
	switch name {
	case "bash":
		return "Bash", args.Command
	case "edit_file", "write_file":
		return "Edit", clean(args.Path)
	case "read_file", "glob", "grep", "view_image":
		return "Read", clean(args.Path)
	case "open_page", "ds_fetch":
		return "WebFetch", "domain:" + host(args.URL)
	case "web_search", "ds_search":
		return "WebSearch", args.Query
	}
	return name, ""
}

func clean(p string) string { return strings.TrimPrefix(path.Clean("/"+strings.TrimSpace(p)), "/") }

func host(raw string) string {
	raw = strings.TrimSpace(raw)
	if i := strings.Index(raw, "://"); i >= 0 {
		raw = raw[i+3:]
	}
	raw, _, _ = strings.Cut(raw, "/")
	raw, _, _ = strings.Cut(raw, ":")
	return strings.ToLower(raw)
}

// glob 匹配：* 匹配任意字符（包括 /），这样 Bash(go test *) 能匹配带路径参数的命令，Edit(internal/**) 能匹配子目录。
func glob(pattern, value string) bool {
	re := "^" + strings.ReplaceAll(regexp.QuoteMeta(pattern), `\*`, ".*") + "$"
	ok, _ := regexp.MatchString(re, value)
	return ok
}

// segments 把一条 shell 命令按 &&、||、;、| 和换行切开。只是粗切（不理解引号），用来让
// “允许 go build”不至于顺带放行“go build && curl …”：允许要求每一段都命中，拒绝只要有一段命中。
func segments(command string) []string {
	parts := regexp.MustCompile(`&&|\|\||[;|\n]`).Split(command, -1)
	out := []string{}
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func (r rule) matches(category, name, value string) bool {
	if r.tool != category && r.tool != name {
		return false
	}
	if r.pattern == "" {
		return true
	}
	if category != "Bash" {
		return glob(r.pattern, value)
	}
	parts := segments(value)
	if r.effect == "deny" {
		for _, p := range parts {
			if glob(r.pattern, p) {
				return true
			}
		}
		return false
	}
	for _, p := range parts {
		if !glob(r.pattern, p) {
			return false
		}
	}
	return len(parts) > 0
}

// Decide 返回这次调用的决定（allow、ask、deny）和原因。
func Decide(name, arguments string) (string, string) {
	category, value := subject(name, arguments)
	for _, effect := range []string{"deny", "ask", "allow"} {
		for _, r := range rules {
			if r.effect == effect && r.matches(category, name, value) {
				return effect, "规则 " + effect + ": " + r.text
			}
		}
	}
	return modeDefault(category, name, value), "权限模式 " + Mode
}

// 没有规则命中时按模式决定。只读的工具在任何模式下都直接做；改文件、跑命令、调外部服务的工具按模式。
func modeDefault(category, name, value string) string {
	switch category {
	case "Edit":
		return map[string]string{"plan": "deny", "ask": "ask", "edit": "allow", "full": "allow"}[Mode]
	case "Bash":
		// plan 模式下 bash 照常执行，但工作区挂成只读（见 sandbox），命令改不了代码。
		// ask 模式下，每一段都是只读命令（ls、cat、git diff……）的不问，免得看一眼 diff 也要人确认。
		if Mode == "ask" && readOnly(value) {
			return "allow"
		}
		return map[string]string{"plan": "allow", "ask": "ask", "edit": "allow", "full": "allow"}[Mode]
	}
	if strings.HasPrefix(name, "mcp_") || name == "remember_memory" {
		return map[string]string{"plan": "ask", "ask": "ask", "edit": "allow", "full": "allow"}[Mode]
	}
	return "allow"
}

// 只读命令：只看不改的常用命令（参考 Claude Code 内置的只读命令集）。带重定向的不算，一段不在名单里就不算。
var readOnlyCommands = map[string]bool{"ls": true, "cat": true, "head": true, "tail": true, "wc": true, "grep": true, "find": true,
	"pwd": true, "echo": true, "stat": true, "du": true, "diff": true, "which": true, "file": true, "tree": true}
var readOnlyGit = map[string]bool{"status": true, "diff": true, "log": true, "show": true, "branch": true, "blame": true, "ls-files": true}

// gitSubcommand 跳过 git 的全局选项（-C 目录、-c 键=值、--no-pager 等），取出子命令。
func gitSubcommand(args []string) string {
	for len(args) > 0 {
		switch {
		case args[0] == "-C" || args[0] == "-c":
			args = args[min(2, len(args)):]
		case strings.HasPrefix(args[0], "-"):
			args = args[1:]
		default:
			return args[0]
		}
	}
	return ""
}

func readOnly(command string) bool {
	parts := segments(command)
	if len(parts) == 0 || strings.ContainsAny(command, ">`$") {
		return false
	}
	for _, p := range parts {
		fields := strings.Fields(p)
		switch {
		case fields[0] == "git" && readOnlyGit[gitSubcommand(fields[1:])]:
		case fields[0] == "find" && (strings.Contains(p, "-exec") || strings.Contains(p, "-delete")):
			return false
		case readOnlyCommands[fields[0]]:
		default:
			return false
		}
	}
	return true
}

// Describe 写进环境信息：模型知道当前模式和规则，就不会反复尝试会被拒的操作。
func Describe() string {
	text := map[string]string{
		"plan": "plan（只读：不能改文件，bash 里工作区只读；先给出计划）",
		"ask":  "ask（改文件、跑 bash、调 MCP 前都要用户确认）",
		"edit": "edit（工作区内的编辑和沙箱里的 bash 直接执行，MCP 照常）",
		"full": "full（不再询问，高风险操作也直接执行并写审计；用户的 deny、ask 规则照常）",
	}[Mode]
	for _, r := range rules {
		text += "；" + r.effect + " " + r.text
	}
	return text
}
