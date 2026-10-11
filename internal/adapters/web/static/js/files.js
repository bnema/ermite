// files.js: the Files view. A directory opens in place: its entries are
// fetched once as table rows (the tree page with ?rows), inserted under it
// and kept in the DOM, so closing and opening it again needs no request.
//
// Rows opened in place carry data-depth and the --depth custom property
// (set through the CSSOM: the CSP forbids style attributes). Rows of the
// page itself have depth 0. Nothing here touches the document at load; the
// handlers are declared in the templates (hx-on:click on #tree-rows).
window.ermiteTree = (() => {
  const depthOf = (tr) => Number(tr.dataset.depth || 0);
  const linkOf = (tr) => tr.querySelector('a[data-expand]');
  const isOpen = (a) => a.getAttribute('aria-expanded') === 'true';

  function setOpen(tr, a, open) {
    a.setAttribute('aria-expanded', open ? 'true' : 'false');
    const kind = tr.querySelector('.c-kind');
    if (kind) kind.textContent = open ? '▾' : '▸';
  }

  // collapse hides every row under tr.
  function collapse(tr, a) {
    setOpen(tr, a, false);
    const d = depthOf(tr);
    for (let r = tr.nextElementSibling; r && depthOf(r) > d; r = r.nextElementSibling) r.hidden = true;
  }

  // expand shows the rows under tr that no collapsed directory between hides.
  function expand(tr, a) {
    setOpen(tr, a, true);
    const d = depthOf(tr);
    let closedAt = Infinity; // depth of the collapsed directory whose rows are skipped
    for (let r = tr.nextElementSibling; r && depthOf(r) > d; r = r.nextElementSibling) {
      const rd = depthOf(r);
      if (rd <= closedAt) closedAt = Infinity;
      r.hidden = rd > closedAt;
      const la = linkOf(r);
      if (closedAt === Infinity && la && !isOpen(la)) closedAt = rd;
    }
  }

  async function load(tr, a) {
    tr.setAttribute('aria-busy', 'true');
    try {
      const res = await fetch(a.dataset.expand, { headers: { Accept: 'text/html' } });
      if (!res.ok) throw new Error('HTTP ' + res.status);
      const t = document.createElement('template');
      t.innerHTML = await res.text();
      const d = depthOf(tr) + 1;
      const rows = Array.from(t.content.children);
      for (const r of rows) {
        r.dataset.depth = String(d);
        r.style.setProperty('--depth', String(d));
      }
      tr.after(...rows);
      tr.dataset.loaded = '1';
      tr.removeAttribute('aria-busy');
      expand(tr, a);
    } catch (e) {
      location.href = a.href;
    }
  }

  function toggle(tr) {
    const a = linkOf(tr);
    if (!a || tr.getAttribute('aria-busy') === 'true') return;
    if (isOpen(a)) collapse(tr, a);
    else if (tr.dataset.loaded) expand(tr, a);
    else load(tr, a);
  }

  return {
    // click opens or closes the directory under a plain left click; any
    // modified or other click keeps the link's own navigation.
    click(event) {
      const a = event.target.closest('a[data-expand]');
      if (!a || event.button !== 0 || event.ctrlKey || event.metaKey || event.shiftKey || event.altKey) return;
      event.preventDefault();
      toggle(a.closest('tr'));
    },
  };
})();
