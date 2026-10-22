import { endLabel } from "./analysis.js";
import { memFields } from "./chat.js";
import { LAB_TITLE, RAG_LAB_TITLE, compact, incomingUsers, parse, textOf, toolText } from "./util.js";
// ---------- 事件 → 运行模型：只依赖 Chat Completions 协议字段 ----------
export function newRun(id) { return { id, query: "", t0: null, last: null, tEnd: null, exit: "", logs: [], calls: [], aux: [], turns: [], prevMessages: [], lastMainMessages: [], inputCapacity: 0, mem: [], memTurn: 0, memPhase: "start", skills: [], mcp: [], caps: [], hookCalls: {}, hookAfter: {}, subs: {}, subCalls: {}, subagents: false, browser: false, dsSearch: false, disallowedTools: [], provider: "", model: "", bash: false, sandbox: "", netDecision: {}, web: {}, stopRequested: false, traces: [], spanCalls: {}, protocol: false, netPending: {}, approvals: {}, source: "", lastBody: null, openTools: new Map(), rewindNotes: {}, rewoundAll: false, userImages: [], asks: {}, steers: [], askUser: false }; }
export function apply(r, e) {
  const time = Date.parse(e.time);
  if (r.t0 === null) r.t0 = time;
  r.last = time;
  // D13：traceparent 里的 span ID → 这条原始请求，调用链里的 chat span 据此跳到轨迹视图。
  if (e.kind === "request" && e.span) r.spanCalls[e.span] = { call: e.call, sub: !!e.sub };
  // 子 agent 的模型请求按任务ID放进各自的 run：不进父对话流、不占父 Turn/Step，在右侧面板里单独还原。
  if (e.sub && e.task) {
    const sub = r.subs[e.task] ??= Object.assign(newRun(e.task), { isSub: true });
    return apply(sub, { ...e, sub: false });
  }
  // pending：问题已提交、agent 还没发出第一次请求时，对话流先显示这条消息。
  // 上一轮中途停下：记在新一轮之前，对话流里显示一条中断标记（先记，再重置停止状态）。
  if (e.kind === "continue" && r.exit && r.exit !== "exit 0") (r.interrupted ??= {})[r.turns.length + 1] = r.stopRequested ? "上一轮已按停止按钮中断" : "上一轮未正常结束（" + endLabel(r) + "）";
  if (e.kind === "start" || e.kind === "continue") { r.memory = !!e.memory; r.memoryTTL = e.memory_ttl || ""; r.memTurn++; r.memPhase = "start"; r.skills = e.skills || []; r.mcp = e.mcp || []; r.subagents = !!e.subagents; r.askUser = !!e.ask_user; r.browser = !!e.browser; r.dsSearch = !!e.ds_search; r.disallowedTools = e.disallowed_tools || []; r.provider = e.provider || "ark"; r.model = e.model || ""; r.bash = !!e.bash; r.rag = !!e.rag; r.workspace = e.workspace || ""; r.maxSteps = e.max_steps || 0; r.permission = e.permission || ""; r.stopRequested = false; r.checkpoint = ""; r.protocol = !!e.protocol; r.source = e.source || ""; if (e.trace) r.traces.push({ turn: r.memTurn, id: e.trace }); }
  if (e.kind === "start") { r.query = e.text; r.embedding = e.embedding; r.embeddingExplicit = !!e.embedding; r.pending = [LAB_TITLE, RAG_LAB_TITLE].includes(e.text) ? "" : e.text; r.turnPending = true; r.pendingTime = time; }
  else if (e.kind === "continue") {
    r.exit = ""; r.termination = ""; r.finalCheck = null; r.tEnd = null; r.pending = e.text; r.turnPending = true; r.pendingTime = time;
    if (e.embedding) { r.embedding = e.embedding; r.embeddingExplicit = true; }
    r.logs.push({ ...e, kind: "stdout", text: "用户续聊：" + e.text });
  }
  else if (e.kind === "request" && (e.purpose?.startsWith("memory_") || e.purpose === "verify")) {
    // 记忆辅助调用按 agent 的明确用途标记分开：不开新 Turn、不占 Step、不参与续聊恢复和最终回答判断。
    const turn = r.turnPending || !r.turns.length ? r.turns.length + 1 : r.turns.at(-1).n;
    r.aux.push({ aux: true, purpose: e.purpose, n: e.call, turn, req: e.body || {}, start: time, tools: [], usage: {} });
  }
  else if (e.kind === "request") {
    // 导入的对话（Codex / Claude Code）只存新增的消息：接在上一次请求后面还原完整输入，工具定义沿用。
    if (e.body?.append && r.lastBody) e = { ...e, body: { ...e.body, messages: (r.lastBody.messages || []).concat(e.body.messages || []), tools: e.body.tools ?? r.lastBody.tools } };
    if (e.body) r.lastBody = e.body;
    const messages = (e.body && e.body.messages) || [];
    // 先比引用再比内容：长会话（几百次请求、上千条消息）逐条序列化会很慢。
    const prefixLength = (a, b) => { let n = 0; while (n < a.length && n < b.length && (a[n] === b[n] || JSON.stringify(a[n]) === JSON.stringify(b[n]))) n++; return n; };
    const shared = prefixLength(r.prevMessages, messages);
    const prev = r.calls[r.calls.length - 1];
    const lastMessage = messages[messages.length - 1];
    const isCompact = e.purpose === "compact" || (!!lastMessage && lastMessage.role === "user" && String(lastMessage.content).startsWith("[上下文压缩请求]"));
    const users = isCompact ? [] : incomingUsers(messages), lastMain = r.calls.findLast((c) => !c.compact);
    let turn = r.turns.at(-1);
    // start/continue是明确的回合边界；实验与旧存档依据新user和上次最终响应恢复边界。
    // 工具结果、失败后的同请求重试、摘要都不另开用户回合。
    const opensTurn = !turn || r.turnPending || r.caseStart || (users.length > 0 && lastMain?.turn === turn.n && lastMain.choice && !lastMain.tools.length);
    const userMessage = opensTurn ? (r.pending ? { role: "user", content: r.pending } : users.at(-1)) : null;
    if (opensTurn) {
      turn = { n: r.turns.length + 1, steps: 0, question: userMessage ? textOf(userMessage) : "", start: r.pendingTime || time, caseLabel: r.caseLabel, checkpoint: r.checkpoint, idle: r.lastReply ? time - r.lastReply : 0, images: r.userImages[r.turns.length] || [] };
      r.turns.push(turn);
    }
    const step = isCompact ? 0 : ++turn.steps;
    r.memPhase = "end";
    const prompt = compact({ system: messages.filter((m) => m.role === "system"), tools: e.body?.tools || [] });
    const promptChanged = prompt !== r.prompt;
    r.prompt = prompt; r.turnPending = false; r.pendingTime = null; r.pending = "";
    const rebuilt = !isCompact && ((prev && prev.compact) || (r.lastMainMessages.length > 0 && prefixLength(r.lastMainMessages, messages) < r.lastMainMessages.length));
    // 压缩后的消息数可能变短；按调用ID在所有历史请求中关联结果，不能依赖旧数组长度。
    // 前缀相同的部分上次已经处理过，只看新增的消息。
    for (const m of rebuilt || isCompact ? messages : messages.slice(shared)) if (m.role === "tool") {
      const content = toolText(m.content), observation = parse(content);
      if (!r.embeddingExplicit && observation?.tool === "search_docs" && ["ark", "local"].includes(observation.result?.backend)) r.embedding = observation.result.backend;
      const t = r.openTools.get(m.tool_call_id);
      if (t) { t.result = content; t.done = time; t.isError = !!m.is_error; r.openTools.delete(m.tool_call_id); }
    }
    if (!isCompact) r.lastMainMessages = messages;
    if (!r.query) { const u = messages.find((m) => m.role === "user"); if (u) r.query = u.content; }
    r.prevMessages = messages;
    r.calls.push({ i: r.calls.length + 1, compact: isCompact, rebuilt, shared, caseLabel: r.caseLabel, caseStart: r.caseStart, turn: turn.n, step, userMessage, promptChanged,
      n: e.call, req: e.body || {}, start: time, tools: [], usage: {} });
    r.caseStart = false;
  } else if (e.kind === "response") {
    // 请求编号包含辅助调用，不能再当作 r.calls 的下标。
    const c = r.calls.find((x) => x.n === e.call) || r.aux.find((x) => x.n === e.call);
    if (!c) return;
    const body = e.body || {};
    Object.assign(c, { res: body, rawBody: e.body, end: time, ms: e.ms, status: e.status, errorText: e.text,
      choice: (body.choices || [])[0], usage: body.usage || {}, chunks: e.chunks || 0, firstToken: e.first_token_ms || 0, live: null });
    const m = (c.choice && c.choice.message) || {};
    c.invalidSummary = c.compact && (m.tool_calls || []).length > 0;
    if (c.aux) return;
    // D30：续聊那一轮的第一次请求：距上一轮闲置了多久、缓存还剩多少。隔得越久，缓存越可能已经过期，整段上下文要按全价重新处理。
    if (c.choice) r.lastReply = time;
    const t = r.turns[c.turn - 1];
    if (t && t.n > 1 && c.step === 1 && !t.firstUsage && body.usage) t.firstUsage = body.usage;
    c.tools = (c.compact ? [] : (m.tool_calls || [])).map((t, i) => ({ id: t.id, name: t.function.name, args: t.function.arguments, start: time, call: c, index: i }));
    for (const t of c.tools) r.openTools.set(t.id, t);
  } else if (e.kind === "stdout" || e.kind === "stderr") {
    r.logs.push(e);
    if (!r.protocol) legacyLine(r, e, time);
  }
  else if (e.kind === "notify" || e.kind === "delta") applyNotify(r, e.method, e.body || {}, time);
  else if (e.kind === "approval") {
    const p = e.body || {};
    if (e.method === "tool/requestApproval") r.approvals[p.call_id] = { ...p, state: "waiting" };
    else if (e.method === "user/requestInput") r.asks[p.call_id] = { ...p, state: "waiting" }; // D34：模型的提问，等人回答
    else if (p.domain) r.netPending[p.domain] = true;
  }
  else if (e.kind === "answer") { const a = r.asks[e.text]; if (a) Object.assign(a, { state: "answered", answer: (e.body || {}).answer }); }
  else if (e.kind === "decision") { const a = r.approvals[e.text]; if (a) a.state = (e.body || {}).decision; }
  else if (e.kind === "allow" || e.kind === "deny") { r.netDecision[e.text] = e.kind; delete r.netPending[e.text]; }
  else if (e.kind === "rewind") {
    // D29：撤销了这一轮及之后各轮的文件改动；对话从撤销点的上一轮接着聊（它的检查点），这几轮留着、淡化显示。
    const b = e.body || {}, hit = r.turns.filter((t) => (b.turns || []).includes(t.checkpoint));
    for (const t of hit) t.rewound = true;
    const range = hit.length > 1 ? `第 ${hit[0].n}–${hit.at(-1).n} 轮` : hit.length ? `第 ${hit[0].n} 轮` : `${(b.turns || []).length} 轮`;
    r.rewindNotes[r.turns.length + 1] = `已撤销${range}：恢复 ${(b.restore || []).length} 个文件，删除 ${(b.delete || []).length} 个新建文件` + (b.continues ? "；接着上一轮聊" : "；第一轮也撤销了，请新建对话");
    r.checkpoint = b.continues || ""; r.rewoundAll = !b.continues;
  }
  else if (e.kind === "stop") { r.stopRequested = true; r.logs.push({ ...e, kind: "stdout", text: e.text }); }
  else if (e.kind === "exit") {
    r.exit = e.text || "exit"; r.tEnd = time; r.netPending = {};
    for (const a of Object.values(r.approvals)) if (a.state === "waiting") a.state = "expired";
  }
}

// B0：agent 经协议发来的结构化通知。页面需要的信息（任务ID、输入容量、记忆与能力事件、浏览器地址、子 agent 的起止和输出、
// 实验分组）都从这里来，不再解析终端文本。方法名见 internal/protocol 与 docs/bonus-protocol。
function applyNotify(r, method, p, time) {
  if (method === "turn/started") { if (p.turn_id) r.checkpoint = p.turn_id; if (p.input_capacity) r.inputCapacity = p.input_capacity; if (p.sandbox) r.sandbox = p.sandbox; if (p.model) r.model = p.model; }
  // 附图：用户消息带了哪几张图由 agent 在协议里报（引用、编号、尺寸），页面据此画缩略图。
  // 这条通知在本次交互的第一次请求之前发出，所以此刻的轮次数就是这条消息所属轮的序号。
  // D34：运行中插话（steer=true）排进了下一次模型请求：记下它在哪次请求之前，对话流里显示在那里。
  else if (method === "item/completed" && p.item?.type === "userMessage" && p.item.data?.steer) r.steers.push({ call: r.calls.length + 1, text: p.item.data.text || "" });
  else if (method === "item/completed" && p.item?.type === "userMessage") r.userImages[r.turns.length] = p.item.data?.images || [];
  // D33：agent 标明的回复分段，挂在产生它的那次模型请求上（响应先于这条通知记录）。旧存档没有，按有没有工具调用判断。
  else if (method === "item/completed" && p.item?.type === "agentMessage") { const c = r.calls.filter((x) => !x.compact && x.end).at(-1); if (c) c.phase = p.item.data?.phase; }
  else if (method === "memory/event") {
    // 长期记忆只在开头调入、结尾写入；工具里的 search/forget 已作为工具步骤显示。
    if (!["search", "forget", "hit"].includes(p.kind) && !/^phase=(search|cli) /.test(p.text || "")) r.mem.push({ turn: r.memTurn, phase: r.memPhase, type: p.kind, text: p.text, fields: memFields(p.text || "") });
  }
  // skill 索引与 MCP 连接在 agent 启动时发生，归到本轮开头的“能力”卡片。
  else if (method === "capability/event") r.caps.push({ turn: r.memTurn, kind: p.kind, text: p.text, count: p.count });
  // D26：工具前后的 hook 挂在那次工具调用上；Stop 挂在被拦下的那次回答后面；UserPromptSubmit 归到本轮开头的能力卡片。
  else if (method === "hook/completed") {
    const h = { event: p.event, outcome: p.kind, command: p.command, output: p.output || "", text: p.text };
    if (p.call_id) (r.hookCalls[p.call_id] ||= []).push(h);
    else if (p.event === "Stop" && r.calls.length) (r.hookAfter[r.calls.at(-1).n] ||= []).push(h);
    else r.caps.push({ turn: r.memTurn, kind: "hook", text: p.text });
  }
  // 浏览器画面按“任务ID/调用ID”保存；续聊每轮是新进程、新任务ID，所以记下当时的任务ID。
  else if (method === "browser/navigate") r.web[p.call_id] = { task: r.checkpoint, url: p.url };
  else if (method === "subagent/started") Object.assign(r.subCalls[p.call_id] ??= {}, { task: p.task_id, text: p.task, state: "running" });
  // 被取消、超时或进程中途退出的子任务可以续跑，标为“已中断”（stopped）；预算耗尽、熔断等才算失败。
  else if (method === "subagent/completed") Object.assign(r.subCalls[p.call_id] ??= { task: p.task_id }, { state: p.state, calls: p.model_calls || 0, replayed: !!p.replayed, error: p.error || "" });
  else if (method === "subagent/log") subRun(r, p.task_id).logs.push({ kind: "stdout", text: p.text, time });
  // 子 agent 转发上来的通知，原样应用到它自己的运行状态上（任务ID、浏览器地址、结局……），和父 agent 走同一套处理。
  else if (method === "subagent/message" && p.message?.method) applyNotify(subRun(r, p.task_id), p.message.method, p.message.params || {}, time);
  // B1：流式增量只拼到正在进行的这次请求上做实时显示；响应到达后以完整响应为准（live 清空）。
  // 主任务的模型请求一次只有一个在进行，增量一定属于最后一次请求；晚到的增量直接丢弃。
  // O1：模型请求重试前 agent 发 reset，清掉这次请求已经显示的半段，免得同一段话出现两遍。
  else if (method === "item/delta") { const c = r.calls.at(-1); if (c && !c.end) { if (p.field === "reset") c.live = {}; else { const live = c.live ??= {}; live[p.field] = (live[p.field] || "") + p.delta; } } }
  else if (method === "turn/completed") { r.termination = p.outcome === "no_tool_calls" ? "" : p.outcome + (p.error ? "：" + p.error : ""); r.finalCheck = p.final_check || null; }
  else if (method === "lab/case") {
    r.caseLabel = p.id + (p.mode === "rag" ? " · 有检索" : " · 无检索");
    r.caseStart = true;
    r.prevMessages = []; r.lastMainMessages = []; // 对比实验每题独立，不能误标为压缩。
  }
}
function subRun(r, task) { return r.subs[task] ??= Object.assign(newRun(task), { isSub: true }); }

// B0 之前的存档没有协议通知，只有终端文本：把当时的日志行换算成同样的通知。新的运行不经过这里。
function legacyLine(r, e, time) {
  const t = e.text || "";
  let m;
  if ((m = /^RAG case: (Q\d+) mode=(baseline|rag) /.exec(t))) applyNotify(r, "lab/case", { id: m[1], mode: m[2] }, time);
  if ((m = /^Memory (\w+): (.*)$/.exec(t))) applyNotify(r, "memory/event", { kind: m[1], text: m[2] }, time);
  if (/^(Skills?|MCP) (loaded|error|connect|initialize|tools\/list|register|skip)\b|^Skills: /.test(t)) {
    const kind = t.startsWith("Skills: ") ? "skills_loaded" : t.startsWith("MCP register:") ? "mcp_register" : /^(Skill|MCP) error/.test(t) ? (t.startsWith("MCP") ? "mcp_error" : "skill_error") : t.startsWith("MCP skip") ? "mcp_skip" : "mcp";
    applyNotify(r, "capability/event", { kind, text: t, count: Number(/^Skills: loaded=(\d+)/.exec(t)?.[1] || 0) }, time);
  }
  if ((m = /^Checkpoint: id=(\S+) /.exec(t))) r.checkpoint = m[1];
  if ((m = /^Browser \[([^\]]+)\]: (search|open) url=(\S+)/.exec(t))) applyNotify(r, "browser/navigate", { call_id: m[1], url: m[3] }, time);
  if ((m = /^Context: .* cap=(\d+) /.exec(t))) r.inputCapacity = Number(m[1]);
  if ((m = /^Subagent (start|done|stop) \[([^\]]+)\]: (.*)$/.exec(t))) {
    if (m[1] === "start") applyNotify(r, "subagent/started", { call_id: m[2], task_id: /task_id=(\S+)/.exec(m[3])?.[1] || "" }, time);
    if (m[1] === "done") applyNotify(r, "subagent/completed", { call_id: m[2], state: "done", model_calls: Number(/model_calls=(\d+)/.exec(m[3])?.[1] || 0), replayed: /replayed=true/.test(m[3]) }, time);
    if (m[1] === "stop") applyNotify(r, "subagent/completed", { call_id: m[2], state: /（(cancelled|timeout|进程中途退出)）/.test(m[3]) ? "stopped" : "failed", error: m[3] }, time);
  }
  const child = /^  │ (\S+) ?(.*)$/.exec(t), info = child && r.subCalls[child[1]];
  if (info?.task) {
    subRun(r, info.task).logs.push({ ...e, text: child[2] });
    if (child[2].startsWith("Termination:") && child[2] !== "Termination: no_tool_calls") subRun(r, info.task).termination = child[2].slice(12).trim();
  }
}

