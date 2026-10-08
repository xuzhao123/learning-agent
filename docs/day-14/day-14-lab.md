# Day 14 项目实践：用一次真实任务串起第二周

复盘与知识串联见 [第二周复盘](../week-02-review.md)。本篇记录一次综合演练：同一个任务同时用到子 agent（D10+）、浏览器（D11，顺带复测）、Bash 沙箱（D12），中途按停止（D8），再续聊恢复（D9），最后在调用链（D13）里复盘。全部使用真实方舟模型，执行顺序由模型决定。

## 1. 怎么跑

```sh
~/workspace/jaeger/start.sh                                         # 可选：同时发给 Jaeger
OTEL_EXPORTER_OTLP_ENDPOINT=http://127.0.0.1:4318 go run . observe  # 新对话勾选“子 agent”“浏览器”“Bash 沙箱”
```

问题原文：

> 帮我做个小调研。请并行派两个子 agent：A 用浏览器打开 https://go.dev/doc/devel/release ，找出 Go 最新稳定版的版本号和发布日期；B 用浏览器打开 https://pkg.go.dev/github.com/chromedp/chromedp ，找出 chromedp 最新版本号和发布日期。拿到两个结果后，你自己在 Bash 沙箱里用 date 命令算出这两个发布日期相差多少天，最后用一段话汇总。

两个子 agent 开始打开网页后按一次 ■ 停止；进程退出后续聊“刚才被我中断了，请接着完成原来的任务。”

## 2. 第一轮：停止

```text
00.0s  start → 父 agent 写检查点
10.0s  并行 spawn_agent ×2（子任务 …-sub-9bd11a0aac62、…-sub-6b9e77584df6），各自写检查点
13–15s 两个子 agent 各开一个标签页 open_page
16.0s  ■ 停止：SIGINT → 父 agent 取消 ctx → RunChild 向子进程发 SIGINT
       子 agent：open_page 回填 unknown → Termination: cancelled → 检查点 stopped
       父 agent：两个 spawn_agent 回填 unknown → Termination: cancelled → exit 1
```

从按下停止到父进程退出不到 1 秒，没有用到 RunChild 的 10 秒强制等待。调用链（13 个 span）把这条取消路径完整画了出来：

```text
interaction                         error  agent_exit
└ invoke_agent                      error  cancelled
  ├ execute_tool spawn_agent        error  unknown     ← 子任务 …9bd11a0aac62
  │ └ invoke_agent（子进程）         error  cancelled
  │   └ execute_tool open_page      error  unknown
  │     └ browser.tab               error  browser_error
  └ execute_tool spawn_agent        error  unknown     ← 子任务 …6b9e77584df6
    └ …（同上）
```

“结果未知”从最内层的标签页一路向上传到父 agent，每一层都如实标记，没有被写成“失败”或“未执行”。

## 3. 第二轮：续聊

观测台从父 agent 的检查点取出上下文（包括两条“结果未知”），启动新的 agent 进程。模型决定重新派发两个子 agent：

| | 子 agent B（chromedp） | 子 agent A（Go 版本） |
| --- | --- | --- |
| 过程 | 打开 pkg.go.dev 一次，读到 v0.20.1、Oct 5, 2026 | 打开发布页后，用 `find` 依次搜 go1.27、go1.28、beta、rc，共 5 次 open_page |
| 结局 | `no_tool_calls`，12.5s | `max_steps`（6 次请求用完），62.3s，spawn_agent 记为 permanent 失败、不重试 |

父 agent 看到 A 失败，自己打开发布页，读到 go1.27.1（2026-09-01），再在沙箱里执行：

```sh
echo $(( ($(date -d 2026-10-05 +%s) - $(date -d 2026-09-01 +%s)) / 86400 ))   # → 34，exit=0，58ms
```

最终回答：Go 最新稳定版 go1.27.1（2026-09-01），chromedp v0.20.1（2026-10-05），相差 34 天。对照两个网页核实无误。第二轮的 trace 有 35 个 span，Jaeger 收到了同样的数据。

## 4. 发现的问题

### 4.1 观测台续聊后，子任务没有续跑而是重跑

两轮的子任务 ID：

```text
第一轮  6237db09-…-sub-9bd11a0aac62   6237db09-…-sub-6b9e77584df6
第二轮  b7f3ba89-…-sub-9bd11a0aac62   b7f3ba89-…-sub-6b9e77584df6
```

哈希后缀完全相同：模型原样照抄了 task。但子任务 ID 的前缀是父 agent 的检查点 ID，而观测台的续聊会启动一个**新检查点 ID** 的进程（命令行 `-resume` 才沿用原 ID），所以两个子任务都从头跑了，第一轮的子任务检查点被搁置。[Q15](../deep-questions.md#q15) 原来的结论“原样照抄就能命中”只对 `-resume` 成立，已补充。代价和 Q15 分析的一致：多花时间和 token，不会把错误的结论交给父 agent。

**修复**，分两步（第 1 步后来被替换：沿用并覆盖旧检查点会让一个 ID 对应两个问题，见 [Q21](../deep-questions.md#q21)；现在每轮用新 ID，以 `continues` 串成续接链，task_id 可以引用链上任何一轮的子任务）：

1. 观测台在停止后续聊时传 `-task-id <上一轮的检查点ID>`，agent 允许 `-history-stdin` 覆盖一份未完成的检查点。父 ID 不变，子任务 ID 的前缀也就不变。正常结束的轮次续聊仍用新 ID：新问题里出现相同的 task 应当重新执行，不能拿旧结论回答。
2. 只修第一步还不够。之后两次真实运行里，模型续聊时都**改写**了 task（哈希后缀变了），子任务照样重跑。于是按 Q15 的第一条改进：`spawn_agent` 的失败和“结果未知”里写明 `task_id`（被取消时，执行器把子 agent 的收尾信息一并带上）；`spawn_agent` 新增可选参数 `task_id`，传入时从检查点取原任务，只接受本任务派出的子任务。

修复后的真实运行：停止后模型在“结果未知”里看到两个 `task_id`，续聊时直接调用 `spawn_agent(task_id=…)`；两个子 agent 都从检查点续跑（`completed_rounds=1`，已用的 1 次请求计入预算），最终答案正确。

### 4.2 子 agent 在同一个页面上反复搜索

子 agent A 的 5 次 open_page 全部成功，失败的原因是策略：发布页正文 6.2 万字被截断，最新小版本 go1.27.1 不在开头，模型换关键词逐个试，每次 `find` 都重新加载整页，6 次请求预算很快用完。这与 [Day 11 实践](../day-11/day-11-lab.md) 3.7 的现象相同，说明是稳定可复现的问题，不是偶然：子 agent 的预算、任务说明的写法和无状态的页面工具共同决定了它。父 agent 接手完成了任务，整体结果正确。

### 4.3 D11 复测：web_search 不可用

去掉 UA 伪装后用方舟做端到端复测：

- `open_page`：演练第二轮 7 次调用全部成功（go.dev、pkg.go.dev；第一轮的 2 次因停止回填为“结果未知”），命令行另一次打开 GitHub 仓库页也成功；
- `web_search`：必应搜索每次都报 `Inspected target navigated or closed (-32000)`，执行器按暂时故障重试 3 次后放弃。用 D13 之前的版本（提交 `5592b88`）复现了同样的错误，与调用链打点无关。必应地址本身返回 200、没有服务端跳转，是页面在无头浏览器里读取时被脚本跳转。

按项目约定不绕过反爬。用无头浏览器观察：必应约 2 秒后把地址改成带 `&rdr=1` 的同一个搜索页，跳转后的页面上仍然没有结果条目（`li.b_algo` 一直为 0），所以等待或重试都没用。

**修复**：读取途中遇到页面跳转时，在同一个标签页里等新页面就绪再读一次；仍然失败就标为不可重试，告诉模型“页面被自己的脚本跳转、读不到结果，可以改用 open_page”。修复后只尝试 1 次，模型随即用 `open_page` 打开 GitHub 仓库页答对。web_search 仍然拿不到必应的结果，正式的搜索 API 留作后续改进。

## 5. 从调用链读这次运行

只看终端或对话流，很难回答下面这些问题；调用链里可以直接读出来：

| 问题 | 在调用链里看哪里 |
| --- | --- |
| 停止信号传到了哪几层，各层怎样收尾 | 第一轮所有 span 的状态：cancelled（agent）与 unknown（工具） |
| 第二轮 87.7s 花在哪 | 子 agent A 的 62.3s（6 次 chat + 5 次 open_page），其中真正打开网页只有约 9.6s |
| 两个子 agent 是否真的并行 | 两个 `spawn_agent` 的起点相同、时间条重叠 |
| 子 agent 失败后父 agent 做了什么 | spawn_agent 的 `no_retry` 事件，下一个 Step（step=2）出现父 agent 自己的 open_page，step=3 是 bash |
| 沙箱有没有生效 | `sandbox.exec` 带 `lock_acquired` 事件，`sandbox.cgroup=true`，退出码 0 |

## 6. 小结

| 周内功能 | 这次演练里的表现 |
| --- | --- |
| D8 取消与两段式停止 | 一次 SIGINT，三个进程在 1 秒内各自回填并退出 |
| D9 检查点 | 父、子 agent 都留下 stopped 检查点；续聊从父检查点恢复，包括“结果未知” |
| D10+ 子 agent | 并行、独立上下文、预算耗尽时如实失败；续聊后重跑的问题已修复，模型用 task_id 续跑子任务（4.1） |
| D11 浏览器 | open_page 端到端可用；web_search 被必应的页面跳转阻断，已改为不白白重试（4.3） |
| D12 沙箱 | date 计算在 cgroup + bubblewrap 里执行，58ms |
| D13 调用链 | 两轮共 48 个 span，取消路径、并行、重试判断与耗时分布都可读 |
