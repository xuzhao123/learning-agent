# Day 33 项目实践：模型适配层

先读 [学习笔记](day-33-notes.md)。对应能力：H02 模型客户端（多协议适配、错误分类与重试、按模型的能力）；相关的有 H01（回复分段）、H04（按模型的窗口）。做的是评估里的 O1、O6 与复查里的 R5（见 [架构评估](../architecture-review.md) 第 4、5 节）。

## 改动一览

| 改动 | 代码 | 作用 |
| --- | --- | --- |
| 分类重试 | [llm.go](../../internal/llm/llm.go) 的 `transient`、`CallKeeping` | 429（额度用完除外）、5xx、连接失败、流中途报错或断开时退避 1、2、4 秒或按 `Retry-After`，最多 3 次；要求等超过 60 秒的不重试；重试不占请求预算，span 上记 `retry` 事件；重试前发 `item/delta` 的 `reset` 让界面清掉半段 |
| Anthropic Messages | [messages.go](../../internal/llm/messages.go)、[stream.go](../../internal/llm/stream.go) | 新供应商 `deepseekanthropic`（DeepSeek 的 `/anthropic` 接口，同一把密钥）；`MessagesBody` 把 chat 请求换成 Messages，`Stream` 认出 Messages 事件拼回同一个 `Message`；thinking 的签名存在 `encrypted_content`，下次原样传回 |
| 转换入口 | [messages.go](../../internal/llm/messages.go) 的 `ConvertBody`、[server.go](../../internal/observer/server.go) 的 `proxy` | agent 直连与观测台代理共用一个转换入口，按 `X-Agent-Protocol` 选 Responses 或 Messages |
| 按模型的窗口 | [protocol.go](../../internal/llm/protocol.go) 的 `Model.Window`、`WindowOf`；[settings.go](../../internal/observer/settings.go) | 路由表里记核对过的输入上限；观测台设置的 `context_window` 默认改成 0（按模型），开对话时取模型的窗口，不知道时 262144；命令行的 12288 实验值不变 |
| 回复分段 | [react.go](../../internal/agent/react.go) 的 `announce`；[runs.js](../../internal/observer/web/runs.js)、[chat.js](../../internal/observer/web/chat.js) | 每段模型文字发一个 `agentMessage` 项，`phase` 是 `commentary` 或 `final_answer`；页面按它区分过程说明与最终回答，旧存档仍按有没有工具调用判断 |

## 1. 重试

```go
for attempt := 0; ; attempt++ {
	reply, err = callModel(...)
	var t *transient
	if err == nil || !errors.As(err, &t) || attempt == modelRetries || ctx.Err() != nil {
		break
	}
	wait := t.after             // 服务端的 Retry-After
	if wait == 0 {
		wait = time.Second << attempt // 1、2、4 秒
	}
	if wait > maxRetryWait { break } // 比如额度按小时重置：不等
	…
}
```

`callModel` 只在确定是暂时故障时返回 `transient`：

| 情况 | 是否重试 |
| --- | --- |
| HTTP 429，说明里没有 quota | 是，遵守 `Retry-After` |
| HTTP 5xx | 是 |
| 连接失败、读流中断、没收到任何数据、流结束时没有结束原因 | 是 |
| 流里的 error 事件（供应商在 200 之后报错） | 是 |
| 400、401、403、上下文过长、额度用完、`finish_reason=length` | 否 |

## 2. Messages 协议

请求：system 提到顶层；assistant 的推理（有签名时）、文字、工具调用排成 `thinking`、`text`、`tool_use` 块；工具结果成为下一条 user 消息里的 `tool_result` 块；相邻的同角色消息合成一条（Messages 要求交替）；`max_tokens` 必填，没给时用 32768；推理强度放进 `output_config.effort` 并开启 `thinking`。

流：`message_start` 带 id 和输入用量；`content_block_start` 时 `tool_use` 块按 index 记下对应的工具调用；`text_delta`、`thinking_delta`、`signature_delta`、`input_json_delta` 分别拼进正文、推理、签名、参数；`message_delta` 带 `stop_reason`（换成 chat 的 finish_reason）和累计用量。用量里 `input_tokens` 不含缓存，和 `cache_read_input_tokens`、`cache_creation_input_tokens` 相加才是 prompt_tokens。

接入前用真实请求核对过（2026-10-21）：

- `Authorization: Bearer` 和 `x-api-key` 都能用，所以观测台代理转发时不用改头。
- 第二轮请求带不带 thinking 块都返回 200：DeepSeek 在这个接口下不强制传回推理（它的 Chat Completions 接口要求传回）。签名就是消息 id。
- base64 图片块能看到图。

## 3. 窗口

| 供应商 | 来源 | 填了的模型 |
| --- | --- | --- |
| DeepSeek（两个接口） | DeepSeek `GET /models` 的 `context_window` | deepseek-flash、deepseek-v4-pro：1048576 |
| 方舟 | 方舟 `GET /models` 的 `token_limits.max_input_token_length`；套餐名对应多个版本时取最小 | doubao-seed-2.1-pro 262144、2.1-lite 1048576、evolving 1048576、2.0-mini 229376、glm-5.3-flash 1048576、deepseek-v4 系列 1048576；kimi、minimax、glm-5.3、ark-code-latest 列表里没有，不填 |
| Coconut | 服务的 `/models`：`context_window` 936000、`effective_context_window_percent` 95 | 全部 889200（照 Codex 只用 95%） |

观测台新对话的输入容量（窗口减去输出与推理预留）实测：DeepSeek 1043456、doubao-seed-2.1-pro 257024、kimi-k3（不知道，按 262144）257024、Coconut 884080。

## 4. 方舟为什么不命中缓存

用真实请求核对：

- 方舟 `GET /models` 里，套餐用到的多数模型版本标着 `features.cache.prefix_cache=false`（deepseek-v4 系列全是 false）。
- 同样约 1700 token 的系统提示词经套餐入口的 Chat Completions 连发两次，`cached_tokens` 都是 0。
- 换成套餐入口的 Responses 接口并显式开启缓存，返回 400：`caching is not supported in coding plan / agent plan / codex flow`。

结论：方舟的 Coding Plan 套餐不支持上下文缓存，和请求前缀稳不稳定无关。要用缓存只能走按量付费的入口（另一套计费），本项目不切换。

## 5. 验证

格式化、`go vet`、`go build` 通过。真实运行：

| 场景 | 结果 |
| --- | --- |
| Coconut 的 `coconut-openrouter-4o-max`（上游一直限流，流里报错） | 退避 1、2、4 秒共重试 3 次后报错；请求预算只算 1 次；chat span 上有 3 个 `retry` 事件 |
| 错误的 DeepSeek 密钥（401） | 不重试，直接报出 `Authentication Fails` |
| `deepseekanthropic`：并行算两个乘法再求和 | 先输出一句过程说明，并行调用两次计算器，答 3523；第二、三次请求命中缓存 768、896 token；检查点里每段回复都有推理文字和签名 |
| 同上，经观测台代理 | 存档仍是 chat 形式，答 399；页面的供应商下拉出现“DeepSeek（Anthropic 接口）” |
| 观测台四个模型的新对话 | 输入容量见第 3 节 |
| 工作区里有编译不过的改动，要求“不调工具直接回答” | 三次“想结束”都被 Stop hook 拦下，协议里都标 `commentary`；最后因重复动作熔断，收尾检查照常跑 |

## 6. 已知边界

- 只有模型请求重试；观测台代理本身不重试，重试发生在 agent 一侧，所以存档里会看到失败的那次和重试的那次两条记录。
- 流断开前已经生成的部分可能已经计费，重试会再计一次。
- Messages 的 `max_tokens` 没给时固定 32768，没有按模型的最大输出填；只接了 DeepSeek 的 Anthropic 接口，没有接 Anthropic 自家的服务（没有密钥）。
- Coconut（Responses）的推理只有摘要文字，没法回传。
- 方舟只给能从模型列表确定的模型填了窗口；套餐名到具体版本的对应关系方舟没有公开，窗口取的是几个版本中最小的。
