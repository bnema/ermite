// Tests of the file tree of the rail of a file page (static/js/filenav.js) on
// the hand-rolled DOM of dom.mjs. Run with
//   node --test 'internal/adapters/web/jstest/*.test.mjs'
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { test } from 'node:test';
import { El, el, installDom, settle } from './dom.mjs';

const SRC = readFileSync(new URL('../static/js/filenav.js', import.meta.url), 'utf8');
const KEY = 'ermite:fnav:r/main';

// Elements take listeners; the document keeps its own.
El.prototype.addEventListener = function () {};

// A sessionStorage over a Map; raw lets a test store anything.
function storage(init = {}) {
  const m = new Map(Object.entries(init));
  return {
    getItem: (k) => (m.has(k) ? m.get(k) : null),
    setItem: (k, v) => m.set(k, String(v)),
    removeItem: (k) => m.delete(k),
    get raw() { return m; },
  };
}

// dir is a folder of the tree, closed and empty unless open.
const dir = (path, { open = false, items = [] } = {}) => {
  const d = el('details', { attrs: { 'data-src': '/r/tree/main/' + path + '?nav' } }, el('summary', {}, path + '/'), el('ul', { cls: 'fnav' }, ...items));
  d.open = open;
  return el('li', { attrs: { 'data-kind': 'dir', 'data-path': path } }, d);
};
const file = (path, current = false) =>
  el('li', { attrs: { 'data-kind': 'file', 'data-path': path } }, el('a', { attrs: current ? { href: '/r/blob/main/' + path, 'aria-current': 'page' } : { href: '/r/blob/main/' + path } }));

// setup builds a page with the tree, the storage and fetch given, then runs
// the script. fetch answers reply(url) or, by default, one file of the folder.
// It answers the page: the folders by path, the fetches, the navigations.
async function setup({ tree, store = {}, reply, body = { ref: 'main' }, path = '/r/blob/main/README.md' } = {}) {
  const parsed = [];
  const doc = installDom({ parseHTML: (text) => { parsed.push(text); return JSON.parse(text).map((p) => (p.startsWith('dir:') ? dir(p.slice(4)) : file(p))); } });
  const handlers = {};
  doc.addEventListener = (type, fn) => { (handlers[type] ??= []).push(fn); };
  for (const [k, v] of Object.entries(body)) doc.body.dataset[k] = v;
  const fetches = [], assigned = [];
  globalThis.location = { pathname: path, href: path, assign: (u) => assigned.push(u) };
  globalThis.sessionStorage = storage(store);
  globalThis.fetch = async (url) => {
    fetches.push(url);
    const r = reply ? await reply(url) : null;
    if (r instanceof Error) throw r;
    const dirPath = url.replace('/r/tree/main/', '').replace('?nav', '');
    return { ok: r?.ok ?? true, status: r?.status ?? 200, redirected: !!r?.redirected, text: async () => JSON.stringify(r?.items ?? [dirPath + '/f']) };
  };
  delete globalThis.ResizeObserver;
  const root = el('ul', { cls: 'fnav' }, ...(tree ?? [dir('src'), file('README.md', true)]));
  doc.body.append(el('aside', { cls: 'rail' }, el('section', {}, root)));
  delete globalThis.ermiteFileNav;
  (0, eval)(SRC);
  for (let i = 0; i < 6; i++) await settle();
  const details = (p) => root.querySelectorAll('li[data-kind="dir"]').find((li) => li.dataset.path === p)?.querySelector(':scope > details[data-src]');
  const fire = (type, target, extra = {}) => (handlers[type] ?? []).forEach((h) => h({ type, target, defaultPrevented: false, ...extra }));
  return { F: window.ermiteFileNav, root, details, fetches, assigned, fire, store: globalThis.sessionStorage };
}

test('opening a folder loads its entries once', async () => {
  const p = await setup();
  const d = p.details('src');
  d.open = true;
  p.fire('toggle', d);
  p.fire('toggle', d);
  await settle();
  assert.deepEqual(p.fetches, ['/r/tree/main/src?nav']);
  assert.equal(d.querySelector(':scope > ul.fnav').children.length, 1);
  assert.ok(d.hasAttribute('data-loaded'));
  d.open = false;
  p.fire('toggle', d);
  d.open = true;
  p.fire('toggle', d);
  await settle();
  assert.equal(p.fetches.length, 1, 'a loaded folder is not fetched again');
});

for (const [name, reply] of [
  ['a non-OK answer', { ok: false, status: 500 }],
  ['a redirected answer', { redirected: true }],
  ['a network error', new Error('offline')],
  ['an answer without entries', { items: [] }],
]) {
  test(`a click load that fails on ${name} closes the folder and goes to its page`, async () => {
    const p = await setup({ reply: () => reply });
    const d = p.details('src');
    d.open = true;
    assert.equal(await p.F.load(d), false);
    assert.equal(d.open, false);
    assert.deepEqual(p.assigned, ['/r/tree/main/src']);
    assert.equal(d.hasAttribute('aria-busy'), false);
  });
}

test('a restore that fails stays on the page, closes the folder and forgets it on an HTTP error', async () => {
  const p = await setup({ store: { [KEY]: '["src","docs"]' }, tree: [dir('src'), dir('docs'), file('README.md', true)], reply: (u) => (u.includes('/src') ? { ok: false, status: 404 } : null) });
  assert.deepEqual(p.assigned, []);
  assert.equal(p.details('src').open, false);
  assert.equal(p.details('docs').open, true);
  assert.deepEqual(p.F.read(), ['docs'], 'the refused folder is forgotten');
});

test('a restore that fails on a network error keeps the folder remembered', async () => {
  const p = await setup({ store: { [KEY]: '["src"]' }, reply: () => new Error('offline') });
  assert.deepEqual(p.assigned, []);
  assert.equal(p.details('src').open, false);
  assert.deepEqual(p.F.read(), ['src']);
});

test('restore opens parents before their children', async () => {
  // src/lib is in the page only once src is loaded; it was remembered first.
  const p = await setup({
    store: { [KEY]: '["src/lib","src"]' },
    reply: (u) => (u.endsWith('/src?nav') ? { items: ['dir:src/lib'] } : null),
  });
  assert.deepEqual(p.fetches, ['/r/tree/main/src?nav', '/r/tree/main/src/lib?nav']);
  assert.equal(p.details('src').open, true);
  assert.equal(p.details('src/lib').open, true);
  assert.deepEqual(p.F.read(), ['src/lib', 'src'], 'a restore changes nothing remembered');
});

test('a toggle and a restore of the same folder share one fetch', async () => {
  let release;
  const gate = new Promise((r) => { release = r; });
  const p = await setup({ reply: () => gate.then(() => null) });
  const d = p.details('src');
  d.open = true;
  p.fire('toggle', d);
  const restored = p.F.load(d, true);
  p.fire('toggle', d);
  release();
  assert.equal(await restored, true);
  await settle();
  assert.deepEqual(p.fetches, ['/r/tree/main/src?nav']);
  assert.equal(d.querySelector(':scope > ul.fnav').children.length, 1, 'the entries are inserted once');
});

test('read ignores what is not a list of paths', async () => {
  for (const [raw, want] of [['{bad', []], ['{"a":1}', []], ['"src"', []], ['null', []], ['["src",1,null,{"x":1},"docs"]', ['src', 'docs']]]) {
    // No folder of the page matches: the restore opens nothing.
    const p = await setup({ store: { [KEY]: raw }, tree: [file('README.md', true)] });
    assert.deepEqual(p.F.read(), want, raw);
  }
});

test('a click on a summary remembers an open and forgets a close', async () => {
  const p = await setup();
  const d = p.details('src');
  const summary = d.querySelector('summary');
  // The click comes before the toggle: open is still the old state.
  p.fire('click', summary);
  assert.deepEqual(p.F.read(), ['src']);
  d.open = true;
  p.fire('click', summary);
  assert.deepEqual(p.F.read(), []);
  p.fire('click', summary, { defaultPrevented: true });
  assert.deepEqual(p.F.read(), [], 'a prevented click is not a choice');
  p.fire('click', p.root.querySelector('a'));
  assert.deepEqual(p.F.read(), [], 'a file link is not a folder');
});

test('the remembered folders keep the newest MAX', async () => {
  const p = await setup();
  for (let i = 0; i < p.F.MAX + 5; i++) p.F.remember('d' + i, true);
  const got = p.F.read();
  assert.equal(got.length, p.F.MAX);
  assert.equal(got[0], 'd5');
  assert.equal(got.at(-1), 'd' + (p.F.MAX + 4));
  p.F.remember('d5', true);
  assert.equal(p.F.read().at(-1), 'd5', 'a folder opened again is the newest');
});

test('the key is per repository and ref; an abbreviated detached commit uses the ref of the URL', async () => {
  assert.equal((await setup()).F.key, KEY);
  assert.equal((await setup({ body: { sha: 'c0ffee0c0ffee0c0ffee0c0ffee0c0ffee0c0ffe' } })).F.key, 'ermite:fnav:r/c0ffee0c0ffee0c0ffee0c0ffee0c0ffee0c0ffe');
  assert.equal((await setup({ body: {}, path: '/r/blob/abc1234/src/a.go' })).F.key, 'ermite:fnav:r/abc1234');
});
