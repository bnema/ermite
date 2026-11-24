// Tests of the owner navigation of static/js/ermite.js (window.ermiteNav).
// Only the block between its markers is evaluated, with stubbed globals. Run with
//   node --test 'internal/adapters/web/jstest/*.test.mjs'
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { test } from 'node:test';

const src = readFileSync(new URL('../static/js/ermite.js', import.meta.url), 'utf8');
const block = src.slice(src.indexOf('  // N. Owner navigation'), src.indexOf('  // end owner navigation'));

const ORIGIN = 'https://forge.test';
const el = (name, extra = {}) => ({ name, ...extra });

function setup({ current = ['#owner-nav', '#view', '#panel-cmd', '.hint-live'], pages = {} } = {}) {
  const log = [];
  const listeners = {}, winListeners = {};
  const h1 = { focus: (o) => log.push(['focus', o]) };
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
      querySelector: (s) => (s === '#view h1' ? h1 : doc[s] || null),
      querySelectorAll: (s) => (s === '#view input[data-filter]' ? [filter, { value: '' }] : []),
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
    scrollTo: (x, y) => log.push(['scroll', x, y]),
    addEventListener: (n, fn) => { winListeners[n] = fn; },
  };
  g.window = {
    ermiteLive: { replaceRegion: async (o, f) => log.push(['region', o.name, f.name]) },
    ermiteFit: { all: () => log.push(['fit']) },
    ermite: { hide: (i) => log.push(['hide', i.value]) },
  };
  const names = Object.keys(g);
  const nav = new Function(...names, block.replaceAll('window.ermiteNav =', 'return'))(...names.map((n) => g[n]));
  return { nav, g, log, listeners, winListeners };
}

const all = ['#owner-nav', '#view', '#panel-cmd', '.hint-live'];
const click = (over = {}) => {
  let prevented = false;
  const a = { href: ORIGIN + '/-/work', dataset: { nav: '' } };
  return { e: { target: { closest: () => a }, button: 0, ctrlKey: false, metaKey: false, shiftKey: false, altKey: false,
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
    ['region', 'old #owner-nav', 'new #owner-nav'],
    ['region', 'old #view', 'new #view'],
    ['region', 'old #panel-cmd', 'new #panel-cmd'],
    ['region', 'old .hint-live', 'new .hint-live'],
    ['push', { ermiteNav: 1 }, ORIGIN + '/-/work'],
    ['scroll', 0, 0],
    ['focus', { preventScroll: true }],
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
  listeners.click(c.e);
  await flush();
  assert.ok(log.some((l) => l[0] === 'push'));
  assert.ok(!log.some((l) => l[0] === 'scroll' || l[0] === 'focus'));
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
  ['a thrown error', { respond: () => ({ throw: true }) }],
]) {
  test(`${name} falls back to a full load`, async () => {
    const { nav, log, g } = setup({ pages: { full: all, noview: ['#owner-nav'], nolive: ['#owner-nav', '#view'] } });
    g.respond = opts.respond;
    log.length = 0;
    await nav.go(ORIGIN + '/-/deps', { push: true });
    assert.deepEqual(log.filter((l) => l[0] !== 'fetch'), [['assign', ORIGIN + '/-/deps']]);
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
  winListeners.popstate({ state: { ermiteNav: 1 } });
  await flush();
  assert.equal(log[0][1], ORIGIN + '/-/inbox');
  assert.ok(log.some((l) => l[0] === 'region'));
  assert.ok(!log.some((l) => l[0] === 'push'));
});
