import { analyze, endLabel, items } from "./analysis.js";
import { renderChat } from "./chat.js";
import { renderCompare, renderInspector, renderMaze, renderSpans, renderTrace } from "./observe.js";
import { renderShell, syncComposer } from "./shell.js";
import { $, el, kfmt, sec, ui } from "./util.js";
// ---------- 渲染入口 ----------
export function schedule() {
  if (ui.queued) return;
  ui.queued = true;
  requestAnimationFrame(() => {
    ui.queued = false;
    // 流式输出时每段增量都会重建对话区；鼠标按下期间先不重建，否则按下与松开落在不同元素上，点击（展开思考等）会丢。
    if (ui.pressing) ui.held = true;
    else render();
  });
}
$("thread").addEventListener("pointerdown", () => { ui.pressing = true; });
for (const type of ["pointerup", "pointercancel"]) addEventListener(type, () => {
  ui.pressing = false;
  if (ui.held) { ui.held = false; setTimeout(schedule); } // 等 click 事件先处理完
});
export function render() {
  syncComposer();
  renderShell();
  if (ui.page === "chat") return renderChat();
  for (const tab of ["trace", "maze", "spans", "compare"]) $("tab-" + tab).className = ui.tab === tab ? "on" : "";
  $("tab-compare").disabled = ui.checked.size < 2;
  $("tab-compare").textContent = "对比" + (ui.checked.size ? " " + ui.checked.size : "");
  $("search").hidden = $("expand").hidden = ui.tab !== "trace";
  $("replay").textContent = ui.replay ? "■ 停止" : "▶ 回放";
  $("terminal").disabled = !ui.run || ui.tab === "compare";
  $("attribute").disabled = !ui.run || !ui.run.exit || !!ui.run.source || !!ui.replay || ui.tab === "compare";
  if (ui.tab === "compare") return renderCompare();
  const r = ui.run;
  if (!r) return;
  const a = analyze(r);
  const list = items(r);
  if (ui.follow) {
    const pick = r.exit ? list.find((i) => i.id === (r.exit === "exit 0" && r.calls.length ? "m" + r.calls.at(-1).n : "end")) : list.filter((i) => !i.hidden && i.type !== "process").pop();
    ui.selected = pick.id;
  }
  renderStats(r, a);
  if (ui.tab === "spans") return renderSpans(r);
  if (ui.tab === "trace") renderTrace(r, a, list); else renderMaze(r, a, list);
  renderInspector(list.find((i) => i.id === ui.selected));
}
function renderStats(r, a) {
  const box = $("stats");
  box.hidden = false;
  box.replaceChildren();
  const add = (k, v, cls) => { const s = el("span", "", k + " "); s.append(el("b", cls || "", v)); box.append(s); };
  add("用户回合", r.turns.length);
  add("模型调用", a.calls + (a.auxCalls ? `（主任务 ${r.calls.length} + 辅助 ${a.auxCalls}）` : ""));
  add("工具调用", a.toolCount);
  add("主路径", a.mainLen + " 步");
  add("绕路", a.detours.length, a.detours.length ? "bad" : "");
  add("回退", a.backtracks.length, a.backtracks.length ? "bad" : "");
  add("tokens", kfmt(a.tokens) + (a.cached ? `（缓存 ${kfmt(a.cached)}）` : "") + (a.auxTokens ? `（辅助 ${kfmt(a.auxTokens)}）` : ""));
  add("模型耗时", sec(a.modelMs));
  add("总耗时", sec(a.totalMs));
  if (ui.replay) add("状态", "回放中");
  else if (!r.exit) add("状态", "运行中");
  else add("状态", a.failed ? "未正常结束：" + endLabel(r) : a.clean ? "完成 · 干净" : "完成 · 有绕路", a.failed || !a.clean ? "bad" : "good");
}

