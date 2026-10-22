# 模型路由项目实践：豆包、DeepSeek 与 Coconut

原理见 [模型路由学习笔记](routing-notes.md)。代码在 [protocol.go](../../internal/llm/protocol.go) 的 `Providers` 与 `LoadConfig`、[llm.go](../../internal/llm/llm.go) 的 `X-Agent-Upstream` 请求头、[responses.go](../../internal/llm/responses.go) 的协议转换，以及观测台 [server.go](../../internal/observer/server.go) 的 `proxy`。

## 1. 实现概览

```sh
./bin/learning-agent -provider deepseek -question '……'   # 默认 -provider ark（豆包）
./bin/learning-agent -model kimi-k3 -question '……'        # 方舟套餐里的其他模型
./bin/learning-agent -provider coconut -question '……'     # 本机 Codex 兼容服务，Responses 协议（第 5 节）
go run . observe                                          # 新对话在输入框左下角选“豆包 / DeepSeek / Coconut”，旁边再选具体模型；设置页的“默认模型”同一份清单
```

| 供应商 | 默认地址 | 默认模型（可选清单） | 密钥 | 覆盖地址 / 模型 |
| --- | --- | --- | --- | --- |
| `ark` | `https://ark.cn-beijing.volces.com/api/coding/v3/chat/completions`（Coding Plan 订阅入口） | `doubao-seed-2.1-pro`（另有 `doubao-seed-2.1-lite`、`kimi-k3`、`glm-5.3`、`deepseek-v4-pro`、`minimax-m3`、`ark-code-latest` 等，共 14 个） | `ARK_API_KEY`（兼容 `LLM_API_KEY`） | `LLM_API_URL`、`LLM_MODEL` |
| `deepseek` | `https://api.deepseek.com/chat/completions` | `deepseek-flash`（只有这一个） | `DEEPSEEK_API_KEY` | `DEEPSEEK_API_URL`、`DEEPSEEK_MODEL` |
| `deepseekanthropic` | `https://api.deepseek.com/anthropic/v1/messages`（Anthropic Messages 协议，D33） | `deepseek-flash`（另有 `deepseek-v4-pro`，只收文字） | `DEEPSEEK_API_KEY` | `DEEPSEEK_ANTHROPIC_URL`、`DEEPSEEK_MODEL` |
| `coconut` | 没有默认值，必须配 `COCONUT_API_URL`（本机服务的 `.../v1/responses`） | `coconut-gpt-6-1-sol-max`（共 12 个） | 不要密钥 | `COCONUT_API_URL`、`COCONUT_MODEL` |

密钥写在 `.env`（权限 0600、不提交）或环境变量里。

| 设计点 | 本项目的选择 |
| --- | --- |
| 路由策略 | 手动选择：一个进程一个供应商、一个模型，`-provider` 与 `-model` 决定 |
| 路由表 | `llm.Providers`：名字、显示名、协议、默认地址与模型、读取哪些变量。方舟与 DeepSeek 是 OpenAI 兼容的 chat/completions，请求体不用改；Coconut 只说 Responses，发出前换请求体（第 5 节） |
| 方舟的旧变量 | 方舟沿用 `LLM_API_URL` / `LLM_MODEL`，变量名不用改；值是套餐入口地址与套餐模型名 `doubao-seed-2.1-pro`，旧 `.env` 里写的老地址 `.../api/v3` 与带日期版本号的老模型名要换掉或删掉；选 DeepSeek 时不读 `.env` 里的这两个值 |
| 方舟两个入口 | `.../api/coding/v3` 走 Coding Plan 订阅额度，文档列的是套餐 Model Name 与 `ark-code-latest`；`.../api/v3` 按量付费，用带日期版本号的模型名。文档说明用 `.../api/v3` 不消耗套餐额度，所以地址不能混用；套餐入口实测也接受带日期的旧名字（3.4 节），是否计入套餐以控制台用量为准 |
| 观测台代理 | 观测台仍通过进程环境变量 `LLM_API_URL` 把 agent 指向本机代理；agent 在请求头 `X-Agent-Upstream` 里声明真实上游，代理按它转发：只接受 https，不带密钥的请求可以是 http |
| 一段对话一个模型 | 观测台的两级下拉（供应商 + 具体模型）只在新对话时可选；`-provider`、`-model` 都是启动参数，写进检查点，续聊、`-resume`、归因回放与子 agent 都沿用 |
| 具体模型 | 清单写在 `Provider.Models`，默认值仍是 `Provider.Model`（`LLM_MODEL` / `DEEPSEEK_MODEL` 可改默认值）；`GET /models` 把清单交给页面，页面不抄第二份；选定后以 `-model` 传给 agent，`/runs` 拒绝清单外的名字 |
| 向量模型 | 只有方舟提供 embedding，`-rag`、`-memory` 的向量始终走方舟，与聊天模型选哪家无关 |
| 推理强度 | 原样传给供应商。DeepSeek 的模型列表写着支持 low/high/max，实测传 minimal、medium 也返回 200 |

## 2. 读代码

### 2.1 路由表与配置

```go
var Providers = []Provider{
	{Name: "ark", Label: "豆包", APIURL: "https://ark.cn-beijing.volces.com/api/coding/v3/chat/completions",
		Model: "doubao-seed-2.1-pro", URLVar: "LLM_API_URL", ModelVar: "LLM_MODEL", KeyVars: []string{"ARK_API_KEY", "LLM_API_KEY"},
		Models: []Model{{Name: "doubao-seed-2.1-pro", Vision: true}, …, {Name: "ark-code-latest"}}},
	{Name: "deepseek", Label: "DeepSeek", APIURL: "https://api.deepseek.com/chat/completions", …},
	{Name: "coconut", Label: "Coconut", Protocol: "responses", Model: "coconut-gpt-6-1-sol-max", URLVar: "COCONUT_API_URL", ModelVar: "COCONUT_MODEL", …},
}
```

`LoadConfig(provider)` 返回的 `Config` 有两个地址：`Upstream` 是供应商的真实地址，`APIURL` 是这次实际请求的地址。直接运行时两者相同；经观测台时，进程环境变量 `LLM_API_URL` 把 `APIURL` 换成本机代理，`Upstream` 不变。`KeyVars` 为空表示这家不要密钥；没有默认地址的（Coconut）缺 `URLVar` 时报错。观测台的 `GET /models` 带上显示名和“本机没配好”的原因，输入框与设置页的供应商下拉都按它渲染，没配好的灰掉。

### 2.2 观测台怎样知道转发给谁

```go
// llm.go：只在请求本机代理时附加
request.Header.Set("X-Agent-Upstream", config.Upstream)
```

```go
// server.go proxy：只接受 https；不带密钥的请求可以是 http（密钥不经明文发出）
target, err := url.Parse(req.Header.Get("X-Agent-Upstream"))
```

以前观测台自己从 `.env` 读一个固定的上游地址。改成由 agent 声明后，路由表只在 `llm` 包里有一份，观测台仍然不需要知道 agent 的配置，和“只看协议”的原则一致。这个头和 `X-Agent-Purpose`、`X-Agent-Task` 一样只给代理看，不转发给上游。

浏览器里的网页能不能伪造这个头，让代理替它把请求转发到任意地址？不能：自定义请求头会触发 CORS 预检，代理不响应预检，浏览器就不会发出这个请求。能设置这个头的只有本机进程，而本机进程本来就能直接访问外网。

### 2.3 具体模型怎样传下去

供应商之下再选一级模型，用的是 agent 的启动参数 `-model`，不是环境变量。原因在检查点：中断续跑（`-resume`）只用检查点里存的启动参数恢复配置，环境变量不在里面。最初的实现用 `LLM_MODEL` 环境变量传模型，结果续跑和归因回放会悄悄换回默认模型，回放结论就不成立了。

```go
// server.go /runs：清单也来自 llm，页面不做第二份
p, ok := llm.FindProvider(input.Provider)
if _, found := p.FindModel(input.Model); !ok || !found {
	http.Error(w, "模型不在 "+input.Provider+" 的可选清单里", http.StatusBadRequest)
	return
}
// server.go snapshotArgs：模型和工具开关一样，从设置快照还原成启动参数
if e.Model != "" {
	args = append(args, "-model", e.Model)
}
```

几处对应关系：

- `GET /models` 把 `Provider.Models` 交给页面，`default` 按 `.env` 与环境变量解析成不选具体模型时 agent 实际会用的那个；输入框的第二级下拉用它渲染，清单只有一条时隐藏。
- start/continue 事件记下 `model`（空表示用默认值），agent 在 `turn/started` 里报它真正解析出的模型。打开已有对话时只显示这个名字，它可以不在清单里（如旧对话的带日期版本号的名字）；没记录到就不显示，不拿现在的默认值冒充。
- 续聊不重新选：观测台从上一轮的请求里取出实际用的模型（`history.Model`），写进 continue 事件并以 `-model` 启动，换模型要新建对话。子 agent 通过参数继承表拿到 `-model`。
- 链路追踪的 chat span 记 `gen_ai.request.model`，也记响应里的 `gen_ai.response.model`；`ark-code-latest` 的响应是 `auto`，所以看不出实际模型。

## 3. 动手

运行时间 2026-10-07（Asia/Shanghai）。方舟账户当天欠费（403 `AccountOverdueError`），以下都是 DeepSeek。

### 3.1 先用原始请求核对差异

接入前先直接请求 `https://api.deepseek.com`：

```text
GET /models → deepseek-flash（DeepSeek-V4.1-Flash，上下文 1048576，effort 支持 low/high/max）、deepseek-v4-pro
POST /chat/completions（带 tools）→ 200，finish_reason=tool_calls，message 里有 reasoning_content，
     usage 里同时有 prompt_tokens_details.cached_tokens 和 prompt_cache_hit_tokens
第二轮去掉 assistant 消息的 reasoning_content → 400：
     The `reasoning_content` in the thinking mode must be passed back to the API.
```

最后一条最关键：DeepSeek 的思考模式要求把 `reasoning_content` 原样传回。本项目的 `llm.Message` 本来就保留这个字段，历史和检查点里都有，所以不用改。但这说明一段对话中途不能从 DeepSeek 换到会拒绝这个字段的模型（笔记第 4 节）。

### 3.2 CLI

```sh
./bin/learning-agent -provider deepseek -reasoning-effort low -max-steps 4 -question '先查一下今天星期几，再算 23*19+7，一起告诉我。'
```

```text
Model: provider=deepseek model=deepseek-flash upstream=https://api.deepseek.com/chat/completions
Usage: purpose=main prompt_tokens=441 completion_tokens=82 cached_tokens=0
Action [call_00_JeFP…]: get_current_datetime
Action [call_01_c4aT…]: calculator
Usage: purpose=main prompt_tokens=658 completion_tokens=71 cached_tokens=512
1. **今天日期**：2026 年 10 月 7 日，**星期三**。 2. **计算结果**：23 × 19 + 7 = **444**
```

两个工具在同一轮里并行调用；第二次请求有 512 个 token 命中了缓存（前缀就是第一次的 system、user 与工具定义）。

### 3.3 观测台

- 新对话选 DeepSeek、勾选子 agent：父 agent 3 次请求、两个子 agent 各 2 次，7 次请求的 `model` 都是 `deepseek-flash`，全部 200；子 agent 通过继承的 `-provider` 走同一家。
- 续聊“把刚才的计算结果再乘以 3”：新进程打印 `provider=deepseek`，续聊历史带着前几轮的 `reasoning_content`，请求 200，回答 7941。
- 新对话第一次请求后点停止，再发“继续完成”：从检查点续聊，同样走 DeepSeek，exit 0。
- 页面：回答下方显示模型名（`请求 #7 · deepseek-flash · 2.3k tokens`）；续聊时下拉框锁定为这段对话的供应商；390 宽度无横向滚动，无 JS 报错。

### 3.4 方舟 Coding Plan 与具体模型（2026-10-20）

- 清单里 14 个模型各跑一次 `-model <名字> -question '用计算工具算 17*23…'`：都返回 200，都调用了 calculator 并答出 391。其中 `kimi-k2.7-code` 只出现在方舟的一份文档里，实测可用，也收进了清单。
- `-model doubao-seed-2-1-pro-260628`（按量付费入口的旧名字）发到套餐入口同样成功，所以旧对话在新地址上还能续聊；是否计入套餐额度，要看控制台用量。
- `-model ark-code-latest`：trace 里的 `gen_ai.response.model` 是 `auto`。
- `-model glm-5.3 -lab-tools` 在 `slow_job` 执行中按 Ctrl+C：检查点 `args` 里有 `-model glm-5.3`，`-resume` 后启动行仍是 `model=glm-5.3`。
- `-model kimi-k3 -subagents`：子 agent 的启动行也是 `model=kimi-k3`。
- 观测台（新代码另起一个、临时平台账号，用完删除）：`POST /runs` 带 `model:"gpt-9"` 返回 400；带 `kimi-k3` 时首问与续聊的启动行、检查点参数都是 `kimi-k3`。无头 Chrome 打开真实页面：新对话默认 DeepSeek 时第二级隐藏，切到豆包后出现“默认（doubao-seed-2.1-pro）”和 14 个模型；在页面上选 `glm-5.3` 点发送，start 事件与 agent 都是 `glm-5.3`；旧对话显示 `doubao-seed-2-1-pro-260628`，DeepSeek 对话不显示第二级。

## 4. 已知边界

- 只有手动选择；没有按用途（如摘要、记忆辅助调用用小模型）、按难度、级联或故障转移。
- 推理强度原样透传，没有按供应商映射取值。
- 一段对话不能中途换模型；`-resume` 也不能改供应商（配置取自检查点）。
- 错误只按 HTTP 状态码报告；余额不足（方舟 403）与限流没有分开处理。
- 上下文窗口取命令行的 `-context-window`（观测台取设置），没有按模型的真实窗口调整；同一个套餐里换模型时，窗口不跟着变。

## 5. Coconut（Responses 协议）

Coconut 是本机的一个 Codex 兼容服务，只提供 OpenAI Responses 接口（`POST /v1/responses`）。和前两家不同，它换的不只是地址、模型名、密钥，而是整个请求体与流式事件的格式（笔记第 2.1 节）。

### 5.1 接入前的真实请求（2026-10-21）

- `GET /v1/models` 要带 `User-Agent: codex`，返回 Codex 的模型清单格式（`slug`、`context_window` 等），共 12 个模型。
- 流式事件：正文 `response.output_text.delta`；工具调用由 `response.output_item.added` 开始、`response.function_call_arguments.delta` 分段，同一轮两个独立算式会得到 `output_index` 0 和 1 两个 `function_call`；最后 `response.completed` 带完整的 `output` 与 `usage`（`input_tokens`、`input_tokens_details.cached_tokens`、`output_tokens`），之后还有一行 `data: [DONE]`。
- 部分模型（如 `coconut-gpt-6-astra-max`、`coconut-deepseek-v4-1-flash-max`）给出 `reasoning` 项的摘要文字，没有加密的推理内容。
- **模型名不校验**：请求 `model: "nope"` 照样返回 200，响应里的 `model` 只是把请求原样写回，看不出实际是谁回答的。
- **`max_output_tokens` 不生效**：设成 20，仍返回完整的长回答，状态是 `completed`。
- 上游限流时返回 502：`{"error":{"message":"...rate limit...","type":"upstream_error"}}`。

### 5.2 怎么接

agent 内部只有一种消息格式（chat 形式的 `llm.Message`），协议差异只在发出与收回的边上处理：

```text
agent 历史（chat 形式）──ResponsesBody──▶ Responses 请求体 ──▶ 上游
agent ◀── Message（chat 形式）◀── Stream.Add（按 type 认出 Responses 事件）◀── SSE
```

- `llm.ResponsesBody`：`messages[0]` 的 system 变成 `instructions`；assistant 的每个 `tool_calls` 变成一项 `function_call`，`tool` 消息变成 `function_call_output`（带图时 `output` 是数组）；图片是 `input_image`，开、闭标签照旧围在图的两边；`tools` 摊平；`reasoning_effort` 变成 `reasoning.effort` 并要摘要；`store: false`，每次发完整历史。推理内容不回传。
- `Stream.Add`：chat 的 chunk 顶层没有 `type`，Responses 的事件都有，据此分开。正文与推理摘要按 delta 边收边显示；最终回答与工具调用以 `response.completed` 的 `output` 为准，`finish_reason` 换成 chat 的取值（有调用为 `tool_calls`，`response.incomplete` 为 `length`），usage 换成 chat 的字段名。所以截断检查、页面、检查点、指标都不用分协议。
- 经观测台时，agent 仍把 chat 形式的请求发给代理，并加请求头 `X-Agent-Protocol: responses`；代理转发前用同一个 `ResponsesBody` 换请求体，**存档留 chat 形式**，响应由 `llm.Stream` 拼回 chat 形式。页面、续聊恢复、归因、附图换引用读存档的代码都没改。代价是存档里看不到线上的 Responses 原文。
- 明文 HTTP：agent 与代理原来都只接受 https（本机代理除外），为的是密钥不经明文发出。这个服务在本机、走 http、不要密钥，所以改成“不带密钥时允许 http”，带密钥的请求仍然只能走 https。
- 地址因机器而异，路由表里不写默认值，只从 `.env` 或环境变量的 `COCONUT_API_URL` 读。

### 5.3 看图实测（2026-10-21）

请求方式同 [图片输入设计](../image-input-design.md) 2.2：同一张 640×360 的 PNG（“7 3 9”加一个红圆），`input_image` 放 data URL。

| 结果 | 模型 |
| --- | --- |
| 能看图，全部答对 | gpt-6-1-sol、gpt-6-astra、gpt-5-6-sol、gpt-5-6-terra、gpt-5-6-luna、gpt-5-5、gpt-5-4、openrouter-3o、openrouter-2o、deepseek-v4-1-flash、doubao-seed-evolving-dev（名字都带 `coconut-` 前缀与 `-max` 等后缀，见路由表） |
| 没测成（上游一直限流，502），按“不知道就不发图”标为不能看图 | openrouter-4o-max |
| 工具结果（`function_call_output` 的数组）里带图也能看到 | gpt-6-1-sol |

单图输入 token：gpt 系约 320，deepseek-v4-1-flash 约 240，doubao-seed-evolving-dev 约 1370。

### 5.4 验证

- CLI：`-provider coconut` 要求“并行算 37*91 和 12*13 再求和”，第一次回复两个 `calculator` 调用、第二次再算和、第三次回答 3523，流式正文边收边打印；`-model coconut-gpt-5-6-terra-max -image` 附图问“三个数字相加”，答 19 与红色圆形；`-model coconut-gpt-6-astra-max` 的推理摘要进了检查点的 `reasoning_content`。
- 观测台（新代码另起一个、临时平台账号，用完删除）：设置页把默认模型改成 `coconut` 生效，`nope` 被拒（`provider 取 ark、deepseek、coconut`）；不指定供应商的新对话走 Coconut，三次请求的存档都是 chat 形式，第二次起有缓存命中；`model:"nope"` 返回 400；给不能看图的 `coconut-openrouter-4o-max` 附图返回 400；`coconut-gpt-5-5-max` 附图对话答出乘积 189，续聊“再除以最小的数字”沿用同一个模型、历史带着图，答 63，存档里的图是 `attachment://` 引用。
- 无头 Chrome 打开真实页面：输入框的供应商下拉是“豆包 / DeepSeek / Coconut”，选 Coconut 时第二级是“默认（coconut-gpt-6-1-sol-max）”加 12 个模型，切到 DeepSeek 隐藏第二级、切回又出现；设置页“默认模型”是同样三项。
- 不配 `COCONUT_API_URL` 时启动报 `请在 .env 或环境变量配置 COCONUT_API_URL`。
- D16 评测（10-21，`-set all -k 3 -judge ark -- -provider coconut -model coconut-deepseek-v4-1-flash-max`）：22 题、75 个试次全部通过，主分 pass@1 与 pass^3 都是 100%，每试次约 2.6k tokens、P50 3.2s；检索消融 Δ 与以前一致（`rag-retry` +0.83、`rag-parallel` +1.00、`rag-outside` 0）。评审换成方舟，因为默认评审 DeepSeek `deepseek-flash` 与被测的是同一个模型。题目对这个模型已经饱和，分不出高下；服务把请求的模型名原样写回，也无法从响应确认实际答题的模型。

### 5.5 边界

- 服务不校验模型名、不执行 `max_output_tokens`：清单外的名字也能“成功”，截断检查在这个服务上不会触发。
- 推理不回传；服务只给摘要，没有可回传的加密推理内容。
- 存档是转换前的 chat 形式，不是线上原文。
- 上下文窗口仍取设置里的 `context_window`，没有用 `/models` 报的 936000。
- 模型清单是写死的，服务端增删模型时要改路由表并重新实测看图。

