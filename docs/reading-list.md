# 阅读材料

先理解当天笔记，再选择原始资料延伸阅读。Day 1–7 保留任务最初提供的材料清单；后续课程链接到笔记的参考部分，原始链接在该处维护，避免两份清单不同步。缺少原始链接的材料保留“待补”，不猜测来源。完整课程导航见 [README](../README.md#学习目录)。

## Day 1 · Tool Calling

学习入口：[当天笔记](day-01/day-01-notes.md)。

| 材料 | 链接 |
| --- | --- |
| ReAct 论文：*ReAct: Synergizing Reasoning and Acting in Language Models* | https://arxiv.org/abs/2210.03629 |
| OpenAI Function Calling 指南 | https://platform.openai.com/docs/guides/function-calling |
| Lilian Weng《LLM Powered Autonomous Agents》 | https://lilianweng.github.io/posts/2023-06-23-agent/ |

## Day 2 · ReAct 进阶

学习入口：[当天笔记](day-02/day-02-notes.md)。

| 材料 | 链接 |
| --- | --- |
| Anthropic《Building Effective Agents》 | https://www.anthropic.com/engineering/building-effective-agents |
| OpenAI《A Practical Guide to Building Agents》 | https://cdn.openai.com/business-guides-and-resources/a-practical-guide-to-building-agents.pdf |
| 中文《Agent Loop 的循环控制流》 | https://github.com/joshuayang228/my-agent/blob/HEAD/methodology/m01-agent-loop.md |

## Day 3 · 上下文工程（上）

学习入口：[当天笔记](day-03/day-03-notes.md)。

| 材料 | 链接 |
| --- | --- |
| Anthropic《Effective context engineering for AI agents》 | https://www.anthropic.com/engineering/effective-context-engineering-for-ai-agents |
| LangChain Short-term memory 文档 | https://docs.langchain.com/oss/python/langchain/short-term-memory |
| 上文中文翻译解读 | https://blog.csdn.net/qq_41185868/article/details/153544846 |

## Day 4 · 上下文工程（下）

学习入口：[当天笔记](day-04/day-04-notes.md)。

| 材料 | 链接 |
| --- | --- |
| Anthropic《Introducing Contextual Retrieval》 | https://www.anthropic.com/news/contextual-retrieval |
| Manus 官方博客《Context Engineering for AI Agents: Lessons from Building Manus》 | https://manus.im/blog/Context-Engineering-for-AI-Agents-Lessons-from-Building-Manus |
| 上文完整中文翻译 | https://github.com/xinyuliucs/ai-agent-articles-zh/blob/HEAD/context-engineering-for-ai-agents-lessons-from-building-manus-zh.md |

## Day 5 · 记忆系统

学习入口：[当天笔记](day-05/day-05-notes.md)。

| 材料 | 链接 |
| --- | --- |
| MemGPT 论文《Towards LLMs as Operating Systems》 | https://arxiv.org/abs/2310.08560 |
| Generative Agents 论文：*Generative Agents: Interactive Simulacra of Human Behavior* | https://arxiv.org/abs/2304.03442 |
| 记忆系统横评（MemGPT / mem0 / Zep / A-MEM 机制与实测对比） | 待补 |

## Day 6 · MCP 协议（上）

学习入口：[当天笔记](day-06/day-06-notes.md)。

| 材料 | 链接 |
| --- | --- |
| MCP 官方文档与 spec（先看 Concepts → Tools） | https://modelcontextprotocol.io |
| Anthropic MCP 公告《Introducing the Model Context Protocol》 | https://www.anthropic.com/news/model-context-protocol |
| mcp-go（Go SDK，client + server） | https://github.com/mark3labs/mcp-go |

## Day 7 · MCP 协议（下）

学习入口：[当天笔记](day-07/day-07-notes.md)。

| 材料 | 链接 |
| --- | --- |
| MCP spec 的 Server 部分（Server → Tools） | https://modelcontextprotocol.io |
| mcp-go 仓库的 server example（`server.ServeStdio`） | https://github.com/mark3labs/mcp-go |
| JSON-RPC 2.0 规范 | https://www.jsonrpc.org/specification |

## Bonus · Agent Skills

学习入口：[Skills 笔记](bonus-skills/skills-notes.md)。

| 材料 | 链接 |
| --- | --- |
| Agent Skills 官方站（spec、frontmatter 字段、三级加载） | https://agentskills.io |
| mcode 的 skills 实现文档 | https://github.com/immutex/mcode/blob/HEAD/docs/08-skills-and-agents-md.md |
| 《Agent Skills Explained: SKILL.md vs MCP》 | https://www.alekseialeinikov.com/en/blog/topics/ai/agent-skills-explained-skill-md-vs-mcp |

## Day 8 · 取消、超时与重试

先读 [学习笔记](day-08/day-08-notes.md)，重点资料是 Go context、gRPC Deadlines、退避重试与级联故障。完整原始链接集中见该篇的 [参考](day-08/day-08-notes.md#参考)。

## Day 9 · 检查点与恢复

先读 [学习笔记](day-09/day-09-notes.md)，围绕写前日志、原子更新、任务锁与恢复语义阅读。完整资料见 [参考](day-09/day-09-notes.md#参考)。

## Day 10 · 任务调度与幂等

先读 [学习笔记](day-10/day-10-notes.md)，重点是队列、投递语义、幂等与并发容量。完整资料见 [参考](day-10/day-10-notes.md#参考)。

## Day 11 · 浏览器自动化

先读 [学习笔记](day-11/day-11-notes.md)，重点是 chromedp、CDP、等待条件、页面读取与安全边界。完整资料见 [参考](day-11/day-11-notes.md#参考)。

## Day 12 · 代码执行沙箱

先读 [学习笔记](day-12/day-12-notes.md)，重点是 bubblewrap、namespace、cgroup、seccomp 与组合工具风险。完整资料见 [参考](day-12/day-12-notes.md#参考)。

## Day 13 · 可观测性

先读 [学习笔记](day-13/day-13-notes.md)，重点是 trace/span 数据结构、W3C traceparent 传播、OpenTelemetry 的处理器与导出器、GenAI 语义约定，以及代理与进程内打点的取舍。完整资料见 [参考](day-13/day-13-notes.md#参考)。

## Day 14 · 第二周综合与复盘

先读 [第二周复盘](week-02-review.md)，重点是“一次工具调用的一生”和实测中反复出现的问题；再看 [综合演练](day-14/day-14-lab.md) 怎样从调用链读出取消路径与耗时分布。

## 扩展 · 子 agent

先读 [学习笔记](bonus-subagents/subagents-notes.md)，重点是独立上下文、任务说明与权限继承。完整资料见 [参考](bonus-subagents/subagents-notes.md#参考)。

## Day 15 · 评测指标

先读 [学习笔记](day-15/day-15-notes.md)，重点是完成与答对的区别、pass@k 与 pass^k、分母的取法、每次成功的成本，以及为什么指标的数据不能只取自 trace。完整资料见 [参考](day-15/day-15-notes.md#参考)。

## Day 16 · 可复现评测集与回归

先读 [学习笔记](day-16/day-16-notes.md)，重点是好题目的条件（参考答案、反例）、打分器按成本分层、评审模型的偏差与三票表决、按题聚类的标准误与逐题配对、留出集与消融。原始资料首推 Anthropic《Demystifying evals for AI agents》与 Claude Code 插件评测文档。完整资料见 [参考](day-16/day-16-notes.md#参考)。

## Day 17 · 失败归因

先读 [学习笔记](day-17/day-17-notes.md)，重点是第一个上游错误、开放编码与轴心编码、自动归因为什么还不可靠（Who&When），以及反事实回放怎样把根因假设变成证据。完整资料见 [参考](day-17/day-17-notes.md#参考)。

## 扩展 · 结构化协议（B0）

先读 [学习笔记](bonus-protocol/protocol-notes.md)，重点是 JSON-RPC 的请求、响应与通知，Thread / Turn / Item 会话模型，服务端主动发起的审批请求，以及为什么不用 MCP 承载。完整资料见 [参考](bonus-protocol/protocol-notes.md#参考)。

## 扩展 · 模型路由

先读 [学习笔记](bonus-routing/routing-notes.md)，重点是供应商差异、按用途路由与故障转移。完整资料见 [参考](bonus-routing/routing-notes.md#参考)。
