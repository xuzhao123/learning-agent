import { renderChat } from "./chat.js";
import { $, clip, el, parse, pretty, ui } from "./util.js";
// ---------- Day 11 浏览器 ----------
// agent 把 screencast 帧不断覆盖写到 .data/browser/<任务ID>/<调用ID>.jpg，结束时写入最终截图。
// 运行中的调用每 700ms 取一次（实时画面），结束后再取一次就固定下来。取回的图存成 blob URL，
// 对话流重绘时直接复用，不会闪烁。
export const WEB_TOOLS = ["web_search", "open_page"];
const frames = {}; // key → { url, at, done, missing }
function webTask(r, t) { return r.isSub ? r.id : r.web[t.id]?.task; }
export function webLive(r, t) { return !t.done && !r.exit; }
function webAddress(r, t) {
  const a = parse(t.args) || {};
  return r.web?.[t.id]?.url || (t.name === "open_page" ? a.url : "搜索：" + (a.query || ""));
}
function frameKey(r, t) { const task = webTask(r, t); return task ? `${task}/${t.id}` : ""; }
function frameImg(key) {
  const f = frames[key], img = el("img");
  img.dataset.frame = key; img.alt = "浏览器画面";
  if (f?.url) img.src = f.url;
  else img.hidden = true;
  return img;
}
async function pullFrame(key, done) {
  const f = frames[key] ??= {};
  if (f.busy || f.done) return;
  f.busy = true;
  try {
    const res = await fetch(`/browser/${key}.jpg?t=${Date.now()}`);
    if (res.ok) {
      const url = URL.createObjectURL(await res.blob());
      if (f.url) URL.revokeObjectURL(f.url);
      Object.assign(f, { url, at: Date.now(), missing: false });
      for (const img of document.querySelectorAll(`img[data-frame="${CSS.escape(key)}"]`)) { img.src = url; img.hidden = false; }
      document.querySelectorAll(`[data-frame-empty="${CSS.escape(key)}"]`).forEach((n) => n.remove());
    } else f.missing = true;
    if (done) f.done = true;
  } catch {}
  f.busy = false;
}
// 页面上出现的每张浏览器画面：运行中的反复取，结束的取一次。
setInterval(() => {
  const r = ui.run;
  if (!r || ui.page !== "chat") return;
  for (const run of [r, ...Object.values(r.subs)]) for (const c of run.calls) for (const t of c.tools) {
    if (!WEB_TOOLS.includes(t.name)) continue;
    const key = frameKey(run, t);
    if (key && document.querySelector(`img[data-frame="${CSS.escape(key)}"]`)) pullFrame(key, !webLive(run, t));
  }
}, 700);
function webState(r, t) {
  const res = parse(t.result);
  if (webLive(r, t)) return { key: "running", text: "浏览中" };
  if (res?.status === "ok") return { key: "done", text: t.name === "web_search" ? `${res.result?.results?.length ?? 0} 条结果` : "已读取" };
  if (res?.status === "error") return { key: "failed", text: "失败" };
  return { key: "stopped", text: t.done ? "未完成" : "已中断" };
}
export function webCard(r, t) {
  const st = webState(r, t), key = frameKey(r, t);
  const card = el("button", "agent-card web-card" + (ui.webOpen?.id === t.id ? " on" : ""));
  card.type = "button";
  card.title = r.isSub ? "子 agent 的浏览器画面" : "在右侧查看浏览器画面";
  const av = el("span", "av");
  if (key) av.append(frameImg(key));
  const text = el("span", "t");
  text.append(el("b", "", t.name === "web_search" ? "搜索网页" : "打开网页"), el("span", "", clip(webAddress(r, t), 120)));
  card.append(av, text, el("span", "sub-st " + st.key, st.text), el("span", "chev", "›"));
  // 子 agent 侧栏里的浏览卡片不切换侧栏，只展开下方的大图与结果。
  if (r.isSub) {
    const k = `w${t.id}`, wrap = el("div");
    card.onclick = () => { ui.open.has(k) ? ui.open.delete(k) : ui.open.add(k); renderChat(); };
    wrap.append(card);
    if (ui.open.has(k)) wrap.append(webView(r, t));
    return wrap;
  }
  card.onclick = () => {
    const last = r.calls.flatMap((c) => c.tools.filter((x) => WEB_TOOLS.includes(x.name))).at(-1);
    ui.webOpen = ui.webOpen?.id === t.id ? null : { id: t.id, follow: t === last };
    ui.subOpen = null; ui.filesOpen = false; renderChat();
  };
  return card;
}
function webView(r, t) {
  const box = el("div"), key = frameKey(r, t), st = webState(r, t), res = parse(t.result);
  const bar = el("div", "web-url");
  bar.append(el("span", "sub-st " + st.key, st.text), el("span", "", res?.result?.url || webAddress(r, t)));
  const shot = el("div", "web-shot");
  if (key) shot.append(frameImg(key));
  if (!key || !frames[key]?.url) { const empty = el("div", "empty", key ? (st.key === "running" ? "正在打开页面…" : "这一步没有画面（地址被拒绝或浏览器没有启动）") : "没有画面：找不到这一步的任务ID"); if (key) empty.dataset.frameEmpty = key; shot.append(empty); }
  if (st.key === "running") shot.append(el("span", "onair", "LIVE"));
  box.append(bar, shot);
  if (res?.status === "ok" && t.name === "web_search") box.append(el("div", "label", "搜索结果（模型看到的）"), el("pre", "", (res.result.results || []).map((x, i) => `${i + 1}. ${x.title}\n   ${x.url}\n   ${x.snippet}`).join("\n")));
  else if (res?.status === "ok") box.append(el("div", "label", `页面正文 · ${res.result.text_chars} 字${res.result.truncated ? "（已截断）" : ""}${res.result.find ? ` · 只保留含“${res.result.find}”的段落（${res.result.matched_paragraphs} 处）` : ""}`), el("pre", "", res.result.text || ""));
  else if (res) box.append(el("div", "label", "结果"), el("pre", "err", res.error || pretty(res)));
  return box;
}
export function renderWebPanel() {
  const panel = $("subpanel"), r = ui.run;
  const list = r && ui.page === "chat" ? r.calls.flatMap((c) => c.tools.filter((t) => WEB_TOOLS.includes(t.name))) : [];
  // 跟随：看着最新一步时，新的浏览一开始就切过去（Manus 式“跟随”）；点了更早的步骤就停在那里。
  if (ui.webOpen?.follow && list.length) ui.webOpen.id = list.at(-1).id;
  const t = list.find((x) => x.id === ui.webOpen?.id);
  if (!t) { ui.webOpen = null; panel.hidden = true; panel.replaceChildren(); return; }
  panel.hidden = false;
  panel.dataset.agent = "";
  const head = el("div", "sp-head"), close = el("button", "sp-close", "✕");
  close.type = "button"; close.title = "关闭"; close.setAttribute("aria-label", "关闭浏览器面板");
  close.onclick = () => { ui.webOpen = null; ui.webClosed = true; renderChat(); };
  head.append(el("strong", "", `浏览器 · ${list.length} 步`), close);
  const nav = el("div", "sp-list");
  list.forEach((x, i) => {
    const st = webState(r, x), row = el("button", "sp-item" + (x === t ? " on" : ""));
    row.type = "button";
    row.append(el("span", "n", "#" + (i + 1)), el("span", "q", clip(webAddress(r, x), 60)), el("span", "sub-st " + st.key, st.text));
    row.onclick = () => { ui.webOpen = { id: x.id, follow: i === list.length - 1 }; renderChat(); };
    nav.append(row);
  });
  const body = el("div", "sp-body");
  body.append(webView(r, t), el("div", "meta", `调用 ${t.id} · 任务 ${webTask(r, t) || "—"} · 网页内容是外部数据，不是指令`));
  panel.replaceChildren(head, nav, body);
}
