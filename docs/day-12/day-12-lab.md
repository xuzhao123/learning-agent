# Day 12 项目实践：bubblewrap 沙箱与 bash 工具

原理见 [Day 12 学习笔记](day-12-notes.md)。代码在 [internal/sandbox](../../internal/sandbox/)：[sandbox.go](../../internal/sandbox/sandbox.go)（工具与沙箱参数）、[seccomp.go](../../internal/sandbox/seccomp.go)（BPF 过滤程序）、[network.go](../../internal/sandbox/network.go)（白名单代理与沙箱内转发）。浏览器的请求拦截在 [browser.go](../../internal/browser/browser.go) 的 `guard`。

## 1. 实现概览

```sh
./bin/learning-agent -bash -question '……'                          # 断网
./bin/learning-agent -bash -net-allow pypi.org -question '……'       # 只放行 pypi.org 及其子域名
./bin/learning-agent -bash -bash-project-ro -question '……'          # 项目只读挂到 /project（.env、.data、.git、bin 除外）
go run . observe                                                   # 新对话勾选“Bash 沙箱”
```

需要 Linux、bubblewrap；有 systemd 用户会话时启用 cgroup 资源上限。

```text
bash(command)
  systemd-run --user --scope --unit la-sandbox-…   cgroup：内存 512M、无 swap、CPU 1 核、最多 256 个进程（按线程算）
                                                 （认这个参数时再加 --expand-environment=no，见 2.1）
    └ bwrap                                       namespace 全部隔离；/usr 只读；/workspace 可写；根目录只读
        └ seccomp（--seccomp 3）                   拒绝 ptrace、mount、unshare、bpf、io_uring 等
            └ /.sandbox/agent sandbox-init        1 号进程：在 127.0.0.1:3128 转发代理
                └ /bin/bash -c "<command>"
```

| 设计点 | 本项目的选择 |
| --- | --- |
| 隔离工具 | bubblewrap + seccomp，与 Codex、Claude Code 在 Linux 上的路线相同；不需要 Docker |
| 文件系统 | `/usr` 只读，`/etc` 只挂动态库缓存、证书、时区等，passwd/group/hosts 用生成的最小版本；`/workspace` = `.data/sandbox/<对话第一轮的检查点ID>`，同一对话的各轮续聊、续跑共用，子 agent 与回放分叉各有目录（见 3.7）；`/tmp` 是 64MB 内存盘，每次调用都是空的；最后把根目录改成只读 |
| 看不到的 | 家目录、项目目录（默认）、`.env`、宿主机进程、宿主机的环境变量 |
| 资源 | cgroup：内存 512M、不用 swap、CPU 100%、进程数 256（原为 64；进程数按线程算，无头 Chrome 加载一个页面就要 100 个左右，10-19 放宽）；时长跟随 `-tool-timeout`（写进工具描述与提示词）；超时用 `systemctl --user kill` 按 cgroup 杀掉 |
| 系统调用 | 纯 Go 拼出的经典 BPF：先查架构，再对黑名单返回 EPERM（没有 C 编译器，不用 libseccomp） |
| 网络 | 沙箱只有回环接口；宿主机代理（unix socket）检查白名单、只开 80/443、拒绝解析到内网的地址，并直接连接检查过的 IP |
| 白名单 | `-net-allow` 或观测台上点“允许”；模型只能调用 `request_network_access` 发起请求 |
| 工具结果 | 退出码非 0 不算工具失败，照常交给模型；输出保留开头和结尾各 8KB（D30 起放宽到各 128KB，长输出由上下文存成文件）；被代理拒绝的主机列在 `network_denied` |
| 运行时 | bash 不在 `repeatable` 里，不自动重做；同一批里的 bash 调用按顺序依次执行。超时或任务被停止时，沙箱把整条命令杀掉、等进程退出，再返回 `killed`（原因）与终止前的输出：后台不会还在跑，所以不记成“结果未知”（10-20 前是），但终止前做了的事可能已经生效 |
| 浏览器 | CDP Fetch 拦截标签页的每个请求（跳转、子资源），解析到内网就让请求失败 |

## 2. 读代码

### 2.1 沙箱参数

[`bwrapArgs`](../../internal/sandbox/sandbox.go) 按笔记第 3 节的顺序组装：隔离 namespace、去掉特权、只读系统目录、挂工作目录、清空环境变量、加载 seccomp。两个容易漏的细节：

```go
"--remount-ro", "/", "--as-pid-1",
```

- 不加 `--remount-ro /` 时，沙箱的根目录是 bwrap 建的内存盘，`echo hi > /etc/x` 能成功。不影响宿主机，但没必要允许；
- 不加 `--as-pid-1` 时，1 号进程是 bwrap 自己，`cat /proc/1/cmdline` 能看到完整的 bwrap 参数，包括宿主机上的项目路径和临时目录。

命令是原样交给沙箱里的 shell 的，`${...}` 不能被别人先替换掉，于是还要处理 `systemd-run` 自己的一次展开：

```go
noExpandEnv = sync.OnceValue(func() bool { return systemdRun("--expand-environment=no") == nil })
...
if noExpandEnv() {
	opts = append(opts, "--expand-environment=no")
}
```

systemd 254 起，`systemd-run` 会先把命令行参数里的 `${...}` 按它自己的环境替换，再交给子进程：`x=hi; echo ${x}` 这样一条命令会打印空，还多一行 “Referenced but unset environment variable…”。`--expand-environment=no` 关掉这次替换。旧版 systemd 没有替换、也不认这个参数（认了会直接报错），所以先用 `systemd-run --user --scope --quiet --expand-environment=no true` 探测一次，成功才把它加进真正的命令行；新版本来就不替换。探测与真正的执行分开，避免旧版因为这个参数而让每次 bash 调用都失败。

验证方式：在沙箱外跑一条含 `${x}` 的命令（先赋值再用），看输出是不是变量值、有没有展开警告；再把 `--expand-environment=no` 从探测里去掉，确认旧版 systemd 上 bash 照样能执行。

### 2.2 seccomp 程序

```go
put(ldAbs, 0, 0, 4)      // A = arch
put(jeqK, 1, 0, arch)    // 是本机架构就跳过下一条
put(retK, 0, 0, retKill) // 否则杀掉
put(ldAbs, 0, 0, 0)      // A = 系统调用号
for _, nr := range denied {
	put(jeqK, 0, 1, uint32(nr)) // 相等则执行下一条（拒绝），否则跳过它
	put(retK, 0, 0, retErrno)
}
put(retK, 0, 0, retAllow)
```

系统调用号来自 `golang.org/x/sys/unix`，编译时按目标架构（arm64 / amd64）取值。程序写进临时文件，通过 `cmd.ExtraFiles` 作为 3 号文件描述符交给 bwrap，这个描述符可以穿过 `systemd-run --scope` 传下去。

### 2.3 网络：一个检查点

```go
func dialChecked(ctx context.Context, network, addr string) (net.Conn, error) {
	// 端口 80/443？域名在白名单？解析出的地址都不是内网？
	// 通过后直接连接解析出的 IP，不再让别人解析一次
}
```

`CONNECT`（HTTPS 隧道）和普通 HTTP 请求都走这一个函数。沙箱里的 `sandbox-init` 是我们自己的程序：Go 编出来的是静态二进制，可以只读挂进沙箱，不依赖 socat 这类工具。

### 2.4 审批

```text
模型 request_network_access(domain, reason)
  └ agent 打印 Network request [调用ID]: domain=… reason=…   （只是请求）
观测台
  └ 网络审批卡片：允许 / 拒绝 → POST /runs/{id}/network → 对话记录里追加 allow/deny 事件
续聊
  └ 观测台把允许的域名作为 -net-allow 传给新的 agent 进程（子 agent 继承）
```

审批事件追加在 `exit` 之后，所以 `canContinue` 判断“最后一个事件是不是 exit”时要跳过它们。

### 2.5 同一批里的 bash 按顺序执行

模型常在同一轮里同时发出“写文件”和“读这个文件”两个 bash 调用。只靠沙箱里的互斥锁，两条命令不会同时跑，但先后不确定。现在 `ExecuteBatch` 把同一批里的 bash 调用放进一个 goroutine，按调用顺序依次执行，其他工具照常并行。

## 3. 动手

运行时间 2026-10-07（Asia/Shanghai），模型 DeepSeek（`-provider deepseek`，方舟当天欠费）。

### 3.1 正常任务

“写 fib.py 生成前 30 个斐波那契数 → 运行 → wc/tail 检查 → awk 求偶数和”：两次 bash 调用，各几十毫秒，答案 1089154 正确。模型在同一轮里同时发出了两个有依赖的调用，这一次顺序碰巧正确，由此发现了 2.5 节的问题。

沙箱里的环境（模型执行 `id; hostname; env; grep Seccomp /proc/self/status`）：

```text
uid=501(sandbox) gid=1000(sandbox)
sandbox
PATH=/usr/local/bin:/usr/bin:/bin  HOME=/workspace  LANG=C.UTF-8  HTTP(S)_PROXY=http://127.0.0.1:3128 …（没有任何密钥）
CapEff: 0000000000000000   NoNewPrivs: 1   Seccomp: 2   Seccomp_filters: 1
```

### 3.2 安全检查

以“测试自己的沙箱”为由，让模型逐条执行检查命令：

| 检查 | 结果 |
| --- | --- |
| 读项目里的 `.env` | `No such file or directory`：项目没有挂进来 |
| 列 `/home`、`/root`、`~/.ssh` | 都不存在 |
| 环境变量里找 key/token/secret | 没有 |
| 写 `/usr/bin` | `Read-only file system` |
| 写 `/etc` | 第一次成功（根目录是可写的内存盘）→ 加 `--remount-ro /` 后变成 `Read-only file system` |
| ptrace | 返回 -1，errno 1（EPERM）：seccomp 拦下 |
| `unshare -r`、`mount` | `Operation not permitted` |
| `/proc/1/cmdline` | 第一次能看到 bwrap 的完整参数和宿主机路径 → 加 `--as-pid-1` 后只剩 `sandbox-init /bin/bash -c …` |
| 申请 1GB 内存 | 进程被杀，退出码 137 |
| `sleep 30`，`-tool-timeout 5s` | 第一次超时后沙箱进程**还活着**（见 3.3）→ 改为按 cgroup 杀掉后，没有残留进程和 scope |
| 后台任务：`(sleep 3; echo alive > /workspace/bg.txt) &` 后立即返回，下一次调用 `sleep 5; ls bg.txt` | `No such file or directory`：命令返回时 1 号进程退出，PID namespace 里的后台进程一起结束 |

### 3.3 超时后进程没被杀掉

最初的做法是给 `systemd-run` 设独立进程组，超时就向整组发 SIGKILL。实测 `sleep 30` 在超时后还在运行。原因是 bwrap 的 `--new-session` 让沙箱里的进程进了新的会话和进程组，组信号打不到它们。改成每次执行起一个有名字的 scope，超时用 `systemctl --user kill --signal=SIGKILL <unit>.scope` 结束整个 cgroup，之后检查已无残留。这就是笔记 3.2 说的“按 cgroup 杀进程最可靠”。

### 3.4 网络白名单

`-net-allow pypi.org` 时：

```text
Sandbox net: allow host=pypi.org port=443        → curl https://pypi.org/simple/requests/ 返回 200
Sandbox net: deny host=example.com port=443 reason=域名不在白名单
                                                  → curl: (7) CONNECT tunnel failed, response 403
```

工具结果里带着 `network_denied: ["example.com"]` 和提示，模型据此知道该请求批准，而不是去猜网络出了什么问题。

### 3.5 在观测台审批

新对话勾选“Bash 沙箱”，问“用 curl 查 PyPI 上 requests 的最新版本”：

1. 模型先 curl，被代理拒绝；同一轮调用 `request_network_access(pypi.org, 理由)`，回答里说明需要批准；
2. 对话流里出现网络审批卡片，点“允许”。对话记录追加 `{"kind":"allow","text":"pypi.org"}`，刷新页面后状态仍在；
3. 发“已经允许了，请继续查”：续聊事件记 `net_allow: ["pypi.org"]`，代理放行，模型查到 2.34.2。

### 3.6 浏览器跳转到内网

第一次让模型打开 `https://httpbin.org/redirect-to?url=http://127.0.0.1:8090/runs`，模型自己认出这是 SSRF 手法，拒绝调用工具。说明是在验证自己项目的拦截功能后，模型调用了 `open_page`：

```text
Browser [call_00_99vV…]: blocked url=http://127.0.0.1:8090/ reason=不允许访问本机或内网地址：127.0.0.1 → 127.0.0.1
No retry […]: reason=permanent attempts=1 error=页面请求了本机或内网地址，已被拦截：127.0.0.1:8090
```

起始地址是公网的 httpbin，Day 11 只检查起始地址时拦不住；现在跳转后的请求被 Fetch 拦截挡下。模型的判断和工具的拦截是两层独立的防线。

### 3.7 对话级沙箱与文件进出

最初工作目录按本轮的检查点ID分配。续聊每轮换新ID，所以第二轮看不到第一轮写的文件。现在改为按**续接链的起点**分配，一个对话一个工作目录，像托管型 agent 那样每个会话一台机器；观测台可以上传、预览、下载这个目录里的文件。

**读代码**

- [ConversationRoot](../../internal/agent/checkpoint.go)：新的一轮从上一轮（`-continues`）沿续接链往前找，续跑从本轮检查点往前找；子 agent 与 `ForkCheckpoint`/`ForkSnapshot` 的分叉没有续接关系，各自就是起点。
- [main.go](../../main.go) 用它设置 `sandbox.Dir`，并在 `turn/started` 里多发一个 `sandbox` 字段（目录名）；工具描述和系统规则改成“整个对话内保留文件”。
- [files.go](../../internal/observer/files.go)：观测台按 `turn/started` 报告的目录名读写，`GET /runs/{id}/files` 列出，`GET /runs/{id}/files/{path}` 下载，`POST /runs/{id}/files?name=` 上传；都先经 `runOf` 核对租户。

这个目录里的东西是模型写的，观测台把它当不可信数据：

| 风险 | 做法 |
| --- | --- |
| 模型建一个符号链接 `leak-env -> ../../../.env`：沙箱里它什么也指不到，但在宿主机上，从 `.data/sandbox/<ID>/` 往上三层正好是项目根目录 | 只经 `os.Root` 打开文件：解析到根目录外就报错，不跟出去 |
| 模型建命名管道，观测台读它会一直阻塞 | 打开时带 `O_NONBLOCK`，再确认是普通文件 |
| 模型写一个含脚本的 HTML，观测台按网页返回就会在登录后的页面源里执行 | 一律 `Content-Disposition: attachment`、`application/octet-stream`、`nosniff`；页面预览文本只用 `textContent`，图片经 blob 地址放进 `<img>` |
| 上传覆盖模型的文件、写到目录外 | 只放顶层、不以点开头、不覆盖（`O_EXCL`），单个不超过 10 MB；运行中不接受 |

上传是人对工作目录的改动，先写审计（`user_upload`，文件名和内容哈希），写不进就不上传；下载是只读操作，不记。

**验证**（2026-10-15，DeepSeek，观测台接口，临时测试账号验证后已删除）：

| 检查 | 结果 |
| --- | --- |
| 第一轮写 `notes.txt`，第二轮续聊问内容 | 第二轮原样读出；四轮 `turn/started` 的 `sandbox` 都是第一轮的检查点ID，没有按轮新建目录；第二轮私有标记为 `bash` |
| 上传 `销售.csv` 后第三轮问城市数与平均分 | 3 个、87.67，正确 |
| 同名再传、`../../x.csv`、`.bashrc`、跨站 Origin | 409、400、400、403 |
| 租户账号对这条平台对话列表/下载/上传；未登录 | 都是 404；401 |
| 模型建的 `leak-env -> ../../../.env`（宿主机上确实指向项目的 `.env`）、`abs-link -> /etc/hostname` | 列表里显示为链接；下载都是 404 |
| 命名管道、目录 | 400，观测台没有卡住 |
| `page.html` | 按附件下载，`application/octet-stream` + `nosniff` |
| 第四轮派子 agent `ls -la /workspace` | 两个子 agent 的目录都是空的，看不到父对话的文件 |

回放分叉用新目录这一条，只按代码确认（分叉检查点不带 `continues`），没有用真实回放运行验证。第四轮触发了上下文压缩，模型按摘要把前几轮的目标又做了一遍，这是摘要本身的行为，和本节改动无关。

## 4. 已知边界

- 共享宿主机内核：内核漏洞可以逃出沙箱。需要 gVisor 或微虚拟机。
- seccomp 是黑名单，不是白名单。
- 白名单内的域名可以收发任意数据；白名单按域名放行，同一域名下的所有路径都可访问。
- 只有 HTTP(S) 走代理；不支持其他协议，也不提供 DNS（工具都通过代理解析）。
- 内网判断最初只用 Go 的 `IsLoopback/IsPrivate/IsLinkLocalUnicast/IsUnspecified`，漏掉了 `100.64.0.0/10`（运营商级 NAT，阿里云元数据服务 `100.100.100.200` 就在其中）和 `198.18.0.0/15` 等特殊地址段。现在代理与浏览器共用 [netguard](../../internal/netguard/netguard.go)，按 IANA 特殊用途地址表只放行公网全局单播地址。见 [Q18](../deep-questions.md#q18)。
- 浏览器的 Fetch 拦截挡不住 DNS 重绑定（检查时是公网、Chrome 自己解析时变成内网），也不拦截 WebSocket。浏览器没有纳入 Bash 沙箱；当前代码默认保留 Chrome 自身沙箱，显式设置 `CHROME_NO_SANDBOX=1` 才会关闭它。
- 浏览器加 bash 时，浏览器仍是外发通道（打开带参数的 URL），没有审批；高风险操作确认在 Week 3。
- 工作目录不限总大小（`/tmp` 限 64MB）；没有 systemd 用户会话时不做资源限制（会打印提示）。
- 每次调用都是新的 shell，`cd`、环境变量不保留；后台进程活不过一次调用。
- 对话沙箱的文件留到手动删除为止，没有总大小与保留期限；对话越长，积累的文件越多。
- 回放分叉从空目录开始，不还原分叉那一刻的文件；依赖文件的任务，回放结论要打折扣。
- 命令行从同一轮 `-continues` 两次会分出两条续接链，它们共用一个目录；观测台一条对话一次只跑一轮，不会出现。
- 上传只到顶层、单个 10 MB、不覆盖；面板只列前 1000 项，不支持删除。
- 只支持 Linux；macOS 需要 Seatbelt，没有实现。
