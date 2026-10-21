import { analyze, items, tokenParts, toolResult, toolSummary } from "./analysis.js";
import { fetchEvents, stopReplay } from "./follow.js";
import { render, schedule } from "./render.js";
import { apply, newRun } from "./runs.js";
import { loadRuns } from "./shell.js";
import { $, AUX_LABEL, callPosition, clip, compact, el, esc, fold, ico, kfmt, legend, message, pretty, rawIO, save, sec, section, ui } from "./util.js";
// ---------- 视图一：Trajectory 事件账本 + 瀑布时间轴 ----------
export function renderTrace(r, a, list) {
  const view = $("view");
  view.replaceChildren();
  const total = Math.max(1, (r.tEnd || r.last) - r.t0);
  const [from, to] = ui.zoom || [0, total];
  const span = Math.max(1, to - from);
  const head = el("div", "lrow headrow");
  const rulerWrap = el("div", "rulerwrap");
  const ruler = el("div", "ruler");
  ruler.title = "在刻度上拖拽聚焦一段时间，双击还原";
  for (let i = 0; i <= 4; i++) {
    const s = el("span", "", sec(from + (span * i) / 4));
    s.style.left = i * 25 + "%";
    s.style.transform = i === 0 ? "none" : i === 4 ? "translateX(-100%)" : "translateX(-50%)";
    ruler.append(s);
  }
  bindRuler(ruler, from, span);
  rulerWrap.append(ruler);
  head.append(el("span", "num", "#"), el("span", "", "类型"), el("span", "", "内容摘要"), el("span", "tok", "tokens"), el("span", "dur", "耗时"), rulerWrap);
  view.append(head);
  const query = ui.search.trim().toLowerCase();
  let turn = 0, group = null, seq = 0;
  for (const it of list) {
    if (it.hidden) continue;
    seq++;
    if (query && !(it.label + " " + it.sum + (it.message ? " " + compact(it.message) : "")).toLowerCase().includes(query)) continue;
    if (it.turn !== turn) {
      turn = it.turn; group = null;
      const calls = [...r.calls, ...r.aux].filter((c) => c.turn === turn), info = r.turns[turn - 1];
      const open = query || ui.expanded || !ui.collapsed.has(turn);
      const t = el("div", "turn");
      t.append(ico("tchev" + (open ? " open" : ""), "chev"), el("span", "", `Turn ${turn}`));
      if (info?.caseLabel) t.append(el("span", "meta", info.caseLabel));
      t.append(el("span", "meta", `${info?.steps || 0} 个 Step · ${kfmt(calls.reduce((s, c) => s + (c.usage.total_tokens || 0), 0))} tokens · 模型 ${sec(calls.reduce((s, c) => s + (c.ms || 0), 0))}`));
      const n = turn;
      t.onclick = () => { ui.expanded = false; ui.collapsed.has(n) ? ui.collapsed.delete(n) : ui.collapsed.add(n); render(); };
      view.append(t);
    }
    if (!query && !ui.expanded && ui.collapsed.has(it.turn)) continue;
    const c = it.call;
    const key = it.type === "aux" ? "aux" + c.n : it.step ? "step" + c.n : it.type === "compacted" ? "compact" + c.n : null;
    if (key && key !== group) {
      group = key;
      const h = el("div", "step-header");
      h.append(el("strong", "", c.aux ? "辅助调用 · " + AUX_LABEL[c.purpose] : c.compact ? "上下文压缩" : `Step ${c.step}`), el("span", "", `请求 #${c.n}${c.ms ? " · " + sec(c.ms) : " · 进行中"}`));
      const input = el("button", "input-link", `${(c.req.messages || []).length} 条 messages`);
      input.setAttribute("aria-label", `${callPosition(c)} · 请求 #${c.n} 查看模型输入`);
      input.onclick = () => select("in" + c.n);
      const raw = rawIO(c, r.id);
      h.append(input, raw.button); view.append(h, raw.panel);
    }
    const row = el("div", "lrow" + (it.id === ui.selected ? " selected" : ""));
    row.dataset.event = it.id;
    row.append(el("span", "num", seq), el("span", "badge " + it.type + (it.bad ? " bad" : ""), it.type === "tool" ? "TOOL" : it.type === "process" ? "EXIT" : it.label),
      el("span", "sum" + (it.bad ? " err" : ""), (it.type === "tool" ? it.label + "  " : "") + clip(it.sum, 200)),
      el("span", "tok", it.tokens ? kfmt(it.tokens) : ""));
    const end = it.stop || (it.running ? r.last : null);
    row.append(el("span", "dur", it.start && end ? sec(end - it.start) : ""));
    const track = el("div", "track");
    if (it.start && end) {
      const left = (it.start - r.t0 - from) / span, width = (end - it.start) / span;
      if (left <= 1 && left + width >= 0) {
        const bar = el("div", `bar ${it.type === "tool" ? "tool" : "model"}${it.bad ? " bad" : ""}${it.running ? " running" : ""}`);
        bar.style.left = Math.max(0, left) * 100 + "%";
        bar.style.width = (Math.min(1, left + width) - Math.max(0, left)) * 100 + "%";
        bar.title = `开始 +${sec(it.start - r.t0)} · 持续 ${sec(end - it.start)}` + (["message", "compacted", "aux"].includes(it.type) ? "（非流式响应，无法拆分 TTFT / decode）" : "");
        track.append(bar);
      }
    }
    row.append(track);
    row.onclick = () => select(it.id);
    view.append(row);
  }
}
function bindRuler(ruler, from, span) {
  let startX = null, sel = null;
  const frac = (ev) => { const b = ruler.getBoundingClientRect(); return Math.min(1, Math.max(0, (ev.clientX - b.left) / b.width)); };
  ruler.onpointerdown = (ev) => { startX = frac(ev); sel = el("div", "sel"); ruler.append(sel); ruler.setPointerCapture(ev.pointerId); };
  ruler.onpointermove = (ev) => {
    if (startX === null) return;
    const x = frac(ev);
    sel.style.left = Math.min(x, startX) * 100 + "%";
    sel.style.width = Math.abs(x - startX) * 100 + "%";
  };
  ruler.onpointerup = (ev) => {
    if (startX === null) return;
    const x = frac(ev), lo = Math.min(x, startX), hi = Math.max(x, startX);
    startX = null;
    if (hi - lo > 0.02) ui.zoom = [from + lo * span, from + hi * span];
    render();
  };
  ruler.ondblclick = () => { ui.zoom = null; render(); };
}

// ---------- 视图：D13 调用链（OpenTelemetry span）----------
// 数据来自 /traces/<trace_id>：各进程的 SDK 在 span 结束时写入的记录。运行中每隔一会儿重新读取。
const spanCache = {}, metricCache = {};
function fetchCached(cache, key, url, live) {
  const hit = cache[key];
  if (hit && (hit.loading || !live || Date.now() - hit.at < 1500)) return hit.data;
  cache[key] = { data: hit?.data, at: Date.now(), loading: true };
  fetch(url).then((res) => res.ok ? res.json() : null).then((data) => { cache[key] = { data, at: Date.now() }; schedule(); })
    .catch(() => { cache[key] = { data: hit?.data, at: Date.now() }; });
  return hit?.data;
}
const spanMs = (s) => Date.parse(s.end) - Date.parse(s.start);
function spanTree(spans) {
  const ids = new Set(spans.map((s) => s.span_id)), kids = {};
  for (const s of spans) (kids[ids.has(s.parent_id) ? s.parent_id : ""] ??= []).push(s);
  for (const list of Object.values(kids)) list.sort((a, b) => Date.parse(a.start) - Date.parse(b.start));
  const rows = [];
  const walk = (s, depth) => { rows.push({ s, depth }); for (const c of kids[s.span_id] || []) walk(c, depth + 1); };
  for (const s of kids[""] || []) walk(s, 0);
  return rows;
}
export function renderSpans(r) {
  const view = $("view");
  view.replaceChildren();
  const box = el("div", "spans");
  view.append(box);
  const live = !r.exit;
  // 运行中工具可能长时间没有新事件：定时重读，新结束的 span 才会出现。
  if (live && !ui.spanTimer) ui.spanTimer = setTimeout(() => { ui.spanTimer = null; if (ui.tab === "spans" && ui.page === "observe") schedule(); }, 1600);
  const metrics = fetchCached(metricCache, (ui.metricsAll ? "*" : r.id) + (live ? "" : ":done"), "/trace-metrics" + (ui.metricsAll ? "" : "?conversation=" + encodeURIComponent(r.id)), live);
  box.append(renderSpanMetrics(metrics));
  if (!r.traces.length) {
    box.append(el("div", "empty", "这条对话产生于 D13 之前，没有调用链记录；新提问或续聊后会出现。"));
    return renderSpanInspector(r, null);
  }
  let selected = null;
  for (const t of r.traces) {
    // 结束后换一个缓存键再读一次：根 span 在进程退出时才写入。读到之前先显示运行中最后一次的结果。
    const spans = fetchCached(spanCache, t.id + (live ? "" : ":done"), "/traces/" + t.id, live) || spanCache[t.id]?.data || [];
    const head = el("div", "span-trace");
    const start = Math.min(...spans.map((s) => Date.parse(s.start))), end = Math.max(...spans.map((s) => Date.parse(s.end)));
    head.append(el("strong", "", `Turn ${t.turn}`), el("span", "meta", `trace ${t.id}`), el("span", "meta", spans.length ? `${spans.length} 个 span · ${sec(end - start)}` : "等待 span 写入…"));
    const copy = el("button", "ghost", "复制 trace_id"); copy.type = "button";
    copy.onclick = async () => { try { await navigator.clipboard.writeText(t.id); copy.textContent = "已复制 ✓"; } catch { copy.textContent = "请手动复制"; } };
    head.append(copy);
    box.append(head);
    const rows = spanTree(spans);
    if (rows.some(({ s, depth }) => depth === 0 && s.parent_id)) box.append(el("div", "span-note", "⚠ 标黄的 span 找不到父节点：父 span 还没结束（运行中），或所在进程被强制结束、没来得及写出。"));
    for (const { s, depth } of rows) {
      const row = el("div", "span-row" + (ui.span === s.span_id ? " selected" : ""));
      const name = el("span", "span-name" + (s.status === "error" ? " err" : ""));
      name.style.paddingLeft = depth * 14 + "px";
      name.append((depth ? "└ " : "") + s.name);
      if (depth === 0 && s.parent_id) name.append(el("span", "orphan", " · 父 span 缺失"));
      const attrs = s.attributes || {};
      name.title = s.name + (attrs["agent.step"] ? ` · Step ${attrs["agent.step"]}` : "");
      const track = el("div", "span-track"), bar = el("div", "span-bar" + (s.status === "error" ? " err" : attrs["gen_ai.operation.name"] === "execute_tool" ? " tool" : s.kind === "client" ? " client" : ""));
      const total = Math.max(1, end - start);
      bar.style.left = ((Date.parse(s.start) - start) / total) * 100 + "%";
      bar.style.width = (spanMs(s) / total) * 100 + "%";
      track.append(bar);
      row.append(name, el("span", "span-svc", s.service), el("span", "span-dur", sec(spanMs(s))), track);
      row.onclick = () => { ui.span = s.span_id; render(); };
      box.append(row);
      if (ui.span === s.span_id) selected = s;
    }
  }
  renderSpanInspector(r, selected);
}
function renderSpanMetrics(m) {
  const wrap = el("div", "span-metrics");
  const table = (title, list, cols) => {
    const card = el("div", "section"), head = el("div", "label");
    head.append(el("span", "", title));
    const t = el("table"), tr = el("tr");
    for (const [label] of cols) tr.append(el("th", "", label));
    t.append(tr);
    for (const item of list || []) {
      const row = el("tr");
      for (const [, get] of cols) row.append(el("td", "", get(item)));
      t.append(row);
    }
    card.append(head, (list || []).length ? t : el("div", "meta", "暂无数据"));
    return card;
  };
  const toggle = el("button", "ghost", ui.metricsAll ? "只看本对话" : "汇总全部对话"); toggle.type = "button";
  toggle.onclick = () => { ui.metricsAll = !ui.metricsAll; render(); };
  const caption = el("div", "label");
  caption.append(el("span", "", `指标（由 ${m?.traces ?? "…"} 个 trace 的 span 汇总）`), toggle);
  const rate = (i) => i.count ? Math.round((i.errors / i.count) * 100) + "%" : "";
  wrap.append(
    table("gen_ai.client.operation.duration · token.usage", m?.["gen_ai.client.operation.duration"], [["操作 · 用途", (i) => i.name], ["次数", (i) => i.count], ["失败", rate], ["P50", (i) => sec(i.p50_ms)], ["P95", (i) => sec(i.p95_ms)], ["输入/输出 tokens", (i) => i.input_tokens || i.output_tokens ? `${kfmt(i.input_tokens || 0)} / ${kfmt(i.output_tokens || 0)}` : ""]]),
    table("execute_tool 耗时与失败率", m?.["execute_tool.duration"], [["工具", (i) => i.name], ["次数", (i) => i.count], ["失败", rate], ["P50", (i) => sec(i.p50_ms)], ["P95", (i) => sec(i.p95_ms)]]),
    table("任务结局（invoke_agent）", m?.["agent.outcome"], [["结局", (i) => i.name], ["次数", (i) => i.count], ["P50", (i) => sec(i.p50_ms)], ["P95", (i) => sec(i.p95_ms)]]),
  );
  const outer = el("div");
  outer.append(caption, wrap);
  caption.style.padding = "8px 12px 0";
  return outer;
}
function renderSpanInspector(r, s) {
  const box = $("inspector");
  box.replaceChildren();
  if (!s) return box.append(el("div", "empty", "点击 span 查看属性与事件。内容（提示词、工具参数与结果）默认不采集，见 README 的 D13 配置。"));
  const top = el("div", "top");
  top.append(el("strong", "", s.name), el("div", "meta", `${s.kind} · ${s.service} · ${sec(spanMs(s))}${s.status === "error" ? " · 失败：" + s.error : ""}`));
  box.append(top);
  const kv = el("div", "kv"), add = (k, v) => kv.append(el("span", "", k), el("span", "", v));
  add("trace_id", s.trace_id); add("span_id", s.span_id); add("parent_id", s.parent_id || "（根）");
  add("开始", new Date(s.start).toLocaleTimeString()); add("结束", new Date(s.end).toLocaleTimeString());
  box.append(section("标识", kv));
  const link = r.spanCalls[s.span_id];
  if (link && !link.sub) {
    const go = el("button", "ghost", `在轨迹中查看原始请求 #${link.call}`); go.type = "button";
    go.onclick = () => { ui.tab = "trace"; ui.follow = false; select("in" + link.call); };
    box.append(section("观测代理记录", go));
  }
  box.append(section("属性 attributes", el("pre", "", pretty(s.attributes || {}))));
  if (s.events?.length) box.append(section(`事件 events（${s.events.length}）`, ...s.events.map((e) => fold(`${e.name} · ${new Date(e.time).toLocaleTimeString()}`, e.attributes || {}, true))));
  box.append(fold("原始记录 JSON", s));
}

// ---------- 视图二：dsh-maze 式探索迷宫 + 数据轨 ----------
const COL = 170, LEFT = 70, NODE_H = 26, GAP = 34;
const colX = (k) => LEFT + (k - 1) * COL;
export function renderMaze(r, a, list) {
  const view = $("view");
  view.replaceChildren();
  if (!r.calls.length) return view.append(el("div", "empty", "等待第一次模型调用…"));
  const n = r.calls.length, last = r.calls[n - 1];
  const tailX = colX(n) + (last.tools.length ? COL : COL * 0.55);
  const width = tailX + 120;
  // 排版：主路径工具围绕主干上下展开，绕路工具放在下方
  let above = 1, below = 1;
  for (const c of r.calls) {
    const main = c.tools.filter((t) => !t.detour).length, det = c.tools.length - main;
    above = Math.max(above, Math.ceil(main / 2));
    below = Math.max(below, Math.floor(main / 2) + det);
  }
  const spineY = 44 + above * GAP, height = spineY + below * GAP + 60;
  const pos = new Map();
  for (const c of r.calls) {
    const main = c.tools.filter((t) => !t.detour), det = c.tools.filter((t) => t.detour);
    const x = colX(c.i) + COL / 2;
    main.forEach((t, i) => pos.set(t, [x, spineY + (i - (main.length - 1) / 2) * GAP]));
    const base = spineY + (main.length ? ((main.length - 1) / 2) * GAP : 0) + GAP + 8;
    det.forEach((t, j) => pos.set(t, [x, base + j * GAP]));
  }
  const parts = [];
  for (const c of r.calls) parts.push(`<text class="muted" x="${colX(c.i) + COL / 2}" y="16" text-anchor="middle">T${c.turn} · ${c.compact ? "压缩" : "S" + c.step}</text>`);
  parts.push(`<line class="spine" x1="${colX(1)}" y1="${spineY}" x2="${tailX}" y2="${spineY}" opacity=".15"/>`);
  for (const c of r.calls) {
    const x0 = colX(c.i), x1 = c.i < n ? colX(c.i + 1) : tailX;
    // 没有有效工具的一轮，主干直接连到下一次模型调用
    if (c.tools.every((t) => t.detour)) parts.push(`<line class="spine" x1="${x0}" y1="${spineY}" x2="${x1}" y2="${spineY}"/>`);
    for (const t of c.tools) {
      const [x, y] = pos.get(t);
      if (!t.detour) parts.push(`<path class="edge main" d="M${x0},${spineY} C${x0 + 40},${spineY} ${x - 60},${y} ${x - 55},${y} M${x + 55},${y} C${x + 60},${y} ${x1 - 40},${spineY} ${x1},${spineY}"/>`);
      else parts.push(`<path class="edge detour" d="M${x0},${spineY} C${x0 + 30},${spineY} ${x - 70},${y} ${x - 55},${y} M${x + 55},${y} L${x + 70},${y}"/><path class="x" d="M${x + 68},${y - 5} l8,10 m0,-10 l-8,10"/>`);
    }
  }
  for (const b of a.backtracks) {
    const [fx, fy] = pos.get(b.from), [tx, ty] = pos.get(b.to);
    const lift = Math.min(fy, ty) - 30;
    parts.push(`<path class="back" marker-end="url(#arrow)" d="M${fx + 20},${fy - NODE_H / 2} C${fx + 20},${lift} ${tx - 20},${lift} ${tx - 20},${ty - NODE_H / 2}"/>`);
  }
  const node = (id, cls, inner) => `<g class="n ${cls}${id === ui.selected ? " sel" : ""}" data-id="${id}">${inner}</g>`;
  for (const c of r.calls) {
    const x = colX(c.i), it = list.find((i) => i.id === "m" + c.n);
    parts.push(node("m" + c.n, "model" + (it.running ? " running" : "") + (it.bad ? " bad" : ""),
      `<circle cx="${x}" cy="${spineY}" r="13"/><text class="white" x="${x}" y="${spineY + 4}" text-anchor="middle">M${c.n}</text>` +
      `<text class="muted" x="${x}" y="${spineY - 20}" text-anchor="middle">${c.ms ? sec(c.ms) : "…"}</text>` +
      `<text class="muted" x="${x}" y="${spineY + 30}" text-anchor="middle">${c.usage.total_tokens ? kfmt(c.usage.total_tokens) + " tok" : ""}</text>`));
    for (const t of c.tools) {
      const [tx, ty] = pos.get(t);
      parts.push(node(`t${c.n}-${t.index}`, (t.detour ? "detour" : "main") + (t.state === "running" ? " running" : ""),
        `<rect x="${tx - 55}" y="${ty - NODE_H / 2}" width="110" height="${NODE_H}" rx="6"/><text x="${tx}" y="${ty + 4}" text-anchor="middle">${esc((t.retryOf ? "↺ " : "") + clip(t.name, 14))}</text><title>${esc(t.name + (t.reason ? " · " + t.reason : ""))}</title>`));
    }
  }
  const ans = list.find((i) => i.id === "ans"), endItem = list.find((i) => i.id === "end");
  if (ans) parts.push(node("ans", "final", `<circle cx="${tailX}" cy="${spineY}" r="13"/><text class="white" x="${tailX}" y="${spineY + 4}" text-anchor="middle">A</text><text class="muted" x="${tailX}" y="${spineY + 30}" text-anchor="middle">回答</text>`));
  const ex = tailX + (ans ? 60 : 0);
  if (ans) parts.push(`<line class="spine" x1="${tailX + 13}" y1="${spineY}" x2="${ex - 9}" y2="${spineY}" opacity=".3"/>`);
  parts.push(node("end", "end" + (endItem.bad ? " bad" : "") + (r.exit ? "" : " running"), `<circle cx="${ex}" cy="${spineY}" r="9"/><text class="muted" x="${ex}" y="${spineY - 16}" text-anchor="middle">${esc(clip(r.exit || "运行中", 14))}</text>`));
  const map = el("div", "scroll");
  map.innerHTML = `<svg width="${width}" height="${height}" viewBox="0 0 ${width} ${height}"><defs><marker id="arrow" viewBox="0 0 10 10" refX="8" refY="5" markerWidth="7" markerHeight="7" orient="auto"><path d="M0,0 L10,5 L0,10 z" style="fill:var(--think)"/></marker></defs>${parts.join("")}</svg>`;
  map.onclick = (ev) => { const g = ev.target.closest && ev.target.closest("[data-id]"); if (g) select(g.getAttribute("data-id")); };
  const head = el("div", "trackhead");
  head.append(el("span", "", "探索迷宫"), legend([["var(--accent)", "模型调用（主干）"], ["var(--ok)", "主路径工具"], ["var(--err)", "绕路：错误 / 空结果 / 重复"], ["var(--think)", "回退重试 ↺"]]));
  view.append(head, map, ...dataTracks(r, width));
  const inv = el("div", "list");
  inv.append(el("div", "label", `绕路清单 · ${a.detours.length} 处 · 回退 ${a.backtracks.length} 次`));
  if (!a.detours.length) inv.append(el("div", "meta", "没有绕路：每次工具调用都返回了有效结果，也没有用相同参数重复调用。"));
  for (const t of a.detours) {
    const row = el("div", "item", `${callPosition(t.call)} · ${t.name} · ${t.reason}` + (a.backtracks.some((b) => b.from === t) ? " · 之后重试" : ""));
    row.onclick = () => select(`t${t.call.n}-${t.index}`);
    inv.append(row);
  }
  view.append(inv);
}
function dataTracks(r, width) {
  const H = 70, pad = 14, x0 = LEFT - 30, x1 = width - 20;
  const svg = (inner, h) => { const box = el("div", "scroll"); box.innerHTML = `<svg width="${width}" height="${h}" viewBox="0 0 ${width} ${h}">${inner}</svg>`; return box; };
  const baseline = `<line class="grid" x1="${x0}" y1="${H - pad}" x2="${x1}" y2="${H - pad}"/>`;
  // 1. 工具调用密度：每轮工具数，绕路部分标红
  const maxTools = Math.max(1, ...r.calls.map((c) => c.tools.length));
  let s = baseline;
  for (const c of r.calls) {
    if (!c.tools.length) continue;
    const x = colX(c.i) + COL / 2 - 10, unit = (H - 2 * pad) / maxTools;
    const det = c.tools.filter((t) => t.detour).length, ok = c.tools.length - det;
    s += `<rect class="f-main" x="${x}" y="${H - pad - ok * unit}" width="20" height="${ok * unit}"/>`;
    s += `<rect class="f-detour" x="${x}" y="${H - pad - c.tools.length * unit}" width="20" height="${det * unit}"/>`;
    s += `<text x="${x + 26}" y="${H - pad - c.tools.length * unit + 10}">${c.tools.length}</text>`;
  }
  const h1 = el("div", "trackhead");
  h1.append(el("span", "", "工具调用密度"), legend([["var(--ok)", "有效"], ["var(--err)", "绕路"]]));
  // 2. Token 脉冲：每次模型调用的 token 构成
  const maxTok = Math.max(1, ...r.calls.map((c) => c.usage.total_tokens || 0));
  let t = baseline;
  for (const c of r.calls) {
    const p = tokenParts(c.usage), unit = (H - 2 * pad) / maxTok;
    let y = H - pad;
    for (const [cls, v, name] of [["f-cache", p.cached, "缓存命中输入"], ["f-input", p.input, "未缓存输入"], ["f-think", p.reasoning, "推理"], ["f-out", p.output, "输出"]]) {
      if (v <= 0) continue;
      y -= v * unit;
      t += `<rect class="${cls}" x="${colX(c.i) - 11}" y="${y}" width="22" height="${v * unit}"><title>${name} ${v} tokens</title></rect>`;
    }
    if (c.usage.total_tokens) t += `<text x="${colX(c.i) + 16}" y="${y + 10}">${kfmt(c.usage.total_tokens)}</text>`;
  }
  const h2 = el("div", "trackhead");
  h2.append(el("span", "", "Token 脉冲"), legend([["var(--cache)", "缓存命中输入"], ["color-mix(in srgb, var(--accent) 55%, var(--panel))", "未缓存输入"], ["var(--think)", "推理"], ["var(--ok)", "输出"]]));
  // 3. 上下文压力：每次请求的输入 tokens 占窗口比例
  const W = r.inputCapacity || ui.ctxWindow, CH = 90, top = 12, bottom = CH - 14;
  const y = (v) => bottom - Math.min(1, v / W) * (bottom - top);
  let p = `<line class="grid" x1="${x0}" y1="${bottom}" x2="${x1}" y2="${bottom}"/>`;
  for (const [cls, f] of [["lim70", 0.7], ["lim90", 0.9]]) p += `<line class="${cls}" x1="${x0}" y1="${y(W * f)}" x2="${x1}" y2="${y(W * f)}"/><text class="muted" x="${x0}" y="${y(W * f) - 3}">${f * 100}%</text>`;
  const pts = r.calls.filter((c) => c.usage.prompt_tokens).map((c) => [colX(c.i), y(c.usage.prompt_tokens), c.usage.prompt_tokens, c]);
  if (pts.length) p += `<polyline class="curve" points="${pts.map((q) => q[0] + "," + q[1]).join(" ")}"/>`;
  pts.forEach(([x, py, v, c]) => {
    p += `<circle class="dot" cx="${x}" cy="${py}" r="3"><title>${v} / ${W}</title></circle><text x="${x + 6}" y="${py - 5}">${(v / W * 100).toFixed(1)}%</text>`;
    if (c.rebuilt) p += `<text class="warn" x="${x - 14}" y="${bottom - 3}">▼压缩</text>`;
  });
  const h3 = el("div", "trackhead");
  const win = el("input"); win.type = "number"; win.min = "1000"; win.step = "1000"; win.value = W; win.style.width = "96px";
  win.onchange = () => { ui.ctxWindow = Math.max(1000, Number(win.value) || 128000); save("ctxWindow", ui.ctxWindow); render(); };
  win.disabled = !!r.inputCapacity;
  const label = el("label", "legend", r.inputCapacity ? "输入容量 " : "窗口 ");
  label.append(win, r.inputCapacity ? " tokens，来自agent；曲线为API输入量，触发判断见终端used" : " tokens，按所用模型填写");
  h3.append(el("span", "", "上下文压力"), label);
  return [h1, svg(s, H), h2, svg(t, H), h3, svg(p, CH)];
}

// ---------- 视图三：多次运行对比，按轮次自动对齐 ----------
export async function openCompare() {
  stopReplay();
  ui.tab = "compare";
  const ids = [...ui.checked];
  ui.compare = await Promise.all(ids.map(async (id) => { const r = newRun(id); (await fetchEvents(id)).forEach((e) => apply(r, e)); return r; }));
  render();
  loadRuns();
}
export function renderCompare() {
  const view = $("view");
  $("stats").hidden = true;
  view.replaceChildren();
  const runs = ui.compare.filter((r) => ui.checked.has(r.id));
  if (runs.length < 2) return view.append(el("div", "empty", "在左侧勾选 2–5 条运行"));
  const stats = runs.map(analyze);
  const table = el("table", "cmp");
  const headRow = el("tr");
  headRow.append(el("th", "", "指标"), ...runs.map((r) => { const th = el("th", "", r.id); th.title = r.query; return th; }));
  table.append(headRow);
  const rows = [
    ["问题", (r) => clip(r.query, 40)],
    ["用户回合", (r) => r.turns.length],
    ["模型调用（含辅助）", (r, s) => s.calls, true],
    ["其中记忆辅助调用", (r, s) => s.auxCalls],
    ["工具调用", (r, s) => s.toolCount, true],
    ["主路径步数", (r, s) => s.mainLen],
    ["绕路", (r, s) => s.detours.length, true],
    ["回退", (r, s) => s.backtracks.length, true],
    ["总 tokens（含辅助）", (r, s) => s.tokens, true],
    ["其中辅助 tokens", (r, s) => s.auxTokens],
    ["缓存命中", (r, s) => s.cached],
    ["推理 tokens", (r, s) => s.reasoning],
    ["模型耗时（含辅助）", (r, s) => s.modelMs, true, sec],
    ["其中辅助耗时", (r, s) => s.auxMs, false, sec],
    ["总耗时", (r, s) => s.totalMs, true, sec],
    ["结果", (r, s) => r.exit ? (s.failed ? "失败" : s.clean ? "完成 · 干净" : "完成 · 有绕路") : "运行中"],
    ["回答摘要", (r, s) => clip(s.answer || "—", 60)],
  ];
  for (const [name, get, lowerIsBetter, fmt] of rows) {
    const tr = el("tr");
    tr.append(el("th", "", name));
    const values = runs.map((r, i) => get(r, stats[i]));
    const min = Math.min(...values);
    const differs = values.some((v) => v !== values[0]);
    values.forEach((v) => tr.append(el("td", lowerIsBetter && differs && v === min ? "best" : "", fmt ? fmt(v) : String(v))));
    table.append(tr);
  }
  const tableBox = el("div", "scroll");
  tableBox.append(table);
  view.append(el("div", "trackhead", "指标对比（绿色为该项最低）"), tableBox);
  const rounds = Math.max(...runs.map((r) => r.calls.length));
  const cols = `150px repeat(${rounds}, minmax(150px, 1fr))`;
  const lanes = el("div", "lanes scroll");
  const head = el("div", "lane");
  head.style.gridTemplateColumns = cols;
  head.append(el("div", "lanehead", "运行 \\ 请求"));
  for (let k = 1; k <= rounds; k++) head.append(el("div", "lanehead", `主任务第 ${k} 次`));
  lanes.append(head);
  for (const r of runs) {
    const lane = el("div", "lane");
    lane.style.gridTemplateColumns = cols;
    const name = el("div", "lanehead", r.id);
    name.title = r.query;
    name.append(el("div", "", clip(r.query, 24)));
    lane.append(name);
    for (let k = 1; k <= rounds; k++) {
      const c = r.calls[k - 1];
      if (!c) { lane.append(el("div", "cell none")); continue; }
      const cell = el("div", "cell");
      cell.append(el("div", "meta", `${callPosition(c)} · ${c.ms ? sec(c.ms) : "…"} · ${kfmt(c.usage.total_tokens || 0)} tok`));
      cell.onclick = () => select("m" + c.n, r);
      for (const t of c.tools) {
        const chip = el("span", "chip" + (t.detour ? " detour" : ""), (t.retryOf ? "↺ " : "") + t.name);
        chip.title = t.reason || toolSummary(t);
        chip.onclick = (ev) => { ev.stopPropagation(); select(`t${c.n}-${t.index}`, r); };
        cell.append(chip);
      }
      if (!c.compact && !c.tools.length && c.choice) {
        const chip = el("span", "chip answer", "回答");
        chip.onclick = (ev) => { ev.stopPropagation(); select("m" + c.n, r); };
        cell.append(chip);
      }
      lane.append(cell);
    }
    lanes.append(lane);
  }
  view.append(el("div", "trackhead", "按主任务请求的先后对齐（记忆辅助调用计入上方总量，不占格子）；各格标注 Turn / Step（红色为绕路，↺ 为回退重试）"), lanes);
  const inv = el("div", "list");
  inv.append(el("div", "label", "各运行绕路清单"));
  runs.forEach((r, i) => inv.append(el("div", "meta", `${r.id}：` + (stats[i].detours.length ? stats[i].detours.map((t) => `${callPosition(t.call)} ${t.name}（${t.reason}）`).join("；") : "无绕路"))));
  view.append(inv);
  const sel = ui.compareSel && runs.includes(ui.compareSel.run) ? items(ui.compareSel.run).find((i) => i.id === ui.compareSel.id) : null;
  renderInspector(sel);
}

// ---------- 详情面板（inspector） ----------
function inspectorTabs(box, entries) {
  const nav = el("div", "ins-tabs"); nav.setAttribute("role", "tablist"); nav.setAttribute("aria-label", "事件详情");
  const panels = entries.map(([id, label, ...content]) => {
    const button = el("button", "", label), panel = el("div"); panel.append(...content);
    button.id = "detail-tab-" + id; button.setAttribute("role", "tab"); button.setAttribute("aria-controls", "detail-panel-" + id);
    panel.id = "detail-panel-" + id; panel.setAttribute("role", "tabpanel"); panel.setAttribute("aria-labelledby", button.id);
    button.onclick = () => { ui.inspectorTab = id; show(id); };
    nav.append(button); return { id, button, panel };
  });
  const show = (id) => panels.forEach((p) => { p.panel.hidden = p.id !== id; p.button.setAttribute("aria-selected", String(p.id === id)); });
  box.append(nav, ...panels.map((p) => p.panel));
  show(panels.some((p) => p.id === ui.inspectorTab) ? ui.inspectorTab : panels[0].id);
}
export function select(id, run) {
  ui.follow = false;
  if (ui.tab === "compare") ui.compareSel = { id, run };
  else ui.selected = id;
  render();
}
export function renderInspector(it) {
  const box = $("inspector");
  box.replaceChildren();
  if (!it) return box.append(el("div", "empty", "点击事件或节点查看详情"));
  const r = it.run, c = it.call;
  const titles = { system: "SYSTEM · 系统规则与工具定义", user: "USER · 用户问题", input: `模型输入 · 请求 #${c?.n}`, message: `ASSISTANT · 模型输出 #${c?.n}`, compacted: `COMPACTED · 上下文摘要 #${c?.n}`, aux: `${it.label} · 辅助调用 #${c?.n}`, tool: "TOOL · " + it.label, answer: "最终回答", process: "agent 进程" };
  const top = el("div", "top");
  top.append(el("strong", "", titles[it.type]), el("div", "meta", `${r.id} · ${c && it.type !== "user" && it.type !== "system" ? callPosition(c) + " · 请求 #" + c.n : "Turn " + it.turn}` + (it.start ? ` · +${sec(it.start - r.t0)}` : "")));
  box.append(top);
  const kv = el("div", "kv");
  const add = (k, v) => kv.append(el("span", "", k), el("span", "", v));
  if (it.type === "user") return inspectorTabs(box, [
    ["overview", "内容", section("问题", it.message ? message(it.message) : el("div", "text", it.sum))],
    ["raw", "原文", section("消息 JSON", el("pre", "", pretty(it.message || { role: "user", content: it.sum })))],
  ]);
  if (it.type === "system") {
    const systems = (c.req.messages || []).filter((m) => m.role === "system");
    return inspectorTabs(box, [
      ["overview", "系统规则", section("系统提示词", ...systems.map(message))],
      ["tools", "工具定义", section(`${(c.req.tools || []).length} 个工具`, ...(c.req.tools || []).map((t) => fold(t.function?.name || "tool", t)))],
    ]);
  }
  if (it.type === "input") {
    const params = Object.fromEntries(Object.entries(c.req).filter(([k]) => k !== "messages" && k !== "tools"));
    return inspectorTabs(box, [
      ["overview", "messages", requestInput(c)],
      ["tools", "工具定义", section(`工具定义 · ${(c.req.tools || []).length} 个（tools 字段）`, ...(c.req.tools || []).map((t) => fold(t.function?.name || "tool", t)))],
      ["raw", "请求 JSON", section("请求参数与原文", fold("请求参数", params), fold("完整请求 JSON", c.req, true))],
    ]);
  }
  if (["message", "compacted", "aux"].includes(it.type)) {
    const u = c.usage, p = tokenParts(u), m = (c.choice && c.choice.message) || {};
    add("用途", c.aux ? `${AUX_LABEL[c.purpose]}（${c.purpose}，无工具，不是用户回合或最终回答）` : c.compact ? "上下文摘要（不执行工具）" : "任务决策");
    add("公共前缀", `${c.shared || 0} 条消息（实际缓存看usage）`);
    add("耗时", c.ms ? `${c.ms} ms` + (c.firstToken ? `（首字 ${c.firstToken} ms，生成 ${c.ms - c.firstToken} ms）` : c.chunks ? "" : "（非流式，无首字拆分）") : "等待响应");
    if (c.chunks) add("流式", `${c.chunks} 个 chunk；响应 JSON 由观测台按 chunk 拼回`);
    if (c.status) add("HTTP", String(c.status));
    if (u.total_tokens) { add("输入", `${u.prompt_tokens}（缓存 ${p.cached}）`); add("输出", `${u.completion_tokens}（推理 ${p.reasoning}）`); }
    if (c.choice) add("finish_reason", c.choice.finish_reason);
    const input = el("button", "ghost", `查看模型输入 · ${(c.req.messages || []).length} 条 messages`);
    input.onclick = () => select("in" + c.n, r);
    const output = [];
    if (m.reasoning_content) output.push(section(`Thinking · ${m.reasoning_content.length} 字`, el("div", "msg think text", m.reasoning_content)));
    if (m.content) output.push(section(c.aux ? "辅助输出（原文）" : c.compact ? "交接摘要" : "回答内容", el(c.aux ? "pre" : "div", "text", m.content)));
    if (c.tools.length) output.push(section(`工具调用请求 · ${c.tools.length} 项`, ...c.tools.map((t) => fold(`${t.name} · ${t.id}`, t.args, true))));
    if (c.end && !c.choice) output.push(section("错误", el("pre", "err", c.errorText || pretty(c.res))));
    if (!output.length) output.push(section("输出", el("div", "meta", c.end ? "没有文本或工具请求" : "等待模型响应…")));
    inspectorTabs(box, [
      ["overview", "概览", section("概览", kv, input), section("输出概述", el("div", "text", clip(m.content || (c.tools.length ? "调用 " + c.tools.map((t) => t.name).join("、") : c.end ? it.sum : "等待模型响应…"), 240)))],
      ["output", "输出", ...output],
      ["raw", "响应 JSON", section("原文", c.res ? fold("完整响应 JSON", c.res, true) : el("div", "meta", "等待模型响应…"))],
    ]);
  } else if (it.type === "tool") {
    const t = it.tool;
    add("调用 ID", t.id);
    add("发起", `模型调用 #${c.n}，同批第 ${t.index + 1} 项`);
    add("状态", { ok: "有效结果", error: "错误", empty: "空结果", running: "执行中", lost: "没有返回结果" }[t.state]);
    if (t.done) add("批次耗时", `${t.done - c.end} ms`);
    if (t.reason) add("判定", "绕路：" + t.reason);
    if (t.retryOf) add("回退", `重试 ${callPosition(t.retryOf.call)} 失败的 ${t.retryOf.name}`);
    const schema = (c.req.tools || []).find((tool) => tool.function?.name === t.name);
    inspectorTabs(box, [
      ["overview", "概览", section("概览", kv), section("执行摘要", el("div", "text", toolSummary(t))), section("判定规则", el("div", "meta", "结果取自下一次模型请求中 tool_call_id 相同的 tool 消息。返回错误、空结果、用相同参数重复调用判为绕路；同名工具在绕路后再次调用记为回退。"))],
      ["input", "参数", section("参数", el("pre", "", pretty(t.args)))],
      ["output", "结果", section("结果", t.done ? toolResult(t) : el("div", "meta", t.state === "lost" ? "进程已结束，未收到结果" : "执行中…"))],
      ["schema", "Schema", section("调用时工具定义", el("pre", "", schema ? pretty(schema) : "此请求没有记录工具定义"))],
      ["raw", "原文", section("参数与观测结果", el("pre", "", pretty({ tool_call_id: t.id, name: t.name, arguments: t.args, observation: t.result })))],
    ]);
  } else if (it.type === "answer") {
    box.append(section("回答", el("div", "text", c.choice.message.content)));
  } else {
    box.append(section("状态", el("div", it.bad ? "err" : "meta", r.exit || "运行中")));
    const log = el("pre", "log");
    r.logs.forEach((l) => log.append(el("div", l.kind, l.text || " ")));
    box.append(section(`终端输出 · ${r.logs.length} 行`, log));
  }
}

function requestInput(c) {
  const messages = c.req.messages || [];
  const cards = messages.map((m, i) => {
    const card = message(m);
    card.querySelector(".role").prepend(`#${i + 1} · `);
    return card;
  });
  return section(`全部 messages · ${messages.length} 条`,
    el("div", "meta", "按原始请求顺序展示，包括 system、用户消息、assistant 工具请求与 tool 结果。工具定义位于独立的 tools 字段，不计入 messages。"), ...cards);
}

