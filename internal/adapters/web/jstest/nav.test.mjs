// Tests of the owner navigation of static/js/ermite.js (window.ermiteNav).
// Only the block between its markers is evaluated, with stubbed globals. Run with
//   node --test 'internal/adapters/web/jstest/*.test.mjs'
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { test } from 'node:test';

const src = readFileSync(new URL('../static/js/ermite.js', import.meta.url), 'utf8');
const block = src.slice(src.indexOf('  // 11. Owner navigation'), src.indexOf('  // end owner navigation'));

const ORIGIN = 'https://forge.test';
const el = (name, extra = {}) => ({ name, ...extra });

function setup({ shell = false, current = ['#owner-nav', '#view', '#panel-cmd', '.hint-live'], pages = {} } = {}) {
  const log = [];
  const listeners = {}, winListeners = {};
  const g0 = {};
  const focusable = (name) => ({ focus(o) { log.push(['focus', name, o]); g0.doc.activeElement = this; } });
  const h1 = focusable('h1');
  const main = focusable('main');
  // A scroll position that logs its writes, for #main and the document.
  const scrolls = (box, name, top) => {
    let v = top;
    Object.defineProperty(box, 'scrollTop', { get: () => v, set: (y) => { v = y; log.push(['scroll', name, y]); } });
    return box;
  };
  scrolls(main, 'main', 120);
  const root = scrolls({}, 'root', 80);
  const links = [Object.assign(focusable('link'), { href: ORIGIN + '/-/activity?range=7d' })];
  const doc = Object.fromEntries(current.map((s) => [s, el('old ' + s)]));
  const filter = { value: 'abc' };
  const g = {
    location: { href: ORIGIN + '/', origin: ORIGIN, assign: (h) => log.push(['assign', h]) },
    history: {
      pushState: (st, _, h) => log.push(['push', st, h]),
      replaceState: (st, _, h) => log.push(['replace', st, h]),
    },
    document: {
      title: 'old',
      body: {},
      scrollingElement: root,
      activeElement: null,
      querySelector: (s) => (s === '#view h1' ? h1 : s === '#main' ? main : doc[s] || null),
      querySelectorAll: (s) => (s === '#view input[data-filter]' ? [filter, { value: '' }] : s === '#view a[href]' ? links : []),
      addEventListener: (n, fn) => { listeners[n] = fn; },
    },
    DOMParser: class { parseFromString(html) {
      const p = pages[html];
      return { title: 'T ' + html, querySelector: (s) => (p.includes(s) ? el('new ' + s) : null) };
    } },
    fetch: async (href, opts) => {
      log.push(['fetch', href, opts.headers.Accept]);
      const r = g.respond(href);
      return { ok: r.ok ?? true, redirected: !!r.redirected, text: async () => { await r.wait; if (r.throw) throw new Error('x'); return r.html; } };
    },
    respond: () => ({ html: 'full' }),
    getComputedStyle: (e) => ({ overflowY: e === main && shell ? 'auto' : 'visible' }),
    addEventListener: (n, fn) => { winListeners[n] = fn; },
  };
  g0.doc = g.document;
  g.document.activeElement = g.document.body;
  g.window = {
    ermiteLive: { replaceRegion: async (o, f) => log.push(['region', o.name, f.name]) },
    ermiteFit: { all: () => log.push(['fit']) },
    ermite: { hide: (i) => log.push(['hide', i.value]) },
  };
  const names = Object.keys(g);
  const nav = new Function(...names, block.replaceAll('window.ermiteNav =', 'return'))(...names.map((n) => g[n]));
  return { nav, g, log, listeners, winListeners, h1, doc };
}

const all = ['#owner-nav', '#view', '#panel-cmd', '.hint-live'];
const click = (over = {}) => {
  let prevented = false;
  const a = { href: ORIGIN + '/-/work', target: '', dataset: { nav: '' }, hasAttribute: () => false };
  return { e: { target: { closest: () => a }, defaultPrevented: false, button: 0, ctrlKey: false, metaKey: false, shiftKey: false, altKey: false,
    preventDefault() { prevented = true; }, ...over }, a, prevented: () => prevented };
};
const flush = () => new Promise((r) => setTimeout(r, 0));

test('load records the owner page in history', () => {
  const { log } = setup();
  assert.deepEqual(log, [['replace', { ermiteNav: 1 }, ORIGIN + '/']]);
});

test('a modified or non-primary click is not intercepted', async () => {
  for (const over of [{ ctrlKey: true }, { metaKey: true }, { shiftKey: true }, { altKey: true }, { button: 1 }]) {
    const { listeners, log } = setup({ pages: { full: all } });
    log.length = 0;
    const c = click(over);
    listeners.click(c.e);
    await flush();
    assert.equal(c.prevented(), false);
    assert.deepEqual(log, []);
  }
});

test('a plain click swaps the regions in order, then pushes, titles, focuses and re-runs initialisers', async () => {
  const { listeners, log, g } = setup({ pages: { full: all } });
  log.length = 0;
  const c = click();
  listeners.click(c.e);
  await flush();
  assert.equal(c.prevented(), true);
  assert.deepEqual(log, [
    ['fetch', ORIGIN + '/-/work', 'text/html'],
    ['region', 'old .hint-live', 'new .hint-live'],
    ['region', 'old #owner-nav', 'new #owner-nav'],
    ['region', 'old #view', 'new #view'],
    ['region', 'old #panel-cmd', 'new #panel-cmd'],
    ['replace', { ermiteNav: 1, y: 80 }, undefined],
    ['push', { ermiteNav: 1 }, ORIGIN + '/-/work'],
    ['scroll', 'root', 0],
    ['focus', 'h1', { preventScroll: true }],
    ['fit'],
    ['hide', 'abc'],
  ]);
  assert.equal(g.document.title, 'T full');
});

test('keep-scroll links neither scroll nor move the focus', async () => {
  const { listeners, log } = setup({ pages: { full: all } });
  log.length = 0;
  const c = click();
  c.a.dataset.nav = 'keep-scroll';
  c.a.href = ORIGIN + '/-/activity?range=7d';
  listeners.click(c.e);
  await flush();
  assert.ok(log.some((l) => l[0] === 'push'));
  assert.ok(!log.some((l) => l[0] === 'scroll'));
  assert.deepEqual(log.filter((l) => l[0] === 'focus'), [['focus', 'link', { preventScroll: true }]]);
});

test('a cross-origin link is not intercepted', async () => {
  const { listeners, log } = setup({ pages: { full: all } });
  log.length = 0;
  const c = click();
  c.a.href = 'https://elsewhere.test/x';
  listeners.click(c.e);
  await flush();
  assert.equal(c.prevented(), false);
  assert.deepEqual(log, []);
});

for (const [name, opts] of [
  ['a refused response', { respond: () => ({ ok: false, html: 'full' }) }],
  ['a redirected response', { respond: () => ({ redirected: true, html: 'full' }) }],
  ['a page without #view', { respond: () => ({ html: 'noview' }) }],
  ['a page with a stream on one side only', { respond: () => ({ html: 'nolive' }) }],
  ['a page without the owner nav', { respond: () => ({ html: 'nonav' }) }],
  ['a thrown error', { respond: () => ({ throw: true }) }],
]) {
  test(`${name} falls back to a full load`, async () => {
    const { nav, log, g } = setup({ pages: { full: all, noview: ['#owner-nav'], nolive: ['#owner-nav', '#view', '#panel-cmd'], nonav: ['#view', '#panel-cmd', '.hint-live'] } });
    g.respond = opts.respond;
    log.length = 0;
    await nav.go(ORIGIN + '/-/deps', { push: true });
    assert.deepEqual(log.filter((l) => l[0] !== 'fetch'), [['assign', ORIGIN + '/-/deps']]);
    assert.ok(!log.some((l) => l[0] === 'region'), 'nothing swapped before the fallback');
  });
}

test('a response superseded by a newer navigation is ignored', async () => {
  const { nav, log, g } = setup({ pages: { a: all, b: all } });
  let release;
  const slow = new Promise((r) => { release = r; });
  g.respond = (href) => (href.endsWith('/a') ? { html: 'a', wait: slow } : { html: 'b' });
  log.length = 0;
  const first = nav.go(ORIGIN + '/a', { push: true });
  await nav.go(ORIGIN + '/b', { push: true });
  release();
  await first;
  assert.deepEqual(log.filter((l) => l[0] === 'push'), [['push', { ermiteNav: 1 }, ORIGIN + '/b']]);
  assert.equal(g.document.title, 'T b');
  assert.ok(!log.some((l) => l[0] === 'assign'));
});

test('popstate of an owner page swaps without pushing; other states are left alone', async () => {
  const { winListeners, log, g } = setup({ pages: { full: all } });
  g.location.href = ORIGIN + '/-/inbox';
  log.length = 0;
  winListeners.popstate({ state: null });
  await flush();
  assert.deepEqual(log, []);
  winListeners.popstate({ state: { ermiteNav: 1, y: 300 } });
  await flush();
  assert.equal(log[0][1], ORIGIN + '/-/inbox');
  assert.ok(log.some((l) => l[0] === 'region'));
  assert.ok(!log.some((l) => l[0] === 'push' || l[0] === 'replace'));
  assert.deepEqual(log.filter((l) => l[0] === 'scroll'), [['scroll', 'root', 300]], 'only the saved position');
});

for (const [name, slow] of [['refused', { ok: false }], ['throwing', { throw: true }]]) {
  test(`a superseded ${name} navigation does not fall back`, async () => {
    const { nav, log, g } = setup({ pages: { b: all } });
    let release;
    const wait = new Promise((r) => { release = r; });
    g.respond = (href) => (href.endsWith('/a') ? { html: 'b', wait, ...slow } : { html: 'b' });
    log.length = 0;
    const first = nav.go(ORIGIN + '/a', { push: true });
    await nav.go(ORIGIN + '/b', { push: true });
    release();
    await first;
    assert.ok(!log.some((l) => l[0] === 'assign'));
  });
}

test('a navigation superseded during the swaps stops there', async () => {
  const { nav, log, g } = setup({ pages: { full: all } });
  let second;
  g.window.ermiteLive.replaceRegion = async (o, f) => {
    log.push(['region', o.name, f.name]);
    if (!second) second = nav.go(ORIGIN + '/b');
  };
  log.length = 0;
  assert.equal(await nav.go(ORIGIN + '/a', { push: true }), false);
  await second;
  assert.deepEqual(log.filter((l) => l[0] === 'push'), []);
  assert.ok(!log.some((l) => l[0] === 'assign'));
});

test('a focus that lands nowhere moves to #main', async () => {
  const { nav, log, h1 } = setup({ pages: { full: all } });
  h1.focus = () => {};
  log.length = 0;
  await nav.go(ORIGIN + '/x');
  assert.deepEqual(log.filter((l) => l[0] === 'focus'), [['focus', 'main', { preventScroll: true }]]);
});

test('clicks the browser should handle itself are left alone', async () => {
  for (const tweak of [(c) => { c.e.defaultPrevented = true; }, (c) => { c.a.target = '_blank'; }, (c) => { c.a.hasAttribute = (n) => n === 'download'; }]) {
    const { listeners, log } = setup({ pages: { full: all } });
    log.length = 0;
    const c = click();
    tweak(c);
    listeners.click(c.e);
    await flush();
    assert.equal(c.prevented(), false);
    assert.deepEqual(log, []);
  }
});

test('a click on the current page swaps and replaces the history entry', async () => {
  const { listeners, log, g } = setup({ pages: { full: all } });
  g.location.href = ORIGIN + '/-/work';
  log.length = 0;
  const c = click();
  c.a.target = '_self';
  listeners.click(c.e);
  await flush();
  assert.equal(c.prevented(), true);
  assert.ok(!log.some((l) => l[0] === 'push'));
  assert.deepEqual(log.filter((l) => l[0] === 'replace'), [['replace', { ermiteNav: 1 }, ORIGIN + '/-/work']]);
});

test('in the app shell #main is the scroller: saved, reset and restored there', async () => {
  const { nav, winListeners, log, g } = setup({ shell: true, pages: { full: all } });
  log.length = 0;
  await nav.go(ORIGIN + '/-/work', { push: true });
  assert.deepEqual(log.filter((l) => l[0] === 'replace' || l[0] === 'scroll'),
    [['replace', { ermiteNav: 1, y: 120 }, undefined], ['scroll', 'main', 0]]);
  g.location.href = ORIGIN + '/';
  log.length = 0;
  winListeners.popstate({ state: { ermiteNav: 1, y: 120 } });
  await flush();
  assert.deepEqual(log.filter((l) => l[0] === 'scroll'), [['scroll', 'main', 120]]);
});
