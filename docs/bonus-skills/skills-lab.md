# Skills 项目实践：给 Agent 加上知识型 skill

原理见 [Skills 学习笔记](skills-notes.md)。代码在 [skills.go](../../skills.go)，第一个 skill 是 [skills/code-review/SKILL.md](../../skills/code-review/SKILL.md)。

## 1. 实现概览

```text
启动（-skills）
  loadSkills("skills")：扫描 skills/*/SKILL.md
    readSkill → parseFrontmatter（手写，约40行）→ 校验 name/description/compatibility
    坏文件：打印 Skill error 并跳过；最后打印 Skills: loaded=N names=…
  有可用 skill 时：system 末尾追加索引（名字：description）+ 注册 load_skill 工具

运行
  模型判断相关 → load_skill(name) → 按索引查到路径 → 重新读取并校验 → 返回正文
  名字不存在 → 返回 {error: unknown_skill, available: [...]}（结果，不是 error）
```

| 设计点 | 本项目的选择 |
| --- | --- |
| 开关 | `-skills`，默认关闭，和 `-rag`、`-memory` 一样显式开启，不改变其他实验的 system prompt |
| 目录 | 固定为项目根目录下的 `skills/`，只扫一层 `skills/<名字>/SKILL.md` |
| 解析 | 不引入 YAML 库：只读顶层 `key: value`，去掉成对引号；缩进行视为上一个键的嵌套值（如 `metadata`）并跳过；遇到 `\|`、`>` 多行写法、重复键、缺少分隔线时报错 |
| 校验 | `name` 必填、≤64、`^[a-z0-9]+(-[a-z0-9]+)*$`、与目录名一致；`description` 必填、≤1024 字符；`compatibility` ≤500 字符；文件 ≤64KB |
| 坏文件 | 逐个报错、跳过，不影响启动（降级）；没有任何可用 skill 时不注册 `load_skill`，system 也不加索引 |
| 触发 | 模型判断：system 索引 + `load_skill` 工具；程序不做关键词预检 |
| 读取安全 | 只在索引里按名字查表、用记录的路径读文件，模型给的字符串不拼进路径；调用时重新读取并校验，启动后被改坏的文件不会被返回 |
| 未知名字 | 返回结构化结果，而不是 Go error：错误会触发原有的重试（默认再试 2 次），但同样的名字重试也没用。这和 MCP 把可纠正错误放进 `isError` 结果是同一个思路 |
| 第三级 | v1 不读 references/assets，不执行 scripts |

## 2. 读代码

按调用顺序读 [skills.go](../../skills.go)：

1. **`parseFrontmatter`**：先统一换行、去掉 BOM，再用 `strings.Cut` 找结尾的 `\n---\n`。逐行处理时注意报错行号要加 2（第 1 行是开头的 `---`）。
2. **`readSkill`**：解析之后用一个 `switch` 依次检查各项约束，第一个不满足的就返回；成功时同时返回正文，供 `loadSkill` 复用。
3. **`loadSkills`**：`filepath.Glob` 扫描，错误逐条打印，最后打印汇总。
4. **`skillPrompt`**：生成 system 里的索引，同时写明使用规则：相关时先 `load_skill` 再照做，无关时不要加载。
5. **`loadSkill`**：在索引里找名字；找到就重新 `readSkill` 返回正文，找不到就返回可用名字列表，让模型改正。

接入点：[main.go](../../main.go) 在 `runAgent` 之前调用 `loadSkills` 并注册工具；[react.go](../../react.go) 的 `systemPrompt` 末尾追加 `skillPrompt()`；[tools.go](../../tools.go) 的 `runTool` 增加 `load_skill` 分支。

## 3. 第一个 skill：code-review

[skills/code-review/SKILL.md](../../skills/code-review/SKILL.md) 由 Day 5 的一条程序性记忆改写而来：用户要求记住“代码评审先跑构建，再看逻辑”。改写时做了三件事：

- **description 写清 what + when**：“按先构建、再看逻辑的固定顺序评审 Go 代码……用于用户请求代码评审、review、检查一段 Go 代码或补丁有没有问题、能不能合并时；不用于解释概念或写新功能。”最后半句用来减少误触发；
- **把一句话展开成可执行的步骤**：第一步 `go build ./...`、`go vet ./...`、`gofmt -l .`；第二步逐项检查错误处理、边界、并发、资源、上下文、输入信任；最后规定输出格式；
- **写明没有执行工具时怎么办**：本 Agent 没有执行命令的工具，所以 skill 要求模型“不要假装已经运行”，而是请用户运行并贴出输出。skill 写的是做法，能不能执行取决于 Agent 实际有哪些工具，两者要对得上。

frontmatter 里的 `metadata` 是嵌套写法，用来验证解析器会正确跳过缩进行。

## 4. 动手与真实运行

```sh
go build -o bin/learning-agent .
```

### 4.1 坏文件不 crash，无关任务不加载

临时放了三个不合规的 SKILL.md（运行后已删除），同时问一个无关问题：

```sh
./bin/learning-agent -skills -max-steps 4 -question '北京今天天气怎么样？'
```

```text
Skill error: skills/Bad-Name/SKILL.md: name "Bad-Name" 不合规：只能用小写字母、数字和单个连字符，最长64
Skill error: skills/no-frontmatter/SKILL.md: 缺少开头的 --- frontmatter
Skill error: skills/wrong-dir/SKILL.md: name "other-name" 与目录名 "wrong-dir" 不一致
Skills: loaded=1 names=code-review
…
Round 1
Usage: purpose=main prompt_tokens=789 completion_tokens=176 cached_tokens=0
我目前没有查询实时天气的相关工具能力，无法为你提供北京今天的天气情况，你可以通过气象类APP、天气预报网站等渠道获取最新天气信息。
Termination: no_tool_calls
```

三个坏文件各自给出明确原因，进程继续启动；模型没有调用 `load_skill`，一次请求就结束了。code-review 的全文没有进入上下文，这就是渐进式加载的意义。

### 4.2 相关任务主动加载并照做

```sh
./bin/learning-agent -skills -max-steps 5 -question '帮我 review 一下这段 Go 代码，能合并吗？
（一个忽略 os.Open / io.ReadAll 错误、没有关闭文件、按 "=" 分割后直接取 parts[1] 的 readConfig 函数）'
```

真实运行（正文省略细节）：

```text
Skills: loaded=1 names=code-review
Round 1
Usage: purpose=main prompt_tokens=890 completion_tokens=104 cached_tokens=0
Action [call_d4jf…]: load_skill
Action Input: {"name": "code-review"}
Observation [call_d4jf…]: {…"tool":"load_skill","result":{"content":"# Go 代码评审\n\n来源：…","name":"code-review"},"attempts":1}

Round 2
Usage: purpose=main prompt_tokens=1548 completion_tokens=5308 cached_tokens=0
### 构建检查
当前提供的是函数片段，若未导入`os`、`io`、`strings`包会直接触发编译错误……
需要你在项目目录执行以下三条命令并提供输出……
1. `go build ./...`  2. `go vet ./...`  3. `gofmt -l .`
### 问题清单
#### 必须修 …… 忽略错误 / 文件未关闭 / parts[1] 越界 panic（末尾换行产生空行即触发）
#### 建议修 …… Split 应为 SplitN；\r\n；空白与注释行
#### 可选 …… 签名改为返回 (map[string]string, error)
### 结论
**不能合并** ……
Termination: no_tool_calls
```

对照 skill 逐项看：先写“构建”一节，没有假装运行，而是请用户执行三条命令；问题按“必须修 / 建议修 / 可选”分级，每条有位置、原因和建议；最后给出“能否合并”的结论。读取全文让第二次请求的输入从 890 增加到 1548 token，只有在用到时才付这笔成本。

## 5. 已知边界

- 只验证了“名字存在”的路径；`unknown_skill` 分支只做了代码阅读，没有让真实模型触发（模型照着索引传名字，没有传错）。
- 只观察了 2 个问题，不能说明触发的准确率。误触发和漏触发需要更多样的问题才能评估。
- 不支持 YAML 多行值和列表；`allowed-tools` 被解析但没有使用。
- skill 内容按可信指令处理，没有来源校验或签名。目前只有项目自己写的 skill，安装第三方 skill 前需要人工审阅。
- 观测台的 Skills 中心可以查看、启停、新建与删除 skill，对话流显示本轮启用的 skill；新建只写 SKILL.md，同样只支持知识型。
