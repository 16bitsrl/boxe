// Copy buttons: hidden without JavaScript, where the command stays readable.
for (const b of document.querySelectorAll("button.copy")) {
  b.hidden = false;
  b.addEventListener("click", async () => {
    const label = b.textContent;
    try {
      await navigator.clipboard.writeText(b.dataset.copy);
      b.textContent = "Copied";
    } catch {
      b.textContent = "Copy failed";
    }
    setTimeout(() => (b.textContent = label), 1500);
  });
}

// Charts: a hairline and the value at the pointer, at the same time in
// every chart of the group; arrow keys move it too.
for (const group of document.querySelectorAll(".charts")) {
  const times = group.dataset.times.split(",");
  const charts = [...group.querySelectorAll(".chart")].map((chart) => {
    const plot = chart.querySelector(".plot");
    const cross = document.createElement("div");
    const tip = document.createElement("div");
    cross.className = "cross";
    tip.className = "tip";
    cross.hidden = tip.hidden = true;
    plot.append(cross, tip);
    plot.tabIndex = 0;
    return { chart, plot, cross, tip, values: chart.dataset.values.split("|") };
  });
  let at = -1;
  const show = (i) => {
    at = Math.max(0, Math.min(times.length - 1, i));
    const when = new Date(times[at] * 1000).toLocaleString([], { month: "short", day: "numeric", hour: "2-digit", minute: "2-digit" });
    for (const c of charts) {
      const x = ((at + 0.5) / times.length) * c.plot.clientWidth;
      c.cross.style.left = x + "px";
      const b = document.createElement("b");
      b.textContent = c.values[at] || "nothing";
      c.tip.replaceChildren(b, when);
      c.cross.hidden = c.tip.hidden = false;
      c.chart.classList.add("reading");
      const w = c.tip.offsetWidth;
      c.tip.style.left = Math.max(0, Math.min(c.plot.clientWidth - w, x - w / 2)) + "px";
    }
  };
  const hide = () => {
    for (const c of charts) {
      c.cross.hidden = c.tip.hidden = true;
      c.chart.classList.remove("reading");
    }
  };
  for (const c of charts) {
    c.plot.addEventListener("pointermove", (e) => {
      const r = c.plot.getBoundingClientRect();
      show(Math.floor(((e.clientX - r.left) / r.width) * times.length));
    });
    c.plot.addEventListener("pointerleave", hide);
    c.plot.addEventListener("blur", hide);
    c.plot.addEventListener("focus", () => show(at < 0 ? times.length - 1 : at));
    c.plot.addEventListener("keydown", (e) => {
      const step = { ArrowLeft: -1, ArrowRight: 1 }[e.key];
      if (step) {
        e.preventDefault();
        show(at + step);
      }
    });
  }
}

// The console opens at its end, where the latest lines are.
for (const pre of document.querySelectorAll("pre.console")) pre.scrollTop = pre.scrollHeight;
