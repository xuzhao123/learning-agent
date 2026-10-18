// Package commands 是 D26 的自定义斜杠命令：用户输入 /名字 参数，harness 把对应的 Markdown 模板展开成本轮的提示词。
// 和 skill 的区别在于谁来触发：skill 由模型看索引后决定加载，斜杠命令由用户明确输入；内容都只是文字，不执行任何东西。
//
//	个人级  commands/<名字>.md（harness 目录，所有工作区都能用）
//	项目级  <工作区>/.agents/commands/<名字>.md（随仓库共享；同名时项目级优先）
//
// 文件可以有 frontmatter（description、argument-hint），正文里 $ARGUMENTS 换成全部参数，$1…$9 换成按空格切开的第几个参数；
// 正文没有引用参数而用户给了参数时，参数追加在末尾（和 Claude Code 一致）。
package commands

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"learning-agent/internal/skills"
)

type Command struct {
	Name         string `json:"name"`
	Description  string `json:"description"`
	ArgumentHint string `json:"argument_hint,omitempty"`
	Scope        string `json:"scope"` // user、project
	path         string
}

// 只有 /名字 后面是空白或结尾才算命令，所以 /home/dev/x 这样的路径不会被当成命令。
var invocation = regexp.MustCompile(`^/([a-z0-9][a-z0-9_-]*)(?:\s+([\s\S]*))?$`)

// List 列出可用的命令；workspace 为空时只有个人级。坏文件跳过，不影响其他命令。
func List(harness, workspace string) []Command {
	found := map[string]Command{}
	dirs := [][2]string{{filepath.Join(harness, "commands"), "user"}}
	if workspace != "" {
		dirs = append(dirs, [2]string{filepath.Join(workspace, ".agents", "commands"), "project"})
	}
	for _, d := range dirs {
		paths, _ := filepath.Glob(filepath.Join(d[0], "*.md"))
		for _, path := range paths {
			name := strings.TrimSuffix(filepath.Base(path), ".md")
			if !invocation.MatchString("/" + name) {
				continue
			}
			c, _, err := read(path)
			if err != nil {
				continue
			}
			c.Name, c.Scope = name, d[1]
			found[name] = c
		}
	}
	out := []Command{}
	for _, c := range found {
		out = append(out, c)
	}
	slices.SortFunc(out, func(a, b Command) int { return strings.Compare(a.Name, b.Name) })
	return out
}

func read(path string) (Command, string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Command{}, "", err
	}
	if len(data) > 32<<10 {
		return Command{}, "", errors.New("命令文件超过 32KB")
	}
	text := string(data)
	c := Command{path: path}
	if strings.HasPrefix(strings.TrimPrefix(text, "\ufeff"), "---") {
		fields, body, err := skills.ParseFrontmatter(text)
		if err != nil {
			return Command{}, "", err
		}
		c.Description, c.ArgumentHint, text = fields["description"], fields["argument-hint"], body
	}
	return c, strings.TrimSpace(text), nil
}

// Expand 处理用户输入：不是 /名字 开头的原样返回（name 为空）；是命令就返回展开后的提示词。
// 命令不存在时报错，不把“/拼错的名字”当普通问题发给模型。
func Expand(input, harness, workspace string) (prompt, name string, err error) {
	m := invocation.FindStringSubmatch(strings.TrimSpace(input))
	if m == nil {
		return input, "", nil
	}
	name, args := m[1], strings.TrimSpace(m[2])
	var command *Command
	all := List(harness, workspace)
	for i := range all {
		if all[i].Name == name {
			command = &all[i]
		}
	}
	if command == nil {
		names := []string{}
		for _, c := range all {
			names = append(names, "/"+c.Name)
		}
		return "", name, fmt.Errorf("没有 /%s 这个命令；可用的命令：%s", name, strings.Join(names, "、"))
	}
	_, body, err := read(command.path)
	if err != nil {
		return "", name, fmt.Errorf("命令 /%s 读取失败：%w", name, err)
	}
	fields := strings.Fields(args)
	used := strings.Contains(body, "$ARGUMENTS")
	body = strings.ReplaceAll(body, "$ARGUMENTS", args)
	for i := 9; i >= 1; i-- {
		placeholder := fmt.Sprintf("$%d", i)
		value := ""
		if i <= len(fields) {
			value = fields[i-1]
		}
		used = used || strings.Contains(body, placeholder)
		body = strings.ReplaceAll(body, placeholder, value)
	}
	if !used && args != "" {
		body += "\n\nARGUMENTS: " + args
	}
	return body, name, nil
}
