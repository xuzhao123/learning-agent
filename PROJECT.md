# 学习 Agent：进度与待办

## 目标与阅读入口

以 Manus「Agent 全栈工程师」岗位要求为长期目标，用 Go 逐步实现并理解 Agent。课程安排见 [学习计划](plan.md)，能力覆盖见 [JOB_REQUIREMENTS.md](JOB_REQUIREMENTS.md)。

启动、配置、学习目录和源码导航统一见 [README.md](README.md)。协作规则统一见 [AGENTS.md](AGENTS.md)。本文件只记录当前进度、理解情况与仍未解决的问题。

## 当前实现范围

一个 Go 模块、一个入口：`go run .` 运行 agent，`go run . observe` 运行观测台，`go run . queue` 运行队列。一个 agent 进程执行一个任务；检索与记忆在任务进程内调用，观测台、队列和子 agent 通过子进程执行任务。

已覆盖 Day 1–14 与 Skills、子 agent、模型路由扩展：原生 Tool Calling、并行工具、循环护栏、上下文压缩、向量检索、长期记忆、MCP、取消超时、检查点、队列、浏览器、Bash 沙箱与 OpenTelemetry 调用链。MCP 支持 stdio 和 Streamable HTTP，普通问答可连接多个 server；工具列表只在连接时读取。

## 课程进度

功能状态依据当前代码与各篇实践中已有的验证记录；本次文档整理未重新进行模型验收。功能运行通过与用户已经掌握分别记录，未确认的理解保持“待反馈”。

| 课程或模块 | 实现与验证状态 | 理解情况 | 实践与证据入口 |
| --- | --- | --- | --- |
| Day 1：工具调用 | Go 原生工具调用循环与并行执行；已有真实运行记录 | 待反馈 | [Day 1 实践](docs/day-01/day-01-lab.md) |
| Day 2：循环护栏 | 请求预算、退避重试、重复动作熔断与停止摘要；故障场景已有记录 | 待反馈 | [Day 2 实践](docs/day-02/day-02-lab.md) |
| Day 3：上下文 | 已按设计实现 Transcript/View、工具截断、集中清理与摘要；已有长对话和工具链验证 | 用户已指出输出上限、配额与原话保留问题；完整复述待确认 | [Day 3 实践](docs/day-03/day-03-lab.md)、[设计](docs/day-03/context-system-design.md) |
| Day 4：检索 | 进程内检索，本地/线上向量可选；已有召回、引用及库外拒答对比 | 待反馈 | [Day 4 实践](docs/day-04/day-04-lab.md) |
| Day 5：长期记忆 | 写入、来源、TTL、遗忘和跨进程召回已有验证；Contextual Retrieval 升级已编译，真实对比仍待按清单运行 | 待反馈 | [Day 5 实践](docs/day-05/day-05-lab.md) |
| Day 6：MCP client | SDK 与手写 JSON-RPC 接入；两代协议及真实工具调用已有记录 | 待反馈 | [Day 6 实践](docs/day-06/day-06-lab.md) |
| Day 7：MCP server | calculator server、自举调用与参数校验已有验证；第一周复盘已完成 | 待反馈 | [Day 7 实践](docs/day-07/day-07-lab.md)、[复盘](docs/week-01-review.md) |
| Day 8：运行时 | 整次/单次超时、取消、四种结局、分类重试与两段式停止已有验证 | 待反馈 | [Day 8 实践](docs/day-08/day-08-lab.md) |
| Day 9：恢复 | 检查点、写前日志、单执行者锁与中断续跑已有验证 | 用户追问 repeatable 误标与只存 View 的续跑正确性，回答见 [Q13](docs/deep-questions.md#q13)、[Q14](docs/deep-questions.md#q14)；复述待确认 | [Day 9 实践](docs/day-09/day-09-lab.md) |
| Day 10：任务队列 | 固定 worker、去重、任务级重试和完成结果复用已有验证 | 待反馈 | [Day 10 实践](docs/day-10/day-10-lab.md) |
| Day 11：浏览器 | 只读浏览与实时画面、停止续聊已有记录；去掉 UA 伪装后的方舟复测（Day 14）：open_page 正常，web_search 不可用 | 待反馈 | [Day 11 实践](docs/day-11/day-11-lab.md) |
| Day 12：Bash 沙箱 | bubblewrap、Go BPF、cgroup、白名单代理与用户审批已有实现及实践记录；组合风险仍见下表 | 待反馈 | [Day 12 实践](docs/day-12/day-12-lab.md) |
| Day 13：可观测性 | OpenTelemetry Go SDK 打点，GenAI 语义约定；观测台、agent、子 agent、队列、MCP server 跨进程成树；本地文件与可选 OTLP 导出；观测台“调用链”与由 span 汇总的指标。CLI、观测台（子 agent + MCP）、队列、kill -9 续跑与 Jaeger 导出已用真实方舟运行 | 待反馈 | [Day 13 实践](docs/day-13/day-13-lab.md) |
| Day 14：综合与复盘 | 真实方舟综合演练：子 agent + 浏览器 + Bash 沙箱，中途停止后续聊完成，答案经网页核实；调用链两轮共 48 个 span。顺带完成 D11 复测：open_page 正常，web_search 被必应页面跳转阻断。发现并修复两处：续聊后子任务重跑（续聊沿用被打断的检查点 ID，spawn_agent 支持 task_id 引用），web_search 跳转错误不再白白重试；均已用真实模型复验。第二周复盘已完成 | 待反馈 | [Day 14 实践](docs/day-14/day-14-lab.md)、[复盘](docs/week-02-review.md) |
| 扩展：Skills | 索引常驻、正文按需加载、坏文件处理已有验证 | 待反馈 | [Skills 实践](docs/bonus-skills/skills-lab.md) |
| 扩展：子 agent | 独立上下文、子进程、权限继承、幂等与父任务恢复已有验证 | 用户追问换措辞导致哈希失配，回答见 [Q15](docs/deep-questions.md#q15)；复述待确认 | [子 agent 实践](docs/bonus-subagents/subagents-lab.md) |
| 扩展：模型路由 | 方舟/DeepSeek 手动路由、工具调用、子 agent 继承与续聊已有记录 | 待反馈 | [路由实践](docs/bonus-routing/routing-lab.md) |
| 观测台 | 对话/续聊/停止、Turn/Step、原始输入输出、轨迹/迷宫/对比、能力中心与审批已有实现和运行记录 | 展示规则已多次讨论；整体理解待反馈 | [观测台说明](docs/observer/observer-notes.md) |
| 文档整理 | 学习入口、文档职责、当前进度与阅读层次已统一；旧轨迹文件及过期表述已清理；本地链接、锚点与公共文档表格检查通过，程序代码保持整理前状态 | 阅读效果待反馈 | [README](README.md)、[深度问题](docs/deep-questions.md) |
| Agent 体系 Wiki | 15 篇模块分基础理解与技术展开，覆盖状态契约、预算算法、检索算例、并发提交、协议报文、恢复窗口与隔离规则；17 段 Go 片段整体编译，数学算例、链接与导航检查通过；明确示意方案与已有实现边界，原始 notes 和程序代码保留 | 技术深度与理解待反馈 | [Wiki 入口](wiki/README.md)、[技术索引](wiki/README.md#技术展开索引)、[术语表](wiki/TERMS.md) |
| Agent 面试准备 | 按 17 个主题整理 126 题，含参考回答、追问、误区、6 道系统设计与 6 道 Go 编码题；检查五份 GitHub 资源的入口与部分内容，明确社区答案及项目实现边界；题号与导航检查通过，2 段 Go 示例编译、JSON 与数学算例校验通过 | 口述、编码与独立推演待反馈，不记为生产经验 | [题库](wiki/16-interview-preparation.md)、[优先题](wiki/16-interview-preparation.md#先练这-24-题) |

## 当前待办与实现边界

已完成的改造归到对应实践文档，不再作为待办重复列出。下表保留当前仍需处理或评估的范围；具体限制以源码及对应实践为准。

| 范围 | 当前问题或限制 | 后续方向 |
| --- | --- | --- |
| 循环控制 | 只检测相邻重复；A/B 交替调用不能被熔断识别 | 结合滑动窗口、结果变化识别无进展循环，见 [Q1](docs/deep-questions.md#q1) |
| 检索结果 | 低分正文已过滤，标题与来源仍可能误导模型；资料不足后的改写重搜缺少限制，库外回答规则仍有歧义 | 收紧返回内容与检索次数，明确拒答规则，见 [Q2](docs/deep-questions.md#q2) |
| 记忆行为 | 已注入记忆仍可能被重复检索；遗忘权重与敏感信息过滤需更多样本 | 评估漏记、误记、误召回和淘汰结果，见 [Q5–Q8](docs/deep-questions.md#q5) |
| 模型请求与路由 | 主请求失败即停止；只做手动路由；推理强度原样透传 | 按错误类型有限重试，研究按用途路由与参数映射 |
| MCP 权限 | 第三方 stdio server 继承环境变量 | 启动环境改为最小白名单 |
| MCP 契约 | 不处理 list_changed；schema 错误可读性不足，远程鉴权未完成 | 动态更新、错误转译与鉴权按后续任务引入 |
| Skills | 触发准确率未系统评估 | 纳入评测集；知识内容与执行权限分开 |
| 工具取消 | 不配合取消的工具可能在调用方放弃等待后继续执行；Bash 已使用 cgroup 清理 | 按工具检查取消和资源回收，见 [Day 8](docs/day-08/day-08-lab.md) |
| 父子进程 | 父进程被 kill -9 后，子进程可能暂时继续执行并占有任务锁；task_id 引用靠模型遵守规则，模型若仍改写 task 重派，会新开子任务 | 父死亡信号或锁释放等待；续跑时把已派出的子任务列进上下文，见 [Q15](docs/deep-questions.md#q15) |
| 检查点 | 没有版本兼容标记、fsync 与自动清理；只存 View，续跑后再压缩的素材变少，首次用量只能粗估；观测台在中断后的续聊沿用并覆盖旧检查点，用户另起一题时旧任务的问题与结局被抹掉，“一个ID一个问题”不再成立 | 持久化版本和保留策略；Transcript 追加另存、usage 写入检查点，见 [Q14](docs/deep-questions.md#q14)；每轮新 ID 加续接链、子任务按链查找、撤掉沿用父 ID，见 [Q21](docs/deep-questions.md#q21) |
| 副作用恢复 | 模型看到“结果未知”后仍可能决定重做；repeatable 按工具名写死，误标会静默重做 | 幂等键、状态查询与高风险确认；MCP 注解仅对可信 server 采信，见 [Q13](docs/deep-questions.md#q13) |
| 浏览器网络 | 已拦截起始 URL、跳转和子资源的内网请求；`100.64.0.0/10`（含阿里云元数据 100.100.100.200）等特殊地址段未判为内网，DNS 重绑定与 WebSocket 仍有缺口 | 补全特殊地址段或只放行全局单播；用受控出口限制实际连接，见 [Q18](docs/deep-questions.md#q18) |
| 网页内容 | 注入防护主要依赖 system 规则；工具失败后模型仍可能凭旧知识回答 | Week 3 注入攻防与回答可靠性评测 |
| 浏览器执行 | 浏览器未纳入 Bash 沙箱；设置 CHROME_NO_SANDBOX=1 会关闭 Chrome 自身沙箱 | 保留 Chrome 自身沙箱，后续研究独立浏览器隔离 |
| 浏览器效率 | 必应不向无头浏览器返回结果，web_search 当前不可用（已改为不重试并提示改用 open_page）；同页多次 find 会重新加载，子 agent 因此耗尽预算（Day 11、Day 14） | 正式搜索 API、正文缓存或会话句柄 |
| 执行隔离 | bubblewrap 共享内核，seccomp 使用黑名单（主隔离靠 namespace 与去权限，seccomp 只缩小内核攻击面）；没有 systemd 用户会话时不施加 cgroup 限制 | 不可信或多租户场景改白名单并加 gVisor/微虚拟机，见 [Q16](docs/deep-questions.md#q16) |
| 组合工具 | 浏览器仍能通过请求 URL 外发 Bash 取得的数据，这一条只靠 system 规则与模型判断；允许域名内的路径和数据没有细分权限（开放重定向已实测拦下，大域名与域前置仍是风险） | 跨工具数据流与高风险审批，见 [Q12](docs/deep-questions.md#q12)、[Q17](docs/deep-questions.md#q17)、[Q18](docs/deep-questions.md#q18) |
| 运行数据 | 工作目录总大小、截图、检查点与 trace 文件缺少配额或自动清理 | 磁盘配额与保留期限 |
| 可观测性 | 重试等事件用 span event（OTel 已宣布弃用该 API），终端仍是文本行；kill -9 丢失未结束的 span，按 trace 统计的成功率、尾部延迟和工具失败率会有偏；无采样、无 Metrics SDK 与告警；GenAI 约定仍在变 | 带 trace_id 的结构化日志经 Logs API 导出；评测结局以检查点为准、trace 只做归因，检查点存 trace_id，长 span 开始时也落一行，见 [Q19](docs/deep-questions.md#q19)；指标与告警按 Week 3 评测课程补齐 |
| 评测 | 现有小样本参与过调试，独立留出集、规模化评分与指标告警未建立 | 按后续评测课程补齐 |

## 理解确认

每天的自测在对应笔记中，设计取舍集中见 [深度问题](docs/deep-questions.md)，第二周的待确认清单见 [复盘](docs/week-02-review.md#待你确认的理解)。优先确认：

- 循环：解析、调度、状态维护与终止各负责什么。
- 上下文：保留原话、摘要和最近消息组怎样决定压缩后的大小。
- 记忆：原文、来源与派生索引为什么分开；何时读、写、忘。
- 运行时：结果未知为什么不能当失败；写前日志覆盖哪个崩溃窗口。
- 多任务：任务队列和子 agent 的责任有什么不同。
- 安全：namespace、cgroup、seccomp 各限制什么；为什么断网的 Bash 加浏览器仍能外发数据。
- 可观测性：traceparent 怎样让子进程挂到父 span 下；为什么 kill -9 后会出现孤儿 span；Simple 与 Batch 处理器的取舍。
