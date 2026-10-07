package observer

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
)

// Skills 中心与 MCP 中心：观测台只保存“装了哪些、开没开”，解析、校验与连接都交给 agent 自己的命令行模式，
// 不在观测台另写一份 SKILL.md 解析或 MCP client。配置放 agent 的 .data/hub.json（已忽略提交）。
type mcpServer struct {
	Name    string `json:"name"`
	Command string `json:"command"`
	Enabled bool   `json:"enabled"`
}

type hubConfig struct {
	MCPServers     []mcpServer `json:"mcp_servers"`
	DisabledSkills []string    `json:"disabled_skills"`
}

var skillName = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)
var serverName = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)

func (s *server) hubPath() string { return filepath.Join(s.agentDir, ".data", "hub.json") }

// 调用方持有 s.hubMu。文件不存在时是空配置：新 skill 默认启用，MCP server 需要手动添加。
func (s *server) readHub() (hubConfig, error) {
	config := hubConfig{MCPServers: []mcpServer{}, DisabledSkills: []string{}}
	data, err := os.ReadFile(s.hubPath())
	if errors.Is(err, os.ErrNotExist) {
		return config, nil
	}
	if err == nil {
		err = json.Unmarshal(data, &config)
	}
	return config, err
}

// 先写临时文件再改名，写到一半退出也不会留下半个 JSON。
func (s *server) writeHub(config hubConfig) error {
	data, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.hubPath()), 0o700); err != nil {
		return err
	}
	tmp := s.hubPath() + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.hubPath())
}

func (s *server) updateHub(w http.ResponseWriter, change func(*hubConfig) error) {
	s.hubMu.Lock()
	defer s.hubMu.Unlock()
	config, err := s.readHub()
	if err == nil {
		err = change(&config)
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := s.writeHub(config); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Write([]byte("ok"))
}

// 新对话开始时取一次快照，写进 start 事件；续聊沿用快照，中心里之后的改动只影响新对话。
// skill 只按目录名列出，坏文件由 agent 启动时报错跳过。
func (s *server) enabledCapabilities() ([]string, []mcpServer, error) {
	s.hubMu.Lock()
	config, err := s.readHub()
	s.hubMu.Unlock()
	if err != nil {
		return nil, nil, err
	}
	paths, _ := filepath.Glob(filepath.Join(s.agentDir, "skills", "*", "SKILL.md"))
	skills := []string{}
	for _, path := range paths {
		name := filepath.Base(filepath.Dir(path))
		if !slices.Contains(config.DisabledSkills, name) {
			skills = append(skills, name)
		}
	}
	servers := []mcpServer{}
	for _, m := range config.MCPServers {
		if m.Enabled {
			servers = append(servers, m)
		}
	}
	return skills, servers, nil
}

func appendCapabilityArgs(args, skills []string, servers []mcpServer) []string {
	if len(skills) > 0 {
		args = append(args, "-skills")
	}
	for _, name := range skills {
		args = append(args, "-skill", name)
	}
	for _, m := range servers {
		args = append(args, "-mcp-server", m.Command)
	}
	return args
}

// 输入框显示新对话会带上的数量（skill 按目录名计数），MCP 中心列出全部 server；都不启动 agent。
func (s *server) enabledHub(w http.ResponseWriter, _ *http.Request) {
	skills, servers, err := s.enabledCapabilities()
	s.hubMu.Lock()
	config, readErr := s.readHub()
	s.hubMu.Unlock()
	if err = errors.Join(err, readErr); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"skills": skills, "mcp": servers, "mcp_servers": config.MCPServers, "hosted": "与观测台同一进程，随观测台启动和退出"})
}

// 列表直接来自 agent 的 -skills-list：和运行时用的是同一套 frontmatter 解析与校验。
func (s *server) listHub(w http.ResponseWriter, req *http.Request) {
	s.hubMu.Lock()
	config, err := s.readHub()
	s.hubMu.Unlock()
	if err != nil {
		http.Error(w, "无法读取 .data/hub.json："+err.Error(), http.StatusInternalServerError)
		return
	}
	ctx, cancel := context.WithTimeout(req.Context(), 2*time.Minute)
	defer cancel()
	cmd := s.agentCommand(ctx, "-skills-list")
	output, err := cmd.Output()
	var listed struct {
		Skills []map[string]any `json:"skills"`
		Errors []map[string]any `json:"errors"`
	}
	if err == nil {
		err = json.Unmarshal(output, &listed)
	}
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			err = errors.New(strings.TrimSpace(string(exit.Stderr)))
		}
		http.Error(w, "agent -skills-list 失败："+err.Error(), http.StatusInternalServerError)
		return
	}
	for _, skill := range listed.Skills {
		skill["enabled"] = !slices.Contains(config.DisabledSkills, skill["name"].(string))
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"path": ".data/hub.json", "skills": listed.Skills, "skill_errors": listed.Errors, "mcp_servers": config.MCPServers})
}

// 新建知识型 skill：只写一个 SKILL.md。写完立刻用 agent 的校验检查，不合规就删掉并返回原因。
func (s *server) createSkill(w http.ResponseWriter, req *http.Request) {
	var input struct{ Name, Description, Body string }
	if json.NewDecoder(req.Body).Decode(&input) != nil {
		http.Error(w, "请求必须是JSON", http.StatusBadRequest)
		return
	}
	input.Name, input.Description, input.Body = strings.TrimSpace(input.Name), strings.TrimSpace(input.Description), strings.TrimSpace(input.Body)
	switch {
	case len(input.Name) > 64 || !skillName.MatchString(input.Name):
		http.Error(w, "name 只能用小写字母、数字和单个连字符，最长64", http.StatusBadRequest)
		return
	case input.Description == "" || strings.ContainsAny(input.Description, "\r\n"):
		http.Error(w, "description 必填，且写在一行内（frontmatter 不支持多行值）", http.StatusBadRequest)
		return
	case input.Body == "":
		http.Error(w, "正文不能为空：写清楚什么时候用、按什么步骤做", http.StatusBadRequest)
		return
	}
	dir := filepath.Join(s.agentDir, "skills", input.Name)
	if err := os.Mkdir(dir, 0o755); err != nil {
		http.Error(w, "无法创建 skills/"+input.Name+"（可能已存在）", http.StatusConflict)
		return
	}
	content := "---\nname: " + input.Name + "\ndescription: " + input.Description + "\n---\n\n" + input.Body + "\n"
	err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(content), 0o644)
	if err == nil {
		cmd := s.agentCommand(req.Context(), "-skills-list")
		var output []byte
		var listed struct{ Errors []struct{ Dir, Error string } }
		if output, err = cmd.Output(); err == nil {
			err = json.Unmarshal(output, &listed)
		}
		for _, failure := range listed.Errors {
			if failure.Dir == input.Name {
				err = errors.New(failure.Error)
			}
		}
	}
	if err != nil {
		os.RemoveAll(dir)
		http.Error(w, "skill 未通过校验，已撤销："+err.Error(), http.StatusBadRequest)
		return
	}
	w.Write([]byte("ok"))
}

// 删除整个 skills/<name> 目录；名字先按 spec 校验，不会拼出 skills/ 之外的路径。
func (s *server) deleteSkill(w http.ResponseWriter, req *http.Request) {
	name := req.PathValue("name")
	dir := filepath.Join(s.agentDir, "skills", name)
	if !skillName.MatchString(name) {
		http.Error(w, "skill 名字不合规", http.StatusBadRequest)
		return
	}
	if _, err := os.Stat(filepath.Join(dir, "SKILL.md")); err != nil {
		http.Error(w, "找不到 skills/"+name+"/SKILL.md", http.StatusNotFound)
		return
	}
	if err := os.RemoveAll(dir); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.updateHub(w, func(config *hubConfig) error {
		config.DisabledSkills = slices.DeleteFunc(config.DisabledSkills, func(n string) bool { return n == name })
		return nil
	})
}

func (s *server) toggleSkill(w http.ResponseWriter, req *http.Request) {
	name := req.PathValue("name")
	var input struct{ Enabled bool }
	if !skillName.MatchString(name) || json.NewDecoder(req.Body).Decode(&input) != nil {
		http.Error(w, "需要合规的 skill 名字与 {\"enabled\": true|false}", http.StatusBadRequest)
		return
	}
	s.updateHub(w, func(config *hubConfig) error {
		config.DisabledSkills = slices.DeleteFunc(config.DisabledSkills, func(n string) bool { return n == name })
		if !input.Enabled {
			config.DisabledSkills = append(config.DisabledSkills, name)
		}
		return nil
	})
}

func (s *server) addMCP(w http.ResponseWriter, req *http.Request) {
	var input struct{ Name, Command string }
	if json.NewDecoder(req.Body).Decode(&input) != nil {
		http.Error(w, "请求必须是JSON", http.StatusBadRequest)
		return
	}
	input.Name, input.Command = strings.TrimSpace(input.Name), strings.TrimSpace(input.Command)
	if !serverName.MatchString(input.Name) {
		http.Error(w, "name 用小写字母、数字、_ 或 -，最长32", http.StatusBadRequest)
		return
	}
	// 与 agent 一致：http(s) 地址是远程 server；其他当作 stdio 命令，按空格切分直接执行，不经过 shell。
	if input.Command == "" || len(input.Command) > 1024 || strings.ContainsAny(input.Command, "\r\n") {
		http.Error(w, "command 是一行 stdio 启动命令或 http(s) 地址，最长1024", http.StatusBadRequest)
		return
	}
	s.updateHub(w, func(config *hubConfig) error {
		for _, m := range config.MCPServers {
			if m.Name == input.Name {
				return errors.New("已有同名 server：" + input.Name)
			}
		}
		config.MCPServers = append(config.MCPServers, mcpServer{Name: input.Name, Command: input.Command, Enabled: true})
		return nil
	})
}

func (s *server) changeMCP(w http.ResponseWriter, name string, change func(config *hubConfig, i int)) {
	s.updateHub(w, func(config *hubConfig) error {
		i := slices.IndexFunc(config.MCPServers, func(m mcpServer) bool { return m.Name == name })
		if i < 0 {
			return errors.New("找不到 server：" + name)
		}
		change(config, i)
		return nil
	})
}

func (s *server) deleteMCP(w http.ResponseWriter, req *http.Request) {
	s.changeMCP(w, req.PathValue("name"), func(config *hubConfig, i int) {
		config.MCPServers = slices.Delete(config.MCPServers, i, i+1)
	})
}

func (s *server) toggleMCP(w http.ResponseWriter, req *http.Request) {
	var input struct{ Enabled bool }
	if json.NewDecoder(req.Body).Decode(&input) != nil {
		http.Error(w, "需要 {\"enabled\": true|false}", http.StatusBadRequest)
		return
	}
	s.changeMCP(w, req.PathValue("name"), func(config *hubConfig, i int) { config.MCPServers[i].Enabled = input.Enabled })
}

// 测试连接复用 agent 的 -mcp-list：SDK 连接、协商、列出工具，不请求模型。
func (s *server) testMCP(w http.ResponseWriter, req *http.Request) {
	s.hubMu.Lock()
	config, err := s.readHub()
	s.hubMu.Unlock()
	i := slices.IndexFunc(config.MCPServers, func(m mcpServer) bool { return m.Name == req.PathValue("name") })
	if err != nil || i < 0 {
		http.Error(w, "找不到这个 server", http.StatusNotFound)
		return
	}
	ctx, cancel := context.WithTimeout(req.Context(), 2*time.Minute)
	defer cancel()
	cmd := s.agentCommand(ctx, "-mcp-server", config.MCPServers[i].Command, "-mcp-list")
	output, err := cmd.CombinedOutput()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"ok": err == nil, "output": string(output)})
}

// 中心能写配置、能让 agent 启动任意命令，所以拒绝其他网站发来的跨站请求（浏览器会带上 Origin），
// 也拒绝非本机 Host，防止 DNS 重绑定把外部域名指到 127.0.0.1。
func sameOrigin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		host := req.Host
		if h, _, ok := strings.Cut(host, ":"); ok {
			host = h
		}
		origin, err := url.Parse(req.Header.Get("Origin"))
		if (host != "127.0.0.1" && host != "localhost") || (req.Header.Get("Origin") != "" && (err != nil || origin.Host != req.Host)) {
			http.Error(w, "只接受本机观测页面发来的请求", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, req)
	})
}
