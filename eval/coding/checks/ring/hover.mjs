// D36 页面悬浮题的校验（复查 R1、R12）：在沙箱里用真实的无头 Chrome 打开 /workspace/index.html，
// 把鼠标移到 #ctx-ring 上，看页面上“看得见的文字”里有没有用量明细，移开后又消失。
// 只算看得见的：元素与伪元素的 display、visibility、opacity 都算进去；原生 title 提示不进 DOM，靠它过不了。
// 用法：node hover.mjs [页面路径]，通过时退出码 0。
import { spawn } from "node:child_process";

const page = "file://" + (process.argv[2] || "/workspace/index.html");
const port = 9333;
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
let chrome;
const fail = (msg) => { console.log("FAIL：" + msg); chrome?.kill("SIGKILL"); process.exit(1); };

chrome = spawn("google-chrome", ["--headless", "--no-sandbox", "--disable-gpu", "--user-data-dir=/tmp/chrome-check",
  `--remote-debugging-port=${port}`, "--window-size=1000,800", "about:blank"], { stdio: "ignore" });
let version;
for (let i = 0; i < 50 && !version; i++) {
  await sleep(200);
  try { version = await (await fetch(`http://127.0.0.1:${port}/json/version`)).json(); } catch {}
}
if (!version) fail("Chrome 没有启动");
const target = await (await fetch(`http://127.0.0.1:${port}/json/new?${page}`, { method: "PUT" })).json();
const ws = new WebSocket(target.webSocketDebuggerUrl);
await new Promise((resolve, reject) => { ws.onopen = resolve; ws.onerror = reject; });
let seq = 0;
const pending = new Map(), errors = [];
ws.onmessage = (ev) => {
  const m = JSON.parse(ev.data);
  if (m.id && pending.has(m.id)) { pending.get(m.id)(m); pending.delete(m.id); }
  else if (m.method === "Runtime.exceptionThrown") errors.push(m.params.exceptionDetails.exception?.description || m.params.exceptionDetails.text);
};
const send = (method, params = {}) => new Promise((resolve) => { const id = ++seq; pending.set(id, resolve); ws.send(JSON.stringify({ id, method, params })); });
const run = async (expression) => (await send("Runtime.evaluate", { expression, returnByValue: true })).result?.result?.value;

await send("Runtime.enable");
await send("Page.enable");
await send("Page.reload");
await sleep(1500);

// 看得见的文字：元素自己的文字节点，加上 ::before / ::after 的 content（attr() 换成属性值）。
const visibleText = `(() => {
  const out = [];
  const seen = (el) => el.checkVisibility({ opacityProperty: true, visibilityProperty: true, contentVisibilityAuto: true });
  for (const el of document.querySelectorAll("body, body *")) {
    if (!seen(el)) continue;
    for (const node of el.childNodes) if (node.nodeType === 3 && node.textContent.trim()) out.push(node.textContent.trim());
    for (const pseudo of ["::before", "::after"]) {
      const cs = getComputedStyle(el, pseudo);
      if (!cs.content || cs.content === "none" || cs.content === "normal" || cs.display === "none" || cs.visibility === "hidden" || Number(cs.opacity) === 0) continue;
      out.push(cs.content.replace(/attr\\(([\\w-]+)\\)/g, (_, name) => el.getAttribute(name) || "").replace(/["']/g, ""));
    }
  }
  return out.join(" ");
})()`;

const box = await run(`(() => {
  const r = document.getElementById("ctx-ring"), s = document.getElementById("send");
  if (!r || !s) return null;
  const a = r.getBoundingClientRect(), b = s.getBoundingClientRect();
  return { x: a.x + a.width / 2, y: a.y + a.height / 2, w: a.width, h: a.height, right: a.right, top: a.top, bottom: a.bottom, sendLeft: b.left, sendTop: b.top, sendBottom: b.bottom };
})()`);
if (!box) fail("页面上没有 #ctx-ring（或 #send 不见了）");
if (box.w < 8 || box.h < 8) fail(`#ctx-ring 太小或没显示：${box.w}×${box.h}`);
if (box.right > box.sendLeft + 1) fail("#ctx-ring 不在发送键左侧");
if (box.bottom < box.sendTop || box.top > box.sendBottom) fail("#ctx-ring 和发送键不在同一行");

const want = ["18%", "82%", "151.2k", "828k"];
const before = await run(visibleText);
if (before.includes("151.2k")) fail("还没悬浮，明细就已经显示了");
await send("Input.dispatchMouseEvent", { type: "mouseMoved", x: box.x, y: box.y });
await sleep(700);
const during = await run(visibleText);
const missing = want.filter((w) => !during.includes(w));
if (missing.length) fail("悬浮时看不到：" + missing.join("、") + "；页面上看得见的文字：" + during.slice(0, 300));
await send("Input.dispatchMouseEvent", { type: "mouseMoved", x: 5, y: 5 });
await sleep(700);
if ((await run(visibleText)).includes("151.2k")) fail("鼠标移开后明细没有消失");
if (errors.length) fail("页面脚本报错：" + errors.join("；"));
console.log("OK：悬浮显示 " + want.join("、") + "，移开后消失，没有脚本错误");
chrome.kill("SIGKILL");
process.exit(0);
