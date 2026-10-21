import "./runs.js";
import "./analysis.js";
import "./render.js";
import "./observe.js";
import "./chat.js";
import "./agents.js";
import "./sandbox.js";
import "./browser.js";
import "./files.js";
import "./composer.js";
import "./eval.js";
import { restoreRunURL } from "./follow.js";
import { mathLoaded } from "./md.js";
import { loadEnabled } from "./pages.js";
import { loadModels, loadSettings, syncComposer } from "./shell.js";
import { $, ui } from "./util.js";
window.addEventListener("popstate", restoreRunURL);
// 多租户：会话过期或被注销时，接口返回 401，回到登录页。
const rawFetch = window.fetch.bind(window);
window.fetch = async (...args) => {
  const res = await rawFetch(...args);
  if (res.status === 401) location.href = "/login";
  return res;
};
// 当前账号：显示名字与租户；租户账号隐藏没开通的功能和平台专用入口（评测、归因）。服务端同样会拒绝，这里只是不让人点到。
// 工具开关在设置页按租户策略显示“未开通”；这里只隐藏没开通的入口。
const FEATURE_INPUTS = { skills: ["skills-hub"], mcp: ["mcp-hub"], labs: ["lab", "rag-lab", "security-lab"] };
async function loadMe() {
  const res = await fetch("/me");
  if (!res.ok) return;
  ui.me = await res.json();
  $("avatar").textContent = ui.me.name.slice(0, 1);
  $("whoami").textContent = ui.me.platform ? ui.me.name : `${ui.me.name} · ${ui.me.tenant}`;
  $("whoami").title = ui.me.platform ? "平台账号" : `租户 ${ui.me.tenant}`;
  for (const [feature, ids] of Object.entries(FEATURE_INPUTS)) {
    if (ui.me.features.includes(feature)) continue;
    for (const id of ids) {
      const node = $(id);
      if (!node) continue;
      if (node.type === "checkbox") node.checked = false;
      (node.closest("label") || node).hidden = true;
    }
  }
  if (!ui.me.platform) for (const id of ["evals", "attribute"]) $(id).hidden = true;
  syncComposer();
}
$("logout").onclick = async () => { await fetch("/logout", { method: "POST" }); location.href = "/login"; };
// KaTeX 异步加载，不挡页面启动：模块跑起来时还没到，就等它到了重画一次（之前公式先按源码显示）。
if (!window.katex) $("katex-js").addEventListener("load", mathLoaded);
loadMe();
restoreRunURL();
loadEnabled();
loadModels();
loadSettings();
