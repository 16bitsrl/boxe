// Two small things: the command tabs of the terminal, and the copy buttons.
// Without this script every transcript is shown and the copy buttons stay hidden.

const tabs = [...document.querySelectorAll('.demo-tabs [role="tab"]')];

function select(tab) {
  for (const t of tabs) {
    const on = t === tab;
    t.setAttribute('aria-selected', on);
    t.tabIndex = on ? 0 : -1;
    document.getElementById(t.getAttribute('aria-controls')).classList.toggle('on', on);
  }
}

tabs.forEach((tab, i) => {
  tab.addEventListener('click', () => select(tab));
  tab.addEventListener('keydown', (e) => {
    const step = { ArrowRight: 1, ArrowDown: 1, ArrowLeft: -1, ArrowUp: -1 }[e.key];
    let next = null;
    if (step) next = tabs[(i + step + tabs.length) % tabs.length];
    if (e.key === 'Home') next = tabs[0];
    if (e.key === 'End') next = tabs[tabs.length - 1];
    if (next) {
      e.preventDefault();
      select(next);
      next.focus();
    }
  });
});

async function copyText(text) {
  try {
    await navigator.clipboard.writeText(text);
    return true;
  } catch {
    // file:// and older browsers: fall back to a hidden textarea.
    const area = document.createElement('textarea');
    area.value = text;
    area.setAttribute('readonly', '');
    area.style.position = 'fixed';
    area.style.opacity = '0';
    document.body.append(area);
    area.select();
    const ok = document.execCommand('copy');
    area.remove();
    return ok;
  }
}

for (const button of document.querySelectorAll('.copy')) {
  button.addEventListener('click', async () => {
    const ok = await copyText(button.dataset.copy);
    if (!ok) {
      // Leave the command selected, so the visitor can copy it by hand.
      const range = document.createRange();
      range.selectNodeContents(button.parentElement.querySelector('pre'));
      getSelection().removeAllRanges();
      getSelection().addRange(range);
    }
    button.textContent = ok ? 'Copied' : 'Selected';
    button.classList.toggle('done', ok);
    clearTimeout(button.timer);
    button.timer = setTimeout(() => {
      button.textContent = 'Copy';
      button.classList.remove('done');
    }, 1600);
  });
}
