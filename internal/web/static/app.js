// Remembers which panels and rule groups are open across live refreshes and
// reloads; anything never toggled keeps the server's default. A
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
    document.querySelectorAll("#list details, #side details").forEach((d) => {
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
  // afterSwap so a refreshed panel never flashes its default state.
  document.addEventListener("htmx:afterSwap", apply);
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

// Colour swatches in the rule editor set the colour input and mark the
// matching swatch, including when a colour is picked by hand.
(() => {
  const sync = (input) => {
    input.closest(".swatches")?.querySelectorAll(".swatch").forEach((b) => {
      b.setAttribute("aria-pressed", String(b.dataset.color === input.value.toLowerCase()));
    });
  };
  document.addEventListener("click", (e) => {
    const b = e.target.closest?.(".swatch");
    const input = b?.closest(".swatches")?.querySelector('input[type="color"]');
    if (!input) return;
    input.value = b.dataset.color;
    sync(input);
    input.dispatchEvent(new Event("input", { bubbles: true }));
  });
  document.addEventListener("input", (e) => {
    if (e.target.matches?.('.swatches input[type="color"]')) sync(e.target);
  });
  const syncAll = () => document.querySelectorAll('.swatches input[type="color"]').forEach(sync);
  document.addEventListener("DOMContentLoaded", syncAll);
  document.addEventListener("htmx:afterSettle", syncAll);
})();

// File pickers: the filter hides rows whose path doesn't contain its text,
// ignoring case like the rules do, and the header box toggles only the
// visible rows, so a filter can select a subset. Hidden rows stay in the
// form, so filtering never changes what is submitted.
(() => {
  const rows = (fs) => [...fs.querySelectorAll("li[data-path]")];
  const visible = (fs) => rows(fs).filter((li) => !li.hidden);
  const boxes = (lis) => lis.map((li) => li.querySelector("input[name=file]"));

  // bytes matches the server's humanize.Bytes: SI units, one decimal
  // below 10.
  const bytes = (n) => {
    const units = ["B", "kB", "MB", "GB", "TB", "PB"];
    let i = 0;
    while (n >= 1000 && i < units.length - 1) {
      n /= 1000;
      i++;
    }
    return `${i && n < 10 ? n.toFixed(1) : Math.round(n)} ${units[i]}`;
  };
  const sync = (fs) => {
    const shown = boxes(visible(fs));
    const header = fs.querySelector("[data-toggle-files]");
    header.checked = shown.length > 0 && shown.every((c) => c.checked);
    const picked = rows(fs).filter((li) => li.querySelector("input[name=file]").checked);
    const size = picked.reduce((sum, li) => sum + Number(li.dataset.size), 0);
    fs.querySelector("[data-summary]").textContent = `${picked.length} of ${rows(fs).length} · ${bytes(size)}`;
  };
  const filter = (input) => {
    const fs = input.closest("fieldset");
    const q = input.value.trim().toLowerCase();
    let n = 0;
    for (const li of rows(fs)) {
      li.hidden = q !== "" && !li.dataset.path.toLowerCase().includes(q);
      if (!li.hidden) n++;
    }
    fs.querySelector("[data-shown]").textContent = q ? `${n} shown` : "";
    sync(fs);
  };

  document.addEventListener("input", (e) => {
    if (e.target.matches("[data-filter-files]")) filter(e.target);
  });
  // Enter in the filter would otherwise submit the form.
  document.addEventListener("keydown", (e) => {
    if (e.key === "Enter" && e.target.matches("[data-filter-files]")) e.preventDefault();
  });
  document.addEventListener("change", (e) => {
    const fs = e.target.closest("fieldset.picker");
    if (!fs) return;
    if (e.target.matches("[data-toggle-files]")) {
      boxes(visible(fs)).forEach((c) => (c.checked = e.target.checked));
    }
    sync(fs);
  });
})();

// Boosted links and forms replace only the dialog, leaving the dashboard and
// its live list running behind it. Every page renders its own dialog, so URLs
// still work directly and a response with an empty one closes it.
(() => {
  const sync = () => {
    const modal = document.getElementById("modal");
    if (!modal) return;
    const wanted = document.getElementById("modal-body").children.length > 0;
    if (!wanted) {
      if (modal.open) modal.close();
      return;
    }
    if (modal.matches(":modal")) return;
    // A dialog restored from history has the open attribute but isn't modal.
    if (modal.open) modal.close();
    modal.showModal();
  };

  document.addEventListener("htmx:beforeSwap", (e) => {
    if (!e.detail.boosted) return;
    e.detail.target = document.getElementById("modal-body");
    e.detail.selectOverride = "#modal-body";
    e.detail.swapOverride = "outerHTML";
  });
  document.addEventListener("htmx:afterSettle", (e) => {
    sync();
    if (e.detail.boosted && e.detail.requestConfig.verb !== "get") {
      htmx.trigger(document.body, "panels-changed");
    }
  });
  document.addEventListener("htmx:historyRestore", sync);
  document.addEventListener("DOMContentLoaded", sync);

  // Closing has to change the URL too, so Escape follows the close link. The
  // cancel event doesn't bubble.
  document.addEventListener("cancel", (e) => {
    if (e.target.id !== "modal") return;
    e.preventDefault();
    e.target.querySelector(".modal-close")?.click();
  }, true);
})();
