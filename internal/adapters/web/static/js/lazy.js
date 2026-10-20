// lazy.js: the files of a diff that are not in the page yet (commit, compare
// and merge request pages).
//
// A placeholder is a .dlazy[data-src] in a details.dfile. Two kinds:
//  - a window of a merge request: loaded by itself when it comes within two
//    screens of the view (the files of its window replace the placeholders of
//    the same ids);
//  - a collapsed file (generated, large) [data-manual]: never loaded by
//    itself. Its "load diff" link loads that one file (click, or Enter / Space
//    on the link); without scripts the link reloads the page with the file.
// A fragment is fetched once. Nothing observes the DOM: the one
// IntersectionObserver watches the automatic placeholders, and a manual one
// costs no more than its markup.
//
// window.ermiteLazy.load is also the loader of mr.js (reveal).
window.ermiteLazy = (() => {
  const $ = (sel, root = document) => root.querySelector(sel);
  const $$ = (sel, root = document) => [...root.querySelectorAll(sel)];
  const AUTO = '.dlazy[data-src]:not([data-manual])';
  const loading = new Map(); // src -> promise of the swap
  let moved = false;
  let lazyIO = null; // the observer of the automatic placeholders
  let generation = 0; // bumped by reset: older responses are ignored
  let onSwap = () => {};

  // load fetches the fragment of a placeholder and swaps its files in. The
  // promise resolves once they are in the page (or the page said to reload).
  function load(ph) {
    const gen = generation;
    const src = ph?.dataset.src;
    if (!src || moved) return Promise.resolve();
    if (loading.has(src)) return loading.get(src);
    for (const p of $$('.dlazy[data-src]')) if (p.dataset.src === src) p.setAttribute('data-loading', '');
    // The fragment renders like the page: the layout shown, and both layouts
    // when the page switches them itself (data-diff) or one (data-view).
    const d = $('#diffs')?.dataset || {};
    const view = d.diff || d.view;
    const layouts = d.diff ? 'both' : d.view ? 'one' : '';
    const url = src + (view ? '&view=' + encodeURIComponent(view) : '') + (layouts ? '&layouts=' + layouts : '');
    const p = fetch(url, { headers: { Accept: 'text/html' }, credentials: 'same-origin' })
      .then(async (res) => {
        const html = await res.text();
        if (gen !== generation) return;
        if (res.status === 409) return stale(html);
        if (!res.ok) throw new Error('loading files: ' + res.status);
        // A fragment of the old diff after the page went stale stays out.
        if (!moved) swap(html);
      })
      .catch((err) => {
        if (gen !== generation) return;
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
      // A file still collapsed stays closed; a collapsed file opens once
      // loaded; a file of a window keeps the fold.
      f.open = f.querySelector('.dlazy[data-manual]') ? false
        : old.querySelector('.dlazy[data-manual]') ? true : old.open;
      for (const p of $$('.dlazy', old)) lazyIO?.unobserve(p);
      old.replaceWith(f);
      window.htmx?.process(f);
      window.ermite?.watchFile?.(f, old);
    }
    onSwap();
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

  // Automatic placeholders load as they come within two screens of the view;
  // files that come in are marked in the list of files (ermite.js marks the
  // one in view). Manual ones are left alone.
  function watch() {
    lazyIO?.disconnect();
    if (!('IntersectionObserver' in window)) {
      for (const p of $$(AUTO)) load(p);
      return;
    }
    lazyIO = new IntersectionObserver((entries) => {
      for (const e of entries) {
        if (!e.isIntersecting) continue;
        lazyIO.unobserve(e.target);
        load(e.target);
      }
    }, { rootMargin: '200% 0px 200% 0px' });
    for (const p of $$(AUTO)) lazyIO.observe(p);
  }

  // reset forgets the loads in flight and the stale state: the page shows
  // another diff (refresh in place). watch starts the observer again.
  function reset() {
    generation++;
    moved = false;
    loading.clear();
    lazyIO?.disconnect();
  }

  // manualLink is the "load diff" link of an event target, when it is one that
  // loads in place (the placeholder has an address).
  const manualLink = (t) => {
    const a = t?.closest?.('.dlazy[data-manual] > a');
    return a && a.closest('.dlazy')?.dataset.src ? a : null;
  };

  // loadManual loads the file of a "load diff" link and puts the focus in it:
  // on its first line link, else on its header. The scroll never moves for it.
  function loadManual(a) {
    const ph = a.closest('.dlazy');
    const id = ph.closest('details')?.id;
    return load(ph).then(() => {
      const f = id && document.getElementById(id);
      if (!f || f.querySelector('.dlazy[data-manual]')) return;
      (f.querySelector('.ln a') || f.querySelector('summary'))?.focus({ preventScroll: true });
    });
  }

  function onClick(e) {
    if (e.defaultPrevented || e.button || e.ctrlKey || e.metaKey || e.shiftKey || e.altKey) return false;
    const a = manualLink(e.target);
    if (!a) return false;
    e.preventDefault();
    loadManual(a);
    return true;
  }
  // Enter makes the browser click a link by itself; Space does not. Both are
  // handled here, once (a fragment is fetched once).
  function onKey(e) {
    // A held key repeats: after the load, focus is on a line link.
    if (e.repeat) return false;
    if ((e.key !== 'Enter' && e.key !== ' ') || e.ctrlKey || e.metaKey || e.altKey || e.shiftKey) return false;
    const a = manualLink(e.target);
    if (!a) return false;
    e.preventDefault();
    loadManual(a);
    return true;
  }

  let started = false;
  function start() {
    if (started || !$('#diffs')) return;
    started = true;
    watch();
    document.addEventListener('click', onClick);
    document.addEventListener('keydown', onKey);
  }

  return { load, start, watch, reset, onSwap: (fn) => { onSwap = fn; }, onClick, onKey };
})();

if (document.readyState === 'loading') document.addEventListener('DOMContentLoaded', () => window.ermiteLazy.start());
else window.ermiteLazy.start();
