// 最近一次模型请求的上下文用量（真实页面里来自服务端的 usage）。
const usage = { used: 151234, capacity: 828000, cached: 112000 };

document.getElementById("composer").addEventListener("submit", (ev) => {
  ev.preventDefault();
  const q = document.getElementById("query");
  if (!q.value.trim()) return;
  const m = document.createElement("div");
  m.className = "msg";
  m.textContent = q.value.trim();
  document.getElementById("thread").append(document.createElement("br"), m);
  q.value = "";
});
