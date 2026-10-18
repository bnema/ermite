// Tests of the keyboard accessibility of static/js/ermite.js and files.js on
// the small DOM of dom.mjs: the "go to" palette as a combobox, Escape in the
// file finder, the focus kept through a live swap, and the preference that
// turns the single-key shortcuts off. Run with
//   node --test 'internal/adapters/web/jstest/*.test.mjs'
import assert from 'node:assert/strict';
import { test } from 'node:test';
import { dom, El, el, flushFrames, installDom, settle } from './dom.mjs';

// The few things ermite.js uses that dom.mjs does not have.
El.prototype.replaceWith = function (n) {
  const p = this.parent;
  const i = p.nodes.indexOf(this);
  const [added] = p.adopt([n]);
  p.nodes.splice(i, 1, added);
  this.parent = null;
};
El.prototype.contains = function (e) { for (let n = e; n; n = n.parent) if (n === this) return true; return false; };
Object.defineProperty(El.prototype, 'isConnected', { get() { let n = this; while (n.parent) n = n.parent; return n === dom.body; } });
El.prototype.toggleAttribute = function (k, on) { if (on) this.setAttribute(k, ''); else this.removeAttribute(k); return on; };
El.prototype.dispatchEvent = function (ev) { (this.events ||= []).push(ev.type); };
// The focusable controls of a fragment, as the selector of ermiteLive says.
const nativeAll = El.prototype.querySelectorAll;
El.prototype.querySelectorAll = function (sel) {
  if (sel === '[data-rows] > [data-cursor]') return this.querySelectorAll('li[data-cursor]'); // no cursor in these tests
  if (sel === '[data-rows] > *') return [];
  if (!sel.startsWith('a[href], button')) return nativeAll.call(this, sel);
  const out = [];
  const walk = (e) => e.children.forEach((c) => {
    if ((c.tagName === 'A' && c.hasAttribute('href')) || ['BUTTON', 'INPUT', 'SELECT', 'TEXTAREA', 'SUMMARY'].includes(c.tagName) || c.hasAttribute('tabindex')) out.push(c);
    walk(c);
  });
  walk(this);
  return out;
};

let active = null;
// As in a browser, an element inside a hidden one does not take the focus.
El.prototype.focus = function (opts) {
  dom.focused.push({ el: this, opts });
  if (!this.closest('[hidden]')) active = this;
};
const doc = installDom({ parseHTML: (text) => parsed[text] });
const parsed = {};
Object.defineProperty(doc, 'activeElement', { get: () => active ?? dom.body });
doc.documentElement = { classList: { add() {} } };
const query = { all: doc.querySelectorAll, one: doc.querySelector };
doc.querySelectorAll = () => []; // the page-wide queries of the top of ermite.js
doc.querySelector = () => null;

const store = new Map();
globalThis.localStorage = { getItem: (k) => store.get(k) ?? null, setItem: (k, v) => store.set(k, String(v)) };
const listeners = {};
globalThis.addEventListener = (type, f) => (listeners[type] ||= []).push(f);
globalThis.scrollTo = () => {};
globalThis.fetch = async () => ({ ok: true, json: async () => ({ paths: ['cmd/main.go', 'README.md'] }) });

await import('../static/js/files.js');
await import('../static/js/ermite.js');
doc.querySelectorAll = query.all;
doc.querySelector = query.one;
const F = window.ermiteFinder;
const K = window.ermiteKeys;

// keydown runs an element handler, then the window ones unless stopped, as a
// browser does.
function keydown(target, key, elementHandler, extra = {}) {
  const e = {
    key, target, isComposing: false, prevented: false, stopped: false, blurred: false,
    preventDefault() { this.prevented = true; },
    stopPropagation() { this.stopped = true; },
    ...extra,
  };
  target.closest ||= () => null;
  elementHandler?.(e);
  if (!e.stopped) for (const f of listeners.keydown) f(e);
  return e;
}

// ---- the palette --------------------------------------------------------

function palette() {
  const li = (k, label) => el('li', { attrs: { 'data-k': k } }, el('a', { attrs: { href: '/' + label } }));
  const items = [li('go home', 'home'), li('section files', 'files'), li('branch main', 'main')];
  items[0].setAttribute('data-cursor', '');
  const input = el('input', { id: 'cmd-q', attrs: { role: 'combobox', 'aria-controls': 'cmd-list' } });
  const list = el('ul', { id: 'cmd-list' }, ...items);
  dom.body.replaceChildren(input, list);
  return { input, list, items };
}

test('the palette: options, selected option and aria-activedescendant follow the cursor', () => {
  const p = palette();
  window.ermiteCmd.sync();
  assert.deepEqual(p.items.map((i) => i.getAttribute('role')), ['option', 'option', 'option']);
  assert.equal(new Set(p.items.map((i) => i.id)).size, 3, 'unique ids');
  assert.equal(p.input.getAttribute('aria-expanded'), 'true');
  assert.equal(p.input.getAttribute('aria-activedescendant'), p.items[0].id);
  assert.deepEqual(p.items.map((i) => i.getAttribute('aria-selected')), ['true', 'false', 'false']);

  window.ermite.cmdKey({ key: 'ArrowDown', preventDefault() {} });
  assert.equal(p.input.getAttribute('aria-activedescendant'), p.items[1].id);
  assert.deepEqual(p.items.map((i) => i.getAttribute('aria-selected')), ['false', 'true', 'false']);
  assert.ok(p.items[1].hasAttribute('data-cursor') && !p.items[0].hasAttribute('data-cursor'));

  window.ermite.cmdKey({ key: 'ArrowUp', preventDefault() {} });
  assert.equal(p.input.getAttribute('aria-activedescendant'), p.items[0].id);
});

test('the palette: a filter moves the active option, no match empties it and collapses the popup', () => {
  const p = palette();
  p.input.value = 'branch';
  window.ermite.cmdFilter(p.input);
  assert.equal(p.input.getAttribute('aria-activedescendant'), p.items[2].id);
  assert.equal(p.items[2].getAttribute('aria-selected'), 'true');
  assert.equal(p.items[0].getAttribute('aria-selected'), 'false');
  p.input.value = 'zzz';
  window.ermite.cmdFilter(p.input);
  assert.equal(p.input.getAttribute('aria-expanded'), 'false');
  assert.equal(p.input.getAttribute('aria-activedescendant'), null);
});

test('the palette: the file hits that arrive later get ids, roles and the active option', async () => {
  const p = palette();
  dom.body.setAttribute('data-paths', '/paths.json');
  dom.body.setAttribute('data-blob', '/blob/main');
  p.input.value = 'main';
  window.ermite.cmdFilter(p.input); // the branch "main" is the active option
  assert.equal(p.input.getAttribute('aria-activedescendant'), p.items[2].id);
  F.palette(p.input);
  flushFrames();
  await settle();
  const hits = p.list.children.filter((li) => li.className === 'finder-hit');
  assert.ok(hits.length > 0, 'a file hit came');
  for (const h of hits) {
    assert.equal(h.getAttribute('role'), 'option');
    assert.ok(h.id);
  }
  assert.equal(p.list.children.length, new Set(p.list.children.map((i) => i.id)).size, 'ids stay unique');
  // down to the hit: the field points at it
  window.ermite.cmdKey({ key: 'ArrowDown', preventDefault() {} });
  assert.equal(p.input.getAttribute('aria-activedescendant'), hits[0].id);
  assert.equal(hits[0].getAttribute('aria-selected'), 'true');
  dom.body.removeAttribute('data-paths');
});

// ---- the file finder ----------------------------------------------------

function finder() {
  const q = el('input', { id: 'finder-q', attrs: { role: 'combobox' } });
  const rows = ['a', 'b'].map((n) => el('tr', { attrs: { 'data-nm': n } }));
  rows[0].setAttribute('data-cursor', '');
  const treeRows = el('tbody', { id: 'tree-rows', attrs: { 'data-rows': '' } }, ...rows);
  const list = el('ul', { id: 'finder-list' });
  dom.body.replaceChildren(q, el('div', { id: 'tree-view' }, treeRows), el('div', { id: 'finder', hidden: true }, el('p', { id: 'finder-status' }), list));
  dom.body.setAttribute('data-paths', '/paths.json');
  dom.body.setAttribute('data-blob', '/blob/main');
  return { q, list };
}

test('Escape in the finder clears the query, keeps the focus in the field and does not reach the global handler', async () => {
  const f = finder();
  f.q.value = 'main';
  F.input(f.q);
  flushFrames();
  await settle();
  active = f.q;
  let blurred = false;
  f.q.blur = () => { blurred = true; };
  f.q.closest = () => f.q; // the global handler sees a field being typed in
  const e = keydown(f.q, 'Escape', (ev) => F.key(f.q, ev));
  assert.equal(f.q.value, '');
  assert.equal(e.stopped, true);
  assert.equal(e.prevented, true);
  assert.equal(blurred, false, 'the field keeps the focus');
  assert.equal(active, f.q);
  // Escape again, with nothing typed: the field is left (global handler)
  const e2 = keydown(f.q, 'Escape', (ev) => F.key(f.q, ev));
  assert.equal(e2.stopped, false);
  assert.equal(blurred, true);
});

test('the finder follows the cursor moved from outside (j / k)', async () => {
  const f = finder();
  f.q.value = 'm';
  F.input(f.q);
  flushFrames();
  await settle();
  const [first, second] = f.list.children;
  assert.equal(f.q.getAttribute('aria-activedescendant'), first.id);
  // ermite.move: the cursor goes to the next option
  first.removeAttribute('data-cursor');
  second.setAttribute('data-cursor', '');
  F.sync();
  assert.equal(second.getAttribute('aria-selected'), 'true');
  assert.equal(first.getAttribute('aria-selected'), 'false');
  assert.equal(f.q.getAttribute('aria-activedescendant'), second.id);
});

// ---- the focus through a live swap --------------------------------------

function rows() {
  const mk = (id, href) => el('li', { id }, el('a', { attrs: { href }, id: id + '-a' }), el('button', { id: id + '-b' }));
  const main = el('main', { id: 'main', attrs: { tabindex: '-1' } });
  const list = el('ul', { id: 'repo-rows' }, mk('repo-a', '/a'), mk('repo-b', '/b'), mk('repo-c', '/c'));
  dom.body.replaceChildren(main, list);
  return { main, list, mk };
}
const swapWith = (html, ...nodes) => { parsed[html] = nodes; window.ermiteLive.swap(html); };

test('a swap that replaces the focused row puts the focus on the same control of the new one', () => {
  const r = rows();
  const old = r.list.children[1];
  active = old.children[0];
  dom.focused = [];
  const fresh = r.mk('repo-b', '/b');
  swapWith('b1', fresh);
  assert.equal(active, fresh.children[0], 'the link with the same href');
  assert.equal(dom.focused.at(-1).opts.preventScroll, true);
  // the second control
  active = fresh.children[1];
  const fresh2 = r.mk('repo-b', '/b');
  swapWith('b2', fresh2);
  assert.equal(active, fresh2.children[1], 'the button with the same id');
});

test('a swap with the focus elsewhere does not touch it', () => {
  const r = rows();
  active = r.list.children[0].children[0];
  dom.focused = [];
  swapWith('b3', r.mk('repo-b', '/b'));
  assert.equal(dom.focused.length, 0);
  assert.equal(active, r.list.children[0].children[0]);
});

test('a removed row hands the focus to the next row, else the previous, else the content', () => {
  const r = rows();
  active = r.list.children[1].children[0];
  const gone = (id) => { const g = el('li', { id, attrs: { 'data-gone': '' } }); return g; };
  swapWith('g1', gone('repo-b'));
  assert.equal(active, r.list.children[1].children[0], 'repo-c, which took its place');
  assert.equal(r.list.children[1].id, 'repo-c');
  active = r.list.children[1].children[0];
  swapWith('g2', gone('repo-c'));
  assert.equal(active, r.list.children[0].children[0], 'repo-a, the previous row');
  swapWith('g3', gone('repo-a'));
  // focus was on repo-a's link? it was handed there: now it is removed too
  assert.equal(active, r.main);
});

test('a removed row skips the rows a filter hides', () => {
  const r = rows();
  const [a, b, c] = r.list.children;
  b.hidden = true; // filtered out
  active = a.children[0];
  swapWith('g4', el('li', { id: 'repo-a', attrs: { 'data-gone': '' } }));
  assert.equal(active, c.children[0], 'repo-c, the next shown row, not the hidden repo-b');
  c.hidden = true;
  swapWith('g5', el('li', { id: 'repo-c', attrs: { 'data-gone': '' } }));
  assert.equal(active, r.main, 'no shown row left: the content');
});

// ---- single-character shortcuts -----------------------------------------

test('shortcuts off: single characters do nothing, ctrl k, escape and enter still work', () => {
  const events = () => dom.body.events.splice(0);
  dom.body.events = [];
  doc.querySelector = () => null;
  const press = (key, extra) => keydown({ closest: () => null }, key, null, extra);
  press('j');
  assert.deepEqual(events(), ['k-j'], 'on by default');

  K.set(true);
  assert.equal(store.get('keys-off'), '1', 'persisted');
  for (const k of ['j', 'k', '?', ':', 'G', 's', 'y']) press(k);
  press('g'); press('h');
  assert.deepEqual(events(), [], 'nothing fires');
  press('k', { ctrlKey: true });
  press('Escape');
  assert.deepEqual(events(), ['k-cmd', 'k-esc']);

  K.set(false);
  assert.equal(store.get('keys-off'), '', 'persisted off');
  press('j');
  assert.deepEqual(events(), ['k-j']);
});

test('shortcuts off: the checkbox, the note and the palette action show the state', () => {
  const box = el('input', { id: 'keys-off', attrs: { type: 'checkbox' } });
  const note = el('p', { id: 'keys-note' });
  const lbl = el('span', { cls: 'lbl' });
  const list = el('ul', { id: 'cmd-list' }, el('li', {}, el('a', { attrs: { 'data-fire': 'keys' } }, lbl)));
  dom.body.replaceChildren(box, note, list);
  const find = doc.querySelector;
  doc.querySelector = (sel) => (sel === '#cmd-list [data-fire="keys"] .lbl' ? lbl : null);
  K.set(true);
  assert.equal(box.checked, true);
  assert.match(note.textContent, /are off/);
  assert.match(lbl.textContent, /on$/);
  K.set(false);
  assert.equal(box.checked, false);
  assert.match(note.textContent, /are on/);
  assert.match(lbl.textContent, /off$/);
  doc.querySelector = find;
});

test('the preference is read from storage when the page loads', async () => {
  store.set('keys-off', '1');
  listeners.keydown = [];
  const src = await import('node:fs').then((m) => m.readFileSync(new URL('../static/js/ermite.js', import.meta.url), 'utf8'));
  doc.querySelectorAll = () => [];
  doc.querySelector = () => null;
  new Function(src)(); // a second page load
  assert.equal(window.ermiteKeys.off(), true);
  dom.body.events = [];
  keydown({ closest: () => null }, 'j');
  assert.deepEqual(dom.body.events, []);
  store.set('keys-off', '');
});
