package skills

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"

	"learning-agent/internal/llm"
	"learning-agent/internal/protocol"
)

// Bonus：知识型 skill。三级加载：索引常驻 system → load_skill 读全文 →（v1 不做）references/scripts。
// v1 只读 Markdown，不执行 skill 自带脚本；脚本执行等沙箱。
var Enabled bool
var Index []Skill // nil 表示未启用

type Skill struct{ Name, Description, Path string }

var LoadDefinition = map[string]any{
	"name":        "load_skill",
	"description": "按名称读取一个 skill 的完整操作指南（SKILL.md 正文）。只在任务与 system 中列出的某个 skill 相关时调用；name 必须是列表里的名字。",
	"parameters":  llm.Parameters("name"),
}

// spec：小写字母、数字、单个连字符分隔，不以连字符开头或结尾。
var skillNamePattern = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

const maxSkillBytes = 64 << 10

// 坏文件逐个报错并跳过，不让一个 skill 拖垮启动：索引是可选增强，降级比 fail-fast 合适。
// only 非空时只启用其中的名字（观测台 Skills 中心的开关）；列了却不存在的名字同样报错跳过。
func Scan(dir string, only []string) []Skill {
	paths, _ := filepath.Glob(filepath.Join(dir, "*", "SKILL.md"))
	skills := []Skill{}
	names := []string{}
	found := map[string]bool{}
	for _, path := range paths {
		s, _, err := readSkill(path)
		if err != nil {
			protocol.Event("capability/event", "skill_error", fmt.Sprintf("Skill error: %s: %v", path, err))
			continue
		}
		found[s.Name] = true
		if len(only) > 0 && !slices.Contains(only, s.Name) {
			continue
		}
		skills = append(skills, s)
		names = append(names, s.Name)
	}
	for _, name := range only {
		if !found[name] {
			protocol.Event("capability/event", "skill_error", "Skill error: "+name+": 没有找到这个 skill")
		}
	}
	protocol.Event("capability/event", "skills_loaded", fmt.Sprintf("Skills: loaded=%d names=%s", len(skills), strings.Join(names, ",")), "count", len(skills))
	return skills
}

// -skills-list：观测台 Skills 中心用同一套解析与校验列出 skill，不在网页端另写一份规则。
func List(dir string) map[string]any {
	paths, _ := filepath.Glob(filepath.Join(dir, "*", "SKILL.md"))
	skills, failures := []map[string]any{}, []map[string]string{}
	for _, path := range paths {
		s, body, err := readSkill(path)
		if err != nil {
			failures = append(failures, map[string]string{"path": path, "dir": filepath.Base(filepath.Dir(path)), "error": err.Error()})
			continue
		}
		skills = append(skills, map[string]any{"name": s.Name, "description": s.Description, "path": path, "body": body})
	}
	return map[string]any{"skills": skills, "errors": failures}
}

func readSkill(path string) (Skill, string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return Skill{}, "", err
	}
	if info.Size() > maxSkillBytes {
		return Skill{}, "", fmt.Errorf("文件超过 %d 字节", maxSkillBytes)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return Skill{}, "", err
	}
	fields, body, err := parseFrontmatter(string(data))
	if err != nil {
		return Skill{}, "", err
	}
	name, description := fields["name"], fields["description"]
	dir := filepath.Base(filepath.Dir(path))
	switch {
	case name == "":
		return Skill{}, "", errors.New("缺少 name")
	case len(name) > 64 || !skillNamePattern.MatchString(name):
		return Skill{}, "", fmt.Errorf("name %q 不合规：只能用小写字母、数字和单个连字符，最长64", name)
	case name != dir:
		return Skill{}, "", fmt.Errorf("name %q 与目录名 %q 不一致", name, dir)
	case description == "":
		return Skill{}, "", errors.New("缺少 description")
	case utf8.RuneCountInString(description) > 1024:
		return Skill{}, "", errors.New("description 超过1024字符")
	case utf8.RuneCountInString(fields["compatibility"]) > 500:
		return Skill{}, "", errors.New("compatibility 超过500字符")
	}
	return Skill{Name: name, Description: description, Path: path}, body, nil
}

// 只解析顶层 key: value；缩进行属于上一个键的嵌套值（如 metadata），v1 跳过。
// 不引入 YAML 库，所以不支持 | 和 > 多行值，遇到时明确报错而不是读错。
func parseFrontmatter(text string) (map[string]string, string, error) {
	text = strings.ReplaceAll(strings.TrimPrefix(text, "\ufeff"), "\r\n", "\n")
	if !strings.HasPrefix(text, "---\n") {
		return nil, "", errors.New("缺少开头的 --- frontmatter")
	}
	header, body, ok := strings.Cut(text[4:], "\n---\n")
	if !ok {
		if header, ok = strings.CutSuffix(text[4:], "\n---"); !ok {
			return nil, "", errors.New("frontmatter 缺少结尾的 ---")
		}
	}
	fields := map[string]string{}
	for i, line := range strings.Split(header, "\n") {
		if strings.TrimSpace(line) == "" || strings.HasPrefix(strings.TrimSpace(line), "#") || line[0] == ' ' || line[0] == '\t' {
			continue
		}
		key, value, ok := strings.Cut(line, ":")
		key, value = strings.TrimSpace(key), strings.TrimSpace(value)
		if !ok || key == "" {
			return nil, "", fmt.Errorf("frontmatter 第%d行需要 key: value", i+2)
		}
		if value == "|" || value == ">" || strings.HasPrefix(value, "|-") || strings.HasPrefix(value, ">-") {
			return nil, "", fmt.Errorf("frontmatter 第%d行：不支持多行值，请写在一行", i+2)
		}
		if _, dup := fields[key]; dup {
			return nil, "", fmt.Errorf("frontmatter 第%d行：重复的键 %s", i+2, key)
		}
		if len(value) >= 2 && (value[0] == '"' || value[0] == '\'') && value[len(value)-1] == value[0] {
			value = value[1 : len(value)-1]
		}
		fields[key] = value
	}
	return fields, strings.TrimSpace(body), nil
}

func Prompt() string {
	lines := []string{"\n可用 skills（只列名字与用途）。任务与某个 skill 相关时，先调用 load_skill 读取全文，再按其中的步骤执行；无关任务不要加载："}
	for _, s := range Index {
		lines = append(lines, "- "+s.Name+"："+s.Description)
	}
	return strings.Join(lines, "\n")
}

// 只按索引里的名字找文件，模型传入的字符串不会拼进路径。
// 名字不存在时返回结构化结果而不是 error：这是可自我纠正的业务错误，重试同样的参数没有意义。
func Load(name string) (any, error) {
	available := []string{}
	for _, s := range Index {
		available = append(available, s.Name)
		if s.Name != name {
			continue
		}
		// 调用时重新读取并校验，启动后被改坏的文件不会被当作指南返回。
		_, body, err := readSkill(s.Path)
		if err != nil {
			return nil, fmt.Errorf("skill %s 读取失败：%v", name, err)
		}
		return map[string]any{"name": name, "content": body}, nil
	}
	return map[string]any{"error": "unknown_skill", "message": fmt.Sprintf("没有名为 %q 的 skill", name), "available": available}, nil
}
