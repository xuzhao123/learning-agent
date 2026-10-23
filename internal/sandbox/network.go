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

	"learning-agent/internal/audit"
	"learning-agent/internal/netguard"
	"learning-agent/internal/permission"
	"learning-agent/internal/protocol"
)

// 网络：沙箱在自己的网络 namespace 里，只有回环接口，直接连外网一定失败。
// 需要联网时只能走代理：
//
//	沙箱内 curl/pip ──HTTP_PROXY──▶ 127.0.0.1:3128（沙箱里的 sandbox-init 转发）
//	   ──unix socket（挂进沙箱）──▶ agent 进程里的代理 ──白名单 + 拒绝内网地址──▶ 外网
//
// 白名单只能由用户添加（-net-allow，或在观测台的设置、审批里）。连到不在白名单的域名时，代理当场请界面上的人批准，
// 连接挂起等答复，批准了命令接着执行（同 Claude Code 的沙箱代理）；模型也可以先调用 request_network_access 说明理由。
// 两条路都只是发起请求，批准只能由人在界面上做。
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

// askUser 经协议请界面上的人批准访问 record.Target，在这里等答复，返回 allow、deny、
// unavailable（没有界面，或界面上没有能答复的人）、timeout；整轮被中断时返回 ctx 的错误。
// 请求先写审计（D21），写不进就不问；批准后加进白名单，本轮接下来的命令立即可以访问。
// request_network_access（模型发起）与代理（程序发起）共用这一份。
func askUser(ctx context.Context, record audit.Record, reason string) (_ string, err error) {
	fmt.Printf("Network request [%s]: domain=%s reason=%s\n", record.CallID, record.Target, reason)
	record.Event, record.Risk = "network_request", "network"
	// full 模式同 Claude Code 的 bypassPermissions：不在白名单的域名不问人，由程序放行。照常先写审计，写不进就不放行；
	// 端口、IP 地址与内网地址的检查在 dialChecked 里照旧。
	if permission.Mode == "full" {
		record.Event, record.Actor, record.Decision, record.Detail = "network_decision", "program", "allow_mode", "权限模式 full，不询问："+reason
		if err := audit.Write(ctx, record); err != nil {
			return "", fmt.Errorf("审计日志写入失败，不放行：%w", err)
		}
		AllowDomain(record.Target)
		fmt.Printf("Network decision [%s]: domain=%s decision=allow_mode\n", record.CallID, record.Target)
		return "allow", nil
	}
	if err := audit.Write(ctx, record); err != nil {
		return "", fmt.Errorf("审计日志写入失败，不发起联网审批：%w", err)
	}
	record.Event, record.Actor, record.Detail = "network_decision", "program", ""
	defer func() { audit.Log(ctx, record) }()
	waitCtx, cancel := context.WithTimeout(ctx, ApprovalWait)
	defer cancel()
	var answer struct {
		Decision string `json:"decision"`
	}
	err = protocol.Call(waitCtx, "network/requestApproval", map[string]string{"call_id": record.CallID, "domain": record.Target, "reason": reason}, &answer)
	switch {
	case ctx.Err() != nil:
		record.Decision = "cancelled"
		return "", ctx.Err()
	case err == nil && (answer.Decision == "allow" || answer.Decision == "deny"):
		record.Decision, record.Actor, record.Detail = answer.Decision, "user", "经界面答复"
	case errors.Is(err, protocol.ErrNoClient) || (err == nil && answer.Decision == "unavailable"):
		record.Decision = "unavailable" // 命令行或队列里没有可以批准的人
	default:
		record.Decision = "timeout"
	}
	if record.Decision == "allow" {
		AllowDomain(record.Target)
	}
	askMu.Lock()
	if record.Decision == "deny" || record.Decision == "timeout" {
		settled[record.Target] = record.Decision
	} else {
		delete(settled, record.Target)
	}
	askMu.Unlock()
	fmt.Printf("Network decision [%s]: domain=%s decision=%s\n", record.CallID, record.Target, record.Decision)
	return record.Decision, nil
}

// 代理发起的审批：同一域名同时有几条连接（pip 会并发下载）只问一次，其余等同一个答复。
// 本轮里人拒绝过或没答复的域名记在 settled，代理不再重复问、直接按上次的结果拒绝，免得每条命令都停下来等；
// request_network_access 是模型带着理由重新申请，照常问人。
var (
	askMu    sync.Mutex
	asking   = map[string]*pendingAsk{}
	settled  = map[string]string{}
	lastCall string // 最近启动的沙箱命令的调用ID：代理发起的审批挂在它下面显示（代理分不出是哪个沙箱连进来的）
	// 等人答复的时间不算进命令时限（见 execute）：从第一个审批开始等到最后一个答复为止算一段，几个同时等的不重复计。
	waiting   int
	waitSince time.Time
	waitTotal time.Duration
)

type pendingAsk struct {
	done     chan struct{}
	decision string
}

// askProxy 在代理里挂起这条连接、请人批准。这里没有整轮的 ctx：等待只受 ApprovalWait 限制，
// 命令先被终止了也照样等到答复，人批准的域名本轮后面的命令仍可用。理由由程序写，不经过模型。
func askProxy(host, port string) string {
	if protocol.Client == nil && permission.Mode != "full" { // 命令行或队列里没有界面，没有人可问：照旧立即拒绝，不发请求也不记审计
		return "unavailable"
	}
	askMu.Lock()
	if decision, ok := settled[host]; ok {
		askMu.Unlock()
		return decision
	}
	a, asked := asking[host]
	if !asked {
		a = &pendingAsk{done: make(chan struct{})}
		asking[host] = a
		if waiting == 0 {
			waitSince = time.Now()
		}
		waiting++
	}
	call := lastCall
	askMu.Unlock()
	if asked {
		<-a.done
		return a.decision
	}
	record := audit.Record{Actor: "program", CallID: call, Tool: "bash", Target: host}
	record.Detail = "沙箱命令连接 " + host + ":" + port + "，代理请用户批准"
	decision, err := askUser(context.Background(), record, "沙箱里的命令要连接 "+host+":"+port)
	if err != nil {
		decision = err.Error()
	}
	askMu.Lock()
	a.decision = decision
	delete(asking, host)
	if waiting--; waiting == 0 {
		waitTotal += time.Since(waitSince)
	}
	askMu.Unlock()
	close(a.done)
	return decision
}

// paused 是到现在为止等人批准联网一共用了多久（包括正在等的这一段）。
func paused() time.Duration {
	askMu.Lock()
	defer askMu.Unlock()
	if waiting > 0 {
		return waitTotal + time.Since(waitSince)
	}
	return waitTotal
}

var (
	proxyOnce sync.Once
	proxyDir  string
	proxyErr  error
	deniedMu  sync.Mutex
	denials   []denial // 本次 bash 调用被拒绝的连接（调用串行，所以一份就够）
)

// denial 记下被拒绝的主机、原因和类别，提示里按类别告诉模型下一步：
// ask 是不在白名单、当时没有界面可以问人（可以调用 request_network_access）；denied 是人拒绝了；timeout 是人还没答复；
// fixed 是端口不对、解析到内网或解析失败这类批准了也打不开的，免得模型去申请一个没用的批准。
type denial struct {
	host, reason, kind string
}

// 唯一的放行检查点：白名单、端口、解析后的地址都在这里核对，然后直接连接核对过的 IP，
// 不让后续再解析一次（否则 DNS 可以在“检查”和“连接”之间换成内网地址）。
func dialChecked(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	deny := func(reason, kind string) (net.Conn, error) {
		d := denial{host, reason, kind}
		deniedMu.Lock()
		if !slices.Contains(denials, d) {
			denials = append(denials, d)
		}
		deniedMu.Unlock()
		fmt.Printf("Sandbox net: deny host=%s port=%s reason=%s\n", host, port, reason)
		return nil, errors.New(reason)
	}
	if port != "80" && port != "443" {
		return deny("只允许 80 和 443 端口", "fixed")
	}
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	if !allowed(host) {
		if !ValidDomain(host) {
			return deny("白名单只收域名，IP 地址批准不了", "fixed")
		}
		switch decision := askProxy(host, port); decision {
		case "allow":
		case "deny":
			return deny("用户拒绝了这个域名", "denied")
		case "timeout":
			return deny("等待用户批准超时", "timeout")
		case "unavailable":
			return deny("域名不在白名单", "ask")
		default:
			return deny(decision, "fixed")
		}
	}
	ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil || len(ips) == 0 {
		return deny("无法解析域名", "fixed")
	}
	for _, ip := range ips {
		if netguard.Internal(ip.IP) {
			return deny("解析到本机、内网或保留地址 "+ip.IP.String(), "fixed")
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
