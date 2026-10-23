import { agentCard, renderSubPanel } from "./agents.js";
import { analyze, endLabel, tokenParts, toolResult } from "./analysis.js";
import { WEB_TOOLS, webCard } from "./browser.js";
import { start } from "./composer.js";
import { exploreCall, exploreLabel } from "./explore.js";
import { claimViz, md, parkViz } from "./md.js";
import { openMCP, openMemory, openSkills } from "./pages.js";
import { render } from "./render.js";
import { approvalCard, askCard, bashCard, netCard, proxyNetCards } from "./sandbox.js";
import { loadRuns, rewindTurn, syncComposer, toolOn } from "./shell.js";
import { $, AUX_LABEL, LAB_TITLE, MEM_KIND, MEM_STATUS, MEM_TYPE, RAG_LAB_TITLE, callPosition, clip, compact, el, fold, ico, idleText, kfmt, parse, pretty, rawIO, sec, textOf, ui, userPictures } from "./util.js";
// agent 终端行形如 "Memory recall: id=M1 kind=semantic score=0.48 content=…"；content 放在最后，可含空格。
export function memFields(text) {
  const out = {}, at = text.indexOf("content=");
  if (at >= 0) out.content = text.slice(at + 8);
  for (const part of (at >= 0 ? text.slice(0, at) : text).trim().split(" ")) {
    const k = part.indexOf("=");
    if (k > 0) out[part.slice(0, k)] = part.slice(k + 1);
  }
  return out;
}
function memoryCard(r, turn, phase) {
  const list = r.mem.filter((m) => m.turn === turn && m.phase === phase);
  const aux = phase === "start" ? r.aux.filter((c) => c.turn === turn && !r.calls.some((x) => x.turn === turn && x.n < c.n)) : [];
  if (!list.length && !aux.length) return null;
  const count = (type) => list.filter((m) => m.type === type && m.text !== "none").length;
  const load = list.find((m) => m.type === "load"), ret = list.find((m) => m.type === "retrieve")?.fields, idx = list.find((m) => m.type === "index" && m.fields.plan)?.fields;
  let label = phase === "start"
    ? (load?.fields.skipped ? "续聊沿用首轮调入的长期记忆" : `调入长期记忆 · ${count("recall")}/${load?.fields.total ?? "?"} 条写进 system`)
    : count("write") || count("refresh") ? `写入长期记忆 · 新增 ${count("write")}` + (count("refresh") ? ` · 刷新 ${count("refresh")}` : "") : "本轮没有写入长期记忆";
  if (ret) label += ` · 向量 ${ret.vector} / BM25 ${ret.bm25} → 融合 ${ret.fused} → 重排 ${ret.rerank === "ok" ? "完成" : ret.rerank === "failed" ? "失败" : "跳过"}`;
  if (count("expire")) label += ` · 过期 ${count("expire")}`;
  if (count("evict")) label += ` · 淘汰 ${count("evict")}`;
  if (count("skip") || count("error")) label += " · 有未保存内容";
  if (ret && ret.content !== "无") label += " · 有降级";
  const rows = [];
  if (idx) rows.push(el("div", "meta", `索引（${idx.plan}）：${idx.chunks} 个片段 · 已补背景 ${idx.contextual} · 旧记忆无来源 ${idx.no_source} · 背景待处理 ${idx.pending}` + (Number(idx.too_long) ? ` · 背景超限 ${idx.too_long}` : "") + ` · 向量缓存 ${idx.vectors_cached} / 新算 ${idx.vectors_built}`));
  if (ret && ret.content !== "无") rows.push(el("div", "err", "降级：" + ret.content));
  for (const m of list.filter((m) => !["load", "index", "retrieve"].includes(m.type) && m.text !== "none")) {
    const row = el("div", "mem-line" + (["expire", "evict", "skip", "error", "drop", "hint"].includes(m.type) || (m.type === "contextualize" && m.fields.status !== "ok") ? " drop" : ""));
    const f = m.fields, meta = [f.id, MEM_KIND[f.kind] || f.kind, f.status && (MEM_STATUS[f.status] || f.status), f.vector && "向量 " + f.vector, f.bm25 && "BM25 " + f.bm25, f.rrf && "RRF " + f.rrf, f.rerank && "重排 " + f.rerank, f.final && "最终 " + f.final, f.backup && "保守规则选入", f.score && "score " + f.score, f.retention && "保留分 " + f.retention, f.expire && f.expire !== "never" && "过期 " + new Date(f.expire).toLocaleTimeString("zh-CN", { hour12: false })].filter(Boolean).join(" · ");
    row.append(el("b", "", MEM_TYPE[m.type] || m.type), el("span", "", f.content || f.reason || m.text), el("span", "meta", meta));
    if (f.id) { row.style.cursor = "pointer"; row.title = "查看这条记忆的原文、背景与来源"; row.onclick = () => openMemory(f.id); }
    rows.push(row);
  }
  if (phase === "start" && !load?.fields.skipped && !count("recall")) rows.push(el("div", "meta", "没有相关记忆；system 中注明“没有取回相关记忆”。"));
  if (aux.length) rows.push(el("div", "meta", "分数说明：向量是余弦相似度、BM25 是原始分、RRF 只看名次、重排是模型给的0–100相对分；都不是正确概率。"));
  for (const c of aux) rows.push(auxCard(r, c));
  const all = el("button", "link", "查看全部记忆");
  all.onclick = () => openMemory();
  return step(`mem-${phase}-${turn}`, [ico("mem", "mem"), el("span", "", label)], [...rows, all]);
}
// 本轮启用的 skill 与 MCP：快照来自观测台的 start/continue 事件，实际结果来自 agent 启动时的终端行。
function capsSummary(r, turn) {
  const logs = r.caps.filter((l) => l.turn === turn);
  const loaded = logs.find((l) => l.kind === "skills_loaded");
  const tools = logs.filter((l) => l.kind === "mcp_register").length;
  const errors = logs.filter((l) => ["skill_error", "mcp_error", "mcp_skip"].includes(l.kind)).length;
  const hookRuns = logs.filter((l) => l.kind === "hook").length, command = logs.find((l) => l.kind === "command");
  const parts = [];
  if (r.skills.length) parts.push(`Skills ${loaded ? loaded.count : "?"}/${r.skills.length}`);
  if (r.mcp.length) parts.push(`MCP ${r.mcp.length} 个 · ${tools} 个工具`);
  if (errors) parts.push(`${errors} 条跳过`);
  if (hookRuns) parts.push(`hook ${hookRuns} 次`);
  return { logs, errors, command, parts };
}
// 同 Claude Code 只在启动时报一次能力、出问题时再提：续聊各轮和上一轮一样（没有斜杠命令）就不再显示。
function capsCard(r, turn) {
  const { logs, errors, command, parts } = capsSummary(r, turn);
  if (!logs.length) return null;
  if (turn > 1 && !command && parts.join() === capsSummary(r, turn - 1).parts.join()) return null;
  if (command) parts.push(command.text.replace(/^Command: /, "命令 "));
  const rows = [];
  if (r.skills.length) rows.push(el("div", "meta", "skill：" + r.skills.join("、") + " · 模型判断相关时调用 load_skill 读取全文"));
  for (const m of r.mcp) rows.push(el("div", "meta", `MCP ${m.name}：${m.command}`));
  rows.push(el("pre", "", logs.map((l) => l.text).join("\n")));
  const links = el("div", "answer-meta"), sk = el("button", "link", "Skills 中心"), mc = el("button", "link", "MCP 中心");
  sk.onclick = () => openSkills(); mc.onclick = () => openMCP();
  links.append(sk, mc);
  return step(`caps-${turn}`, [ico("cap" + (errors ? " bad" : ""), "caps"), el("span", "", parts.join(" · ") || "能力")], [...rows, links]);
}
// D26：hook 运行记录。结局 ok（放行或补充上下文）、block（拦截，原因交给模型或用户）、error（hook 自己出错，照常继续）。
const HOOK_OUTCOME = { ok: "通过", block: "拦截", error: "出错（照常继续）" };
// 同 Claude Code：hook 安静通过就不占一行，只有拦截、出错或者给了反馈（输出会交给模型）时才显示。
const hooksWorthShowing = (hooks) => hooks?.some((h) => h.outcome !== "ok" || h.output);
function hookCard(key, hooks) {
  const blocked = hooks.some((h) => h.outcome === "block");
  const head = [ico("cap" + (blocked ? " bad" : ""), "hook"), el("span", "hub-chip", "hook"), el("span", "", hooks.map((h) => `${h.event} ${HOOK_OUTCOME[h.outcome] || h.outcome}`).join(" · "))];
  const rows = hooks.flatMap((h) => [el("div", "meta", `${h.event} · ${HOOK_OUTCOME[h.outcome] || h.outcome} · ${h.command}`), ...(h.output ? [el("pre", h.outcome === "block" ? "err" : "", h.output)] : [])]);
  return step(key, head, rows);
}
// 记忆辅助调用：保留原始输入输出与 token 用量，显示为卡片中的一行，不当作 assistant 回答。
function auxCard(r, c) {
  const box = el("div", "aux-call"), raw = rawIO(c, r.id), m = (c.choice && c.choice.message) || {};
  const state = !c.end ? "进行中…" : !c.choice ? `失败 · HTTP ${c.status}` : `${kfmt(c.usage.total_tokens || 0)} tokens · ${sec(c.ms)}`;
  const head = el("div", "mem-line");
  head.append(el("b", "", AUX_LABEL[c.purpose] || c.purpose), el("span", "", `请求 #${c.n} · ${c.purpose} · ${state}` + (m.content ? " · " + clip(m.content, 60) : "")), raw.button);
  box.append(head, raw.panel);
  return box;
}
// 聊天页与轨迹页共用同一份回合归属，避免在两处重复推测用户边界。
function chatFlow(r) {
  const flow = [], shown = new Set();
  [...r.calls, ...r.aux].sort((x, y) => x.n - y.n).forEach((c) => {
    if (c.aux) { if (shown.has(c.turn)) flow.push({ kind: "aux", call: c }); return; } // 开场的辅助调用在记忆卡片里显示
    if (c.caseStart) flow.push({ kind: "case", text: c.caseLabel });
    if (c.userMessage) { flow.push({ kind: "user", text: textOf(c.userMessage), turn: c.turn }, { kind: "caps", turn: c.turn }, { kind: "memory", turn: c.turn, phase: "start" }); shown.add(c.turn); }
    for (const s of r.steers) if (s.call === c.i) flow.push({ kind: "steer", text: s.text, turn: c.turn });
    if (c.compact) flow.push({ kind: "compact", call: c });
    else flow.push({ kind: "call", call: c });
    const next = r.calls.find((x) => x.n > c.n);
    if (next?.turn !== c.turn) flow.push({ kind: "memory", turn: c.turn, phase: "end" });
  });
  return flow;
}
export function step(key, head, body) {
  const box = el("details", "step");
  box.dataset.key = key;
  box.open = ui.open.has(key);
  box.ontoggle = () => box.open ? ui.open.add(key) : ui.open.delete(key);
  const sum = el("summary");
  sum.append(...head);
  const inner = el("div", "body");
  inner.append(...body);
  box.append(sum, inner);
  return box;
}
function observeAt(id) {
  Object.assign(ui, { page: "observe", tab: "trace", follow: false, selected: id, expanded: true, inspectorTab: "output" });
  render(); loadRuns();
}
const span = (ms) => ms < 60e3 ? sec(ms) : `${Math.floor(ms / 60e3)} 分 ${Math.round(ms % 60e3 / 1e3)} 秒`;
// 连续的只读调用（读文件、搜索、列目录）并成一项；只有一次只读调用时照常显示。
function exploreRuns(r, flow) {
  const out = [];
  for (const f of flow) {
    if (f.kind === "call" && exploreCall(r, ui.run, f.call)) {
      if (out.at(-1)?.kind === "explore") out.at(-1).calls.push(f.call);
      else out.push({ kind: "explore", calls: [f.call] });
    } else out.push(f);
  }
  return out.map((f) => f.kind === "explore" && f.calls.reduce((s, c) => s + c.tools.length, 0) < 2 ? { kind: "call", call: f.calls[0] } : f);
}
// 同 Codex 的 Explored：一行写读了哪些文件、搜了几次，点开是每次请求原来的思考与工具行，思考收在组里不打断这一行。
function exploreGroup(r, calls) {
  const running = calls.some((c) => c.tools.some((t) => t.state === "running"));
  const icon = running ? el("span", "ico") : ico("", "explore");
  if (running) icon.append(el("span", "spin"));
  const n = calls.reduce((s, c) => s + c.tools.length, 0), inner = el("div");
  for (const c of calls) renderCall(inner, r, c, false);
  return step("x" + calls[0].n, [icon, el("span", "xname", running ? "探索中" : "已探索"), el("span", "xdesc", exploreLabel(calls)), el("span", "", `${n} 次调用`)], [inner]);
}
// 流式输出进行中的等待提示带上已等多久：思考时不发内容的供应商，前一两分钟页面上只有这一行。
const liveWait = (c) => `${c.live?.content ? "生成中" : "思考中"}… ${Math.max(0, Math.floor((Date.now() - c.start) / 1000))} s`;
setInterval(() => {
  const s = ui.live;
  if (s?.wait?.isConnected && !s.call.end) s.wait.textContent = liveWait(s.call);
}, 1000);
export function renderCall(outer, r, c, last) {
  const m = (c.choice && c.choice.message) || {};
  // 每次请求不再单独占一行编号（同 Codex、Claude Code 的主视图）：原始输入 / 输出的开关悬停时出现在右上角，
  // Turn、Step 与请求号写在它的提示里；按 Step 展开的完整记录在轨迹页。
  const raw = rawIO(c, r.id), box = el("div", "req");
  raw.button.title = `${callPosition(c)} · 请求 #${c.n}`;
  box.append(raw.button, raw.panel); outer.append(box);
  if (!c.end) {
    // B1：流式输出进行中：推理与正文边收边显示，完整响应到达后换成下面的正式渲染。
    const live = c.live || {}, slot = { box, call: c, reasoning: live.reasoning?.length || 0, content: live.content || "" };
    if (live.reasoning) {
      slot.count = el("span", "", `思考中 · ${slot.reasoning} 字`); slot.text = el("div", "reason", live.reasoning);
      const fold = step("r" + c.n, [ico("", "think"), slot.count], [slot.text]);
      fold.addEventListener("toggle", () => { if (fold.open) slot.text.scrollTop = slot.text.scrollHeight; }); // 展开时从最新的思考看起，之后跟随
      box.append(fold);
    }
    if (live.content) box.append(slot.md = md(live.content, true));
    slot.wait = el("div", "shimmer", liveWait(c));
    slot.wait.title = "从发出请求算起。有的供应商（如 Coconut）思考时什么都不发，回答写完才一次给出思考内容";
    if (r === ui.run) ui.live = slot;
    return box.append(slot.wait);
  }
  if (!c.choice) {
    if (c.status === 499) return box.append(step("e" + c.n, [ico("", "stop"), el("span", "", `请求已取消（agent 停止）· ${sec(c.ms)}`)], [el("pre", "", c.errorText || "")]));
    return box.append(step("e" + c.n, [ico("bad", "error"), el("span", "bad", `模型请求失败 · HTTP ${c.status}`)], [el("pre", "err", c.errorText || pretty(c.res))]));
  }
  if (m.reasoning_content) {
    const p = tokenParts(c.usage);
    // 只知道整次请求的耗时：思考与写回答各占多少，各家流式的顺序不同，算不出来。
    const label = el("span", "", "思考");
    label.title = (p.reasoning ? `${kfmt(p.reasoning)} tokens · ` : "") + `整次请求 ${sec(c.ms)}`;
    box.append(step("r" + c.n, [ico("", "think"), label], [el("div", "reason", m.reasoning_content)]));
  }
  // 过程说明：和工具调用一起出现的文字，或 agent 标明是 commentary 的（如被 Stop hook 拦下的“想结束”）。
  const commentary = m.content && (c.tools.length || c.phase === "commentary");
  if (commentary) { const mid = md(m.content, true); mid.classList.add("mid"); if (!c.tools.length) mid.title = "过程说明：还没有结束（比如被 Stop hook 拦下，接着做）"; box.append(mid); }
  for (const t of c.tools) {
    if (hooksWorthShowing(r.hookCalls[t.id])) box.append(hookCard(`h${c.n}-${t.index}`, r.hookCalls[t.id]));
    if (ui.run.approvals[t.id]) box.append(approvalCard(ui.run, t.id));
    if (t.name === "spawn_agent" && !r.isSub) { box.append(agentCard(r, t)); continue; }
    if (WEB_TOOLS.includes(t.name)) { box.append(webCard(r, t)); continue; }
    if (t.name === "bash") { box.append(bashCard(r, c, t), ...proxyNetCards(t)); continue; }
    if (t.name === "request_network_access") { box.append(netCard(r, t)); continue; }
    if (t.name === "ask_user") { box.append(askCard(ui.run, t)); continue; }
    const icon = t.state === "running" ? el("span", "ico") : ico(t.detour ? "bad" : "ok", t.detour ? "bad" : "ok");
    if (t.state === "running") icon.append(el("span", "spin"));
    const head = [icon, el("span", "tname", t.name), el("span", "targs", clip(compact(t.args), 90))];
    if (t.name === "load_skill" || t.name.startsWith("mcp_")) head.splice(1, 0, el("span", "hub-chip", t.name === "load_skill" ? "skill" : "mcp"));
    if (t.done) head.push(el("span", "", sec(t.done - c.end)));
    const result = t.done ? toolResult(t) : el("div", "meta", t.state === "lost" ? "进程已结束，没有结果" : "执行中…");
    box.append(step(`t${c.n}-${t.index}`, head, [el("div", "label", "参数"), el("pre", "", pretty(t.args)), el("div", "label", "结果" + (t.reason ? " · " + t.reason : "")), result]));
  }
  if (!commentary && m.content) {
    const answer = md(m.content, true);
    // 只把本题实际取回且过阈值的编号变成可点击引用；其他 [Dxx] 标红，提示它不是有效来源。
    const caseStart = r.calls.filter((x) => x.caseStart && x.n <= c.n).at(-1)?.n || 1;
    const sources = new Map();
    for (const earlier of r.calls.filter((x) => x.n >= caseStart && x.n <= c.n)) for (const t of earlier.tools) {
      const result = parse(t.result)?.result;
      if (t.name === "search_docs") for (const h of result?.hits || []) if (h.accepted) sources.set(h.id, t);
      if (t.name === "search_memory") for (const m of result?.memories || []) sources.set(m.id, `t${t.call.n}-${t.index}`);
    }
    // [Mxx] 指向本对话开头调入的记忆卡片或 search_memory 结果；不是本对话取回的编号标红。
    for (const m of r.mem) if (m.type === "recall" && m.fields.id && !sources.has(m.fields.id)) sources.set(m.fields.id, `mem-start-${m.turn}`);
    const walker = document.createTreeWalker(answer, NodeFilter.SHOW_TEXT), nodes = [];
    while (walker.nextNode()) nodes.push(walker.currentNode);
    for (const node of nodes) {
      if (node.parentElement.closest("code,pre")) continue;
      const parts = node.textContent.split(/(\[[DM]\d+\])/g);
      if (parts.length < 2) continue;
      const frag = document.createDocumentFragment();
      for (const part of parts) {
        const t = sources.get(part.slice(1, -1));
        if (!t && /^\[[DM]\d+\]$/.test(part)) {
          const bad = el("span", "cite bad", part); bad.title = part[1] === "M" ? "本对话没有取回这条记忆：不能作为依据" : "本题没有取回这条，或它低于阈值：不能作为证据";
          frag.append(bad); continue;
        }
        if (!t) { frag.append(part); continue; }
        if (typeof t === "string") {
          const link = el("button", "cite", part); link.title = "查看本对话取回的这条记忆";
          link.onclick = () => { ui.open.add(t); renderChat(); document.querySelector(`[data-key="${t}"]`)?.scrollIntoView({ block: "center", behavior: "smooth" }); };
          frag.append(link); continue;
        }
        const link = el("button", "cite", part); link.title = "查看本题检索到的原始片段";
        link.onclick = () => observeAt(`t${t.call.n}-${t.index}`); frag.append(link);
      }
      node.replaceWith(frag);
    }
    box.append(answer);
    // 回答下面只写这一轮的用时（同 Claude Code 的 “Worked for …”）；模型、请求次数与 token 合计放在悬停提示里。
    // 同 claude.ai 的消息操作：最后一轮常显，之前各轮悬停时才出现（见 app.css）。
    const turn = r.calls.filter((x) => x.turn === c.turn), used = turn.reduce((s, x) => s + (x.usage?.total_tokens || 0), 0);
    const meta = el("div", "answer-meta turn-sum"), took = el("span", "", "用时 " + span(c.end - turn[0].start));
    took.title = `${c.res?.model || c.req.model || ""} · 本轮 ${turn.length} 次请求 · 累计 ${used >= 1e6 ? (used / 1e6).toFixed(1) + "M" : kfmt(used)} tokens`;
    meta.append(took);
    if (!r.isSub) { // 子 agent 的请求不在父对话的观测视图里
      const link = el("button", "link", "观测");
      link.onclick = () => observeAt("m" + c.n);
      meta.append(link);
    }
    box.append(meta);
  }
  if (hooksWorthShowing(r.hookAfter[c.n])) box.append(hookCard(`hs${c.n}`, r.hookAfter[c.n]));
  if (last && !r.exit && c.tools.length && c.tools.every((t) => t.done)) box.append(el("div", "shimmer", "思考中…"));
}
export function renderCompact(c) {
  const box = el("details", "divider"), key = "s" + c.n, m = (c.choice && c.choice.message) || {};
  box.open = ui.open.has(key);
  box.ontoggle = () => box.open ? ui.open.add(key) : ui.open.delete(key);
  const label = !c.end ? "正在压缩上下文…" : !c.choice || c.invalidSummary ? "上下文压缩失败" : `上下文已压缩 · 摘要 ${(m.content || "").length} 字 · ${sec(c.ms)}`;
  box.append(el("summary", "", label));
  const body = el("div", "body");
  body.append(c.choice && m.content ? md(m.content) : el("pre", "err", c.errorText || pretty(c.res || "")));
  if (c.end) box.append(body);
  else if (c.live?.content) box.append(md(c.live.content)); // B1：摘要边生成边显示
  return box;
}
function welcome() {
  const box = el("div", "welcome"), sugs = el("div", "sugs");
  box.append(el("h1", "", "想让 agent 做什么？"), sugs);
  const suggestions = toolOn("memory") ? ["记住：我偏好 Go 而不是 Python", "你还记得我偏好什么语言吗？", "记住流程：代码评审先跑构建，再看逻辑"] : toolOn("rag") ? ["本项目工具失败后总共尝试几次，等待多久？", "我们项目 TS-999 错误码是什么？请给出资料依据。"] : [
    toolOn("calculator") && toolOn("get_current_datetime") && "计算 (1234×5678) 开根号取整，并查询当前日期时间。两项独立，请同轮调用。",
    toolOn("search_notes") && toolOn("calculator") && "先查学习笔记中的循环职责，再计算职责数量乘以25。",
    toolOn("ds_search") && "搜索一下有什么方式可以学习 agent harness，注明来源。",
  ].filter(Boolean);
  for (const q of suggestions) {
    const b = el("button", "", q);
    b.onclick = () => { $("query").value = q; syncComposer(); $("query").focus(); };
    sugs.append(b);
  }
  return box;
}
export function renderChat() {
  parkViz();
  drawChat();
  claimViz();
}
function drawChat() {
  const box = $("thread"), scroller = $("chat-scroll");
  const top = scroller.scrollTop, height = scroller.scrollHeight;
  ui.live = null;
  // 展开的思考框自带滚动条：重建前记下位置，原来在底部的继续跟随流式输出。
  const reasons = new Map([...box.querySelectorAll("details.step[open] > .body > .reason")].map((x) =>
    [x.closest("details").dataset.key, x.scrollHeight - x.scrollTop - x.clientHeight < 24 ? Infinity : x.scrollTop]));
  box.replaceChildren();
  const r = ui.run;
  if (!r) { renderSubPanel(); return box.append(welcome()); }
  analyze(r);
  if (r.query === LAB_TITLE) box.append(el("div", "banner", "Day 3 上下文实验：程序自动发出33轮真实对话，再触发一次大工具输出。下面的用户消息由实验程序发送。"));
  if (r.query === RAG_LAB_TITLE) box.append(el("div", "banner", "Day 4：10个问题，每题分别运行无检索与有检索。每组使用独立上下文；问题由实验程序发送。检索评分见终端，引用可点击核对。"));
  let group = null;
  const flow = exploreRuns(r, chatFlow(r));
  flow.forEach((f, i) => {
    if (f.kind === "case") { box.append(el("div", "banner", f.text)); group = null; return; }
    if (f.kind === "user" && r.interrupted?.[f.turn]) box.append(el("div", "banner", r.interrupted[f.turn]));
    if (f.kind === "user" && r.rewindNotes[f.turn]) box.append(el("div", "banner", r.rewindNotes[f.turn]));
    const turn = r.turns[(f.turn ?? f.call?.turn) - 1];
    if (f.kind === "user") {
      const u = el("div", turn?.rewound ? "u rewound" : "u"), bubble = el("div", "", f.text);
      if (turn?.images?.length) bubble.append(userPictures(turn.images));
      u.append(bubble); box.append(u); group = null;
      const tools = el("div", "utools");
      // 问题下面的信息与操作悬停时才出现（同 claude.ai 用户消息下的操作，见 app.css）。
      // D30：续聊的代价：闲置时长与第一次请求的缓存命中，未命中的 token 数在悬停提示里。
      const first = turn?.firstUsage;
      if (first && first.prompt_tokens && turn.idle) {
        const cached = (first.prompt_tokens_details && first.prompt_tokens_details.cached_tokens) || 0, hit = Math.round(100 * cached / first.prompt_tokens);
        const info = el("span", "meta", `闲置 ${idleText(turn.idle)} · 缓存命中 ${hit}%`);
        info.title = `这一轮第一次请求的输入 ${hit}% 命中提示词缓存，${kfmt(first.prompt_tokens - cached)} token 没命中，按全价重新处理`;
        tools.append(info);
      }
      // D29：有工作区的对话，每一轮都可以撤销（连同之后各轮）；正在运行、回放中、导入的对话不行。
      if (turn?.checkpoint && !turn.rewound && r.workspace && r.exit && !r.source && !ui.replay && !ui.loading) {
        const b = el("button", "link", "撤销");
        b.title = "撤销到这一轮之前：把这一轮及之后用 edit_file / write_file 改过的文件放回去，对话退回上一轮";
        b.onclick = () => rewindTurn(r, turn);
        tools.append(b);
      }
      if (tools.childNodes.length) box.append(tools);
      return;
    }
    if (f.kind === "steer") {
      const u = el("div", "u steer"), bubble = el("div", "", f.text);
      u.title = "运行中插话：排进了这一次模型请求";
      u.append(bubble); box.append(u); group = null;
      return;
    }
    const fresh = () => { group = el("div", turn?.rewound ? "a rewound" : "a"); box.append(group); return group; };
    if (f.kind === "compact") return box.append(renderCompact(f.call));
    if (f.kind === "aux") { if (!group) fresh(); group.append(auxCard(r, f.call)); return; }
    if (f.kind === "caps") { const card = capsCard(r, f.turn); if (card) { if (!group) fresh(); group.append(card); } return; }
    if (f.kind === "memory") { const card = memoryCard(r, f.turn, f.phase); if (card) { if (!group) fresh(); group.append(card); } return; }
    if (!group) fresh();
    if (f.kind === "explore") {
      group.append(exploreGroup(r, f.calls));
      if (i === flow.length - 1 && !r.exit && f.calls.at(-1).tools.every((t) => t.done)) group.append(el("div", "shimmer", "思考中…"));
      return;
    }
    renderCall(group, r, f.call, i === flow.length - 1);
  });
  if (r.rewindNotes[r.turns.length + 1]) box.append(el("div", "banner", r.rewindNotes[r.turns.length + 1]));
  if (r.pending) {
    const u = el("div", "u"); u.append(el("div", "", r.pending)); box.append(u);
    const a = el("div", "a");
    for (const card of [capsCard(r, r.memTurn), memoryCard(r, r.turns.length + 1, "start") || memoryCard(r, r.memTurn, "start")]) if (card) a.append(card);
    if (a.childNodes.length) box.append(a);
    box.append(el("div", "shimmer", "正在启动 agent…"));
  } else if (!r.exit && !r.calls.length) box.append(el("div", "shimmer", "正在启动 agent…"));
  const last = r.calls.filter((c) => !c.compact).at(-1);
  const answered = last && last.choice && !last.tools.length && last.choice.message.content;
  if (r.exit && (r.exit !== "exit 0" || !answered)) {
    const card = el("div", "stopcard");
    card.append(el("div", "err", r.source && r.exit === "exit 0" ? "记录在这里结束：最后一次请求之后没有回答" : (r.stopRequested ? "已按停止按钮中断：" : "agent 已停止：") + endLabel(r)));
    // O12：不正常结束时 harness 自己跑的收尾检查（Stop hook 的命令），告诉人留下的状态能不能用。
    if (r.finalCheck) card.append(r.finalCheck.passed ? el("div", "meta", "收尾检查通过（Stop hook）") : fold("收尾检查没通过（Stop hook）", r.finalCheck.detail || "", true));
    if (r.checkpoint) card.append(el("div", "meta", "检查点已保存，可以直接继续提问"));
    const tail = r.logs.filter((l) => !(l.text || "").startsWith("用户续聊：")).slice(-6);
    if (tail.length) card.append(fold("最后几行输出", tail.map((l) => l.text).join("\n")));
    const link = el("button", "link", "完整终端输出");
    link.onclick = () => { observeAt("end"); };
    card.append(link);
    box.append(card);
  }
  // D34：计划模式下的一轮正常结束：一键批准，切到 edit 并让 agent 按计划执行（D27 的“下一轮切到 edit”）。
  if (r.exit === "exit 0" && answered && r.permission === "plan" && !r.source && !ui.replay) {
    const bar = el("div", "utools"), b = el("button", "", "批准计划并切到 edit");
    b.type = "button";
    b.title = "下一轮用 edit 权限，告诉 agent 计划已批准、按计划开始执行";
    b.onclick = () => { ui.chatFollow = true; start(`/runs/${encodeURIComponent(r.id)}/messages`, { query: "计划已批准，切到 edit 模式，按计划开始执行。", permission: "edit" }); };
    bar.append(b, el("span", "meta", "或者在输入框里提修改意见，继续在 plan 模式下讨论"));
    box.append(bar);
  }
  for (const [key, pos] of reasons) {
    const x = box.querySelector(`details.step[open][data-key="${key}"] > .body > .reason`);
    if (x) x.scrollTop = pos === Infinity ? x.scrollHeight : pos;
  }
  if (ui.chatFollow) toBottom(); else { scroller.scrollTop = top; if (scroller.scrollHeight > height) ui.unseen = true; }
  syncJump();
  renderSubPanel();
}
// 跟随最新输出由用户的滚动决定：往上滚（滚轮、拖滚动条、键盘、触摸）就停止跟随，回到底部再恢复；
// 不再按“离底部多近”猜，否则流式输出时滚一格就会被下一帧拉回底部。
function toBottom() { const s = $("chat-scroll"); s.scrollTop = s.scrollHeight; ui.chatSetTop = s.scrollTop; }
function unfollow() { if (ui.chatFollow) { ui.chatFollow = false; syncJump(); } }
function syncJump() {
  const s = $("chat-scroll"), b = $("jump"), far = s.scrollHeight - s.scrollTop - s.clientHeight > 80;
  if (ui.chatFollow || !far) ui.unseen = false;
  b.hidden = ui.page !== "chat" || !ui.run || ui.chatFollow || !far;
  b.classList.toggle("fresh", !!ui.unseen);
  b.textContent = ui.unseen ? "有新内容 · 回到最新" : "回到最新";
}
$("chat-scroll").addEventListener("wheel", (ev) => { if (ev.deltaY < 0) unfollow(); }, { passive: true });
$("chat-scroll").addEventListener("scroll", () => {
  const s = $("chat-scroll");
  if (s.scrollHeight - s.scrollTop - s.clientHeight < 8) ui.chatFollow = true;
  else if (s.scrollTop < (ui.chatSetTop ?? 0) - 2) ui.chatFollow = false;
  ui.chatSetTop = s.scrollTop;
  syncJump();
});
$("jump").onclick = () => {
  const s = $("chat-scroll");
  ui.chatFollow = true; ui.unseen = false; syncJump();
  s.scrollTo({ top: s.scrollHeight, behavior: "smooth" });
};
// B1 的增量只改最后一次请求的思考与正文：就地追加，不重建整个对话，页面其余部分的点击、选中文字和滚动都不受打扰。
// 结构有变化（第一次出现思考或正文、含可视化块）时返回 false，交给整体重建。
export function patchLive(r) {
  const s = ui.live, c = r.calls.at(-1), live = c?.live || {};
  if (ui.page !== "chat" || ui.run !== r || !s || s.call !== c || c.end || !s.box.isConnected) return false;
  if (!s.text !== !live.reasoning || !s.md !== !live.content || /```(echarts|html)/.test(live.content || "")) return false;
  const scroller = $("chat-scroll"), height = scroller.scrollHeight;
  if (live.reasoning && live.reasoning.length > s.reasoning) {
    const x = s.text, bottom = x.scrollHeight - x.scrollTop - x.clientHeight < 24;
    x.append(live.reasoning.slice(s.reasoning)); // 追加文本节点：已选中的思考文字不会丢
    s.reasoning = live.reasoning.length;
    s.count.textContent = `思考中 · ${s.reasoning} 字`;
    if (bottom) x.scrollTop = x.scrollHeight;
  }
  if (live.content && live.content !== s.content) { s.md.innerHTML = md(live.content).innerHTML; s.content = live.content; }
  if (ui.chatFollow) toBottom(); else if (scroller.scrollHeight > height) ui.unseen = true;
  syncJump();
  return true;
}

