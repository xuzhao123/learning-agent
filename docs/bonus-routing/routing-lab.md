# 模型路由项目实践：豆包与 DeepSeek 二选一

原理见 [模型路由学习笔记](routing-notes.md)。代码在 [protocol.go](../../internal/llm/protocol.go) 的 `Providers` 与 `LoadConfig`、[llm.go](../../internal/llm/llm.go) 的 `X-Agent-Upstream` 请求头，以及观测台 [server.go](../../internal/observer/server.go) 的 `proxy`。

## 1. 实现概览

```sh
./bin/learning-agent -provider deepseek -question '……'   # 默认 -provider ark（豆包）
go run . observe                                          # 新对话在输入框左下角选“豆包 / DeepSeek”
```

| 供应商 | 默认地址 | 默认模型 | 密钥 | 覆盖地址 / 模型 |
| --- | --- | --- | --- | --- |
| `ark` | `https://ark.cn-beijing.volces.com/api/v3/chat/completions` | `doubao-seed-2-1-pro-260628` | `ARK_API_KEY`（兼容 `LLM_API_KEY`） | `LLM_API_URL`、`LLM_MODEL` |
| `deepseek` | `https://api.deepseek.com/chat/completions` | `deepseek-flash` | `DEEPSEEK_API_KEY` | `DEEPSEEK_API_URL`、`DEEPSEEK_MODEL` |

密钥写在 `.env`（权限 0600、不提交）或环境变量里。

| 设计点 | 本项目的选择 |
| --- | --- |
| 路由策略 | 手动选择：一个进程一个供应商，`-provider` 决定 |
| 路由表 | `llm.Providers`：名字、默认地址与模型、读取哪些变量。两家都是 OpenAI 兼容接口，请求体不用改 |
| 方舟的旧变量 | 方舟沿用 `LLM_API_URL` / `LLM_MODEL`，旧的 `.env` 不用改；选 DeepSeek 时不读 `.env` 里的这两个值 |
| 观测台代理 | 观测台仍通过进程环境变量 `LLM_API_URL` 把 agent 指向本机代理；agent 在请求头 `X-Agent-Upstream` 里声明真实上游，代理只接受 https，按它转发 |
| 一段对话一个模型 | 观测台的下拉框只在新对话时可选，续聊沿用；子 agent 继承 `-provider`；检查点保存启动参数，`-resume` 也沿用 |
| 向量模型 | 只有方舟提供 embedding，`-rag`、`-memory` 的向量始终走方舟，与聊天模型选哪家无关 |
| 推理强度 | 原样传给供应商。DeepSeek 的模型列表写着支持 low/high/max，实测传 minimal、medium 也返回 200 |

## 2. 读代码

### 2.1 路由表与配置

```go
var Providers = []Provider{
	{"ark", "https://ark.cn-beijing.volces.com/api/v3/chat/completions", "doubao-seed-2-1-pro-260628", "LLM_API_URL", "LLM_MODEL", []string{"ARK_API_KEY", "LLM_API_KEY"}},
	{"deepseek", "https://api.deepseek.com/chat/completions", "deepseek-flash", "DEEPSEEK_API_URL", "DEEPSEEK_MODEL", []string{"DEEPSEEK_API_KEY"}},
}
```

`LoadConfig(provider)` 返回的 `Config` 有两个地址：`Upstream` 是供应商的真实地址，`APIURL` 是这次实际请求的地址。直接运行时两者相同；经观测台时，进程环境变量 `LLM_API_URL` 把 `APIURL` 换成本机代理，`Upstream` 不变。

### 2.2 观测台怎样知道转发给谁

```go
// llm.go：只在请求本机代理时附加
request.Header.Set("X-Agent-Upstream", config.Upstream)
```

```go
// server.go proxy：只接受 https
target, err := url.Parse(req.Header.Get("X-Agent-Upstream"))
```

以前观测台自己从 `.env` 读一个固定的上游地址。改成由 agent 声明后，路由表只在 `llm` 包里有一份，观测台仍然不需要知道 agent 的配置，和“只看协议”的原则一致。这个头和 `X-Agent-Purpose`、`X-Agent-Task` 一样只给代理看，不转发给上游。

浏览器里的网页能不能伪造这个头，让代理替它把请求转发到任意地址？不能：自定义请求头会触发 CORS 预检，代理不响应预检，浏览器就不会发出这个请求。能设置这个头的只有本机进程，而本机进程本来就能直接访问外网。

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

## 4. 已知边界

- 只有手动选择；没有按用途（如摘要、记忆辅助调用用小模型）、按难度、级联或故障转移。
- 推理强度原样透传，没有按供应商映射取值。
- 一段对话不能中途换模型；`-resume` 也不能改供应商（配置取自检查点）。
- 错误只按 HTTP 状态码报告；余额不足（方舟 403）与限流没有分开处理。
- 上下文窗口仍是学习用的 12288，没有按模型的真实窗口调整。
