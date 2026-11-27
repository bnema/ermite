// SPDX-License-Identifier: AGPL-3.0-or-later
// filenav.js: the file tree of the rail of a file page (FileNav in
// filenav.templ). The server sends the root entries and the directories above
// the file, open; any other directory is a details[data-src] with an empty
// list, filled the first time it opens from data-src (its tree page with ?nav).
// A failed load closes it and goes to the page of the directory instead.
//
// The folders open in the tree are remembered for the tab (sessionStorage),
// per repository and ref, and opened again on the next file page, parents
// first. Only the user's own clicks are remembered; the server-open
// ancestors stay open, as a restore only opens.
//
// The current file is brought into view inside the list, which scrolls on its
// own; the page itself never scrolls. The listeners sit on the document, so a
// rail that rail.js moves or rebuilds keeps working.
(() => {
  const MAX = 100; // remembered folders, the oldest dropped first
  const repo = location.pathname.split('/')[1] || '';
  const ref = document.body.dataset.ref || document.body.dataset.sha || 'detached';
  const key = 'ermite:fnav:' + repo + '/' + ref;
  const loads = new WeakMap(); // details -> promise of its load

  const read = () => {
    try {
      const v = JSON.parse(sessionStorage.getItem(key) || '[]');
      return Array.isArray(v) ? v.filter((p) => typeof p === 'string') : [];
    } catch { return []; }
  };
  const write = (paths) => {
    try { sessionStorage.setItem(key, JSON.stringify(paths.slice(-MAX))); } catch { /* storage off: this page only */ }
  };
  const pathOf = (details) => details.parentElement?.dataset.path ?? '';
  const listOf = (details) => details.querySelector(':scope > ul.fnav');
  const pageOf = (details) => details.dataset.src.replace(/\?nav$/, '');
  const isNav = (el) => el instanceof HTMLDetailsElement && el.matches('.fnav details[data-src]');

  // load fills an empty directory once; it resolves to false when it failed.
  // quiet (a restore) closes it without leaving the page.
  function load(details, quiet = false) {
    const ul = listOf(details);
    if (!ul || ul.children.length || details.hasAttribute('data-loaded')) return Promise.resolve(true);
    if (loads.has(details)) return loads.get(details);
    const p = (async () => {
      details.setAttribute('aria-busy', 'true');
      try {
        const res = await fetch(details.dataset.src, { headers: { Accept: 'text/html' }, credentials: 'same-origin' });
        if (!res.ok || res.redirected) throw new Error('HTTP ' + res.status);
        const t = document.createElement('template');
        t.innerHTML = await res.text();
        const items = [...t.content.children].filter((n) => n.tagName === 'LI');
        if (!items.length) throw new Error('no entries');
        if (!ul.children.length) ul.append(...items);
        details.setAttribute('data-loaded', '');
        return true;
      } catch {
        details.open = false;
        if (!quiet) location.assign(pageOf(details));
        return false;
      } finally {
        details.removeAttribute('aria-busy');
        loads.delete(details);
      }
    })();
    loads.set(details, p);
    return p;
  }

  // remember records what the user did to one folder; the folders the
  // server opened, and the opens of a restore, are not choices.
  function remember(p, open) {
    const paths = read().filter((x) => x !== p);
    if (open) paths.push(p);
    write(paths);
  }

  const find = (p) => [...document.querySelectorAll('.fnav li[data-kind="dir"]')]
    .find((li) => li.dataset.path === p)?.querySelector(':scope > details[data-src]');

  // restore opens the remembered folders, parents before their children.
  async function restore() {
    const paths = read().sort((a, b) => a.split('/').length - b.split('/').length);
    for (const p of paths) {
      const d = find(p);
      if (!d) continue;
      d.open = true;
      await load(d, true);
    }
  }

  // reveal scrolls the list, and only the list, to the current file.
  function reveal() {
    const a = document.querySelector('.fnav a[aria-current="page"]');
    const box = a?.closest('section > .fnav');
    if (!a || !box || box.scrollHeight <= box.clientHeight) return;
    const top = a.getBoundingClientRect().top - box.getBoundingClientRect().top + box.scrollTop;
    box.scrollTop = Math.max(0, top - (box.clientHeight - a.offsetHeight) / 2);
  }

  document.addEventListener('toggle', (e) => {
    if (isNav(e.target) && e.target.open) load(e.target);
  }, true);
  // The choice is saved on the click of the summary (a key press on it is a
  // click too), before the toggle, which comes a task later: a link followed
  // at once would leave before it.
  document.addEventListener('click', (e) => {
    const sum = e.target.closest?.('.fnav summary');
    const d = sum?.parentElement;
    if (!isNav(d) || e.defaultPrevented) return;
    const p = pathOf(d);
    if (p) remember(p, !d.open);
  });

  const box = document.querySelector('section > .fnav');
  if (!box) return;
  // Once the user scrolls the list, it is theirs: a late restore leaves it.
  let touched = false;
  for (const ev of ['wheel', 'touchstart', 'pointerdown', 'keydown']) box.addEventListener(ev, () => { touched = true; }, { passive: true });
  // A hidden rail (collapsed, or the closed phone sheet) has no height: the
  // current file is revealed when the list shows.
  let shown = false;
  new ResizeObserver(() => {
    const now = box.clientHeight > 0;
    if (now && !shown && !touched) reveal();
    shown = now;
  }).observe(box);
  restore().then(() => { if (!touched) reveal(); });
})();
