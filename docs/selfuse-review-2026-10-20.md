# 自用对话复查：图片输入第一步收尾（2026-10-20）

> 范围：观测台里的一段真实对话（对话 `f6ed2e51-5a60-4519-aa69-1b57af449a3d`，检查点 `819ebd75-5841-455c-a6d2-508a3ce70d84`，存档与检查点都在 `.data/`，不提交）。方法：逐条读存档里的协议事件与代理截获的请求、响应，对照源码找原因；用存档里的请求体做了一次清理策略的粗算。本次没有改代码，也没有运行模型。
>
> 编号接着 [架构评估](architecture-review.md) 第 5 节：发现从 R13 起，优化建议从 O11 起。规划的改动见 [plan.md](../plan.md)。

## 1. 概况

对话在方舟 Coding Plan 上用 deepseek-v4.1-flash，工作区是 `.data/selfuse/` 里的 clone，权限 edit，开了 bash、子 agent、浏览器、ds_search，hooks 是 secret-guard、protect-generated、gofmt + js-check、go-check 四组。任务是 [图片输入设计](image-input-design.md) 的 A–D：附件函数、agent 的张数上限与私有来源、观测台接口与代理存档、页面。

结论：一轮、104 次模型请求、约 12 分钟，第 104 次被 Coding Plan 的 5 小时额度拦下（HTTP 429），任务停在页面自查之后、观测台 curl 自查之前，Stop hook 没有跑。单次决定大多合理，问题集中在两处：一轮里请求很多，每次都带着全部历史；几处工具与 hook 的反馈不准确，让模型绕开了编辑工具。

| 项 | 数值 |
| --- | --- |
| 模型请求 | 104（最后一次 429）；103 次带工具调用，其中 101 次只调一个工具 |
| 工具调用 | 105：bash 56、edit_file 25、read_file 22、write_file 1、read_output 1；grep、glob 一次没用 |
| 失败 | edit_file 4（2 次没先 read_file，2 次缩进对不上）；bash 2 次超时 |
| token | 输入 1058 万（缓存 1041 万，98.4%），输出 4.5 万；单次输入从 1.1 万涨到 16.4 万 |
| 三段 | 读代码（第 1–38 次）219 万；改代码（39–88）603 万；自查（89–104）235 万 |
| hooks | 63 次，8 次拦截，全是 gofmt、同一段差异 |
| D24 提醒 | 9 次“文件在你读过之后被改过”，都是模型自己用 bash 改的 |
| 存档 | 39.5 MB，其中 96% 是每次请求的完整请求体 |

**额度怎么算（推断，以控制台用量明细为准）**：这个 5 小时窗口里只有两段对话，前一段 8 次请求、13 万 token 后被取消，这一段 1058 万 token。第三方实测 Pro 套餐 600 次请求、5000 万 token 用掉 5 小时额度的 86%；Lite 约为 Pro 的 1/5，折合约 1100 万 token，与这里用掉的量相当。如果套餐是 Lite，额度很可能按 token 计、缓存命中也算在内，那么缓存命中率再高也省不了额度，每次请求带多长的上下文直接决定能跑多少步。

## 2. 发现

### R13 额度用完时丢了原因，也没有收尾

- 上游的原话是 `AccountQuotaExceeded`、“You have exceeded the 5-hour usage quota. It will reset at …”，agent 只留下“模型API返回HTTP 429”（[`internal/llm/llm.go`](../internal/llm/llm.go) 第 131 行只取状态码）。用户要打开存档才知道是额度、什么时候恢复。
- O1 计划对 429 退避重试，但 429 有两种：限流过一会儿就好；额度用完要等几个小时，重试只是空等。
- 对话以 model_error 结束，Stop hook（go-check）只在模型想结束时运行，这次没有跑；用户不知道停下时代码能不能编译。检查点可以续跑（`checkpoint.go` 把 model_error 算作可续跑）。

### R14 一轮之内上下文只增不减

- 单次输入从 1.1 万涨到 16.4 万，全程没有清理；1058 万的总输入就是每次请求上下文的累加，请求越多增长越接近平方。
- 最后一次请求里，工具结果占 63%，模型写的工具参数（edit_file 的新旧原文、python heredoc）占 32%，系统提示词 2%。
- R2 计划的“每轮开始清理上一轮以前的工具结果”对只有一轮的长任务不起作用。
- 粗算（按字符，不算缓存失效的代价）：每次请求只保留最近 20 个工具结果、更早的换成约 150 字的占位，全程输入是原来的 48%；保留 40 个是 65%，保留 10 个是 38%。[`context_manager.go`](../internal/agent/context_manager.go) 的 Prepare 已经有“较早的工具结果已清理 + read_output”的替换，但只在接近压缩线时触发，这次最高只到 16.4 万（压缩线 23.1 万）。

### R15 几乎全是一次一个工具

- 101/103 次请求只有一个工具调用。读代码阶段 38 次请求大多是 `sed -n` 一段一段读 `index.html`（三千多行），每次只读几十行。
- 请求里开着 `parallel_tool_calls`、执行器允许 4 个并行，但提示词没有提过并行，也没有说优先用 grep、glob、read_file。内置 grep、glob 一次没用，bash 56 次里约 30 次只是用 grep、sed 看代码。
- 在上下文 15 万左右时，每多一次请求就多花约 15 万 token；R14 与这条叠在一起，是额度很快用完的主要原因。

### R16 用 bash 改工作区文件，绕过了编辑工具的保障

- 工作区里 8 处改动是用 bash 写的：`index.html` 7 处 python 字符串替换，`attachments.go` 1 处 python 加 import，外加一次对 6 个文件的 `gofmt -w`。
- 绕过的东西：D29 快照没有这些改动；python 与 `gofmt -w` 在最后一次 edit 之后又改了 `server.go`、`attachments.go`，撤销这一轮会因冲突整次被拒；PreToolUse 的 protect-generated、PostToolUse 的 gofmt 与 js-check 都没跑（模型后来自己 `node --check` 了一次）。
- D24 提醒每次都写“可能是 bash 命令、编辑器或其他程序”（[`internal/workspace/context.go`](../internal/workspace/context.go) 第 116 行），其实程序知道上一批里有 bash，而且是模型自己改的；9 条提醒只增加了上下文。
- 系统提示词写着“用 bash 改过的文件，要用文件工具再改之前先重新 read_file”（[`internal/sandbox/sandbox.go`](../internal/sandbox/sandbox.go) 第 124 行），等于默认可以这样改。
- 模型为什么换成 python，推理内容里没有写，存档里看不出来。Claude Code 的检查点同样不记录 Bash 改的文件（见来源），这是常见的缺口，不是本项目独有。

### R17 gofmt hook 让同一段差异重复了 8 次

- 一次 edit 把 `server.go` 里结构体标签的对齐弄乱，之后 7 次改别处的 edit 每次都收到同一段 2117 字的 diff，前后不到一分钟。模型先把手头的修改做完，最后用 bash 跑了 `gofmt -w`，这也是 R16 的来源之一。
- [`hooks/gofmt.sh`](../hooks/gofmt.sh) 要求“用 edit_file 改成 gofmt 的样子”，对齐类的差异很难照做；它只提醒、不改，是因为 hook 改了文件会和快照、D24 的读取状态对不上。
- diff 里是宿主机上的绝对路径，模型在沙箱里看到的是 `/workspace`。

### R18 bash 超时报“结果未知”，实际结果是确定的

- 时限 30 秒（`-tool-timeout`）没写进工具描述，提示词只说“时长有限”，bash 也没有时限参数。模型写了 `timeout 60 google-chrome`，不知道 30 秒就会被杀。
- 超时后沙箱按 cgroup 整体 SIGKILL 并等进程退出（`sandbox.go` 第 306 行起），不会有东西留在后台；但执行器回给模型“可能已经生效或仍在后台进行……不要重试，直接告诉用户结果未知”（[`internal/agent/react.go`](../internal/agent/react.go) 第 693 行），已经产生的输出也丢了。这段说明本来是为有外部副作用的工具写的（D8、D20）。
- 这次模型没有照做，接着查原因，又花了一次请求 `pgrep chrome`；真正的原因是测试页里的 `alert()` 卡住了无头 Chrome，部分输出本来可能直接看出来。换一个更听话的模型，任务会在这里停下。
- 用户要求“一次 bash 调用里构建、起观测台、curl 测试、关掉”，冷缓存下 30 秒不够。

### R19 read_file 的结果进了 read_output，行号印了两遍

- 设计文档 1.29 万字，read_file 的结果超过 4000 token 的结果预算，被存成文件；再用 read_output 读，每行变成 `58\t    57\t…`，外层行号比文件行号多 1。
- read_file 有自己的分页（默认 200 行、最多 2000 行、每行最多 2000 字），但不按 token 封顶；超出后由 [`context_manager.go`](../internal/agent/context_manager.go) 第 127 行统一转存，[`outputs.go`](../internal/agent/outputs.go) 再加一层行号。

### R20 bash 的描述与工作区提示词不一致

- 工具描述（`sandbox.go` 第 103 行）一直是没有工作区时的版本：“/workspace……用户上传的都在”。有工作区时提示词写的是上传的文件在 `/scratch`。
- 两处都说可以写 `/tmp`，没说每次调用的 `/tmp` 都是空的；模型在第 96 次请求的推理里写着 “/tmp is empty?!”，又花了一次请求确认。

### R21 存档随请求数平方增长

- 每次请求的请求体整份写进存档，最后几次每次约 700 KB；这段对话 39.5 MB，`.data/runs` 合计 210 MB。打开对话、归因重建都要读完整个文件。
- 页面已经能还原增量格式（`index.html` 里的 `body.append`），但只有导入对话时用（[`internal/observer/import.go`](../internal/observer/import.go) 第 90 行），实时代理写的仍是整份。

### 其他

- everything 演示 server 把调试输出（整段字节数组）打进运行日志，在编码对话里占着 6 个工具；calculator 两个来源重复注册。仍是 R8：能力中心的设置，由用户决定。
- 自查阶段 16 次请求里，4 次在找建账号为什么失败：脚本把 `-add-user` 的报错丢进了 `/dev/null`，密码少于 8 位。这是模型写脚本的问题，不改 harness。

### 做得对的地方

- 先读后改拦下了 2 次没读就改；D24 提醒发现了读过之后的改动；`markPrivate` 的来源标记照常工作。
- 缓存命中 98.4%，请求前缀在整轮里保持稳定。
- 额度用完后检查点完整，可以从第 104 次请求处续跑。

## 3. 优化建议

按收益与代价排序。“现在可做”的几项都是改一处、不加新层；其余对应到已有计划。验证沿用项目约定：真实模型、真实工具执行和终端轨迹，不写测试文件。

| 编号 | 做什么 | 对应 | 放在哪 | 代价 |
| --- | --- | --- | --- | --- |
| O11 | 模型请求失败时保留上游的 `error.code` 与截短的 `message`（保留“模型API返回HTTP”前缀，`llm.go` 第 327 行按它分类）；额度用完（`AccountQuotaExceeded`、`allocated quota exceeded` 一类）单独归类，不重试，把重置时间放进 `turn/completed` 的 error，观测台照常显示 | R13 | 报错原文现在可做；分类随 D33（O1）一起，O1 的 429 重试只针对限流 | 小 |
| O12 | 不正常结束（model_error、预算用完、超时）时，harness 自己跑一次收尾检查（复用 Stop hook 的命令），结果写进 `turn/completed` 与检查点，不再请求模型 | R13 | D31（“预算耗尽时由 harness 自己跑最后的检查”扩到 model_error） | 小 |
| O13 | 轮内清理旧工具结果：过阈值后保留最近 N 个，更早的换成“已清理 + read_output”占位；每次至少清一大块，一次破缓存换一段长期收益；可选连同过长的工具参数一起清。做法与参数参照 Anthropic 的 context editing（trigger、keep、clear_at_least、clear_tool_inputs） | R14 | D37 上下文策略消融，与 R2 的跨轮清理放在同一组；用这段对话第 40 次左右的请求快照分叉回放，比较总输入、缓存命中与完成情况 | 中 |
| O14 | 编码基础提示词加三句候选：互不依赖的读取放在同一次回复里一起发出；找代码用 grep、glob，看代码用 read_file（可以一次读大段）；工作区文件只用 edit_file、write_file 改，bash 用来构建、运行和检查。同时删掉“用 bash 改过的文件……”那句 | R15、R16 | D37 编码基础提示词消融，与 R12 的两条一起；看每题请求数、单工具请求占比、bash 写文件次数 | 小 |
| O15 | bash 改了工作区文件时如实说明：每次 bash 前后比对工作区文件状态（D24 已有读取状态），提醒写成“上一条 bash 改了 X、Y，这些改动不在快照里”；这些文件记进本轮的“未快照改动”，撤销时单独列出，不只报冲突 | R16 | D32（工具声明里标出“可能写工作区”）或计划外单做 | 中 |
| O16 | hook 改的文件算进这次写入：PostToolUse 跑完后重读文件，变了就更新快照的“写后”状态与 D24 的读取状态，结果里告诉模型“hook 已改写，片段如下”；这样 `gofmt.sh` 可以直接 `gofmt -w`。hook 输出的路径改成相对工作区；同一 hook 对同一文件的反馈与上次相同时只说“同上，仍未处理” | R17 | D26 的后续，计划外单做 | 中 |
| O17 | bash 超时如实回报：沙箱已经终止，返回 `timed_out`、类似 124 的退出码和已有输出，不再走“结果未知”；工具描述写明时限；可以加一个有上限的时限参数（Claude Code 的 Bash 默认 2 分钟、最多 10 分钟） | R18 | 前两点现在可做；时限参数随 D32 的“默认时限”声明与后台命令 | 小 |
| O18 | read_file 按结果预算提前停下、给出 next_offset（read_output 已经这样做），不再转存进 read_output | R19 | 现在可做 | 小 |
| O19 | bash 工具描述随有没有工作区切换，与提示词一致；写明每次调用的 `/tmp` 都是空的，要跨调用保留的放 `/scratch` | R20 | 现在可做 | 极小 |
| O20 | 代理存档写增量：同一任务的请求体只存比上一次多出来的消息（沿用页面已认识的 `append` 格式），压缩或改写历史时写整份；Go 里读请求体的几处（`server.go`、`eval.go`）共用一个还原函数 | R21 | 随 O9（只追加的会话记录）；存档变慢之前可以不做 | 中 |

不建议改的：MCP 演示 server 是设置问题（R8）；模型写的自查脚本把报错丢掉，是模型行为，交给 D36 的评测题去暴露。

## 来源

- Anthropic，[Context editing](https://platform.claude.com/docs/en/build-with-claude/context-editing)：工具结果清理的 trigger、keep、clear_at_least、clear_tool_inputs，以及清理会让缓存前缀失效、要一次清够
- Anthropic，[Claude Code checkpointing](https://code.claude.com/docs/en/checkpointing)：“Checkpointing does not track files modified by Bash commands”
- Anthropic，[Claude Code environment variables](https://code.claude.com/docs/en/env-vars)：Bash 前台命令默认时限 2 分钟（`BASH_DEFAULT_TIMEOUT_MS`），模型最多可设到 10 分钟（`BASH_MAX_TIMEOUT_MS`）
- 方舟 Coding Plan 额度的第三方资料（非官方，数字以控制台为准）：[codepick 指南](https://codepick.dev/zh/guides/ark-coding-plan-guide/)、[80aj 实测](https://www.80aj.com/2026/06/07/volcano-engine-coding-test/)、[80aj 套餐对比](https://www.80aj.com/2026/07/13/volcano-engine-coding-plan-comparison/)、[jia.je：coding plan](https://jia.je/kb/en/software/coding_plan.html)
