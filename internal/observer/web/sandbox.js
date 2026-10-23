import { renderChat, step } from "./chat.js";
import { bashExit } from "./explore.js";
import { clip, el, ico, parse, pretty, sec, ui } from "./util.js";
// ---------- Day 12 bash 沙箱 ----------
export function bashCard(r, c, t) {
  const args = parse(t.args) || {}, res = parse(t.result), out = res?.result || {};
  const running = !t.done && !r.exit;
  const icon = running ? el("span", "ico") : ico(res?.status === "ok" ? "ok" : "bad", res?.status === "ok" ? "ok" : "bad");
  if (running) icon.append(el("span", "spin"));
  const head = [icon, el("span", "hub-chip", "bash"), el("span", "targs", clip(args.command || t.args, 90))];
  // D32：background=true 的命令立刻返回 job_id，没有退出码；之后用 bash_output 读输出。
  const code = bashExit(out);
  if (out.job_id) head.push(el("span", "exit-chip", "后台 " + out.job_id));
  else if (res?.status === "ok" && (out.signal || (code !== undefined && code !== 0))) head.push(el("span", "exit-chip bad", out.signal ? out.signal : "exit " + code));
  if (t.done) head.push(el("span", "", sec(t.done - c.end)));
  const term = el("pre", "term");
  term.append(el("span", "cmd", "$ " + (args.command || t.args) + "\n"));
  if (res?.status === "ok") term.append(out.output || out.preview || (out.job_id ? "（在后台运行）" : "（无输出）"));
  else if (res) term.append(res.error || pretty(res));
  else term.append(running ? "执行中…" : t.state === "lost" ? "进程已结束，没有结果" : "");
  const body = [term];
  const notes = [];
  if (out.truncated) notes.push(`输出共 ${out.output_bytes} 字节，只保留开头和结尾各 8KB`);
  if (out.saved_output) notes.push(`结果共 ${out.total_lines} 行，上面是模型看到的预览；完整内容存为 ${out.saved_output}，模型用 read_output 读回`);
  if (out.note) notes.push(out.note);
  if (out.workspace_changes) notes.push("改了工作区：" + out.workspace_changes.join("、") + "（没有快照，撤销时不会还原）");
  // 旧存档只有主机名，原因一律是“不在白名单”；之后的记录带 network_hint，写着每个主机被拒的原因。
  if (out.network_denied) notes.push(out.network_hint?.startsWith("访问被代理拒绝") ? out.network_hint : "被代理拒绝的主机：" + out.network_denied.join("、") + "（不在白名单）");
  if (res?.status === "unknown") notes.push("超时或被停止：沙箱里的进程已全部结束；命令是否已经产生副作用不确定");
  for (const n of notes) body.push(el("div", "meta", n));
  return step(`t${c.n}-${t.index}`, head, body);
}
// 模型只能发起请求；“允许 / 拒绝”只有人在页面上点。B0 起 agent 在这里暂停等待：页面上点了，观测台当场回复，本轮接着执行；
// 决定同时记进对话记录，之后的续聊作为 -net-allow 传给 agent。
export function netCard(r, t) {
  const args = parse(t.args) || {}, res = parse(t.result), domain = res?.result?.domain || args.domain || "";
  const done = { already_allowed: "已在白名单", allowed: "已允许联网", denied: "已拒绝联网" }[res?.result?.status];
  return netBox(domain, "沙箱请求访问 ", "理由：" + (args.reason || ""), { done, open: res?.status === "ok", idle: res ? (res.error || "请求无效") : "等待中…", waitText: "agent 正在等待：批准后本轮立即生效" });
}
// 命令连到不在白名单的域名时，代理当场发起审批，命令停在那里等：批准后同一条命令接着跑。卡片跟在发起它的 bash 后面。
export function proxyNetCards(t) {
  return Object.entries(ui.run.netAsks[t.id] || {}).map(([domain, reason]) =>
    netBox(domain, "命令要联网 ", reason, { open: true, waitText: "命令正停在代理里等待：批准后接着执行" }));
}
// “允许”只在本对话内（续聊沿用）；“始终允许”另外写进你的个人设置（沙箱联网白名单），之后新开的对话都不再询问。
// 同 Claude Code：只有等人答复时是一张带按钮的卡片，答过的只留一行，理由与生效范围放在悬停提示里。
const NET_EFFECT = { "已允许联网": "本对话内可经代理访问它及其子域名（agent 正在等的当场生效，否则从下一轮起）", "已始终允许联网": "写进了你的个人设置，之后新开的对话都可经代理访问它及其子域名" };
function netBox(domain, head, why, { done, open, idle, waitText }) {
  const root = ui.run, decision = root.netDecision[domain], waiting = !!root.netPending[domain];
  const settled = decision === "always" ? "已始终允许联网" : done || (decision && !waiting ? (decision === "allow" ? "已允许联网" : "已拒绝联网") : "");
  if (settled || !(waiting || open)) {
    const line = el("div", "net-line");
    line.append((settled || idle) + " ", el("b", "", domain));
    line.title = [why, NET_EFFECT[settled]].filter(Boolean).join("\n");
    return line;
  }
  const card = el("div", "net-card");
  card.append(el("div", "", head), el("b", "", domain), el("div", "why", why));
  const acts = el("div", "acts");
  const yes = el("button", "yes", "允许"), always = el("button", "", "始终允许"), no = el("button", "", "拒绝"), buttons = [yes, always, no];
  for (const b of buttons) b.type = "button";
  yes.title = "只在本对话内允许，续聊沿用";
  always.title = "同时加进你的个人设置（沙箱联网白名单），之后新开的对话都不再询问";
  const decide = async (allow, forever) => {
    for (const b of buttons) b.disabled = true;
    try {
      const resp = await fetch(`/runs/${encodeURIComponent(root.id)}/network`, { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ domain, allow, always: forever }) });
      if (!resp.ok) throw new Error(await resp.text());
      root.netDecision[domain] = !allow ? "deny" : forever ? "always" : "allow";
      delete root.netPending[domain];
      renderChat();
    } catch (err) { alert("操作失败：" + err.message); for (const b of buttons) b.disabled = false; }
  };
  yes.onclick = () => decide(true, false); always.onclick = () => decide(true, true); no.onclick = () => decide(false, false);
  acts.append(...buttons, el("span", "meta", waiting ? waitText : "批准后下一轮生效"));
  card.append(acts);
  return card;
}
// D20：高风险操作确认。规则在 agent 的执行器里，命中后 agent 暂停等这里的答复；“允许”只对这一项调用有效。
// 外发类另有“本对话内都允许”：范围由 agent 给出（搜索按工具，读网页按工具和主机），之后同一范围不再询问，续聊沿用，审计照记。
const RISK_LABEL = { irreversible: "不可撤回", destructive: "破坏性操作", outbound: "可能外发私有数据", redo: "重做中断的操作", permission: "权限设置要求确认" };
export function approvalCard(root, callID) {
  const a = root.approvals[callID];
  const card = el("div", "net-card");
  card.append(el("div", "", `需要你确认 · ${RISK_LABEL[a.risk] || a.risk} · `), el("b", "", a.tool), el("div", "why", "原因：" + a.reason + "\n参数：" + a.arguments));
  const acts = el("div", "acts");
  if (a.state === "waiting") {
    const yes = el("button", "yes", "允许"), no = el("button", "", "拒绝");
    const always = a.scope ? el("button", "", `本对话内 ${a.scope} 都允许`) : null;
    const buttons = [yes, always, no].filter(Boolean);
    for (const b of buttons) b.type = "button";
    const decide = async (allow, session) => {
      for (const b of buttons) b.disabled = true;
      try {
        const resp = await fetch(`/runs/${encodeURIComponent(root.id)}/approval`, { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ call_id: callID, allow, session }) });
        if (!resp.ok) throw new Error(await resp.text());
        a.state = (await resp.json()).decision;
        renderChat();
      } catch (err) { alert("操作失败：" + err.message); for (const b of buttons) b.disabled = false; }
    };
    yes.onclick = () => decide(true, false); no.onclick = () => decide(false, false);
    if (always) always.onclick = () => decide(true, true);
    acts.append(...buttons, el("span", "meta", always ? "agent 正在等待：“允许”只对这一次调用有效" : "agent 正在等待：只对这一次调用有效"));
  } else acts.append(el("span", "meta", { allow: "已允许", allow_session: `已允许，本对话内 ${a.scope || "同一范围"} 不再询问`, deny: "已拒绝", expired: "未答复（agent 已结束或等待超时）" }[a.state] || a.state));
  card.append(acts);
  return card;
}
// D34：模型向你提问（ask_user）。agent 在这里等：点一个选项或写一句话，回答作为这次工具调用的结果交给模型。
// 页面随新事件整页重画，输入到一半的草稿记在提问上，重画后放回去、保持焦点。
export function askCard(root, t) {
  const args = parse(t.args) || {}, a = root.asks[t.id], res = parse(t.result);
  const card = el("div", "net-card ask-card");
  card.append(el("div", "", "agent 问你："), el("b", "", args.question || ""));
  const acts = el("div", "acts");
  if (a?.state === "waiting" && !root.exit) {
    const buttons = [];
    const send = async (answer) => {
      for (const b of buttons) b.disabled = true;
      try {
        const resp = await fetch(`/runs/${encodeURIComponent(root.id)}/answer`, { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ call_id: t.id, answer }) });
        if (!resp.ok) throw new Error(await resp.text());
        Object.assign(a, { state: "answered", answer });
        ui.askFocus = null;
        renderChat();
      } catch (err) { alert("回答失败：" + err.message); for (const b of buttons) b.disabled = false; }
    };
    for (const option of args.options || []) {
      const b = el("button", "", option);
      b.type = "button";
      b.onclick = () => send(option);
      buttons.push(b);
    }
    const input = el("input", "ask-input");
    input.placeholder = (args.options || []).length ? "或者写下你的回答，回车发送" : "写下你的回答，回车发送";
    input.value = a.draft || "";
    input.oninput = () => { a.draft = input.value; ui.askFocus = t.id; };
    input.onkeydown = (ev) => { if (ev.key === "Enter" && !ev.isComposing && input.value.trim()) { ev.preventDefault(); send(input.value.trim()); } };
    buttons.push(input);
    acts.append(...buttons);
    if (ui.askFocus === t.id) queueMicrotask(() => { input.focus(); input.setSelectionRange(input.value.length, input.value.length); });
  } else {
    const answer = a?.answer ?? res?.result?.answer;
    acts.append(el("span", "meta", answer != null ? "你的回答：" + answer : res?.status === "error" ? res.error || "" : res?.result?.answered === false ? "没有回答（等待超时）" : root.exit ? "没有回答（agent 已结束）" : "等待中…"));
  }
  card.append(acts);
  return card;
}
