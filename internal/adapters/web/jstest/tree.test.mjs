// Tests of the tree rows of static/js/files.js (window.ermiteTree) on a tiny
// hand-rolled DOM. Run with
//   node --test 'internal/adapters/web/jstest/*.test.mjs'
import assert from 'node:assert/strict';
import { test } from 'node:test';
import { dom, El, el, installDom } from './dom.mjs';

// A row is a name: a trailing "/" makes a directory, "!x" a div that is not a
// row, "more" the tree-more row of a truncated directory.
function row(name, depth) {
  const dir = name.endsWith('/');
  const li = el('li', { cls: name === 'more' ? 'tree-more' : '', attrs: { 'data-depth': depth } });
  li.name = name;
  if (dir) {
    li.setAttribute('data-expand', '/rows/' + name);
    li.setAttribute('aria-expanded', 'false');
  }
  li.setAttribute('role', 'treeitem');
  const main = el('span', { cls: 't-main' }, el('span', { cls: 't-node' }));
  const a = el('a', { cls: 't-name', href: (dir ? '/tree/' : '/blob/') + name });
  a.click = () => { globalThis.location.href = a.href; };
  main.append(a);
  li.append(main);
  li.append(el('span', { cls: 't-sub' }, el('a', { cls: 't-commit', href: '/commit/' + name })));
  return li;
}

installDom({
  parseHTML: (text) => text.split(',').filter(Boolean).map((n) => (n[0] === '!' ? new El('div') : row(n, 0))),
});
await import('../static/js/files.js');
const T = window.ermiteTree;

// fetches: URL -> resolve function, to answer a request when the test says so.
const pending = new Map();
globalThis.fetch = (url) => new Promise((resolve) => {
  pending.set(url, (text, ok = true) => resolve({ ok, status: ok ? 200 : 500, text: async () => text }));
});
const answer = async (url, text, ok) => {
  pending.get(url)(text, ok);
  await new Promise((r) => setTimeout(r, 0));
};

// click sends a click on the link of tr; it answers whether the page's own
// navigation was kept (no preventDefault).
const click = (tr, mods = {}, target = tr.querySelector('a.t-name')) => {
  let prevented = false;
  T.click({ target, button: 0, preventDefault() { prevented = true; }, ...mods });
  return !prevented;
};
const isOpen = (tr) => tr.getAttribute('aria-expanded') === 'true';

// table builds the page: the list of rows, with the cursor on the first.
function table(...names) {
  const ul = el('ul', { id: 'tree-rows', attrs: { 'data-rows': '' } }, ...names.map((n) => row(n, 0)));
  dom.body.replaceChildren(ul);
  ul.children[0]?.setAttribute('data-cursor', '');
  T.init();
  dom.active = null;
  return ul;
}
const byName = (body, n) => body.children.find((r) => r.name === n);
const shown = (body) => body.children.filter((r) => !r.hidden).map((r) => r.name);

test('a directory opens in place and closes again without a request', async () => {
  const body = table('P/', 'f');
  click(body.children[0]);
  assert.equal(body.children[0].getAttribute('aria-busy'), 'true');
  await answer('/rows/P/', 'a,b');
  assert.deepEqual(shown(body), ['P/', 'a', 'b', 'f']);
  assert.equal(byName(body, 'a').dataset.depth, '1');
  assert.ok(isOpen(body.children[0]));
  click(body.children[0]);
  assert.deepEqual(shown(body), ['P/', 'f']);
  pending.clear();
  click(body.children[0]);
  assert.deepEqual(shown(body), ['P/', 'a', 'b', 'f']);
  assert.equal(pending.size, 0, 'no new request');
});

test('TS-001/TS-002: a directory that loads under a closed ancestor comes in hidden', async () => {
  pending.clear();
  const body = table('P/', 'f');
  const P = body.children[0];
  click(P);
  await answer('/rows/P/', 'C/,x');
  const C = byName(body, 'C/');
  click(C); // C starts loading
  assert.equal(C.getAttribute('aria-busy'), 'true');
  C.setAttribute('data-cursor', '');
  click(P); // P closes meanwhile
  assert.deepEqual(shown(body), ['P/', 'f']);
  assert.ok(P.hasAttribute('data-cursor'), 'the cursor of a hidden row moves to the closed row');
  assert.ok(!C.hasAttribute('data-cursor'));

  await answer('/rows/C/', 'c1,c2');
  assert.deepEqual(shown(body), ['P/', 'f'], 'no visible row under a closed directory');
  assert.deepEqual(body.children.map((r) => r.name), ['P/', 'C/', 'c1', 'c2', 'x', 'f']);
  assert.equal(byName(body, 'c1').hidden, true);
  assert.equal(C.getAttribute('aria-busy'), null);
  assert.ok(isOpen(C), 'C is open and loaded');
  assert.ok(isOpen(C));

  pending.clear();
  click(P); // P opens: C's rows show with it
  assert.deepEqual(shown(body), ['P/', 'C/', 'c1', 'c2', 'x', 'f']);
  assert.equal(pending.size, 0, 'C was not fetched again');
});

test('a closed directory under a closed ancestor stays closed when the ancestor opens', async () => {
  pending.clear();
  const body = table('P/');
  click(body.children[0]);
  await answer('/rows/P/', 'C/');
  const C = byName(body, 'C/');
  click(C);
  await answer('/rows/C/', 'c1');
  click(C); // C closed
  click(body.children[0]);
  click(body.children[0]);
  assert.deepEqual(shown(body), ['P/', 'C/']);
});

test('TS-006: only rows are kept; no row falls back to navigating', async () => {
  pending.clear();
  const body = table('P/');
  click(body.children[0]);
  await answer('/rows/P/', '!junk,a');
  assert.deepEqual(body.children.map((r) => r.name), ['P/', 'a']);

  const other = table('Q/');
  globalThis.location.href = '';
  click(other.children[0]);
  await answer('/rows/Q/', '!junk');
  assert.equal(globalThis.location.href, '/tree/Q/');
  assert.deepEqual(other.children.map((r) => r.name), ['Q/']);
});

test('a modified or non-left click keeps the link navigation', () => {
  pending.clear();
  const cases = [
    { ctrlKey: true }, { metaKey: true }, { shiftKey: true }, { altKey: true }, { button: 1 }, { button: 2 },
  ];
  for (const mods of cases) {
    const body = table('P/');
    assert.equal(click(body.children[0], mods), true, JSON.stringify(mods) + ' is not intercepted');
    assert.equal(pending.size, 0, 'no request');
    assert.equal(body.children[0].getAttribute('aria-busy'), null);
  }
  assert.equal(click(table('P/').children[0]), false, 'a plain left click is intercepted');
});

test('a second click while the rows are loading is ignored', async () => {
  pending.clear();
  const body = table('P/');
  let requests = 0;
  const fetch0 = globalThis.fetch;
  globalThis.fetch = (url) => { requests++; return fetch0(url); };
  click(body.children[0]);
  click(body.children[0]);
  assert.equal(requests, 1);
  await answer('/rows/P/', 'a');
  globalThis.fetch = fetch0;
  assert.deepEqual(shown(body), ['P/', 'a']);
});

test('a failed request navigates to the directory', async () => {
  pending.clear();
  const body = table('P/');
  globalThis.location.href = '';
  click(body.children[0]);
  await answer('/rows/P/', 'oops', false);
  assert.equal(globalThis.location.href, '/tree/P/');
  assert.deepEqual(body.children.map((r) => r.name), ['P/']);
});

test('the tree-more row has a depth and follows its directory', async () => {
  pending.clear();
  const body = table('P/', 'f');
  click(body.children[0]);
  await answer('/rows/P/', 'a,more');
  const more = byName(body, 'more');
  assert.equal(more.dataset.depth, '1');
  assert.equal(more.style.props['--depth'], '1');
  click(body.children[0]);
  assert.equal(more.hidden, true, 'hidden with its directory');
  assert.deepEqual(shown(body), ['P/', 'f']);
  click(body.children[0]);
  assert.equal(more.hidden, false, 'shown with it again');
});

// ---- the keyboard: l, h and Enter on the row under the cursor ----------------

const cursorName = (body) => body.children.find((r) => r.hasAttribute('data-cursor'))?.name;
const nav = () => globalThis.location.href;

test('l opens a closed directory, then enters it; h closes it, or goes to its directory', async () => {
  pending.clear();
  const body = table('P/', 'f');
  assert.equal(T.key(true), true);
  await answer('/rows/P/', 'C/,a');
  assert.deepEqual(shown(body), ['P/', 'C/', 'a', 'f']);
  assert.equal(cursorName(body), 'P/', 'the cursor stays on the directory it opened');
  T.key(true); // open: into its first entry
  assert.equal(cursorName(body), 'C/');
  T.key(false); // C/ is closed: go to its directory
  assert.equal(cursorName(body), 'P/');
  T.key(false); // P/ is open: close it
  assert.deepEqual(shown(body), ['P/', 'f']);
  assert.ok(!isOpen(body.children[0]));
  assert.equal(cursorName(body), 'P/');
});

test('l on a file opens it; Enter opens a file and toggles a directory', async () => {
  pending.clear();
  const body = table('f', 'P/');
  globalThis.location.href = '';
  assert.equal(T.key(true), true);
  assert.equal(nav(), '/blob/f');
  body.children[0].removeAttribute('data-cursor');
  body.children[1].setAttribute('data-cursor', '');
  assert.equal(T.open(), true);
  await answer('/rows/P/', 'x');
  assert.deepEqual(shown(body), ['f', 'P/', 'x']);
  assert.equal(T.open(), true);
  assert.deepEqual(shown(body), ['f', 'P/']);
});

test('h on a top-level row goes to the parent page, when there is one', async () => {
  pending.clear();
  const body = table('f');
  globalThis.location.href = '';
  T.key(false);
  assert.equal(nav(), '', 'at the root there is nowhere to go');
  body.setAttribute('data-up', '/tree/main');
  T.key(false);
  assert.equal(nav(), '/tree/main');
});

test('the keys are not the tree\'s while the finder shows its results', async () => {
  pending.clear();
  const body = table('P/', 'f');
  body.removeAttribute('data-rows');
  assert.equal(T.active(), false);
  assert.equal(T.key(true), false);
  assert.equal(T.open(), false);
  assert.deepEqual(pending.size, 0);
});

test('a click on the name puts the cursor on the row', async () => {
  pending.clear();
  const body = table('a', 'b');
  click(body.children[1], {}, body.children[1].querySelector('a.t-name'));
  assert.equal(cursorName(body), 'b');
  assert.equal(body.children[0].hasAttribute('data-cursor'), false);
  // the click on the last commit is the link's own, and the row takes the cursor
  let prevented = false;
  const sub = body.children[0].querySelector('a.t-commit');
  T.click({ target: sub, button: 0, preventDefault() { prevented = true; } });
  assert.equal(prevented, false);
  assert.equal(cursorName(body), 'a', 'the cursor follows the click on the row, whatever the target');
});

// ---- focus: one model, a roving tabindex on the rows ---------------------------

const stops = (body) => body.children.filter((r) => r.getAttribute('tabindex') === '0').map((r) => r.name);
const subTab = (r) => r.querySelector('a.t-commit').getAttribute('tabindex');

test('init: one tab stop, the row under the cursor; name links are out of the tab order', () => {
  const body = table('a', 'P/', 'b');
  assert.deepEqual(stops(body), ['a']);
  for (const r of body.children) {
    assert.equal(r.querySelector('a.t-name').getAttribute('tabindex'), '-1', 'the row stands for its name');
    assert.equal(subTab(r), r.name === 'a' ? '0' : '-1', 'the commit link follows its row');
  }
  assert.equal(body.children[1].getAttribute('tabindex'), '-1');
});

test('l or h before any focus in the tree brings the focus to the cursor row', async () => {
  pending.clear();
  const body = table('P/', 'f');
  assert.equal(dom.active, null, 'the page has the focus after a load');
  T.key(true); // opens P/, the cursor stays on it
  assert.equal(dom.active, body.children[0]);
  await answer('/rows/P/', 'a');
  dom.active = null;
  T.key(false); // closes P/
  assert.equal(dom.active, body.children[0]);
});

test('l and h move the focus with the cursor, and the tab stop with them', async () => {
  pending.clear();
  const body = table('P/', 'f');
  T.key(true);
  await answer('/rows/P/', 'C/,a');
  const C = byName(body, 'C/');
  assert.equal(C.getAttribute('tabindex'), '-1', 'rows that come in are out of the tab order');
  assert.equal(C.querySelector('a.t-name').getAttribute('tabindex'), '-1');
  assert.equal(subTab(C), '-1');
  T.key(true); // into the first entry
  assert.equal(dom.active, C);
  assert.deepEqual(stops(body), ['C/']);
  assert.equal(subTab(C), '0');
  assert.equal(subTab(body.children[0]), '-1');
  T.key(false); // up to P/
  assert.equal(dom.active, body.children[0]);
  assert.deepEqual(stops(body), ['P/']);
});

test('sync (j k gg G of ermite.js) puts the focus and the tab stop on the row', () => {
  const body = table('a', 'b', 'c');
  body.children[0].removeAttribute('data-cursor');
  body.children[2].setAttribute('data-cursor', ''); // ermite.js moved the cursor
  T.sync(body.children[2]);
  assert.equal(dom.active, body.children[2]);
  assert.deepEqual(stops(body), ['c']);
  assert.equal(body.children.filter((r) => r.hasAttribute('data-cursor')).length, 1);
  T.sync(el('li')); // a row of another list: not the tree's
  assert.equal(dom.active, body.children[2]);
});

test('a click puts the focus and the tab stop on its row, directory or not', async () => {
  pending.clear();
  const body = table('a', 'b/');
  click(body.children[1]);
  assert.equal(dom.active, body.children[1]);
  assert.deepEqual(stops(body), ['b/']);
  await answer('/rows/b/', 'x');
  // a click on the commit link: the row takes the focus, the link keeps its own navigation
  let prevented = false;
  T.click({ target: body.children[0].querySelector('a.t-commit'), button: 0, preventDefault() { prevented = true; } });
  assert.equal(prevented, false);
  assert.equal(dom.active, body.children[0]);
  assert.deepEqual(stops(body), ['a']);
  // a modified click on the name too
  click(body.children[1], { ctrlKey: true });
  assert.equal(dom.active, body.children[1]);
});

test('collapsing a branch with the focus or the cursor in it moves both to the directory', async () => {
  pending.clear();
  const body = table('P/', 'f');
  T.key(true);
  await answer('/rows/P/', 'a,b');
  const a = byName(body, 'a');
  click(a); // the cursor and the focus on a
  assert.equal(dom.active, a);
  click(body.children[0]); // the directory closes
  assert.deepEqual(shown(body), ['P/', 'f']);
  assert.equal(dom.active, body.children[0], 'the focus leaves the hidden row');
  assert.deepEqual(stops(body), ['P/']);
  assert.ok(body.children[0].hasAttribute('data-cursor'));
  assert.equal(a.hasAttribute('data-cursor'), false);
  assert.equal(a.getAttribute('tabindex'), '-1');
});

test('collapsing a directory the focus is not in leaves the focus alone', async () => {
  pending.clear();
  const body = table('P/', 'f');
  T.key(true);
  await answer('/rows/P/', 'a');
  click(body.children[0]); // P/ has the cursor and the focus; closing it keeps them
  assert.equal(dom.active, body.children[0]);
  click(body.children[3] ?? body.children[2]); // f
  const f = byName(body, 'f');
  assert.equal(dom.active, f);
  T.key(false); // f: no parent in the page, no up: nothing
  assert.equal(dom.active, f);
});

test('Enter acts on the row that has the cursor, which is the focused row', async () => {
  pending.clear();
  const body = table('a', 'P/');
  T.sync(body.children[1]); // ermite.js moved the cursor and the focus to P/
  assert.equal(dom.active, body.children[1]);
  T.open();
  await answer('/rows/P/', 'x');
  assert.ok(isOpen(body.children[1]));
  assert.equal(body.children.filter((r) => r.hasAttribute('data-cursor')).length, 1);
});

// ---- ARIA: the level is set on the rows inserted; position and size come with them ----

test('inserted rows get their level; the server gave them posinset and setsize', async () => {
  pending.clear();
  const body = table('P/', 'f');
  T.key(true);
  await answer('/rows/P/', 'C/,a');
  assert.equal(byName(body, 'C/').getAttribute('aria-level'), '2');
  assert.equal(byName(body, 'C/').getAttribute('role'), 'treeitem');
  // a directory inside it: level 3
  click(byName(body, 'C/'));
  await answer('/rows/C/', 'z');
  assert.equal(byName(body, 'z').getAttribute('aria-level'), '3');
});

// ---- the arrow keys: the APG tree pattern, apart from the Vim keys ---------------

test('Down, Up, Home and End walk the visible rows, with the focus', async () => {
  pending.clear();
  const body = table('P/', 'f', 'g');
  T.arrow('ArrowRight');
  await answer('/rows/P/', 'a,b');
  T.arrow('ArrowRight'); // open: into the first entry
  assert.equal(cursorName(body), 'a');
  T.arrow('ArrowDown');
  assert.equal(cursorName(body), 'b');
  assert.equal(dom.active, byName(body, 'b'));
  T.arrow('ArrowUp');
  assert.equal(cursorName(body), 'a');
  T.arrow('End');
  assert.equal(cursorName(body), 'g');
  T.arrow('ArrowDown'); // the last row: stays
  assert.equal(cursorName(body), 'g');
  T.arrow('Home');
  assert.equal(cursorName(body), 'P/');
  T.arrow('ArrowUp');
  assert.equal(cursorName(body), 'P/');
  // a collapsed branch is skipped
  T.arrow('ArrowLeft');
  T.arrow('ArrowDown');
  assert.equal(cursorName(body), 'f');
  assert.deepEqual(stops(body), ['f']);
});

test('Right opens a closed directory, enters an open one, and does nothing on a file', async () => {
  pending.clear();
  globalThis.location.href = '';
  const body = table('P/', 'f');
  assert.equal(T.arrow('ArrowRight'), true);
  await answer('/rows/P/', 'a');
  assert.ok(isOpen(body.children[0]));
  assert.equal(cursorName(body), 'P/', 'opening keeps the cursor on the directory');
  T.arrow('ArrowRight');
  assert.equal(cursorName(body), 'a', 'on an open directory: the first entry');
  T.arrow('ArrowRight'); // a file: nothing, in particular it is not opened
  assert.equal(cursorName(body), 'a');
  assert.equal(nav(), '', 'a file is not opened by the arrow');
  assert.equal(pending.size, 1, 'no further request');
});

test('Left closes an open directory, goes to the parent, and does nothing at the top', async () => {
  pending.clear();
  globalThis.location.href = '';
  const body = table('P/', 'f');
  body.setAttribute('data-up', '/tree/main'); // a page to go up to: not for an arrow
  T.arrow('ArrowRight');
  await answer('/rows/P/', 'a');
  T.arrow('ArrowRight');
  T.arrow('ArrowLeft'); // a: to its directory
  assert.equal(cursorName(body), 'P/');
  T.arrow('ArrowLeft'); // P/ is open: closes
  assert.deepEqual(shown(body), ['P/', 'f']);
  T.arrow('ArrowLeft'); // closed and top level: nothing
  assert.equal(cursorName(body), 'P/');
  assert.equal(nav(), '', 'the arrow does not leave the page; h does');
  T.key(false);
  assert.equal(nav(), '/tree/main');
});

test('l opens a file where Right does not', () => {
  pending.clear();
  globalThis.location.href = '';
  const body = table('f');
  T.arrow('ArrowRight');
  assert.equal(nav(), '');
  T.key(true);
  assert.equal(nav(), '/blob/f');
  assert.ok(body);
});

test('the tree takes no arrow key while the finder shows its results, nor any other key', () => {
  const body = table('a', 'b');
  body.removeAttribute('data-rows');
  assert.equal(T.arrow('ArrowDown'), false);
  assert.equal(cursorName(body), 'a');
  body.setAttribute('data-rows', '');
  assert.equal(T.arrow('x'), false);
  assert.equal(T.arrow('PageDown'), false);
});
