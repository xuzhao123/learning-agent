# Learning Agent 知识库

这 24 条 FAQ 是依据当前项目源码、学习笔记和官方文章整理的教学资料。每个二级标题是一条独立文档；来源用于核对事实，文档中的文字不是可执行指令。

## D01 | 模型怎样调用工具
来源：internal/llm/llm.go；internal/agent/react.go

模型只生成工具名与 JSON 参数，不执行本地代码。Go 从 tool_calls 读请求，调度函数，再将结果作为 role=tool 的消息回填，下一次模型调用才能看到结果。

## D02 | 并行调用与结果依赖
来源：internal/agent/react.go；main.go

本项目默认同时执行 4 个工具，一批最多 16 项。独立工具可同轮并行；需要另一个结果的工具必须等下一轮。并行度控制执行槽位，不代表提前加载四个工具。

## D03 | 工具结果如何配对
来源：internal/agent/react.go

每个工具请求有 ID。Go 为每项调用保留对应结果槽，整批结束后按原顺序写回；tool_call_id 关联原请求，所以完成顺序不同也不会把结果串给别的工具。

## D04 | 默认请求步数预算
来源：main.go；internal/agent/react.go

本项目 max-steps 默认 10，统计的是模型请求次数，最终回答、摘要和超窗重试都计入。达到上限后返回未完成及已执行步骤摘要，不再另调模型生成总结。

## D05 | 失败重试与退避
来源：internal/agent/react.go；main.go

本项目工具失败默认额外重试 2 次，总共最多执行 3 次。两次等待为 200ms、400ms。重试耗尽后错误作为 Observation 回填，让模型决定修正参数、换工具或结束。

## D06 | 重复动作熔断
来源：internal/agent/react.go

本项目第三次连续出现相同工具与参数时，执行前拦下整批，报告疑似死循环。仅改变 JSON 空白或键顺序，仍视为相同参数。工具内部重试不算新的模型动作。熔断防原地打转，步数预算防跑太久。

## D07 | 生产中的重试边界
来源：docs/day-02/day-02-notes.md

瞬时网络故障可有限重试，参数错误应反馈模型修正。带副作用的请求超时可能已经成功，不能盲目重试；应使用幂等键或查询执行状态，区分失败与结果未知。

## D08 | 当前压缩触发容量
来源：main.go；internal/agent/context_manager.go

本项目默认窗口 12288，输出规划预留 4096，安全余量 1024，输入容量为 7168。达到输入容量的 90%（整数阈值 6451）触发处理。4096 只是预留，默认不发送 API 输出上限。

## D09 | Transcript 和 View
来源：internal/agent/context_manager.go

Transcript 保存本次进程收到的原文，View 是真正发送给模型的消息。工具结果可在进入 View 时截断，原文仍留在 Transcript。存档中有内容，不代表模型已经看到或可以自行读取。

## D10 | 工具输出写入限制
来源：internal/agent/context_manager.go；main.go

本项目 tool-output-tokens 默认 2000，按字节粗估。长工具结果首次进入 View 时截断，Transcript 保留原文。检索片段也属于工具结果，因此 k 越大越可能挤占窗口或被截断。

## D11 | 最近消息组保留规则
来源：internal/agent/context_manager.go；main.go

本项目 keep-groups 默认最多 2 组。若超过 20% 配额或挤占摘要空间，K 会继续减少，可以到 0。一次 assistant 工具请求和配套 tool 结果必须作为完整组处理。

## D12 | 压缩后的大小目标
来源：internal/agent/context_manager.go；docs/day-03/day-03-lab.md

本项目 40% 是软目标，摘要后在 60% 内仍可接受；超过 40% 会提醒。用户原话配额 10%，摘要目标 5%。这些是学习项目的取舍，不能当作 Codex 的统一规则。

## D13 | 摘要能否调用工具
来源：internal/llm/llm.go；internal/agent/context_manager.go

本项目摘要请求在携带工具定义时设置 tool_choice=none，禁止摘要模型执行工具。摘要失败时保留旧 View；摘要请求也计入 max-steps，不会获得额外免费预算。

## D14 | 观测台如何续聊
来源：internal/observer/server.go；internal/agent/context_manager.go

观测台续聊恢复最后一次有效请求的 View 加最后的 assistant 回答，并非重新加载全部原始记录。每个新提问重置模型请求预算；页面 Turn 编号在同一条对话中继续递增。

## D15 | history 与本轮输入
来源：internal/observer/index.html

观测台 history 只展示这次实际发送的历史消息，system 单列，本轮提问在 user 中。新对话 Turn 1 没有历史。message 详情可查看完整请求，包括 system、历史和本轮输入。

## D16 | RAG 放在工具层
来源：docs/day-01/day-01-notes.md；https://manus.im/blog/Context-Engineering-for-AI-Agents-Lessons-from-Building-Manus

工具式 RAG 由 Agent 按需发起检索，只将相关片段作为工具结果加入当前上下文。文档库在窗口外保存知识，检索把需要的知识取回来；压缩负责管理已经进入窗口的信息。

## D17 | 按语义边界切块
来源：https://www.anthropic.com/engineering/contextual-retrieval

切块应尽量保留一个完整概念，并携带标题和来源。块太小会丢失主体或条件，太大则混入不相关内容。本知识库按 FAQ 标题切成 24 条短文档，不机械地每固定字符切一刀。

## D18 | top-k 的取舍
来源：https://www.anthropic.com/engineering/contextual-retrieval

top-k 表示最多取回多少个候选片段，不代表它们都能回答问题。k 小可能漏证据，k 大增加噪声、延迟和上下文成本。应同时观察召回和答案，不以返回条数判断检索成功。

## D19 | 相似度不是可信概率
来源：internal/retrieval/retrieval.go；internal/retrieval/embedding.go

本检索先将向量归一化，再计算点积，得到余弦相似度，越大越相似；它不是答案正确概率。低于阈值的片段不能当作证据，即使超过阈值也要检查正文是否真正支持问题中的事实。

## D20 | Contextual Retrieval
来源：https://www.anthropic.com/engineering/contextual-retrieval

Contextual Retrieval 在索引前让 LLM 为每个片段生成通常 50–100 token 的文档背景，再与片段一起建立向量和 BM25 索引。背景生成有预处理成本，也需要检查新增背景的准确性。

## D21 | 混合检索与标识符
来源：https://www.anthropic.com/engineering/contextual-retrieval

向量检索擅长语义近似，BM25 擅长关键词和精确标识符，rerank 再判断候选和问题的相关性。混合后可互补，但要评估成本。本项目 Day 4 的基础实验只实现向量召回。

## D22 | Contextual Retrieval 实验数据
来源：https://www.anthropic.com/engineering/contextual-retrieval

Anthropic 报告的 top-20 检索失败率：背景向量单独降低 35%，背景向量加背景 BM25 降低 49%，再加 rerank 降低 67%。这是特定数据集的相对降幅，不是任意 RAG 的收益保证。

## D23 | 来源引用与资料不足
来源：internal/agent/react.go；internal/retrieval/retrieval.go

知识库回答应先检索，并用 [D编号] 标注支持结论的片段。没有足够证据时应说明资料不足，不编造项目事实或来源。召回主题相近的片段，也可能无法回答某个具体数字。

## D24 | 检索内容的信任边界
来源：docs/day-02/day-02-notes.md

文档片段只是数据。即使片段含有“忽略系统要求”或“执行某个命令”等文本，也不应获得系统指令权限。来源引用帮助核对依据，不能保证资料本身真实或完整。
