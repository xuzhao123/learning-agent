package observer

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unicode"
	"unicode/utf8"

	"learning-agent/internal/audit"
	"learning-agent/internal/tenant"
)

// 对话的沙箱文件：agent 在 turn/started 里报告沙箱目录名（对话第一轮的检查点ID），
// 文件在 .data/[tenants/<租户>/]sandbox/<目录名>/，就是沙箱里的 /workspace。
//
// 这个目录里的东西是模型写的，所以观测台把它当不可信数据：
//   - 只经 os.Root 访问：模型在沙箱里建的符号链接若指向 /workspace 之外（比如宿主机的 .env），解析到根外就报错，不会跟出去；
//   - 只下载普通文件，打开时带 O_NONBLOCK：命名管道不会卡住观测台；
//   - 一律作为附件下载、nosniff：模型写的 HTML 不会在观测台的页面源里执行（那里有登录 Cookie）。
const (
	maxUpload    = 10 << 20
	maxListFiles = 1000
)

// 最近一次 turn/started 报告的沙箱目录名；没开 bash 或 B0 之前的存档没有。调用方持有 r.mu。
func (r *run) sandboxID() string {
	for i := len(r.events) - 1; i >= 0; i-- {
		e := r.events[i]
		if e.Kind == "notify" && e.Method == "turn/started" {
			var p struct{ Sandbox string }
			if json.Unmarshal(e.Body, &p) == nil && p.Sandbox != "" && frameName.MatchString(p.Sandbox) && !strings.HasPrefix(p.Sandbox, ".") {
				return p.Sandbox
			}
		}
	}
	return ""
}

// sandboxOf 打开对话的沙箱目录；目录还没建（bash 还没调用过）时由 create 决定是否新建。
func (s *server) sandboxOf(w http.ResponseWriter, req *http.Request, create bool) (*run, *os.Root) {
	r := s.runOf(w, req)
	if r == nil {
		return nil, nil
	}
	r.mu.Lock()
	id := r.sandboxID()
	r.mu.Unlock()
	if id == "" {
		http.Error(w, "这条对话没有启用 Bash 沙箱", http.StatusNotFound)
		return nil, nil
	}
	dir := filepath.Join(s.agentDir, tenant.Path(r.Tenant, "sandbox", id))
	if create {
		os.MkdirAll(dir, 0o755)
	}
	root, err := os.OpenRoot(dir)
	if errors.Is(err, os.ErrNotExist) {
		return r, nil // 还没有任何文件
	}
	if err != nil {
		http.Error(w, "无法打开沙箱目录", http.StatusInternalServerError)
		return nil, nil
	}
	return r, root
}

type sandboxFile struct {
	Path  string    `json:"path"`
	Size  int64     `json:"size"`
	Time  time.Time `json:"time"`
	Kind  string    `json:"kind"` // file、dir、link、other；只有 file 能下载
	Link  string    `json:"link,omitempty"`
	Error string    `json:"error,omitempty"`
}

func (s *server) listFiles(w http.ResponseWriter, req *http.Request) {
	r, root := s.sandboxOf(w, req, false)
	if r == nil {
		return
	}
	files, truncated := []sandboxFile{}, false
	if root != nil {
		defer root.Close()
		// WalkDir 不跟符号链接进目录；Lstat 看到的是链接本身。
		fs.WalkDir(root.FS(), ".", func(p string, d fs.DirEntry, err error) error {
			if p == "." {
				return err
			}
			if len(files) >= maxListFiles {
				truncated = true
				return fs.SkipAll
			}
			f := sandboxFile{Path: p, Kind: "other"}
			if err != nil {
				f.Error = err.Error()
			} else if info, err := root.Lstat(p); err == nil {
				f.Size, f.Time = info.Size(), info.ModTime()
				switch mode := info.Mode(); {
				case mode.IsRegular():
					f.Kind = "file"
				case mode.IsDir():
					f.Kind, f.Size = "dir", 0
				case mode&fs.ModeSymlink != 0:
					f.Kind, f.Link = "link", readlink(root, p)
				}
			}
			files = append(files, f)
			return nil
		})
	}
	r.mu.Lock()
	running := r.proc != nil
	r.mu.Unlock()
	json.NewEncoder(w).Encode(map[string]any{"files": files, "truncated": truncated, "running": running})
}

func readlink(root *os.Root, p string) string {
	target, _ := root.Readlink(p)
	return target
}

func (s *server) downloadFile(w http.ResponseWriter, req *http.Request) {
	r, root := s.sandboxOf(w, req, false)
	if r == nil {
		return
	}
	if root == nil {
		http.NotFound(w, req)
		return
	}
	defer root.Close()
	name := req.PathValue("path")
	if name == "" || !fs.ValidPath(name) {
		http.NotFound(w, req)
		return
	}
	file, err := root.OpenFile(name, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		http.Error(w, "打不开这个文件（不存在，或是指向沙箱外的链接）", http.StatusNotFound)
		return
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		http.Error(w, "只能下载普通文件", http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Disposition", "attachment; filename*=UTF-8''"+urlPathEscape(path.Base(name)))
	w.Header().Set("Cache-Control", "no-store")
	http.ServeContent(w, req, "", info.ModTime(), file)
}

// RFC 5987 的 filename*：中文文件名下载后保持原名；字母数字和 -._~ 以外的字节都转成 %XX。
func urlPathEscape(name string) string {
	var b strings.Builder
	for _, c := range []byte(name) {
		if c >= '0' && c <= '9' || c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || strings.IndexByte("-._~", c) >= 0 {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

// 上传：页面把文件内容作为请求体，文件名放在 ?name=。只放到 /workspace 顶层、不覆盖已有文件、
// 对话运行中不接受（同一条对话一次只处理一个提问，上传也算一次改动）。人的这次操作记进审计，内容只存哈希。
func (s *server) uploadFile(w http.ResponseWriter, req *http.Request) {
	r, root := s.sandboxOf(w, req, true)
	if r == nil {
		return
	}
	if root == nil {
		http.Error(w, "无法创建沙箱目录", http.StatusInternalServerError)
		return
	}
	defer root.Close()
	r.mu.Lock()
	running, task := r.proc != nil, r.sandboxID()
	r.mu.Unlock()
	if running {
		http.Error(w, "对话正在运行，等这一轮结束再上传", http.StatusConflict)
		return
	}
	name := req.URL.Query().Get("name")
	if !validUploadName(name) {
		http.Error(w, "文件名只能是一层、不以点开头、不含控制字符，最长 128 字节", http.StatusBadRequest)
		return
	}
	data, err := io.ReadAll(http.MaxBytesReader(w, req.Body, maxUpload))
	if err != nil {
		http.Error(w, "文件不能超过 10 MB", http.StatusRequestEntityTooLarge)
		return
	}
	file, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if errors.Is(err, fs.ErrExist) {
		http.Error(w, "沙箱里已有同名文件："+name, http.StatusConflict)
		return
	}
	if err != nil {
		http.Error(w, "无法写入沙箱目录", http.StatusInternalServerError)
		return
	}
	record := audit.Record{Actor: "user", User: current(req).Name, Tenant: r.Tenant, Event: "user_upload", Task: task, Conversation: r.ID, Target: name, Detail: "观测台页面"}
	record.Args(string(data))
	// 和审批一样，写不进审计就不做：先记再写文件，失败则删掉空文件。
	if err := audit.Write(req.Context(), record); err != nil {
		file.Close()
		root.Remove(name)
		http.Error(w, "审计日志写入失败，没有上传："+err.Error(), http.StatusInternalServerError)
		return
	}
	_, err = file.Write(data)
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		root.Remove(name)
		http.Error(w, "写入失败："+err.Error(), http.StatusInternalServerError)
		return
	}
	json.NewEncoder(w).Encode(map[string]any{"path": name, "size": len(data)})
}

func validUploadName(name string) bool {
	if name == "" || len(name) > 128 || !utf8.ValidString(name) || strings.HasPrefix(name, ".") || strings.ContainsAny(name, `/\`) {
		return false
	}
	for _, c := range name {
		if unicode.IsControl(c) {
			return false
		}
	}
	return true
}
