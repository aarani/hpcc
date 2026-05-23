/* ───────── tweaks defaults (host-rewritable) ───────── */
const TWEAK_DEFAULTS = /*EDITMODE-BEGIN*/{
  "accent": "lime",
  "theme": "dark",
  "grid": "on"
}/*EDITMODE-END*/;


/* ───────── tweaks ───────── */
const ACCENT_MAP = {
  amber: { a: "oklch(0.80 0.135 75)", d: "oklch(0.62 0.135 75)", h: 75 },
  cyan:  { a: "oklch(0.78 0.13 215)", d: "oklch(0.60 0.13 215)", h: 215 },
  lime:  { a: "oklch(0.82 0.16 130)", d: "oklch(0.62 0.16 130)", h: 130 },
  rose:  { a: "oklch(0.72 0.16 15)",  d: "oklch(0.55 0.16 15)",  h: 15 },
};

const tweaks = { ...TWEAK_DEFAULTS };

function applyTweaks() {
  const a = ACCENT_MAP[tweaks.accent] || ACCENT_MAP.amber;
  document.documentElement.style.setProperty("--accent", a.a);
  document.documentElement.style.setProperty("--accent-d", a.d);
  document.documentElement.style.setProperty("--accent-bg", `oklch(${tweaks.theme === "light" ? "0.55" : "0.80"} 0.135 ${a.h} / ${tweaks.theme === "light" ? "0.10" : "0.12"})`);
  document.documentElement.classList.toggle("light", tweaks.theme === "light");
  document.documentElement.style.setProperty("--grid-vis", tweaks.grid === "on" ? "0.6" : "0.0");
  document.body.classList.toggle("show-grid", tweaks.grid === "on");

  document.querySelectorAll("[data-accent]").forEach(b => b.setAttribute("aria-selected", b.dataset.accent === tweaks.accent ? "true" : "false"));
  document.querySelectorAll("[data-theme]").forEach(b => b.setAttribute("aria-selected", b.dataset.theme === tweaks.theme ? "true" : "false"));
  document.querySelectorAll("[data-grid]").forEach(b => b.setAttribute("aria-selected", b.dataset.grid === tweaks.grid ? "true" : "false"));
}

function setTweak(k, v) {
  tweaks[k] = v;
  applyTweaks();
  try {
    window.parent.postMessage({ type: "__edit_mode_set_keys", edits: { [k]: v } }, "*");
  } catch (e) {}
}

document.querySelectorAll("[data-accent]").forEach(b => b.addEventListener("click", () => setTweak("accent", b.dataset.accent)));
document.querySelectorAll("[data-theme]").forEach(b => b.addEventListener("click", () => setTweak("theme", b.dataset.theme)));
document.querySelectorAll("[data-grid]").forEach(b => b.addEventListener("click", () => setTweak("grid", b.dataset.grid)));

const twClose = document.getElementById("tw-close");
if (twClose) {
  twClose.addEventListener("click", () => {
    document.getElementById("tweaks").setAttribute("data-open", "false");
    try { window.parent.postMessage({ type: "__edit_mode_dismissed" }, "*"); } catch (e) {}
  });
}

window.addEventListener("message", (e) => {
  if (!e.data) return;
  const t = document.getElementById("tweaks");
  if (!t) return;
  if (e.data.type === "__activate_edit_mode") {
    t.setAttribute("data-open", "true");
  } else if (e.data.type === "__deactivate_edit_mode") {
    t.setAttribute("data-open", "false");
  }
});

try { window.parent.postMessage({ type: "__edit_mode_available" }, "*"); } catch (e) {}

applyTweaks();

const gridStyle = document.createElement("style");
gridStyle.textContent = `body.show-grid::before {
  content: ""; position: fixed; inset: 0; pointer-events: none; z-index: 0;
  background-image:
    linear-gradient(to right, var(--line) 1px, transparent 1px),
    linear-gradient(to bottom, var(--line) 1px, transparent 1px);
  background-size: 80px 80px;
  opacity: 0.4;
}`;
document.head.appendChild(gridStyle);

/* ───────── copy-to-clipboard buttons (hero install one-liner, etc.) ───────── */
document.querySelectorAll(".copy-btn").forEach(btn => {
  btn.addEventListener("click", async () => {
    const target = document.getElementById(btn.dataset.copy);
    if (!target) return;
    try {
      await navigator.clipboard.writeText(target.textContent.trim());
      const prev = btn.textContent;
      btn.textContent = "copied";
      setTimeout(() => { btn.textContent = prev; }, 1400);
    } catch {
      // navigator.clipboard requires HTTPS or localhost — silently fall
      // back to selecting the text so the user can ⌘C / Ctrl-C.
      const sel = window.getSelection();
      const range = document.createRange();
      range.selectNodeContents(target);
      sel.removeAllRanges();
      sel.addRange(range);
    }
  });
});
