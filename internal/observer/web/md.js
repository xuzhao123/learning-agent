import { render } from "./render.js";
import { $, el, esc, ui } from "./util.js";
// ---------- 对话流：从同一批模型请求还原用户消息、思考、工具和回答 ----------
// 只渲染少量 Markdown：先转义再替换，模型输出不能注入 HTML。
// visual 为真时（对话里的模型回答），visualization skill 的 ```echarts 与 ```html type="renderer" 块换成 vizSlot，由 renderChat 放入沙箱 iframe。
// 链接只认 http(s)：[文字](地址) 与裸地址，新标签页打开。匹配的是转义后的文本：地址只取 ASCII 的 URL 字符，
// & 只认 &amp;（&quot;、&lt;、&gt; 是原文的引号与尖括号，到这里截断），紧跟的中文不算进去；允许一层括号（如维基百科的地址）。
const MD_URL = String.raw`https?:\/\/(?:[A-Za-z0-9\-._~:\/?#@!$'*+,;=%]|&amp;|\((?:[A-Za-z0-9\-._~:\/?#@!$'*+,;=%]|&amp;)*\))+`;
const MD_LINK = new RegExp(String.raw`\[([^\]\n]+)\]\((${MD_URL})\)|&lt;(${MD_URL})&gt;|(${MD_URL})`, "g"); // 第二种是 Markdown 的 <地址> 写法
const mdAnchor = (href, text) => `<a href="${href}" target="_blank" rel="noopener noreferrer">${text}</a>`;
// 数学公式：\( … \)、$ … $ 是行内公式，\[ … \]、$$ … $$ 是独立公式（可以独占几行）。单个 $ 照 Pandoc 的规则：
// 开头的 $ 后面、结尾的 $ 前面都不能是空白，结尾的 $ 后面不能紧跟数字，“$5 和 $10”这样的金额不会被当成公式。
// KaTeX 还没加载好或被拦下时按源码显示，加载完重画一次。
const MD_MATH = /\\\((.+?)\\\)|\\\[(.+?)\\\]|\$\$(.+?)\$\$|\$(?=\S)([^$\n]*?\S)\$(?!\d)/g;
function tex(src, display) {
  try {
    if (window.katex) return window.katex.renderToString(src, { displayMode: display, throwOnError: false });
  } catch {}
  return display ? `<pre><code>${esc(src)}</code></pre>` : `<code>${esc(src)}</code>`;
}
export function mathLoaded() { if (ui.run) render(); }
export function md(text, visual) {
  const lines = String(text || "").split("\n"), out = [];
  // 行内代码和公式先换成占位符：里面的地址不变成链接、* 不变成粗体，代码两边的 ** 仍能配成粗体。
  // 行内代码照 CommonMark：两边是一样长的一串反引号，里面可以有更短的反引号（`` `x` ``）；内容两头都是空格时各去掉一个。
  const inline = (s) => {
    const codes = [], maths = [];
    s = String(s).replace(/(?<!`)(`+)(?!`)(.+?)(?<!`)\1(?!`)/g, (_, tick, code) => `${codes.push(/^ .*[^ ].* $/.test(code) ? code.slice(1, -1) : code) - 1}`)
      .replace(MD_MATH, (_, a, b, c, d) => `\uE001${maths.push([a ?? b ?? c ?? d, b !== undefined || c !== undefined]) - 1}\uE001`);
    return esc(s)
      .replace(MD_LINK, (_, label, href, angled, bare) => {
        if (label) return mdAnchor(href, label);
        if (angled) return mdAnchor(angled, angled);
        const url = bare.replace(/[.,;:!?]+$/, ""); // 句末的英文标点留在链接外
        return mdAnchor(url, url) + bare.slice(url.length);
      })
      .replace(/\*\*([^*]+)\*\*/g, "<strong>$1</strong>")
      .replace(/(\d+)/g, (_, n) => `<code>${esc(codes[n])}</code>`)
      .replace(/\uE001(\d+)\uE001/g, (_, n) => tex(...maths[n]));
  };
  const block = /^(```|#{1,6}\s|\s*([-*]|\d+\.)\s|\||\s*(\\\[|\$\$)|\s*(-{3,}|\*{3,}|_{3,})\s*$)/;
  for (let i = 0; i < lines.length;) {
    const line = lines[i];
    if (line.startsWith("```")) {
      // 围栏照 CommonMark：结束行只有反引号、不比开头短，所以 ```` 围起来的块里可以写 ``` 开头的示例。
      const fence = line.match(/^`+/)[0], code = [];
      for (i++; i < lines.length && !(/^`+$/.test(lines[i].trim()) && lines[i].trim().length >= fence.length); i++) code.push(lines[i]);
      const closed = i++ < lines.length, info = line.slice(fence.length).trim(); // 流式输出中未闭合的块先按代码显示
      const kind = info === "echarts" ? "echarts" : /^html\b.*\btype=["']?renderer\b/.test(info) ? "renderer" : "";
      if (visual && closed && kind) out.push(vizSlot(kind, code.join("\n")));
      else out.push(`<pre><code>${esc(code.join("\n"))}</code></pre>`);
    } else if (/^\s*(\\\[|\$\$)/.test(line)) {
      // 公式块：\[ 或 $$ 开头，到对应的 \] 或 $$ 为止（可以在同一行）；流式输出中还没闭合的先按源码显示。
      const open = line.trim().slice(0, 2), close = open === "$$" ? "$$" : "\\]";
      let body = line.trim().slice(2), j = i;
      while (!body.includes(close) && ++j < lines.length) body += "\n" + lines[j];
      const end = body.indexOf(close), rest = end < 0 ? "" : body.slice(end + 2).trim();
      i = j + 1;
      out.push(end < 0 ? `<pre><code>${esc(open + body)}</code></pre>` : tex(body.slice(0, end), true));
      if (rest) out.push(`<p>${inline(rest)}</p>`);
    } else if (/^\s*(-{3,}|\*{3,}|_{3,})\s*$/.test(line)) {
      out.push("<hr>"); i++;
    } else if (/^#{1,6}\s/.test(line)) {
      out.push(`<h4>${inline(line.replace(/^#+\s*/, ""))}</h4>`); i++;
    } else if (/^\s*([-*]|\d+\.)\s/.test(line)) {
      const tag = /^\s*\d/.test(line) ? "ol" : "ul", items = [];
      for (; i < lines.length && /^\s*([-*]|\d+\.)\s/.test(lines[i]); i++) items.push(`<li>${inline(lines[i].replace(/^\s*([-*]|\d+\.)\s/, ""))}</li>`);
      out.push(`<${tag}>${items.join("")}</${tag}>`);
    } else if (line.startsWith("|")) {
      const rows = [];
      for (; i < lines.length && lines[i].startsWith("|"); i++) {
        if (/^\|[\s:|-]+\|?\s*$/.test(lines[i])) continue;
        const cells = lines[i].trim().replace(/^\||\|$/g, "").split("|").map((c) => inline(c.trim()));
        const tag = rows.length ? "td" : "th";
        rows.push("<tr>" + cells.map((c) => `<${tag}>${c}</${tag}>`).join("") + "</tr>");
      }
      out.push(`<table>${rows.join("")}</table>`);
    } else if (!line.trim()) i++;
    else {
      const para = [];
      for (; i < lines.length && lines[i].trim() && !block.test(lines[i]); i++) para.push(inline(lines[i]));
      out.push(`<p>${para.join("<br>")}</p>`);
    }
  }
  const box = el("div", "md");
  box.innerHTML = out.join("");
  return box;
}
// ---------- 可视化：模型写的 HTML 与 ECharts option 只在沙箱 iframe 里运行 ----------
// sandbox 只给 allow-scripts、不给 allow-same-origin：iframe 是独立的空源，读不到观测台的 cookie、页面和接口。
// renderChat 每个事件都重建 DOM，iframe 一旦离开文档就会重载、丢掉交互状态；所以重建前先 moveBefore 停到 #viz-park，
// 重建后按内容认领回原位。不支持 moveBefore 的浏览器退化为重载。
const vizFrames = new Map(), vizPending = [];
const VIZ_HEIGHT = `<script>new ResizeObserver(function(){parent.postMessage({vizHeight:Math.ceil(document.documentElement.getBoundingClientRect().height)},"*")}).observe(document.documentElement)<\/script>`;
function vizSrc(kind, code) {
  if (kind === "renderer") return code + VIZ_HEIGHT;
  // echarts 块是 option 对象字面量，可能含 ES5 callback，不是 JSON；在 iframe 内求值。
  return `<!doctype html><meta charset="utf-8"><style>html,body{margin:0}#c{height:420px}</style><div id="c"></div>
<script src="https://cdn.jsdelivr.net/npm/echarts@5.5.1/dist/echarts.min.js"><\/script>
<script>try{if(typeof echarts==="undefined")throw new Error("ECharts 库加载失败");var chart=echarts.init(document.getElementById("c"));chart.setOption(new Function("return ("+${JSON.stringify(code).replace(/</g, "\\u003c")}+")")());addEventListener("resize",function(){chart.resize()})}catch(e){document.body.textContent="图表渲染失败："+e.message}<\/script>`;
}
function vizSlot(kind, code) {
  vizPending.push({ kind, code });
  return `<div class="viz-slot" data-viz="${vizPending.length - 1}"></div>`;
}
function vizMove(parent, node, before) {
  if (node.isConnected && parent.moveBefore) parent.moveBefore(node, before); else parent.insertBefore(node, before);
}
export function parkViz() {
  for (const f of vizFrames.values()) if (f.isConnected) vizMove($("viz-park"), f, null);
}
export function claimViz() {
  const used = new Set();
  for (const slot of document.querySelectorAll(".viz-slot")) {
    const { kind, code } = vizPending[slot.dataset.viz], key = kind + "\n" + code;
    let f = vizFrames.get(key);
    if (!f || used.has(key)) {
      f = el("iframe", "viz");
      f.sandbox = "allow-scripts";
      f.style.height = kind === "echarts" ? "420px" : "160px";
      f.srcdoc = vizSrc(kind, code);
      if (!used.has(key)) vizFrames.set(key, f);
    }
    used.add(key);
    vizMove(slot.parentNode, f, slot);
    slot.remove();
  }
  vizPending.length = 0;
  // 没被认领的先留在停放区：续聊时对话会重新加载，中间有几次渲染还看不到旧回答。只在超过 20 个时删最旧的。
  for (const [key, f] of vizFrames) if (vizFrames.size > 20 && !used.has(key)) { f.remove(); vizFrames.delete(key); }
}
addEventListener("message", (e) => {
  const h = e.data?.vizHeight;
  if (typeof h !== "number") return;
  for (const f of document.querySelectorAll("iframe.viz")) if (f.contentWindow === e.source) f.style.height = Math.min(Math.max(h, 40), 4000) + "px";
});
