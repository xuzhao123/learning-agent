import { show } from "./follow.js";
import { md } from "./md.js";
import { TOOL_INFO, loadSettings, newChat, syncComposer } from "./shell.js";
import { $, MEM_KIND, MEM_SOURCE, MEM_STATUS, el, fold, ui } from "./util.js";
$("memory").onclick = () => openMemory();
// D21：审计日志只读。服务端每次从头校验哈希链；页面只展示，不提供修改或删除。
const AUDIT_ACTOR = { user: "人", model: "模型", program: "程序" };
$("audit").onclick = () => openAudit();
async function openAudit(task = "") {
  document.body.classList.remove("nav-open");
  if (ui.sending) return;
  $("new-chat").click();
  const panel = el("div", "mempanel evalpanel");
  $("thread").replaceChildren(panel);
  panel.append(el("div", "meta", "读取中…"));
  let data;
  try { const res = await fetch("/audit?task=" + encodeURIComponent(task)); if (!res.ok) throw new Error(await res.text()); data = await res.json(); }
  catch (err) { panel.replaceChildren(el("div", "err", "读取失败：" + err.message)); return; }
  const records = data.records || [];
  const form = el("div", "eval-form"), input = el("input");
  input.placeholder = "按任务ID过滤（含子任务）"; input.value = task; input.style.minWidth = "320px";
  const go = el("button", "", "过滤"); go.type = "button"; go.onclick = () => openAudit(input.value.trim());
  input.onkeydown = (e) => { if (e.key === "Enter") go.click(); };
  form.append(input, go);
  if (task) { const all = el("button", "", "全部"); all.type = "button"; all.onclick = () => openAudit(); form.append(all); }
  const status = data.broken_seq ? el("div", "err", `哈希链校验失败：第 ${data.broken_seq} 条，${data.reason}`) : el("div", "meta", `哈希链完整：共 ${data.total || 0} 条记录`);
  panel.replaceChildren(el("h2", "", "审计日志"), el("p", "meta", ".data/audit/audit.jsonl · 只追加，每条带上一条的哈希。agent 记录程序判断、执行结局与记忆变更，观测台记录你的点击；参数只存哈希，原文在检查点与对话存档里。命令行校验：go run . audit -verify"), status, form);
  if (!records.length) { panel.append(el("div", "empty", "还没有审计记录。")); return; }
  const table = el("table", "eval-table"), head = el("tr");
  for (const h of ["#", "时间", "谁", "事件", "任务", "工具 / 对象", "风险", "决定 / 结局", "说明"]) head.append(el("th", "", h));
  table.append(head);
  for (const r of [...records].reverse()) {
    const tr = el("tr");
    const taskCell = el("td", "mono");
    if (r.task) { const a = el("a", "", r.task.length > 22 ? r.task.slice(0, 8) + "…" + r.task.slice(-10) : r.task); a.href = "#"; a.title = r.task; a.onclick = (e) => { e.preventDefault(); openAudit(r.task); }; taskCell.append(a); }
    if (r.conversation) { const a = el("a", "", " 对话"); a.href = "#"; a.onclick = (e) => { e.preventDefault(); show(r.conversation); }; taskCell.append(a); }
    tr.append(el("td", "num", String(r.seq)), el("td", "num", new Date(r.time).toLocaleString("zh-CN", { hour12: false })), el("td", "num", AUDIT_ACTOR[r.actor] || r.actor), el("td", "mono", r.event), taskCell,
      el("td", "mono", [r.tool, r.target].filter(Boolean).join(" · ")), el("td", "", r.risk || ""), el("td", "", [r.decision, r.status].filter(Boolean).join(" / ")),
      el("td", "", [r.detail, r.args_sha256 ? `参数 sha256 ${r.args_sha256.slice(0, 12)}…（${r.args_bytes} 字节）` : ""].filter(Boolean).join("；")));
    table.append(tr);
  }
  const wrap = el("div", "eval-scroll"); wrap.append(table); panel.append(wrap);
}
// 记忆面板只读 .data/memory.json；删除由 agent 的 -memory-forget 执行，与 agent 共用文件锁。
export async function openMemory(focus) {
  document.body.classList.remove("nav-open");
  if (ui.sending) return;
  $("new-chat").click();
  const panel = el("div", "mempanel"), when = (t) => new Date(t).toLocaleString("zh-CN", { hour12: false });
  $("thread").replaceChildren(panel);
  panel.append(el("div", "meta", "读取中…"));
  let data;
  try { const res = await fetch("/memory"); if (!res.ok) throw new Error(await res.text()); data = await res.json(); }
  catch (err) { panel.replaceChildren(el("div", "err", "读取失败：" + err.message)); return; }
  const list = data.file.memories || [], now = Date.parse(data.now);
  const chunks = Object.values(data.index?.chunks || {});
  panel.replaceChildren(el("h2", "", "长期记忆"), el("p", "meta", `${data.path} · 共 ${list.length} 条 · 在设置里打开“长期记忆”后，新对话开头经两路召回、融合和重排，最多注入 5 条；过期条目在 agent 下次读写时删除。原文与来源是依据，背景说明是模型生成的派生数据，可重建`));
  if (!list.length) panel.append(el("div", "empty", "还没有记忆。在设置里打开“长期记忆”，再新建对话让 agent 记住一件事，例如“别忘了我对花生过敏”。"));
  for (const m of [...list].reverse()) {
    const expired = m.expire_at && Date.parse(m.expire_at) <= now;
    const card = el("article", "mem-item" + (expired ? " expired" : "") + (m.id === focus ? " focus" : "")), head = el("div", "mem-head");
    const del = el("button", "ghost", "删除");
    del.onclick = async () => {
      if (!confirm(`删除 ${m.id}？会写回 .data/memory.json，无法撤销。`)) return;
      del.disabled = true;
      const res = await fetch(`/memory/${m.id}/forget`, { method: "POST" });
      if (!res.ok) alert(await res.text());
      openMemory();
    };
    head.append(el("strong", "", m.id), el("span", "mem-kind", MEM_KIND[m.kind] || m.kind), el("span", "meta", `${MEM_SOURCE[m.source] || m.source} · 重要性 ${m.importance}`), del);
    const derived = chunks.filter((c) => c.memory_id === m.id).sort((a, b) => a.plan.localeCompare(b.plan) || a.chunk - b.chunk);
    const details = [];
    for (const c of derived) details.push(el("div", "meta", `[${c.plan} · 片段 ${c.chunk}/${c.chunks} · ${MEM_STATUS[c.status] || c.status}] ` + (c.context ? "背景：" + c.context : c.reason || "") + (c.chunks > 1 ? " ｜ 片段：" + c.text : "")));
    if (!derived.length) details.push(el("div", "meta", "还没有索引：下次检索时建立"));
    details.push(m.source_context ? fold(`来源上下文 · ${when(m.source_context.time)}` + (m.source_context.truncated ? " · 已省略更早部分" : ""), m.source_context.text) : el("div", "meta", "来源：无（升级前写入的旧记忆，不猜测来源，按原文检索）"));
    card.append(head, el("div", "mem-text", m.content), ...details, el("div", "meta", `创建 ${when(m.created_at)} · 最近访问 ${when(m.last_access)} · ` + (m.expire_at ? (expired ? "已过期，下次读写时删除" : "过期于 " + when(m.expire_at)) : "不过期")));
    panel.append(card);
  }
}
$("skills-hub").onclick = () => openSkills();
$("mcp-hub").onclick = () => openMCP();
// 设置页：工具常驻，按 内置默认 → 平台 → 个人 合并；租户策略和本机条件只会关掉工具。改动只影响之后的新对话。
const SET_SOURCE = { builtin: "内置默认", platform: "平台", user: "你" };
export async function openSettings() {
  const panel = await openHubPage("设置", "打开的工具在新对话里都会带上，模型自己决定用不用；每个对话只选模型和工作区。改动只影响之后的新对话，进行中的对话沿用开始时的快照。保存在 .data/settings.json。");
  if (!panel) return;
  let data;
  try { data = await (await hubCall("GET", "/settings")).json(); }
  catch (err) { panel.lastChild.replaceWith(el("div", "err", "读取失败：" + err.message)); return; }
  if (!panel.isConnected) return;
  ui.settings = data; syncComposer();
  const e = data.effective, values = { ...e.values };
  panel.lastChild.replaceWith(el("p", "meta", "生效顺序：内置默认 › 平台 › 你。" + (ui.me && !ui.me.platform ? `租户 ${ui.me.tenant} 没开通的功能会被关掉。` : "")));
  const tag = (key) => el("span", "set-tag" + (e.source[key] === "user" ? " user" : ""), SET_SOURCE[e.source[key]] || e.source[key]);
  const select = (key, options) => {
    const node = el("select");
    for (const [v, label] of options) node.append(new Option(label, v));
    node.value = values[key]; node.onchange = () => { values[key] = node.value; };
    return node;
  };
  panel.append(el("h3", "", "工具"));
  const tools = el("div", "set-box");
  for (const key of data.tool_order) {
    const [name, desc] = TOOL_INFO[key], row = el("label", "set-row"), text = el("span"), box = el("input"), blocked = e.blocked[key];
    box.type = "checkbox"; box.checked = values[key] === "on"; box.onchange = () => { values[key] = box.checked ? "on" : "off"; };
    text.append(el("strong", "", name), el("div", "meta", desc));
    if (key === "rag" || key === "memory") {
      const sub = el("div", "sub");
      if (key === "memory") sub.append(select("memory_ttl", [["", "记住：永久"], ["1h", "记住：1小时后过期"], ["24h", "记住：1天后过期"]]));
      sub.append(select("embedding", [["ark", "向量：线上方舟"], ["local", "向量：本地 MiniLM"]]), el("span", "meta", "检索和记忆共用"));
      text.append(sub);
    }
    if (key === "skills") text.append(el("div", "meta", `已启用 ${e.skills.length} 个`));
    if (key === "mcp") text.append(el("div", "meta", `已启用 ${e.mcp.length} 个 server`));
    row.append(text, el("span", "set-tag " + (blocked ? "bad" : "ok"), blocked ? "不可用" : "可用"), tag(key), box);
    tools.append(row);
    if (blocked) text.append(el("div", "err", blocked));
  }
  panel.append(tools);
  panel.append(el("h3", "", "工作区"), el("p", "meta", "对话里只能选这里登记的目录（本机上的 git 仓库）。编码工具只在所选目录里读写，bash 在沙箱里把它挂到 /workspace。只有平台账号能登记。"));
  const wsBox = el("div", "set-box");
  if (!e.workspaces.length) wsBox.append(el("div", "set-row", e.blocked.coding ? "编码工具不可用：" + e.blocked.coding : "还没有登记工作区"));
  for (const w of e.workspaces) {
    const row = el("div", "set-row"), text = el("span");
    text.append(el("strong", "", w.name), el("div", "meta", w.path));
    row.append(text, el("span"), el("span"));
    if (data.can_edit_platform) {
      const del = el("button", "ghost", "移除");
      del.onclick = async () => { if (!confirm(`移除工作区 ${w.name}？目录本身不会删除。`)) return; try { await hubCall("DELETE", `/settings/workspaces/${encodeURIComponent(w.name)}`); openSettings(); } catch (err) { alert(err.message); } };
      row.append(del);
    } else row.append(el("span"));
    wsBox.append(row);
  }
  if (data.can_edit_platform) {
    const form = el("form", "set-row"), path = el("input"), add = el("button", "ghost", "添加");
    path.placeholder = "仓库目录路径：绝对路径，或相对 agent 目录（如 . 表示本仓库）"; path.style.width = "100%";
    form.append(path, el("span"), el("span"), add);
    form.onsubmit = async (ev) => { ev.preventDefault(); add.disabled = true; try { await hubCall("POST", "/settings/workspaces", { path: path.value }); openSettings(); } catch (err) { alert(err.message); add.disabled = false; } };
    wsBox.append(form);
  }
  panel.append(wsBox);
  panel.append(el("h3", "", "默认值"));
  const grid = el("div", "set-box set-grid"), num = (key, label, hint) => {
    const input = el("input"); input.type = "number"; input.value = values[key]; input.oninput = () => { values[key] = input.value; };
    const wrap = el("span"); wrap.append(input, document.createTextNode(" "), el("span", "meta", hint), document.createTextNode(" "), tag(key));
    grid.append(el("span", "meta", label), wrap);
  };
  const line = (label, node, key) => { const wrap = el("span"); wrap.append(node, document.createTextNode(" "), tag(key)); grid.append(el("span", "meta", label), wrap); };
  // 选项与输入框同一份（/models）；没配好的供应商标出来，开对话时 agent 会报出缺什么。
  const providers = [...(ui.models?.values() || [])].map((p) => [p.name, (p.label || p.name) + (p.error ? "（未配置）" : "")]);
  line("默认模型", select("provider", providers.length ? providers : [[values.provider, values.provider]]), "provider");
  line("默认工作区", select("workspace", [["", "无工作区"], ...e.workspaces.map((w) => [w.name, w.name])]), "workspace");
  num("max_steps", "模型请求预算", "每轮最多多少次模型请求；0 表示不限，靠重复动作与无进展熔断、停止按钮收住");
  num("context_window", "上下文窗口", "token，工具全开时每次请求要带全部工具定义，窗口小会很快压缩");
  num("tool_output", "工具输出上限", "单个工具结果写进上下文的 token 上限");
  line("默认权限模式", select("permission_mode", [["plan", "只读计划：不能改文件，bash 里工作区只读"], ["ask", "改动都问：改文件、跑 bash、调 MCP 前确认"], ["edit", "自动编辑：工作区内编辑与沙箱 bash 直接做"], ["full", "全权：不再询问（含高风险确认），deny、ask 规则照常"]]), "permission_mode");
  const rules = el("textarea"); rules.rows = 4; rules.style.width = "100%"; rules.value = values.permission_rules;
  rules.placeholder = "一行一条，顺序 deny › ask › allow：\nallow: Bash(go build *)\nask: Edit(internal/agent/**)\ndeny: Bash(git push *)";
  rules.oninput = () => { values.permission_rules = rules.value; };
  const rulesWrap = el("span"); rulesWrap.append(rules, tag("permission_rules"));
  grid.append(el("span", "meta", "权限规则"), rulesWrap);
  panel.append(grid, el("p", "meta", "权限模式在每条对话的输入框旁边还能换（每轮生效）；规则在对话开始时拍进快照。D20 的高风险确认（删除记忆、破坏性 MCP、含私有数据后的外发、续跑重做）在全权以外的模式下都询问；全权模式同 Claude Code、Codex，不再询问，由程序放行并写审计（续跑重做不自动做，回填结果未知）。"));
  // D26：hooks 在本机以观测台的身份运行命令，不在沙箱里，所以只有平台账号能改；个人设置里不生效。
  panel.append(el("h3", "", "Hooks"), el("p", "meta", "在固定时刻运行本机命令：用户提交（UserPromptSubmit）、工具执行前（PreToolUse）、工具成功后（PostToolUse）、模型准备结束（Stop）。格式同 Claude Code 的 hooks 段；填配置原文，或相对 agent 目录的文件路径（如 hooks/hooks.json）。对话开始时读成原文拍进快照。命令不在沙箱里运行，只有平台账号能设置。"));
  const hooksBox = el("textarea"); hooksBox.rows = 3; hooksBox.style.width = "100%"; hooksBox.value = values.hooks;
  hooksBox.placeholder = "hooks/hooks.json\n或 {\"hooks\": {\"PostToolUse\": [{\"matcher\": \"edit_file|write_file\", \"hooks\": [{\"type\": \"command\", \"command\": \"...\"}]}]}}";
  hooksBox.disabled = !data.can_edit_platform;
  hooksBox.oninput = () => { values.hooks = hooksBox.value; };
  const hooksWrap = el("div", "set-box"); hooksWrap.append(hooksBox, tag("hooks"));
  panel.append(hooksWrap);
  const actions = el("div", "set-actions"), mine = el("button", "ghost", "保存为我的设置"), reset = el("button", "ghost", "恢复继承");
  const saveTo = async (url, body) => { try { await hubCall("PUT", url, body); await loadSettings(); openSettings(); } catch (err) { alert(err.message); } };
  mine.onclick = () => saveTo("/settings", values);
  reset.onclick = () => { if (confirm("清掉你的个人设置，改用平台默认？")) saveTo("/settings", {}); };
  actions.append(mine, reset);
  if (data.can_edit_platform) {
    const plat = el("button", "ghost", "保存为平台默认");
    plat.title = "所有账号没单独设过的项都跟着变";
    plat.onclick = () => saveTo("/settings/platform", values);
    actions.append(plat);
  }
  actions.append(el("span", "meta", "只保存和上一层不同的项；标签显示每项来自哪一层"));
  panel.append(actions);
}
export async function loadEnabled() {
  try { const res = await fetch("/hub/enabled"); if (res.ok) ui.hub = await res.json(); } catch {}
  syncComposer();
}
async function hubCall(method, url, body) {
  const res = await fetch(url, { method, headers: { "Content-Type": "application/json" }, body: body ? JSON.stringify(body) : undefined });
  if (!res.ok) throw new Error(await res.text());
  return res;
}
function hubToggle(enabled, onchange) {
  const label = el("label", "switch"), box = el("input");
  box.type = "checkbox"; box.checked = enabled;
  box.onchange = async () => { box.disabled = true; try { await onchange(box.checked); } catch (err) { alert(err.message); box.checked = !box.checked; } box.disabled = false; loadEnabled(); };
  label.append(box, enabled ? "已启用" : "已停用");
  return label;
}
// 两个中心都在对话区展示，和“长期记忆”一样；数据来自 agent 自己的 -skills-list 与 -mcp-list。
export async function openHubPage(title, intro) {
  document.body.classList.remove("nav-open");
  if (ui.sending) return null;
  newChat();
  const panel = el("div", "mempanel hubpanel");
  $("thread").replaceChildren(panel);
  panel.append(el("h2", "", title), el("p", "meta", intro), el("div", "meta", "读取中…"));
  return panel;
}
export async function openSkills() {
  const panel = await openHubPage("Skills 中心", "skills/*/SKILL.md · 由 agent 的 -skills-list 解析和校验 · 开关保存在 .data/hub.json。新对话勾选 Skills 时，打开的 skill 只把名字和用途写进 system；模型判断任务相关时调用 load_skill 读取正文。只支持知识型 skill，不执行脚本。");
  if (!panel) return;
  let data;
  try { data = await (await hubCall("GET", "/hub")).json(); }
  catch (err) { panel.lastChild.replaceWith(el("div", "err", "读取失败：" + err.message)); return; }
  if (!panel.isConnected) return;
  const bar = el("div", "hub-bar"), add = el("button", "ghost", "＋ 新建 skill"), form = el("form", "hub-form");
  form.hidden = true;
  add.onclick = () => { form.hidden = !form.hidden; };
  bar.append(add, el("span", "meta", `${data.skills.length} 个可用` + (data.skill_errors.length ? ` · ${data.skill_errors.length} 个未通过校验` : "")));
  const name = el("input"), desc = el("input"), body = el("textarea"), submit = el("button", "ghost", "创建");
  name.placeholder = "name：小写字母、数字、连字符，如 commit-message"; desc.placeholder = "description：一行，写清做什么、什么时候用（模型靠它判断是否加载）";
  body.placeholder = "# 标题\n\n## 步骤\n1. …\n\n## 输出格式\n…";
  const row = el("div", "row"); row.append(submit, el("span", "meta", "写入 skills/<name>/SKILL.md，随后用 agent 的校验检查；不合规会撤销并给出原因"));
  form.append(name, desc, body, row);
  form.onsubmit = async (ev) => {
    ev.preventDefault(); submit.disabled = true;
    try { await hubCall("POST", "/hub/skills", { name: name.value, description: desc.value, body: body.value }); openSkills(); }
    catch (err) { alert(err.message); submit.disabled = false; }
  };
  panel.lastChild.replaceWith(bar);
  panel.append(form);
  if (!data.skills.length && !data.skill_errors.length) panel.append(el("div", "empty", "还没有 skill。点“新建 skill”写一份操作指南。"));
  for (const sk of data.skills) {
    const card = el("article", "hub-card" + (sk.enabled ? "" : " off")), head = el("div", "hub-head"), push = el("div", "push"), del = el("button", "ghost", "删除");
    del.onclick = async () => {
      if (!confirm(`删除 skills/${sk.name}/ 整个目录？无法撤销。`)) return;
      try { await hubCall("DELETE", `/hub/skills/${sk.name}`); openSkills(); loadEnabled(); } catch (err) { alert(err.message); }
    };
    push.append(hubToggle(sk.enabled, async (on) => { await hubCall("POST", `/hub/skills/${sk.name}/enabled`, { enabled: on }); openSkills(); }), del);
    head.append(el("strong", "", sk.name), push);
    const full = el("details", "step"), sum = el("summary"), inner = el("div", "body");
    sum.append(el("span", "", `SKILL.md 正文 · ${sk.body.length} 字（load_skill 返回的内容）`)); inner.append(md(sk.body)); full.append(sum, inner);
    card.append(head, el("div", "hub-desc", sk.description), el("div", "cmd", sk.path), full);
    panel.append(card);
  }
  for (const bad of data.skill_errors) {
    const card = el("article", "hub-card broken"), head = el("div", "hub-head");
    head.append(el("strong", "", bad.dir), el("span", "err", "未通过校验，agent 启动时跳过"));
    card.append(head, el("div", "hub-desc", bad.error), el("div", "cmd", bad.path));
    panel.append(card);
  }
}
// 预设统一从源码启动，在项目根目录执行，无需提前准备可执行文件。
const MCP_PRESETS = [["observer", location.origin + "/mcp", "观测台内置的远程 MCP server（Streamable HTTP），工具是 agent 的 calculator"], ["everything", "go run github.com/mark3labs/mcp-go/examples/everything@v1.1.1", "Day 6 示例 server；首次运行会下载并编译"], ["calculator", "go run . -mcp-serve", "Day 7 本项目 server，始终用当前源码"]];
export async function openMCP() {
  const panel = await openHubPage("MCP 中心", "两种 server：填 http(s) 地址是远程 server（Streamable HTTP，每条消息一个 POST）；填命令是本地 stdio server（相对 agent 目录执行，按空格切分、不经过 shell）。配置保存在 .data/hub.json。新对话勾选 MCP 时，打开的 server 由 agent 逐个连接，工具注册为 mcp_<原名>；连不上的 server 跳过，与已有工具重名的工具跳过。");
  if (!panel) return;
  let data;
  try { data = await (await hubCall("GET", "/hub/enabled")).json(); }
  catch (err) { panel.lastChild.replaceWith(el("div", "err", "读取失败：" + err.message)); return; }
  if (!panel.isConnected) return;
  const form = el("form", "hub-form"), name = el("input"), command = el("input"), submit = el("button", "ghost", "添加 server");
  name.placeholder = "name：如 observer"; command.placeholder = "远程地址如 " + location.origin + "/mcp；或 stdio 命令如 go run . -mcp-serve";
  const presets = el("div", "row");
  presets.append(el("span", "meta", "快速填入："));
  for (const [n, c, tip] of MCP_PRESETS) { const b = el("button", "link", n); b.type = "button"; b.title = tip; b.onclick = () => { name.value = n; command.value = c; }; presets.append(b); }
  const row = el("div", "row"); row.append(submit, el("span", "meta", "添加后默认启用；先点“测试连接”看协商结果和工具列表"));
  form.append(presets, name, command, row);
  form.onsubmit = async (ev) => {
    ev.preventDefault(); submit.disabled = true;
    try { await hubCall("POST", "/hub/mcp", { name: name.value, command: command.value }); openMCP(); loadEnabled(); }
    catch (err) { alert(err.message); submit.disabled = false; }
  };
  panel.lastChild.replaceWith(form);
  if (data.hosted) panel.insertBefore(el("div", "banner", `观测台内置远程 MCP server：${location.origin}/mcp · ${data.hosted}。它把 agent 的 calculator 以 Streamable HTTP 提供出来；要在对话里使用，用“快速填入 → observer”添加。`), form);
  if (!data.mcp_servers.length) panel.append(el("div", "empty", "还没有 MCP server。用上面的表单添加一个。"));
  for (const m of data.mcp_servers) {
    const card = el("article", "hub-card" + (m.enabled ? "" : " off")), head = el("div", "hub-head"), push = el("div", "push");
    const test = el("button", "ghost", "测试连接"), del = el("button", "ghost", "删除"), out = el("div");
    test.onclick = async () => {
      test.disabled = true; out.replaceChildren(el("div", "shimmer", "agent -mcp-list 连接中…"));
      try {
        const result = await (await hubCall("POST", `/hub/mcp/${m.name}/test`)).json();
        const lines = result.output.split("\n"), count = lines.map((l) => /^MCP tools\/list: count=(\d+)/.exec(l)).find(Boolean);
        const hint = /exec format error/.test(result.output) ? "：请使用上方的源码启动预设，或重新构建后重试" : "";
        out.replaceChildren(el("div", result.ok ? "meta" : "err", result.ok ? `连接成功 · ${count ? count[1] : "?"} 个工具` : "连接失败" + hint), el("pre", result.ok ? "" : "err", result.output.trim()));
      } catch (err) { out.replaceChildren(el("div", "err", err.message)); }
      test.disabled = false;
    };
    del.onclick = async () => {
      if (!confirm(`从配置中删除 ${m.name}？不会删除它的程序文件。`)) return;
      try { await hubCall("DELETE", `/hub/mcp/${m.name}`); openMCP(); loadEnabled(); } catch (err) { alert(err.message); }
    };
    push.append(hubToggle(m.enabled, async (on) => { await hubCall("POST", `/hub/mcp/${m.name}/enabled`, { enabled: on }); openMCP(); }), test, del);
    head.append(el("strong", "", m.name), push);
    card.append(head, el("div", "cmd", m.command), out);
    panel.append(card);
  }
}
