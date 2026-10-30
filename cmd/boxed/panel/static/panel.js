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
