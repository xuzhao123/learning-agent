# Day 28 项目实践：复盘后的四处修改

先读 [学习笔记](day-28-notes.md)，本周的串联图与遗留问题的判断见 [第四周复盘](../week-04-review.md)。对应能力：H23 客户端（界面显示结局）、H20 规划与长任务（请求预算与终止条件）、H10 权限策略（规则里的域名）。

复盘里判为“修”的三项，加上用户要求去掉的每轮请求上限：

| 修改 | 代码 | 起因 |
| --- | --- | --- |
| 界面显示结局，不显示退出码 | [analysis.js](../../internal/observer/web/analysis.js) | D27 预算耗尽被显示成“失败：exit status 1” |
| 默认上下文窗口 256K | [settings.go](../../internal/observer/settings.go) | 64K 下跨文件任务反复压缩，做不完 |
| 规则里的域名转小写 | [permission.go](../../internal/permission/permission.go) | D26 review 发现 `WebFetch(domain:GitHub.com)` 永远匹配不到 |
| 请求次数默认不限 | [llm.go](../../internal/llm/llm.go)、[main.go](../../main.go)、[server.go](../../internal/observer/server.go) | 预算难定：编码任务两次在收尾前用完 |

## 1. 结局：读协议，不读退出码

B0 的 `turn/completed` 本来就带结局（如 `max_steps`），观测台存进 `r.termination`，以前只用进程的 `exit status 1`。现在凡是显示“没正常结束”的地方（顶栏、统计、时间轴末尾、停止卡片、续聊时的中断标记）都改用 `endLabel`：

```js
function endLabel(r) {
  if (!r.exit || r.exit === "exit 0" || !r.termination) return r.exit;   // 正常结束或旧存档：照旧
  const at = r.termination.indexOf("："), outcome = at < 0 ? r.termination : r.termination.slice(0, at);
  const detail = at < 0 ? "" : r.termination.slice(at + 1).replace(/^未完成：/, "");
  return (OUTCOME_LABEL[outcome] || outcome) + (detail ? "（" + detail + "）" : "");
}
```

退出码仍决定“是否正常结束”（红色与否），结局只决定显示什么。续聊开始时把 `r.termination` 清空，避免上一轮的结局留到这一轮。

## 2. 默认窗口与请求次数

内置默认值改为 `context_window: 262144`、`max_steps: 0`，设置范围放宽到 0–1000。“不限”在 agent 里是一个足够大的数，预算的计数与比较一行不改：

```go
const Unlimited = 1 << 30   // -max-steps 0 在内部的取值；日志经 LimitText 写成“不限”
```

难点在存档。观测台在对话开始时把设置拍成快照写进 start 事件，旧存档没有这一项，`max_steps` 为 0，含义是“按 agent 默认值（10）”。如果设置里的 0 原样写进快照，旧存档就会被当成“不限”。所以快照里用 -1 表示不限，启动 agent 时再翻译回 `-max-steps 0`：

```go
if snap.MaxSteps == 0 {
	snap.MaxSteps = -1 // 设置里 0 表示不限；快照里 0 留给“旧存档没有这项、按 agent 默认值”
}
```

去掉上限后，循环靠重复动作熔断（`repeated_action`）、无进展熔断（`no_progress`）和人按停止收住；命令行直接运行时默认仍是 10。

## 3. 域名小写

调用一侧，`open_page` 的地址经 `host()` 转成小写后匹配 `domain:` 模式；规则一侧以前原样保存，于是规则里只要有大写字母就永远不命中。现在 `Add` 时把 `domain:` 后面的部分转小写，显示时仍保留用户的原文。

## 4. 验证

全部用 DeepSeek 真实运行，观测台的运行在常驻 `test` 账号下，记录保留。

| 检查 | 做法 | 结果 |
| --- | --- | --- |
| 命令行不限 | `-max-steps 0`，问一道乘法 | 日志 `Limits: max_steps=不限`、`Model request: 1/不限`，正常回答 |
| 负数被拒 | `-max-steps -1` | `max-steps 不能为负（0 表示不限）` |
| 观测台不限 | 默认设置开新对话 | start 事件快照 `max_steps: -1`、`context_window: 262144`；agent 日志 `max_steps=不限` |
| 结局显示 | `test` 个人设置临时改成 2 次请求，让它连跑三条 bash 命令 | 协议结局 `max_steps`；顶栏“未正常结束 · 请求预算用完（已达到 2 次模型请求上限（包含摘要））”，停止卡片“agent 已停止：请求预算用完（…）”。验证后个人设置恢复为继承 |
| 域名规则 | `-browser -deny 'WebFetch(domain:Go.Dev)'`，让它打开 `https://go.dev/doc/` | `open_page` 被拒：`规则 deny: WebFetch(domain:Go.Dev)`，模型在回答里说明需要调整权限，没有换网址绕过 |

旧存档（start 事件里没有 `max_steps`）仍按 agent 默认值启动，这一点只做了代码阅读。

## 5. 当前没有覆盖的范围

- 结局的中文名只覆盖 agent 当前会给出的几种；遇到新的结局就原样显示英文代码。
- 命令行默认仍是 10 次请求，只有观测台默认不限。
- 其余遗留问题（批准按钮、预算耗尽时的最后检查、首字延迟口径、T5）按复盘的判断推迟，见 [第四周复盘](../week-04-review.md#d27-遗留问题的处理建议)。
