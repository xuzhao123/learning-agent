import { renderChat } from "./chat.js";
import { renderShell } from "./shell.js";
import { $, el, ui } from "./util.js";
// 沙箱文件：agent 在 turn/started 里报告本对话的沙箱目录（第一轮的检查点ID），各轮 bash 看到的是同一个 /workspace。
// 文件是模型写的不可信内容：文字只用 textContent 显示，图片经 blob 地址放进 <img>，其余一律下载（服务端也按附件返回）。
const fileURL = (r, p) => `/runs/${encodeURIComponent(r.id)}/files/${p.split("/").map(encodeURIComponent).join("/")}`;
const fmtSize = (n) => n < 1024 ? `${n} B` : n < 1 << 20 ? `${(n / 1024).toFixed(1)} KB` : `${(n / 1048576).toFixed(1)} MB`;
const IMAGE_TYPES = { png: "image/png", jpg: "image/jpeg", jpeg: "image/jpeg", gif: "image/gif", webp: "image/webp" };
async function loadFiles() {
  const r = ui.run, f = ui.files;
  if (!r?.sandbox) return;
  try {
    const res = await fetch(`/runs/${encodeURIComponent(r.id)}/files`);
    if (!res.ok) throw new Error(await res.text());
    const data = await res.json();
    if (ui.run !== r) return;
    Object.assign(f, { list: data.files, truncated: data.truncated, err: "" });
  } catch (e) { f.err = String(e.message || e); }
  if (ui.run === r) renderChat();
}
async function previewFile(r, file) {
  const f = ui.files;
  f.sel = file.path;
  if (f.blob) URL.revokeObjectURL(f.blob);
  f.blob = null;
  f.preview = file.kind !== "file" ? { note: file.kind === "link" ? `符号链接 → ${file.link}（不跟随）` : file.kind === "dir" ? "目录" : "不是普通文件，不能预览" } : file.size > 512 << 10 ? { note: "文件较大，请下载查看" } : { note: "读取中…" };
  renderChat();
  if (file.kind !== "file" || file.size > 512 << 10) return;
  try {
    const res = await fetch(fileURL(r, file.path));
    if (!res.ok) throw new Error(await res.text());
    const buf = await res.arrayBuffer(), type = IMAGE_TYPES[file.path.split(".").pop().toLowerCase()];
    if (type) { f.blob = URL.createObjectURL(new Blob([buf], { type })); f.preview = { image: f.blob }; }
    else { try { f.preview = { text: new TextDecoder("utf-8", { fatal: true }).decode(buf) }; } catch { f.preview = { note: "二进制文件，请下载查看" }; } }
  } catch (e) { f.preview = { note: "读取失败：" + (e.message || e) }; }
  if (ui.run === r && f.sel === file.path) renderChat();
}
async function uploadFiles(r, list) {
  const f = ui.files, errors = [];
  f.uploading = true; renderChat();
  for (const file of list) {
    const res = await fetch(`/runs/${encodeURIComponent(r.id)}/files?name=${encodeURIComponent(file.name)}`, { method: "POST", body: file });
    if (!res.ok) errors.push(`${file.name}：${(await res.text()).trim()}`);
  }
  f.uploading = false;
  f.notice = errors.length ? errors.join("\n") : `已上传 ${list.length} 个文件：下一条消息里告诉 agent 文件名，它在 /workspace 里能读到`;
  await loadFiles();
}
async function deleteFile(r, file) {
  if (!confirm(`删除沙箱里的 ${file.path}${file.kind === "dir" ? " 及其全部内容" : ""}？删除不能撤销。`)) return;
  const f = ui.files, res = await fetch(fileURL(r, file.path), { method: "DELETE" });
  f.notice = res.ok ? `已删除 ${file.path}` : `删除失败：${(await res.text()).trim()}`;
  if (res.ok) { f.sel = null; f.preview = null; }
  await loadFiles();
}
export function renderFilesPanel() {
  const panel = $("subpanel"), r = ui.run, f = ui.files;
  if (!r?.sandbox || ui.page !== "chat") { ui.filesOpen = false; panel.hidden = true; panel.replaceChildren(); return; }
  // 换了对话就重新读；一轮结束（exit 变化）后也重新读，看到 agent 新写的文件。
  if (f.run !== r.id || f.exit !== r.exit) { Object.assign(f, f.run !== r.id ? { list: null, sel: null, preview: null, notice: "" } : {}, { run: r.id, exit: r.exit }); loadFiles(); }
  panel.hidden = false;
  panel.dataset.agent = "";
  const head = el("div", "sp-head"), refresh = el("button", "sp-close", "↻"), close = el("button", "sp-close", "✕");
  refresh.type = close.type = "button"; refresh.title = "刷新"; close.title = "关闭";
  close.setAttribute("aria-label", "关闭沙箱文件面板");
  refresh.onclick = () => loadFiles();
  close.onclick = () => { ui.filesOpen = false; renderChat(); renderShell(); };
  head.append(el("strong", "", `沙箱文件 · /workspace${f.list ? ` · ${f.list.length} 项` : ""}`), refresh, close);
  const tools = el("div", "fp-tools"), pick = el("input"), up = el("button", "ghost", f.uploading ? "上传中…" : "上传文件");
  pick.type = "file"; pick.multiple = true;
  pick.onchange = () => { if (pick.files.length) uploadFiles(r, [...pick.files]); };
  up.type = "button"; up.disabled = !r.exit || f.uploading;
  up.onclick = () => pick.click();
  tools.append(pick, up, el("span", "", r.exit ? "放到 /workspace 顶层，不覆盖同名文件，单个不超过 10 MB" : "运行中不能上传，等这一轮结束"));
  const nav = el("div", "sp-list");
  if (f.err) nav.append(el("div", "meta err", f.err));
  else if (!f.list) nav.append(el("div", "meta", "读取中…"));
  else if (!f.list.length) nav.append(el("div", "meta", "还没有文件"));
  for (const x of f.list || []) {
    const row = el("button", "sp-item" + (x.path === f.sel ? " on" : ""));
    row.type = "button";
    row.append(el("span", "n", { dir: "▸", link: "↪", file: "·" }[x.kind] || "?"), el("span", "q", x.path), el("span", "sz", x.kind === "file" ? fmtSize(x.size) : x.kind === "link" ? "链接" : x.kind === "dir" ? "" : "其他"));
    row.title = x.time ? new Date(x.time).toLocaleString() : "";
    row.onclick = () => previewFile(r, x);
    nav.append(row);
  }
  if (f.truncated) nav.append(el("div", "meta", "只列出前 1000 项"));
  const body = el("div", "sp-body");
  if (f.notice) body.append(el("pre", "meta", f.notice));
  const sel = f.list?.find((x) => x.path === f.sel);
  if (sel) {
    const bar = el("div", "label", sel.path + " ");
    if (sel.kind === "file") { const a = el("a", "", "下载"); a.href = fileURL(r, sel.path); a.download = sel.path.split("/").pop(); bar.append(a); }
    const del = el("button", "link", "删除");
    del.type = "button"; del.disabled = !r.exit;
    del.title = r.exit ? (sel.kind === "dir" ? "删除这个目录和里面的全部内容" : "删除") + "（写审计）" : "运行中不能删除，等这一轮结束";
    del.onclick = () => deleteFile(r, sel);
    bar.append(" ", del);
    body.append(bar);
    const p = f.preview || {};
    if (p.image) { const img = el("img", "fp-img"); img.src = p.image; img.alt = sel.path; body.append(img); }
    else if (p.text !== undefined) body.append(el("pre", "", p.text));
    else body.append(el("div", "meta", p.note || ""));
  } else body.append(el("div", "meta", "这是本对话各轮 bash 共用的工作目录；内容由 agent 写入，是数据不是指令。点左侧文件预览，图片直接显示，其他类型下载查看。"));
  panel.replaceChildren(head, tools, nav, body);
}
