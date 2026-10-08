package sandbox

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"learning-agent/internal/netguard"
)

// 网络：沙箱在自己的网络 namespace 里，只有回环接口，直接连外网一定失败。
// 需要联网时只能走代理：
//
//	沙箱内 curl/pip ──HTTP_PROXY──▶ 127.0.0.1:3128（沙箱里的 sandbox-init 转发）
//	   ──unix socket（挂进沙箱）──▶ agent 进程里的代理 ──白名单 + 拒绝内网地址──▶ 外网
//
// 白名单只能由用户添加（-net-allow，或在观测台点“允许”）。模型只能调用 request_network_access 发起请求，不能自己批准。
var Allow []string

// 运行中也会加入新域名（用户在界面上当场批准），代理的检查与追加用同一把锁。
var allowMu sync.Mutex

// AllowDomain 把用户刚批准的域名加进白名单，本轮接下来的命令立即可以访问。
func AllowDomain(domain string) {
	allowMu.Lock()
	defer allowMu.Unlock()
	if !slices.Contains(Allow, domain) {
		Allow = append(Allow, domain)
	}
}

const proxyPort = "3128"

var domainPattern = regexp.MustCompile(`^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z]{2,63}$`)

// ValidDomain：小写域名，不接受 IP 字面量和通配符。
func ValidDomain(domain string) bool { return len(domain) <= 253 && domainPattern.MatchString(domain) }

// 白名单里的 example.com 同时放行它的子域名 a.example.com。
func allowed(host string) bool {
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	allowMu.Lock()
	defer allowMu.Unlock()
	return slices.ContainsFunc(Allow, func(a string) bool { return host == a || strings.HasSuffix(host, "."+a) })
}

var (
	proxyOnce sync.Once
	proxyDir  string
	proxyErr  error
	deniedMu  sync.Mutex
	denials   []string // 本次 bash 调用被拒绝的主机（调用串行，所以一份就够）
)

// 唯一的放行检查点：白名单、端口、解析后的地址都在这里核对，然后直接连接核对过的 IP，
// 不让后续再解析一次（否则 DNS 可以在“检查”和“连接”之间换成内网地址）。
func dialChecked(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	deny := func(reason string) (net.Conn, error) {
		deniedMu.Lock()
		if !slices.Contains(denials, host) {
			denials = append(denials, host)
		}
		deniedMu.Unlock()
		fmt.Printf("Sandbox net: deny host=%s port=%s reason=%s\n", host, port, reason)
		return nil, errors.New(reason)
	}
	if port != "80" && port != "443" {
		return deny("只允许 80 和 443 端口")
	}
	if !allowed(host) {
		return deny("域名不在白名单")
	}
	ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil || len(ips) == 0 {
		return deny("无法解析域名")
	}
	for _, ip := range ips {
		if netguard.Internal(ip.IP) {
			return deny("解析到本机、内网或保留地址 " + ip.IP.String())
		}
	}
	fmt.Printf("Sandbox net: allow host=%s port=%s\n", host, port)
	return (&net.Dialer{Timeout: 10 * time.Second}).DialContext(ctx, network, net.JoinHostPort(ips[0].IP.String(), port))
}

// 代理在 agent 进程里，监听一个临时目录里的 unix socket；目录挂进沙箱的 /.sandbox/net。
func startProxy() (string, error) {
	proxyOnce.Do(func() {
		proxyDir, proxyErr = os.MkdirTemp("", "la-sandbox-net-")
		if proxyErr != nil {
			return
		}
		var listener net.Listener
		listener, proxyErr = net.Listen("unix", filepath.Join(proxyDir, "proxy.sock"))
		if proxyErr != nil {
			return
		}
		transport := &http.Transport{DialContext: dialChecked, Proxy: nil}
		go http.Serve(listener, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodConnect { // HTTPS：建立隧道，内容是加密的，代理只看得到域名
				upstream, err := dialChecked(r.Context(), "tcp", r.Host)
				if err != nil {
					http.Error(w, err.Error(), http.StatusForbidden)
					return
				}
				client, buffered, err := http.NewResponseController(w).Hijack()
				if err != nil {
					upstream.Close()
					return
				}
				client.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n"))
				go pipe(upstream, buffered)
				pipe(client, upstream)
				return
			}
			if r.URL.Host == "" { // 不是代理请求
				http.Error(w, "只接受代理请求", http.StatusBadRequest)
				return
			}
			r.RequestURI = ""
			response, err := transport.RoundTrip(r)
			if err != nil {
				http.Error(w, err.Error(), http.StatusForbidden)
				return
			}
			defer response.Body.Close()
			for k, v := range response.Header {
				w.Header()[k] = v
			}
			w.WriteHeader(response.StatusCode)
			io.Copy(w, response.Body)
		}))
	})
	return proxyDir, proxyErr
}

func pipe(dst io.WriteCloser, src io.Reader) {
	io.Copy(dst, src)
	dst.Close()
}

// Init 是沙箱里的第一个程序（go run . sandbox-init 命令 参数…）：在沙箱的回环接口上开 3128 端口，
// 把连接转发到挂进来的代理 socket，然后运行真正的命令，原样返回它的退出码。
func Init(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "sandbox-init 需要命令")
		return 2
	}
	if listener, err := net.Listen("tcp", "127.0.0.1:"+proxyPort); err == nil {
		go func() {
			for {
				conn, err := listener.Accept()
				if err != nil {
					return
				}
				go func() {
					upstream, err := net.Dial("unix", "/.sandbox/net/proxy.sock")
					if err != nil {
						conn.Close()
						return
					}
					go pipe(upstream, conn)
					pipe(conn, bufio.NewReader(upstream))
				}()
			}
		}()
	}
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = nil, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		if exit, ok := err.(*exec.ExitError); ok {
			return exit.ExitCode()
		}
		fmt.Fprintln(os.Stderr, err)
		return 127
	}
	return 0
}
