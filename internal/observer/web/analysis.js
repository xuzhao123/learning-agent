import { AUX_LABEL, clip, compact, el, parse, pretty, textOf, userPictures } from "./util.js";
// ---------- 确定性分析：主路径、绕路、回退（不调用模型） ----------
function isEmpty(v) {
  if (v === null || v === undefined || v === "") return true;
  if (Array.isArray(v)) return v.length === 0;
  if (typeof v === "object") {
    const arrays = Object.values(v).filter(Array.isArray);
    return Object.keys(v).length === 0 || (arrays.length > 0 && arrays.every((a) => a.length === 0));
  }
  return false;
}
// B0 的 turn/completed 带结局（max_steps 等）；进程退出码只说明“不是正常结束”，界面优先显示结局，看得出是没做完还是出错。
const OUTCOME_LABEL = { max_steps: "请求预算用完", context_error: "上下文放不下", hook_blocked: "被 hook 拦下", no_progress: "无进展熔断", repeated_action: "重复动作熔断",
  invalid_tool_calls: "工具调用格式错误", checkpoint_error: "检查点写入失败", encoding_error: "编码错误", permanent: "不可重试的错误", permission: "权限拒绝", unknown_not_repeatable: "结果未知且不可重做", cancelled: "已取消", error: "出错" };
export function endLabel(r, short) {
  if (!r.exit || r.exit === "exit 0" || !r.termination) return r.exit;
  const at = r.termination.indexOf("："), outcome = at < 0 ? r.termination : r.termination.slice(0, at);
  const detail = at < 0 ? "" : r.termination.slice(at + 1).replace(/^未完成：/, "");
  return (OUTCOME_LABEL[outcome] || outcome) + (detail && !short ? "（" + detail + "）" : "");
}
export function analyze(r) {
  const seen = new Map(), failed = new Map(), backtracks = [], detours = [];
  for (const c of r.calls) for (const t of c.tools) {
    const v = parse(t.result);
    const shown = v && typeof v === "object" && "result" in v ? v.result : v;
    t.state = !t.done ? (r.exit ? "lost" : "running") : v?.status === "denied" ? "denied" : t.isError || (v && typeof v === "object" && v.error) ? "error" : (isEmpty(shown) || shown?.status === "insufficient") ? "empty" : "ok";
    const key = t.name + " " + compact(t.args);
    t.repeatOf = seen.has(key) && seen.get(key) < c.n ? seen.get(key) : null;
    if (!seen.has(key)) seen.set(key, c.n);
    t.reason = { error: "返回错误", empty: "空结果", lost: "没有返回结果", denied: "未获用户确认" }[t.state] || (t.repeatOf ? `重复调用（请求 #${t.repeatOf} 已用相同参数）` : "");
    t.detour = !!t.reason;
    if (t.detour) detours.push(t);
    const prior = failed.get(t.name);
    t.retryOf = prior && prior.call.n < c.n ? prior : null;
    if (t.retryOf) backtracks.push({ from: prior, to: t });
    if (t.detour && t.state !== "running") failed.set(t.name, t); else if (t.state === "ok") failed.delete(t.name);
  }
  // 成本总量包含记忆辅助调用；主路径、绕路等行为分析只看主任务调用。
  const all = [...r.calls, ...r.aux];
  const sum = (f, list = all) => list.reduce((s, c) => s + (f(c.usage) || 0), 0);
  const toolCount = r.calls.reduce((s, c) => s + c.tools.length, 0);
  const last = r.calls.filter((c) => !c.compact).at(-1);
  return {
    detours, backtracks, toolCount,
    answer: last && last.choice && !last.tools.length ? last.choice.message.content || "" : "",
    mainLen: r.calls.length + toolCount - detours.length,
    calls: all.length, auxCalls: r.aux.length,
    tokens: sum((u) => u.total_tokens), auxTokens: sum((u) => u.total_tokens, r.aux),
    cached: sum((u) => u.prompt_tokens_details && u.prompt_tokens_details.cached_tokens),
    reasoning: sum((u) => u.completion_tokens_details && u.completion_tokens_details.reasoning_tokens),
    modelMs: all.reduce((s, c) => s + (c.ms || 0), 0), auxMs: r.aux.reduce((s, c) => s + (c.ms || 0), 0),
    totalMs: (r.tEnd || r.last || r.t0) - r.t0,
    clean: detours.length === 0 && backtracks.length === 0,
    failed: !!r.exit && r.exit !== "exit 0",
  };
}
export const SOURCE_LABEL = { codex: "Codex", "claude-code": "Claude Code" };
export function tokenParts(u) {
  const cached = (u.prompt_tokens_details && u.prompt_tokens_details.cached_tokens) || 0;
  const reasoning = (u.completion_tokens_details && u.completion_tokens_details.reasoning_tokens) || 0;
  return { cached, input: (u.prompt_tokens || 0) - cached, reasoning, output: (u.completion_tokens || 0) - reasoning };
}
export function toolSummary(t) {
  const args = clip(compact(t.args), 40);
  if (t.state === "running") return args + " → 执行中…";
  if (t.state === "lost") return args + " → 无结果";
  if (t.state === "empty") return args + " → 空结果";
  const v = parse(t.result);
  if (t.state === "error") return args + " → 错误：" + clip(v?.error ?? v, 40);
  const shown = v && typeof v === "object" && "result" in v ? v.result : v;
  if (t.name === "search_memory" && shown?.memories) return args + ` → ${shown.memories.length} 条记忆 · ` + shown.memories.map((m) => m.id).join(" / ");
  if (t.name === "forget_memory" && shown?.deleted) return args + ` → 已删除 ${shown.deleted}`;
  if (t.name === "remember_memory" && shown) return args + (shown.accepted ? " → 已登记，本轮结束时保存" : ` → 未登记：${shown.reason || ""}`);
  if (t.name === "load_skill" && shown) return args + (shown.content ? ` → 读取全文 ${shown.content.length} 字` : ` → ${shown.message || shown.error}`);
  if (t.name === "search_docs" && shown?.hits) return args + ` → ${shown.hits.filter((h) => h.accepted).length}/${shown.hits.length} 片段过阈值 · ` + shown.hits.map((h) => `${h.id} ${h.score.toFixed(3)}`).join(" / ");
  return args + " → " + clip(compact(shown), 60);
}

// 检索内容使用 textContent，来源当作数据展示，不执行片段中的 HTML 或指令。
function retrievalPanel(result) {
  const box = el("div", "retrieval");
  box.append(el("div", "meta", `top-${result.k} · 阈值 ${result.threshold} · 余弦相似度，不是正确概率`));
  if (result.status === "insufficient") box.append(el("div", "bad", "资料不足：没有候选通过阈值"));
  for (const h of result.hits) {
    const card = el("article", "doc-hit" + (h.accepted ? "" : " rejected"));
    card.append(el("strong", "", `[${h.id}] ${h.title}`), el("div", "meta", `score ${h.score.toFixed(4)} · ${h.accepted ? "通过阈值，仍需核对正文" : "低于阈值，不作证据"}`), el("div", "text", h.text || "正文未注入：低于阈值，仅保留候选元信息。"), el("div", "meta", "来源：" + h.source));
    box.append(card);
  }
  box.append(el("div", "meta", `${result.backend === "ark" ? "线上方舟" : result.backend === "local" ? "本地 Go" : ""} · ${result.model} · 语料 ${result.corpus}`));
  return box;
}

export function toolResult(t) {
  const value = parse(t.result), result = value?.result;
  if (t.name === "view_image" && result?.ref) {
    const box = el("div");
    box.append(userPictures([result]), el("pre", "", pretty(t.result)));
    return box;
  }
  return t.name === "search_docs" && Array.isArray(result?.hits) ? retrievalPanel(result) : el("pre", t.state === "error" ? "err" : "", pretty(t.result));
}

// ---------- 统一的事件项：轨迹行、迷宫节点、对比格子都指向它 ----------
export function items(r) {
  const list = [];
  const first = r.calls[0];
  if (!first) list.push({ id: "q", type: "user", turn: 1, run: r, label: "USER", sum: r.query });
  for (const c of [...r.calls, ...r.aux].sort((x, y) => x.n - y.n)) {
    if (c.aux) {
      const m = (c.choice && c.choice.message) || {};
      list.push({ id: "in" + c.n, type: "input", turn: c.turn, step: 0, run: r, call: c, hidden: true, label: "模型输入", sum: `${(c.req.messages || []).length} 条 messages` });
      list.push({ id: "m" + c.n, type: "aux", turn: c.turn, step: 0, run: r, call: c, label: c.purpose.toUpperCase(), bad: !!(c.end && !c.choice),
        sum: !c.end ? "等待模型响应…" : !c.choice ? `HTTP ${c.status} ` + clip(c.errorText || compact(c.res), 80) : AUX_LABEL[c.purpose] + "：" + clip(m.content, 100),
        start: c.start, stop: c.end, running: !c.end, tokens: c.usage.total_tokens });
      continue;
    }
    const where = { turn: c.turn, step: c.step, run: r, call: c };
    const messages = c.req.messages || [];
    if (c.promptChanged) list.push({ id: "system" + c.n, type: "system", ...where, step: c.userMessage ? 0 : c.step,
      label: "SYSTEM", sum: `${c.n === 1 ? "初始系统规则" : "系统规则 / 工具定义变化"} · ${(c.req.tools || []).length} 个工具` });
    if (c.userMessage) list.push({ id: "u" + c.turn, type: "user", ...where, step: 0, label: "USER", sum: textOf(c.userMessage), message: c.userMessage });
    // 完整输入是每次请求的快照，保留详情入口；主表只展示新发生的事件。
    list.push({ id: "in" + c.n, type: "input", ...where, hidden: true, label: "模型输入",
      sum: `${messages.length} 条 messages · ${messages.map((m) => m.role).join(" → ")}` });
    const m = (c.choice && c.choice.message) || {};
    const think = m.reasoning_content ? `思考 ${m.reasoning_content.length} 字 → ` : "";
    let sum = "等待模型响应…", bad = false;
    if (c.end && !c.choice) { sum = `HTTP ${c.status} ` + clip(c.errorText || compact(c.res), 80); bad = true; }
    else if (c.invalidSummary) { sum = "摘要返回工具调用，执行器已拒绝执行"; bad = true; }
    else if (c.end) sum = think + (c.compact ? "上下文摘要：" + clip(m.content, 80) : c.tools.length ? "调用 " + c.tools.map((t) => t.name).join("、") : "回答：" + clip(m.content, 80));
    list.push({ id: "m" + c.n, type: c.compact ? "compacted" : "message", ...where, label: c.compact ? "COMPACTED" : "ASSISTANT", sum, bad,
      start: c.start, stop: c.end, running: !c.end, tokens: c.usage.total_tokens });
    for (const t of c.tools) list.push({ id: `t${c.n}-${t.index}`, type: "tool", ...where, tool: t, label: t.name,
      sum: toolSummary(t), bad: t.detour, start: t.start, stop: t.done, running: t.state === "running" });
  }
  const a = r.calls.filter((c) => !c.compact).at(-1);
  if (a && a.choice && !a.tools.length && a.choice.message.content) list.push({ id: "ans", type: "answer", turn: a.turn, step: a.step, run: r, call: a, hidden: true, label: "answer", sum: a.choice.message.content });
  const last = r.calls.at(-1);
  list.push({ id: "end", type: "process", turn: last?.turn || 1, step: last?.step || 0, run: r, call: last, label: "exit", sum: endLabel(r) || "运行中", bad: !!r.exit && r.exit !== "exit 0" });
  return list;
}

