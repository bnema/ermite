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
    flash,
    fire(name) {
      body().dispatchEvent(new CustomEvent('k-' + name, { bubbles: false }));
    },

    // Row cursor: j / k. In the side panel they move the focus between its
    // controls instead.
    move(d) {
      if ($('dialog[open]')) return;
      if (window.ermiteMR?.stepFiles(d) || window.ermiteRail.step(d)) return;
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
      if (!row) return scrollPage(last ? 1e9 : 0);
      setCursor(row, $$('[data-rows] > [data-cursor]'));
      row.scrollIntoView({ block: 'nearest' });
    },
    open() {
      if (!$('dialog[open]')) $('[data-cursor] a')?.click();
    },
    // l / h: into the side panel (the current file of a merge request, else
    // its main action, else its first control) and back to the content.
    // Enter then activates the control.
    rail(into) {
      if ($('dialog[open]')) return;
      if (into && window.ermiteMR?.focusFiles()) return flash('files · j k move · enter open · h back');
      if (!into && window.ermiteMR?.leaveFiles()) return;
      const msg = into ? window.ermiteRail.enter() : window.ermiteRail.leave();
      if (msg) flash(msg);
    },
    // openFile opens a file of the diff the table of contents links to.
    openFile(id) {
      const f = document.getElementById(id);
      if (f) f.open = true;
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
      const rows = ermite.hide(input);
      if (input.dataset.filter) {
        const first = rows.find((r) => !r.hidden);
        for (const r of rows) r.toggleAttribute('data-cursor', r === first);
      }
    },
    // hide applies the text of a filter input to its rows and groups, without
    // moving the cursor, and returns the rows.
    hide(input) {
      const t = input.value.toLowerCase();
      // A bare data-filter marks a field with its own search (the file finder
      // of the Files view): there are no rows to hide.
      const sel = input.dataset.filter || input.dataset.pick;
      if (!sel) return [];
      const rows = $$(sel);
      for (const r of rows) r.hidden = !r.textContent.toLowerCase().includes(t);
      if (input.dataset.groups) {
        for (const g of $$(input.dataset.groups)) g.hidden = !rows.some((r) => !r.hidden && g.contains(r));
      }
      return rows;
    },
    // The "go to" palette.
    cmdFilter(input) {
      const t = input.value.toLowerCase().trim();
      // The file hits the finder adds (li.finder-hit, files.js) have no data-k:
      // the finder owns them.
      const items = $$('#cmd-list > li[data-k]');
      for (const li of items) li.hidden = !li.dataset.k.includes(t);
      const first = items.find((li) => !li.hidden);
      if (first) setCursor(first, $$('#cmd-list > li'));
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
      const enc = (b.ref || '').split('/').map(encodeURIComponent).join('/').replace(/[.*+?^${}()|[\]\\]/g, '\\$&');
      // The ref follows the repository and the route: /<repo>/<tree|blob|commits>/<ref>.
      const path = enc ? location.pathname.replace(new RegExp('^(/[^/]+/[^/]+/)' + enc + '(?=/|$)'), '$1' + b.sha) : location.pathname;
      if (path === location.pathname) return flash('this page has no ref to pin');
      history.replaceState(null, '', path + location.hash);
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

  // The side panel (aside.rail) by keyboard: l enters it on its main action
  // (the primary button) or its first control, j / k step between its shown
  // controls (not hidden, not in a closed <details>), h leaves it for <main>.
  // Each returns the hint to flash, if any. Self-contained: jstest/rail.test.mjs
  // evaluates this block alone.
  window.ermiteRail = (() => {
    const CONTROLS = ['a', 'button', 'select', 'summary', 'input', 'textarea'];
    const reachable = (e) => {
      if (e.disabled || e.closest('[hidden]')) return false;
      if (e.tagName === 'A' && !e.hasAttribute('href')) return false;
      if (e.tagName === 'INPUT' && e.getAttribute('type') === 'hidden') return false;
      return !e.getClientRects || e.getClientRects().length > 0;
    };
    const rail = () => document.querySelector('.rail');
    // items walks the panel in document order.
    const items = (r) => {
      const out = [];
      const walk = (e) => {
        for (const c of e.children) {
          if (CONTROLS.includes(c.tagName.toLowerCase())) { if (reachable(c)) out.push(c); }
          else walk(c);
        }
      };
      walk(r);
      return out;
    };
    const inside = (r) => r && r.contains(document.activeElement);
    const go = (e) => { e.focus(); e.scrollIntoView({ block: 'nearest' }); };
    return {
      enter() {
        const r = rail();
        if (!r) return 'no side panel on this page';
        const list = items(r);
        const target = list.find((e) => e.matches('.btn.primary')) || list[0];
        if (!target) return 'nothing to act on in the side panel';
        go(target);
        return 'side panel · j k move · enter act · h back';
      },
      leave() {
        if (inside(rail())) document.getElementById('main')?.focus({ preventScroll: true });
      },
      // step moves d controls within the panel; false when the focus is not in it.
      step(d) {
        const r = rail();
        if (!inside(r)) return false;
        const list = items(r);
        const i = list.indexOf(document.activeElement);
        const next = list[Math.max(0, Math.min(list.length - 1, i + d))];
        if (next) go(next);
        return true;
      },
    };
  })();

  // 1. Keys.
  const single = { j: 'j', k: 'k', G: 'bottom', '/': 'filter', '?': 'help', b: 'refs', f: 'cmd', ':': 'cmd', y: 'permalink',
    s: 's', t: 't', c: 'c', r: 'r', m: 'm', n: 'next', h: 'left', l: 'right', d: 'split', u: 'unified', x: 'collapse', Enter: 'open', Escape: 'esc',
    // the merge request page (mr.js): the merge menu, mark read
    M: 'merge', R: 'read' };
  // "g t" is the key "t": the tabs have one event each.
  const goto = { h: 'gh', t: 't', c: 'c', r: 'r', m: 'm', g: 'top', f: 'gf' };
  let pending = 0;
  let pendingTimer = 0;
  addEventListener('keydown', (e) => {
    const typing = e.target.closest('input:not([type=checkbox], [type=radio], [type=submit], [type=button]), textarea, select, [contenteditable]');
    // ctrl k / cmd k opens the palette; in a field only cmd k, so that the
    // field keeps ctrl k (kill to end of line on macOS).
    if ((e.metaKey || (e.ctrlKey && !typing)) && !e.altKey && !e.shiftKey && e.key.toLowerCase() === 'k') { e.preventDefault(); pending = 0; return ermite.fire('cmd'); }
    if (e.ctrlKey || e.metaKey || e.altKey) return;
    const mr = window.ermiteMR;
    if (e.key === 'Escape') {
      pending = 0;
      if (mr?.closeMenus() || mr?.leaveFiles()) return;
      if (typing) e.target.blur();
      return ermite.fire('esc');
    }
    // An open merge menu takes its keys (1-9, D) before the field check: its
    // radios and its checkbox hold the focus.
    if (mr?.menuKey(e)) return;
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
    if (e.key === 'g') { pending = 1; clearTimeout(pendingTimer); pendingTimer = setTimeout(() => (pending = 0), 1200); return; }
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

  // A login link signs in without a click: its page submits its form once a
  // browser shows it, not while it is prerendered.
  const auto = $('form[data-autosubmit]');
  if (auto) {
    const go = () => auto.requestSubmit();
    if (document.prerendering) document.addEventListener('prerenderingchange', go, { once: true });
    else go();
  }

  // In the app shell the page does not scroll, <main> does: give it focus so arrows,
  // PageUp/PageDown and space scroll the content without a click first.
  if (!location.hash) document.getElementById('main')?.focus({ preventScroll: true });

  // 4. The table of contents of a diff marks the file in view: the last file
  // in the top third of the window, else the last file whose top is above that
  // line (the first file when none is). One observer tracks the files in the
  // band; it reports only when a file enters or leaves it, so a scroll costs
  // nothing in between, and a jump that skips the band is caught by the scan.
  let files = $$('.dfile');
  const links = new Map($$('.files a[href^="#"]').map((a) => [a.hash.slice(1), a]));
  if (files.length && links.size && 'IntersectionObserver' in window) {
    const inBand = new Set();
    let current = null;
    const mark = () => {
      const f = files.findLast((x) => inBand.has(x))
        ?? files.findLast((x) => x.getBoundingClientRect().top < innerHeight / 3)
        ?? files[0];
      if (f === current) return;
      links.get(current?.id)?.removeAttribute('aria-current');
      const a = links.get(f.id);
      a?.setAttribute('aria-current', 'true');
      // The list scrolls on its own: the current file stays in it. Only the
      // list moves, never the page (scrollIntoView would scroll the page to
      // the list when the rail is under the diff).
      const box = a?.closest('.files');
      if (a && box && box.scrollHeight > box.clientHeight && !box.contains(document.activeElement)) {
        const top = a.offsetTop;
        if (top < box.scrollTop) box.scrollTop = top;
        else if (top + a.offsetHeight > box.scrollTop + box.clientHeight) box.scrollTop = top + a.offsetHeight - box.clientHeight;
      }
      current = f;
    };
    const io = new IntersectionObserver((entries) => {
      for (const e of entries) {
        if (e.isIntersecting) inBand.add(e.target);
        else inBand.delete(e.target);
      }
      mark();
    }, { rootMargin: '0px 0px -67% 0px' });
    for (const f of files) io.observe(f);
    // A window of files of a merge request came in (mr.js): its files replace
    // placeholders, which the observer and the list knew by element.
    ermite.watchFile = (f, old) => {
      if (old) { io.unobserve(old); inBand.delete(old); }
      files = $$('.dfile');
      for (const x of [...inBand]) if (!x.isConnected) inBand.delete(x);
      if (current && !current.isConnected) current = document.getElementById(current.id);
      io.observe(f);
    };
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
  // A stream that drops and comes back within LOST_AFTER (a proxy or the
  // server closing it, a network hiccup) is not worth a message.
  const LOST_AFTER = 5000;
  let lostTimer = 0;
  const setState = (s) => { const el = document.querySelector('.hint-live'); if (el) el.dataset.state = s; };
  return {
    // lost is htmx:sse:close and htmx:sse:error: "live updates paused" shows
    // only if the stream is still down LOST_AFTER later.
    lost() {
      if (lostTimer) return;
      lostTimer = setTimeout(() => { lostTimer = 0; setState('offline'); }, LOST_AFTER);
    },
    // back is htmx:sse:after:connection.
    back() {
      clearTimeout(lostTimer);
      lostTimer = 0;
      setState('live');
    },
    swap(html) {
      const t = document.createElement('template');
      t.innerHTML = html;
      const cursor = document.querySelector('[data-rows] > [data-cursor]');
      const cursorKey = cursor?.id || cursor?.querySelector('a')?.getAttribute('href');
      for (const el of [...t.content.children]) {
        const old = el.id && document.getElementById(el.id);
        if (el.hasAttribute('data-gone')) { old?.remove(); continue; }
        if (!old) {
          const rows = document.getElementById('repo-rows');
          if (rows && el.id.startsWith('repo-')) { rows.prepend(el); window.htmx?.process(el); }
          continue;
        }
        if (old.hasAttribute('data-slot')) { old.replaceChildren(...el.childNodes); continue; }
        if (old.hasAttribute('data-cursor')) el.setAttribute('data-cursor', '');
        old.replaceWith(el);
        window.htmx?.process(el);
      }
      // The rows may have changed under an active filter: apply it again and
      // keep the cursor on its row while that row is still shown.
      for (const input of document.querySelectorAll('input[data-filter]')) {
        if (input.value) window.ermite?.hide(input);
      }
      if (!cursorKey) return;
      const rows = [...document.querySelectorAll('[data-rows] > *')];
      const back = rows.find((r) => !r.hidden && (r.id === cursorKey || r.querySelector('a')?.getAttribute('href') === cursorKey));
      if (back) for (const r of rows) r.toggleAttribute('data-cursor', r === back);
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
