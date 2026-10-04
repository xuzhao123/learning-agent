// 课件框架：翻页、分步动画、自动播放、目录、键盘与触摸。
// 每页的 data-step="n" 元素在第 n 步出现，data-until="m" 在第 m 步之后隐藏，data-mark="n" 在第 n 步加上 .marked。
(function () {
  const stage = document.querySelector(".stage");
  const slides = [...document.querySelectorAll(".slide")];
  const bar = document.querySelector(".bar");
  const where = bar.querySelector(".where");
  const title = bar.querySelector(".title");
  const dots = bar.querySelector(".dots");
  const playBtn = bar.querySelector("[data-act=play]");
  const tocBtn = bar.querySelector("[data-act=toc]");
  const progress = document.querySelector(".progress");
  const toc = document.querySelector(".toc");
  let index = 0, step = 0, timer = null;

  // 一页的最后一步 = 所有 data-step、data-until、data-mark 中的最大值
  const last = (slide) => Math.max(0, ...[...slide.querySelectorAll("[data-step],[data-mark]")]
    .flatMap((el) => [el.dataset.step, el.dataset.until, el.dataset.mark])
    .filter((v) => v !== undefined).map(Number));

  function fit() {
    const box = document.querySelector(".deck").getBoundingClientRect();
    const scale = Math.min((box.width - 16) / 1280, (box.height - 16) / 720);
    stage.style.setProperty("--scale", Math.max(scale, 0.1));
  }

  function render() {
    slides.forEach((s, k) => s.classList.toggle("active", k === index));
    const slide = slides[index];
    slide.querySelectorAll("[data-step]").forEach((el) => {
      const from = Number(el.dataset.step);
      const until = el.dataset.until ? Number(el.dataset.until) : Infinity;
      const on = step >= from && step <= until;
      if (el.classList.contains("on") !== on) {
        el.classList.toggle("on", on);
        // travel 动画每次出现都要重播
        if (on && el.dataset.fx === "travel") { el.style.animation = "none"; void el.offsetWidth; el.style.animation = ""; }
      }
    });
    slide.querySelectorAll("[data-mark]").forEach((el) => el.classList.toggle("marked", step >= Number(el.dataset.mark)));
    const total = last(slide);
    where.textContent = `${index + 1} / ${slides.length}`;
    title.textContent = slide.dataset.title || "";
    dots.innerHTML = "";
    for (let i = 1; i <= total; i++) {
      const d = document.createElement("i");
      if (i <= step) d.className = "on";
      dots.appendChild(d);
    }
    progress.style.width = `${((index + (total ? step / (total + 1) : 0)) / Math.max(1, slides.length - 1)) * 100}%`;
    try { history.replaceState(null, "", `#s${index + 1}`); } catch (e) { /* 预览环境可能不允许 */ }
    toc.querySelectorAll("button").forEach((b, k) => b.classList.toggle("cur", k === index));
  }

  function go(i, s) {
    index = Math.max(0, Math.min(slides.length - 1, i));
    step = s === "end" ? last(slides[index]) : (s || 0);
    render();
  }
  function next() {
    if (step < last(slides[index])) { step++; render(); return true; }
    if (index < slides.length - 1) { go(index + 1, 0); return true; }
    return false;
  }
  function prev() {
    if (step > 0) { step--; render(); return; }
    if (index > 0) go(index - 1, "end");
  }
  function play(on) {
    clearInterval(timer); timer = null;
    playBtn.setAttribute("aria-pressed", on ? "true" : "false");
    playBtn.textContent = on ? "暂停" : "自动播放";
    if (on) timer = setInterval(() => { if (!next()) play(false); }, 2600);
  }
  function stopAuto() { if (timer) play(false); }

  // 目录
  slides.forEach((s, k) => {
    const b = document.createElement("button");
    b.type = "button";
    b.innerHTML = `<span class="n">${k + 1}</span><span></span>`;
    b.lastChild.textContent = s.dataset.title || "";
    b.addEventListener("click", () => { stopAuto(); go(k, 0); toc.hidden = true; tocBtn.setAttribute("aria-pressed", "false"); });
    toc.appendChild(b);
  });

  bar.addEventListener("click", (e) => {
    const act = e.target.closest("button")?.dataset.act;
    if (act === "prev") { stopAuto(); prev(); }
    if (act === "next") { stopAuto(); next(); }
    if (act === "play") play(!timer);
    if (act === "toc") { toc.hidden = !toc.hidden; tocBtn.setAttribute("aria-pressed", String(!toc.hidden)); }
    if (act === "full") {
      const el = document.documentElement;
      (document.fullscreenElement ? document.exitFullscreen() : el.requestFullscreen?.())?.catch?.(() => {});
    }
  });
  document.addEventListener("keydown", (e) => {
    if (e.target.closest?.("input, textarea")) return;
    if (["ArrowRight", "PageDown", " ", "Enter"].includes(e.key)) { e.preventDefault(); stopAuto(); next(); }
    else if (["ArrowLeft", "PageUp", "Backspace"].includes(e.key)) { e.preventDefault(); stopAuto(); prev(); }
    else if (e.key === "Home") { stopAuto(); go(0, 0); }
    else if (e.key === "End") { stopAuto(); go(slides.length - 1, "end"); }
    else if (e.key === "p" || e.key === "P") play(!timer);
    else if (e.key === "f" || e.key === "F") bar.querySelector("[data-act=full]").click();
  });
  // 点击舞台：右侧前进，左侧后退
  stage.addEventListener("click", (e) => {
    if (e.target.closest("a, button")) return;
    const r = stage.getBoundingClientRect();
    stopAuto();
    if (e.clientX > r.left + r.width * 0.3) next(); else prev();
  });
  let touchX = null;
  stage.addEventListener("touchstart", (e) => { touchX = e.touches[0].clientX; }, { passive: true });
  stage.addEventListener("touchend", (e) => {
    if (touchX === null) return;
    const dx = e.changedTouches[0].clientX - touchX;
    touchX = null;
    if (Math.abs(dx) > 40) { stopAuto(); dx < 0 ? next() : prev(); }
  });

  window.addEventListener("resize", fit);
  fit();
  const m = /^#s(\d+)$/.exec(location.hash);
  go(m ? Number(m[1]) - 1 : 0, 0);
})();
