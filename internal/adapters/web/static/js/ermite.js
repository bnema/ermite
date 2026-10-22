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
//  6. The lines a diff hides load in place (ermiteGaps; e and E).
//  7. The service worker (ermiteInstall).
//  8. Chip lists kept on one line, with a … chip (ermiteFit).
//  9. The layout of the home page, remembered in the browser (ermiteLayout).
// 10. The wide mode: every page takes the whole width (ermiteWide).
//
// Preferences are kept by ermiteStore (store.js, loaded first).
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
      window.ermiteTree?.sync(rows[j]);
      window.ermiteFinder?.sync(); // the rows may be the file finder's options
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
      window.ermiteTree?.sync(row);
      window.ermiteFinder?.sync();
    },
    open() {
      if ($('dialog[open]') || window.ermiteTree?.open()) return;
      $('[data-cursor] a')?.click();
    },
    // l / h: in the Files view, open a directory or a file / close a directory
    // or go up. Elsewhere: into the side panel (the current file of a merge
    // request, else its main action, else its first control) and back to the
    // content. Enter then activates the control.
    rail(into) {
      if ($('dialog[open]')) return;
      if (window.ermiteTree?.key(into)) return;
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

    // The preference "single-key shortcuts" (ermiteKeys): the checkbox of the
    // help dialog and the action of the palette.
    setKeysOff(off) {
      window.ermiteKeys.set(off);
    },
    toggleKeys() {
      const off = !window.ermiteKeys.off();
      window.ermiteKeys.set(off);
      ermite.closeDialogs();
      flash(off ? 'single-key shortcuts off · ctrl k and esc still work' : 'single-key shortcuts on');
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
      if (id === 'panel-cmd') window.ermiteCmd?.sync();
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
      for (const r of rows) {
        const was = r.hidden;
        r.hidden = !r.textContent.toLowerCase().includes(t);
        // A row shown again lays out its chips again (it had no width hidden).
        if (was && !r.hidden) for (const ul of $$('[data-fit]', r)) window.ermiteFit?.fit(ul);
      }
      if (input.dataset.groups) {
        for (const g of $$(input.dataset.groups)) g.hidden = !rows.some((r) => !r.hidden && g.contains(r));
      }
      return rows;
    },
    // The "go to" palette (ermiteCmd below).
    cmdFilter(input) {
      window.ermiteCmd.filter(input);
    },
    cmdKey(e) {
      window.ermiteCmd.key(e);
    },

    // Diff.
    layout(mode) {
      const el = $('[data-diff]');
      if (!el) return;
      if (el.hasAttribute('data-mr') && el.dataset.mobile === 'true') return;
      el.dataset.diff = mode;
      try { sessionStorage.setItem('diff', mode); } catch { /* storage refused: the layout holds for this page */ }
      for (const d of $$('[data-file]')) if ($('.dt', d)) d.open = true;
    },
    collapse() {
      const files = $$('[data-file]');
      const open = !files.some((d) => d.open);
      for (const d of files) d.open = open;
    },
    // e / E: the whole file in view, or every file of the diff, with every
    // line its hunks hide.
    expandFile() {
      const f = window.ermiteGaps.current();
      if (!f) return flash('no file in view');
      f.open = true;
      window.ermiteGaps.file(f).then((r) => {
        if (!r.done) return; // load said what failed
        flash(r.lines ? 'whole file' : 'nothing hidden in this file');
      });
    },
    async expandAll() {
      const files = $$('[data-file]').filter((f) => f.querySelector('tr.gap'));
      if (!files.length) return flash('nothing hidden in the loaded files');
      flash('loading every line…', 10000);
      for (const f of files) {
        if (!(await window.ermiteGaps.file(f)).done) return; // load said what failed
      }
      flash('every line of the loaded files');
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

  // The "go to" palette is an editable combobox with a listbox popup: the
  // focus stays in #cmd-q, the options are the <li> of #cmd-list, and the one
  // with data-cursor is the active one (aria-selected on it, aria-activedescendant
  // on the field). sync keeps the two in step; it runs on every change of the
  // cursor or of the list, including the file hits files.js adds later.
  // Self-contained: jstest/palette.test.mjs evaluates this block alone.
  window.ermiteCmd = (() => {
    const list = () => document.getElementById('cmd-list');
    const field = () => document.getElementById('cmd-q');
    const options = () => [...list().querySelectorAll(':scope > li')];
    let seq = 0;
    const moveCursor = (li) => {
      for (const o of options()) if (o !== li) o.removeAttribute('data-cursor');
      li.setAttribute('data-cursor', '');
    };
    function sync() {
      const input = field();
      if (!input || !list()) return;
      const all = options();
      const shown = all.filter((li) => !li.hidden);
      const cur = shown.find((li) => li.hasAttribute('data-cursor'));
      for (const li of all) {
        li.id ||= 'cmd-h' + seq++;
        li.setAttribute('role', 'option');
        li.setAttribute('aria-selected', li === cur ? 'true' : 'false');
      }
      input.setAttribute('aria-expanded', shown.length ? 'true' : 'false');
      if (cur) input.setAttribute('aria-activedescendant', cur.id);
      else input.removeAttribute('aria-activedescendant');
    }
    return {
      sync,
      // filter hides the entries that do not contain the text of the field.
      // The file hits the finder adds (li.finder-hit, files.js) have no data-k:
      // the finder owns them.
      filter(input) {
        const t = input.value.toLowerCase().trim();
        const items = options().filter((li) => li.hasAttribute('data-k'));
        for (const li of items) li.hidden = !li.getAttribute('data-k').includes(t);
        const first = items.find((li) => !li.hidden);
        if (first) moveCursor(first);
        sync();
      },
      key(e) {
        const vis = options().filter((li) => !li.hidden);
        const i = vis.findIndex((li) => li.hasAttribute('data-cursor'));
        const step = e.key === 'ArrowDown' ? 1 : e.key === 'ArrowUp' ? -1 : 0;
        if (step) {
          e.preventDefault();
          const n = vis[Math.max(0, Math.min(vis.length - 1, i + step))];
          if (n) {
            moveCursor(n);
            n.scrollIntoView({ block: 'nearest' });
          }
          sync();
        } else if (e.key === 'Enter') {
          e.preventDefault();
          vis[Math.max(i, 0)]?.querySelector('a')?.click();
        }
      },
    };
  })();

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

  // The installed app: the service worker (/-/sw.js) is registered once the
  // page has loaded. With it and the manifest the browser offers the install
  // by itself (the address bar of a desktop, the menu or a banner on a phone);
  // the page leaves that offer alone. Self-contained: jstest/install.test.mjs
  // evaluates this block alone.
  window.ermiteInstall = (() => {
    const api = {
      // register installs the service worker; a browser without one, or a
      // refusal, leaves the site working as before.
      register() {
        if (!navigator.serviceWorker) return Promise.resolve(null);
        return navigator.serviceWorker.register('/-/sw.js', { scope: '/' }).catch(() => null);
      },
    };
    if (document.readyState === 'complete') api.register();
    else addEventListener('load', () => api.register(), { once: true });
    return api;
  })();

  // The hidden lines of a diff: a row .gap holds a button that loads them
  // (/context); the rows that come back replace the gap in the table of each
  // layout (the unified and the split tables hold one row each for it). A
  // file whose lines are all loaded has no gap left.
  window.ermiteGaps = (() => {
    const pending = new Map();
    // selectLoaded marks the loaded rows that fall in the range of lines the
    // URL selects (?on=new:21-149:a.go, a shift + click), as the server marks
    // the rows it sent: the range stays one block once its hidden lines show.
    // The loaded rows are context lines, the same on both sides.
    function selectLoaded(file, rows) {
      const m = /^(new|old):(?:(\d+)-)?(\d+):(.+)$/.exec(new URLSearchParams(location.search).get('on') || '');
      // data-file is the path comments name (the new path of a rename), as ?on=
      if (!m || file.dataset.file !== m[4]) return;
      const end = Number(m[3]);
      const start = m[2] ? Number(m[2]) : end;
      for (const r of rows) {
        const cells = [...r.querySelectorAll('td')];
        // unified: old, new, mark, text; split: old side, then new side
        const num = Number((cells.length === 4 ? cells[m[1] === 'old' ? 0 : 1] : cells[m[1] === 'old' ? 0 : 3])?.textContent.trim());
        if (!(num >= start && num <= end)) continue;
        if (cells.length === 4) r.classList.add('sel');
        else for (const c of cells) c.classList.add('sel');
      }
    }
    // load fills the gap the button is in, in both tables of its file.
    function load(btn) {
      const src = btn.dataset.src;
      const row = btn.closest('tr.gap');
      const file = row?.closest('[data-file]');
      if (!src || !file) return Promise.resolve(0);
      if (pending.has(src)) return pending.get(src);
      const key = row.dataset.gap;
      const table = btn.closest('table');
      const focused = document.activeElement === btn;
      btn.disabled = true;
      const p = fetch(src, { headers: { Accept: 'text/html' }, credentials: 'same-origin' })
        .then(async (res) => {
          if (!res.ok) throw new Error('loading the hidden lines: ' + res.status);
          const t = document.createElement('template');
          t.innerHTML = await res.text();
          let n = 0;
          for (const table of file.querySelectorAll('table.dt')) {
            const kind = table.classList.contains('spl') ? 'spl' : 'uni';
            const from = t.content.querySelector('tbody[data-for="' + kind + '"]');
            const at = [...table.querySelectorAll('tr.gap')].find((r) => r.dataset.gap === key);
            if (!from || !at) continue;
            const rows = [...from.children].map((r) => r.cloneNode(true));
            n = Math.max(n, rows.filter((r) => !r.classList.contains('gap')).length);
            selectLoaded(file, rows.filter((r) => !r.classList.contains('gap')));
            at.replaceWith(...rows);
          }
          // The focus was on the button, which is gone: on the gap that is
          // left in the same table, else on the content.
          if (focused && (document.activeElement === btn || document.activeElement === document.body)) {
            const next = [...(table?.querySelectorAll('tr.gap') || [])].find((r) => r.dataset.gap === key)?.querySelector('button');
            if (next) next.focus({ preventScroll: true });
            else document.getElementById('main')?.focus({ preventScroll: true });
          }
          return n;
        })
        .catch((err) => {
          btn.disabled = false;
          window.ermite?.flash?.(err.message, 4000);
          return -1;
        })
        .finally(() => pending.delete(src));
      pending.set(src, p);
      return p;
    }
    // file loads every gap of a file, until none is left (a long gap comes
    // back with a gap for the rest). A gap already loading is waited for.
    // done is false when a load failed (it said why) or the file never ran
    // out of gaps.
    async function file(f) {
      let lines = 0;
      for (let guard = 0; guard < 1000; guard++) {
        const btn = f.querySelector('tr.gap button[data-src]');
        if (!btn) return { done: true, lines };
        const n = await load(btn);
        if (n < 0) return { done: false, lines };
        if (btn.isConnected) {
          // an answer that did not take the gap's place: stop, not loop
          window.ermite?.flash?.('some hidden lines could not be shown', 4000);
          return { done: false, lines };
        }
        lines += n;
      }
      window.ermite?.flash?.('the file is too long to show every line at once', 4000);
      return { done: false, lines };
    }
    // current is the file in view: the one the table of contents marks, else
    // the last whose top is above the top third of the window.
    function current() {
      const a = document.querySelector('.files a[aria-current="true"]');
      const marked = a && document.getElementById(a.hash.slice(1));
      if (marked?.matches('[data-file]')) return marked;
      const files = [...document.querySelectorAll('[data-file]')];
      return files.findLast((f) => f.getBoundingClientRect().top < innerHeight / 3) ?? files[0] ?? null;
    }
    addEventListener('click', (e) => {
      const btn = e.target.closest?.('tr.gap button[data-src]');
      if (btn) load(btn);
    });
    return { load, file, current };
  })();

  // The preference "single-character shortcuts" (WCAG 2.1.4): off, the keys
  // that are one printable character (j, k, s, ?, :, G, 1-9 in the merge menu,
  // …) do nothing. Shortcuts with ctrl or cmd, escape, enter and the arrows
  // keep working. The choice is kept by ermiteStore ("keys-off": '1' when off)
  // and is made in the help dialog (a checkbox) or the palette (an action).
  // Self-contained with store.js: jstest/a11y.test.mjs evaluates this block.
  window.ermiteKeys = (() => {
    const pref = window.ermiteStore.pref('keys-off', { values: ['1', ''], fallback: '' });
    const isOff = () => pref.get() === '1';
    // apply shows the state: the checkbox and the note of the help dialog,
    // and the label of the palette action.
    function apply() {
      const off = isOff();
      const box = document.getElementById('keys-off');
      if (box) box.checked = off;
      const note = document.getElementById('keys-note');
      if (note) {
        note.textContent = off
          ? 'Single-key shortcuts are off: only ctrl k and esc, enter and the arrows work. Turn them on again here.'
          : 'Single-key shortcuts are on. ctrl k and esc work whatever this says.';
      }
      const act = document.querySelector('#cmd-list [data-fire="keys"] .lbl');
      if (act) act.textContent = off ? 'turn single-key shortcuts on' : 'turn single-key shortcuts off';
    }
    function set(v) {
      pref.set(v ? '1' : '');
      apply();
    }
    pref.watch(apply);
    apply();
    return {
      off: isOff,
      set,
      apply,
      // blocks says whether the preference turns this key off.
      blocks: (e) => isOff() && e.key.length === 1,
    };
  })();

  // The layout of the home page: "cards" (the default), "split" (two
  // repositories per row) or "compact" (one line each). The choice is kept by
  // ermiteStore ("home-layout"), made with the buttons [data-set-layout] of
  // the layout switch, and shown as .home[data-layout], which the styles
  // follow, and as aria-pressed on the buttons. Another tab's change applies
  // here at once. The topic chips are fitted again: their room changes with the
  // layout. Self-contained with store.js: jstest/layout.test.mjs evaluates
  // this block.
  window.ermiteLayout = (() => {
    const values = ['cards', 'split', 'compact'];
    const pref = window.ermiteStore.pref('home-layout', { values, fallback: 'cards' });
    function apply() {
      const layout = pref.get();
      const home = document.querySelector('.home');
      if (home) home.setAttribute('data-layout', layout);
      for (const b of document.querySelectorAll('[data-set-layout]')) {
        b.setAttribute('aria-pressed', String(b.getAttribute('data-set-layout') === layout));
      }
      window.ermiteFit?.all();
    }
    function set(v) {
      pref.set(v);
      apply();
    }
    addEventListener('click', (e) => {
      const btn = e.target.closest?.('[data-set-layout]');
      if (btn) set(btn.getAttribute('data-set-layout'));
    });
    pref.watch(apply);
    apply();
    return { get: pref.get, set, apply, values };
  })();

  // The wide mode (w, on every page): the work area takes the whole width of
  // the screen, the rail at the right edge and the content in the rest.
  // The choice is kept by ermiteStore ("wide": '1' when wide) and shown
  // as html.wide, which the styles follow. Another tab's change applies here
  // at once.
  window.ermiteWide = (() => {
    const pref = window.ermiteStore.pref('wide', { values: ['1', ''], fallback: '' });
    function apply() {
      const wide = pref.get() === '1';
      document.documentElement.classList.toggle?.('wide', wide);
    }
    function toggle() {
      pref.set(pref.get() === '1' ? '' : '1');
      apply();
    }
    pref.watch(apply);
    apply();
    return { toggle, apply };
  })();

  // The menus of the bars (<details class="menu">: the diff view, the merge
  // menu): closing one hands the focus back to its button when it was inside,
  // so that a keyboard user keeps their place.
  function closeMenu(d) {
    const had = d.contains(document.activeElement);
    d.open = false;
    if (had) $('summary', d)?.focus();
  }
  // closeMenus closes every open menu (escape); true when one was open.
  function closeMenus() {
    const open = $$('details.menu[open]');
    for (const d of open) closeMenu(d);
    return open.length > 0;
  }

  // 1. Keys.
  const single = { j: 'j', k: 'k', G: 'bottom', '/': 'filter', '?': 'help', b: 'refs', f: 'cmd', ':': 'cmd', y: 'permalink',
    s: 's', t: 't', c: 'c', r: 'r', m: 'm', i: 'i', I: 'inbox', n: 'next', h: 'left', l: 'right', d: 'split', u: 'unified', x: 'collapse', w: 'wide', e: 'expand', E: 'expandAll', Enter: 'open', Escape: 'esc',
    // the merge request page (mr.js): the merge menu, mark read
    M: 'merge', R: 'read' };
  // "g t" is the key "t": the tabs have one event each.
  const goto = { h: 'gh', t: 't', c: 'c', r: 'r', m: 'm', i: 'i', g: 'top', f: 'gf' };
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
      if (closeMenus() || mr?.leaveFiles()) return;
      if (typing) e.target.blur();
      return ermite.fire('esc');
    }
    // Single-character shortcuts turned off: only the keys above work.
    if (window.ermiteKeys.blocks(e)) { pending = 0; return; }
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
    // The arrows and Home / End are the tree's only while the focus is on one of
    // its rows (they scroll, or move the focus, anywhere else); j k l h are
    // page-level keys, as on the other lists.
    if (!e.shiftKey && e.target.closest?.('#tree-rows') && window.ermiteTree?.arrow(e.key)) { e.preventDefault(); return; }
    const name = single[e.key];
    if (!name || (name === 'inbox' && !$('[data-inbox]'))) return;
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
    // A choice in a menu closes it; the focus goes back to its button.
    const menu = el.closest('details.menu');
    if (menu) closeMenu(menu);
  });
  // A click outside an open menu closes it.
  addEventListener('click', (e) => {
    for (const d of $$('details.menu[open]')) if (!d.contains(e.target)) d.open = false;
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
    // A refreshed MR replaced the rail and the windowed diff. Rebuild once,
    // not once per file (which would make a large file list quadratic).
    ermite.refreshFiles = () => {
      io.disconnect(); inBand.clear(); current = null;
      files = $$('.dfile'); links.clear();
      for (const a of $$('.files a[href^="#"]')) links.set(a.hash.slice(1), a);
      for (const f of files) io.observe(f);
    };
    // A window of files of a merge request came in (mr.js): its files replace
    // placeholders, which the observer and the list knew by element.
    ermite.watchFile = (f, old) => {
      if (old) { io.unobserve(old); inBand.delete(old); }
      files = $$('.dfile');
      for (const x of [...inBand]) if (!x.isConnected) inBand.delete(x);
      if (current && !current.isConnected) current = null;
      io.observe(f);
    };
  }

  // 8. A list of chips on one line ([data-fit], the topics of a home row):
  // the chips that wrap out of sight are hidden and a "…" button after the
  // last one shown names them; pressing it opens the list (data-open) on as
  // many lines as it needs. Self-contained: jstest/fit.test.mjs evaluates
  // this block alone.
  window.ermiteFit = (() => {
    const fit = (ul) => {
      if (ul.hasAttribute('data-open')) return;
      // The … button is kept across fits, so that a refit (a resize) does not
      // take the focus from it.
      let more = ul.querySelector(':scope > li.more');
      let btn = more?.firstElementChild;
      const focused = !!btn && document.activeElement === btn;
      more?.remove();
      const chips = [...ul.children];
      for (const li of chips) li.hidden = false;
      const top = chips[0]?.offsetTop;
      if (top === undefined || chips.at(-1).offsetTop === top) {
        // Everything fits now: a focused … button hands the focus to the
        // last chip.
        if (focused && chips.length) {
          chips.at(-1).setAttribute('tabindex', '-1');
          chips.at(-1).focus({ preventScroll: true });
        }
        return;
      }
      if (!more) {
        more = document.createElement('li');
        more.className = 'more';
        btn = document.createElement('button');
        btn.type = 'button';
        btn.textContent = '…';
        more.append(btn);
      }
      ul.append(more);
      if (focused) btn.focus({ preventScroll: true });
      // Keep the chips of the first line, then drop from the end until the
      // … chip fits there too.
      let shown = chips.findIndex((li) => li.offsetTop !== top);
      for (const li of chips.slice(shown)) li.hidden = true;
      while (shown > 0 && more.offsetTop !== top) chips[--shown].hidden = true;
      const rest = chips.slice(shown).map((li) => li.textContent);
      btn.title = rest.join(', ');
      btn.setAttribute('aria-label', 'show ' + rest.length + ' more: ' + rest.join(', '));
    };
    // open shows every chip of the list of the … button pressed.
    const open = (btn) => {
      const ul = btn.closest('[data-fit]');
      if (!ul) return;
      const item = btn.closest('li');
      // The focus goes to the first chip that was hidden, now shown.
      const next = [...ul.children].find((li) => li.hidden);
      ul.setAttribute('data-open', '');
      item.remove();
      for (const li of ul.children) li.hidden = false;
      if (next) {
        next.setAttribute('tabindex', '-1');
        next.focus({ preventScroll: true });
      }
    };
    const all = (root = document) => {
      for (const ul of root.querySelectorAll('[data-fit]')) fit(ul);
    };
    return { fit, all, open };
  })();
  addEventListener('click', (e) => {
    const btn = e.target.closest?.('[data-fit] > li.more > button');
    if (btn) window.ermiteFit.open(btn);
  });
  {
    window.ermiteFit.all();
    // Widths change with the window and when the fonts arrive; one frame per
    // burst. Always listening: a live update can bring the first list.
    let pending = 0;
    const again = () => {
      if (pending) return;
      pending = requestAnimationFrame(() => { pending = 0; window.ermiteFit.all(); });
    };
    addEventListener('resize', again);
    document.fonts?.ready.then(again);
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
// list yet is added at the top of the list. Activity updates also move existing
// home rows to the top when sorted by newest activity; CI updates stay put. A slot (data-slot: the place of a
// banner, a persistent status region) keeps its element and only gets the
// children of the fragment, so that screen readers announce the change. A gap,
// which says the page may have missed events or that what it shows changed,
// reloads the page. A stream the server refuses on reconnection (the
// repository is gone, the session ended) makes the page reload.
window.ermiteLive = (() => {
  let connection = null;
  window.addEventListener?.('htmx:sse:after:connection', (event) => {
    if (event.target.matches?.('.hint-live')) connection = event.detail.connection;
  });
  let reloading = false;
  let stopped = false;
  // A stream that drops and comes back within LOST_AFTER (a proxy or the
  // server closing it, a network hiccup) is not worth a message.
  const LOST_AFTER = 5000;
  let lostTimer = 0;
  // The focus survives a swap. focusables are the controls of a fragment in
  // document order; where(root) says which one has the focus (its id, else its
  // tag and href or name, and its index among the controls) and refocus finds
  // it again in the element that replaced root.
  const FOCUSABLE = 'a[href], button:not([disabled]), input:not([type=hidden]):not([disabled]), select:not([disabled]), textarea:not([disabled]), summary, [tabindex]';
  // shown: not inside a hidden element (a filtered row, a folded block); the
  // browser refuses the focus there.
  const shown = (e) => !e.closest('[hidden]') && (!e.checkVisibility || e.checkVisibility());
  const focusables = (root) => [...root.querySelectorAll(FOCUSABLE)].filter(shown);
  // take focuses e and says whether the focus is there now.
  const take = (e) => { e.focus({ preventScroll: true }); return document.activeElement === e; };
  const where = (root) => {
    const a = document.activeElement;
    if (!a || a === document.body || !root.contains(a)) return null;
    if (a === root) return { self: true };
    const all = focusables(root);
    return { id: a.id, tag: a.tagName, href: a.getAttribute('href'), name: a.getAttribute('name'), index: all.indexOf(a) };
  };
  const refocus = (root, w) => {
    if (w.self) return root.focus({ preventScroll: true });
    const all = focusables(root);
    const same = (e) => e.tagName === w.tag;
    const target =
      (w.id && all.find((e) => e.id === w.id)) ||
      (w.href != null && all.find((e) => same(e) && e.getAttribute('href') === w.href)) ||
      (w.name != null && all.find((e) => same(e) && e.getAttribute('name') === w.name)) ||
      (all[w.index] && same(all[w.index]) && all[w.index]) ||
      all[0];
    if (target) target.focus({ preventScroll: true });
    else if (root.hasAttribute('tabindex')) root.focus({ preventScroll: true });
  };
  // lostFocus: the element that had the focus inside is gone. The focus goes to
  // the first control of the next shown row, else of the nearest shown row
  // before, else to the content.
  const lostFocus = (after, before) => {
    const tryRow = (e) => {
      if (!shown(e)) return false;
      const c = focusables(e)[0] || (e.hasAttribute('tabindex') ? e : null);
      return !!c && take(c);
    };
    for (let e = after; e; e = e.nextElementSibling) if (tryRow(e)) return;
    for (let e = before; e; e = e.previousElementSibling) if (tryRow(e)) return;
    document.getElementById('main')?.focus({ preventScroll: true });
  };
  const setState = (s) => { const el = document.querySelector('.hint-live'); if (el) el.dataset.state = s; };
  // fitIn lays out the one-line chip lists of a fragment now in the page.
  const fitIn = (el) => { for (const ul of el.querySelectorAll('[data-fit]')) window.ermiteFit?.fit(ul); };
  return {
    // teardown runs remove, which deletes the stream element. hx-sse has no
    // public close API: its cleanup aborts the fetch, then calls
    // connection.reader.cancel() without catching the AbortError it rejects
    // with. Wrapping that one reader handles exactly this rejection, with no
    // timing window and no global filter.
    async teardown(remove) {
      const reader = connection?.reader;
      if (reader) connection.reader = { cancel: () => reader.cancel().catch(() => {}) };
      return remove();
    },
    resumeStream(element) {
      // The public connection includes id-only messages as well as named ones.
      const id = connection?.lastEventId;
      if (!id) return;
      const url = new URL(element.getAttribute('hx-sse:connect'), location.href);
      url.searchParams.set('after', id);
      element.setAttribute('hx-sse:connect', url.pathname + url.search);
    },
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
        const had = old ? where(old) : null; // the focus is inside what is replaced
        if (el.hasAttribute('data-gone')) {
          const after = old?.nextElementSibling, before = old?.previousElementSibling;
          old?.remove();
          if (had) lostFocus(after, before);
          continue;
        }
        if (!old) {
          const rows = document.getElementById('repo-rows');
          if (rows && el.id.startsWith('repo-')) { rows.prepend(el); window.htmx?.process(el); fitIn(el); }
          continue;
        }
        if (old.hasAttribute('data-slot')) {
          old.replaceChildren(...el.childNodes);
          if (had) refocus(old, had);
          continue;
        }
        if (old.hasAttribute('data-cursor')) el.setAttribute('data-cursor', '');
        // data-keep-checks: the checkboxes the user ticked stay ticked.
        if (old.hasAttribute('data-keep-checks')) {
          for (const box of old.querySelectorAll('input[type=checkbox][name]:checked')) {
            const same = el.querySelector(`input[type=checkbox][name="${CSS.escape(box.name)}"]`);
            if (same) same.checked = true;
          }
        }
        old.replaceWith(el);
        const rows = document.getElementById('repo-rows');
        if (rows && el.parentElement === rows && rows.dataset.sort === 'activity' &&
            rows.dataset.desc === 'true' && el.hasAttribute('data-activity')) rows.prepend(el);
        window.htmx?.process(el);
        fitIn(el);
        if (had) refocus(el, had);
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
      setTimeout(() => {
        reloading = false;
        if (window.ermiteLive.canReload?.() !== false) location.reload();
      }, 300);
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
