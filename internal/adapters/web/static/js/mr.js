// mr.js: the merge request page beyond what htmx and hx-live give.
//
//  1. Windows of files. The page holds the hunks of one window of files; the
//     other files are placeholders (.dlazy[data-src]). A placeholder that comes
//     near the screen fetches its window (/files?from=&base=&head=), whose
//     files replace the placeholders of the same ids. A window is fetched once;
//     a link to a file or to a comment of a placeholder fetches it first.
//  2. The menus of the bar (merge, more) and their keys: M opens the merge menu
//     on its button, 1-9 pick a mode, D toggles the branch deletion, Enter
//     submits (the browser does it: the focus is on the button), Esc closes.
//     R marks the head read. Nothing merges on one key.
//  3. g f puts the focus in the list of files (opening it when folded); j / k
//     move in it, Enter opens the file, Esc goes back to the diff. The button
//     of its header folds the list, and the choice is remembered.
//
// Nothing runs on a page without #diffs[data-mr]. window.ermiteMR.core is pure
// (jstest/mr.test.mjs).
window.ermiteMR = (() => {
  // core: the decisions, without the DOM.
  const core = {
    rangeURL(previous, current, base) {
      const a = new URL(previous, base), b = new URL(current, base);
      const parse = (u) => /^(new|old):(\d+):(.+)$/.exec(u.searchParams.get('on') || '');
      const x = parse(a), y = parse(b);
      if (!x || !y || a.pathname !== b.pathname || x[1] !== y[1] || x[3] !== y[3] || x[2] === y[2]) return null;
      const nums = [Number(x[2]), Number(y[2])].sort((a,b) => a-b);
      b.searchParams.set('on', `${y[1]}:${nums[0]}-${nums[1]}:${y[3]}`);
      return b.href;
    },
    // Range mode is explicit: ordinary taps still open single-line forms.
    touchRange(previous, current, base) {
      if (!previous) return { start: current, url: null };
      const url = core.rangeURL(previous, current, base);
      if (url) return { start: null, url };
      if (previous === current) return { start: null, url: current };
      return { start: current, url: null }; // another file/side starts afresh
    },
    mobileURL(href, view, narrow) {
      if (!narrow || view !== 'split') return null;
      const url = new URL(href);
      url.searchParams.set('view', 'unified');
      return url.href;
    },
    quote(button) {
      const thread = button.closest('.cmt').closest('[data-thread]');
      const details = thread?.querySelector(':scope > details[data-reply]');
      const field = details?.querySelector('textarea');
      if (!field) return false;
      details.open = true;
      field.value = button.dataset.quote;
      field.focus();
      return true;
    },
    // windowOf is the start of the window of file i (files are f1, f2, ...).
    windowOf(id, size) {
      const n = Number(String(id).replace(/^f/, '')) - 1;
      return Number.isInteger(n) && n >= 0 ? n - (n % size) : -1;
    },
    // menuKey says what a key does while the merge menu is open: a mode
    // (index), the branch deletion, or nothing.
    menuKey(key, modes) {
      if (key === 'D') return { toggle: true };
      if (/^[1-9]$/.test(key) && Number(key) <= modes) return { mode: Number(key) - 1 };
      return null;
    },
    tabIndex(key, index, count) {
      if (key === 'Home') return 0;
      if (key === 'End') return count - 1;
      if (key === 'ArrowRight') return (index + 1) % count;
      if (key === 'ArrowLeft') return (index + count - 1) % count;
      return null;
    },
    // step moves an index within [0, n).
    step(i, d, n) {
      if (n === 0) return -1;
      if (i < 0) return d > 0 ? 0 : n - 1;
      return Math.max(0, Math.min(n - 1, i + d));
    },
  };

  const $ = (sel, root = document) => root.querySelector(sel);
  const $$ = (sel, root = document) => [...root.querySelectorAll(sel)];
  const loading = new Map(); // src -> promise of the swap
  let refreshTouchComments = () => {};
  let moved = false;
  let lazyIO = null; // the observer of the placeholders (watch)
  let revealGen = 0; // the last reveal wins the scroll

  // load fetches the window of a placeholder and swaps its files in. The
  // promise resolves once they are in the page (or the page said to reload).
  function load(ph) {
    const src = ph?.dataset.src;
    if (!src || moved) return Promise.resolve();
    if (loading.has(src)) return loading.get(src);
    for (const p of $$('.dlazy[data-src]')) if (p.dataset.src === src) p.setAttribute('data-loading', '');
    // The window renders like the page: the layout shown, and both layouts
    // when the page switches them itself (data-diff) or one (data-view).
    const d = $('#diffs')?.dataset || {};
    const view = d.diff || d.view;
    const layouts = d.diff ? 'both' : d.view ? 'one' : '';
    const url = src + (view ? '&view=' + encodeURIComponent(view) : '') + (layouts ? '&layouts=' + layouts : '');
    const p = fetch(url, { headers: { Accept: 'text/html' }, credentials: 'same-origin' })
      .then(async (res) => {
        const html = await res.text();
        if (res.status === 409) return stale(html);
        if (!res.ok) throw new Error('loading files: ' + res.status);
        // A window of the old diff after the page went stale stays out.
        if (!moved) swap(html);
      })
      .catch((err) => {
        loading.delete(src);
        for (const p of $$('.dlazy[data-loading]')) if (p.dataset.src === src) p.removeAttribute('data-loading');
        window.ermite?.flash?.(err.message, 4000);
      });
    loading.set(src, p);
    return p;
  }

  // swap replaces each placeholder file with the file of the same id. A
  // file the reader folded stays folded.
  function swap(html) {
    const t = document.createElement('template');
    t.innerHTML = html;
    for (const f of [...t.content.querySelectorAll('details[data-file]')]) {
      const old = document.getElementById(f.id);
      if (!old || !old.querySelector('.dlazy')) continue;
      f.open = old.open;
      for (const p of $$('.dlazy', old)) lazyIO?.unobserve(p);
      old.replaceWith(f);
      window.htmx?.process(f);
      window.ermite?.watchFile?.(f, old);
    }
    refreshTouchComments();
  }

  // stale: the diff changed under the page. The placeholders say so; no file
  // of another diff comes in.
  function stale(html) {
    moved = true;
    for (const p of $$('.dlazy')) {
      p.removeAttribute('data-loading');
      p.innerHTML = html;
    }
  }

  // reveal loads the file of an id (a file "f12" or a comment "c3") when it is
  // a placeholder, then scrolls to it. It returns false when there is nothing
  // to load.
  function reveal(id, scroll = true) {
    // Any navigation, loaded or not, makes the reveals still loading stale.
    const gen = ++revealGen;
    const target = () => {
      // A page with both layouts says which shows in data-diff; a large diff
      // rendered in one layout only says it in data-view.
      const diffs = $('#diffs')?.dataset;
      if (/^c\d+$/.test(id) && !window.matchMedia?.('(max-width: 720px)').matches && (diffs?.diff || diffs?.view) === 'split') {
        const split = document.getElementById(id + '-s');
        if (split) return split;
      }
      return document.getElementById(id);
    };
    let ph = document.getElementById(id)?.querySelector(':scope > .dlazy');
    if (!ph && /^c\d+/.test(id) && !document.getElementById(id)) {
      const base = id.replace(/-s$/, '');
      ph = $$('.dlazy[data-notes]').find((p) => p.dataset.notes.split(' ').includes(base));
    }
    if (!ph) {
      const el = target();
      if (el && el.id !== id) {
        const d = el.closest('details');
        if (d) d.open = true;
        if (scroll) el.scrollIntoView({ block: 'start' });
      }
      return false;
    }
    load(ph).then(() => {
      // A later reveal (another file asked meanwhile) owns the view.
      if (gen !== revealGen) return;
      const el = target();
      if (!el) return;
      const d = el.closest('details');
      if (d) d.open = true;
      if (!scroll) return;
      // The files above may have changed size while it loaded (placeholders,
      // content-visibility): place it, then again once laid out.
      el.scrollIntoView({ block: 'start' });
      requestAnimationFrame(() => requestAnimationFrame(() => {
        if (gen === revealGen) el.scrollIntoView({ block: 'start' });
      }));
    });
    return true;
  }

  // Placeholders load as they come within two screens of the view; files
  // that come in are marked in the list of files (ermite.js marks the one in
  // view).
  function watch() {
    if (!('IntersectionObserver' in window)) {
      for (const p of $$('.dlazy[data-src]')) load(p);
      return;
    }
    lazyIO = new IntersectionObserver((entries) => {
      for (const e of entries) {
        if (!e.isIntersecting) continue;
        lazyIO.unobserve(e.target);
        load(e.target);
      }
    }, { rootMargin: '200% 0px 200% 0px' });
    for (const p of $$('.dlazy[data-src]')) lazyIO.observe(p);
  }

  // The merge menu.
  const menu = () => $('#merge-menu');
  function openMerge() {
    const m = menu();
    if (!m) return false;
    for (const d of $$('details.menu[open]')) if (d !== m) d.open = false;
    m.open = true;
    $('[data-default]', m)?.focus();
    return true;
  }
  // menuKey handles a key while the merge menu is open; true when it used it.
  function menuKey(e) {
    const m = menu();
    if (!m?.open || e.ctrlKey || e.metaKey || e.altKey) return false;
    // An open dialog (help, commands) is above the menu: it keeps the keys.
    if ($('dialog[open]')) return false;
    // A field outside the menu (a comment being written) keeps its keys.
    const field = e.target.closest?.('input, textarea, select, [contenteditable]');
    if (field && !m.contains(field)) return false;
    const radios = $$('input[name="mode"]', m);
    const act = core.menuKey(e.key, radios.length);
    if (!act) return false;
    e.preventDefault();
    if (act.toggle) {
      const box = $('input[name="delete_branch"]', m);
      if (box) box.checked = !box.checked;
    } else {
      const r = radios[act.mode];
      // A change event lets the live bindings enable the Merge button again.
      if (!r.disabled && !r.checked) {
        r.checked = true;
        r.dispatchEvent(new Event('change', { bubbles: true }));
      }
    }
    $('[data-default]', m)?.focus();
    return true;
  }
  function markRead() {
    const b = $('button[data-read]');
    if (!b) return false;
    b.closest('form').requestSubmit(b);
    return true;
  }

  // The list of files by keyboard: g f focuses the current file.
  const links = () => $$('.r-files .files > li:not([hidden]) > a');
  function focusFiles() {
    foldFiles(false);
    const all = links();
    if (!all.length) return false;
    const cur = all.find((a) => a.getAttribute('aria-current') === 'true') || all[0];
    cur.focus();
    cur.scrollIntoView({ block: 'nearest' });
    return true;
  }
  // stepFiles moves the focus in the list; false when the focus is not in it.
  function stepFiles(d) {
    const all = links();
    const i = all.indexOf(document.activeElement);
    if (i < 0) return false;
    const j = core.step(i, d, all.length);
    all[j].focus();
    all[j].scrollIntoView({ block: 'nearest' });
    return true;
  }
  function leaveFiles() {
    if (!links().includes(document.activeElement)) return false;
    document.getElementById('main')?.focus({ preventScroll: true });
    return true;
  }

  // foldFiles folds the list of files to its header, or opens it again:
  // toggles without an argument. The choice is kept for the next pages.
  function foldFiles(fold) {
    const sec = $('.r-files');
    const btn = sec && $('button.fold', sec);
    if (!btn) return;
    if (fold === undefined) fold = !sec.hasAttribute('data-folded');
    sec.toggleAttribute('data-folded', fold);
    btn.setAttribute('aria-expanded', String(!fold));
    btn.title = fold ? 'open the list of files' : 'fold the list of files';
    $('.sr', btn).textContent = btn.title;
    // The list did not follow the review while folded: show its current file,
    // moving the list only (never the page).
    const box = !fold && $('.files', sec);
    const a = box && $('a[aria-current="true"]', box);
    if (a && (a.offsetTop < box.scrollTop || a.offsetTop + a.offsetHeight > box.scrollTop + box.clientHeight)) {
      box.scrollTop = a.offsetTop - (box.clientHeight - a.offsetHeight) / 2;
    }
    try { localStorage.setItem('mr-files-folded', fold ? '1' : ''); } catch { /* storage refused: holds for this page */ }
  }

  // openFile opens the file of a link of the list (and loads it first).
  function openFile(id) {
    const f = document.getElementById(id);
    if (f) f.open = true;
    reveal(id);
  }

  // Enhance the ordinary anchor navigation only when scripts are available.
  function panels() {
    const footer = $('#mr-panels'), nav = $('[data-mr-tabs]');
    if (!footer || !nav) return;
    const tabs = $$('a', nav), panels = $$('[data-mr-panel]', footer);
    nav.setAttribute('role', 'tablist');
    function select(index, focus = false, fromUser = false) {
      tabs.forEach((tab, i) => {
        tab.setAttribute('role', 'tab');
        tab.setAttribute('aria-controls', panels[i].id);
        tab.setAttribute('aria-selected', String(i === index));
        tab.tabIndex = i === index ? 0 : -1;
        panels[i].setAttribute('role', 'tabpanel');
        panels[i].tabIndex = 0;
        panels[i].hidden = i !== index;
      });
      if (focus) tabs[index].focus();
      if (fromUser && location.hash !== '#' + panels[index].id) history.replaceState(null, '', '#' + panels[index].id);
    }
    tabs.forEach((tab, i) => {
      tab.addEventListener('click', (e) => { e.preventDefault(); select(i, false, true); });
      tab.addEventListener('keydown', (e) => {
        const next = core.tabIndex(e.key, i, tabs.length);
        if (next === null) return;
        e.preventDefault(); select(next, true, true);
      });
    });
    function hash() {
      const h = location.hash;
      const index = h === '#mr-checks' || h === '#mr-checks-panel' ? 1 : h === '#mr-commits' ? 0 : -1;
      if (index < 0) return;
      footer.open = true; select(index);
    }
    // A repeated click on the same hash must also reopen a folded footer.
    document.addEventListener('click', (e) => {
      if (e.target.closest?.('#mr-ck')) { footer.open = true; select(1); }
    });
    const summary = $('#mr-ck');
    if (summary) {
      const sync = () => { $('[data-checks-label]', nav).textContent = $('#mr-ck')?.title || $('#mr-ck')?.textContent.trim() || 'no checks'; };
      new MutationObserver(sync).observe(summary.parentElement, { childList: true, subtree: true, characterData: true });
      sync();
    }
    select(0); hash();
    addEventListener('hashchange', hash);
  }

  function touchComments() {
    const diffs = $('#diffs');
    const bar = document.createElement('div');
    bar.className = 'mr-touch-range';
    const button = document.createElement('button');
    button.type = 'button';
    button.className = 'btn';
    button.textContent = 'Select line range';
    button.setAttribute('aria-pressed', 'false');
    const status = document.createElement('span');
    status.setAttribute('role', 'status');
    bar.append(button, status);
    diffs.prepend(bar);
    refreshTouchComments = () => { bar.hidden = !$('.ln a[href*="on="]', diffs); };
    refreshTouchComments();
    const touch = window.matchMedia?.('(pointer: coarse), (hover: none)');
    let enabled = false, start = null, marker = null;
    const clear = () => { marker?.removeAttribute('data-range-start'); marker = null; start = null; };
    button.addEventListener('click', () => {
      enabled = !enabled;
      clear();
      button.setAttribute('aria-pressed', String(enabled));
      status.textContent = enabled ? 'Tap the first line number, then the last. Tap the same line for one line.' : '';
    });
    diffs.addEventListener('click', (e) => {
      const link = e.target.closest?.('.ln a[href*="on="]');
      if (!touch?.matches || !enabled || !link || e.shiftKey || e.ctrlKey || e.metaKey || e.altKey) return;
      e.preventDefault();
      e.stopPropagation(); // do not alter the desktop shift-click anchor
      const next = core.touchRange(start, link.href, location.href);
      clear();
      if (next.url) { location.assign(next.url); return; }
      start = next.start;
      marker = link;
      marker.setAttribute('data-range-start', '');
      status.textContent = `Range starts at line ${link.textContent.trim()}. Tap the last line number (same file and side).`;
    });
    touch?.addEventListener('change', () => {
      if (touch.matches) return;
      enabled = false;
      clear();
      button.setAttribute('aria-pressed', 'false');
      status.textContent = '';
    });
    // The server opens the line form. Scroll after layout without stealing
    // focus or forcing a mobile keyboard to appear.
    if (new URL(location.href).searchParams.has('on')) requestAnimationFrame(() => {
      const form = $$('.fnote .cform', diffs).find((f) => f.getClientRects().length);
      form?.scrollIntoView({ block: 'center' });
    });
  }

  function start() {
    if (!$('#diffs[data-mr]')) return;
    panels();
    // Large diffs may contain only the split table. Ask the existing server
    // route for unified rather than writing the saved session preference.
    const narrow = window.matchMedia?.('(max-width: 720px)');
    const unify = () => {
      $('#diffs').dataset.mobile = String(!!narrow?.matches);
      const url = core.mobileURL(location.href, $('#diffs')?.dataset.view, narrow?.matches);
      if (url) { location.replace(url); return true; }
      return false;
    };
    if (unify()) return;
    narrow?.addEventListener('change', unify);
    addEventListener('click', (e) => {
      if (narrow?.matches && e.target.closest?.('.vbtn[data-view="split"]')) {
        e.preventDefault();
        e.stopImmediatePropagation();
      }
    }, true);
    touchComments();
    let folded = false;
    try { folded = localStorage.getItem('mr-files-folded') === '1'; } catch { /* no storage: open */ }
    if (folded) foldFiles(true);
    // The page answers a refused action from the address of the action: show
    // the address of the request, so that a reload does not post again.
    const canonical = $('#diffs[data-canonical]')?.dataset.canonical;
    if (canonical && location.pathname !== canonical) history.replaceState(history.state, '', canonical + location.hash);
    watch();
    // A link to a file or a comment that is not loaded yet.
    const hash = () => {
      const id = decodeURIComponent(location.hash.slice(1));
      if (id) reveal(id);
    };
    addEventListener('hashchange', hash);
    hash();
    let lastLine = null;
    try { lastLine = sessionStorage.getItem('mr-last-line'); } catch { /* storage optional */ }
    addEventListener('click', (e) => {
      const quote = e.target.closest?.('[data-quote]');
      if (quote) { core.quote(quote); return; }
      const link = e.target.closest?.('a[href*="on="]');
      if (!link || e.ctrlKey || e.metaKey || e.altKey) return;
      const next = e.shiftKey && lastLine && core.rangeURL(lastLine, link.href, location.href);
      lastLine = link.href;
      try { sessionStorage.setItem('mr-last-line', lastLine); } catch { /* storage optional */ }
      if (next) { e.preventDefault(); location.assign(next); }
    });
  }

  return { core, panels, start, load, reveal, openMerge, menuKey, markRead, focusFiles, stepFiles, leaveFiles, foldFiles, openFile };
})();

if (document.readyState === 'loading') document.addEventListener('DOMContentLoaded', () => window.ermiteMR.start());
else window.ermiteMR.start();
