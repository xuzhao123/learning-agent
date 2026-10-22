import { show } from "./follow.js";
import { openSettings } from "./pages.js";
import { autosize, modelVision, syncComposer } from "./shell.js";
import { $, MAX_IMAGES, el, refURL, ui } from "./util.js";
export async function start(url, body) {
  ui.sending = true; syncComposer();
  try {
    const res = await fetch(url, { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify(body) });
    if (!res.ok) {
      // 失败（比如服务端说这个模型不能看图）时保留已附的图：换一下模型就能再发。
      const text = await res.text();
      if (attached.length) { ui.attachError = text; renderAttached(); }
      return alert(text);
    }
    $("query").value = "";
    attached = []; ui.attachError = ""; renderAttached();
    ui.page = "chat";
    show((await res.json()).id);
  } catch (err) {
    alert("启动失败：" + err.message);
  } finally {
    ui.sending = false; syncComposer(); autosize();
  }
}
// ---------- 附图 ----------
// 图先 POST /attachments 换成附件引用，发送时只带引用；缩略图显示 /attachments/<名字> 的原图，缩放由 agent 做。
let attached = [];
function renderAttached() {
  const strip = $("attach-strip"), pics = attached.map((img) => {
    const wrap = el("div", "pic"), thumb = el("img");
    thumb.src = refURL(img.ref);
    thumb.alt = img.name;
    thumb.title = img.name;
    const del = el("button", "del", "×");
    del.type = "button";
    del.title = "不附这张图";
    del.onclick = () => { attached = attached.filter((x) => x !== img); renderAttached(); };
    wrap.append(thumb, del);
    return wrap;
  });
  if (ui.attachError) pics.push(el("div", "err", ui.attachError));
  strip.replaceChildren(...pics);
  strip.hidden = !pics.length;
  syncComposer();
}
// 上传失败的图不进列表，服务端说的话（只支持 PNG…、超过 20 MB…）原样显示在输入框里。
async function addImages(files) {
  ui.attachError = "";
  for (const file of files) {
    if (attached.length >= MAX_IMAGES) { ui.attachError = `每条消息最多 ${MAX_IMAGES} 张图`; break; }
    try {
      const res = await fetch("/attachments?name=" + encodeURIComponent(file.name || "image"), { method: "POST", body: file });
      if (!res.ok) { ui.attachError = await res.text(); continue; }
      attached.push(await res.json());
    } catch (err) { ui.attachError = "上传失败：" + err.message; }
  }
  renderAttached();
}
$("attach").onclick = () => { if (!$("attach").disabled) $("attach-file").click(); };
$("attach-file").onchange = () => {
  const files = [...$("attach-file").files];
  $("attach-file").value = "";
  if (files.length) addImages(files);
};
// 粘贴：剪贴板里的图片文件（截图最常见）；没有图片时照常粘贴文字。
$("query").addEventListener("paste", (ev) => {
  const files = [...(ev.clipboardData?.files || [])].filter((f) => f.type.startsWith("image/"));
  if (!files.length) return;
  ev.preventDefault();
  addImages(files);
});
// 拖放：落在输入区就当附图，不让浏览器直接打开这个文件。
const dropZone = $("composer-wrap");
dropZone.addEventListener("dragover", (ev) => { if ([...(ev.dataTransfer?.types || [])].includes("Files")) { ev.preventDefault(); dropZone.classList.add("drag"); } });
dropZone.addEventListener("dragleave", () => dropZone.classList.remove("drag"));
dropZone.addEventListener("drop", (ev) => {
  const files = [...(ev.dataTransfer?.files || [])].filter((f) => f.type.startsWith("image/"));
  if (!files.length) return;
  ev.preventDefault();
  dropZone.classList.remove("drag");
  addImages(files);
});
$("composer").onsubmit = (ev) => {
  ev.preventDefault();
  const query = $("query").value.trim();
  // D34：运行中按回车是插话：排进 agent 的下一次模型请求，不打断正在进行的请求或工具。
  if ($("send").classList.contains("stop")) { if (query) steer(query); return; }
  if ($("send").disabled || !query) return;
  // 附了图又换到不能看图的模型：说明原因、保留图，换回能看图的模型就能发。
  const vis = ui.run ? modelVision(ui.run.provider || "ark", ui.run.model || "") : modelVision($("provider").value, $("model").value);
  if (attached.length && !vis.vision) {
    ui.attachError = `当前模型 ${vis.name} 不能看图，换一个能看图的模型再发送`;
    renderAttached();
    return;
  }
  ui.chatFollow = true;
  const images = attached.map(({ ref, name }) => ({ ref, name }));
  const body = ui.run ? { query, permission: $("perm").value } : { query, provider: $("provider").value, model: $("model").hidden ? "" : $("model").value, workspace: $("workspace").disabled ? "" : $("workspace").value, permission: $("perm").value };
  start(ui.run ? `/runs/${ui.run.id}/messages` : "/runs", images.length ? { ...body, images } : body);
};
async function steer(text) {
  try {
    const res = await fetch(`/runs/${encodeURIComponent(ui.run.id)}/steer`, { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ text }) });
    if (!res.ok) throw new Error(await res.text());
    $("query").value = "";
    autosize();
  } catch (err) { alert("插话没有送到：" + err.message); }
}
$("send").onclick = async (ev) => {
  if (!$("send").classList.contains("stop")) return; // 发送仍走表单提交
  ev.preventDefault();
  const r = ui.run;
  if (!r) return;
  if (r.stopRequested && !confirm("agent 正在停止（回填结果、保存检查点）。确定强制结束？未保存的进度会丢失。")) return;
  try {
    const res = await fetch(`/runs/${encodeURIComponent(r.id)}/stop`, { method: "POST" });
    if (!res.ok && res.status !== 409) alert(await res.text());
  } catch (err) { alert("停止失败：" + err.message); }
};
$("provider").onchange = () => { ui.picked.provider = true; ui.picked.model = false; syncComposer(); };
$("model").onchange = () => { ui.picked.model = true; syncComposer(); };
$("workspace").onchange = () => { ui.picked.workspace = true; syncComposer(); };
$("perm").onchange = () => { ui.picked.permission = true; syncComposer(); };
$("tools-link").onclick = () => { if (ui.run) { ui.toolsOpen = !ui.toolsOpen; syncComposer(); } else openSettings(); };
$("settings").onclick = () => openSettings();
// 输入法组合输入时的 Enter 用于选词，不能发送。
$("query").onkeydown = (ev) => { if (ev.key === "Enter" && !ev.shiftKey && !ev.isComposing) { ev.preventDefault(); $("composer").requestSubmit(); } };
$("query").oninput = () => { autosize(); syncComposer(); syncSlash(); };
// D26：斜杠命令。输入框里只有 /前缀 时列出命令（个人级 commands/、所选工作区的 .agents/commands/）；展开由 agent 做。
async function loadCommands() {
  const ws = ui.run ? ui.run.workspace || "" : $("workspace").value;
  if (ui.commands && ui.commandsFor === ws) return;
  try { const res = await fetch("/commands?workspace=" + encodeURIComponent(ws)); ui.commands = res.ok ? await res.json() : []; } catch { ui.commands = []; }
  ui.commandsFor = ws;
}
async function syncSlash() {
  const box = $("slash"), m = /^\/([a-z0-9_-]*)$/.exec($("query").value);
  if (!m) { box.hidden = true; return; }
  await loadCommands();
  const list = ui.commands.filter((c) => c.name.startsWith(m[1]));
  box.replaceChildren(...list.map((c) => {
    const b = el("button", "slash-item");
    b.type = "button";
    b.append(el("span", "slash-name", "/" + c.name + (c.argument_hint ? " " + c.argument_hint : "")), el("span", "meta", (c.scope === "project" ? "项目 · " : "个人 · ") + (c.description || "")));
    b.onmousedown = (ev) => ev.preventDefault();
    b.onclick = () => { $("query").value = "/" + c.name + " "; box.hidden = true; $("query").focus(); syncComposer(); };
    return b;
  }));
  if (!list.length) box.append(el("div", "meta", ui.commands.length ? "没有匹配的命令" : "没有可用的命令：放在 commands/ 或工作区的 .agents/commands/"));
  box.hidden = false;
}
$("query").onblur = () => { $("slash").hidden = true; };
$("workspace").addEventListener("change", () => { ui.commands = null; });
