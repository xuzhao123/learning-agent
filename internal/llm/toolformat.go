package llm

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// D37 消融（O5）：工具结果发给模型的格式。json（默认）是整个 Observation 用 json.Marshal 编码：文件内容里的换行、引号都被转义，
// < > & 还会变成 < 一类。text 只在发请求时换成纯文本（检查点、存档、存长输出、观测台都仍是 JSON）：
// 第一行是工具、状态等外层字段，结果里的多行文字原样分行。
var ToolFormat = "json"

func ToolText(content string) string {
	var o map[string]any
	if json.Unmarshal([]byte(content), &o) != nil {
		return content
	}
	head := []string{}
	for _, k := range []string{"tool", "status", "attempts", "source", "authority"} {
		if v, ok := o[k]; ok {
			head = append(head, k+"="+plain(v))
		}
	}
	lines := []string{"[" + strings.Join(head, " ") + "]"}
	for _, k := range []string{"error", "note"} {
		if v, ok := o[k].(string); ok && v != "" {
			lines = append(lines, k+": "+v)
		}
	}
	if r, ok := o["result"]; ok {
		lines = append(lines, render("", r)...)
	}
	return strings.Join(lines, "\n")
}

// render 把结果展开：对象按键排序，多行字符串原样分行（前面一行写键名），其余值一行。
func render(key string, v any) []string {
	if m, ok := v.(map[string]any); ok {
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var lines []string
		for _, k := range keys {
			name := k
			if key != "" {
				name = key + "." + k
			}
			lines = append(lines, render(name, m[k])...)
		}
		return lines
	}
	if s, ok := v.(string); ok && strings.Contains(s, "\n") {
		if key == "" {
			return []string{s}
		}
		return []string{key + ":", s}
	}
	if key == "" {
		return []string{plain(v)}
	}
	return []string{key + ": " + plain(v)}
}

// plain：字符串不加引号；其余值用不转义 HTML 的 JSON。
func plain(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if enc.Encode(v) != nil {
		return fmt.Sprint(v)
	}
	return strings.TrimSpace(buf.String())
}
