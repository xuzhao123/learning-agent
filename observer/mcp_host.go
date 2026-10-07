package main

import (
	"bufio"
	"fmt"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
)

// 观测台内置的远程 MCP server：工具实现仍在 agent（-mcp-serve 复用 calculate），
// 观测台把它以 Streamable HTTP 常驻运行，再把自己的 /mcp 转发过去，所以观测台本身不引入 MCP SDK。
// 先编译到临时目录再运行：直接 go run 的话，结束 go 命令不会结束它编译出的子进程。
type mcpHost struct {
	addr string
	mu   sync.Mutex
	cmd  *exec.Cmd
	dir  string
	note string // 页面显示的状态：编译中、运行中或失败原因
}

func (h *mcpHost) status() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.note
}

func (h *mcpHost) set(note string) {
	h.mu.Lock()
	h.note = note
	h.mu.Unlock()
	fmt.Println("内置 MCP server：" + note)
}

// 后台编译并启动，观测页面不用等它；编译完成前 /mcp 返回 503。
func (h *mcpHost) start(agentDir string) {
	h.set("编译中")
	dir, err := os.MkdirTemp("", "observer-mcp-")
	if err != nil {
		h.set("无法创建临时构建目录，请检查目录是否可写")
		return
	}
	binary := filepath.Join(dir, "learning-agent")
	build := exec.Command("go", "build", "-o", binary, ".")
	build.Dir = agentDir
	if output, err := build.CombinedOutput(); err != nil {
		os.RemoveAll(dir)
		// 页面保留编译诊断，项目与临时目录用通用名称展示。
		detail := strings.ReplaceAll(string(output), agentDir, ".")
		detail = strings.ReplaceAll(detail, dir, "[临时构建目录]")
		h.set("编译失败：" + detail)
		return
	}
	cmd := exec.Command(binary, "-mcp-serve", "-mcp-http", h.addr)
	cmd.Dir = agentDir
	stderr, _ := cmd.StderrPipe()
	if err := cmd.Start(); err != nil {
		os.RemoveAll(dir)
		h.set("启动失败：" + strings.ReplaceAll(err.Error(), binary, "learning-agent"))
		return
	}
	h.mu.Lock()
	h.cmd, h.dir = cmd, dir
	h.mu.Unlock()
	h.set("运行中，转发到 http://" + h.addr + "/mcp")
	// server 日志（每次工具调用一行）原样打印到观测台终端。
	go func() {
		scanner := bufio.NewScanner(stderr)
		for scanner.Scan() {
			fmt.Println(scanner.Text())
		}
		err := cmd.Wait()
		h.set(fmt.Sprintf("已退出：%v（端口 %s 可能被占用，重启观测台重试）", err, h.addr))
	}()
}

func (h *mcpHost) stop() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.cmd != nil && h.cmd.Process != nil {
		h.cmd.Process.Kill()
	}
	if h.dir != "" {
		os.RemoveAll(h.dir)
	}
}

// 反向代理保留原请求的 Host（127.0.0.1:8090），SDK 的 DNS 重绑定检查照常生效。
// FlushInterval -1：Streamable HTTP 可能用 SSE 分段返回，每段立刻转发。
func (h *mcpHost) handler() http.Handler {
	target := &url.URL{Scheme: "http", Host: h.addr}
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.FlushInterval = -1
	proxy.ErrorHandler = func(w http.ResponseWriter, _ *http.Request, err error) {
		http.Error(w, "内置 MCP server 不可用："+h.status(), http.StatusBadGateway)
	}
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		h.mu.Lock()
		ready := h.cmd != nil
		h.mu.Unlock()
		if !ready {
			http.Error(w, "内置 MCP server 未就绪："+h.status(), http.StatusServiceUnavailable)
			return
		}
		proxy.ServeHTTP(w, req)
	})
}
