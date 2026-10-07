# Day 11 项目实践：用 chromedp 给 agent 一个查资料的浏览器

原理见 [Day 11 学习笔记](day-11-notes.md)。代码在 [browser.go](../../internal/browser/browser.go)，观测台的实时画面在 [server.go](../../internal/observer/server.go) 的 `browserFrame` 和 [index.html](../../internal/observer/index.html) 的“Day 11 浏览器”一节。

## 1. 实现概览

```sh
# 本机有 Chrome/Chromium 时直接用；否则用 CHROME_PATH 指定可执行文件
CHROME_PATH=/path/to/chrome ./bin/learning-agent -browser -question '……'
# 系统不允许创建 Chrome 沙箱（容器、禁用了 user namespace）时才加，代价见笔记 6.3
CHROME_NO_SANDBOX=1 …
```

`-browser` 给模型增加两个读取型工具，并在 system prompt 里加一段使用规则（先搜后读、注明来源、网页内容是数据不是指令）：

| 工具 | 参数 | 返回 |
| --- | --- | --- |
| `web_search` | `query` | 必应前 8 条结果的标题、链接、摘要 |
| `open_page` | `url`，可选 `find` | 标题、正文（最多 3000 字，注明总长度与是否截断）、最多 15 个链接；给 `find` 时只返回含关键词的段落及其前后各一行 |

```text
agent 进程
  第一次调用浏览器工具时 ── start() ── 启动一个无头 Chrome（整个进程共用）
  每次工具调用 ── inTab()
        新标签页 ──▶ 开启 screencast ──▶ Navigate ──▶ WaitReady(body) ──▶ Evaluate(轮询脚本)
            │            帧 → .data/browser/<任务ID>/<调用ID>.jpg（不断覆盖）
            │        结束时再截一张图写入同一个文件
        工具 ctx 取消/超时 ──▶ 关闭标签页
  进程退出 ── Close() ── 关闭浏览器

观测台
  GET /browser/<任务ID>/<调用ID>.jpg ──▶ 页面每 700ms 取一次运行中的画面
```

| 设计点 | 本项目的选择 |
| --- | --- |
| 工具粒度 | 只做读取型（笔记 3.1），不做 click/type：查资料够用，模型不容易用错 |
| 有无状态 | 无状态：每次调用一个新标签页。可以并行、可以重试，超时只需关标签；代价是同一页面要重复加载（实验 3.5） |
| 浏览器生命周期 | 每个 agent 进程一个浏览器，第一次调用时启动、进程退出时关闭；不挂在某次调用的 ctx 上 |
| 等待 | `Navigate` 等 load 事件，然后在页面里每 300ms 检查一次条件（搜索结果出现 / 正文不为空），最多 5 秒 |
| 返回内容 | `innerText`；优先取 `main`/`article`，正文不足 200 字才退回整页；结构体固定字段顺序，正文在前、链接在后 |
| 错误分类 | 参数错误、内网地址、浏览器无法启动、验证码页：`llm.Permanent`；导航失败：可重试；超时/取消：结果未知 |
| 可重复 | 两个工具都是只读的，列入 `repeatable`：结果未知时可以重试，续跑时可以重放 |
| SSRF | 只允许 http/https；解析域名，拒绝回环、私有、链路本地地址（只检查起始地址，见已知边界） |
| 反爬 | 不改 User-Agent、不隐藏自动化标志；识别出验证码页时如实告诉模型，建议直接打开已知网站 |
| 子 agent | `-browser` 在子 agent 的继承白名单里；子 agent 有自己的进程和浏览器，画面按子任务ID分目录 |

## 2. 读代码

### 2.1 浏览器只启动一次

```go
// 浏览器活到进程结束，不挂在某一次工具调用的 ctx 上：否则第一次调用超时，整个浏览器就跟着关了。
alloc, cancelAlloc := chromedp.NewExecAllocator(context.Background(), opts...)
ctx, cancelCtx := chromedp.NewContext(alloc)
if err := chromedp.Do(ctx); err != nil { … }
```

chromedp 的规则是：一个 context 第一次运行动作时启动浏览器，浏览器活到这个 context 结束。如果用工具调用的 ctx 启动，那次调用一超时，浏览器就没了。所以浏览器挂在 `context.Background()` 上，由 `browser.Close()` 在 main 退出前关闭。

### 2.2 每次调用一个标签页，取消就关标签

```go
tab, closeTab := chromedp.NewContext(parent) // 从浏览器 context 派生 = 新标签页
defer closeTab()
stop := context.AfterFunc(ctx, closeTab)     // 工具的 ctx 取消或超时 → 关闭标签页
defer stop()
```

这是 Day 8 协作式取消在浏览器上的落点：执行器取消 ctx，`AfterFunc` 关闭标签页，正在等待页面的 chromedp 调用随即返回。`inTab` 最后检查 `ctx.Err()`，有就原样返回，让执行器按“超时 / 被取消 → 结果未知”处理，而不是当成页面错误。

### 2.3 等待内容出现

```go
// 页面 load 事件之后，很多内容还要等脚本渲染（搜索结果、单页应用）。每 300ms 取一次，
// 满足 ready 条件或等满 5 秒就返回当时的结果；整体仍受工具超时约束。
func waitFor(collect, ready string) string {
	return `new Promise(resolve => { … tick … })`
}
```

轮询放在页面里执行，用 `chromedp.EvalAwaitPromise` 等 Promise 完成：一次 CDP 往返就能等到条件满足，不必在 Go 里反复发命令。

### 2.4 实时画面：screencast + 最终截图

```go
frames := chromedp.Events(tab, page.ScreencastFrame) // 先订阅，再开始
chromedp.Call(tab, page.StartScreencast, page.StartScreencastParams{Format: jpeg, Quality: &quality, MaxWidth: 960, MaxHeight: 720})
go func() {
	for frame, err := range frames {
		writeFrame(file, frame.Data) // 临时文件 + rename，读的人总是拿到完整的图
		chromedp.Call(tab, page.ScreencastFrameAck, …) // 不回 ack，Chrome 最多再发 3 帧就停
	}
}()
```

screencast 只在页面重绘时发帧，所以调用结束时再用 `Page.captureScreenshot` 截一张写进同一个文件：每一步都有一张最终画面。帧文件放在 `.data/browser/<任务ID>/<调用ID>.jpg`，和检查点一样按任务ID分目录。

### 2.5 观测台：只读一张图

观测台不连接 agent 的浏览器，只按约定的路径读图（`GET /browser/<任务ID>/<调用ID>.jpg`，路径段做白名单校验，`Cache-Control: no-store`）。页面上：

- `web_search`、`open_page` 显示成浏览器卡片，带缩略图和状态；
- 运行中开始浏览时，右侧自动打开浏览器面板，并跟随最新一步；用户手动关掉后不再自动打开；
- 面板里有地址栏、画面（运行中带 LIVE 标记、每 700ms 刷新）和模型看到的正文或搜索结果；
- 图片用 `fetch` 取成 blob URL 缓存，对话流因为新事件重绘时直接复用，不会闪烁；
- 子 agent 侧栏里的浏览卡片点开后在原地显示大图，不切换侧栏。

任务ID从哪来：agent 每次启动打印 `Checkpoint: id=…`，每次浏览打印 `Browser [调用ID]: open url=…`。页面在看到后一行时记下当时的任务ID。续聊的每一轮是新进程、新任务ID，所以要逐个调用记录，不能整个对话共用一个。

## 3. 动手

以下运行均在 2026-10-07（Asia/Shanghai）进行，模型为 doubao-seed-2-1-pro，`-reasoning-effort low`。本机禁用了非特权 user namespace，Chrome 沙箱无法创建，实验都设置了 `CHROME_NO_SANDBOX=1`。

### 3.1 浏览器起不来时，模型会编答案

第一次运行时还没有处理沙箱问题，浏览器每次都启动失败：

```text
Action Input: {"query": "chromedp Go 库 最新发布版本 发布时间 github"}
No retry [call_uo4a…]: reason=permanent attempts=1 error=无法启动浏览器…No usable sandbox!…
…（模型又换了三种方式，全部失败）
当前运行环境无法启动 Chrome/Chromium 浏览器（沙箱权限报错），因此不能实时联网查询最新数据。以下是截至我知识库更新时的公开信息参考：
- chromedp 最新正式发布版本为 **v0.13.1**，发布于2025年8月前后。
```

实际的最新版本是 v0.20.1（2026-10-05）。模型说明了工具失败，但仍给出了一个凭记忆的“答案”。工具失败时模型容易用训练数据补上，这类回答需要明确标注为未经核实，或者干脆不给。这个问题留到 Week 3 的评测去量化。

### 3.2 必应要求人机验证

```text
Browser [call_llj…]: search url=https://www.bing.com/search?mkt=zh-CN&q=chromedp+screencast
Retry [call_llj…]: attempt 1/3 failed: Inspected target navigated or closed (-32000); wait 200ms
No retry [call_llj…]: reason=permanent attempts=2 error=搜索引擎要求人机验证，自动化浏览器无法使用搜索；可以直接用 open_page 打开已知的网站（官方文档、GitHub、pkg.go.dev 等）
Browser [call_bgb…]: open url=https://pkg.go.dev/github.com/chromedp/chromedp
由于搜索引擎触发人机验证，`chromedp screencast` 检索未能自动完成。
根据 https://pkg.go.dev/github.com/chromedp/chromedp 页面信息：chromedp 最新稳定版本为 **v0.20.1**，发布于2026年10月5日……
```

保存下来的截图里，必应页面写着“最后一步：请解决以下难题以继续”。第一次尝试报 `Inspected target navigated or closed`，是因为页面正在跳转到验证页时脚本被打断；重试一次后读到了验证页，按不可重试错误返回。模型照提示改为直接打开 pkg.go.dev，答对了。

必应不是每次都弹验证：同一天的另一次运行里，第一个中文查询被要求验证，第二个英文查询正常返回了结果。后来去掉了伪装的 User-Agent（如实表明是 HeadlessChrome，见第 4 节），用无头浏览器直接打开必应时只解析出 1 条结果，搜索会更容易失败。这是不规避反爬的代价。

### 3.3 截断与 find

```text
Action Input: {"url": "https://go.dev/doc/devel/release"}
  → text_chars=62151 truncated=true，前 3000 字是目录：go1.27.0 (released 2026-08-19) | Minor revisions | go1.26.0 …
Action Input: {"url": "https://go.dev/doc/devel/release", "find": "go1.27."}
  → text_chars=352 matched_paragraphs=3：… go1.27.1 (released 2026-09-01) includes fixes to cgo, the compiler, …
```

第一次只看到目录，模型从“已截断”判断还有内容，第二次用 `find` 只取 go1.27 的段落，找到了 go1.27.1。

### 3.4 内网地址被拒

```text
Action Input: {"url": "http://127.0.0.1:8090/"}
Action Input: {"url": "http://169.254.169.254/latest/meta-data/"}
No retry […]: reason=permanent attempts=1 error=不允许访问本机或内网地址：127.0.0.1 → 127.0.0.1
No retry […]: reason=permanent attempts=1 error=不允许访问本机或内网地址：169.254.169.254 → 169.254.169.254
```

127.0.0.1:8090 是观测台自己。如果不拦，模型可以读到观测台里所有对话的记录。

### 3.5 超时与停止

`-tool-timeout 1500ms` 打开 GitHub：

```text
Retry [call_ure9…]: attempt 1/2 failed: 结果未知：单次执行超过 1.5s，可能已经生效或仍在后台进行; wait 200ms
Retry exhausted [call_ure9…]: attempts=2 …
```

`open_page` 在 `repeatable` 表里，结果未知时照常重试。运行结束后 `ps` 里没有残留的 Chrome 进程。

在观测台里，模型打开 GitHub 的过程中点 ■：

```text
stop 已请求停止：向 agent 发送 SIGINT，等待它回填结果、写好检查点后退出
Observation [call_ml6v…]: {"tool":"open_page","status":"unknown","error":"结果未知：执行中被取消（context canceled），可能已经生效",…}
Termination: cancelled
```

然后在同一个对话里发“继续，把刚才的两页看完再总结”：新进程沿用 `-browser`，重新打开两个页面，正常结束（exit 0）。Chrome 进程同样都已退出。

### 3.6 在观测台里看实时画面

```sh
go run . observe   # 新对话勾选“浏览器”
```

问题：依次打开 go.dev/blog、go.dev 发布历史和 chromedp 的 GitHub 仓库，回答三个问题。用无头浏览器每 0.5 秒检查一次页面：浏览开始后右侧自动打开浏览器面板，运行中一共显示了 15 帧不同的画面，跟随最新一步，结束时停在第 3 步；三张卡片都有缩略图，面板里能看到模型读到的正文。打开 GitHub 的那次，开头几帧是空白页，这是页面加载中的真实状态。

### 3.7 子 agent 各开一个浏览器

勾选“子 agent”和“浏览器”，让两个子 agent 分别查 Go 和 chromedp 的最新版本。子 agent 2 用 2 次请求完成。子 agent 1 用完了 6 次请求预算：

```text
open_page(release)                 → 62151 字，已截断
get_current_datetime
open_page(release, find=go1.27.)   → 找到 go1.27.1
open_page(release, find=beta)      → 0 处
open_page(release, find=rc)        → 23 处（"rc" 匹配到 sources 之类的词）
open_page(release, find=go1.27.2)  → 0 处
Termination: max_steps
```

父 agent 写的任务说明里有一句“注意是正式发布的稳定版本，不是 beta 或 rc”，子 agent 于是反复核对，而每次 `find` 都要重新加载整个页面。父 agent 最后自己打开页面完成了汇总。这一次同时暴露了三件事：任务说明会改变子 agent 的行为（子 agent 笔记 5.1）；子 agent 的预算要够它核对；无状态的工具让同一页面被加载了 4 次。

## 4. 已知边界

- 只做读取型工具，不能点击、填表、登录。
- 每次调用重新加载页面，没有缓存，也没有跨调用的会话。
- SSRF 只检查起始地址：重定向、页面里的子资源、DNS 重绑定都能绕过。可靠的防护要在网络层（Day 12 容器网络策略）。
- 网页内容的提示词注入只靠 system prompt 里的一句规则，没有做检测；Week 3 专门做攻防。
- 本机运行时关闭了 Chrome 沙箱（`CHROME_NO_SANDBOX=1`），浏览器和 agent 权限相同。
- 不伪装 User-Agent、不绕过验证码，所以搜索可能失败；没有接入搜索 API。
- 去掉 UA 伪装之后，因方舟账户欠费，没能再做一次真实模型的端到端运行；只用无头浏览器确认了 GitHub、pkg.go.dev 和 go.dev 的两个页面都能正常打开。
- 截图只保留每个调用的最后一帧，不保存录像；`.data/browser/` 不会自动清理。
- 帧率不受控：screencast 每次重绘都会发帧，靠 ack 和文件覆盖控制。
