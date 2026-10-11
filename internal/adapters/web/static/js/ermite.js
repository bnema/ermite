// ermite.js: the one thing hx-live cannot do is a global key *sequence* state machine
// ("g" then "h"), nor ignore keystrokes aimed at inputs before any element is focused.
// It only translates keys into DOM events named "k-<name>" on <body>; every reaction
// (cursor, panels, filters, permalink) is declared in templates/components/keys.tmpl with hx-on + hx-live.
(() => {
  const single = { j: 'j', k: 'k', G: 'bottom', '/': 'filter', '?': 'help', b: 'refs', f: 'cmd', ':': 'cmd', y: 'permalink',
    s: 's', t: 't', c: 'c', r: 'r', m: 'm', n: 'next', d: 'split', u: 'unified', x: 'collapse', Enter: 'open', Escape: 'esc' };
  const goto = { h: 'gh', t: 'gt', c: 'gc', r: 'gr', m: 'gm', g: 'top' };
  let pending = 0;
  const fire = (name) => document.body.dispatchEvent(new CustomEvent('k-' + name, { bubbles: false }));
  addEventListener('keydown', (e) => {
    // ctrl k / cmd k opens the palette from anywhere, inputs included.
    if ((e.ctrlKey || e.metaKey) && !e.altKey && e.key.toLowerCase() === 'k') { e.preventDefault(); pending = 0; return fire('cmd'); }
    if (e.ctrlKey || e.metaKey || e.altKey) return;
    const typing = e.target.closest('input, textarea, select, [contenteditable]');
    if (e.key === 'Escape') { pending = 0; if (typing) e.target.blur(); return fire('esc'); }
    if (typing) return;
    if (e.key === 'Enter' && e.target.closest('a, button, summary')) return;
    if (pending) {
      const name = goto[e.key];
      pending = 0;
      if (name) { e.preventDefault(); fire(name); }
      return;
    }
    if (e.key === 'g') { pending = 1; setTimeout(() => (pending = 0), 1200); return; }
    const name = single[e.key];
    if (!name) return;
    if (e.key !== 'Enter' || !document.querySelector('dialog[open]')) e.preventDefault();
    fire(name);
  });
  // In the app shell the page does not scroll, <main> does: give it focus so arrows,
  // PageUp/PageDown and space scroll the content without a click first.
  if (!location.hash) document.getElementById('main')?.focus({ preventScroll: true });
})();

// ermiteLive applies the fragments of the event stream (/-/events, see
// internal/adapters/web/events.go). A fragment is a top-level element with an
// id: it replaces the element of the page that has the same id. A fragment
// marked data-gone removes it; a row of a repository the home page does not
// list yet is added at the top of the list. A gap, which says the page may
// have missed events or that what it shows changed, reloads the page.
window.ermiteLive = (() => {
  let reloading = false;
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
  };
})();
