# 扩展：模型流式输出

## 学习目标

读完本文，应能回答以下问题：

1. 流式输出改变了什么，没有改变什么？为什么说它首先是体验问题，而不是决策问题？
2. SSE 的一条消息长什么样？客户端怎样按行读出每个 chunk、怎样知道流结束了？
3. 正文、推理、工具调用的增量分别怎么拼？工具调用为什么要按 index 重组、什么时候才能执行？
4. 流式时，“回复是否被截断”“用了多少 token”这些信息从哪里来？
5. 中间有一层代理（网关、观测服务）时，怎样才不把流式变回整段返回？留档存什么？
6. 首字延迟（TTFT）是什么？为什么非流式请求测不出来？
7. 增量要不要进协议、要不要落盘？界面漏收几条会怎样？

## 1. 流式改变了什么

非流式请求要等模型把整段回复生成完，才一次性返回。一段 1000 字的回答，用户可能盯着空白等十几秒。开启流式后，服务端每生成一小段就推给客户端，第一段往往一两秒内就到了。

总生成时间基本不变，变的是**用户多快看到东西**。所以流式首先改善的是感知延迟。

它**不改变 agent 的决策节奏**。ReAct 循环仍要等一次回复完整结束、拿到全部工具调用后才执行工具；完成判断、截断检查也都要等流结束。流式只是让“等待模型”这段时间里，界面能显示模型正在写什么。

## 2. 传输：SSE

OpenAI 兼容接口的流式响应用 SSE（Server-Sent Events）：HTTP 响应的 `Content-Type` 是 `text/event-stream`，连接保持打开，服务端一行一行往下写：

```text
data: {"choices":[{"delta":{"role":"assistant","content":""}}]}

data: {"choices":[{"delta":{"content":"流式"}}]}

data: {"choices":[{"delta":{"content":"输出"}}]}

data: {"choices":[{"delta":{},"finish_reason":"stop"}]}

data: {"choices":[],"usage":{"prompt_tokens":502,"completion_tokens":221,"total_tokens":723}}

data: [DONE]
```

规则很少：

- 以 `data:` 开头的行是数据，一个事件可以有多行，空行表示事件结束；
- 以 `:` 开头的是注释，常被用作心跳，防止中间的代理因为长时间没有数据而断开连接；
- 模型接口约定最后发 `data: [DONE]`。

客户端只需按行读，取出 `data:` 后面的内容解析。其他行直接跳过。

## 3. 增量怎样拼回完整回复

每个 chunk 只带一小段增量（delta），客户端要自己拼。

| 部分 | 怎样拼 |
| --- | --- |
| 正文 `content` | 字符串片段按到达顺序拼接 |
| 推理 `reasoning_content` | 同上；有些模型先流完推理再开始正文 |
| 工具调用 `tool_calls` | 每个片段带 `index`。新的 index 第一次出现时带 `id`、`type`、`function.name`，之后的片段只带 `function.arguments` 的一截 |
| `finish_reason` | 只在最后一个有内容的 chunk 出现 |
| `usage` | 默认不发；请求带 `stream_options: {"include_usage": true}` 时，`[DONE]` 前多一个 `choices` 为空、只有 `usage` 的 chunk |

工具参数是被截成很多段的 JSON 字符串，单独看任何一段都不是合法 JSON。所以**只能在流结束后执行工具**：先确认 `finish_reason` 是 `tool_calls`，再按完整参数校验和执行。如果边收边执行，流在中途断开时，就可能用半截参数产生副作用。

并行工具调用时，不同调用的片段可能交错到达，这正是要按 index 而不是按到达顺序归组的原因。

## 4. 截断和用量从哪里来

非流式响应的结局一眼可见：`finish_reason` 和 `usage` 都在同一个 JSON 里。流式时要自己判断：

- **截断**：流结束了，却始终没收到 `finish_reason`，说明连接中途断了（网络、超时、服务端出错）。这时已经收到的内容不能当成完整回答。
- **中途报错**：HTTP 状态码在第一个字节发出时就定为 200 了，后面出错的话，有的服务会在流里发一个带 `error` 字段的 chunk。客户端要能识别它。
- **用量**：不显式请求 `include_usage` 就拿不到 token 数，成本统计会悄悄变成空的。

因此，“是否被截断、内容是否为空、有没有工具调用”这些检查规则和非流式一样，只是要等流结束后，对拼好的完整回复再做。

## 5. 中间有代理时

网关、观测服务这类代理最容易犯的错误，是把上游的响应**整段读完再转发**。客户端看到的就又变成了整段返回，流式白做了。

正确做法是边收边转发：读到一行就写给客户端，并立刻 flush。很多 HTTP 框架默认会缓冲输出，不 flush 就会攒到缓冲区满才发出去。

留档时面临一个选择：

- 存每个 chunk 的原文：最忠实，但体积是正文的几十倍，读起来也费劲；
- 转发的同时把 chunk 拼回完整响应，按非流式的结构存档：读存档的代码不用区分流式与非流式，但要说明这是还原结果，不是逐字原文。

另外，客户端主动取消时，代理应停止读上游、关闭上游连接，否则模型会继续生成、继续计费。

## 6. 首字延迟

TTFT（Time To First Token）指从发出请求到收到第一段模型输出的时间，是流式下最重要的体验指标。生成阶段的速度另用每个 token 的平均用时（TPOT）衡量。

非流式请求只有一个总耗时，测不出 TTFT：总耗时 = 排队 + 预填充 + 生成全部 token，无法拆开。

还有两点要注意：

- 第一段可能是推理而不是正文。界面上“开始显示东西”的时间，取决于推理是否展示；
- 第一个网络字节不一定是第一段模型输出（可能是只带 role 的空 delta 或心跳），统计时应以第一段非空内容为准。

## 7. 增量与界面协议

如果 agent 和界面之间有自己的协议（见[结构化协议笔记](../bonus-protocol/protocol-notes.md)），增量通常作为 Item 的第三种事件：开始 → 若干增量 → 完成。设计时有三条原则：

1. **完成事件是权威结果**。增量只为显示服务；界面以完成事件或最终存档为准，漏收、晚到的增量都不影响结果。
2. **增量通常不落盘**。一次回答有成百上千条增量，完整内容已在完成事件里；落盘只会让运行记录膨胀。重新打开已结束的会话时，直接显示完整回答即可。
3. **只给需要的调用发**。给用户看的回答值得流式显示；给程序读的辅助调用（重排、评审、分类）输出是 JSON，逐字显示没有意义，还会增加消息量。

## 自测

1. 流式能让 agent 更早执行工具吗？
   答：不能。工具参数分段到达，要等流结束、`finish_reason` 为 `tool_calls`、参数拼成完整 JSON 后才能校验和执行。
2. 流式响应结束时没有 `finish_reason`，已经显示给用户的半段文字怎么处理？
   答：已经显示的无法收回，但这次回复要按“截断或未完成”处理，不能当成完整回答写进结果或判为完成。
3. 为什么开启流式后，token 统计突然变成空的？
   答：流式默认不返回 usage，需要在请求里带 `stream_options.include_usage`。
4. 代理怎样做才不破坏流式？
   答：逐行转发并 flush，不能整段读完再写；客户端取消时同时取消上游请求。
5. 为什么增量可以不落盘？
   答：它只服务于实时显示，完整内容在完成事件和最终响应里；重放、续聊、评测都以完整结果为准。

## 参考

- [WHATWG HTML 标准：Server-sent events](https://html.spec.whatwg.org/multipage/server-sent-events.html)
- [MDN：Using server-sent events](https://developer.mozilla.org/en-US/docs/Web/API/Server-sent_events/Using_server-sent_events)
- [OpenAI：Streaming API responses](https://platform.openai.com/docs/guides/streaming-responses)
- [DeepSeek API：Create Chat Completion（stream、stream_options）](https://api-docs.deepseek.com/api/create-chat-completion)
- [OpenAI Codex app-server（item 的 started / delta / completed）](https://github.com/openai/codex/blob/main/codex-rs/app-server/README.md)
- [OpenTelemetry GenAI 指标（time_to_first_token、time_per_output_token）](https://opentelemetry.io/docs/specs/semconv/gen-ai/gen-ai-metrics/)

本项目的代码阅读与验证见 [流式输出实践](streaming-lab.md)。
