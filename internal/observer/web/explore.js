import { parse } from "./util.js";
// 对话页把连续的“看代码”调用合成一行“已探索”（参考 Codex 的 Explored 与 Claude Code 的折叠工具行），逐条记录点开再看。
// 只认得出的只读调用：专用的读、搜、列工具，和每一段都是只读命令的 bash；认不出的照常单独显示。
const TOOLS = { read_file: "read", view_image: "read", read_output: "look", grep: "search", glob: "list" }; // read_output 的名字是输出编号，不是文件
const READ = new Set(["cat", "head", "tail", "nl", "sed"]), SEARCH = new Set(["grep", "egrep", "rg"]), LIST = new Set(["ls", "find", "tree"]);
const LOOK = new Set(["wc", "stat", "file", "du", "pwd", "echo", "sort", "uniq", "cut", "tr", "column", "true"]);
const GIT = new Set(["log", "show", "diff", "status", "grep", "ls-files", "blame"]);
const base = (p) => String(p || "").replace(/^['"]|['"]$/g, "").split("/").filter(Boolean).at(-1) || "";
// 返回 {kind, names}：kind 是 read / search / list / look；不是只读调用时返回 null。
export function exploreKind(t) {
  const args = parse(t.args) || {};
  if (TOOLS[t.name]) return { kind: TOOLS[t.name], names: TOOLS[t.name] === "read" ? [base(args.path || args.name)].filter(Boolean) : [] };
  if (t.name !== "bash" || args.background || typeof args.command !== "string") return null;
  return bashKind(args.command);
}
function bashKind(command) {
  // 命令替换、反引号、heredoc 在抹掉引号内容之前查：它们在双引号里也会执行。
  if (/`|\$\(|<<|\n/.test(command)) return null;
  const cmd = command.replace(/'[^']*'|"(?:[^"\\]|\\.)*"/g, "Q").replace(/\s2>\s*\/dev\/null/g, " ").replace(/\s2>&1/g, " ");
  if (/>|\bxargs\b|\btee\b|-exec\b|-delete\b|-fprint|-fls\b|--output\b|["']/.test(cmd)) return null;
  let kind = "look";
  const names = [];
  for (let seg of cmd.split(/&&|\|\||;|\|/)) {
    seg = seg.trim();
    if (!seg) continue;
    const words = seg.split(/\s+/), head = words[0];
    if (head === "cd") continue;
    // 带输出文件参数的会写文件：sort -o、tree -o、uniq 输入 输出。
    if (((head === "sort" || head === "tree") && words.some((w) => w.startsWith("-o"))) || (head === "uniq" && words.slice(1).filter((w) => !w.startsWith("-")).length > 1)) return null;
    if (head === "git" ? !GIT.has(words[1]) : head === "sed" ? !words.includes("-n") || words.some((w) => w.startsWith("-i")) : !READ.has(head) && !SEARCH.has(head) && !LIST.has(head) && !LOOK.has(head)) return null;
    if (SEARCH.has(head) || (head === "git" && words[1] === "grep")) kind = "search";
    else if (LIST.has(head) && kind !== "search") kind = "list";
    else if (READ.has(head) && kind === "look") kind = "read";
    if (READ.has(head)) {
      const file = words.slice(1).filter((w) => !w.startsWith("-") && w !== "Q" && !/^[\d,$]+p$/.test(w)).at(-1);
      if (file) names.push(base(file));
    }
  }
  return { kind, names };
}
// bash 的退出码。D30 起太长的结果存成文件，模型（和页面）拿到的是带行号的预览，退出码在预览的 result.exit_code 一行里。
export function bashExit(out) {
  if (!out || out.exit_code !== undefined || !out.saved_output) return out?.exit_code;
  const m = /result\.exit_code: (-?\d+)/.exec(out.preview || "");
  return m ? Number(m[1]) : undefined;
}
// 一次请求能并进“已探索”组：已经结束、没有过程说明，每个工具都是只读调用且没出错（出错与失败退出照常单独显示，同 Codex）。
export function exploreCall(r, run, c) {
  const m = c.choice?.message;
  if (c.compact || c.aux || !c.end || !m || m.content || !c.tools.length) return false;
  return c.tools.every((t) => {
    if (!exploreKind(t) || run.approvals[t.id] || r.hookCalls[t.id]?.some((h) => h.outcome === "block")) return false;
    if (t.state === "running") return true;
    if (t.state !== "ok") return false;
    const out = parse(t.result)?.result;
    return t.name !== "bash" || (out && !out.signal && bashExit(out) === 0);
  });
}
// 组的标题：读了哪些文件（同名只算一次，最多列 3 个）、搜索与列目录各几次。
export function exploreLabel(calls) {
  const files = [], count = { search: 0, list: 0, look: 0 };
  for (const c of calls) for (const t of c.tools) {
    const k = exploreKind(t);
    for (const n of k.names) if (!files.includes(n)) files.push(n);
    if (k.kind !== "read") count[k.kind]++;
    else if (!k.names.length) count.look++;
  }
  const parts = [];
  if (files.length) parts.push("读 " + files.slice(0, 3).join("、") + (files.length > 3 ? ` 等 ${files.length} 个文件` : ""));
  if (count.search) parts.push(`搜索 ${count.search} 次`);
  if (count.list) parts.push(`列目录 ${count.list} 次`);
  if (count.look) parts.push(`查看 ${count.look} 次`);
  return parts.join(" · ");
}
