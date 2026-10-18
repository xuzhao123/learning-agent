package observer

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"learning-agent/internal/commands"
	"learning-agent/internal/hooks"
	"learning-agent/internal/llm"
)

// 设置：工具常驻，由这里决定；每个对话只选模型和工作区（参考 Claude Code 的 settings.json、Codex 的 config.toml）。
// 一份设置是一张扁平的“键 → 字符串值”表，按 内置默认 → 平台 → 个人 的顺序合并，后面的覆盖前面的；
// 租户策略与本机能不能跑（没装 bubblewrap、没找到 Chrome）只会把工具关掉，不会打开。
// 新对话开始时把合并结果拍成快照写进 start 事件，续聊沿用快照，之后改设置不影响已经开始的对话。
// 文件放 .data/settings.json（不提交）。

// 工具开关：设置键 → 租户策略里的功能名。编码工具要求租户明确开通 workspace（工作区是本机目录，不随“全部功能”默认放行）。
var toolFeatures = map[string]string{"rag": "rag", "memory": "memory", "skills": "skills", "mcp": "mcp", "subagents": "subagents", "bash": "bash", "browser": "browser", "coding": "workspace"}
var toolOrder = []string{"rag", "memory", "skills", "mcp", "subagents", "bash", "browser", "coding"}

// 检索和记忆默认关：检索的系统提示词是为知识库问答写的，会要求每个知识问题先查资料；记忆每次新对话都要多发几次模型请求做召回。
var builtin = map[string]string{
	"rag": "off", "memory": "off", "skills": "on", "mcp": "on", "subagents": "on", "bash": "on", "browser": "on", "coding": "on",
	"embedding": "ark", "memory_ttl": "", "provider": "ark", "workspace": "",
	"max_steps": "20", "context_window": "32768", "tool_output": "4000",
	"permission_mode": "edit", "permission_rules": "",
	"hooks": "",
}

// D26：hooks 在宿主机上以观测台的身份运行命令，只能由平台设置；值是配置原文（以 { 开头）或相对 agent 目录的文件路径，
// 对话开始时读成原文拍进快照，之后改文件不影响已经开始的对话。
var hooksBase string

func hooksConfig(value string) (string, error) {
	text := strings.TrimSpace(value)
	if text == "" {
		return "", nil
	}
	if !strings.HasPrefix(text, "{") {
		path := text
		if !filepath.IsAbs(path) {
			path = filepath.Join(hooksBase, path)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("读取 hooks 配置：%w", err)
		}
		text = string(data)
	}
	_, err := hooks.Parse(text)
	return text, err
}

// 权限规则一行一条：allow: Bash(go build *)、ask: Edit(internal/agent/**)、deny: Bash(git push *)。语法由 agent 的 permission 包校验。
var ruleLine = regexp.MustCompile(`^(allow|ask|deny):\s*([A-Za-z_][A-Za-z0-9_]*(\(.*\))?)$`)

func ruleLines(text string) []string {
	var out []string
	for _, line := range strings.Split(text, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			out = append(out, line)
		}
	}
	return out
}

type workspaceDir struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

type settingsFile struct {
	Platform   map[string]string            `json:"platform,omitempty"`
	Users      map[string]map[string]string `json:"users,omitempty"`
	Workspaces []workspaceDir               `json:"workspaces,omitempty"`
}

func (s *server) settingsPath() string { return filepath.Join(s.agentDir, ".data", "settings.json") }

// 调用方持有 s.hubMu（与 hub.json 共用一把锁，设置和中心配置都很少写）。
func (s *server) readSettings() (settingsFile, error) {
	var f settingsFile
	data, err := os.ReadFile(s.settingsPath())
	if errors.Is(err, os.ErrNotExist) {
		return f, nil
	}
	if err == nil {
		err = json.Unmarshal(data, &f)
	}
	return f, err
}

func (s *server) writeSettings(f settingsFile) error {
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.settingsPath()), 0o700); err != nil {
		return err
	}
	tmp := s.settingsPath() + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.settingsPath())
}

// valid 检查一个设置值；未知的键直接拒绝，免得拼错的键悄悄不生效。
func valid(key, value string, f settingsFile) error {
	if _, ok := toolFeatures[key]; ok {
		if value != "on" && value != "off" {
			return fmt.Errorf("%s 只能是 on 或 off", key)
		}
		return nil
	}
	switch key {
	case "embedding":
		if value != "ark" && value != "local" {
			return errors.New("embedding 只能是 ark 或 local")
		}
	case "memory_ttl":
		if d, err := time.ParseDuration(value); value != "" && (err != nil || d <= 0) {
			return errors.New("memory_ttl 是空（永久）或正的时长，如 1h")
		}
	case "provider":
		if !providerPattern.MatchString(value) {
			return errors.New("provider 只能是小写字母")
		}
	case "workspace":
		if value != "" && !slices.ContainsFunc(f.Workspaces, func(w workspaceDir) bool { return w.Name == value }) {
			return fmt.Errorf("没有登记名为 %s 的工作区", value)
		}
	case "permission_mode":
		if !slices.Contains([]string{"plan", "ask", "edit", "full"}, value) {
			return errors.New("permission_mode 取 plan、ask、edit 或 full")
		}
	case "permission_rules":
		for _, line := range ruleLines(value) {
			if !ruleLine.MatchString(line) {
				return fmt.Errorf("权限规则一行一条，格式如 allow: Bash(go build *)：%q", line)
			}
		}
	case "hooks":
		if _, err := hooksConfig(value); err != nil {
			return err
		}
	case "max_steps", "context_window", "tool_output":
		limits := map[string][2]int{"max_steps": {1, 60}, "context_window": {4096, 1048576}, "tool_output": {64, 32768}}[key]
		if n, err := strconv.Atoi(value); err != nil || n < limits[0] || n > limits[1] {
			return fmt.Errorf("%s 范围为 %d 到 %d", key, limits[0], limits[1])
		}
	default:
		return fmt.Errorf("未知设置项 %s", key)
	}
	return nil
}

// unavailable 返回本机跑不了这个工具的原因；空串表示可用。只做便宜的检查（找可执行文件、看密钥在不在），不真的启动。
func (s *server) unavailable(key string, skills []string, servers []mcpServer) string {
	switch key {
	case "bash":
		if runtime.GOOS != "linux" {
			return "Bash 沙箱只支持 Linux"
		}
		if _, err := exec.LookPath("bwrap"); err != nil {
			return "没有找到 bubblewrap（bwrap），请先安装"
		}
	case "browser":
		if os.Getenv("CHROME_PATH") != "" {
			return ""
		}
		for _, name := range []string{"google-chrome", "google-chrome-stable", "chromium", "chromium-browser", "chrome"} {
			if _, err := exec.LookPath(name); err == nil {
				return ""
			}
		}
		return "没有找到 Chrome：在启动观测台的环境里设置 CHROME_PATH"
	case "skills":
		if len(skills) == 0 {
			return "Skills 中心里没有启用的 skill"
		}
	case "mcp":
		if len(servers) == 0 {
			return "MCP 中心里没有启用的 server"
		}
	}
	return ""
}

// effective 是某个账号此刻的设置：合并后的值、每个值来自哪一层、被关掉的工具及原因。
type effective struct {
	Values     map[string]string `json:"values"`
	Source     map[string]string `json:"source"`     // builtin、platform、user
	Blocked    map[string]string `json:"blocked"`    // 工具 → 被关掉的原因（租户未开通、本机不可用）
	Workspaces []workspaceDir    `json:"workspaces"` // 这个账号能选的工作区
	Skills     []string          `json:"skills"`
	MCP        []mcpServer       `json:"mcp"`
}

func (s *server) effectiveFor(u *user) (effective, settingsFile, error) {
	s.hubMu.Lock()
	f, err := s.readSettings()
	s.hubMu.Unlock()
	if err != nil {
		return effective{}, f, err
	}
	skills, servers, err := s.enabledCapabilities()
	if err != nil {
		return effective{}, f, err
	}
	e := effective{Values: maps.Clone(builtin), Source: map[string]string{}, Blocked: map[string]string{}, Skills: skills, MCP: servers}
	for key := range builtin {
		e.Source[key] = "builtin"
	}
	for _, layer := range []struct {
		name   string
		values map[string]string
	}{{"platform", f.Platform}, {"user", f.Users[u.Name]}} {
		for key, value := range layer.values {
			if layer.name == "user" && key == "hooks" {
				continue
			}
			if _, ok := builtin[key]; ok && valid(key, value, f) == nil {
				e.Values[key], e.Source[key] = value, layer.name
			}
		}
	}
	for _, key := range toolOrder {
		if !u.platform() && !s.auth.policy(u.Tenant).allowsTool(toolFeatures[key]) {
			e.Blocked[key] = "租户 " + u.Tenant + " 没有开通"
		} else if reason := s.unavailable(key, skills, servers); reason != "" {
			e.Blocked[key] = reason
		}
	}
	if e.Values["embedding"] == "ark" {
		if _, err := llm.LoadConfig("ark"); err != nil {
			for _, key := range []string{"rag", "memory"} {
				if _, blocked := e.Blocked[key]; !blocked {
					e.Blocked[key] = "线上向量需要 ARK_API_KEY；可改用本地向量模型"
				}
			}
		}
	}
	if _, blocked := e.Blocked["coding"]; !blocked {
		e.Workspaces = f.Workspaces
	}
	if e.Workspaces == nil {
		e.Workspaces = []workspaceDir{}
	}
	return e, f, nil
}

// on 表示这个工具在新对话里会带上：设置打开，而且没有被租户或本机条件关掉。
func (e effective) on(key string) bool {
	_, blocked := e.Blocked[key]
	return e.Values[key] == "on" && !blocked
}

func (s *server) getSettings(w http.ResponseWriter, req *http.Request) {
	u := current(req)
	e, f, err := s.effectiveFor(u)
	if err != nil {
		http.Error(w, "无法读取设置："+err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"effective": e, "platform": f.Platform, "user": f.Users[u.Name], "builtin": builtin, "can_edit_platform": u.platform(), "tool_order": toolOrder})
}

// putSettings 保存一层设置：body 是这一层想要的完整取值，只存和下一层不同的键，
// 这样平台默认改了，没有单独设过的人跟着变。传 {} 表示这一层恢复为继承。
func (s *server) putSettings(layer string) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		u := current(req)
		var want map[string]string
		if err := json.NewDecoder(http.MaxBytesReader(w, req.Body, 1<<16)).Decode(&want); err != nil {
			http.Error(w, "请求必须是 JSON 对象", http.StatusBadRequest)
			return
		}
		s.hubMu.Lock()
		defer s.hubMu.Unlock()
		f, err := s.readSettings()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		base := maps.Clone(builtin)
		if layer == "user" {
			maps.Copy(base, f.Platform)
		}
		diff := map[string]string{}
		for key, value := range want {
			if err := valid(key, value, f); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			if layer == "user" && key == "hooks" && value != base[key] {
				http.Error(w, "hooks 会在本机上运行命令，只能在平台设置里配置", http.StatusForbidden)
				return
			}
			if base[key] != value {
				diff[key] = value
			}
		}
		if layer == "platform" {
			f.Platform = diff
		} else {
			if f.Users == nil {
				f.Users = map[string]map[string]string{}
			}
			f.Users[u.Name] = diff
			if len(diff) == 0 {
				delete(f.Users, u.Name)
			}
		}
		if err := s.writeSettings(f); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Write([]byte("ok"))
	}
}

// 工作区由平台账号登记：本机上一个已存在的 git 仓库目录。对话只能从这个列表里选，网页不能直接给路径。
func (s *server) addWorkspace(w http.ResponseWriter, req *http.Request) {
	var input struct {
		Path string `json:"path"`
	}
	if json.NewDecoder(req.Body).Decode(&input) != nil || strings.TrimSpace(input.Path) == "" {
		http.Error(w, "请填写目录路径", http.StatusBadRequest)
		return
	}
	path := strings.TrimSpace(input.Path)
	if !filepath.IsAbs(path) {
		path = filepath.Join(s.agentDir, path) // 相对路径按 agent 目录解析，和 agent 的工作目录一致
	}
	path = filepath.Clean(path)
	if info, err := os.Stat(path); err != nil || !info.IsDir() {
		http.Error(w, "目录不存在："+path, http.StatusBadRequest)
		return
	}
	if _, err := os.Stat(filepath.Join(path, ".git")); err != nil {
		http.Error(w, "不是 git 仓库（没有 .git）："+path+"；改坏了要能用 git 回退", http.StatusBadRequest)
		return
	}
	name := filepath.Base(path)
	if !serverName.MatchString(strings.ToLower(name)) {
		name = "workspace"
	}
	s.hubMu.Lock()
	defer s.hubMu.Unlock()
	f, err := s.readSettings()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	for _, existing := range f.Workspaces {
		if existing.Path == path {
			http.Error(w, "这个目录已经登记过："+existing.Name, http.StatusConflict)
			return
		}
	}
	base := strings.ToLower(name)
	for i := 2; slices.ContainsFunc(f.Workspaces, func(w workspaceDir) bool { return w.Name == name }); i++ {
		name = fmt.Sprintf("%s-%d", base, i)
	}
	f.Workspaces = append(f.Workspaces, workspaceDir{Name: name, Path: path})
	if err := s.writeSettings(f); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, workspaceDir{Name: name, Path: path})
}

func (s *server) deleteWorkspace(w http.ResponseWriter, req *http.Request) {
	name := req.PathValue("name")
	s.hubMu.Lock()
	defer s.hubMu.Unlock()
	f, err := s.readSettings()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	n := len(f.Workspaces)
	f.Workspaces = slices.DeleteFunc(f.Workspaces, func(w workspaceDir) bool { return w.Name == name })
	if len(f.Workspaces) == n {
		http.Error(w, "找不到这个工作区", http.StatusNotFound)
		return
	}
	// 设成默认工作区的层一并清掉，免得指向不存在的名字。
	if f.Platform["workspace"] == name {
		delete(f.Platform, "workspace")
	}
	for _, values := range f.Users {
		if values["workspace"] == name {
			delete(values, "workspace")
		}
	}
	if err := s.writeSettings(f); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Write([]byte("ok"))
}

// D26：斜杠命令列表，输入框打 / 时显示。个人级来自 agent 目录的 commands/，项目级来自所选工作区的 .agents/commands/；
// 展开由 agent 做，这里只列名字和说明。
func (s *server) listCommands(w http.ResponseWriter, req *http.Request) {
	e, _, err := s.effectiveFor(current(req))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	dir := ""
	if name := req.URL.Query().Get("workspace"); name != "" && e.on("coding") {
		if i := slices.IndexFunc(e.Workspaces, func(w workspaceDir) bool { return w.Name == name }); i >= 0 {
			dir = e.Workspaces[i].Path
		}
	}
	writeJSON(w, commands.List(s.agentDir, dir))
}
