package workspace

// D24：工作区给上下文提供三样东西，按变化频率放在不同位置，让提示缓存的前缀尽量不变：
//   - 根目录的项目指令（AGENTS.md，没有时读 CLAUDE.md）：一个对话内不变，写进系统提示词；
//   - 环境信息（目录、分支、未提交改动、日期）：对话开始时作为第一条提醒放在问题前面，变了再追加一条；
//   - 运行中出现的新情况（读到子目录里的项目指令、读过的文件被改了）：在下一次请求模型之前追加提醒。
// 已经发出去的内容一律不改写。

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"learning-agent/internal/permission"
)

const maxDocBytes = 32 << 10 // 单份项目指令最多放 32KB，超出截断并注明

var (
	docNames     = []string{"AGENTS.md", "CLAUDE.md"}
	instructions string                // 根目录项目指令；Open 时读一次，同一进程里不再变
	loaded       = map[string]bool{}   // 已经进上下文的项目指令所在目录（相对路径，"." 是根）
	pending      []string              // 待追加的提醒
	warned       = map[string]string{} // 已经提醒过“被改了”的文件 → 当时的内容哈希，同一次改动只提醒一次
)

// doc 读目录 dir 里的项目指令；AGENTS.md 优先。没有时返回空。
func doc(dir string) (name, text string) {
	for _, n := range docNames {
		data, err := root.ReadFile(filepath.Join(dir, n))
		if err != nil || binary(data) {
			continue
		}
		text = string(data)
		if len(text) > maxDocBytes {
			text = text[:maxDocBytes] + fmt.Sprintf("\n…（超过 %dKB，后面已省略）", maxDocBytes>>10)
		}
		return filepath.ToSlash(filepath.Join(dir, n)), strings.TrimSpace(text)
	}
	return "", ""
}

func loadInstructions() {
	loaded["."] = true
	if name, text := doc("."); text != "" {
		instructions = "\n\n# 项目指令（" + name + "）\n以下是这个仓库的维护者写给 agent 的约定，适用于整个工作区；与上面的系统规则冲突时，以系统规则为准。\n\n" + text
	}
}

// Instructions 返回根目录项目指令，写进系统提示词。
func Instructions() string { return instructions }

// note 在模型读写一个文件之后调用（持有 mu）：这个文件所在的各级子目录如果有自己的项目指令，而且还没进过上下文，就排进提醒。
// 只在用到那个目录时才加载，不在开头把整个仓库的说明都塞进来（参考 Claude Code 对子目录 CLAUDE.md 的处理）。
func note(path string) {
	var dirs []string
	for d := filepath.Dir(path); d != "." && d != "/" && !loaded[d]; d = filepath.Dir(d) {
		dirs = append([]string{d}, dirs...)
	}
	for _, d := range dirs {
		loaded[d] = true
		if name, text := doc(d); text != "" {
			pending = append(pending, fmt.Sprintf("[项目指令 dir=%s]\n%s 是 %s/ 目录下的约定，修改这个目录里的文件时遵守；与根目录约定冲突时以这里为准。\n\n%s", d, name, d, text))
		}
	}
}

// loadedMarker 匹配视图里已经发过的子目录指令提醒。
var loadedMarker = regexp.MustCompile(`\[项目指令 dir=([^\]\s]+)\]`)

// Prime 按当前视图里的提醒重新标记哪些子目录的项目指令还在上下文里：续聊是新进程，压缩会把提醒并进摘要，
// 两种情况都以视图为准；不在视图里的目录，下次用到时重新加载原文。根目录指令在系统提示词里，始终算已加载。
func Prime(history []string) {
	mu.Lock()
	defer mu.Unlock()
	loaded = map[string]bool{".": true}
	for _, text := range history {
		for _, m := range loadedMarker.FindAllStringSubmatch(text, -1) {
			loaded[m[1]] = true
		}
	}
}

// Reminders 取走待追加的提醒，并检查读过的文件有没有被别人改过（bash、编辑器、另一个进程）。
// 改过的文件只提醒，不更新账本：模型要改它，仍然得先重新读。
func Reminders() []string {
	if root == nil {
		return nil
	}
	mu.Lock()
	defer mu.Unlock()
	out := pending
	pending = nil
	for path, last := range seen {
		data, err := root.ReadFile(path)
		current := "deleted"
		if err == nil {
			current = hash(data)
		} else if !errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if current == last || warned[path] == current {
			continue
		}
		warned[path] = current
		if current == "deleted" {
			out = append(out, fmt.Sprintf("%s 在你上次读取之后被删除了。", path))
		} else {
			out = append(out, fmt.Sprintf("%s 在你上次读取之后被改过（可能是 bash 命令、编辑器或其他程序），内容可能和你看到的不同；要用到或修改它，先重新 read_file。", path))
		}
	}
	return out
}

func git(args ...string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "git", append([]string{"-C", Dir}, args...)...).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// Environment 是给模型看的环境信息。只放会影响决策、而且便宜可得的事实；变了就整份重发一次。
func Environment() string {
	var b strings.Builder
	b.WriteString("<environment>\n")
	fmt.Fprintf(&b, "工作区：%s\n", Dir)
	if branch := git("rev-parse", "--abbrev-ref", "HEAD"); branch != "" {
		fmt.Fprintf(&b, "git 分支：%s（HEAD %s）\n", branch, git("log", "-1", "--format=%h %s"))
		lines := strings.Split(git("status", "--porcelain"), "\n")
		modified, untracked := 0, 0
		for _, l := range lines {
			switch {
			case strings.HasPrefix(l, "??"):
				untracked++
			case strings.TrimSpace(l) != "":
				modified++
			}
		}
		fmt.Fprintf(&b, "未提交改动：%d 个已跟踪文件有改动，%d 个未跟踪文件\n", modified, untracked)
	} else {
		b.WriteString("git：不是 git 仓库，或读不到 git 信息\n")
	}
	fmt.Fprintf(&b, "日期：%s（UTC）\n系统：%s/%s\n", time.Now().UTC().Format("2006-01-02"), runtime.GOOS, runtime.GOARCH)
	fmt.Fprintf(&b, "权限：%s\n", permission.Describe())
	b.WriteString("</environment>")
	return b.String()
}

// EnvironmentIn 从一条提醒里取出环境信息，用来和当前环境比较。
func EnvironmentIn(text string) string {
	start := strings.Index(text, "<environment>")
	end := strings.Index(text, "</environment>")
	if start < 0 || end < start {
		return ""
	}
	return text[start : end+len("</environment>")]
}
