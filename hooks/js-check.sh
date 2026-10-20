#!/bin/sh
# PostToolUse（edit_file、write_file）：改过的 .js 或 .html 里的脚本有语法错误时把错误交给模型。
# 页面是 embed 进 Go 程序的，go build 查不出脚本错误；浏览器里一处语法错误会让整段脚本都不运行。宿主机没有 node 时跳过。
path=$(jq -r '.tool_input.path // empty')
case "$path" in *.js|*.mjs|*.html) ;; *) exit 0 ;; esac
cd "$AGENT_PROJECT_DIR" 2>/dev/null && [ -f "$path" ] || exit 0
command -v node >/dev/null 2>&1 || exit 0
case "$path" in
*.html)
  # 逐段检查 <script>（跳过带 src 的、module 和数据块），报错的行号按 HTML 文件算。
  out=$(node -e '
const fs = require("fs"), vm = require("vm"), file = process.argv[1], html = fs.readFileSync(file, "utf8");
for (const m of html.matchAll(/<script\b([^>]*)>([\s\S]*?)<\/script>/gi)) {
  const attrs = m[1];
  if (/\bsrc\s*=/i.test(attrs) || (/\btype\s*=/i.test(attrs) && !/type\s*=\s*["\x27]?(text|application)\/javascript/i.test(attrs))) continue;
  const line = html.slice(0, m.index + m[0].indexOf(">") + 1).split("\n").length - 1;
  try { new vm.Script(m[2], { filename: file, lineOffset: line }); }
  catch (e) { console.log(e.stack.split("\n    at ")[0]); process.exitCode = 1; }
}' "$path" 2>&1) ;;
*) out=$(node --check "$path" 2>&1) ;;
esac
[ $? -eq 0 ] && exit 0
jq -n --arg r "$path 的 JavaScript 有语法错误（浏览器加载时这段脚本整段不会运行），先修好：
$(printf '%s' "$out" | sed "s|$PWD/||" | grep -v '^    at \|^Node\.js v' | head -n 20)" '{decision: "block", reason: $r}'
