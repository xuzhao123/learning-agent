import { show } from "./follow.js";
import { md } from "./md.js";
import { openHubPage } from "./pages.js";
import { attribute } from "./shell.js";
import { $, clip, el, fold, kfmt, ui } from "./util.js";
// ---------- D16 评测 · D17 归因 ----------
// 评测记录在 .data/eval/<ID>.json（与 go run . eval 共用）；每个试次是一条普通对话，点击即可看轨迹与调用链。
const pctf = (v) => (v * 100).toFixed(0) + "%";
const ARM = { with: "", without: "对照", replay: "回放" };
$("evals").onclick = () => openEvals(ui.evalOpen || "");
function evalURL(id) {
  const url = new URL(location.href);
  url.searchParams.delete("run");
  url.searchParams.set("eval", id || "");
  if (url.href !== location.href) history.pushState(null, "", url);
}
async function evalPost(url, body) {
  const res = await fetch(url, { method: "POST", headers: { "Content-Type": "application/json" }, body: body ? JSON.stringify(body) : undefined });
  if (!res.ok) throw new Error(await res.text());
}
export async function openEvals(id, updateURL = true) {
  clearTimeout(ui.evalTimer);
  const panel = await openHubPage(id ? "评测 " + id : "评测与归因", "");
  if (!panel) return;
  panel.classList.add("evalpanel");
  if (updateURL) evalURL(id);
  ui.evalOpen = id;
  if (id) return evalDetail(panel, id);
  panel.querySelector("p.meta").textContent = "评测集 eval/cases.jsonl：固定题目、参考答案和打分器（先确定性检查，开放题由 DeepSeek 三票评审）。每道题跑 k 次，每次是一个新的 agent 任务；报告 pass@1（含按题聚类的标准误）与 pass^k，答对率和完成率分开。命令行 go run . eval 读写同一份记录。";
  let cases, list;
  try { [cases, list] = await Promise.all([(await fetch("/evals/cases")).json(), (await fetch("/evals")).json()]); }
  catch (err) { panel.lastChild.replaceWith(el("div", "err", "读取失败：" + err.message)); return; }
  if (!panel.isConnected) return;
  const form = el("div", "eval-form"), set = el("select"), k = el("select"), abl = el("input"), base = el("select"), go = el("button", "ghost", "开始评测（真实调用模型）");
  const count = (name) => cases.cases.filter((c) => name === "all" || (name === "holdout") === !!c.holdout).length;
  for (const [v, t] of [["dev", "调试集（不含留出题）"], ["holdout", "只跑留出题"], ["all", "全部（最终报告）"]]) { const o = el("option", "", `${t} · ${count(v)} 题`); o.value = v; set.append(o); }
  for (const n of [1, 2, 3, 4, 5]) { const o = el("option", "", `k = ${n}`); o.value = n; o.selected = n === 3; k.append(o); }
  abl.type = "checkbox"; abl.checked = true;
  base.append(new Option("不与基线对比", ""));
  for (const r of list.filter((r) => r.status === "done")) base.append(new Option(`基线 ${r.id} · pass@1 ${pctf(r.main.pass1)}`, r.id));
  const ablLabel = el("label", "meta"); ablLabel.append(abl, " 消融（带 ablation 的题再跑一组对照）");
  go.onclick = async () => {
    const chosen = cases.cases.filter((c) => set.value === "all" || (set.value === "holdout") === !!c.holdout);
    const trials = chosen.reduce((n, c) => n + Number(k.value) * (abl.checked && c.ablation ? 2 : 1), 0);
    if (!confirm(`将运行 ${chosen.length} 道题、共 ${trials} 个试次（3 个并行），每个试次真实调用方舟；开放题另由 DeepSeek 三票评审。开始？`)) return;
    go.disabled = true;
    try {
      const res = await fetch("/evals", { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ set: set.value, k: Number(k.value), ablation: abl.checked, baseline: base.value }) });
      if (!res.ok) throw new Error(await res.text());
      openEvals((await res.json()).id);
    } catch (err) { alert(err.message); go.disabled = false; }
  };
  form.append(set, k, ablLabel, base, go);
  panel.lastChild.replaceWith(form);
  // 评测集一览：题目、分组、打分器（结果 / 过程）。
  const box = el("details", "step"), sum = el("summary"), inner = el("div", "body");
  sum.append(el("span", "", `评测集 · ${cases.cases.length} 题 · ${cases.file}@${cases.hash}`));
  const table = el("table", "eval-table");
  table.innerHTML = "<tr><th>用例</th><th>分组</th><th>题目</th><th>打分器</th></tr>";
  for (const c of cases.cases) {
    const tr = el("tr"), id = el("td", "mono", c.id);
    id.append(...caseTags(c));
    tr.append(id, el("td", "", c.group), el("td", "", clip(c.question, 70)), el("td", "", c.graders.map((g) => g.type + (g.with_only ? "*" : "")).join(" · ")));
    table.append(tr);
  }
  const scroll = el("div", "eval-scroll"); scroll.append(table); inner.append(scroll); box.append(sum, inner);
  panel.append(box, el("h3", "", "评测记录"));
  if (!list.length) panel.append(el("div", "empty", "还没有评测。选好范围后点“开始评测”。"));
  for (const r of list) {
    const card = el("article", "hub-card"), head = el("div", "hub-head"), open = el("button", "ghost", "打开");
    open.onclick = () => openEvals(r.id);
    head.append(el("strong", "", r.id), el("span", "meta", (r.kind === "attribution" ? "对话归因 · " : "") + evalStatus(r.status, r.busy)), el("div", "push"));
    head.lastChild.append(open);
    if (r.kind === "attribution") {
      card.append(head, el("div", "hub-desc", (r.label ? `【${r.label}】` : "") + (r.result || ATTR_STAGE[r.stage] || "")), el("div", "cmd", `对话 ${r.run} · ${new Date(r.started).toLocaleString("zh-CN", { hour12: false })}`));
      panel.append(card);
      continue;
    }
    card.append(head, el("div", "hub-desc", `pass@1 ${pctf(r.main.pass1)} ± ${pctf(1.96 * r.main.se)} · pass^${r.k} ${pctf(r.main.passk)} · ${r.main.cases} 题 · 试次 ${r.progress.done}/${r.progress.total}`),
      el("div", "cmd", `set=${r.set} · commit ${r.commit} · ${r.model || "模型未知"}` + (r.baseline ? ` · 基线 ${r.baseline}` : "") + ` · ${new Date(r.started).toLocaleString("zh-CN", { hour12: false })}`));
    panel.append(card);
  }
}
const ATTR_STAGE = { diagnose: "诊断中：模型在读轨迹", grade: "评分中：按诊断写出的标准评判原对话", replay: "回放验证中：纠正 3 次 + 安慰剂 3 次", done: "完成", failed: "失败" };
const VERDICT = { failure: "失败", success: "达标", unclear: "不确定" };
// 对话归因的详情：诊断、评分标准、决定性的一步、回放结论。证据（回放）和卡片沿用评测页面的部分。
function attributionView(rec, id) {
  const a = rec.attribution, box = el("div"), card = el("article", "hub-card" + (a.stage === "failed" ? " broken" : "")), head = el("div", "hub-head");
  head.append(el("strong", "", "自动归因"), el("span", "meta", ATTR_STAGE[a.stage] || a.stage));
  const push = el("div", "push"), open = el("button", "ghost", "打开原对话"), again = el("button", "ghost", "重新归因");
  open.onclick = () => { ui.page = "observe"; show(a.run); };
  again.onclick = () => attribute(a.run);
  push.append(open); if (a.stage === "done" || a.stage === "failed") push.append(again);
  head.append(push);
  card.append(head, el("div", "cmd", `对话 ${a.run} · 最后一轮的检查点 ${a.task_id} · 诊断与评审：${rec.judge}`));
  if (a.error) card.append(el("div", "err", a.error));
  if (a.result) card.append(el("div", "signal", "结论：" + a.result));
  const row = (k, v) => { if (!v) return; const d = el("div", "check"); d.append(el("span", "kind", k), el("span", "", v)); card.append(d); };
  if (a.verdict) {
    row("诊断", VERDICT[a.verdict] || a.verdict);
    if (a.round) row("决定性的一步", `第 ${a.round} 次模型决定：${a.what}`);
    row("原因", a.why); row("类别", a.label); row("建议修在", a.fix);
    row("回放插入", a.note); row("正确结果", a.expected); row("评分标准", a.rubric);
  }
  if (a.trajectory) card.append(fold(a.full_trajectory ? "诊断读到的完整轨迹（由观测台记录重建，按实际请求编号）" : "诊断读到的轨迹（只有检查点里的 View：压缩过的部分只剩摘要）", a.trajectory));
  if (a.tools) card.append(fold("当时可用的工具", a.tools));
  card.append(el("div", "meta", "诊断是模型初判（自动定位出错步骤并不可靠）；证据是下面的回放：纠正能让结局翻转、安慰剂不能，才算已证实。卡片上的开放编码可以改写。"));
  box.append(card);
  return box;
}
function caseTags(c) {
  const tags = [];
  if (c.suite === "regression") tags.push(el("span", "tag-s", "回归"));
  if (c.holdout) tags.push(el("span", "tag-s hold", "留出"));
  if ((c.tags || []).includes("env:network") || c.network) tags.push(el("span", "tag-s net", "外网"));
  if (c.ablation) tags.push(el("span", "tag-s", "消融 " + c.ablation));
  return tags;
}
function evalStatus(status, busy) {
  if (busy) return { run: "运行中", replay: "回放中", regrade: "重新打分中" }[busy];
  return { running: "未完成（观测台重启过）", done: "已完成", interrupted: "已停止" }[status] || status;
}
function tile(value, label) { const t = el("div", "eval-tile"); t.append(el("b", "", value), el("span", "", label)); return t; }
function trialChip(t, onclick) {
  const state = t.status === "pending" ? "" : t.status === "running" ? "running" : t.status === "interrupted" ? "" : t.pass ? "pass" : "fail";
  const b = el("button", "tchip " + state + (ui.evalTrial === t.task_id ? " sel" : ""), t.status === "pending" ? "·" : t.status === "running" ? "…" : t.status === "interrupted" ? "‖" : t.pass ? "✓" : "✗");
  b.title = `${t.task_id}\n${t.status}${t.outcome ? " · " + t.outcome : ""}`;
  b.onclick = onclick;
  return b;
}
async function evalDetail(panel, id) {
  let data;
  try { const res = await fetch("/evals/" + encodeURIComponent(id)); if (!res.ok) throw new Error(await res.text()); data = await res.json(); }
  catch (err) { panel.lastChild.replaceWith(el("div", "err", "读取失败：" + err.message)); return; }
  if (!panel.isConnected || ui.evalOpen !== id) return;
  const { record: rec, summary: sum, cards, cases, busy } = data;
  const running = !!busy;
  const intro = panel.querySelector("p.meta");
  intro.textContent = `${evalStatus(rec.status, busy)} · commit ${rec.commit} · 题目 ${rec.cases_file}@${rec.cases_hash} · 模型 ${rec.model || "未知"} · 评审 ${rec.judge} · set=${rec.set} · k=${rec.k}` + (rec.args?.length ? ` · 参数 ${rec.args.join(" ")}` : "");
  const body = el("div");
  const bar = el("div", "hub-bar"), back = el("button", "link", "← 全部评测");
  back.onclick = () => openEvals("");
  bar.append(back);
  const action = (label, what, ask) => {
    const b = el("button", "ghost", label);
    b.onclick = async () => { if (ask && !confirm(ask)) return; b.disabled = true; try { await evalPost(`/evals/${id}/${what}`); } catch (err) { alert(err.message); } evalDetail(panel, id); };
    bar.append(b);
  };
  if (busy) action("停止", "stop", "停止后，在跑的试次写好检查点再退出；之后可以“继续”。");
  else {
    if (rec.kind !== "attribution" && rec.trials.some((t) => t.status === "interrupted" || t.status === "running")) action("继续未完成的试次", "resume");
    action("重新打分", "regrade", "用当前的打分器和评审模型重新打分（会请求 DeepSeek，不重跑 agent）。继续？");
  }
  body.append(bar);
  if (rec.kind === "attribution") {
    intro.textContent = `对话归因 · ${evalStatus(rec.status, busy)} · commit ${rec.commit}`;
    body.append(attributionView(rec, id));
  }
  if (rec.kind !== "attribution" && sum.progress.done < sum.progress.total) {
    const p = el("div", "eval-bar"), i = el("i"); i.style.width = (100 * sum.progress.done / sum.progress.total) + "%"; p.append(i);
    body.append(p, el("div", "meta", `试次 ${sum.progress.done}/${sum.progress.total}`));
  }
  // 主分：答对率与完成率分开；SE 按题聚类，95% 区间约 ±1.96 SE。
  const m = sum.main, tiles = el("div", "eval-tiles");
  tiles.append(tile(`${pctf(m.pass1)} ± ${pctf(1.96 * m.se)}`, `pass@1 · ${m.cases} 题（95% 区间）`), tile(pctf(m.passk), `pass^${m.k}：${m.k} 次全对的题`), tile(pctf(m.completion), "完成率（给出最终回答）"),
    tile(kfmt(Math.round(m.tokens_per_trial)), "每试次 tokens"), tile(m.tokens_per_pass ? kfmt(Math.round(m.tokens_per_pass)) : "—", "每答对一次 tokens"), tile(m.p50_seconds.toFixed(1) + " s", "延迟 P50"));
  const attr = rec.kind === "attribution";
  if (!attr) body.append(el("h3", "", "主分（不含依赖外网的题与消融对照组）"), tiles);
  const parts = attr ? [] : [...sum.suites, ...(sum.network ? [sum.network] : [])];
  if (parts.length) {
    const t = el("table", "eval-table");
    t.innerHTML = `<tr><th>范围</th><th>题</th><th>pass@1</th><th>pass^${rec.k}</th><th>完成率</th><th>每试次 tokens</th></tr>`;
    for (const p of parts) { const tr = el("tr"); tr.append(el("td", "", p.name), el("td", "num", p.cases), el("td", "num", `${pctf(p.pass1)} ± ${pctf(1.96 * p.se)}`), el("td", "num", pctf(p.passk)), el("td", "num", pctf(p.completion)), el("td", "num", Math.round(p.tokens_per_trial))); t.append(tr); }
    body.append(t);
  }
  if (sum.compare) {
    const c = sum.compare;
    body.append(el("h3", "", "与基线逐题对比"), el("div", "sub", `基线 ${c.baseline} · ${c.common} 道共同的题 · 配对平均差 ${(c.mean_diff * 100).toFixed(1)} 个百分点 ± ${(1.96 * c.se * 100).toFixed(1)}`));
    for (const [label, ids, cls] of [["回归（基线全对，这次没有全对）", c.regressed, "err"], ["修复（基线全错，这次全对）", c.fixed, "meta"], ["其他变化", c.changed, "meta"]])
      body.append(el("div", cls, `${label}：${ids.length ? ids.join("、") : "无"}`));
  }
  if (sum.ablation?.length) {
    body.append(el("h3", "", "消融：有 / 无这个能力的分差"), el("div", "sub", "不计只有实验组可能通过的检查（如“调用了 search_docs”“带引用”），否则分差虚高"));
    const t = el("table", "eval-table");
    t.innerHTML = "<tr><th>用例</th><th>参数</th><th>有</th><th>无</th><th>Δ</th></tr>";
    for (const d of sum.ablation) { const tr = el("tr"); tr.append(el("td", "mono", d.case), el("td", "mono", d.flag), el("td", "num", d.with.toFixed(2)), el("td", "num", d.without.toFixed(2)), el("td", "num " + (d.delta > 0 ? "delta-pos" : d.delta < 0 ? "delta-neg" : ""), (d.delta > 0 ? "+" : "") + d.delta.toFixed(2))); t.append(tr); }
    body.append(t);
  }
  // 逐题：每个方块是一次尝试（✓ 通过 ✗ 未通过 … 运行中 · 排队）；点开看答案、检查与工具序列。
  body.append(el("h3", "", attr ? "原对话按诊断写出的标准评判" : "逐题"), el("div", "sub", "每个方块是一次尝试，点开看答案、每项检查和工具序列；“轨迹”打开这次尝试的完整对话与调用链"));
  const table = el("table", "eval-table");
  table.innerHTML = "<tr><th>用例</th><th>分组</th><th>尝试</th><th>通过</th><th>题目</th></tr>";
  const byCase = new Map();
  for (const t of rec.trials) { if (!byCase.has(t.case)) byCase.set(t.case, []); byCase.get(t.case).push(t); }
  const all = [...rec.trials, ...(rec.replays || []).flatMap((r) => r.trials)];
  for (const [cid, trials] of byCase) {
    const info = cases[cid] || {}, tr = el("tr"), idCell = el("td", "mono", cid), chips = el("td");
    idCell.append(...caseTags({ ...info, network: false }));
    for (const t of trials) chips.append(trialChip(t, () => { ui.evalTrial = ui.evalTrial === t.task_id ? null : t.task_id; evalDetail(panel, id); }));
    const done = trials.filter((t) => t.arm === "with" && (t.status === "done" || t.status === "failed"));
    tr.append(idCell, el("td", "", info.group || ""), chips, el("td", "num", done.length ? `${done.filter((t) => t.pass).length}/${done.length}` : "—"), el("td", "", clip(info.question || "", 46)));
    table.append(tr);
    const open = trials.find((t) => t.task_id === ui.evalTrial);
    if (open) { const row = el("tr"), cell = el("td"); cell.colSpan = 5; cell.append(trialView(open, info)); row.append(cell); table.append(row); }
  }
  const scroll = el("div", "eval-scroll"); scroll.append(table); body.append(scroll);
  // D17：归因卡片 → 人工开放编码（只记第一个上游错误）→ 归类计数 → 回放验证。
  body.append(el("h3", "", `D17 归因 · ${cards.length} 个未通过的尝试`), el("div", "sub", "卡片只给线索：第一个异常信号与工具序列。读轨迹后写下第一个上游错误（开放编码），再归到一个类别（轴心编码）；对怀疑的那一步做回放验证，结局翻转才算确认。"));
  if (sum.labels?.length) body.append(el("div", "meta", "归类计数：" + sum.labels.map((l) => `${l.label} ${l.n}`).join(" · ")));
  const labels = [...new Set(Object.values(rec.notes || {}).map((n) => n.label).filter(Boolean))];
  const datalist = el("datalist"); datalist.id = "eval-labels";
  for (const l of labels) datalist.append(new Option(l));
  body.append(datalist);
  for (const c of cards) body.append(cardView(panel, id, c, cases[c.case] || {}));
  if (rec.replays?.length) {
    body.append(el("h3", "", "回放验证"));
    for (const rp of rec.replays) {
      const card = el("article", "hub-card"), head = el("div", "hub-head"), done = rp.trials.filter((t) => t.status === "done" || t.status === "failed");
      head.append(el("strong", "", rp.id), el("span", "meta", `${rp.case} · 来自 ${rp.from} · 保留前 ${rp.rounds} 轮 · ${rp.status === "running" ? "运行中" : `翻转 ${done.filter((t) => t.pass).length}/${rp.trials.length}`}`));
      const chips = el("div");
      for (const t of rp.trials) chips.append(trialChip(t, () => { ui.evalTrial = ui.evalTrial === t.task_id ? null : t.task_id; evalDetail(panel, id); }));
      card.append(head, el("div", "hub-desc", "插入的纠正：" + rp.note), chips);
      const open = rp.trials.find((t) => t.task_id === ui.evalTrial);
      if (open) card.append(trialView(open, cases[rp.case] || {}));
      body.append(card);
    }
  }
  body.append(el("div", "meta", `评审模型 ${rec.judge_usage.calls} 次请求 · ${kfmt(rec.judge_usage.input_tokens + rec.judge_usage.output_tokens)} tokens（不计入 agent 成本）`));
  // 运行中每 2 秒刷新；正在输入备注或纠正时不刷新，免得冲掉没保存的文字。
  const editing = panel.contains(document.activeElement) && /TEXTAREA|INPUT|SELECT/.test(document.activeElement.tagName);
  if (!editing || !panel.querySelector(".hub-bar")) {
    const keep = panel.querySelector("p.meta");
    panel.replaceChildren(panel.firstChild, keep, body);
  }
  if (running || rec.trials.some((t) => t.status === "running")) ui.evalTimer = setTimeout(() => { if (panel.isConnected && ui.evalOpen === id) evalDetail(panel, id); }, 2000);
}
function trialView(t, info) {
  const box = el("div", "eval-trial"), head = el("div", "hub-head");
  head.append(el("strong", "", `${t.case} #${t.n}` + (ARM[t.arm] ? " · " + ARM[t.arm] : "")), el("span", "meta", `${t.status}${t.outcome ? " · 结局 " + t.outcome : ""} · ${t.model_calls} 次模型请求 · ${kfmt(t.tokens)} tokens · ${t.seconds.toFixed(1)} s`));
  if (t.run) {
    const push = el("div", "push"), chat = el("button", "ghost", "对话"), trace = el("button", "ghost", "轨迹 / 调用链");
    chat.onclick = () => { ui.page = "chat"; show(t.run); };
    trace.onclick = () => { ui.page = "observe"; show(t.run); };
    push.append(chat, trace); head.append(push);
  }
  box.append(head, el("div", "cmd", t.task_id));
  if (info.question) box.append(el("div", "meta", "题目：" + info.question));
  if (info.reference) box.append(el("div", "meta", "参考答案：" + info.reference));
  const answer = el("div", "answer");
  if (t.answer) answer.append(md(t.answer)); else answer.append(el("span", "meta", t.detail || "没有最终回答"));
  box.append(answer);
  for (const c of t.checks || []) {
    const row = el("div", "check"), mk = el("span", "mk " + (c.pass ? "ok" : c.unknown ? "unk" : "bad"), c.pass ? "✓" : c.unknown ? "?" : "✗"), text = el("span");
    text.append(el("span", "", c.grader), el("span", "meta", " —— " + c.detail));
    row.append(mk, el("span", "kind", c.process ? "过程" : "结果"), text);
    box.append(row);
    if (c.votes?.length) box.append(fold("评审三票", c.votes.join("\n")));
  }
  const seq = el("ul", "toolseq");
  for (const u of t.tools || []) seq.append(el("li", u.status === "ok" ? "" : "bad", `第${u.round}轮 ${u.name}(${clip(u.args, 90)}) → ${u.status}${u.attempts > 1 ? `（尝试 ${u.attempts} 次）` : ""}${u.result ? "：" + clip(u.result, 120) : ""}`));
  if (!t.tools?.length) seq.append(el("li", "", "（没有调用工具）"));
  box.append(seq);
  return box;
}
function cardView(panel, id, c, info) {
  const card = el("article", "hub-card eval-card"), head = el("div", "hub-head");
  head.append(el("strong", "", `${c.case} #${c.n}`), el("span", "meta", `结局 ${c.outcome} · ${c.sibling}`));
  if (c.run) { const push = el("div", "push"), trace = el("button", "ghost", "读轨迹"); trace.onclick = () => { ui.page = "observe"; show(c.run); }; push.append(trace); head.append(push); }
  card.append(head, el("div", "meta", c.question), el("div", "signal", "第一个异常信号：" + c.signal));
  for (const f of c.failed || []) card.append(el("div", "meta", "✗ " + f));
  const seq = el("ul", "toolseq");
  for (const s of c.steps || []) seq.append(el("li", "", s));
  card.append(seq);
  // 开放编码 + 归类
  const note = el("div", "eval-note"), text = el("textarea"), label = el("input"), save = el("button", "ghost", "保存");
  text.placeholder = "开放编码：读轨迹，写下第一个上游错误（后面的错误多是它引起的）"; text.value = c.note?.text || "";
  label.placeholder = "归类，如：工具描述误导"; label.value = c.note?.label || ""; label.setAttribute("list", "eval-labels");
  save.onclick = async () => { save.disabled = true; try { await evalPost(`/evals/${id}/note`, { trial: c.task_id, text: text.value, label: label.value }); } catch (err) { alert(err.message); } evalDetail(panel, id); };
  const row = el("div", "row"); row.append(label, save, el("span", "meta", c.note ? "已保存 " + new Date(c.note.updated).toLocaleString("zh-CN", { hour12: false }) : ""));
  note.append(text, row);
  // 回放验证：保留前 N 轮（原样），插入一条纠正，再跑几次
  const rp = el("details", "step"), sumr = el("summary"), form = el("div", "eval-note"), rounds = el("select"), fix = el("textarea"), n = el("select"), go = el("button", "ghost", "回放验证");
  sumr.append(el("span", "", "回放验证：从某一步插入纠正，看结局会不会翻转"));
  for (let i = 0; i <= c.rounds; i++) rounds.append(new Option(`保留前 ${i} 轮，从第 ${i + 1} 轮开始改变`, i));
  // 默认保留到第一个异常信号所在的那一轮（含它的工具结果），从下一次模型决定开始改变。
  rounds.value = String(Math.min(c.rounds, c.round || c.rounds));
  for (const k of [1, 2, 3, 4, 5]) { const o = new Option(`跑 ${k} 次`, k); o.selected = k === 3; n.append(o); }
  fix.placeholder = "插入的纠正（以用户消息出现），例如：check_task_status 查不到 slow_job 的状态，不要再查，直接告诉我结果未知。";
  go.onclick = async () => {
    if (!fix.value.trim()) return alert("先写一条纠正");
    go.disabled = true;
    try { await evalPost(`/evals/${id}/replay`, { trial: c.task_id, rounds: Number(rounds.value), note: fix.value, n: Number(n.value) }); } catch (err) { alert(err.message); go.disabled = false; return; }
    evalDetail(panel, id);
  };
  const row2 = el("div", "row"); row2.append(rounds, n, go);
  form.append(fix, row2); rp.append(sumr, form);
  card.append(note, rp);
  return card;
}
