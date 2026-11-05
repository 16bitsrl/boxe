// The web terminal: ghostty-web in the page, a WebSocket to boxed, a scroll
// session in the VM. Binary messages are the terminal's bytes both ways,
// text messages are commands to boxed (resize). The session lives on in the
// VM, so a dropped connection just reconnects to it.
import { Ghostty, Terminal, FitAddon } from "./ghostty-web.js";

const params = new URLSearchParams(location.search);
const session = params.get("s") || "main";
const vm = location.hostname.split(".")[0].replace(/--term$/, "");
document.title = vm + (session === "main" ? "" : " · " + session);

const status = document.getElementById("status");
function say(html) {
  status.hidden = !html;
  status.innerHTML = html || "";
}

const ghostty = await Ghostty.load("ghostty-vt.wasm");
const term = new Terminal({
  ghostty,
  fontSize: 14,
  fontFamily: "ui-monospace, SFMono-Regular, Menlo, monospace",
  cursorBlink: true,
  scrollback: 10000,
  theme: { background: "#201c1b", foreground: "#fff7ed", cursor: "#f3c544" },
});
const fit = new FitAddon();
term.loadAddon(fit);
term.open(document.getElementById("term"));
fit.fit();
fit.observeResize();
term.focus();
term.onTitleChange((t) => (document.title = t || vm));

const enc = new TextEncoder();
let ws = null;
let ended = false; // the shell exited: Enter starts a new one
let failures = 0;

term.onData((d) => {
  if (ended) {
    if (d === "\r") connect();
    return;
  }
  if (ws && ws.readyState === WebSocket.OPEN) ws.send(enc.encode(d));
});
term.onResize(({ cols, rows }) => {
  if (ws && ws.readyState === WebSocket.OPEN) ws.send(JSON.stringify({ type: "resize", cols, rows }));
});

function connect() {
  ended = false;
  const q = new URLSearchParams({ s: session, cols: term.cols, rows: term.rows });
  const scheme = location.protocol === "https:" ? "wss" : "ws";
  const sock = new WebSocket(`${scheme}://${location.host}/ws?${q}`);
  sock.binaryType = "arraybuffer";
  ws = sock;
  let opened = false;
  sock.onopen = () => {
    opened = true;
    failures = 0;
    say("");
    // The size may have changed while connecting.
    sock.send(JSON.stringify({ type: "resize", cols: term.cols, rows: term.rows }));
  };
  sock.onmessage = (e) => term.write(new Uint8Array(e.data));
  sock.onclose = async (e) => {
    if (ws !== sock) return;
    if (e.code === 1000 && e.reason === "exited") {
      ended = true;
      term.write("\r\n\x1b[2m[the shell exited: press Enter to start a new one]\x1b[0m\r\n");
      return;
    }
    failures = opened ? 1 : failures + 1;
    // After a few failures, see whether the login expired: then the page
    // itself sends to the login.
    if (failures >= 3) {
      try {
        const r = await fetch("/", { redirect: "manual", cache: "no-store" });
        if (r.type === "opaqueredirect" || r.status === 403) {
          say(r.status === 403 ? "No access to this terminal." : 'Logged out. <a href="">Log in again</a>');
          return;
        }
      } catch {}
    }
    const wait = Math.min(10, 2 ** Math.max(0, failures - 1));
    say(`Disconnected, reconnecting in ${wait} s…`);
    setTimeout(() => {
      if (ws === sock) connect();
    }, wait * 1000);
  };
}

// Back from a sleeping laptop or a hidden tab: reconnect at once.
document.addEventListener("visibilitychange", () => {
  if (!document.hidden && ws && ws.readyState === WebSocket.CLOSED && !ended) connect();
});

connect();
