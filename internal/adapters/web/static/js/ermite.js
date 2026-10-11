// ermite.js: the behaviour of the pages that htmx and hx-live do not give.
//
//  1. Keys. A global key *sequence* state machine ("g" then "h") and the
//     keystrokes aimed at inputs cannot be declared with hx-on. The keydown
//     handler only translates keys into DOM events named "k-<name>" on <body>;
//     keys.templ binds each of them (hx-on, one line each) to a function of
//     window.ermite below, which does the work: row cursor, dialogs, filters,
//     diff layout, permalink.
//  2. Clicks on [data-fire] elements fire the same events (header buttons).
//  3. ermiteLive applies the fragments of the event stream (see events.go).
//  4. The file of the diff in view is marked in the table of contents.
//  5. A handler that fails is shown in the hint badge.
(() => {
  // Marks the page as scripted: what does nothing without scripts (the filters,
  // the "go to" and help buttons) is hidden until this class is set (see the
  // utilities of 10-base.css).
  document.documentElement.classList.add('js');

  const $ = (sel, root = document) => root.querySelector(sel);
  const $$ = (sel, root = document) => [...root.querySelectorAll(sel)];
  const body = () => document.body;

  // flash shows a message in the hint badge (body[data-flash], a status
  // region), then clears it unless another message came in the meantime.
  let flashTimer = 0;
  const flash = (msg, ms = 2000) => {
    body().dataset.flash = msg;
    clearTimeout(flashTimer);
    flashTimer = setTimeout(() => (body().dataset.flash = ''), ms);
  };

  const visibleRows = () => $$('[data-rows] > :not([hidden])');
  // setCursor puts data-cursor on el and takes it from the others.
  const setCursor = (el, others) => {
    for (const o of others) if (o !== el) o.removeAttribute('data-cursor');
    el.setAttribute('data-cursor', '');
  };
  const scrollPage = (to) => {
    scrollTo(0, to);
    $('#main')?.scrollTo(0, to);
  };

  const ermite = {
    fire(name) {
      body().dispatchEvent(new CustomEvent('k-' + name, { bubbles: false }));
    },

    // Row cursor: j / k.
    move(d) {
      if ($('dialog[open]')) return;
      const rows = visibleRows();
      if (!rows.length) {
        scrollBy(0, d * 180);
        $('#main')?.scrollBy(0, d * 180);
        return;
      }
      const i = rows.findIndex((r) => r.hasAttribute('data-cursor'));
      const j = Math.max(0, Math.min(rows.length - 1, i + d));
      setCursor(rows[j], $$('[data-rows] > [data-cursor]'));
      rows[j].scrollIntoView({ block: 'nearest' });
      const mark = j + 1 + '/' + rows.length;
      body().dataset.nav = mark;
      setTimeout(() => body().dataset.nav === mark && (body().dataset.nav = ''), 1500);
    },
    // gg / G: first or last row, or the top or bottom of the page.
    edge(last) {
      const rows = visibleRows();
      const row = last ? rows.at(-1) : rows[0];
      if (row) row.scrollIntoView({ block: 'nearest' });
      else scrollPage(last ? 1e9 : 0);
    },
    open() {
      if (!$('dialog[open]')) $('[data-cursor] a')?.click();
    },
    focusFilter() {
      $('[data-filter]')?.focus();
    },
    go(sel) {
      $(sel)?.click();
    },
    tab(key) {
      $(`.tabs [data-key=${key}]`)?.click();
    },

    // Dialogs.
    closeDialogs() {
      for (const d of $$('dialog[open]')) d.close();
    },
    dialog(id, focusId) {
      const d = document.getElementById(id);
      if (!d) return;
      ermite.closeDialogs();
      d.showModal();
      if (focusId) document.getElementById(focusId)?.focus();
    },

    // filter hides the rows that do not contain the text of an input. The
    // input says which rows (data-filter, or data-pick for a list without a
    // cursor) and, optionally, the groups to hide when they have no row left
    // (data-groups). The cursor goes to the first row left.
    filter(input) {
      const sel = input.dataset.filter || input.dataset.pick;
      const t = input.value.toLowerCase();
      const rows = $$(sel);
      for (const r of rows) r.hidden = !r.textContent.toLowerCase().includes(t);
      if (input.dataset.groups) {
        for (const g of $$(input.dataset.groups)) g.hidden = !rows.some((r) => !r.hidden && g.contains(r));
      }
      if (input.dataset.filter) {
        const first = rows.find((r) => !r.hidden);
        for (const r of rows) r.toggleAttribute('data-cursor', r === first);
      }
    },
    // The "go to" palette.
    cmdFilter(input) {
      const t = input.value.toLowerCase().trim();
      const items = $$('#cmd-list > li');
      for (const li of items) li.hidden = !li.dataset.k.includes(t);
      const first = items.find((li) => !li.hidden);
      if (first) setCursor(first, items);
    },
    cmdKey(e) {
      const vis = $$('#cmd-list > li:not([hidden])');
      const i = vis.findIndex((li) => li.hasAttribute('data-cursor'));
      const step = e.key === 'ArrowDown' ? 1 : e.key === 'ArrowUp' ? -1 : 0;
      if (step) {
        e.preventDefault();
        const n = vis[Math.max(0, Math.min(vis.length - 1, i + step))];
        if (n) {
          setCursor(n, $$('#cmd-list > li'));
          n.scrollIntoView({ block: 'nearest' });
        }
      } else if (e.key === 'Enter') {
        e.preventDefault();
        vis[Math.max(i, 0)]?.querySelector('a')?.click();
      }
    },

    // Diff.
    layout(mode) {
      const el = $('[data-diff]');
      if (!el) return;
      el.dataset.diff = mode;
      try { sessionStorage.setItem('diff', mode); } catch { /* storage refused: the layout holds for this page */ }
      for (const d of $$('[data-file]')) if ($('.dt', d)) d.open = true;
    },
    collapse() {
      const files = $$('[data-file]');
      const open = !files.some((d) => d.open);
      for (const d of files) d.open = open;
    },
    nextNote() {
      const notes = $$('#diffs .cmt');
      if (!notes.length) return;
      const i = notes.findIndex((n) => n.getBoundingClientRect().top > innerHeight / 3);
      const n = notes[i < 0 ? 0 : i];
      const d = n.closest('details');
      if (d) d.open = true;
      n.scrollIntoView({ block: 'center' });
    },

    // y: pin the URL to the commit and copy it.
    async permalink() {
      const b = body().dataset;
      if (!b.sha) return flash('no ref to pin on this page');
      history.replaceState(null, '', location.pathname.replace('/' + b.ref + '/', '/' + b.sha + '/') + location.hash);
      try {
        await navigator.clipboard.writeText(location.href);
        flash('permalink ' + b.sha.slice(0, 7) + ' copied');
      } catch {
        flash('permalink in the address bar (copy refused)');
      }
    },

    // The [copy] button of a clone line: the label says "Copied" for 1.5 s,
    // and so does the accessible name; the hint badge (a status region)
    // announces it.
    async copy(btn) {
      const line = btn.closest('.clone-line')?.querySelector('code')?.textContent ?? '';
      btn.dataset.idleName ||= btn.getAttribute('aria-label') || '';
      let ok = true;
      try { await navigator.clipboard.writeText(line); } catch { ok = false; }
      btn.textContent = ok ? 'Copied' : 'Copy failed';
      btn.setAttribute('aria-label', (ok ? 'copied: ' : 'copy failed: ') + line);
      flash(ok ? 'copied' : 'copy failed', 1500);
      clearTimeout(btn._restore);
      btn._restore = setTimeout(() => {
        btn.textContent = btn.dataset.label;
        btn.setAttribute('aria-label', btn.dataset.idleName);
      }, 1500);
    },
  };
  window.ermite = ermite;

  // 1. Keys.
  const single = { j: 'j', k: 'k', G: 'bottom', '/': 'filter', '?': 'help', b: 'refs', f: 'cmd', ':': 'cmd', y: 'permalink',
    s: 's', t: 't', c: 'c', r: 'r', m: 'm', n: 'next', d: 'split', u: 'unified', x: 'collapse', Enter: 'open', Escape: 'esc' };
  // "g t" is the key "t": the tabs have one event each.
  const goto = { h: 'gh', t: 't', c: 'c', r: 'r', m: 'm', g: 'top' };
  let pending = 0;
  addEventListener('keydown', (e) => {
    const typing = e.target.closest('input, textarea, select, [contenteditable]');
    // ctrl k / cmd k opens the palette; in a field only cmd k, so that the
    // field keeps ctrl k (kill to end of line on macOS).
    if ((e.metaKey || (e.ctrlKey && !typing)) && !e.altKey && !e.shiftKey && e.key.toLowerCase() === 'k') { e.preventDefault(); pending = 0; return ermite.fire('cmd'); }
    if (e.ctrlKey || e.metaKey || e.altKey) return;
    if (e.key === 'Escape') { pending = 0; if (typing) e.target.blur(); return ermite.fire('esc'); }
    if (typing) return;
    // An open dialog takes only the keys that move between dialogs: the page
    // behind it does not react (scripted clicks would go through inert).
    if ($('dialog[open]') && !['?', 'b', ':', 'f', 'Enter'].includes(e.key)) { pending = 0; return; }
    if (e.key === 'Enter' && e.target.closest('a, button, summary')) return;
    if (pending) {
      const name = goto[e.key];
      pending = 0;
      if (name) { e.preventDefault(); ermite.fire(name); }
      return;
    }
    if (e.key === 'g') { pending = 1; setTimeout(() => (pending = 0), 1200); return; }
    const name = single[e.key];
    if (!name) return;
    if (e.key !== 'Enter' || !$('dialog[open]')) e.preventDefault();
    ermite.fire(name);
  });

  // 2. A [data-fire] element does what the key of that name does (the header
  // buttons, the diff layout links). The link to the refs page works without
  // scripts and is upgraded here to open the dialog; a click with a modifier
  // keeps its meaning (new tab).
  addEventListener('click', (e) => {
    const el = e.target.closest?.('[data-fire]');
    if (!el || e.button || e.ctrlKey || e.metaKey || e.shiftKey || e.altKey) return;
    e.preventDefault();
    ermite.fire(el.dataset.fire);
  });
  for (const a of $$('a[data-fire="refs"]')) {
    a.setAttribute('aria-haspopup', 'dialog');
    a.setAttribute('aria-controls', 'panel-refs');
  }

  // In the app shell the page does not scroll, <main> does: give it focus so arrows,
  // PageUp/PageDown and space scroll the content without a click first.
  if (!location.hash) document.getElementById('main')?.focus({ preventScroll: true });

  // 4. The table of contents of a diff marks the file in view: the last file
  // whose top is above the first third of the window (the first file when none
  // is). One observer watches the files against that band; a file has passed
  // the line when it is in the band or wholly above it. It reports only when a
  // file enters or leaves the band, so a scroll costs nothing in between.
  const files = $$('.dfile');
  const links = new Map($$('.files a[href^="#"]').map((a) => [a.hash.slice(1), a]));
  if (files.length && links.size && 'IntersectionObserver' in window) {
    const passed = new Set();
    let current = null;
    const mark = () => {
      const f = files.findLast((x) => passed.has(x)) ?? files[0];
      if (f === current) return;
      links.get(current?.id)?.removeAttribute('aria-current');
      links.get(f.id)?.setAttribute('aria-current', 'true');
      current = f;
    };
    const io = new IntersectionObserver((entries) => {
      for (const e of entries) {
        const line = e.rootBounds ? e.rootBounds.bottom : innerHeight / 3;
        if (e.isIntersecting || e.boundingClientRect.top < line) passed.add(e.target);
        else passed.delete(e.target);
      }
      mark();
    }, { rootMargin: '0px 0px -67% 0px' });
    for (const f of files) io.observe(f);
  }

  // 5. A failure of a handler or of a request (htmx logs it to the console
  // already) is shown, so that a key that does nothing is not a mystery.
  addEventListener('htmx:error', (e) => {
    if (e.target.closest?.('.hint-live')) return; // the stream says so itself ("live updates paused")
    const err = e.detail?.error;
    flash('something went wrong' + (err?.message ? ': ' + err.message : ''), 4000);
  });
})();

// ermiteLive applies the fragments of the event stream (/-/events, see
// internal/adapters/web/events.go). A fragment is a top-level element with an
// id: it replaces the element of the page that has the same id. A fragment
// marked data-gone removes it; a row of a repository the home page does not
// list yet is added at the top of the list. A slot (data-slot: the place of a
// banner, a persistent status region) keeps its element and only gets the
// children of the fragment, so that screen readers announce the change. A gap,
// which says the page may have missed events or that what it shows changed,
// reloads the page. A stream the server refuses on reconnection (the
// repository is gone, the session ended) makes the page reload.
window.ermiteLive = (() => {
  let reloading = false;
  let stopped = false;
  return {
    swap(html) {
      const t = document.createElement('template');
      t.innerHTML = html;
      for (const el of t.content.children) {
        const old = el.id && document.getElementById(el.id);
        if (el.hasAttribute('data-gone')) { old?.remove(); continue; }
        if (!old) {
          const rows = document.getElementById('repo-rows');
          if (rows && el.id.startsWith('repo-')) { rows.prepend(el); window.htmx?.process(el); }
          continue;
        }
        if (old.hasAttribute('data-slot')) { old.replaceChildren(...el.childNodes); continue; }
        if (old.hasAttribute('data-cursor')) el.setAttribute('data-cursor', '');
        if (old.hidden && old.tagName === 'TR') el.hidden = true;
        old.replaceWith(el);
        window.htmx?.process(el);
      }
    },
    reload() {
      if (reloading) return;
      reloading = true;
      setTimeout(() => location.reload(), 300);
    },
    // error handles htmx:sse:error (detail.status is the status of a refused
    // reconnection). hx-sse retries for ever; a 404 or a 401 will not change by
    // itself, and the fetched page says what is the matter (or has no stream).
    // A page that reloaded for this a moment ago and still gets refused would
    // loop: it stops following the stream instead (see gate).
    error(status) {
      if (status !== 404 && status !== 401) return;
      let last = 0;
      try { last = +sessionStorage.getItem('ermite-live-reload') || 0; } catch { /* no storage: no guard */ }
      if (Date.now() - last < 15000) { stopped = true; return; }
      try { sessionStorage.setItem('ermite-live-reload', String(Date.now())); } catch { /* idem */ }
      this.reload();
    },
    // gate is htmx:sse:before:connection: once stopped, no more attempts.
    gate(event) {
      if (stopped) event.preventDefault();
    },
  };
})();
