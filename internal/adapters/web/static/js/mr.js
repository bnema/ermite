// mr.js: the merge request page beyond what htmx and hx-live give.
//
//  1. Windows of files. The page holds the hunks of one window of files; the
//     other files are placeholders (.dlazy[data-src]), loaded by lazy.js (which
//     the commit and compare pages use too) when they come near the screen. A
//     link to a file or to a comment of a placeholder loads it first (reveal).
//  2. The menus of the bar (merge, more) and their keys: M opens the merge menu
//     on its button, 1-9 pick a mode, D toggles the branch deletion, Enter
//     submits (the browser does it: the focus is on the button), Esc closes.
//     R marks the head read. Nothing merges on one key.
//  3. l puts the focus in the list of files (opening it when folded); j / k
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
  let refreshTouchComments = () => {};
  let revealGen = 0; // the last reveal wins the scroll
  // The bar wraps only when its items do not fit: the file headers stick
  // under its measured height (--mr-bar-h), not under a guessed one.
  let barObserver = null;
  function watchBar() {
    barObserver?.disconnect();
    const bar = $('.mr-bar');
    if (!bar || typeof ResizeObserver !== 'function') return;
    barObserver = new ResizeObserver(() => document.body.style.setProperty('--mr-bar-h', bar.offsetHeight + 'px'));
    barObserver.observe(bar);
  }
  const load = (ph) => window.ermiteLazy.load(ph);
  window.ermiteLazy.onSwap(() => refreshTouchComments());

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
    let ph = document.getElementById(id)?.querySelector(':scope > .dlazy:not([data-manual])');
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

  // The list of files by keyboard: l focuses the current file.
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

  // Enhance the server-rendered disclosures when scripts are available.
  let disposePanels = () => {};
  // The tabs are disclosure buttons (aria-expanded + aria-controls), not a
  // tablist: a tablist always has a selected tab, and here every panel can be
  // folded. State: the index of the open panel, or -1 for none.
  function panels(initial = -1) {
    disposePanels();
    const footer = $('.mr-footer'), nav = $('[data-mr-tabs]');
    if (!footer || !nav) return;
    const tabs = $$('button', nav), panels = $$('[data-mr-panel]', footer);
    const ids = new Set(['#mr-commits', '#mr-checks', '#mr-checks-panel']);
    let current = -1;
    // show applies the state; fromUser also keeps the URL fragment in step.
    function show(index, fromUser = false) {
      current = index;
      tabs.forEach((tab, i) => {
        tab.setAttribute('aria-controls', panels[i].id);
        tab.setAttribute('aria-expanded', String(i === index));
        panels[i].hidden = i !== index;
      });
      if (!fromUser) return;
      if (index >= 0) {
        if (location.hash !== '#' + panels[index].id) history.replaceState(null, '', '#' + panels[index].id);
      } else if (ids.has(location.hash)) {
        history.replaceState(null, '', location.pathname + location.search);
      }
    }
    const toggle = (i) => show(i === current ? -1 : i, true);
    tabs.forEach((tab, i) => {
      tab.addEventListener('click', (e) => { e.preventDefault(); toggle(i); });
      tab.addEventListener('keydown', (e) => {
        const next = core.tabIndex(e.key, i, tabs.length);
        if (next === null) return;
        e.preventDefault(); tabs[next].focus();
      });
    });
    // hash opens the panel of the URL fragment. Its target was hidden when the
    // browser scrolled, so the panel is brought in; never on a refresh, which
    // keeps the scroll.
    function hash() {
      const h = location.hash;
      const index = h === '#mr-checks' || h === '#mr-checks-panel' ? 1 : h === '#mr-commits' ? 0 : -1;
      if (index >= 0) { show(index); panels[index].scrollIntoView?.({ block: 'nearest' }); }
    }
    // The checks badge always opens Checks; it never folds it.
    const checksClick = (e) => {
      if (e.target.closest?.('#mr-ck')) show(1);
    };
    document.addEventListener('click', checksClick);
    let observer;
    const summary = $('#mr-ck');
    if (summary) {
      // Both #mr-ck and #mr-actions can be replaced: observe the stable bar,
      // and write the label only when it changes. hx-live rewrites
      // other text in the same bar on every mutation of the document; a write
      // here on each of them would wake hx-live again, a loop that never yields.
      const sync = () => {
        const label = $('[data-checks-label]', nav);
        const text = $('#mr-ck')?.title || $('#mr-ck')?.textContent.trim() || 'no checks';
        if (label && label.textContent !== text) label.textContent = text;
      };
      observer = new MutationObserver(sync);
      observer.observe(summary.closest('.mr-bar'), { childList: true, subtree: true });
      sync();
    }
    show(initial);
    footer.setAttribute('data-panels', '');
    if (initial < 0) hash();
    addEventListener('hashchange', hash);
    disposePanels = () => {
      document.removeEventListener?.('click', checksClick);
      window.removeEventListener?.('hashchange', hash);
      observer?.disconnect?.();
    };
  }

  let disposeTouch = () => {};
  function touchComments(scroll = true) {
    disposeTouch();
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
    const changed = () => {
      if (touch.matches) return;
      enabled = false;
      clear();
      button.setAttribute('aria-pressed', 'false');
      status.textContent = '';
    };
    touch?.addEventListener('change', changed);
    disposeTouch = () => { touch?.removeEventListener?.('change', changed); bar.remove?.(); };
    // The server opens the line form. Scroll after layout without stealing
    // focus or forcing a mobile keyboard to appear.
    if (scroll && new URL(location.href).searchParams.has('on')) requestAnimationFrame(() => {
      const form = $$('.fnote .cform', diffs).find((f) => f.getClientRects().length);
      form?.scrollIntoView({ block: 'center' });
    });
  }

  // A refresh scheduler has one request in flight, a trailing debounce, and
  // a dirty bit for pushes received during that request. Drafts are checked
  // again after the fetch: typing while the network is slow is safe too.
  function refresher({ blocked, fetchPage, apply, notice, failed, delay = 300 }) {
    let timer, busy = false, pending = false;
    const schedule = () => { clearTimeout(timer); timer = setTimeout(run, delay); };
    async function run() {
      if (busy || !pending) return;
      if (blocked()) { notice(true); return; }
      busy = true;
      pending = false;
      try {
        const page = await fetchPage();
        if (blocked()) { pending = true; notice(true); return; }
        if (await apply(page) === false) { pending = true; notice(true); }
        else notice(false);
      } catch { if (blocked()) { pending = true; notice(true); } else failed(); }
      finally {
        busy = false;
        if (pending && !blocked()) schedule();
      }
    }
    return {
      request() { pending = true; schedule(); },
      resume() { if (pending) schedule(); },
      pending() { return pending || busy; },
    };
  }

  function hasDraft() {
    // Only form controls, never diff rows. Inline edits live in .cmt forms; the
    // merge form (d-acts) holds a chosen mode and the delete-branch box.
    return $$(':is(form.cform, .cmt form, form.d-acts) :is(textarea, input, select)').some((t) => {
      if (['hidden', 'submit', 'button', 'reset', 'image'].includes(t.type)) return false;
      if (document.activeElement === t) return true;
      if (t.type === 'radio' || t.type === 'checkbox') return t.checked !== t.defaultChecked;
      if (t.tagName === 'SELECT') return [...t.options].some((o) => o.selected !== o.defaultSelected);
      return t.value !== t.defaultValue;
    }) ||
      !!document.activeElement?.closest?.('.cform');
  }

  // Path + side + line is stable when files are inserted before this one;
  // fN is only a positional anchor. Hidden unified/split rows are ignored.
  function scrollAnchor() {
    const top = Math.max(0, $('.hdr')?.getBoundingClientRect().bottom || 0,
      $('.mr-bar')?.getBoundingClientRect().bottom || 0);
    const diffs = $('#diffs');
    const bounds = diffs.getBoundingClientRect();
    const x = Math.max(1, Math.min(innerWidth - 1, bounds.left + Math.min(100, bounds.width / 2)));
    // A bounded number of probes, independent of the number of diff lines.
    let y = top + 2, fallback;
    for (let probe = 0; probe < 6 && y < innerHeight; probe++) {
      const hit = document.elementFromPoint(x, y);
      const file = hit?.closest('details.dfile');
      if (!file || !diffs.contains(file)) { y += 32; continue; }
      const row = hit.closest('tr');
      const link = row && $('.ln a[href*="on="]', row);
      const header = $(':scope > summary', file);
      const headerOffset = file.getBoundingClientRect().top;
      fallback = { file: file.dataset.file, offset: headerOffset, headerOffset, y: scrollY };
      if (link) return { ...fallback,
        on: new URL(link.href, location.href).searchParams.get('on'),
        suffix: link.getAttribute('href').split('on=')[1].split('#')[0],
        offset: row.getBoundingClientRect().top };
      if (file.open && header) y = Math.max(y + 24, header.getBoundingClientRect().bottom + 2);
      else return fallback;
    }
    if (fallback) return fallback;
    return { y: scrollY };
  }

  function restoreAnchor(anchor) {
    const file = $$('#diffs details.dfile').find((f) => f.dataset.file === anchor.file);
    let el, offset = anchor.offset;
    if (file && anchor.on) {
      const suffix = anchor.suffix && 'on=' + anchor.suffix + '#' + file.id;
      const layout = $('#diffs').dataset.diff || $('#diffs').dataset.view;
      const table = $(layout === 'split' && !window.matchMedia?.('(max-width: 720px)').matches ? '.dt.spl' : '.dt.uni', file) || file;
      if (suffix) el = $('a[href$="' + CSS.escape(suffix) + '"]', table)?.closest('tr');
    }
    if (!el && file) { el = file; offset = anchor.headerOffset ?? anchor.offset; }
    if (el) window.scrollBy({ top: el.getBoundingClientRect().top - offset, behavior: 'instant' });
    else window.scrollTo({ top: Math.max(0, Math.min(anchor.y, document.documentElement.scrollHeight - innerHeight)), behavior: 'instant' });
  }

  async function fetchPage(url = location.href, mobileRetry = false) {
    const res = await fetch(url, { credentials: 'same-origin', headers: { Accept: 'text/html' } });
    if (!res.ok || res.redirected) throw new Error('MR refresh refused');
    const page = new DOMParser().parseFromString(await res.text(), 'text/html');
    if (!$('#diffs[data-mr]', page) || !$('.mr-bar', page) || !$('.hint-live', page)) throw new Error('not an MR page');
    const unified = core.mobileURL(url, $('#diffs', page).dataset.view,
      !!window.matchMedia?.('(max-width: 720px)').matches);
    if (unified && !mobileRetry) return fetchPage(unified, true);
    return page;
  }

  // htmx 4's public delete swap invokes recursive before:cleanup (including
  // hx-sse's abort/reader.cancel and hx-live's binding disposal). Only an empty
  // string is parsed; the fetched nodes themselves are adopted, never serialized.
  async function replaceRegion(old, fresh) {
    if (!old || !fresh) throw new Error('missing MR refresh region');
    for (const el of $$('[autofocus]', fresh)) el.removeAttribute('autofocus');
    const marker = document.createComment('MR region');
    old.before(marker);
    const remove = () => window.htmx.swap({ sourceElement: old, target: old, text: '',
      swap: 'delete settle:0 transition:false', push: false, replace: false });
    if (old.matches('.hint-live')) {
      window.ermiteLive.resumeStream(fresh);
      await window.ermiteLive.teardown(remove);
    } else await remove();
    const node = document.adoptNode(fresh);
    marker.replaceWith(node);
    window.htmx.process(node);
  }

  async function refreshPage(page) {
    const regions = ['.mr-footer', '#mr-range', '.mr-bar', '#mr-diff-notices', '#diffs', '#mr-comments', '.rail', '.hint-live'];
    for (const sel of regions) if (!$(sel) || !$(sel, page)) throw new Error('missing MR region');
    let anchor = scrollAnchor();
    const nextDiffs = $('#diffs', page);
    const mobile = !!window.matchMedia?.('(max-width: 720px)').matches;
    const chosen = mobile ? 'unified' : ($('#diffs').dataset.diff || $('#diffs').dataset.view);
    if (nextDiffs.dataset.diff) nextDiffs.dataset.diff = chosen;
    // A one-layout response must use its own contract, never the old page's.
    if (mobile && nextDiffs.dataset.view === 'split') throw new Error('mobile needs unified');
    const anchorFile = $$('#diffs details.dfile', page).find((f) => f.dataset.file === anchor.file);
    const placeholder = anchorFile && $('.dlazy[data-src]', anchorFile);
    if (placeholder) {
      const d = nextDiffs.dataset;
      const url = placeholder.dataset.src + '&view=' + encodeURIComponent(d.diff || d.view || 'unified') + '&layouts=' + (d.diff ? 'both' : 'one');
      const res = await fetch(url, { credentials: 'same-origin', headers: { Accept: 'text/html' } });
      if (!res.ok || res.redirected) throw new Error('anchor window changed');
      let windowPage = new DOMParser().parseFromString(await res.text(), 'text/html');
      if (hasDraft()) return false;
      anchor = scrollAnchor();
      const windowFiles = $$('details.dfile', windowPage);
      // If the reader moved outside this window, discard it; lazy loading will
      // fetch the new position rather than committing an unrelated window.
      if (windowFiles.some((f) => f.dataset.file === anchor.file)) {
        const byID = new Map($$('#diffs details.dfile', page).map((f) => [f.id, f]));
        for (const f of windowFiles) byID.get(f.id)?.replaceWith(f);
      }
      windowPage = null;
    }
    if (hasDraft()) return false;
    const files = new Map($$('#diffs details.dfile').map((f) => [f.dataset.file, f.open]));
    const folded = $('.r-files')?.hasAttribute('data-folded');
    const railY = $('.rail')?.scrollTop, filesY = $('.r-files .files')?.scrollTop;
    const openPanel = $$('[data-mr-tabs] button').findIndex((t) => t.getAttribute('aria-expanded') === 'true');
    const active = document.activeElement;
    const focus = active && { file: active.closest?.('.files a')?.getAttribute('title'), region: regions.find((sel) => $(sel)?.contains(active)), menu: active.closest?.('details.menu')?.id, id: active.id, tag: active.tagName, name: active.getAttribute('name'), href: active.getAttribute('href') };
    for (const f of $$('#diffs details.dfile', page)) if (files.has(f.dataset.file)) f.open = files.get(f.dataset.file);
    ++revealGen; window.ermiteLazy.reset();
    $('#diffs', page).dataset.mrStarted = 'true';
    // The fresh footer comes in with the panels as they are, so that an open
    // panel does not fold and open again while the other regions swap.
    const freshFooter = $('.mr-footer', page);
    if (freshFooter) {
      freshFooter.setAttribute('data-panels', '');
      $$('[data-mr-panel]', freshFooter).forEach((p, i) => { p.hidden = i !== openPanel; });
    }
    for (const sel of regions) {
      const fresh = $(sel, page);
      await replaceRegion($(sel), fresh);
    }
    for (const key of ['ref', 'sha', 'paths', 'blob']) {
      if (page.body.dataset[key] !== undefined) document.body.dataset[key] = page.body.dataset[key];
    }
    const oldSHA = $('.hdr .ref .sha'), newSHA = $('.hdr .ref .sha', page);
    if (oldSHA && newSHA) oldSHA.textContent = newSHA.textContent;
    page = null;
    panels(openPanel);
    watchBar();
    foldFiles(!!folded);
    if ($('.rail')) $('.rail').scrollTop = railY;
    if ($('.r-files .files')) $('.r-files .files').scrollTop = filesY;
    touchComments(false);
    $('#diffs').dataset.mobile = String(!!window.matchMedia?.('(max-width: 720px)').matches);
    window.ermite?.refreshFiles?.();
    window.ermiteLazy.watch();
    if (focus && active !== document.body) {
      const region = focus.region ? $(focus.region) : document;
      const menu = focus.menu && document.getElementById(focus.menu);
      if (menu) menu.open = true;
      const el = focus.file ? $('a[title="' + CSS.escape(focus.file) + '"]', region) :
        focus.id ? document.getElementById(focus.id) :
        focus.href ? $('a[href="' + CSS.escape(focus.href) + '"]', region) :
        focus.name ? $('[name="' + CSS.escape(focus.name) + '"]', region) :
        menu?.querySelector(':scope > summary');
      el?.focus({ preventScroll: true });
    }
    restoreAnchor(anchor);
  }

  function refreshNotice(deferred) {
    const slot = $('#mr-refresh-status');
    if (!slot) return;
    slot.replaceChildren();
    if (!deferred) {
      const text = document.createElement('span'); text.className = 'sr'; text.textContent = 'branch updated'; slot.append(text);
      return;
    }
    slot.append('branch updated — refreshing after your comment. ');

  }
  const headRefresh = refresher({ blocked: hasDraft, fetchPage, apply: refreshPage, notice: refreshNotice,
    failed: () => window.ermiteLive?.reload() });
  function headChanged() { if ($('#diffs[data-mr]')) headRefresh.request(); }

  function start() {
    if (!$('#diffs[data-mr]')) return;
    if (window.ermiteLive) window.ermiteLive.canReload = () => {
      if (!hasDraft()) return true;
      headRefresh.request(); refreshNotice(true); return false;
    };
    // Global delegation is installed once; replacement regions have their own
    // disposable panel/touch hooks below.
    if ($('#diffs').dataset.mrStarted) return;
    $('#diffs').dataset.mrStarted = 'true';
    for (const type of ['input', 'focusout', 'reset', 'submit']) document.addEventListener?.(type, () => headRefresh.resume());
    document.addEventListener?.('click', (e) => {
      const cancel = e.target.closest?.('.cform .f-btns a');
      if (!cancel || !headRefresh.pending() || e.ctrlKey || e.metaKey || e.shiftKey || e.altKey) return;
      e.preventDefault();
      const form = cancel.closest('.cform');
      form.reset();
      if (form.contains(document.activeElement)) document.activeElement.blur();
      headRefresh.resume();
    });
    panels();
    watchBar();
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

  return { core, refresher, hasDraft, fetchPage, scrollAnchor, restoreAnchor, headChanged, panels, start, load, reveal, openMerge, menuKey, markRead, focusFiles, stepFiles, leaveFiles, foldFiles, openFile };
})();

if (document.readyState === 'loading') document.addEventListener('DOMContentLoaded', () => window.ermiteMR.start());
else window.ermiteMR.start();
