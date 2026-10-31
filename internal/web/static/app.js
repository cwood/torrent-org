// Remembers which list sections and rule groups are open across live
// refreshes and reloads; anything never toggled keeps the server's default. A
// rule can appear in both sections, so keys are scoped by section. Storage
// can be unavailable (private windows), so every access is guarded and the
// list falls back to its defaults.
(() => {
  const KEY = "torrent-org:open";

  const load = () => {
    try {
      return JSON.parse(localStorage.getItem(KEY) || "{}");
    } catch {
      return {};
    }
  };
  const save = (state) => {
    try {
      localStorage.setItem(KEY, JSON.stringify(state));
    } catch {}
  };
  const keyOf = (d) => {
    const s = d.closest("[data-section]");
    return s ? `${s.dataset.section}:${d.dataset.key ?? ""}` : null;
  };
  const apply = () => {
    const state = load();
    document.querySelectorAll("#list details").forEach((d) => {
      const open = state[keyOf(d)];
      if (open !== undefined) d.open = open;
    });
  };

  document.addEventListener("toggle", (e) => {
    const d = e.target;
    if (!(d instanceof HTMLDetailsElement)) return;
    const key = keyOf(d);
    if (!key) return;
    const state = load();
    state[key] = d.open;
    save(state);
  }, true);
  document.addEventListener("DOMContentLoaded", apply);
  document.addEventListener("htmx:afterSettle", apply);
  document.addEventListener("htmx:sseMessage", apply);

  if ("serviceWorker" in navigator) {
    navigator.serviceWorker.register("/sw.js").catch(() => {});
  }
})();

// "Add torrent" opens the file dialog directly. The chosen file is handed off
// the same way `torrent-org open` does it, so the preview gets a URL that
// survives reloads and round trips through the rule editor. Without
// JavaScript the link falls back to the upload page.
(() => {
  const input = document.getElementById("pick-torrent");
  if (!input) return;

  document.addEventListener("click", (e) => {
    const link = e.target.closest("[data-pick-torrent]");
    if (!link) return;
    e.preventDefault();
    e.stopPropagation();
    input.value = "";
    input.click();
  }, true);

  input.addEventListener("change", async () => {
    const file = input.files[0];
    if (!file) return;
    try {
      const res = await fetch("/api/uploads", {
        method: "POST",
        headers: { "Content-Type": "application/x-bittorrent" },
        body: file,
      });
      if (!res.ok) throw new Error((await res.text()).trim());
      const { url } = await res.json();
      window.location.assign(url);
    } catch (err) {
      alert(`Could not open ${file.name}: ${err.message}`);
    }
  });
})();

// Emoji picker for rule names: the chosen emoji replaces the name's leading
// emoji, so a name carries at most one. Graphemes are used so flags, ZWJ
// sequences and skin tones count as a single emoji.
(() => {
  const segmenter = new Intl.Segmenter(undefined, { granularity: "grapheme" });
  const isEmoji = (s) => /\p{Extended_Pictographic}|\p{Regional_Indicator}/u.test(s);

  // split returns the name's leading emoji (or "") and the rest of the name.
  const split = (name) => {
    const first = segmenter.segment(name)[Symbol.iterator]().next().value;
    if (!first || !isEmoji(first.segment)) return ["", name];
    return [first.segment, name.slice(first.segment.length).trimStart()];
  };

  const sync = (input) => {
    const button = document.querySelector(`button[data-emoji-for="${input.id}"]`);
    if (button) button.textContent = split(input.value)[0] || "🙂";
  };
  const syncAll = () => {
    document.querySelectorAll("button[data-emoji-for]").forEach((b) => {
      const input = document.getElementById(b.dataset.emojiFor);
      if (input) sync(input);
    });
  };

  document.addEventListener("emoji-click", (e) => {
    const picker = e.target.closest("emoji-picker[data-emoji-for]");
    const input = picker && document.getElementById(picker.dataset.emojiFor);
    if (!input) return;
    const [, rest] = split(input.value);
    input.value = rest ? `${e.detail.unicode} ${rest}` : `${e.detail.unicode} `;
    sync(input);
    picker.closest("[popover]")?.hidePopover();
    input.focus();
    // Let the "On save" preview react as it does to typing.
    input.dispatchEvent(new Event("input", { bubbles: true }));
  }, true);
  document.addEventListener("input", (e) => {
    if (e.target.matches?.("input[id]")) sync(e.target);
  });
  document.addEventListener("DOMContentLoaded", syncAll);
  document.addEventListener("htmx:afterSettle", syncAll);
})();

// Show-in-folder buttons swap nothing, so a failure would otherwise vanish.
document.addEventListener("htmx:responseError", (e) => {
  if (e.detail.elt.matches?.(".reveal")) {
    alert(`Could not show the files: ${e.detail.xhr.responseText.trim()}`);
  }
});
