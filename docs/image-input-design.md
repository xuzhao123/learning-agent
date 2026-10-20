# 图片输入设计：观测台附图与 view_image

> 状态（2026-10-20）：第一步的 **agent 侧已实现**（4.1–4.4、4.6，以及 4.5 里的 `-image` 与 `turn/start`），实现与验证见第 9 节；观测台接口、代理存档换引用、页面、`view_image` 与 4.8 的私有标记还没做。主要参考 Codex（[openai/codex](https://github.com/openai/codex) 主干 `e776b27`，2026-10-20）的源码，Claude Code 只有文档可查。模型能力与限制来自当天对方舟 Coding Plan 与 DeepSeek 官方接口的真实调用。
>
> 对应 harness 能力：H02 模型客户端（多模态输入、按模型能力适配）、H04 上下文预算（图片计数与压缩）、H06/H09 工具（`view_image`）、H12 不可信内容、H18 会话记录（附件怎么存）。见 [Harness 能力对照](../JOB_REQUIREMENTS.md#harness-能力对照)。

## 1. 目标与不做的事

分两步：

1. **用户给模型看图**：观测台输入框附图（按钮、粘贴、拖放），命令行 `-image 路径`。
2. **模型自己看图**：`view_image` 工具，读工作区、沙箱和浏览器截图里的图片。

不做：生成图片；音频、视频、PDF；远程图片 URL（Codex 也不接收）；“原图”精度档（Codex 的 `detail: original` 与 unified image budget，只对 OpenAI 自家模型有意义）；方舟 Files API（只有方舟有，DeepSeek 用不了）。

## 2. 现状与约束

### 2.1 代码现状

| 位置 | 现状 | 对图片的影响 |
| --- | --- | --- |
| 观测台“上传文件”（[files.go](../internal/observer/files.go)） | 只把文件放进 Bash 沙箱的 `/workspace`；要开 bash、第一轮之后、运行中不行 | 文件进得去，模型看不到像素 |
| `llm.Message.Content` | 字符串，代码里约 100 处直接读写它 | 放不下图片，需要另加字段 |
| `turn/start` | 只有 `question`、`history`、`continues` | 没有地方带图 |
| token 估算（`llm.MessageTokens`） | 消息 JSON 字节数 ÷ 4 | 1 MB 图片转 base64 后被估成 30 多万 token，一张图就触发压缩 |
| 观测台代理 | 每次模型请求的请求体原样存进对话存档 | 图片随每次请求重复写盘，见第 6 节 |
| 会话导入（[transcript.go](../internal/transcript/transcript.go)） | 图片块显示成“[图片]” | 导入时丢图，可以以后再补 |

### 2.2 模型能力（2026-10-20 实测）

请求方式：同一张 640×360 的 PNG（“7 3 9”加一个红圆），`content` 数组里放 `image_url`（data URL）和一句问题。

| 结果 | 模型 |
| --- | --- |
| 能看图，全部答对 | doubao-seed-2.1-pro、2.1-lite、evolving、2.0-mini；kimi-k3、k2.8-preview、k2.7-code；glm-5.3-flash；deepseek-v4.1-flash；minimax-m3；ark-code-latest |
| 报错 `InvalidParameter: Model only support text input` | glm-5.3 |
| **返回 200，回答“看不到图”**（悄悄丢图） | deepseek-v4-pro、deepseek-v4-flash（方舟套餐） |
| 能看图 | deepseek-flash（DeepSeek 官方接口） |
| 工具结果（`role: tool` 的 content 数组）里带图也能看到 | doubao-seed-2.1-pro、kimi-k3、deepseek-v4.1-flash、minimax-m3、glm-5.3-flash（5/5，方舟套餐） |

单张图的输入 token：豆包 Seed 2.x 约 1280（文档：detail 默认 `high`，单图固定 1280）；kimi、glm-flash、deepseek-v4.1、minimax 实测 200–500。

### 2.3 供应商限制

- 方舟（[图片理解](https://ark.volcengine.com/region:cn-beijing/docs/image-understanding.md)）：base64 单张 < 10 MB，请求体 < 64 MB；像素 196 到 3600 万；格式 JPEG、PNG、GIF、WebP、BMP、TIFF 等；Chat API 无状态，“每次请求时都需传入该图片信息”。
- Claude API（[Vision](https://platform.claude.com/docs/en/build-with-claude/vision)，作对照）：单张 ≤ 10 MB、≤ 8000×8000；超过 20 张时每张长边限 2000 px；JPEG、PNG、GIF、WebP，动图只取第一帧；官方建议多轮对话用 `file_id`，避免请求体随历史增长。

## 3. 参考：Codex 与 Claude Code

| 环节 | Codex（源码） | Claude Code（文档与本机会话记录） |
| --- | --- | --- |
| 加图 | 终端粘贴（剪贴板 → 临时 PNG → 路径）、拖放、`-i/--image`；输入框里是 `[Image #1]` 占位 | 拖放、`Ctrl+V`（Windows/WSL `Alt+V`）、在消息里写路径；回答里的 `[Image #1]` 可点开 |
| 协议 | app-server `UserInput` 数组：`text`、`image`（data URL 或 `fileId`）、`localImage`（路径），可带 `detail` | 无公开协议 |
| 发给模型的形状 | `<image name=[Image #1] path="…">`、图片、`</image>`，再接用户文字 | Messages API 的 `image` 块；官方建议图在文字前，多图加 “Image 1:” 标签 |
| 预处理 | 只收 PNG/JPEG/GIF/WebP；高清档长边 ≤ 2048 且 32 px 块 ≤ 2500；不需要缩放的 PNG/JPEG/WebP 原样保留字节；缩放后告诉模型原尺寸 → 新尺寸；失败换成文字占位，这一轮继续 | 大图缩放、重新压缩到模型限制内；看不清就让它裁剪后再读 |
| 模型看图工具 | `view_image(path)`：先查模型能否看图；经沙箱文件系统读；解码校验；作为工具输出里的图片返回；日志不记图片 | 复用 `Read`：读 PNG/JPEG/GIF/WebP 返回可见内容 |
| 不能看图的模型 | 模型信息里的 `input_modalities`；每次发请求前把历史和工具结果里的图换成 “image content omitted because you do not support image input” | 只用 Claude 模型，不需要 |
| token 估算 | 每张图固定 7373 字节（约 1844 token），不看 base64 长度 | 未公开 |
| 压缩 | 不保留原文的用户消息只留文字，图片丢弃 | 未公开 |
| 存储 | `AttachmentStore` 接口；默认“原样内联”，rollout 里存 data URL；换成文件实现后请求与 rollout 都只放 `file_id` | 会话 JSONL 里 base64 内联；Read 的图在 `tool_result` 与 `toolUseResult` 各存一份 |

Codex 源码位置：[image_preparation.rs](https://github.com/openai/codex/blob/main/codex-rs/core/src/image_preparation.rs)、[utils/image](https://github.com/openai/codex/blob/main/codex-rs/utils/image/src/lib.rs)、[view_image.rs](https://github.com/openai/codex/blob/main/codex-rs/core/src/tools/handlers/view_image.rs)、[normalize.rs](https://github.com/openai/codex/blob/main/codex-rs/core/src/context_manager/normalize.rs)（`strip_images_when_unsupported`）、[history.rs](https://github.com/openai/codex/blob/main/codex-rs/core/src/context_manager/history.rs)（`RESIZED_IMAGE_BYTES_ESTIMATE`）、[attachment-store](https://github.com/openai/codex/blob/main/codex-rs/attachment-store/src/lib.rs)、[image_rollout.rs](https://github.com/openai/codex/blob/main/codex-rs/core/tests/suite/image_rollout.rs)。

## 4. 设计：以 Codex 为主

原则：**语义照 Codex，存储按本项目的存档方式改**。每一项都标出对应的 Codex 做法，偏离的在第 5 节说明原因。

### 4.1 附件存储与引用（对应 `AttachmentStore` 的文件实现）

- 文件按内容寻址：`.data/[tenants/<租户>/]attachments/<sha256>.<扩展名>`，路径由 `internal/tenant` 决定，不提交。同样的字节只存一份。
- 引用写成 `attachment://<sha256>.<扩展名>`，放在 `image_url.url` 里。消息、检查点、对话存档、结果日志里只有引用，形状和发给模型的请求一样，只是 url 不同。
- 只有 agent 在 `callModel` 真正发请求时，才把引用展开成 data URL。

### 4.2 消息结构

`llm.Message` 增加一个字段，`Content` 不动：

```go
type Image struct {
	Ref                 string // attachment://<sha256>.png
	Label, Path         string // [Image #1]；给模型看的来源（文件名或路径）
	Width, Height       int    // 发给模型的尺寸
	SrcWidth, SrcHeight int    // 原尺寸；相同表示没缩放
}

type Message struct {
	// ……现有字段
	Images []Image `json:"-"` // 由 MarshalJSON / UnmarshalJSON 与 content 数组互转
}
```

- **序列化**：有图片时 `content` 写成数组，顺序固定：每张图依次是开标签、`image_url`、闭标签，最后是 `Content` 文字。图在文字前，符合 Claude 的建议和 Codex 粘贴图片的顺序。没有图片时照旧是字符串，旧存档和旧检查点不受影响。
- **开标签**照 Codex，并把缩放信息写进去：`<image name=[Image #1] path="screenshot.png" size="1536x2048" original="3024x4032">`；没缩放时不写 `original`。
- **反序列化**：同时接受字符串和数组。数组里认开、闭标签（照 Codex 的 `is_local_image_open_tag_text`），把图片还原进 `Images`，其余文字拼回 `Content`。续聊从存档读回请求时走的就是这条路径。
- `llm.FromUser` 和记忆只看 `Content`，用户原话的定义不变。

### 4.3 预处理（照 `image_preparation.rs` 与 `utils/image`）

一个函数 `llm.PrepareImage(bytes, path) (Image, error)`，命令行、观测台、`view_image` 三个入口共用：

1. 按文件头识别格式，只收 PNG、JPEG、GIF、WebP（WebP 用已在依赖里的 `golang.org/x/image/webp` 解码）；先用 `image.DecodeConfig` 读尺寸，超过 3600 万像素直接拒绝（方舟上限，也防解压炸弹）。
2. 缩放规则照 Codex 高清档：长边 ≤ 2048，且 32×32 块数 ≤ 2500；用 `golang.org/x/image/draw` 等比缩放。
3. 不需要缩放且是 PNG、JPEG、WebP 时，原样保留字节；否则重新编码：原来是 JPEG 就编码成 JPEG（质量 85，与 Codex 相同），其余编码成 PNG（Go 没有 WebP 编码器）；GIF 只取第一帧。
4. 写进附件存储，返回 `Image`。
5. 任何一步失败都不让这一轮失败：换成一段文字占位，如“图片 screenshot.png 无法处理：格式不支持（只支持 PNG、JPEG、GIF、WebP）”，与 Codex 的 placeholder 做法相同。

观测台上传时只校验文件头和大小（上限 20 MB），原图也按内容寻址存下来，界面显示原图；缩放统一交给 agent。这和 Codex 一样，只在 core 里处理一次。

### 4.4 模型能力（照 `input_modalities`）

- `Provider.Models` 从 `[]string` 改成带能力的条目：`[]Model{{Name: "kimi-k3", Vision: true}, …}`，按 2.2 节实测填写。
- `ark-code-latest` 标为不能看图：Auto 模式可能路由到不能看图的模型，导致悄悄丢图。
- 三道防线：
  1. 界面：所选模型不能看图时，附图按钮禁用并说明原因（`/models` 返回能力）。
  2. 观测台：`POST /runs` 带图而模型不能看图时返回 400。
  3. agent 发请求前：照 Codex `strip_images_when_unsupported`，把历史和工具结果里的图换成“图片已省略：当前模型不支持图片输入”。前两道挡住的是用户附图；这一道兜住续跑、回放和命令行。
- 不能看图的模型不提供 `view_image`（Codex 是提供后再拒绝，我们直接不放进工具列表）。

### 4.5 协议与入口

- `turn/start` 增加 `images: [{"ref": "attachment://…", "name": "screenshot.png"}]`，对应 Codex `UserInput` 的 `image`。界面和命令行先存附件，agent 收到后对每张图跑一次预处理（缩放后会生成新的引用）。
- 用户消息的协议事件（`item/completed` 的 userMessage）带上图片列表（引用、编号、尺寸），页面据此画缩略图。B0：界面只读协议。
- 命令行：`-image 路径`，可以写多次，直接走预处理。验证全靠它和真实模型。
- 观测台：
  - `POST /attachments`：请求体是图片字节。校验文件头与大小；先写审计 `user_attach`（只记哈希），写不进审计就不收（同 D21 的上传）。返回引用。新对话的第一条消息就能用，不依赖沙箱。
  - `GET /attachments/{sha}`：按当前租户的目录读，别的租户返回 404；再按文件头确认是图片，`Content-Type` 只给 `image/png|jpeg|gif|webp`，并加 `nosniff`。SVG 从一开始就不收。
  - `POST /runs`、`POST /runs/{id}/messages` 增加 `images: [引用]`，校验都属于本租户，每条消息最多 8 张。

### 4.6 token 估算与压缩

- 估算：每张图固定按 1280 token 计（豆包 high 档的固定值；Codex 用 1844，同一量级），不看 base64 长度。首个响应回来后，照 D30 用服务端用量校正。
- 压缩：照 Codex，摘要请求里只保留文字，图片换成开标签文字（编号、文件名、尺寸都在），模型知道“这里有过这张图”；保留区（最近几组）的图照常发送。
- D30 的大输出存储只处理文字结果，图片不进 `read_output`。

### 4.7 `view_image`（第二步，照 Codex `view_image`）

- 参数：`path`。路径规则沿用 `read_file`（工作区内；开 bash 时 `/workspace` 映射到本对话沙箱目录），另外允许读本任务的浏览器截图 `.data/browser/<任务ID>/`。
- 只读工具，plan 模式可用；不能看图的模型不提供。
- 结果的 `content` 数组：观测 JSON 文字（照常带来源标记 `data_only`）、开标签、图片、闭标签。实测方舟 5 个模型都接受这种形状；DeepSeek 官方接口还没测。
- 结果日志 `.calls/` 和终端只记引用与尺寸，不记 base64（Codex 的 `log_output` 同样省略 data URL）。
- 协议发一条 `imageView` 事件，界面在工具卡片上显示缩略图。

### 4.8 安全

- 用户附的图属于私有输入：本轮标记私有来源（D20），之后的外发类工具要确认；子 agent 照现有规则继承私有标记，但不继承图片本身（`spawn_agent` 只传文字任务）。
- `view_image` 读到的图片是不可信数据：图里的文字同样可能是注入（D18–19），结果照常带来源标记。
- UserPromptSubmit hook 只拿文字，和现在一样。

## 5. 与 Codex 的差别及原因

| Codex | 本项目 | 原因 |
| --- | --- | --- |
| Responses API 的 `input_image`、`function_call_output` | Chat Completions 的 `image_url` 内容块；工具结果写成 content 数组 | 方舟与 DeepSeek 用的是 Chat Completions；工具结果带图已实测可行 |
| 默认把 data URL 内联在 rollout 里 | 内容寻址文件 + `attachment://` 引用 | 观测台代理按请求存档，内联会成倍膨胀，见第 6 节 |
| 缩放提示是单独一条 developer 消息 | 写进开标签的 `size`、`original` 属性 | 这里没有 developer 角色；D24 规定运行中的提醒走 `<system-reminder>`，放在标签里最简单 |
| `input_modalities` 来自模型元数据 | 路由表里手填，依据实测 | 只做手动路由；悄悄丢图的模型只能靠实测发现 |
| `view_image` 总是注册，处理时拒绝 | 不能看图的模型不提供 | 少一个模型用不了的工具 |
| 剪贴板 → 临时文件 → 路径 | 浏览器上传 → 附件存储 → 引用 | 界面是网页 |
| original 精度档、统一图片预算、音频、code mode、多环境 | 不做 | 只对 OpenAI 自家模型或 Codex 特有功能有意义 |

## 6. 关于“观测台代理每次请求都存档”

**它做什么**：观测台代理把 agent 发出的每次模型请求和拼回的响应原样写进对话存档（`.data/runs/<对话ID>.jsonl`）。依赖它的有：页面上“原始输入输出”与相邻请求的差异；续聊（`resumeHistory` 从最后一次普通请求恢复上下文）；D17 归因回放（`agent.ForkSnapshot` 从当时的请求快照分叉）；评测页的过程还原。它回答的是“模型当时到底看到了什么”，对学习和归因很有价值。

**代价**：每次请求都带完整历史，所以存档按请求次数大致平方增长。2026-10-20 的统计：`.data/runs` 共 130 MB、275 条对话；最大的一条 39 MB，其中请求体 30 MB（106 次请求，平均 294 KB）。纯文字时还能接受；一张 1 MB 的图片放进历史后，之后每次请求要多写 1.3 MB。

**Codex 怎么分**：Codex 有两份记录。
- rollout：只追加的消息记录，每条只存一次，恢复与分叉都从它推出来。
- rollout trace：按请求存原文的诊断记录，默认关闭，只在设了 `CODEX_ROLLOUT_TRACE_ROOT` 时写；它把大块内容写进 `payloads/*.json`，事件里只放引用，不参与恢复。

**结论**：“按请求存原文”本身不是错，Codex 也有，只是做成了可选的诊断记录。本项目的问题有两个：
1. 它**默认一直开着，并且内联在对话存档里**。页面和观测台启动时都要整份读入。
2. 它**同时被当成状态用**：续聊和归因回放依赖它。[架构评估](architecture-review.md)的第 ③、④ 条已经指出这一点（“回放分叉依赖观测台的请求存档”），并把只追加的会话记录列为优化 O9。

**对图片功能的影响**：不必先做 O9。图片只要做到“存一次、处处引用”（4.1 节），请求存档里每张图只是一个几十字节的引用，膨胀问题就不会因为图片而恶化。代理改动很小：存档前把请求体里的 data URL 换回 `attachment://` 引用（按解码后字节的 sha256 算名字，和 agent 写入的是同一个文件）；转发给上游的仍是原始请求体。文字历史的平方增长留给 O9 处理。

## 7. 实施步骤与验证

**第一步：附图**
1. `llm`：`Image`、`Message` 的 JSON 互转、`PrepareImage`、附件存储、发送前展开、能力表与剥离、图片计数。
2. agent：`-image`、`turn/start` 的 `images`、用户消息事件带图。
3. 观测台：`/attachments` 上传与读取、`/runs` 与续聊带图、代理存档时换回引用、`/models` 返回能力。
4. 页面：附图按钮、粘贴、拖放、缩略图、模型不能看图时禁用、对话里显示用户的图。
5. 文档：README 参数与配置、观测台说明、新开一篇实践记录、AGENTS.md 增加约定。

**第二步：`view_image`**：工具定义、路径规则、结果格式、界面卡片。

**验证**（按 AGENTS：格式化、编译、真实运行，不写测试文件）：
- 真实模型各跑一次：豆包、kimi、deepseek-v4.1-flash、glm-5.3-flash、DeepSeek 官方 deepseek-flash。
- 不能看图的模型（deepseek-v4-pro）：界面禁用、`/runs` 返回 400、命令行请求里图片被替换成说明文字。
- 一张大图（如 4032×3024）：缩放到 2048 以内，开标签里有原尺寸。
- 续聊：从存档读回后图片仍在，第二轮还能回答图片相关的问题；`-resume` 与归因回放同样。
- 压缩：压缩后摘要里只有标签文字，保留区的图仍在。
- 存档体积：同一个带图的多轮对话，存档里没有 base64，只有引用。
- 多租户：别的租户读这张图返回 404；审计里有 `user_attach`。

## 8. 待定

- `ark-code-latest` 是否允许附图（建议不允许）。
- 每条消息最多几张（建议 8；claude.ai 是 20）。
- 附件的保留与删除：现在和对话存档一样不自动删；撤销（D29）不删附件。
- DeepSeek 官方接口的工具结果带图还没实测，第二步之前补测。
- 导入 Codex 与 Claude Code 会话时，是否把内联图片落成附件（现在显示“[图片]”）。
- 方舟 Coding Plan 文档写明套餐额度只在 AI 编程工具中生效、不能用于 API 调用，否则可能被判为滥用。验证模型能力时走 harness 本身，不用脚本直接请求套餐入口。

## 9. 实现进度

**已实现（agent 侧）**：`internal/llm/image.go`（`Image`、`PrepareImage`、附件存储、发请求前展开与按能力去图、`ImagesToText`、`LastImageNumber`）；`Message` 的 JSON 互转与每张图 1280 token 的计数（`protocol.go`）；`Provider.Models` 改成带 `Vision` 的条目；摘要请求只发文字（`context_manager.go`）；`-image` 与 `turn/start` 的 `images`，用户消息事件带图片列表（`main.go`、`react.go`）。

**与设计不同或补充的地方**：
- 处理不了的图，说明放在紧跟用户消息的 `<system-reminder>` 里，不拼进用户原话：记忆的 `remember_memory` 只能引用用户原话，来源判断也按原话匹配当前问题。
- 续聊时编号接着历史往后数（历史里的图片和文字里出现过的 `[Image #N]` 都算），不从 `[Image #1]` 重新开始。
- 缩放先按长边与块数算出比例，块数因取整超出时把比例再调小重算，两边一起缩，宽高比不变（4032×3024 → 1829×1371）。
- 不能看图的模型仍按每张 1280 计入估算（发请求前才去图），有意偏保守。

**过程**：在 `.data/selfuse/` 的临时 clone 里，由方舟 deepseek-v4.1-flash 照本设计实现（90 次请求、约 11 分钟），review 后修了 5 处：缩放不等比、失败说明混进用户原话、多轮编号重复、附件权限与写入方式（改为 0700/0600、临时文件改名）、误标为 D31。

**真实验证**（沙箱外，命令行）：deepseek-v4.1-flash、kimi-k3、doubao-seed-2.1-pro 与 DeepSeek 官方 deepseek-flash 分别看对了 PNG、4032×3024 的 JPEG（缩到 1829×1371，标签里有原尺寸）、WebP、动图 GIF 的第一帧；超像素图与伪装成 png 的文本变成提醒，模型如实转述；deepseek-v4-pro 收到“图片已省略”；续聊时新图编号为 `[Image #3]`，上一轮的图仍随请求发出；带图任务中断后 `-resume`，检查点里的图还在；检查点里只有引用、没有 base64；附件目录 0700、文件 0600。

**下一步**：第一步剩下的观测台部分（4.4 第 1、2 道防线，4.5 的接口，4.8，第 6 节的代理存档换引用，每条消息最多 8 张），再做页面与 `view_image`。
