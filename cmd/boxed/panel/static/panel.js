// Copy buttons: hidden without JavaScript, where the command stays readable.
const copyButtons = (root) => {
  for (const b of root.querySelectorAll("button.copy")) {
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
};

// The command of a form, as the fields change: the words of its fields
// with data-arg (a flag, or "" for a word of its own), unless empty or at
// their default, quoted as a shell wants them (as cmdLine in panel.go).
const quote = (w) => (/^[A-Za-z0-9._\/:=@%+-]+$/.test(w) ? w : "'" + w.replaceAll("'", "'\\''") + "'");
for (const cmd of document.querySelectorAll(".cmd[data-cmd]")) {
  const form = cmd.closest("form");
  const line = cmd.querySelector("code").lastChild;
  const copy = cmd.querySelector(".copy");
  const update = () => {
    const words = [cmd.dataset.cmd];
    for (const f of form.querySelectorAll("[data-arg]")) {
      const v = f.value.trim();
      if (v === "" || v === f.dataset.default) continue;
      if (f.dataset.arg) words.push(f.dataset.arg);
      words.push(quote(v));
    }
    line.textContent = " " + words.join(" ");
    copy.dataset.copy = words.join(" ");
  };
  form.addEventListener("input", update);
  form.addEventListener("change", update);
}

// Charts: a hairline and the value at the pointer, at the same time in
// every chart of the group; arrow keys move it too.
const charts = (root) => {
  for (const group of root.querySelectorAll(".charts")) {
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
};

copyButtons(document);
charts(document);

// The console opens at its end, where the latest lines are.
for (const pre of document.querySelectorAll("pre.console")) pre.scrollTop = pre.scrollHeight;

// Live pages (data-poll) read themselves again every 10 seconds, the
// console every 3 while it follows, and swap in the elements with data-live.
// One stays as it is while it is read: a chart under the pointer or the
// keyboard, a focused field or link, selected text. Polling waits while the
// tab is hidden, and stops for good when the page is no longer there as it
// was: logged out (a redirect to the login), the VM deleted, an error.
const poll = document.querySelector("[data-poll]");
const follow = document.querySelector("input[data-follow]");
if (follow) follow.closest("label").hidden = false;
const every = () => (follow?.checked ? 3000 : poll ? 10000 : 0);
let timer = 0, running = false, stopped = false;

const busy = (el) => {
  if (el.querySelector(".chart.reading")) return true;
  const f = document.activeElement;
  if (f && el.contains(f) && f.matches("input, select, textarea, button, a")) return true;
  const s = getSelection();
  return s && !s.isCollapsed && s.containsNode(el, true);
};

// The console gets its new text in place, which keeps its focus and scroll;
// it stays at the end if it was there.
const swap = (old, now) => {
  const pre = old.querySelector("pre.console");
  const text = now.querySelector("pre.console");
  if (pre && text) {
    const end = pre.scrollTop + pre.clientHeight >= pre.scrollHeight - 4;
    if (pre.textContent !== text.textContent) pre.textContent = text.textContent;
    if (end) pre.scrollTop = pre.scrollHeight;
    return;
  }
  old.replaceWith(now);
  copyButtons(now);
  charts(now);
  for (const p of now.querySelectorAll("pre.console")) p.scrollTop = p.scrollHeight;
};

const refresh = async () => {
  let res;
  try {
    res = await fetch(location.href, { redirect: "manual", cache: "no-store", headers: { Accept: "text/html" } });
  } catch {
    return true; // offline for a moment
  }
  if (res.type !== "basic" || res.status !== 200 || res.redirected) return false;
  const doc = new DOMParser().parseFromString(await res.text(), "text/html");
  const fresh = doc.querySelectorAll("[data-live]");
  if (!fresh.length) return false;
  for (const now of fresh) {
    const old = document.querySelector(`[data-live="${CSS.escape(now.dataset.live)}"]`);
    if (old && !busy(old)) swap(old, document.importNode(now, true));
  }
  return true;
};

const schedule = () => {
  clearTimeout(timer);
  if (!stopped && !running && every() && !document.hidden) timer = setTimeout(tick, every());
};
const tick = async () => {
  if (running || stopped) return;
  clearTimeout(timer);
  running = true;
  if (!(await refresh())) {
    stopped = true;
    if (follow) {
      follow.checked = false;
      follow.disabled = true;
    }
  }
  running = false;
  schedule();
};
document.addEventListener("visibilitychange", () => (document.hidden ? clearTimeout(timer) : every() && tick()));
follow?.addEventListener("change", () => (follow.checked ? tick() : schedule()));
schedule();
