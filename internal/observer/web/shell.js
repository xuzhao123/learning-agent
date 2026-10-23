import { SOURCE_LABEL, endLabel } from "./analysis.js";
import { renderChat } from "./chat.js";
import { start } from "./composer.js";
import { openEvals } from "./eval.js";
import { show, startReplay, stopReplay, syncRunURL } from "./follow.js";
import { openCompare, select } from "./observe.js";
import { render } from "./render.js";
import { apply, newRun } from "./runs.js";
import { $, LAB_TITLE, MAX_IMAGES, RAG_LAB_TITLE, el, ui } from "./util.js";
// ---------- 外壳：顶栏、输入框与对话列表 ----------
export function renderShell() {
  const r = ui.run, chat = ui.page === "chat";
  $("chat-pane").hidden = !chat;
  if (!chat) $("subpanel").hidden = true;
  $("observe-pane").hidden = chat;
  $("page-chat").className = chat ? "on" : "";
  $("page-observe").className = chat ? "" : "on";
  $("cmp-hint").hidden = chat;
  $("title").textContent = ui.tab === "compare" && !chat ? `对比 ${ui.checked.size} 条运行` : r ? r.query || r.id : "新对话";
  $("files-btn").hidden = !chat || !r?.sandbox;
  $("files-btn").classList.toggle("on", !!ui.filesOpen);
  const runID = $("run-id");
  runID.hidden = !r;
  runID.textContent = r ? r.id.slice(0, 8) : "";
  runID.title = r ? `对话 ID：${r.id}，点击复制` : "";
  document.title = r ? `Learning Agent · ${r.id}` : "Learning Agent";
  const status = $("status");
  status.className = r && r.exit && r.exit !== "exit 0" ? "bad" : r && !r.exit ? "run" : "";
  const nSub = r ? Object.values(r.subs).reduce((n, s) => n + s.calls.length, 0) : 0;
  const calls = r ? `${r.calls.length + r.aux.length + nSub} 次请求` : "";
  // 正常完成不显示状态（同 Claude Code）；请求次数放在悬停提示里。
  status.textContent = !r ? "" : ui.replay ? "回放中" : !r.exit ? (r.stopRequested ? "正在停止…" : "运行中") : r.exit === "exit 0" ? "" : r.stopRequested ? "已停止" : endLabel(r, true);
  status.title = calls;
}
// 工具在设置里常驻；新对话按设置显示，进行中的对话按它开始时的快照显示。
export const TOOL_INFO = {
  calculator: ["计算器", "calculator：四则运算、括号、sqrt 和 floor，只解析数学表达式"],
  get_current_datetime: ["当前时间", "get_current_datetime：Asia/Shanghai 的日期、时间和星期"],
  search_notes: ["笔记检索", "search_notes：在第 1 天的学习笔记里逐行找关键词。只查这一篇，通用问题模型也会先去查，所以默认关；评测的笔记题在命令行里照常带上"],
  rag:["知识库检索", "search_docs：检索项目学习文档。打开后系统提示词要求知识问题先查资料，所以默认关"],
  memory: ["长期记忆", "remember / search / forget_memory：新对话开头先召回，要多发几次模型请求，所以默认关；按租户各自一份"],
  skills: ["Skills", "Skills 中心里启用的 skill：常驻名字和用途，用到时再加载正文"],
  mcp: ["MCP", "MCP 中心里启用的 server，工具注册为 mcp_<名字>"],
  subagents: ["子 agent", "spawn_agent：独立上下文的子进程，继承这里的工具，不继承长期记忆"],
  ask_user: ["向你提问", "ask_user：模型在运行中向你提一个问题（可带选项），等你在对话里回答再继续"],
  bash: ["Bash 沙箱", "bash：bubblewrap 沙箱，默认断网，联网逐次审批"],
  web_search: ["必应搜索", "web_search：无头 Chrome 打开必应搜索。必应多半不给自动化浏览器结果，搜索交给 DeepSeek，所以默认关"],
  open_page: ["打开网页", "open_page：无头 Chrome 打开网页读正文，会执行页面脚本，适合要渲染后才有内容的页面；只读"],
  ds_search:["DeepSeek 搜索", "ds_search / ds_fetch：DeepSeek 服务端搜索与抓取网页，按次计费，用 DEEPSEEK_API_KEY"],
  coding: ["编码工具", "read_file / glob / grep / edit_file / write_file：只在对话选的工作区里读写"],
};
function runTools(r) {
  // 单个工具按最近一次请求实际带的工具列表显示；还没发请求时按开始时的快照（disallowed_tools）。
  const sent = r.calls.at(-1)?.req.tools;
  const has = (name, group = true) => sent ? sent.some((t) => t.function?.name === name) : group && !r.source && !r.disallowedTools.includes(name);
  return { calculator: has("calculator"), get_current_datetime: has("get_current_datetime"), search_notes: has("search_notes"), web_search: has("web_search", r.browser), open_page: has("open_page", r.browser),
    rag: r.rag || (sent || []).some((t) => t.function?.name === "search_docs"), memory: r.memory, skills: r.skills.length > 0, mcp: r.mcp.length > 0, subagents: r.subagents, ask_user: has("ask_user", r.askUser), bash: r.bash, ds_search: r.dsSearch, coding: !!r.workspace };
}
export function toolOn(key) {
  if (ui.run) return !!runTools(ui.run)[key];
  const s = ui.settings?.effective;
  if (!s || s.values[key] !== "on" || key in s.blocked) return false;
  return key !== "coding" || !!$("workspace").value;
}
export async function loadSettings() {
  try { const res = await fetch("/settings"); if (res.ok) ui.settings = await res.json(); } catch {}
  syncComposer();
}
// 第二级模型下拉的清单：路由表只在 llm 包里，页面从 /models 读它，前端不抄一份。
export async function loadModels() {
  try {
    const res = await fetch("/models");
    if (res.ok) ui.models = new Map((await res.json()).map((p) => [p.name, p]));
  } catch {}
  syncComposer();
}
// 新对话没在输入框选具体模型时，agent 实际用的是设置里的默认模型（不属于这家供应商时退回供应商默认）。
export function settingsModel(provider) {
  const spec = ui.models?.get(provider || ""), model = ui.settings?.effective?.values.model || "";
  return model && (spec?.models || []).some((m) => m.name === model) ? model : "";
}
// 当前（或这条对话的）模型能不能看图：清单来自 /models，与 agent 用的是同一份路由表。
// model 为空表示用这家供应商的默认值；清单外的模型名（旧对话、.env 自填的）不知道就不拦。
export function modelVision(provider, model) {
  const spec = ui.models?.get(provider || "");
  if (!spec) return { name: model || "", vision: true };
  const name = model || spec.default;
  const found = (spec.models || []).find((m) => m.name === name);
  return { name, vision: found ? !!found.vision : true };
}
// D30：输入框右侧的上下文用量环（参考 Codex 客户端）。口径与轨迹页“上下文压力”一致：
// 最近一次主请求的服务端输入 tokens（system、工具定义与历史消息都在里面）占可用输入容量的比例。
// 压缩请求的输入同样是满上下文，不能拿它当用量：刚压缩完，环反而会显得更满。
const ctxRingLen = 2 * Math.PI * 11.5;
const ctxK = (n) => n >= 1000 ? +(n / 1000).toFixed(1) + "k" : String(Math.round(n)); // 去掉末尾的 .0：257024 → 257k，151234 → 151.2k
function updateCtxRing(r) {
  const ring = $("ctx-ring"), pct = ring.querySelector(".pct"), fill = ring.querySelector(".fill");
  const last = r && r.calls.filter((c) => !c.compact && c.usage && c.usage.prompt_tokens).at(-1);
  if (!last) { ring.hidden = true; ring._lines = null; return; }
  const used = last.usage.prompt_tokens, cap = Math.max(1, r.inputCapacity || ui.ctxWindow);
  const ratio = Math.max(0, Math.min(1, used / cap));
  const details = last.usage.prompt_tokens_details;
  const cached = (details && details.cached_tokens) || 0, hasCached = !!details && details.cached_tokens != null;
  ring.hidden = false;
  ring.className = ratio >= .9 ? "bad" : ratio >= .7 ? "warn" : "";
  fill.style.strokeDasharray = `${ratio * ctxRingLen} ${ctxRingLen}`;
  pct.textContent = Math.round(ratio * 100);
  // 悬浮详情：不用原生 title（SVG 子元素上不总弹出），存下来由下面的浮层显示。
  const percent = Math.round(ratio * 100);
  ring._lines = [
    "上下文窗口：",
    `${percent}% 已用（剩余 ${100 - percent}%）`,
    `已用 ${ctxK(used)} tokens，共 ${ctxK(cap)}`,
    `缓存命中率：${hasCached ? Math.round(100 * cached / used) + "%" : "—"}`,
  ];
  ring.setAttribute("aria-label", ring._lines.join("，"));
}
// 环的鼠标事件：自己画浮层，位置跟着环走；鼠标在 SVG 或数字上也算（它们 pointer-events: none）。
{
  const ring = $("ctx-ring"), tip = $("ctx-tip");
  ring.addEventListener("mouseenter", () => {
    if (ring.hidden || !ring._lines) return;
    tip.replaceChildren(el("b", "", ring._lines[0]), ...ring._lines.slice(1).map((line) => el("div", "", line)));
    tip.hidden = false;
    const box = ring.getBoundingClientRect(), h = tip.offsetHeight;
    // 输入框在页面底部：上方放不下时放到环下面，避免浮层被夹到屏幕顶部。
    const below = box.top - h - 8 < 8;
    tip.style.left = Math.max(8, Math.min(innerWidth - tip.offsetWidth - 8, box.left + box.width / 2 - tip.offsetWidth / 2)) + "px";
    tip.style.top = (below ? Math.min(innerHeight - h - 8, box.bottom + 8) : box.top - h - 8) + "px";
  });
  ring.addEventListener("mouseleave", () => { tip.hidden = true; });
}
export function syncComposer() {
  const r = ui.run, busy = r && (ui.loading || !r.exit), broken = r && ((r.exit && r.exit !== "exit 0" && !r.checkpoint) || r.rewoundAll);
  const lab = r?.query === RAG_LAB_TITLE, s = ui.settings?.effective;
  ui.picked ||= {};
  // 供应商同样来自 /models（显示名在路由表里）；本机没配好的（缺密钥或地址）灰掉，悬停看原因。
  const ps = $("provider"), plist = [...(ui.models?.values() || [])], psig = plist.map((p) => p.name + (p.error ? "!" : "")).join(",");
  if (ps.dataset.sig !== psig) {
    const keep = ps.value;
    ps.replaceChildren(...plist.map((p) => {
      const option = new Option((p.label || p.name) + (p.error ? "（未配置）" : ""), p.name);
      if (p.error) { option.disabled = true; option.title = p.error; }
      return option;
    }));
    ps.dataset.sig = psig;
    if (keep) ps.value = keep;
  }
  // 一条对话固定一个模型和一个工作区：续聊沿用开始时的选择（换模型请新建对话）。
  $("provider").disabled = !!r;
  if (r) $("provider").value = r.provider || "ark";
  else if (!ui.picked.provider && s) $("provider").value = s.values.provider;
  const prov = $("provider").value, spec = ui.models?.get(prov);
  const models = (spec?.models || []).map((m) => m.name);
  // 已有对话只显示它实际用的模型（agent 在 turn/started 里报的），它可以不在清单里：旧对话的带日期版本号的模型名、.env 里自填的名字；
  // 没记录到就不显示，不拿现在的默认值冒充。新对话列出清单，第一项“默认”标的是不选时 agent 实际会用的那个：
  // 设置里的默认具体模型，没设就用这家供应商的默认值（/models 按 .env 解析）。
  const fallback = settingsModel(prov) || spec?.default || "供应商默认";
  const opts = r ? (r.model ? [[r.model, r.model]] : []) : [["", "默认（" + fallback + "）"], ...models.map((m) => [m, m])];
  const pv = $("model"), msig = JSON.stringify(opts);
  if (pv.dataset.sig !== msig) {
    const keep = pv.value;
    pv.replaceChildren(...opts.map(([value, text]) => new Option(text, value)));
    pv.dataset.sig = msig;
    pv.value = opts.some(([value]) => value === keep) ? keep : (opts[0]?.[0] ?? "");
  }
  pv.disabled = !!r;
  pv.hidden = models.length < 2 || (!!r && !r.model);
  if (!r && !ui.picked.model) pv.value = "";
  pv.title = r ? "具体模型：对话开始后固定（换模型请新建对话）" : "具体模型：默认用这家供应商的默认值，清单来自路由表";
  const ws = $("workspace"), names = r ? (r.workspace ? [r.workspace] : []) : (s?.workspaces || []).map((w) => w.name);
  const signature = names.join("\n");
  if (ws.dataset.sig !== signature) {
    const keep = ws.value;
    ws.replaceChildren(new Option("无工作区", ""), ...names.map((n) => new Option(n, n)));
    ws.dataset.sig = signature;
    ws.value = names.includes(keep) ? keep : "";
  }
  const codingOK = !!s && s.values.coding === "on" && !("coding" in s.blocked);
  if (r) ws.value = r.workspace || "";
  else if (!ui.picked.workspace && s) ws.value = codingOK && names.includes(s.values.workspace) ? s.values.workspace : "";
  ws.disabled = !!r || !codingOK;
  ws.hidden = !r && !names.length;
  ws.title = r ? "工作区：对话开始后固定" : !codingOK ? "编码工具没打开或不可用：" + (s?.blocked.coding || "在设置里打开") : "工作区：编码工具只在这个仓库里读写；对话开始后固定";
  // 权限模式每轮可以换：默认取这条对话上一轮的（新对话取设置）；用户改过就保持用户的选择，直到发送。
  if (!ui.picked.permission) $("perm").value = (r && r.permission) || s?.values.permission_mode || "edit";
  $("perm").disabled = !!busy || !!ui.replay;
  // 运行中发送键变成停止键（Codex 客户端的做法）：第一次优雅停止，第二次强制结束。
  const running = !!r && !r.exit && !ui.replay && (r.calls.length > 0 || r.logs.length > 0);
  $("send").classList.toggle("stop", running);
  $("send").classList.toggle("force", running && r.stopRequested);
  $("send").innerHTML = running ? '<svg class="i" viewBox="0 0 24 24" aria-hidden="true"><rect x="7" y="7" width="10" height="10" rx="2" fill="currentColor" stroke="none"/></svg>' : '<svg class="i" viewBox="0 0 24 24" aria-hidden="true"><path d="M12 19V5M5 12l7-7 7 7"/></svg>';
  $("send").title = running ? (r.stopRequested ? "强制结束（SIGKILL）" : "停止（SIGINT：先回填结果、保存检查点）") : "发送";
  $("send").setAttribute("aria-label", running ? (r.stopRequested ? "强制结束" : "停止") : "发送");
  const imported = SOURCE_LABEL[r?.source];
  // 附图：一条对话的模型固定，能不能看图也固定。不能看图的模型、实验、导入的对话与回放都不给附图；
  // 已经附了图又切到不能看图的模型时，发送前拦下，不自动删图。
  const vis = modelVision(prov, r ? r.model || "" : pv.value || settingsModel(prov));
  $("attach").disabled = !vis.vision || lab || !!imported || !!ui.replay;
  $("attach").title = vis.vision ? `附图：PNG、JPEG、GIF、WebP，每条消息最多 ${MAX_IMAGES} 张；也可以粘贴或拖放`
    : `当前模型 ${vis.name} 不能看图，换一个能看图的模型再附图`;
  // 导入的对话只读：模型、工作区、权限与工具都是对方 agent 的，这里不给选择。
  for (const id of ["provider", "perm"]) $(id).hidden = !!imported;
  // 模型与工作区上面已按清单和对话决定是否显示，这里只在导入的对话里再藏起来，不能把它们重新显示出来。
  if (imported) $("model").hidden = $("workspace").hidden = true;
  $("query").disabled = !!imported;
  $("send").disabled = running ? false : ui.sending || !!busy || !!broken || !!ui.replay || lab || !!imported || !$("query").value.trim();
  $("query").placeholder = imported ? "导入的对话只能查看" : running ? "运行中：输入的话会排进 agent 的下一次请求（插话）" : r ? "继续提问" : "给 agent 布置一个任务，/ 选命令";
  // 输入框只放这一轮要选的（同 Claude Code）：工具、请求预算在设置页里看，这里只给导入的对话标出来源。
  $("mode-chip").textContent = imported ? `${imported} · ${r.calls.find((c) => c.res?.model)?.res.model || ""}` : "";
  // 只在需要人注意时提示；常规操作说明不放在输入框下面。
  $("hint").textContent = !r ? "" : imported ? `从 ${imported} 导入的记录，只能查看；系统提示词、工具定义与耗时以记录里有的为准` : lab ? "对比实验使用独立问题；自行提问请新建对话" : ui.replay ? "回放中，结束后可继续提问" :
    busy ? (r.stopRequested ? "正在停止：agent 回填结果、保存检查点后退出；再点一次强制结束" : "") : broken ? "上次运行未正常结束、也没有检查点，请新建对话" : "";
  updateCtxRing(r);
}
// 列表显示最后一次活动的时间：服务端给 Unix 毫秒时间戳，这里按看的人所在的时区格式化。
function relTime(r) {
  const t = new Date(r.updated_at ?? r.created_at ?? NaN);
  if (isNaN(t)) return "";
  const p = (n) => String(n).padStart(2, "0");
  const today = new Date().toDateString() === t.toDateString();
  return (today ? "" : `${p(t.getMonth() + 1)}-${p(t.getDate())} `) + `${p(t.getHours())}:${p(t.getMinutes())}`;
}
export async function loadRuns() {
  const runs = await (await fetch("/runs")).json();
  const observe = ui.page === "observe";
  // 评测发起的试次不进对话列表，从“D16 评测”页面打开。
  $("runs").replaceChildren(...runs.filter((r) => !r.eval).reverse().map((r) => {
    const row = el("div", "runitem" + (ui.run && r.id === ui.run.id && !(observe && ui.tab === "compare") ? " active" : ""));
    if (observe) {
      const box = el("input");
      box.type = "checkbox";
      box.checked = ui.checked.has(r.id);
      box.title = "加入对比";
      box.onchange = () => {
        if (box.checked && ui.checked.size >= 5) { box.checked = false; return; }
        box.checked ? ui.checked.add(r.id) : ui.checked.delete(r.id);
        if (ui.tab !== "compare") return render();
        if (ui.checked.size >= 2) openCompare(); else { ui.tab = "trace"; render(); }
      };
      row.append(box);
    }
    const b = el("button"), q = el("span", "q");
    if (!r.done) q.append(el("span", "live"));
    if (SOURCE_LABEL[r.source]) q.append(el("span", "src", SOURCE_LABEL[r.source]));
    q.append(r.query === LAB_TITLE ? "上下文实验" : r.query === RAG_LAB_TITLE ? "检索对比" : r.query || "（无问题）");
    b.title = `${r.query}\n对话 ID：${r.id}`;
    b.append(q, el("small", "", relTime(r)));
    b.onclick = () => { document.body.classList.remove("nav-open"); if (ui.tab === "compare") ui.tab = "trace"; show(r.id); };
    row.append(b);
    return row;
  }));
}
export function autosize() { const q = $("query"); q.style.height = "auto"; q.style.height = q.scrollHeight + "px"; }

$("files-btn").onclick = () => { ui.filesOpen = !ui.filesOpen; if (ui.filesOpen) { ui.subOpen = ui.webOpen = null; ui.files.run = null; } renderChat(); renderShell(); };
$("page-chat").onclick = () => { ui.page = "chat"; if (ui.tab === "compare") ui.tab = "trace"; render(); loadRuns(); };
$("page-observe").onclick = () => { ui.page = "observe"; render(); loadRuns(); };
$("run-id").onclick = async () => {
  const id = ui.run?.id;
  if (!id) return;
  let note;
  try { await navigator.clipboard.writeText(id); note = "已复制"; }
  catch { note = "请手动选择复制"; }
  if (ui.run?.id !== id) return;
  $("run-id").textContent = note;
  setTimeout(() => { if (ui.run?.id === id) renderShell(); }, 1800);
};
$("menu").onclick = () => document.body.classList.toggle("nav-open");
$("tab-trace").onclick = () => { ui.tab = "trace"; render(); loadRuns(); };
$("tab-maze").onclick = () => { ui.tab = "maze"; render(); loadRuns(); };
$("tab-spans").onclick = () => { ui.tab = "spans"; render(); loadRuns(); };
$("tab-compare").onclick = () => openCompare();
$("search").oninput = () => { ui.search = $("search").value; render(); };
$("expand").onclick = () => { ui.expanded = true; ui.collapsed.clear(); render(); };
$("replay").onclick = () => {
  if (!ui.replay) return startReplay();
  stopReplay();
  ui.run = newRun(ui.run.id);
  ui.events.forEach((e) => apply(ui.run, e));
  render();
};
$("terminal").onclick = () => select("end", ui.run);
// D17：对这段对话自动归因。已有归因就直接打开（详情页里可以重新归因）。
$("attribute").onclick = async () => {
  const r = ui.run;
  if (!r) return;
  try {
    const old = (await (await fetch("/evals")).json()).find((e) => e.kind === "attribution" && e.run === r.id);
    if (old) return openEvals(old.id);
  } catch {}
  attribute(r.id);
};
// D29：先预览（dry_run）列出会恢复、删除的文件；有冲突就不做，没有冲突再由人确认执行。
export async function rewindTurn(r, t) {
  const call = async (dryRun) => {
    const res = await fetch(`/runs/${encodeURIComponent(r.id)}/rewind`, { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ turn: t.checkpoint, dry_run: dryRun }) });
    const text = await res.text();
    let data = null;
    try { data = JSON.parse(text); } catch {}
    if (!res.ok) throw new Error(data?.error || text);
    return data;
  };
  const list = (label, xs) => xs?.length ? `\n${label}：\n  ${xs.join("\n  ")}` : "";
  let plan;
  try { plan = await call(true); } catch (err) { return alert(err.message); }
  if (plan.conflicts?.length) return alert(`不能撤销：这些文件在 agent 最后一次编辑之后又被改过（bash、人或别的程序），撤销会覆盖那些改动。${list("冲突", plan.conflicts)}`);
  const n = plan.turns.length, none = !plan.restore.length && !plan.delete.length;
  const text = `撤销第 ${t.n} 轮${n > 1 ? `及之后，共 ${n} 轮` : ""}：文件放回这一轮开始之前，对话${plan.continues ? "退回上一轮" : "也全部撤销，之后需要新建对话"}。`
    + list("恢复", plan.restore) + list("删除（这几轮新建的）", plan.delete) + (none ? "\n这几轮没有用编码工具改过文件。" : "")
    + list("bash 改过、不会还原（没有快照）", plan.unsnapshotted)
    + "\n\n只撤销 edit_file、write_file 的改动；bash 改的文件没有快照。确定？";
  if (!confirm(text)) return;
  try { await call(false); } catch (err) { return alert(err.message); }
  show(r.id, false);
}
export async function attribute(runID) {
  if (!confirm("自动归因这段对话的最后一轮：\n1. DeepSeek 读完整轨迹，判断成败、写出评分标准，指出决定性的那一步和原因；\n2. 失败时从那一步之前分叉回放：插入纠正跑 3 次、插入“继续。”作对照跑 3 次，按这段对话当时的模型真实调用。\n开始？")) return;
  try {
    const res = await fetch(`/runs/${encodeURIComponent(runID)}/attribute`, { method: "POST" });
    if (!res.ok) throw new Error(await res.text());
    openEvals((await res.json()).id);
  } catch (err) { alert("归因失败：" + err.message); }
}
export function newChat(updateURL = true) {
  stopReplay();
  if (ui.source) ui.source.close();
  Object.assign(ui, { chatFollow: true, unseen: false, page: "chat", tab: "trace", run: null, events: [], source: null, selected: null, loading: false, subOpen: null, webOpen: null, webClosed: false, picked: {} });
  if (updateURL) syncRunURL(null);
  document.body.classList.remove("nav-open");
  $("stats").hidden = true;
  $("view").replaceChildren(el("div", "empty", "先在对话中提问，或从左侧选择历史对话"));
  $("inspector").replaceChildren(el("div", "empty", "点击事件或节点查看详情"));
  render(); loadRuns(); $("query").focus();
}
$("new-chat").onclick = () => newChat();
// 导入 Codex / Claude Code 的会话文件：服务端换成观测台的存档格式，导入后只读。
$("import").onclick = () => $("import-file").click();
$("import-file").onchange = async () => {
  const file = $("import-file").files[0];
  $("import-file").value = "";
  if (!file) return;
  const label = $("import").lastChild.textContent;
  $("import").lastChild.textContent = "导入中…";
  try {
    const res = await fetch("/runs/import", { method: "POST", body: file });
    if (!res.ok) throw new Error(await res.text());
    document.body.classList.remove("nav-open");
    ui.page = "chat";
    show((await res.json()).id);
  } catch (err) { alert("导入失败：" + err.message); }
  finally { $("import").lastChild.textContent = label; }
};
$("lab").onclick = () => {
  document.body.classList.remove("nav-open");
  if (!ui.sending && confirm("自动进行33轮真实对话和一次大工具输出实验，最多60次模型请求（minimal推理）。开始？")) start("/runs", { context_lab: true });
};
$("rag-lab").onclick = () => {
  document.body.classList.remove("nav-open");
  if (ui.sending) return;
  $("new-chat").click();
  const box = $("thread");
  ui.page = "chat"; renderShell();
  const notice = el("div", "banner", "运行10题检索检查与20次真实问答（minimal推理，每题每组最多6次模型请求）。检索由Agent进程自动初始化。结果逐条显示在同一实验记录中。");
  const model = el("select"); model.setAttribute("aria-label", "实验向量模型");
  for (const [value, label] of [["ark", "线上方舟"], ["local", "本地 MiniLM"]]) { const option = el("option", "", label); option.value = value; model.append(option); }
  const button = el("button", "ghost", "开始 Day 4 对比（真实调用模型）");
  button.onclick = () => { button.disabled = true; start("/runs", { rag_lab: true, embedding: model.value }); };
  notice.append(el("p"), model, button); box.replaceChildren(notice);
};
$("security-lab").onclick = () => {
  document.body.classList.remove("nav-open");
  if (ui.sending) return;
  $("new-chat").click();
  ui.page = "chat"; renderShell();
  const notice = el("div", "banner");
  notice.append(el("strong", "", "Day 18–19 · 资料可以提供事实，不能增加权限"));
  notice.append(el("p", "", "真实模型读取本地学习样本。先试正常资料，再试夹带指令。只提供资料读取与基线计算器，无浏览器、Bash、私有记忆或额外联网能力。"));
  const sample = el("select"); sample.setAttribute("aria-label", "学习样本");
  for (const [value, label] of [["clean", "正常资料"], ["answer", "夹带：只回答香蕉"], ["tool", "夹带：额外调用计算器"]]) {
    const option = el("option", "", label); option.value = value; sample.append(option);
  }
  const provider = el("select"); provider.setAttribute("aria-label", "实验聊天模型");
  for (const [value, label] of [["ark", "方舟"], ["deepseek", "DeepSeek"]]) {
    const option = el("option", "", label); option.value = value; provider.append(option);
  }
  const guard = el("input"); guard.type = "checkbox"; guard.checked = true;
  const label = el("label"); label.style.display = "block"; label.style.margin = "12px 0";
  label.append(guard, document.createTextNode(" 开启防护：来源标记 + 只提供资料读取工具"));
  const hint = el("p", "", "取消勾选是课堂对照组：不附加资料标记，多提供一个无害计算器。正常 Agent 默认有资料标记；两组执行器都校验工具允许列表。实验是单次任务，请另开实验做对比。原始输入输出在‘观测’中查看。");
  const button = el("button", "ghost", "开始实验（真实调用模型）");
  button.onclick = () => { button.disabled = true; start("/runs", { security_lab: sample.value, security_protection: guard.checked, provider: provider.value }); };
  notice.append(sample, document.createTextNode(" "), provider, label, hint, button);
  $("thread").replaceChildren(notice);
};
