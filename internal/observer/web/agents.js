import { analyze } from "./analysis.js";
import { WEB_TOOLS, renderWebPanel, webLive } from "./browser.js";
import { renderCall, renderChat, renderCompact } from "./chat.js";
import { renderFilesPanel } from "./files.js";
import { md } from "./md.js";
import { $, clip, el, fold, parse, ui } from "./util.js";
// ---------- 子 agent ----------
function subList(r) {
  return r.calls.flatMap((c) => c.tools.filter((t) => t.name === "spawn_agent").map((t) => {
    const info = r.subCalls[t.id];
    return { t, info, sub: info?.task ? r.subs[info.task] : null };
  }));
}
// 状态优先看父 agent 打印的起止，其次看回填给父 agent 的 tool 结果。
function subState(r, item) {
  const { t, info, sub } = item, res = parse(t.result), calls = sub ? sub.calls.length : 0;
  if (res?.status === "ok") return { key: "done", text: (res.result?.replayed ? "取回存档" : "完成") + ` · ${res.result?.model_calls ?? calls} 次请求` };
  if (res?.status === "unknown" || res?.status === "not_run") return { key: "stopped", text: "已中断" };
  if (res?.status === "error") return { key: "failed", text: "失败" };
  if (info?.state === "done") return { key: "done", text: (info.replayed ? "取回存档" : "完成") + ` · ${info.calls} 次请求` };
  if (info?.state === "failed") return { key: "failed", text: "失败" };
  if (info?.state === "stopped") return { key: "stopped", text: "已中断" };
  if (r.exit) return { key: "stopped", text: info ? "已中断" : "未启动" };
  return { key: "running", text: info ? `运行中 · ${calls} 次请求` : "启动中" };
}
export function agentCard(r, t) {
  const list = subList(r), i = list.findIndex((x) => x.t.id === t.id), item = list[i], st = subState(r, item), args = parse(t.args);
  const card = el("button", "agent-card" + (ui.subOpen === t.id ? " on" : ""));
  card.type = "button";
  card.title = "在右侧查看这个子 agent 的完整过程";
  const text = el("span", "t");
  // 只传 task_id 续跑的子任务，显示它的任务原文（来自 subagent/started），而不是一串ID。
  const task = args?.task ?? (args?.task_id ? "续跑：" + (item?.info?.text || args.task_id) : t.args);
  text.append(el("b", "", `子 agent ${i + 1}`), el("span", "", clip(task, 140)));
  card.append(el("span", "av", String(i + 1)), text, el("span", "sub-st " + st.key, st.text), el("span", "chev", "›"));
  card.onclick = () => { ui.subOpen = ui.subOpen === t.id ? null : t.id; ui.webOpen = null; ui.filesOpen = false; renderChat(); };
  return card;
}
export function renderSubPanel() {
  // 运行中开始浏览时自动打开浏览器面板并跟随；用户关掉或正在看子 agent 时不打扰。
  const r0 = ui.run;
  if (r0 && !r0.exit && !ui.webOpen && !ui.subOpen && !ui.filesOpen && !ui.webClosed && r0.calls.some((c) => c.tools.some((t) => WEB_TOOLS.includes(t.name) && webLive(r0, t)))) ui.webOpen = { follow: true };
  if (ui.filesOpen) return renderFilesPanel();
  if (ui.webOpen) return renderWebPanel();
  const panel = $("subpanel"), r = ui.run, list = r && ui.page === "chat" ? subList(r) : [];
  const item = list.find((x) => x.t.id === ui.subOpen);
  if (!item) { panel.hidden = true; panel.replaceChildren(); return; }
  const oldBody = panel.querySelector(".sp-body");
  const stick = !oldBody || oldBody.scrollHeight - oldBody.scrollTop - oldBody.clientHeight < 80, top = oldBody?.scrollTop || 0, sameAgent = panel.dataset.agent === item.t.id;
  panel.hidden = false;
  panel.dataset.agent = item.t.id;
  const head = el("div", "sp-head"), close = el("button", "sp-close", "✕");
  close.type = "button"; close.title = "关闭"; close.setAttribute("aria-label", "关闭子 agent 面板");
  close.onclick = () => { ui.subOpen = null; renderChat(); };
  head.append(el("strong", "", `子 agent · ${list.length} 个`), close);
  const nav = el("div", "sp-list");
  list.forEach((x, i) => {
    const st = subState(r, x), row = el("button", "sp-item" + (x === item ? " on" : ""));
    row.type = "button";
    row.append(el("span", "n", "#" + (i + 1)), el("span", "q", clip(parse(x.t.args)?.task ?? x.t.args, 60)), el("span", "sub-st " + st.key, st.text));
    row.onclick = () => { ui.subOpen = x.t.id; renderChat(); };
    nav.append(row);
  });
  const body = el("div", "sp-body"), st = subState(r, item), res = parse(item.t.result);
  body.append(el("div", "label", "任务 · 父 agent 写给子 agent 的全部输入（看不到父对话）"), el("div", "sp-task", parse(item.t.args)?.task ?? item.t.args));
  body.append(el("div", "meta", `子任务ID ${item.info?.task || "—"} · 调用 ${item.t.id}`));
  const sub = item.sub;
  if (sub) {
    // 子进程的结局来自父 agent 的起止记录；面板据此判断“没有结果”的工具是还在跑，还是进程已经结束。
    sub.exit = st.key === "running" ? "" : st.key === "done" ? "exit 0" : st.text;
    analyze(sub);
    const flowBox = el("div", "a");
    sub.calls.forEach((c, i) => c.compact ? flowBox.append(renderCompact(c)) : renderCall(flowBox, sub, c, i === sub.calls.length - 1));
    body.append(flowBox);
    if (sub.termination) body.append(el("div", "stopcard err", "子 agent 停止：" + sub.termination));
    if (sub.logs.length) {
      const logs = fold(`子进程终端输出 · ${sub.logs.length} 行`, sub.logs.map((l) => l.text).join("\n"));
      body.append(logs);
    }
  }
  if (!sub?.calls.length && res?.result?.replayed) {
    body.append(el("div", "banner", "这个子任务之前已经完成，本次按子任务ID直接取回存档答案，没有发出新的模型请求。"), md(res.result.answer || ""));
  } else if (!sub?.calls.length) body.append(el("div", st.key === "running" ? "shimmer" : "meta", st.key === "running" ? "正在启动子 agent…" : "没有模型请求记录"));
  if (st.key === "running" && sub?.calls.length && sub.calls.at(-1).end && sub.calls.at(-1).tools.every((t) => t.done)) body.append(el("div", "shimmer", "思考中…"));
  panel.replaceChildren(head, nav, body);
  body.scrollTop = sameAgent && !stick ? top : body.scrollHeight;
}
