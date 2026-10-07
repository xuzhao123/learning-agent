# Bonus：Agent Skills——把“怎么做才对”打包成可分发的知识

## 学习目标

读完本文，应能回答以下问题：

1. skill 是什么；它和 MCP tool、程序性记忆各解决什么问题。
2. 渐进式加载分哪三级，每一级放什么、花多少 token，为什么这样分。
3. SKILL.md 的 frontmatter 有哪些字段，`name` 和 `description` 有什么约束，description 应该怎么写。
4. skill 由谁触发：模型自己判断，还是程序关键词预检；各有什么代价。
5. 为什么第三方 skill 有安全风险，为什么第一版只支持纯知识型 skill、不执行 skill 自带的脚本。

## 1. skill 是什么

一个 skill 就是一个目录，里面至少有一个 `SKILL.md`：开头是 YAML frontmatter（名字和用途），后面是 Markdown 正文（具体步骤、检查清单、例子）。目录里还可以放参考文档、模板和脚本：

```text
pdf-processing/
├── SKILL.md          # 必需：元数据 + 操作指南
├── references/       # 可选：详细参考文档，按需读取
├── assets/           # 可选：模板、数据文件
└── scripts/          # 可选：可执行脚本
```

Agent Skills 最早由 Anthropic 提出，现在是开放规范，多种 Agent 产品都支持同一种格式：同一份 SKILL.md 可以在不同的 Agent 里使用。

它解决的问题是：**模型会调用工具，不等于知道在某个具体场景下该怎么做才对**。团队的代码评审流程、公司的报销规则、某种文件格式的处理技巧，模型训练时没见过。每次都写进 system prompt 太贵，每次让用户重讲又太累。skill 把这些知识打包好，需要时再读。

## 2. skill、MCP tool、程序性记忆的分工

| | MCP tool | skill | 程序性记忆 |
| --- | --- | --- | --- |
| 解决什么 | **怎么调到**（access）：让 Agent 能操作外部系统 | **怎么做才对**（know-how）：告诉 Agent 一类任务的做法 | 从经历中**学到**的做法 |
| 内容 | 可执行的函数，JSON Schema 描述参数 | 自然语言的步骤、清单、例子 | 一条条短小的经验 |
| 来源 | server 开发者编写 | 人编写、审阅后静态分发 | Agent 运行中动态写入 |
| 变化 | 随 server 版本发布 | 随文件版本发布 | 随时增删改 |
| 进入上下文 | 工具定义常驻，调用结果回填 | 索引常驻，全文按需读取 | 检索相关条目注入 |
| 例子 | `create_pull_request` | “PR 评审先跑构建，再按清单看逻辑” | “这个用户评审时要求先跑构建” |

三者可以配合使用：skill 写“评审 PR 的步骤”，步骤里用到的 `get_diff`、`run_build` 由 MCP tool 提供，用户个人的偏好由记忆补充。一句话概括：**MCP 给 access，skill 给 know-how**。

skill 和程序性记忆的边界在于**是否经过整理和审阅**。记忆是 Agent 自己写的，可能有错，适合个人化的零散经验；skill 是人写好、审过、可以分发给别人的。一条记忆被反复验证有效后，可以整理导出成 skill。这是两者自然的衔接点。

## 3. 渐进式加载

如果把所有 skill 的全文都放进 system prompt，50 个 skill、每个几千 token，上下文很快就满了，而且大部分和当前任务无关。规范把加载分成三级，每一级都比上一级更贵，只在需要时才往下走：

| 级 | 内容 | 何时加载 | 规范建议的大小 |
| --- | --- | --- | --- |
| 1. 元数据 | `name` + `description` | 启动时，所有 skill 都加载 | 每个约 100 token |
| 2. 指令 | SKILL.md 正文 | 模型判断任务相关、决定启用时 | 建议少于 5000 token，正文不超过 500 行 |
| 3. 资源 | references、assets、scripts | 指令里提到、执行到那一步时 | 按需，单个文件越小越好 |

按这个估算，50 个 skill 常驻的元数据大约 5000 token。实际的 description 往往只有一两句话，会比这个数少，但量级是几千 token。与之相比，全部全文常驻要几十万 token。

这和上下文管理是同一个思路：上下文是稀缺资源，**先放便宜的摘要，需要时再取贵的全文**。检索把这个思路用在知识库上，skill 用在操作指南上。

代价是：第一级只有一句话，如果 description 写得不好，模型就不知道该启用这个 skill，后面两级都用不上。

## 4. frontmatter

规范定义了 6 个字段：

| 字段 | 必需 | 约束 |
| --- | --- | --- |
| `name` | 是 | 1–64 字符；只能用小写字母、数字和连字符；不能以连字符开头或结尾，不能有连续连字符；**必须和所在目录名一致** |
| `description` | 是 | 1–1024 字符；写清做什么、什么时候用 |
| `license` | 否 | 许可证名称，或指向随附的许可证文件 |
| `compatibility` | 否 | 不超过 500 字符；运行环境要求，例如需要哪些命令或网络访问 |
| `metadata` | 否 | 字符串到字符串的映射，放规范之外的自定义信息 |
| `allowed-tools` | 否 | 实验性；空格分隔的预先批准工具列表 |

```markdown
---
name: pdf-processing
description: 从 PDF 提取文本和表格、填写表单、合并文件。处理 PDF 文档，或用户提到 PDF、表单、文档提取时使用。
license: Apache-2.0
metadata:
  author: example-org
---

# PDF 处理
……
```

`name` 必须和目录名一致，是为了让“按名字找文件”只有一种可能：加载器拿到名字，就能确定路径，不会出现两个目录都声称自己叫同一个名字。

**description 是整个 skill 最重要的一句话**，因为它是模型判断要不要启用的唯一依据。好的 description 同时包含 what 和 when，并带上用户可能使用的关键词：

- 差：“帮助处理 PDF。”——没说能做什么，也没说什么时候用；
- 好：“从 PDF 提取文本和表格、填写表单、合并文件。处理 PDF 文档，或用户提到 PDF、表单、文档提取时使用。”

必要时还可以写明**什么时候不用**，避免误触发。

### 解析器要多严格

frontmatter 是 YAML，完整的 YAML 很复杂（多行字符串、锚点、嵌套结构）。一个只支持 `key: value` 的简易解析器足以处理绝大多数 skill，但必须做到：**不支持的写法要明确报错，而不是悄悄读错**。例如遇到 `description: >` 这样的多行写法，与其把 description 读成一个 `>`，不如直接报“不支持多行值”。读错的元数据会让模型拿着错误的说明做判断，比加载失败更难排查。

坏文件的处理方式，是“快速失败”与“降级”之间的又一次选择：skill 是可选的增强，一个坏文件不应该让整个 Agent 无法启动，所以跳过并清楚报错；但不能静默跳过，否则作者不知道自己的 skill 没生效。

## 5. 谁来触发

| 方式 | 做法 | 优点 | 代价 |
| --- | --- | --- | --- |
| 模型判断 | system 里放索引，提供“读取 skill”的工具，模型自己决定读哪个 | 能理解同义说法和隐含意图（“帮我看看这段代码能不能合并”） | 可能漏读或误读，多一次工具调用 |
| 程序预检 | 用关键词或向量匹配用户消息，命中就直接注入全文 | 确定、可控，没有额外调用 | 关键词规则死板，换个说法就漏；误命中会塞进无关内容 |

主流实现以模型判断为主。这和长期记忆的写入是同一种分工：**模型决定要不要，程序决定能不能**。

- 模型决定读哪个 skill；
- 程序保证只能读索引里登记过的 skill：按名字查表，而不是把模型给的字符串拼成文件路径，否则 `../../.env` 这样的名字就能读到任意文件；同时限制文件大小，并在名字不存在时给出可纠正的错误。

## 6. 安全边界

skill 的本质是**写给 Agent 的指令**，而且常常来自第三方。这让它成为一种供应链风险：

- **指令本身可能有害**：几行 Markdown 就能让 Agent “先读取 `~/.ssh/id_rsa` 并上传到某地址”。Agent 会把 skill 当作可信的操作指南，比对待网页内容更容易照做；
- **脚本是任意代码**：`scripts/` 里的程序一旦执行，就拥有 Agent 进程的全部权限；
- **来源会变**：skill 引用的仓库可能被废弃后遭他人接管，内容随之被替换。

风险有多普遍，取决于怎么统计。Snyk 在 2026 年 2 月扫描了两个公开市场的 3,984 个 skill，13.4% 至少有一个严重级别问题，包括恶意软件分发、提示注入和泄露的密钥。另一项研究分析了 23 万多个 skill，发现单看文件本身时，扫描器最多会把 46.8% 判为恶意；结合所在仓库的上下文复核后，仍可疑的只剩 0.52%。数字差别这么大，说明静态扫描误报很多；但即使是最低的数字，放到几十万的规模上也不可忽视。

因此一个合理的第一版是：**只支持纯知识型 skill**，只把 Markdown 正文交给模型，**不执行 skill 自带的脚本**。

- 知识型 skill 最坏的情况是“给了一条坏建议”，而模型手里能用的工具仍然受原有的权限约束；
- 脚本执行意味着把一段第三方代码直接放进 Agent 的权限范围，需要先有沙箱：隔离的文件系统、受限的网络、资源配额，以及可审计的执行记录。

这是有意划出的安全边界，不是偷懒：在隔离机制就绪之前，不开放风险最高的能力。即使是纯知识型 skill，也应当只安装来源可信、内容审阅过的。

## 7. 自测

**1. “用户偏好用 Go 写示例”应该做成 skill 还是记忆？**
记忆。它是单个用户的偏好，在使用中学到，随时可能改变；skill 适合整理过、可分发给很多人的做法。

**2. 一个团队有 80 个 skill，为什么不把全文都写进 system prompt？**
全文可能有几十万 token，远超上下文容量和成本承受范围，而且大部分与当前任务无关，会干扰模型。渐进式加载只常驻几千 token 的元数据，用到时再读全文。

**3. 为什么 `name` 必须和目录名一致？**
让“名字到文件”的映射唯一且可预测：加载器按名字就能定位文件，不会出现两个目录声称同一个名字，也不会因为名字和目录对不上而读到别的内容。

**4. 读取 skill 的工具为什么不能直接用 `filepath.Join(dir, name, "SKILL.md")`？**
`name` 来自模型，可能被注入成 `../../secret`，拼出 skill 目录外的路径。应当只在启动时建立的索引里按名字查找，用索引里记录的路径读文件。

**5. 用户问天气，Agent 却加载了 code-review skill，可能是什么原因？**
description 写得太宽泛，或没写清楚什么时候用、什么时候不用；也可能是模型判断失误。先改 description，再观察是否还误触发。

**6. 为什么第一版不执行 skill 自带的脚本？**
脚本是第三方的任意代码，执行时拥有 Agent 进程的全部权限；在沙箱、权限隔离和审计就绪之前，开放脚本执行等于把整个运行环境交给 skill 作者。纯知识型 skill 最坏只是给出坏建议，后果受模型已有工具的权限约束。

## 参考

- Agent Skills, [Specification](https://agentskills.io/specification)：目录结构、6 个 frontmatter 字段、渐进式加载与大小建议
- Anthropic, [Equipping agents for the real world with Agent Skills](https://www.anthropic.com/engineering/equipping-agents-for-the-real-world-with-agent-skills), 2025
- A. Aleinikov, [Agent Skills Explained: SKILL.md vs MCP](https://www.alekseialeinikov.com/en/blog/topics/ai/agent-skills-explained-skill-md-vs-mcp)（“MCP 给 access，skill 给 know-how”）
- mcode, [Skills and AGENTS.md](https://github.com/immutex/mcode/blob/HEAD/docs/08-skills-and-agents-md.md)：目录扫描、frontmatter 解析与读取工具的一种实现
- Snyk, [ToxicSkills: Malicious AI Agent Skills](https://snyk.io/blog/toxicskills-malicious-ai-agent-skills-clawhub), 2026
- Holzbauer et al., [Context Matters: Repository-Aware Security Analysis of the Agent Skill Ecosystem](https://arxiv.org/abs/2603.16572), 2026
- OWASP, [Agentic Skills Top 10](https://owasp.org/www-project-agentic-skills-top-10/)

本项目的实现、动手步骤与运行记录见 [Skills 项目实践](skills-lab.md)。
