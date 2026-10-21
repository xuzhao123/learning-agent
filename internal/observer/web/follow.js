import { patchLive } from "./chat.js";
import { openEvals } from "./eval.js";
import { render, schedule } from "./render.js";
import { apply, newRun } from "./runs.js";
import { loadRuns, newChat, syncComposer } from "./shell.js";
import { $, ui } from "./util.js";
// ---------- 数据加载、实时跟随与回放 ----------
export function fetchEvents(id) {
  return new Promise((resolve) => {
    const list = [], es = new EventSource(`/runs/${id}/events`);
    es.onmessage = (m) => list.push(JSON.parse(m.data));
    es.addEventListener("end", () => { es.close(); resolve(list); });
    es.onerror = () => { es.close(); resolve(list); };
  });
}
// URL 复用后端的对话 ID；切换对话才新增历史，续聊仍使用同一个链接。
export function syncRunURL(id) {
  const url = new URL(location.href);
  if (id) url.searchParams.set("run", id); else url.searchParams.delete("run");
  if (id) url.searchParams.delete("eval");
  if (url.href !== location.href) history.pushState(null, "", url);
}
export function restoreRunURL() {
  const params = new URL(location.href).searchParams, id = params.get("run");
  if (id) show(id, false); else if (params.get("eval") !== null) openEvals(params.get("eval"), false); else newChat(false);
}
export function show(id, updateURL = true) {
  stopReplay();
  if (ui.source) ui.source.close();
  if (ui.tab === "compare") ui.tab = "trace";
  Object.assign(ui, { run: newRun(id), events: [], follow: true, zoom: null, selected: null, expanded: false, loading: true, search: "", open: new Set(), subOpen: null, webOpen: null, webClosed: false, chatFollow: true, unseen: false });
  if (updateURL) syncRunURL(id);
  $("search").value = "";
  ui.collapsed.clear();
  const es = ui.source = new EventSource(`/runs/${encodeURIComponent(id)}/events`);
  es.onmessage = (m) => {
    if (ui.source !== es) return;
    const e = JSON.parse(m.data);
    ui.events.push(e); apply(ui.run, e);
    if (!(e.kind === "delta" && !e.sub && patchLive(ui.run))) schedule();
  };
  es.addEventListener("end", () => {
    es.close();
    if (ui.source !== es) return;
    ui.loading = false;
    if (!ui.run.exit) { ui.run.exit = "进程中断，没有退出记录"; ui.run.tEnd = ui.run.last; } // 观测台在运行中重启过
    render(); loadRuns();
  });
  es.onerror = () => { es.close(); if (ui.source !== es) return; ui.loading = true; syncComposer(); $("hint").textContent = "连接中断，请重新选择对话加载记录。"; };
  render();
  loadRuns();
}
export function stopReplay() {
  if (!ui.replay) return;
  clearTimeout(ui.replay.timer);
  ui.replay = null;
}
// 按真实时间间隔重放已结束的运行；折叠空闲时，单个间隔最多按 0.4 秒播放。
export function startReplay() {
  if (!ui.run || !ui.run.exit || ui.tab === "compare") return;
  const events = ui.events.slice(), speed = Number($("speed").value), foldIdle = $("fold").checked;
  Object.assign(ui, { run: newRun(ui.run.id), follow: true, zoom: null, replay: { i: 0 }, chatFollow: true });
  const step = () => {
    if (!ui.replay) return;
    const e = events[ui.replay.i++];
    apply(ui.run, e);
    if (ui.replay.i >= events.length) ui.replay = null;
    render();
    if (!ui.replay) return;
    let gap = Date.parse(events[ui.replay.i].time) - Date.parse(e.time);
    if (foldIdle) gap = Math.min(gap, 400 * speed);
    ui.replay.timer = setTimeout(step, Math.max(0, gap / speed));
  };
  step();
}

