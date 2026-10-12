// Tests of the tree rows of static/js/files.js (window.ermiteTree) on a tiny
// hand-rolled DOM. Run with
//   node --test 'internal/adapters/web/jstest/*.test.mjs'
import assert from 'node:assert/strict';
import { test } from 'node:test';
import { El, el, installDom } from './dom.mjs';

// A row is a name: a trailing "/" makes a directory with a link, "!x" a div
// that is not a row, "more" the tree-more row of a truncated directory.
function row(name, depth) {
  const tr = el('tr', { cls: name === 'more' ? 'tree-more' : '', attrs: { 'data-depth': depth } });
  tr.name = name;
  tr.append(el('td', { cls: 'c-kind', text: '▸' }));
  if (name.endsWith('/')) {
    tr.append(el('a', { href: '/tree/' + name, attrs: { 'data-expand': '/rows/' + name, 'aria-expanded': 'false' } }));
  }
  return tr;
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
const click = (tr, mods = {}) => {
  const a = tr.querySelector('a[data-expand]');
  let prevented = false;
  T.click({ target: a, button: 0, preventDefault() { prevented = true; }, ...mods });
  return !prevented;
};
const kindOf = (tr) => tr.querySelector('.c-kind').textContent;

function table(...names) {
  return el('tbody', {}, ...names.map((n) => row(n, 0)));
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
  assert.equal(kindOf(body.children[0]), '▾');
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
  assert.equal(C.querySelector('a[data-expand]').getAttribute('aria-expanded'), 'true', 'C is open and loaded');
  assert.equal(kindOf(C), '▾');

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
