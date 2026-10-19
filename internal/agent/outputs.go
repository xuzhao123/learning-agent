package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"learning-agent/internal/llm"
)

// D30 大工具输出：超过工具输出上限的结果，完整内容存成文件，上下文里只放开头、结尾和文件名，
// 模型需要中间的部分时用 read_output 按行读或按正则搜。之前的做法是截掉中间，被截掉的内容模型再也拿不到。
//
// 文件按对话存放（.data/outputs/<对话第一轮的检查点ID>/<调用ID>.txt）：续聊、续跑都能读到之前各轮存下的输出；
// 子 agent 与回放分叉各有自己的目录。没有检查点的运行（Day 3/4 实验）不存，仍按旧办法截断。
//
// 存下来的是“展开”后的文本：工具结果是一段 JSON，多行字符串（命令输出、网页正文、文件内容）在 JSON 里挤在一行，
// 按行读、按行搜都用不上；展开后每个字段一行，多行字符串按原样分行。
var OutputDir string

const (
	outputDefaultLines = 200 // read_output 默认读多少行
	outputLineChars    = 300 // 预览与读取时单行超过就截断
	outputMaxMatches   = 100 // pattern 最多返回多少行
)

// outputBudget 是工具输出上限（估算 token），由 NewContext 设置：read_output 自己的结果也不能超过它，否则又会被存一次。
var outputBudget = 2000

var ReadOutputDefinition = map[string]any{
	"name":        "read_output",
	"description": "读取之前因为太长而存成文件的工具输出。工具结果里有 saved_output 时，上下文里只有开头和结尾，用它的名字读中间的部分：按行号分页（offset、limit），或用 pattern（Go 正则）只看匹配的行。返回的每行带行号。",
	"parameters": map[string]any{"type": "object", "required": []string{"name"}, "properties": map[string]any{
		"name":    map[string]string{"type": "string", "description": "saved_output 里的名字"},
		"offset":  map[string]string{"type": "integer", "description": "从第几行开始，默认1"},
		"limit":   map[string]string{"type": "integer", "description": "读多少行，默认200；内容太长时会提前停下并给出 next_offset"},
		"pattern": map[string]string{"type": "string", "description": "只返回匹配这个正则的行（最多100行）；给了 pattern 时忽略 offset、limit"},
	}},
}

var outputName = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

// saveOutput 把一条过长的工具结果存成文件，返回放进上下文的版本（结果换成预览）。
// 内容不是 JSON 的 Observation、读输出本身、或没有存放目录时返回 false，调用方退回截断。
func saveOutput(content string, budget int) (string, bool) {
	var o llm.Observation
	if OutputDir == "" || json.Unmarshal([]byte(content), &o) != nil || o.Tool == "read_output" || !outputName.MatchString(o.ID) {
		return "", false
	}
	var lines []string
	if o.Result != nil {
		lines = expand("result", o.Result, lines)
	}
	if o.Error != "" {
		lines = expand("error", o.Error, lines)
	}
	if err := os.MkdirAll(OutputDir, 0o700); err != nil {
		fmt.Printf("Output save error: %v\n", err)
		return "", false
	}
	if err := os.WriteFile(filepath.Join(OutputDir, o.ID+".txt"), []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		fmt.Printf("Output save error: %v\n", err)
		return "", false
	}
	// 外层字段（状态、来源标记、说明）原样保留，只把 result 换成预览、清掉已经存进文件的 error。
	o.Error = ""
	o.Result = map[string]any{"saved_output": o.ID, "total_lines": len(lines), "preview": preview(lines, budget)}
	data, _ := json.Marshal(o)
	fmt.Printf("Output saved: name=%s lines=%d original_chars=%d view_tokens=%d\n", o.ID, len(lines), len([]rune(content)), llm.TextTokens(string(data)))
	return string(data), true
}

// expand 把 JSON 值展开成行：对象按键排序、数组按下标，多行字符串原样分行，其余值一行。
func expand(key string, v any, lines []string) []string {
	switch v := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(v))
		for k := range v {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			lines = expand(key+"."+k, v[k], lines)
		}
	case []any:
		for i, item := range v {
			lines = expand(fmt.Sprintf("%s[%d]", key, i), item, lines)
		}
	case string:
		if !strings.Contains(v, "\n") {
			return append(lines, key+": "+v)
		}
		lines = append(lines, key+":")
		lines = append(lines, strings.Split(strings.TrimSuffix(v, "\n"), "\n")...)
	default:
		data, _ := json.Marshal(v)
		lines = append(lines, key+": "+string(data))
	}
	return lines
}

func numbered(i int, line string) string {
	if r := []rune(line); len(r) > outputLineChars {
		line = string(r[:outputLineChars]) + "…（本行已截断）"
	}
	return fmt.Sprintf("%6d\t%s\n", i+1, line)
}

// preview 取开头和结尾各约一半预算的行，中间写明省略了第几到第几行。命令输出的报错、测试结果常在最后，所以结尾也要留。
// 预算扣掉外层字段与 JSON 转义的余量，放进上下文后整条结果仍在工具输出上限以内。
func preview(lines []string, budget int) string {
	half := max(1, budget*2/5)
	var head, tail []string
	used, end := 0, len(lines)
	for i := 0; i < len(lines); i++ {
		n := llm.TextTokens(numbered(i, lines[i]))
		if used+n > half && i > 0 {
			break
		}
		head, used = append(head, numbered(i, lines[i])), used+n
	}
	used = 0
	for i := len(lines) - 1; i >= len(head); i-- {
		n := llm.TextTokens(numbered(i, lines[i]))
		if used+n > half {
			break
		}
		tail, used, end = append([]string{numbered(i, lines[i])}, tail...), used+n, i
	}
	text := strings.Join(head, "")
	if end > len(head) {
		text += fmt.Sprintf("…（省略第 %d–%d 行，用 read_output 读取）\n", len(head)+1, end)
	}
	return text + strings.Join(tail, "")
}

// readOutput 是 read_output 工具：只在本对话的输出目录里按名字找，名字只允许字母、数字、_ 和 -，拼不出别的路径。
func readOutput(arguments string) (any, error) {
	var args struct {
		Name    string `json:"name"`
		Offset  int    `json:"offset"`
		Limit   int    `json:"limit"`
		Pattern string `json:"pattern"`
	}
	if err := json.Unmarshal([]byte(arguments), &args); err != nil {
		return nil, llm.Permanent(fmt.Errorf("参数类型不对：%v", err))
	}
	if OutputDir == "" {
		return nil, llm.Permanent(errors.New("这次运行没有存工具输出"))
	}
	if !outputName.MatchString(args.Name) {
		return nil, llm.Permanent(errors.New("name 是工具结果里 saved_output 的值"))
	}
	data, err := os.ReadFile(filepath.Join(OutputDir, args.Name+".txt"))
	if err != nil {
		return nil, llm.Permanent(fmt.Errorf("找不到存下的输出 %s（只能读本对话里存下的）", args.Name))
	}
	lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	result := map[string]any{"name": args.Name, "total_lines": len(lines)}
	limit := outputBudget * 4 / 5 // 留出外层字段的余量
	var b strings.Builder
	if args.Pattern != "" {
		re, err := regexp.Compile(args.Pattern)
		if err != nil {
			return nil, llm.Permanent(fmt.Errorf("pattern 不是有效的正则（RE2 语法）：%v", err))
		}
		found, used := 0, 0
		for i, line := range lines {
			if !re.MatchString(line) {
				continue
			}
			text := numbered(i, line)
			if found == outputMaxMatches || used+llm.TextTokens(text) > limit {
				result["truncated"] = "匹配太多，只列出前面的；请把正则写得更具体"
				break
			}
			b.WriteString(text)
			found, used = found+1, used+llm.TextTokens(text)
		}
		result["matches"], result["content"] = found, b.String()
		return result, nil
	}
	offset, count := max(1, args.Offset), args.Limit
	if count < 1 {
		count = outputDefaultLines
	}
	if offset > len(lines) {
		return nil, llm.Permanent(fmt.Errorf("offset=%d 超过总行数 %d", offset, len(lines)))
	}
	end, used := offset-1, 0
	for ; end < len(lines) && end < offset-1+count; end++ {
		text := numbered(end, lines[end])
		if used+llm.TextTokens(text) > limit && end > offset-1 {
			break
		}
		b.WriteString(text)
		used += llm.TextTokens(text)
	}
	result["start"], result["end"], result["content"] = offset, end, b.String()
	if end < len(lines) {
		result["next_offset"] = end + 1
	}
	return result, nil
}
