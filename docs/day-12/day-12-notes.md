# Day 12：代码执行沙箱——让模型安全地使用 shell

## 学习目标

读完本文，应能回答以下问题：

1. 为什么 agent 需要一个能执行任意命令的工具？它带来什么风险？
2. “沙箱”有哪几种实现？各自挡住什么、代价是什么？
3. Linux 上一个轻量沙箱由哪几层组成？namespace、cgroup、seccomp 分别管什么？
4. 沙箱需要联网时，怎样只放行指定的域名？
5. 沙箱挡不住的风险有哪些？还需要哪些措施？

## 1. 为什么要一个 shell 工具

给模型写一堆专用工具（计算器、搜索笔记、读文件、跑测试……）永远写不完。另一条路是给它一个通用的 shell：模型本来就熟悉 `ls`、`grep`、`python`、`curl`，一个工具就能覆盖大量需求。Codex、Claude Code 这类编码 agent 的核心工具就是 shell。

代价是：模型能执行任何命令。而模型不是完全可信的：它会犯错，也可能被它读到的网页、文件、工具结果里的指令诱导（间接提示词注入）。所以 shell 工具必须放在沙箱里：**限制一条命令能看到什么、能做什么、能用多少资源。**

## 2. 隔离的几个层次

| 层次 | 例子 | 原理 | 代价 |
| --- | --- | --- | --- |
| 进程自我限制 | seccomp、Landlock | 进程给自己加规则：哪些系统调用、哪些路径可用 | 几乎为零；只限制不隔离 |
| 操作系统沙箱工具 | Linux 的 bubblewrap、macOS 的 Seatbelt | 用内核功能给进程一个受限的视图 | 几十毫秒；共享宿主机内核 |
| 容器 | Docker、Podman | 同样基于 namespace 和 cgroup，外加镜像、网络管理和生命周期 | 需要运行时；启动更慢 |
| 用户态内核 | gVisor | 系统调用由一个用户态“内核”处理，真正的内核只看到很少的调用 | 系统调用变慢、兼容性稍差 |
| 微虚拟机 | Firecracker、Kata Containers | 每个任务一个独立的轻量虚拟机，有自己的内核 | 隔离最强；启动与内存开销最大 |

前三层共享宿主机的内核：内核漏洞可以让程序逃出来。后两层才能防住内核漏洞。选择取决于威胁模型：

- **在开发者自己的电脑上执行**（Codex CLI、Claude Code）：代码大多由用户自己发起，需要直接读写本机项目、每条命令都要快，通常用操作系统沙箱工具。
- **在服务商的机器上执行别人的代码**（云端 agent、代码解释器、多租户平台）：完全不可信、多个用户共用硬件，用容器加 gVisor，或微虚拟机。

## 3. Linux 轻量沙箱的三层

### 3.1 namespace：看得到什么

namespace 让进程拥有一套自己的系统资源视图：

| namespace | 隔离的东西 | 沙箱里的效果 |
| --- | --- | --- |
| mount | 挂载点 | 只看得到挂进来的目录；系统目录只读，工作目录可写 |
| PID | 进程号 | 只看得到沙箱里的进程，自己是 1 号进程 |
| network | 网卡、路由 | 只有回环接口，直接连外网一定失败 |
| user | 用户与权限 | 沙箱里的“root”不等于宿主机的 root |
| IPC、UTS、cgroup | 共享内存、主机名、cgroup 视图 | 互不干扰 |

bubblewrap（`bwrap`）是一个小工具：按参数建好这些 namespace，把指定目录挂进去，然后运行命令。Flatpak 和 Codex、Claude Code 在 Linux 上都用它。

**PID namespace 的一个好处**：沙箱的 1 号进程退出时，内核会结束这个 namespace 里的所有进程。命令放到后台的进程不会活到下一次调用。

### 3.2 cgroup：能用多少

cgroup（控制组）限制一组进程的资源：内存上限、CPU 份额、进程数上限。超出内存上限的进程会被内核杀掉（OOM）；进程数达到上限后，再创建进程会失败。没有这一层，一个死循环或者不断创建进程的脚本就能拖垮整台机器。

结束一个沙箱时，按 cgroup 杀进程最可靠：cgroup 里有哪些进程是确定的，不管它们换了会话、进程组还是父进程。

### 3.3 seccomp：能做哪些系统调用

程序要做任何事（读文件、建网络连接、创建进程）都得通过系统调用请求内核。seccomp 让进程装上一段过滤程序（经典 BPF 字节码），每次系统调用前先过一遍：放行、返回错误或直接杀掉进程。

沙箱通常用黑名单禁止普通程序用不到、却常被用来逃逸或攻击内核的调用：`ptrace`（读写其他进程）、`mount`、`unshare`/`setns`（新建或进入 namespace）、`bpf`、`perf_event_open`、`io_uring`、内核密钥环、加载内核模块等。过滤程序还要先检查 CPU 架构，否则可以用另一套调用号（比如 x86 上的 32 位调用）绕过。

更严格的做法是白名单：只放行明确需要的调用。白名单更安全，但维护成本高，程序换个版本可能就用到新的调用。

### 3.4 还有几项零碎但重要的设置

- **no_new_privs**：之后执行的程序不能通过 setuid 获得更高权限；
- **去掉全部 capabilities**：沙箱里的进程没有任何特权；
- **清空环境变量**：密钥常常放在环境变量里，不能带进沙箱；
- **新会话**：防止沙箱里的程序向宿主机终端注入按键（TIOCSTI）；
- **根目录只读**：除了工作目录和临时目录，哪里都不能写。

## 4. 联网：通过代理放行指定域名

完全断网最安全，但很多正常任务需要联网：装依赖包、下载数据。常见做法（Claude Code 的沙箱就是这样）：

```text
沙箱（只有回环接口）
  curl / pip ──HTTP_PROXY=127.0.0.1:3128──▶ 沙箱内的转发程序
      ──unix socket（挂进沙箱的文件）──▶ 宿主机上的代理
          检查：域名在白名单？端口是 80/443？解析出的地址不是内网？
          ──▶ 外网
```

- 沙箱自己没有网卡通往外面，**所有**流量只能经过代理，代理是唯一的检查点；
- HTTPS 通过 `CONNECT` 建隧道，代理看不到内容，但看得到域名，足够按域名放行；
- 代理在检查通过后**直接连接它解析出的那个 IP**，不让后续再解析一次。否则 DNS 可以在“检查”和“连接”之间换成内网地址（DNS 重绑定）；
- **白名单由人决定**：模型可以说明理由、发起请求，但批准必须由用户完成。否则被注入的模型可以自己给自己开网。

## 5. 沙箱挡不住什么

沙箱划出边界，边界里面的事它管不了：

- **允许写的地方**：工作目录里的文件可以被删改。对策是让工作目录只放这个任务的东西，重要的内容另有备份。
- **允许访问的地址**：白名单里的域名可以收发任意数据。白名单要尽量小。
- **合法但有害的操作**：沙箱只能限制后果，不能判断意图。高风险操作（删除、付款、发邮件、联网）需要人工确认。
- **内核漏洞**：共享内核的沙箱挡不住，需要 gVisor 或虚拟机。

### 致命三要素

Simon Willison 总结过 agent 数据外泄的必要条件，三者同时具备时，一次提示词注入就可能泄露数据：

1. 能读到**不可信的内容**（网页、邮件、外部文件）；
2. 能接触**私密数据**（对话、记忆、本地文件、密钥）；
3. 有**向外发送数据的通道**（联网的 shell、浏览器打开带参数的 URL、发邮件）。

缓解的思路是至少切断其中一条：读不可信内容的 agent 不给外发通道，或者外发需要人工确认。注意浏览器本身就是一个外发通道：打开 `https://attacker.example/?data=...` 就把数据带出去了。

## 6. 自测

**1. 为什么 Codex、Claude Code 在本机执行命令时不用 Docker？**
它们要直接操作用户本机的项目和工具链，每条命令都要快，也不能假设用户装了 Docker。bubblewrap、Seatbelt 这类操作系统沙箱启动只要几十毫秒，足够限制文件、网络和系统调用。

**2. namespace、cgroup、seccomp 分别解决什么？**
namespace 限制“看得到什么”（文件、进程、网络）；cgroup 限制“用多少”（内存、CPU、进程数）；seccomp 限制“能做哪些系统调用”。

**3. 沙箱完全断网后，怎样让它只能访问 pypi.org？**
在宿主机上开一个代理，把它的 unix socket 挂进沙箱，沙箱里用环境变量把 HTTP(S) 流量指向它；代理按白名单检查域名和端口、拒绝解析到内网的地址，并直接连接检查过的 IP。

**4. 为什么白名单不能让模型自己加？**
模型可能被它读到的内容注入指令。如果它能自己批准，攻击者只要让它“请求访问 attacker.example”就能开网外发数据。批准必须由人完成。

**5. 命令超时后，为什么按 cgroup 杀进程比按进程组杀更可靠？**
沙箱里的进程可能新建会话、换进程组、被重新挂到别的父进程下，进程组信号打不到它们；cgroup 的成员由内核记录，一个都不会漏。

**6. 沙箱里只有一个可写的工作目录，还有什么风险？**
工作目录里的文件可能被删改；白名单内的域名可以外发数据；合法但有害的操作沙箱判断不了；共享内核的沙箱挡不住内核漏洞。

## 参考

- bubblewrap，[github.com/containers/bubblewrap](https://github.com/containers/bubblewrap)
- Linux man pages：[namespaces(7)](https://man7.org/linux/man-pages/man7/namespaces.7.html)、[cgroups(7)](https://man7.org/linux/man-pages/man7/cgroups.7.html)、[seccomp(2)](https://man7.org/linux/man-pages/man2/seccomp.2.html)、[landlock(7)](https://man7.org/linux/man-pages/man7/landlock.7.html)
- Linux kernel，[Seccomp BPF](https://docs.kernel.org/userspace-api/seccomp_filter.html)
- Anthropic，[Claude Code Sandboxing](https://code.claude.com/docs/en/sandboxing)
- OpenAI Codex，[codex-rs/linux-sandbox](https://github.com/openai/codex)；第三方解读 [Codex CLI Sandbox Internals](https://codex.danielvaughan.com/2026/05/03/codex-cli-sandbox-internals-seatbelt-bubblewrap-landlock-windows-dacl/)
- gVisor，[What is gVisor?](https://gvisor.dev/docs/)；Firecracker，[firecracker-microvm.github.io](https://firecracker-microvm.github.io/)
- Simon Willison，[The lethal trifecta for AI agents](https://simonwillison.net/2025/Jun/16/the-lethal-trifecta/)，2025

本课程的代码实现、动手步骤与实验记录见 [Day 12 项目实践](day-12-lab.md)。
