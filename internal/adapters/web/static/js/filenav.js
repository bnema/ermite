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
// ancestors stay open, as a restore only opens. A remembered folder the
// server no longer lists (an HTTP error) is forgotten.
//
// The current file is brought into view inside the list, which scrolls on its
// own; the page itself never scrolls. The listeners sit on the document, so a
// rail that rail.js moves or rebuilds keeps working.
//
// window.ermiteFileNav exposes the pieces for jstest/filenav.test.mjs.
window.ermiteFileNav = (() => {
  const MAX = 100; // remembered folders, the oldest dropped first
  const segs = location.pathname.split('/');
  // An abbreviated detached commit has neither data-ref nor data-sha: the ref
  // segment of the URL (/<repo>/blob/<ref>/…) names it.
  const ref = document.body.dataset.ref || document.body.dataset.sha || segs[3] || '';
  const key = 'ermite:fnav:' + (segs[1] || '') + '/' + ref;
  const loads = new WeakMap(); // details -> promise of its load

  // read is the remembered folders; anything stored that is not a list of
  // paths counts as none.
  const read = () => {
    try {
      const v = JSON.parse(sessionStorage.getItem(key) || '[]');
      return Array.isArray(v) ? v.filter((p) => typeof p === 'string') : [];
    } catch { return []; }
  };
  const write = (paths) => {
    try { sessionStorage.setItem(key, JSON.stringify(paths.slice(-MAX))); } catch { /* storage off: this page only */ }
  };
  // remember records what the user did to one folder; the folders the
  // server opened, and the opens of a restore, are not choices.
  const remember = (p, open) => {
    const paths = read().filter((x) => x !== p);
    if (open) paths.push(p);
    write(paths);
  };

  const inNav = (el) => !!el?.closest?.('.fnav');
  const isNav = (el) => el?.tagName === 'DETAILS' && el.hasAttribute('data-src') && inNav(el);
  const pathOf = (details) => details.parentElement?.dataset.path ?? '';
  const listOf = (details) => details.querySelector(':scope > ul.fnav');
  const pageOf = (details) => details.dataset.src.replace(/\?nav$/, '');
  // top is the outer list of the tree, the box that scrolls.
  const top = (el) => {
    let ul = el?.closest('ul.fnav');
    for (let up = ul?.parentElement?.closest('ul.fnav'); up; up = up.parentElement?.closest('ul.fnav')) ul = up;
    return ul ?? null;
  };

  // load fills an empty directory once; it resolves to false when it failed.
  // A failure closes the folder; loud (a click) goes to the page of the
  // directory, quiet (a restore) stays and forgets a folder the server
  // refused (an HTTP error, not a network one).
  function load(details, quiet = false) {
    const ul = listOf(details);
    if (!ul || ul.children.length || details.hasAttribute('data-loaded')) return Promise.resolve(true);
    if (loads.has(details)) return loads.get(details);
    const p = (async () => {
      details.setAttribute('aria-busy', 'true');
      let answered = false;
      try {
        const res = await fetch(details.dataset.src, { headers: { Accept: 'text/html' }, credentials: 'same-origin' });
        answered = true;
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
        else if (answered) remember(pathOf(details), false);
        return false;
      } finally {
        details.removeAttribute('aria-busy');
        loads.delete(details);
      }
    })();
    loads.set(details, p);
    return p;
  }

  const find = (p) => [...document.querySelectorAll('li[data-kind="dir"]')]
    .find((li) => li.dataset.path === p && inNav(li))?.querySelector(':scope > details[data-src]') ?? null;

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
    const a = [...document.querySelectorAll('a[aria-current="page"]')].find(inNav);
    const box = top(a);
    if (!a || !box || !(box.scrollHeight > box.clientHeight)) return;
    const y = a.getBoundingClientRect().top - box.getBoundingClientRect().top + box.scrollTop;
    box.scrollTop = Math.max(0, y - (box.clientHeight - a.offsetHeight) / 2);
  }

  document.addEventListener('toggle', (e) => {
    if (isNav(e.target) && e.target.open) load(e.target);
  }, true);
  // The choice is saved on the click of the summary (a key press on it is a
  // click too), before the toggle, which comes a task later: a link followed
  // at once would leave before it.
  document.addEventListener('click', (e) => {
    const d = e.target?.closest?.('summary')?.parentElement;
    if (!isNav(d) || e.defaultPrevented) return;
    const p = pathOf(d);
    if (p) remember(p, !d.open);
  });

  const box = top(document.querySelector('ul.fnav'));
  if (box) {
    // Once the user scrolls the list, it is theirs: a late restore leaves it.
    let touched = false;
    for (const ev of ['wheel', 'touchstart', 'pointerdown', 'keydown']) box.addEventListener(ev, () => { touched = true; }, { passive: true });
    // A hidden rail (collapsed, or the closed phone sheet) has no height: the
    // current file is revealed when the list shows.
    if (typeof ResizeObserver === 'function') {
      let shown = false;
      new ResizeObserver(() => {
        const now = box.clientHeight > 0;
        if (now && !shown && !touched) reveal();
        shown = now;
      }).observe(box);
    }
    restore().then(() => { if (!touched) reveal(); });
  }

  return { key, MAX, read, write, remember, load, restore, reveal };
})();
