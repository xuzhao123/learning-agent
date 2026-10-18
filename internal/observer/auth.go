package observer

import (
	"bufio"
	"context"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"learning-agent/internal/audit"
	"learning-agent/internal/tenant"
)

// 登录与多租户。账号放在 .data/users.json（平台数据，不按租户分），观测台启动时读入：
//
//	{"users": [{"name": "alice", "tenant": "acme", "salt": "…", "hash": "…", "iterations": 600000}],
//	 "tenants": {"acme": {"max_running": 2, "features": ["memory", "rag", "bash"]}}}
//
// tenant 为空的账号属于平台：看到的是 .data/ 下原有的数据，可以用评测、归因和 Skills/MCP 配置。
// 租户账号只看得到自己租户的对话、记忆与审计，功能与并发按 tenants 里的策略限制。
// 账号用 go run . observe -add-user 名字 [-tenant 租户] 添加，密码从标准输入读，不出现在命令行和 shell 历史里。
const usersPath = ".data/users.json"

type account struct {
	Name       string `json:"name"`
	Tenant     string `json:"tenant,omitempty"`
	Salt       string `json:"salt"`
	Hash       string `json:"hash"`
	Iterations int    `json:"iterations"`
}

// 租户策略：同时运行的对话数与可用的功能。没有写的租户用默认值（最多 2 个、全部功能）。
type policy struct {
	MaxRunning int      `json:"max_running,omitempty"`
	Features   []string `json:"features,omitempty"`
}

// 功能名与发起对话时的开关对应；labs 指 Day 3/4/18 的实验入口。
var allFeatures = []string{"rag", "memory", "skills", "mcp", "subagents", "browser", "bash", "labs", "workspace"}

func (p policy) allows(feature string) bool {
	return len(p.Features) == 0 || slices.Contains(p.Features, feature)
}

// allowsTool 和 allows 一样，只是工作区（本机仓库目录）必须明确开通：没列功能清单（全部放行）的租户也不默认给。
func (p policy) allowsTool(feature string) bool {
	if feature == "workspace" {
		return slices.Contains(p.Features, feature)
	}
	return p.allows(feature)
}

func (p policy) maxRunning() int {
	if p.MaxRunning > 0 {
		return p.MaxRunning
	}
	return 2
}

type usersFile struct {
	Users   []account         `json:"users"`
	Tenants map[string]policy `json:"tenants,omitempty"`
}

// 登录后的用户：只有名字和租户，不带密码相关字段。
type user struct {
	Name   string `json:"name"`
	Tenant string `json:"tenant"`
}

func (u *user) platform() bool { return u.Tenant == "" }

// OWASP 密码存储建议：PBKDF2-HMAC-SHA256 至少 600000 次迭代，每个账号独立的随机盐。
const iterations = 600_000

func hashPassword(password string, salt []byte, iter int) string {
	key, _ := pbkdf2.Key(sha256.New, password, salt, iter, 32)
	return hex.EncodeToString(key)
}

var namePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,31}$`)

// addUser 是 go run . observe -add-user 的实现：新增或重设一个账号的密码，可同时设置租户策略。
func addUser(name, space, features string, maxRunning int) error {
	if !namePattern.MatchString(name) {
		return errors.New("用户名只能是小写字母、数字和 ._-，最长32")
	}
	if space != "" && !tenant.Valid(space) {
		return errors.New("tenant 只能是小写字母、数字和连字符，最长32")
	}
	var file usersFile
	if data, err := os.ReadFile(usersPath); err == nil {
		if err := json.Unmarshal(data, &file); err != nil {
			return fmt.Errorf("%s 不是有效 JSON：%w", usersPath, err)
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	password, err := readPassword()
	if err != nil {
		return err
	}
	if len(password) < 8 {
		return errors.New("密码至少 8 个字符")
	}
	salt := make([]byte, 16)
	rand.Read(salt)
	a := account{Name: name, Tenant: space, Salt: hex.EncodeToString(salt), Hash: hashPassword(password, salt, iterations), Iterations: iterations}
	if i := slices.IndexFunc(file.Users, func(u account) bool { return u.Name == name }); i >= 0 {
		file.Users[i] = a
	} else {
		file.Users = append(file.Users, a)
	}
	if space != "" && (features != "" || maxRunning > 0) {
		if file.Tenants == nil {
			file.Tenants = map[string]policy{}
		}
		p := file.Tenants[space]
		if maxRunning > 0 {
			p.MaxRunning = maxRunning
		}
		if features != "" {
			p.Features = nil
			for _, f := range strings.Split(features, ",") {
				if f = strings.TrimSpace(f); !slices.Contains(allFeatures, f) {
					return fmt.Errorf("未知功能 %q，可选：%s", f, strings.Join(allFeatures, ","))
				}
				p.Features = append(p.Features, f)
			}
		}
		file.Tenants[space] = p
	}
	data, _ := json.MarshalIndent(file, "", "  ")
	if err := os.MkdirAll(".data", 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(usersPath+".tmp", append(data, '\n'), 0o600); err != nil {
		return err
	}
	if err := os.Rename(usersPath+".tmp", usersPath); err != nil {
		return err
	}
	where := "平台"
	if space != "" {
		where = "租户 " + space
	}
	fmt.Printf("已保存账号 %s（%s）到 %s；观测台重启后生效\n", name, where, usersPath)
	return nil
}

// 从终端读密码时关闭回显；从管道读时（脚本里 printf … | go run . observe -add-user …）直接读一行。
func readPassword() (string, error) {
	info, _ := os.Stdin.Stat()
	terminal := info != nil && info.Mode()&os.ModeCharDevice != 0
	if terminal {
		fmt.Fprint(os.Stderr, "密码（至少 8 位，输入时不显示）：")
		stty := func(arg string) {
			cmd := exec.Command("stty", arg)
			cmd.Stdin = os.Stdin
			cmd.Run()
		}
		stty("-echo")
		defer func() { stty("echo"); fmt.Fprintln(os.Stderr) }()
	}
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && line == "" {
		return "", errors.New("没有读到密码")
	}
	return strings.TrimRight(line, "\r\n"), nil
}

// ---------- 会话 ----------

type session struct {
	user    user
	expires time.Time
}

type auth struct {
	mu       sync.Mutex
	accounts map[string]account
	tenants  map[string]policy
	sessions map[string]session // 键是令牌的 SHA-256：内存里没有可以直接拿去用的令牌
	failures map[string]failure
}

type failure struct {
	count int
	until time.Time
}

const (
	cookieName = "la_session"
	sessionTTL = 12 * time.Hour
	lockAfter  = 5 // 连续失败几次后锁定这个用户名
	lockFor    = time.Minute
)

func loadAuth() (*auth, error) {
	a := &auth{accounts: map[string]account{}, tenants: map[string]policy{}, sessions: map[string]session{}, failures: map[string]failure{}}
	data, err := os.ReadFile(usersPath)
	if os.IsNotExist(err) {
		return nil, errors.New("还没有账号：先运行 go run . observe -add-user 名字 添加平台账号（加 -tenant 租户 添加租户账号），再启动观测台")
	}
	if err != nil {
		return nil, err
	}
	var file usersFile
	if err := json.Unmarshal(data, &file); err != nil {
		return nil, fmt.Errorf("%s 不是有效 JSON：%w", usersPath, err)
	}
	for _, u := range file.Users {
		if !namePattern.MatchString(u.Name) || (u.Tenant != "" && !tenant.Valid(u.Tenant)) {
			return nil, fmt.Errorf("%s 里的账号 %q 无效", usersPath, u.Name)
		}
		a.accounts[u.Name] = u
	}
	for name, p := range file.Tenants {
		a.tenants[name] = p
	}
	if len(a.accounts) == 0 {
		return nil, errors.New(usersPath + " 里没有账号")
	}
	return a, nil
}

func (a *auth) policy(space string) policy {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.tenants[space]
}

func newToken() string {
	buf := make([]byte, 32)
	rand.Read(buf)
	return hex.EncodeToString(buf)
}

func tokenKey(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// 未知用户也算一遍哈希：响应时间不泄露“这个用户名存在”。
var dummySalt = make([]byte, 16)

func (a *auth) login(name, password string) (token string, u user, err error) {
	a.mu.Lock()
	acc, known := a.accounts[name]
	f := a.failures[name]
	a.mu.Unlock()
	if time.Now().Before(f.until) {
		return "", user{}, fmt.Errorf("失败次数过多，请 %d 秒后再试", int(time.Until(f.until).Seconds())+1)
	}
	ok := false
	if known {
		salt, _ := hex.DecodeString(acc.Salt)
		ok = subtle.ConstantTimeCompare([]byte(hashPassword(password, salt, acc.Iterations)), []byte(acc.Hash)) == 1
	} else {
		hashPassword(password, dummySalt, iterations)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if !ok {
		f.count++
		if f.count >= lockAfter {
			f.until, f.count = time.Now().Add(lockFor), 0
		}
		a.failures[name] = f
		return "", user{}, errors.New("用户名或密码错误")
	}
	delete(a.failures, name)
	token = newToken()
	u = user{Name: acc.Name, Tenant: acc.Tenant}
	a.sessions[tokenKey(token)] = session{user: u, expires: time.Now().Add(sessionTTL)}
	return token, u, nil
}

func (a *auth) lookup(req *http.Request) *user {
	cookie, err := req.Cookie(cookieName)
	if err != nil {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	key := tokenKey(cookie.Value)
	s, ok := a.sessions[key]
	if !ok || time.Now().After(s.expires) {
		delete(a.sessions, key)
		return nil
	}
	u := s.user
	return &u
}

func (a *auth) logout(req *http.Request) {
	if cookie, err := req.Cookie(cookieName); err == nil {
		a.mu.Lock()
		delete(a.sessions, tokenKey(cookie.Value))
		a.mu.Unlock()
	}
}

type userKey struct{}

// current 取出这次请求的登录用户；经过 protect 的路由一定有。
func current(req *http.Request) *user {
	u, _ := req.Context().Value(userKey{}).(*user)
	return u
}

// 不需要登录的路径：登录页、课件、agent 进程调用的模型代理（凭进程令牌）和内置 MCP（本机同源）。
func public(path string) bool {
	return path == "/login" || strings.HasPrefix(path, "/slides/") || strings.HasPrefix(path, "/llm/") || path == "/mcp"
}

// protect 包住整个路由表：没登录的页面请求跳到登录页，接口返回 401；
// 会改变状态的请求另外要求同源（Cookie 设了 SameSite=Strict，这里再挡一层跨站请求）。
func (s *server) protect(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if public(req.URL.Path) {
			next.ServeHTTP(w, req)
			return
		}
		u := s.auth.lookup(req)
		if u == nil {
			if req.Method == http.MethodGet && req.URL.Path == "/" {
				http.Redirect(w, req, "/login", http.StatusSeeOther)
				return
			}
			http.Error(w, "请先登录", http.StatusUnauthorized)
			return
		}
		if req.Method != http.MethodGet && req.Method != http.MethodHead && !sameOriginRequest(req) {
			http.Error(w, "只接受本机观测页面发来的请求", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, req.WithContext(context.WithValue(req.Context(), userKey{}, u)))
	})
}

// platform 只允许平台账号：评测、归因、Skills/MCP 配置会读写平台数据，或者跨对话汇总。
func platform(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		if u := current(req); u == nil || !u.platform() {
			http.Error(w, "只有平台账号可以使用这个功能", http.StatusForbidden)
			return
		}
		next(w, req)
	}
}

func sameOriginRequest(req *http.Request) bool {
	host := req.Host
	if h, _, ok := strings.Cut(host, ":"); ok {
		host = h
	}
	origin, err := url.Parse(req.Header.Get("Origin"))
	return (host == "127.0.0.1" || host == "localhost") && (req.Header.Get("Origin") == "" || (err == nil && origin.Host == req.Host))
}

func (s *server) loginPage(w http.ResponseWriter, req *http.Request) {
	if req.Method == http.MethodGet {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte(loginHTML))
		return
	}
	if !sameOriginRequest(req) {
		http.Error(w, "只接受本机观测页面发来的请求", http.StatusForbidden)
		return
	}
	var input struct{ Name, Password string }
	if json.NewDecoder(http.MaxBytesReader(w, req.Body, 4096)).Decode(&input) != nil {
		http.Error(w, "请求必须是JSON", http.StatusBadRequest)
		return
	}
	token, u, err := s.auth.login(input.Name, input.Password)
	// D21：登录成败都写审计。已知账号写进它所属空间的链；不存在的用户名写平台的链，只记合法格式的名字。
	record := audit.Record{Actor: "user", Event: "login", Status: "ok", User: u.Name, Tenant: u.Tenant}
	if err != nil {
		record.Status, record.Detail = "failed", err.Error()
		s.auth.mu.Lock()
		acc, known := s.auth.accounts[input.Name]
		s.auth.mu.Unlock()
		if known {
			record.User, record.Tenant = acc.Name, acc.Tenant
		} else if namePattern.MatchString(input.Name) {
			record.Target = input.Name
		}
	}
	audit.Log(req.Context(), record)
	if err != nil {
		time.Sleep(300 * time.Millisecond)
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: token, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode, MaxAge: int(sessionTTL.Seconds())})
	writeJSON(w, u)
}

func (s *server) logoutUser(w http.ResponseWriter, req *http.Request) {
	u := current(req)
	s.auth.logout(req)
	audit.Log(req.Context(), audit.Record{Actor: "user", Event: "logout", User: u.Name, Tenant: u.Tenant})
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: "", Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode, MaxAge: -1})
	w.WriteHeader(http.StatusNoContent)
}

// 页面启动时取当前用户：显示名字与租户，隐藏不可用的功能。
func (s *server) me(w http.ResponseWriter, req *http.Request) {
	u := current(req)
	features := allFeatures
	if !u.platform() {
		features = slices.DeleteFunc(slices.Clone(allFeatures), func(f string) bool { return !s.auth.policy(u.Tenant).allowsTool(f) })
	}
	writeJSON(w, map[string]any{"name": u.Name, "tenant": u.Tenant, "platform": u.platform(), "features": features})
}

const loginHTML = `<!doctype html>
<html lang="zh-CN"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>登录 · Learning Agent</title>
<style>
:root { --bg: #f9f9f9; --panel: #fff; --text: #0d0d0d; --muted: #6b6b6b; --line: #ebebeb; }
@media (prefers-color-scheme: dark) { :root { --bg: #141414; --panel: #1f1f1f; --text: #ececec; --muted: #a3a3a3; --line: #2d2d2d; } }
body { margin: 0; min-height: 100vh; display: grid; place-items: center; background: var(--bg); color: var(--text); font: 14px/1.6 ui-sans-serif, -apple-system, BlinkMacSystemFont, "Segoe UI", "PingFang SC", "Noto Sans SC", sans-serif; -webkit-font-smoothing: antialiased; }
form { width: 320px; padding: 32px 28px 24px; border: 1px solid var(--line); border-radius: 16px; background: var(--panel); box-shadow: 0 1px 2px rgba(0,0,0,.04), 0 8px 28px rgba(0,0,0,.06); }
.mark { display: grid; place-items: center; width: 32px; height: 32px; margin-bottom: 14px; border-radius: 8px; background: var(--text); color: var(--panel); }
h1 { margin: 0 0 20px; font-size: 18px; font-weight: 600; letter-spacing: -.01em; }
label { display: block; margin: 0 0 14px; color: var(--muted); font-size: 13px; }
input { box-sizing: border-box; width: 100%; margin-top: 6px; padding: 9px 12px; border: 1px solid var(--line); border-radius: 10px; background: var(--panel); color: var(--text); font: inherit; outline: 0; }
input:focus { border-color: var(--muted); }
button { width: 100%; margin-top: 6px; padding: 9px; border: 0; border-radius: 10px; background: var(--text); color: var(--panel); font: inherit; font-weight: 500; cursor: pointer; }
#err { min-height: 18px; margin: 10px 0 0; color: #d03b2f; font-size: 13px; }
</style></head><body>
<form id="f"><div class="mark"><svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="m5 7 5 5-5 5"/><path d="M13 17h6"/></svg></div><h1>登录 Learning Agent</h1>
<label>用户名<input id="name" autocomplete="username" required autofocus></label>
<label>密码<input id="password" type="password" autocomplete="current-password" required></label>
<button>登录</button><p id="err"></p></form>
<script>
document.getElementById("f").onsubmit = async (e) => {
  e.preventDefault();
  const err = document.getElementById("err"); err.textContent = "";
  const res = await fetch("/login", { method: "POST", headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ name: document.getElementById("name").value.trim(), password: document.getElementById("password").value }) });
  if (res.ok) location.href = "/"; else err.textContent = (await res.text()).trim();
};
</script></body></html>`
