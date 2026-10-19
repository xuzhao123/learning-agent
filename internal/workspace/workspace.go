// Package workspace 是 D23 的编码工具：在用户用 -workspace 指定的仓库里读、找、搜、改文件。
// 所有路径都经 os.Root 访问，../ 与指向目录外的符号链接都出不去；
// 编辑与覆盖之前核对“上次读到的内容”，对不上就拒绝，让模型重新读，而不是覆盖别人（或自己并行的另一次编辑）的改动。
// “读过哪些文件”本来只在进程内存里，续聊换进程就会丢，所以由 agent 随检查点保存、开局恢复（见 Seen、RestoreSeen）。
// D29：每次写入之前先存快照，按轮撤销见 snapshot.go。
package workspace

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"

	"learning-agent/internal/llm"
)

// 输出预算：工具结果最终还要经过上下文的工具输出上限，这里先按“模型能用”的粒度切好，并告诉它怎么读下一段。
const (
	defaultLines = 200     // read_file 默认读多少行
	maxLines     = 2000    // read_file 一次最多读多少行
	maxLineChars = 300     // 单行超过就截断
	maxFileBytes = 2 << 20 // 超过 2MB 的文件不整份读，请用 grep 定位
	maxGlob      = 200     // glob 最多返回多少个路径
	maxMatches   = 100     // grep 最多返回多少处
	maxDiffLines = 60      // edit_file 返回的改动片段最多多少行
)

var (
	Dir  string // 工作区的绝对路径；空表示没有启用编码工具
	root *os.Root

	mu   sync.Mutex            // 保护 seen，并让“核对 + 写入”成为一步：并行的两次编辑不会互相覆盖
	seen = map[string]string{} // 路径 → 模型最近一次读到（或自己写入）的内容哈希
)

// 不进入的目录：版本库内部、运行数据和依赖缓存，内容大且不是要改的代码。
var skipDirs = map[string]bool{".git": true, ".data": true, "node_modules": true}

func object(properties map[string]any, required ...string) map[string]any {
	return map[string]any{"type": "object", "properties": properties, "required": required}
}

var Definitions = []map[string]any{
	{"name": "read_file", "description": "读取工作区里的文本文件，每行前面带行号（行号与制表符不属于文件内容）。默认从第1行读200行；文件更长时结果里有 next_offset，用它继续读。改文件之前必须先读。",
		"parameters": object(map[string]any{
			"path":   map[string]string{"type": "string", "description": "相对工作区根目录的路径"},
			"offset": map[string]string{"type": "integer", "description": "从第几行开始，默认1"},
			"limit":  map[string]string{"type": "integer", "description": "读多少行，默认200，最多2000"},
		}, "path")},
	{"name": "glob", "description": "按文件名模式查找工作区里的文件，返回相对路径（最多200个，按路径排序）。* 不跨目录，** 跨任意层目录；模式里没有 / 时只匹配文件名，如 *.go。",
		"parameters": object(map[string]any{
			"pattern": map[string]string{"type": "string", "description": "如 **/*.go、internal/*/README.md、*_test.go"},
		}, "pattern")},
	{"name": "grep", "description": "用 Go 正则表达式（RE2 语法）搜索工作区文件内容，返回“路径:行号:内容”（最多100处）。可以限定子目录和文件名模式。跳过二进制文件和超过2MB的文件。",
		"parameters": object(map[string]any{
			"pattern":     map[string]string{"type": "string", "description": "正则表达式，如 func Run\\("},
			"path":        map[string]string{"type": "string", "description": "只搜这个子目录或文件，默认整个工作区"},
			"glob":        map[string]string{"type": "string", "description": "只搜文件名符合这个模式的文件，规则同 glob 工具"},
			"ignore_case": map[string]string{"type": "boolean", "description": "忽略大小写"},
		}, "pattern")},
	{"name": "edit_file", "description": "把文件里的 old_string 替换成 new_string。old_string 要从 read_file 的输出里原样复制（不含行号前缀，空格与缩进一致），并且在文件中只出现一次；要替换所有出现处时设 replace_all=true。改之前必须先 read_file，文件在那之后被改过会被拒绝。返回改动处的片段。",
		"parameters": object(map[string]any{
			"path":        map[string]string{"type": "string", "description": "相对工作区根目录的路径"},
			"old_string":  map[string]string{"type": "string", "description": "要被替换的原文"},
			"new_string":  map[string]string{"type": "string", "description": "替换后的内容"},
			"replace_all": map[string]string{"type": "boolean", "description": "替换所有出现处，默认 false"},
		}, "path", "old_string", "new_string")},
	{"name": "write_file", "description": "新建文件，或整体覆盖一个已经 read_file 读过的文件（读过之后又被改过会被拒绝）。局部修改请用 edit_file。上级目录不存在时会创建。",
		"parameters": object(map[string]any{
			"path":    map[string]string{"type": "string", "description": "相对工作区根目录的路径"},
			"content": map[string]string{"type": "string", "description": "文件的完整内容"},
		}, "path", "content")},
}

// Open 打开工作区目录；之后所有文件操作都限制在它里面。
func Open(dir string) error {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	if info, err := os.Stat(abs); err != nil || !info.IsDir() {
		return fmt.Errorf("workspace 需要是一个已存在的目录：%s", dir)
	}
	root, err = os.OpenRoot(abs)
	if err != nil {
		return err
	}
	Dir = abs
	loadInstructions()
	return nil
}

// Rules 写进系统提示词：告诉模型工作区在哪、文件工具怎么配合使用。
func Rules() string {
	return "\n工作区：" + Dir + "。read_file、glob、grep、edit_file、write_file 的 path 都相对这个目录。先用 glob、grep 找到位置，用 read_file 看清内容再改；局部修改用 edit_file，old_string 从 read_file 的输出原样复制（不含行号前缀）并保证唯一；新建文件或整体重写才用 write_file。改完说明改了哪些文件、哪几处。"
}

// Run 按工具名执行；参数或路径有问题都是永久错误（重试也一样），交给模型修正。
func Run(ctx context.Context, name, arguments string) (any, error) {
	if root == nil {
		return nil, llm.Permanent(errors.New("请使用 -workspace 指定工作区目录"))
	}
	var args struct {
		Path       string `json:"path"`
		Offset     int    `json:"offset"`
		Limit      int    `json:"limit"`
		Pattern    string `json:"pattern"`
		Glob       string `json:"glob"`
		IgnoreCase bool   `json:"ignore_case"`
		OldString  string `json:"old_string"`
		NewString  string `json:"new_string"`
		ReplaceAll bool   `json:"replace_all"`
		Content    string `json:"content"`
	}
	if err := json.Unmarshal([]byte(arguments), &args); err != nil {
		return nil, llm.Permanent(fmt.Errorf("参数类型不对：%v", err))
	}
	switch name {
	case "read_file":
		return readFile(args.Path, args.Offset, args.Limit)
	case "glob":
		return globFiles(ctx, args.Pattern)
	case "grep":
		return grep(ctx, args.Pattern, args.Path, args.Glob, args.IgnoreCase)
	case "edit_file":
		return editFile(args.Path, args.OldString, args.NewString, args.ReplaceAll)
	case "write_file":
		return writeFile(args.Path, args.Content)
	}
	return nil, llm.Permanent(fmt.Errorf("未知工具：%s", name))
}

// clean 把模型给的路径变成相对工作区的路径；绝对路径只接受工作区里面的。真正的越界检查由 os.Root 做。
func clean(p string) (string, error) {
	p = strings.TrimSpace(p)
	if p == "" {
		return "", llm.Permanent(errors.New("path 不能为空"))
	}
	if filepath.IsAbs(p) {
		rel, err := filepath.Rel(Dir, p)
		if err != nil || rel == ".." || strings.HasPrefix(rel, "../") {
			return "", llm.Permanent(fmt.Errorf("路径不在工作区里：%s", p))
		}
		p = rel
	}
	return filepath.ToSlash(filepath.Clean(p)), nil
}

// escapes 判断 os.Root 拒绝越界访问的错误（标准库没有导出这个错误值）。
func escapes(err error) bool {
	return err != nil && strings.Contains(err.Error(), "path escapes from parent")
}

func hash(data []byte) string {
	sum := sha256.Sum256(data)
	return fmt.Sprintf("%x", sum[:8])
}

func binary(data []byte) bool {
	return bytes.IndexByte(data[:min(len(data), 8000)], 0) >= 0
}

// read 经 os.Root 读整份文件；越界、不存在、是目录、太大、是二进制都给出模型能照着改的错误。
func read(path string) ([]byte, error) {
	info, err := root.Stat(path)
	if err != nil {
		if escapes(err) {
			return nil, llm.Permanent(fmt.Errorf("%s 超出工作区（含指向工作区外的符号链接），不能访问", path))
		}
		if errors.Is(err, fs.ErrNotExist) {
			return nil, llm.Permanent(fmt.Errorf("文件不存在：%s（可以先用 glob 查找）", path))
		}
		return nil, llm.Permanent(fmt.Errorf("无法访问 %s：%v", path, err))
	}
	if info.IsDir() {
		return nil, llm.Permanent(fmt.Errorf("%s 是目录；用 glob 列出里面的文件", path))
	}
	if info.Size() > maxFileBytes {
		return nil, llm.Permanent(fmt.Errorf("%s 有 %d 字节，超过 2MB；用 grep 定位后再按行读", path, info.Size()))
	}
	data, err := root.ReadFile(path)
	if err != nil {
		return nil, llm.Permanent(fmt.Errorf("无法读取 %s：%v", path, err))
	}
	if binary(data) {
		return nil, llm.Permanent(fmt.Errorf("%s 是二进制文件，不能按文本读取或修改", path))
	}
	return data, nil
}

func readFile(p string, offset, limit int) (any, error) {
	path, err := clean(p)
	if err != nil {
		return nil, err
	}
	data, err := read(path)
	if err != nil {
		return nil, err
	}
	mu.Lock()
	seen[path] = hash(data)
	note(path)
	mu.Unlock()
	lines := strings.Split(string(data), "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1] // 末尾换行不算一行
	}
	if offset < 1 {
		offset = 1
	}
	if limit < 1 {
		limit = defaultLines
	}
	limit = min(limit, maxLines)
	result := map[string]any{"path": path, "total_lines": len(lines)}
	if offset > len(lines) {
		if len(lines) == 0 {
			result["content"] = "（空文件）"
			return result, nil
		}
		return nil, llm.Permanent(fmt.Errorf("offset=%d 超过文件总行数 %d", offset, len(lines)))
	}
	end := min(offset-1+limit, len(lines))
	var b strings.Builder
	cut := 0
	for i := offset - 1; i < end; i++ {
		line := lines[i]
		if r := []rune(line); len(r) > maxLineChars {
			line, cut = string(r[:maxLineChars])+"…（本行已截断）", cut+1
		}
		fmt.Fprintf(&b, "%6d\t%s\n", i+1, line)
	}
	result["start"], result["end"], result["content"] = offset, end, b.String()
	if cut > 0 {
		result["truncated_lines"] = cut
	}
	if end < len(lines) {
		result["next_offset"] = end + 1
	}
	return result, nil
}

// pattern 把 glob 模式转成正则：** 跨目录，* 与 ? 不跨目录。模式里没有 / 时只看文件名。
func pattern(glob string) (*regexp.Regexp, bool, error) {
	glob = strings.TrimPrefix(filepath.ToSlash(strings.TrimSpace(glob)), "./")
	if glob == "" {
		return nil, false, llm.Permanent(errors.New("pattern 不能为空"))
	}
	var b strings.Builder
	b.WriteString("^")
	for i := 0; i < len(glob); i++ {
		switch c := glob[i]; {
		case strings.HasPrefix(glob[i:], "**/"):
			b.WriteString("(?:.*/)?")
			i += 2
		case strings.HasPrefix(glob[i:], "**"):
			b.WriteString(".*")
			i++
		case c == '*':
			b.WriteString("[^/]*")
		case c == '?':
			b.WriteString("[^/]")
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	b.WriteString("$")
	re, err := regexp.Compile(b.String())
	if err != nil {
		return nil, false, llm.Permanent(fmt.Errorf("pattern 无效：%v", err))
	}
	return re, !strings.Contains(glob, "/"), nil
}

// walk 遍历工作区里的普通文件（经 os.Root 的 FS，不跟随符号链接），跳过 skipDirs。
func walk(ctx context.Context, start string, visit func(path string) bool) error {
	return fs.WalkDir(root.FS(), start, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if path == start {
				return llm.Permanent(fmt.Errorf("无法访问 %s：%v", start, err))
			}
			return nil // 个别目录没有权限就跳过
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if d.IsDir() {
			if path != start && skipDirs[d.Name()] {
				return fs.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		if !visit(path) {
			return fs.SkipAll
		}
		return nil
	})
}

func matches(re *regexp.Regexp, nameOnly bool, path string) bool {
	if nameOnly {
		return re.MatchString(filepath.Base(path))
	}
	return re.MatchString(path)
}

func globFiles(ctx context.Context, glob string) (any, error) {
	re, nameOnly, err := pattern(glob)
	if err != nil {
		return nil, err
	}
	found := []string{}
	more := false
	err = walk(ctx, ".", func(path string) bool {
		if matches(re, nameOnly, path) {
			if len(found) == maxGlob {
				more = true
				return false
			}
			found = append(found, path)
		}
		return true
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(found)
	result := map[string]any{"pattern": glob, "files": found, "count": len(found)}
	if more {
		result["truncated"] = fmt.Sprintf("超过 %d 个，只列出前 %d 个；请把模式写得更具体", maxGlob, maxGlob)
	}
	return result, nil
}

func grep(ctx context.Context, expr, under, glob string, ignoreCase bool) (any, error) {
	if strings.TrimSpace(expr) == "" {
		return nil, llm.Permanent(errors.New("pattern 不能为空"))
	}
	if ignoreCase {
		expr = "(?i)" + expr
	}
	re, err := regexp.Compile(expr)
	if err != nil {
		return nil, llm.Permanent(fmt.Errorf("正则无效（RE2 语法，括号等特殊字符要转义）：%v", err))
	}
	start := "."
	if strings.TrimSpace(under) != "" {
		if start, err = clean(under); err != nil {
			return nil, err
		}
	}
	var filter *regexp.Regexp
	nameOnly := false
	if strings.TrimSpace(glob) != "" {
		if filter, nameOnly, err = pattern(glob); err != nil {
			return nil, err
		}
	}
	hits := []string{}
	files, more := 0, false
	err = walk(ctx, start, func(path string) bool {
		if filter != nil && !matches(filter, nameOnly, path) {
			return true
		}
		info, err := root.Stat(path)
		if err != nil || info.Size() > maxFileBytes {
			return true
		}
		data, err := root.ReadFile(path)
		if err != nil || binary(data) {
			return true
		}
		files++
		for i, line := range strings.Split(string(data), "\n") {
			if !re.MatchString(line) {
				continue
			}
			if len(hits) == maxMatches {
				more = true
				return false
			}
			if r := []rune(line); len(r) > 200 {
				line = string(r[:200]) + "…"
			}
			hits = append(hits, fmt.Sprintf("%s:%d:%s", path, i+1, line))
		}
		return true
	})
	if err != nil {
		return nil, err
	}
	result := map[string]any{"matches": hits, "count": len(hits), "files_scanned": files}
	if more {
		result["truncated"] = fmt.Sprintf("超过 %d 处，只列出前 %d 处；请缩小 path、glob 或改写正则", maxMatches, maxMatches)
	}
	return result, nil
}

// Seen 返回账本的副本，由 agent 随本轮检查点保存：续聊是新进程，靠它接上上一轮读过哪些文件。
// 返回副本而不是这张表本身：调用方在锁外序列化，工具执行的同时可能还在往里写。
func Seen() map[string]string {
	mu.Lock()
	defer mu.Unlock()
	if len(seen) == 0 {
		return nil
	}
	out := make(map[string]string, len(seen))
	for path, sum := range seen {
		out[path] = sum
	}
	return out
}

// RestoreSeen 在开始工作时用检查点里的账本替换当前账本（nil 或空表示从空账本开始）。
// 只恢复“读过”这个事实和当时的哈希，不凭它放行写入：fresh 仍比对当前内容的哈希，
// 两轮之间被改过的文件照样要求重新 read_file。
func RestoreSeen(ledger map[string]string) {
	mu.Lock()
	defer mu.Unlock()
	seen = make(map[string]string, len(ledger))
	for path, sum := range ledger {
		seen[path] = sum
	}
}

// fresh 在持锁时调用：确认模型读过这个文件，而且读过之后没被改过。
func fresh(path string, current []byte) error {
	last, ok := seen[path]
	if !ok {
		return llm.Permanent(fmt.Errorf("修改 %s 之前需要先用 read_file 读过它", path))
	}
	if last != hash(current) {
		return llm.Permanent(fmt.Errorf("%s 在你上次读取之后被改过（外部修改或另一次编辑），请重新 read_file 再改", path))
	}
	return nil
}

// lineOf 返回字节偏移 i 所在的行号（从1开始）。
func lineOf(text string, i int) int {
	return strings.Count(text[:i], "\n") + 1
}

func editFile(p, oldString, newString string, replaceAll bool) (any, error) {
	path, err := clean(p)
	if err != nil {
		return nil, err
	}
	if oldString == "" {
		return nil, llm.Permanent(errors.New("old_string 不能为空；新建文件或整体重写请用 write_file"))
	}
	if oldString == newString {
		return nil, llm.Permanent(errors.New("old_string 与 new_string 相同，没有要改的内容"))
	}
	mu.Lock()
	defer mu.Unlock()
	data, err := read(path)
	if err != nil {
		return nil, err
	}
	if err := fresh(path, data); err != nil {
		return nil, err
	}
	text := string(data)
	count := strings.Count(text, oldString)
	if count == 0 {
		return nil, llm.Permanent(fmt.Errorf("old_string 在 %s 里没有找到；检查空格、缩进与换行是否和 read_file 输出一致，并且不要带行号前缀", path))
	}
	if count > 1 && !replaceAll {
		lines := []string{}
		for i, from := 0, 0; i < min(count, 10); i++ {
			at := strings.Index(text[from:], oldString) + from
			lines = append(lines, fmt.Sprint(lineOf(text, at)))
			from = at + len(oldString)
		}
		return nil, llm.Permanent(fmt.Errorf("old_string 在 %s 里出现了 %d 次（第 %s 行）；多带几行上下文让它唯一，或设 replace_all=true", path, count, strings.Join(lines, "、")))
	}
	first := lineOf(text, strings.Index(text, oldString))
	updated := strings.Replace(text, oldString, newString, -1)
	if !replaceAll {
		updated = strings.Replace(text, oldString, newString, 1)
	}
	if err := snapshot(path, data, true); err != nil {
		return nil, fmt.Errorf("写入前的快照没存下来，%s 没有改：%w", path, err)
	}
	if err := root.WriteFile(path, []byte(updated), 0o644); err != nil {
		return nil, fmt.Errorf("写入 %s 失败：%w", path, err)
	}
	written(path, []byte(updated))
	seen[path] = hash([]byte(updated))
	note(path)
	replaced := 1
	if replaceAll {
		replaced = count
	}
	return map[string]any{"path": path, "replacements": replaced, "first_line": first, "diff": snippet(first, oldString, newString)}, nil
}

// snippet 给出第一处改动的前后对照，供模型和人核对；不是完整的 unified diff。
func snippet(line int, oldString, newString string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "@@ 第 %d 行\n", line)
	n := 0
	for _, part := range []struct {
		sign string
		text string
	}{{"-", oldString}, {"+", newString}} {
		for _, l := range strings.Split(strings.TrimSuffix(part.text, "\n"), "\n") {
			if n == maxDiffLines {
				b.WriteString("…（片段过长，已截断）\n")
				return b.String()
			}
			b.WriteString(part.sign + l + "\n")
			n++
		}
	}
	return b.String()
}

func writeFile(p, content string) (any, error) {
	path, err := clean(p)
	if err != nil {
		return nil, err
	}
	mu.Lock()
	defer mu.Unlock()
	created := false
	var current []byte
	if info, err := root.Stat(path); err == nil {
		if info.IsDir() {
			return nil, llm.Permanent(fmt.Errorf("%s 是目录", path))
		}
		if current, err = read(path); err != nil {
			return nil, err
		}
		if err := fresh(path, current); err != nil {
			return nil, err
		}
	} else if errors.Is(err, fs.ErrNotExist) {
		created = true
		if dir := filepath.Dir(path); dir != "." {
			if err := root.MkdirAll(dir, 0o755); err != nil {
				return nil, llm.Permanent(fmt.Errorf("无法创建目录 %s：%v", dir, err))
			}
		}
	} else if escapes(err) {
		return nil, llm.Permanent(fmt.Errorf("%s 超出工作区（含指向工作区外的符号链接），不能写入", path))
	} else {
		return nil, llm.Permanent(fmt.Errorf("无法访问 %s：%v", path, err))
	}
	if err := snapshot(path, current, !created); err != nil {
		return nil, fmt.Errorf("写入前的快照没存下来，%s 没有改：%w", path, err)
	}
	if err := root.WriteFile(path, []byte(content), 0o644); err != nil {
		return nil, fmt.Errorf("写入 %s 失败：%w", path, err)
	}
	written(path, []byte(content))
	seen[path] = hash([]byte(content))
	note(path)
	return map[string]any{"path": path, "created": created, "bytes": len(content), "lines": strings.Count(content, "\n") + boolInt(!strings.HasSuffix(content, "\n") && content != "")}, nil
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
