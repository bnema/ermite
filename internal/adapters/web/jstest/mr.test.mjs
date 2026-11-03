// Tests of the pure core of the merge request page (static/js/mr.js). Run with
//   node --test 'internal/adapters/web/jstest/*.test.mjs'
import assert from 'node:assert/strict';
import { test } from 'node:test';

globalThis.window = globalThis;
globalThis.document = { readyState: 'complete', querySelector: () => null, querySelectorAll: () => [] };
await import('../static/js/lazy.js');
await import('../static/js/mr.js');
const C = window.ermiteMR.core;
const railPrefs = new Map();
window.ermiteStore = { get: k => railPrefs.get(k), set: (k, v) => railPrefs.set(k, v) };

test('windowOf: the start of the window of a file', () => {
  assert.equal(C.windowOf('f1', 25), 0);
  assert.equal(C.windowOf('f25', 25), 0);
  assert.equal(C.windowOf('f26', 25), 25);
  assert.equal(C.windowOf('f84', 25), 75);
  assert.equal(C.windowOf('c3', 25), -1, 'not a file');
  assert.equal(C.windowOf('f0', 25), -1);
});

test('menuKey: modes by number, D toggles the deletion, nothing else', () => {
  assert.deepEqual(C.menuKey('1', 2), { mode: 0 });
  assert.deepEqual(C.menuKey('2', 2), { mode: 1 });
  assert.equal(C.menuKey('3', 2), null, 'no third mode');
  assert.equal(C.menuKey('0', 2), null);
  assert.deepEqual(C.menuKey('D', 2), { toggle: true });
  assert.equal(C.menuKey('d', 2), null, 'd is the split layout');
  assert.equal(C.menuKey('Enter', 2), null, 'the browser submits: the focus is on the button');
  assert.equal(C.menuKey('M', 2), null);
});

test('merge prefs: one key per repository, tolerant parsing', () => {
  assert.equal(C.mergePrefsKey('/team/app/merge-requests/12/merge'), 'merge-prefs:/team/app');
  assert.equal(C.mergePrefsKey('/app/merge-requests/3/merge'), 'merge-prefs:/app');
  assert.equal(C.mergePrefsKey('/app/merge-requests/3/auto-merge'), null);
  assert.equal(C.mergePrefsKey(null), null);
  assert.deepEqual(C.parseMergePrefs('{"mode":"squash","del":true}'), { mode: 'squash', del: true });
  assert.deepEqual(C.parseMergePrefs('{"mode":3,"del":"yes"}'), { mode: null, del: false });
  assert.equal(C.parseMergePrefs(null), null);
  assert.equal(C.parseMergePrefs('nope'), null);
});

test('step: stays in the list', () => {
  assert.equal(C.step(0, 1, 3), 1);
  assert.equal(C.step(2, 1, 3), 2);
  assert.equal(C.step(0, -1, 3), 0);
  assert.equal(C.step(-1, 1, 3), 0);
  assert.equal(C.step(-1, -1, 3), 2);
  assert.equal(C.step(0, 1, 0), -1);
});

test('rangeURL requires the same file, side and MR and sorts endpoints', () => {
 const base = 'https://forge.test/demo/merge-requests/1';
 const a = base+'?on=new:24:dir%2Fa.go#f1';
 const b = base+'?from=25&on=new:10:dir%2Fa.go#f1';
 const u = new URL(C.rangeURL(a,b,base));
 assert.equal(u.searchParams.get('on'),'new:10-24:dir/a.go');
 assert.equal(u.searchParams.get('from'),'25');
 assert.equal(u.hash,'#f1');
 assert.equal(C.rangeURL(a,a,base),null);
 assert.equal(C.rangeURL(a,b.replace('new:','old:'),base),null);
 assert.equal(C.rangeURL(a,b.replace('a.go','b.go'),base),null);
 assert.equal(C.rangeURL(a,b.replace('/1?','/2?'),base),null);
});

test('quote opens the thread reply and prefills and focuses its textarea', () => {
 let focused=false, input=false;
 const field={value:'',focus(){focused=true;},dispatchEvent(e){input=e.type==='input' && e.bubbles;}};
 const details={open:false,querySelector(){return field;}};
 const thread={querySelector(){return details;}};
 const button={dataset:{quote:'> source\n\n^2 '},closest(){return {closest(){return thread;}};}};
 assert.equal(C.quote(button),true);
 assert.equal(details.open,true);
 assert.equal(field.value,'> source\n\n^2 ');
 assert.equal(focused,true);
 assert.equal(input,true);
});

for (const dataset of [{diff:'split'}, {view:'split'}]) test(`reveal selects the split note (${JSON.stringify(dataset)}), including after lazy loading`, async () => {
 const saved = globalThis.document;
 const savedRAF = globalThis.requestAnimationFrame;
 let scrolled = '';
 const details = {open:false};
 const node = id => ({id, querySelector(){return null;}, closest(){return details;}, scrollIntoView(){scrolled=id;}});
 const nodes = new Map([['c12',node('c12')],['c12-s',node('c12-s')]]);
 const ph = {dataset:{notes:'c12'}};
 globalThis.document = {
   querySelector: sel => sel === '#diffs' ? {dataset} : null,
   querySelectorAll: () => [ph],
   getElementById: id => nodes.get(id),
 };
 globalThis.requestAnimationFrame = fn => fn();
 try {
   assert.equal(window.ermiteMR.reveal('c12'),false);
   assert.equal(scrolled,'c12-s');
   assert.equal(details.open,true);
   nodes.clear(); scrolled=''; details.open=false;
   assert.equal(window.ermiteMR.reveal('c12'),true);
   nodes.set('c12',node('c12')); nodes.set('c12-s',node('c12-s'));
   await Promise.resolve();
   assert.equal(scrolled,'c12-s');
   assert.equal(details.open,true);
 } finally {
   globalThis.document=saved;
   globalThis.requestAnimationFrame=savedRAF;
 }
});

function footerHarness(extra = {}) {
  const saved = Object.fromEntries(['document', 'location', 'addEventListener', 'history', 'MutationObserver'].map((k) => [k, globalThis[k]]));
  const node = (id) => ({ id, attrs: {}, events: {}, hidden: false,
    setAttribute(k, v) { this.attrs[k] = v; }, getAttribute(k) { return this.attrs[k] ?? null; },
    addEventListener(k, v) { this.events[k] = v; }, focus() { this.focused = true; },
    scrollIntoView() { this.scrolled = (this.scrolled ?? 0) + 1; } });
  const tabs = [node('mr-tab-commits'), node('mr-tab-checks')];
  const panels = [node('mr-commits'), node('mr-checks-panel')];
  const footer = node('footer'); footer.querySelectorAll = () => panels;
  const nav = node('tabs'); nav.querySelectorAll = () => tabs;
  const listeners = {}, replaced = [];
  globalThis.document = { querySelector: (s) => s === '.mr-footer' ? footer : s === '[data-mr-tabs]' ? nav : s === '#mr-ck' ? extra.summary ?? null : null,
    querySelectorAll: (s) => s === '[data-mr-tabs] button' ? tabs : [], addEventListener(k, v) { listeners[k] = v; } };
  globalThis.location = { hash: '', pathname: '/demo/merge-requests/1', search: '?x=1' };
  globalThis.addEventListener = (k, v) => { listeners[k] = v; };
  globalThis.history = { replaceState: (...args) => replaced.push(args) };
  const restore = () => { for (const [k, v] of Object.entries(saved)) { if (v === undefined) delete globalThis[k]; else globalThis[k] = v; } };
  const state = () => panels.map((p) => !p.hidden);
  return { tabs, panels, footer, listeners, replaced, restore, state };
}

test('footer tabs: closed by default, toggle, switch, arrows, hash', () => {
  const h = footerHarness();
  try {
    window.ermiteMR.panels();
    assert.deepEqual(h.state(), [false, false], 'initially closed');
    assert.equal(h.tabs[0].attrs['aria-expanded'], 'false');
    assert.deepEqual(h.replaced, [], 'initial state leaves the URL alone');
    h.tabs[1].events.click({ preventDefault() {} });
    assert.deepEqual(h.state(), [false, true]);
    assert.equal(h.tabs[1].attrs['aria-expanded'], 'true');
    assert.deepEqual(h.replaced, [[null, '', '#mr-checks-panel']]);
    location.hash = '#mr-checks-panel';
    h.tabs[1].events.click({ preventDefault() {} });
    assert.deepEqual(h.state(), [false, false], 'a second click closes');
    assert.deepEqual(h.replaced.at(-1), [null, '', '/demo/merge-requests/1?x=1'], 'closing clears the fragment');
    location.hash = '';
    h.tabs[1].events.click({ preventDefault() {} });
    h.tabs[0].events.click({ preventDefault() {} });
    assert.deepEqual(h.state(), [true, false], 'the other tab switches');
    h.tabs[0].events.keydown({ key: 'ArrowRight', preventDefault() {} });
    assert.equal(h.tabs[1].focused, true, 'arrows move focus only');
    assert.deepEqual(h.state(), [true, false]);
    location.hash = '#mr-checks'; h.replaced.length = 0; h.listeners.hashchange();
    assert.deepEqual(h.state(), [false, true], 'hash opens checks');
    assert.deepEqual(h.replaced, [], 'hash navigation does not rewrite the URL');
    location.hash = '#mr-commits'; h.listeners.hashchange();
    assert.deepEqual(h.state(), [true, false]);
    assert.equal(C.tabIndex('Home', 1, 2), 0);
    assert.equal(C.tabIndex('End', 0, 2), 1);
    assert.equal(C.tabIndex('ArrowLeft', 0, 2), 1);
    assert.equal(C.tabIndex('Escape', 0, 2), null);
  } finally { h.restore(); }
});

test('footer: #mr-ck syncs the checks label; its click opens Checks without toggling', () => {
  const text0 = { v: '' }; let writes = 0;
  const label = { get textContent() { return text0.v; }, set textContent(v) { writes++; text0.v = v; } };
  const bar = {};
  const summary = { title: '1/2 failed', textContent: ' 1/2 ', closest: (s) => s === '.mr-bar' ? bar : null };
  const extra = { summary };
  const h = footerHarness(extra);
  let observed = null, callback = null;
  globalThis.MutationObserver = class { constructor(fn) { callback = fn; } observe(target, opts) { observed = [target, opts]; } };
  const nav = document.querySelector('[data-mr-tabs]');
  nav.querySelector = (s) => s === '[data-checks-label]' ? label : null;
  try {
    window.ermiteMR.panels();
    assert.equal(label.textContent, '1/2 failed', 'initial label sync');
    assert.equal(observed[0], bar);
    assert.deepEqual(observed[1], { childList: true, subtree: true }, 'the stable bar observes action and badge replacements');
    // Replace #mr-actions, including its badge, then replace the badge alone.
    extra.summary = { title: 'checks running', textContent: '' }; callback();
    assert.equal(label.textContent, 'checks running');
    extra.summary = { title: 'checks passed', textContent: '' }; callback();
    assert.equal(label.textContent, 'checks passed');
    extra.summary = summary; callback();
    const before = writes; callback(); callback();
    assert.equal(writes, before, 'an unchanged label is not written');
    summary.title = ''; callback();
    assert.equal(label.textContent, '1/2');
    summary.textContent = ' '; callback();
    assert.equal(label.textContent, '', 'no checks: the tab is "Checks" alone');
    summary.textContent = ' no checks '; callback();
    assert.equal(label.textContent, '', 'the empty summary of the bar is not repeated');
    const click = { target: { closest: (s) => s === '#mr-ck' ? summary : null } };
    h.listeners.click(click);
    assert.deepEqual(h.state(), [false, true]);
    h.listeners.click(click);
    assert.deepEqual(h.state(), [false, true], 'a second badge click keeps it open');
  } finally { h.restore(); }
});

test('footer: panels(index) restores the open panel without touching the URL', () => {
  const h = footerHarness();
  try {
    location.hash = '#mr-commits';
    window.ermiteMR.panels(1);
    assert.deepEqual(h.state(), [false, true], 'a restore ignores the fragment');
    assert.equal(h.tabs[1].attrs['aria-expanded'], 'true');
    assert.deepEqual(h.replaced, []);
    assert.equal(h.panels[0].scrolled, undefined, 'a restore does not scroll');
    assert.equal(h.panels[1].scrolled, undefined);
    location.hash = '';
    window.ermiteMR.panels(-1);
    assert.deepEqual(h.state(), [false, false]);
  } finally { h.restore(); }
});

test('footer: scripts take over the panels; a fragment opens and scrolls to its panel', () => {
  const h = footerHarness();
  try {
    location.hash = '#mr-checks';
    window.ermiteMR.panels();
    assert.equal(h.footer.attrs['data-panels'], '', 'the CSS fallback of the fragment stops');
    assert.deepEqual(h.state(), [false, true]);
    assert.equal(h.panels[1].scrolled, 1, 'the panel of the fragment is brought in');
    location.hash = '#mr-commits'; h.listeners.hashchange();
    assert.deepEqual(h.state(), [true, false]);
    assert.equal(h.panels[0].scrolled, 1);
  } finally { h.restore(); }
});

test('touch range: first tap anchors, second tap opens the sorted range', () => {
  const base = 'https://forge.test/demo/merge-requests/1';
  const first = base + '?on=new:24:a.go#f1';
  const last = base + '?on=new:10:a.go#f1';
  assert.deepEqual(C.touchRange(null, first, base), { start: first, url: null });
  const result = C.touchRange(first, last, base);
  assert.equal(result.start, null);
  assert.equal(new URL(result.url).searchParams.get('on'), 'new:10-24:a.go');
  assert.deepEqual(C.touchRange(first, first, base), { start: null, url: first });
});

test('touch range: a different file, side or request becomes a new anchor', () => {
  const base = 'https://forge.test/demo/merge-requests/1';
  const first = base + '?on=new:24:a.go#f1';
  for (const next of [first.replace('new:', 'old:'), first.replace('a.go', 'b.go'), first.replace('/1?', '/2?')]) {
    assert.deepEqual(C.touchRange(first, next, base), { start: next, url: null });
  }
});

test('mobile unified fallback only redirects single-layout split pages and preserves query/hash', () => {
  const href = 'https://forge.test/demo/merge-requests/1?view=split&from=25&on=new:3:a.go#f26';
  assert.equal(C.mobileURL(href, 'split', false), null);
  assert.equal(C.mobileURL(href, undefined, true), null, 'both layouts are switched by CSS');
  assert.equal(C.mobileURL(href, 'unified', true), null, 'no redirect loop');
  const url = new URL(C.mobileURL(href, 'split', true));
  assert.equal(url.searchParams.get('view'), 'unified');
  assert.equal(url.searchParams.get('from'), '25');
  assert.equal(url.searchParams.get('on'), 'new:3:a.go');
  assert.equal(url.hash, '#f26');
});

test('touch control: lazy links enable toolbar; ordinary taps pass through; ranges navigate and cancel', async () => {
  const saved = Object.fromEntries(['document', 'location', 'addEventListener', 'localStorage', 'sessionStorage', 'fetch', 'matchMedia'].map((k) => [k, globalThis[k]]));
  const make = () => ({
    attrs: {}, events: {}, children: [], textContent: '',
    setAttribute(k, v) { this.attrs[k] = v; },
    removeAttribute(k) { delete this.attrs[k]; },
    addEventListener(k, fn) { this.events[k] = fn; },
    append(...nodes) { this.children.push(...nodes); },
    prepend(node) { this.children.unshift(node); },
    querySelector() { return null; }, querySelectorAll() { return []; },
  });
  const diffs = make();
  diffs.dataset = { diff: 'split' };
  const base = 'https://forge.test/demo/merge-requests/1';
  const line = (n) => Object.assign(make(), { href: `${base}?on=new:${n}:a.go#f1`, textContent: String(n) });
  const a = line(20), b = line(10);
  let loaded = false;
  diffs.querySelector = () => loaded ? a : null;
  let assigned = null;
  globalThis.document = {
    querySelector: (sel) => sel === '#diffs[data-mr]' || sel === '#diffs' ? diffs : null,
    querySelectorAll: () => [], createElement: (tag) => tag === 'template' ? { content: { querySelectorAll: () => [] } } : make(),
  };
  globalThis.location = { href: base, pathname: '/demo/merge-requests/1', hash: '', assign(url) { assigned = url; } };
  globalThis.addEventListener = () => {};
  const changes = {};
  const medias = {};
  globalThis.matchMedia = (q) => medias[q] ??= { matches: true, addEventListener(type, fn) { changes[q] = fn; } };
  const media = globalThis.matchMedia('(max-width: 720px)');
  const touchQuery = '(pointer: coarse), (hover: none)';
  const resize = () => changes['(max-width: 720px)']();
  globalThis.fetch = async () => { loaded = true; return { ok: true, status: 200, text: async () => '' }; };
  globalThis.localStorage = globalThis.sessionStorage = { getItem: () => null };
  const click = (link, modifiers = {}) => {
    const e = { target: { closest: () => link }, prevented: false, stopped: false,
      preventDefault() { this.prevented = true; }, stopPropagation() { this.stopped = true; }, ...modifiers };
    diffs.events.click(e);
    return e;
  };
  try {
    window.ermiteMR.start();
    const bar = diffs.children[0];
    const [button, status] = bar.children;
    assert.equal(bar.hidden, true, 'no commentable links in the initial window');
    assert.equal(typeof diffs.events.click, 'function', 'delegation installed before lazy load');
    await window.ermiteMR.load({ dataset: { src: '/files?from=25' } });
    assert.equal(bar.hidden, false, 'lazy window refresh reveals range control');
    assert.equal(diffs.dataset.mobile, 'true');
    assert.equal(diffs.dataset.diff, 'split', 'preferred layout is retained');
    media.matches = false;
    resize();
    assert.equal(diffs.dataset.mobile, 'false', 'desktop menu returns to preferred layout');
    assert.equal(diffs.dataset.diff, 'split');
    assert.equal(click(a).prevented, false, 'normal single-line navigation is untouched');
    button.events.click();
    assert.equal(button.attrs['aria-pressed'], 'true');
    assert.equal(click(a, { shiftKey: true }).prevented, false, 'desktop range selection is untouched');
    const event = click(a);
    assert.equal(event.prevented, true);
    assert.equal(event.stopped, true);
    assert.ok('data-range-start' in a.attrs);
    assert.match(status.textContent, /20/);
    click(b);
    assert.equal(new URL(assigned).searchParams.get('on'), 'new:10-20:a.go');
    assert.equal('data-range-start' in a.attrs, false);
    click(a);
    button.events.click();
    assert.equal(button.attrs['aria-pressed'], 'false');
    assert.equal(status.textContent, '');
    assert.equal('data-range-start' in a.attrs, false);
    assert.equal(click(b).prevented, false);
    // The touch media query stops matching: range mode is switched off.
    button.events.click();
    click(a);
    assert.equal(button.attrs['aria-pressed'], 'true');
    assert.ok('data-range-start' in a.attrs);
    medias[touchQuery].matches = false;
    changes[touchQuery]();
    assert.equal(button.attrs['aria-pressed'], 'false');
    assert.equal(status.textContent, '');
    assert.equal('data-range-start' in a.attrs, false);
    assert.equal(click(b).prevented, false, 'clicks pass through once touch stops matching');
  } finally {
    for (const [k, v] of Object.entries(saved)) {
      if (v === undefined) delete globalThis[k]; else globalThis[k] = v;
    }
  }
});

// Deterministic timers: no sleeps, and each test restores every mocked global.
function clock() {
  const saved = { setTimeout: globalThis.setTimeout, clearTimeout: globalThis.clearTimeout };
  const tasks = new Map(); let n = 0, now = 0;
  globalThis.setTimeout = (fn, delay = 0) => { tasks.set(++n, { fn, at: now + delay }); return n; };
  globalThis.clearTimeout = (id) => tasks.delete(id);
  return { async tick(ms = 300) { now += ms; const batch = [...tasks].filter(([, t]) => t.at <= now); for (const [id] of batch) tasks.delete(id); for (const [, t] of batch) await t.fn(); },
    size: () => tasks.size, restore() { Object.assign(globalThis, saved); } };
}

test('head refresh defers a real textarea draft, resumes after emptying and blurring', async () => {
  const saved = globalThis.document, c = clock();
  const text = { value: 'unsent', defaultValue: '' };
  let fetched = 0, applied = 0; const notices = [];
  globalThis.document = { activeElement: text, querySelectorAll: () => [text] };
  const r = window.ermiteMR.refresher({ blocked: window.ermiteMR.hasDraft,
    fetchPage: async () => { fetched++; return 'page'; }, apply: () => applied++,
    notice: (v) => notices.push(v), failed: () => assert.fail('unexpected fallback') });
  try {
    r.request(); await c.tick();
    assert.equal(fetched, 0); assert.deepEqual(notices, [true]);
    text.value = ''; r.resume(); await c.tick();
    assert.equal(fetched, 0, 'focus still protects an empty draft');
    document.activeElement = null; r.resume(); await c.tick();
    assert.equal(fetched, 1); assert.equal(applied, 1); assert.equal(r.pending(), false);
  } finally { globalThis.document = saved; c.restore(); }
});

test('head refresh debounces bursts, serializes requests, and reruns after an in-flight push', async () => {
  const c = clock(); let fetched = 0, applied = 0, resolve;
  const r = window.ermiteMR.refresher({ blocked: () => false,
    fetchPage: () => { fetched++; return new Promise((r) => { resolve = r; }); },
    apply: () => applied++, notice() {}, failed: () => assert.fail('unexpected failure') });
  try {
    r.request(); r.request(); r.request(); assert.equal(c.size(), 1);
    await c.tick(299); assert.equal(fetched, 0);
    await c.tick(0); assert.equal(fetched, 0);
    const first = c.tick(1); assert.equal(fetched, 1);
    r.request(); await c.tick(); assert.equal(fetched, 1, 'one fetch in flight');
    resolve('one'); await first; assert.equal(applied, 1); assert.equal(c.size(), 1);
    const second = c.tick(); assert.equal(fetched, 2); resolve('two'); await second;
    assert.equal(applied, 2); assert.equal(c.size(), 0);
  } finally { c.restore(); }
});

test('typing during fetch defers even manual refresh until the draft ends', { timeout: 2000 }, async () => {
  const c = clock(); let blocked = false, applied = 0, resolve, fetched = 0; const notices = [];
  const r = window.ermiteMR.refresher({ blocked: () => blocked,
    fetchPage: () => { fetched++; return new Promise((r) => { resolve = r; }); }, apply: () => { applied++; },
    notice: (v) => notices.push(v), failed: () => assert.fail('unexpected failure') });
  try {
    r.request(); const first = c.tick(); blocked = true; resolve('page'); await first;
    assert.equal(applied, 0); assert.deepEqual(notices, [true]);
    r.request(true); const manual = c.tick();
    assert.equal(fetched, 1, 'manual refresh must not start a fetch during a draft');
    await manual; assert.equal(applied, 0);
    blocked = false; r.resume(); const second = c.tick(); resolve('new page'); await second;
    assert.equal(applied, 1); assert.equal(notices.at(-1), false);
  } finally { c.restore(); }
});

test('fetch failures and non-MR responses take the fallback, never apply', async () => {
  const saved = { fetch: globalThis.fetch, location: globalThis.location, DOMParser: globalThis.DOMParser };
  const c = clock(); let fallback = 0;
  globalThis.location = { href: 'https://forge.test/demo/merge-requests/1?view=split&from=25' };
  const r = window.ermiteMR.refresher({ blocked: () => false, fetchPage: window.ermiteMR.fetchPage,
    apply: () => assert.fail('must not swap'), notice() {}, failed: () => fallback++ });
  try {
    for (const result of ['network', { ok: false }, { ok: true, redirected: true }, { ok: true, text: async () => '<html>login</html>' }]) {
      globalThis.fetch = async (url, opts) => {
        assert.equal(url, location.href); assert.equal(opts.credentials, 'same-origin');
        if (result === 'network') throw new Error('offline'); return result;
      };
      globalThis.DOMParser = class { parseFromString() { return { querySelector: () => null }; } };
      r.request(); await c.tick();
    }
    assert.equal(fallback, 4);
  } finally {
    for (const [k,v] of Object.entries(saved)) { if (v === undefined) delete globalThis[k]; else globalThis[k] = v; }
    c.restore();
  }
});

test('viewport probe finds a stable path/side/line and restores its offset without line scans', () => {
  const names = ['document', 'location', 'innerHeight', 'innerWidth', 'scrollY', 'scrollBy', 'scrollTo', 'CSS', 'matchMedia'];
  const saved = Object.fromEntries(names.map((k) => [k, globalThis[k]]));
  const rect = (top, bottom = top + 20) => ({ top, bottom, left: 0, width: 900 });
  let lineTop = 160, headerTop = -200, found = true; const calls = [];
  const header = { getBoundingClientRect: () => rect(headerTop) };
  const row = { getBoundingClientRect: () => rect(lineTop), querySelector: () => link };
  const link = { href: 'https://forge.test/demo/merge-requests/1?on=new:42:a.go#f2', closest: () => row, getAttribute: () => link.href };
  const file = { getBoundingClientRect: () => rect(headerTop), id: 'f2', dataset: { file: 'a.go' }, querySelector: (s) => s.includes('summary') ? header : s.includes('href$') ? (assert.ok(s.includes('on=new:42:a.go#f3')), found ? link : null) : null };
  const hit = { closest: (s) => s === 'tr' ? row : file };
  const diffs = { dataset: { diff: 'unified' }, getBoundingClientRect: () => rect(120), contains: () => true };
  globalThis.document = {
    querySelector: (s) => s === '#diffs' ? diffs : s === '.hdr' ? { getBoundingClientRect: () => rect(0, 100) } : null,
    querySelectorAll: (s) => { assert.equal(s, '#diffs details.dfile', 'only files, never every line'); return found === 'gone' ? [] : [file]; },
    elementFromPoint: () => hit, documentElement: { scrollHeight: 10000 },
  };
  Object.assign(globalThis, { location: { href: link.href }, innerHeight: 800, innerWidth: 1200, scrollY: 2500,
    scrollBy: (arg) => calls.push(['by', arg]), scrollTo: (arg) => calls.push(['to', arg]), CSS: { escape: (s) => s }, matchMedia: () => ({ matches: false }) });
  try {
    const a = window.ermiteMR.scrollAnchor();
    assert.equal(a.file, 'a.go'); assert.equal(a.on, 'new:42:a.go'); assert.equal(a.offset, 160);
    file.id = 'f3'; lineTop = 260; window.ermiteMR.restoreAnchor(a);
    assert.deepEqual(calls.pop(), ['by', { top: 100, behavior: 'instant' }]);
    found = false; headerTop = -100; window.ermiteMR.restoreAnchor(a);
    assert.deepEqual(calls.pop(), ['by', { top: 100, behavior: 'instant' }]);
    found = 'gone'; window.ermiteMR.restoreAnchor({ ...a, y: 20000 });
    assert.deepEqual(calls.pop(), ['to', { top: 9200, behavior: 'instant' }]);
  } finally { for (const [k,v] of Object.entries(saved)) { if (v === undefined) delete globalThis[k]; else globalThis[k] = v; } }
});

test('empty edited default and focused cform controls protect drafts', () => {
  const saved = document;
  try {
    globalThis.document = { querySelectorAll: () => [{ value: '', defaultValue: 'existing' }], activeElement: null };
    assert.equal(window.ermiteMR.hasDraft(), true);
    document.querySelectorAll = () => [];
    document.activeElement = { closest: (s) => s === '.cform' ? {} : null };
    assert.equal(window.ermiteMR.hasDraft(), true);
  } finally { globalThis.document = saved; }
});

test('failure during a draft keeps pending and resumes after the draft ends', async () => {
  const c = clock(); let blocked = false, reject, applied = 0, count = 0;
  const r = window.ermiteMR.refresher({ blocked: () => blocked,
    fetchPage: () => ++count === 1 ? new Promise((_, no) => { reject = no; }) : Promise.resolve('page'),
    apply: () => applied++, notice() {}, failed: () => assert.fail('draft must defer fallback') });
  try {
    r.request(); const first = c.tick(); blocked = true; reject(new Error('offline')); await first;
    assert.equal(r.pending(), true); assert.equal(applied, 0);
    blocked = false; r.resume(); await c.tick(); assert.equal(applied, 1);
  } finally { c.restore(); }
});

test('anchor restoration retains the server encoding of tilde and star paths', () => {
  const saved = { document, CSS: globalThis.CSS, scrollBy: globalThis.scrollBy };
  try {
    globalThis.CSS = { escape: (s) => s };
    globalThis.scrollBy = () => {};
    for (const path of ['a~.go', 'a*.go']) {
      const encoded = path.replace('*', '%2A');
      const row = { getBoundingClientRect: () => ({ top: 5 }) };
      const file = { id: 'f9', dataset: { file: path }, querySelector: (s) => {
        if (s === '.dt.uni') return file;
        assert.equal(s, 'a[href$="on=new:12:' + encoded + '#f9"]');
        return { closest: () => row };
      } };
      globalThis.document = { querySelectorAll: () => [file], querySelector: () => ({ dataset: { diff: 'unified' } }) };
      window.ermiteMR.restoreAnchor({ file: path, suffix: 'new:12:' + encoded, on: 'new:12:' + path, offset: 5 });
    }
  } finally { Object.assign(globalThis, saved); }
});

for (const control of [
  { type: 'radio', checked: true, defaultChecked: false },
  { type: 'checkbox', checked: false, defaultChecked: true },
  { type: 'number', name: 'start_line', value: '10', defaultValue: '12' },
  { tagName: 'SELECT', options: [{ selected: true, defaultSelected: false }] },
]) test(`changed ${control.name || control.type || control.tagName} remains a draft after blur`, () => {
  const saved = document;
  try {
    globalThis.document = { activeElement: null, querySelectorAll(sel) {
      assert.equal(sel, ':is(form.cform, .cmt form, form.d-acts) :is(textarea, input, select)');
      return [control];
    } };
    assert.equal(window.ermiteMR.hasDraft(), true);
  } finally { globalThis.document = saved; }
});

test('intercepted Cancel resets the entire comment form', () => {
  const saved = Object.fromEntries(['document', 'location', 'addEventListener', 'matchMedia'].map(k => [k, globalThis[k]]));
  const c = clock(), handlers = {};
  const diffs = { dataset: { view: 'split' }, querySelector() { return null; }, querySelectorAll() { return []; } };
  const radio = { type: 'radio', checked: true, defaultChecked: false };
  const line = { type: 'number', value: '2', defaultValue: '4' };
  let reset = 0, prevented = false;
  const form = { reset() { reset++; radio.checked = radio.defaultChecked; line.value = line.defaultValue; }, contains() { return false; } };
  const cancel = { closest: () => form };
  globalThis.document = { activeElement: null,
    querySelector: s => ['#diffs', '#diffs[data-mr]'].includes(s) ? diffs : null,
    querySelectorAll: s => s.includes('form.cform') ? [radio, line] : [],
    addEventListener: (type, fn) => { handlers[type] = fn; } };
  globalThis.location = { pathname: '/demo/merge-requests/1', hash: '', href: 'https://forge.test/demo/merge-requests/1', replace() {} };
  globalThis.addEventListener = () => {};
  globalThis.matchMedia = () => ({ matches: true, addEventListener() {} });
  try {
    window.ermiteMR.start();
    window.ermiteMR.headChanged();
    handlers.click({ target: { closest: () => cancel }, preventDefault() { prevented = true; } });
    assert.equal(prevented, true);
    assert.equal(reset, 1);
    assert.equal(window.ermiteMR.hasDraft(), false);
  } finally { Object.assign(globalThis, saved); c.restore(); }
});


test('live refresh preserves the open button panel even when the hash points elsewhere', async () => {
  const { readFileSync } = await import('node:fs');
  const src = readFileSync(new URL('../static/js/mr.js', import.meta.url), 'utf8');
  const start = src.indexOf('  async function refreshPage(page,');
  const end = src.indexOf('  function refreshNotice', start);
  const { el, installDom } = await import('./dom.mjs');
  const saved = globalThis.document;
  const h = footerHarness();
  try {
    window.ermiteMR.panels();
    h.tabs[1].events.click({ preventDefault() {} });
    location.hash = '#f99';
    const doc = installDom();
    Object.defineProperty(doc, 'activeElement', { get: () => null });
    const diffs = el('div');
    diffs.dataset.diff = 'unified';
    const fresh = el('footer', {}, el('section', { attrs: { 'data-mr-panel': '' } }), el('section', { attrs: { 'data-mr-panel': '' } }));
    const page = { body: { dataset: {} } };
    const $ = (s, root) => s === '#diffs' ? diffs : s === '.mr-footer' ? (root === page ? fresh : h.footer) : el('div');
    const $$ = (s, root) => s === '[data-mr-tabs] button' ? h.tabs : s === '[data-mr-panel]' ? fresh.children : [];
    let selected;
    const lazy = { reset() {}, watch() {} };
    let attached = 0;
    const refresh = new Function('$', '$$', 'scrollAnchor', 'hasDraft', 'replaceRegion', 'panels', 'watchBar', 'restoreMergePrefs', 'foldFiles', 'touchComments', 'restoreAnchor', 'window', 'document', 'sheet',
      'let revealGen = 0; ' + src.slice(start, end) + '; return refreshPage;')(
      $, $$, () => ({}), () => false, async () => {}, (i) => { selected = i; }, () => {}, () => {}, () => {}, () => {}, () => {}, { ermiteLazy: lazy }, doc, { keep() {}, attach() { attached++; } });
    await refresh(page);
    assert.equal(attached, 1, 'the fresh rail is a sheet again');
    assert.equal(selected, 1);
    assert.deepEqual(fresh.children.map((p) => p.hidden), [true, false]);
  } finally { h.restore(); globalThis.document = saved; }
});

test('sheet: open / close, survives a refreshed rail, keys and media', () => {
  const saved = { document: globalThis.document, matchMedia: globalThis.matchMedia, add: globalThis.addEventListener };
  class El {
    constructor(tag) { this.tagName = tag; this.attrs = {}; this.events = {}; this.kids = []; this.id = ''; this.className = ''; }
    setAttribute(k, v) { this.attrs[k] = String(v); }
    getAttribute(k) { return this.attrs[k]; }
    hasAttribute(k) { return k in this.attrs; }
    toggleAttribute(k, on) { if (on) this.attrs[k] = ''; else delete this.attrs[k]; }
    addEventListener(k, fn) { this.events[k] = fn; }
    contains(el) { return el === this || this.kids.some((k) => k.contains?.(el)); }
    focus() { globalThis.document.activeElement = this; }
    querySelector(s) {
      if (s === '.r-files') return this.kids.find((k) => k.className === 'r-files') || null;
      if (s === '.mr-sheet-close') return this.kids.find((k) => k.className?.includes('mr-sheet-close')) || null;
      return null;
    }
    prepend(el) { this.kids.unshift(el); }
    before(el) { tabs.push(el); }
  }
  const tabs = [];
  const makeRail = () => { const r = new El('aside'), f = new El('section'); f.className = 'r-files'; r.kids.push(f); return r; };
  let rail = makeRail();
  const outside = new El('main');
  const docEvents = {};
  let dialogOpen = false;
  globalThis.addEventListener = () => {};
  globalThis.document = {
    documentElement: { classList: { toggle() {} } },
    body: { style: { setProperty() {} } },
    querySelector: (s) => s === '.work > .rail' ? rail : s === 'dialog[open]' ? (dialogOpen ? {} : null) : null,
    querySelectorAll: () => [],
    createElement: (t) => new El(t),
    addEventListener: (k, fn) => { (docEvents[k] ||= []).push(fn); },
  };
  const fire = (k, e) => docEvents[k].forEach((fn) => fn(e));
  const media = { matches: true, addEventListener(k, fn) { this.change = fn; } };
  globalThis.matchMedia = () => media;
  const esc = { key: 'Escape', preventDefault() {}, stopImmediatePropagation() {} };
  const open = () => rail.hasAttribute('data-sheet-open');
  try {
    const S = window.ermiteMR.sheet;
    S.mount();
    S.mount();
    assert.equal(tabs.length, 1, 'one tab, whatever the mounts');
    assert.equal(docEvents.keydown.length, 2, 'one modal and one non-modal listener');
    const tab = tabs[0];
    const closer = () => rail.querySelector('.mr-sheet-close');
    assert.ok(rail.hasAttribute('data-sheet'));
    assert.equal(tab.getAttribute('aria-controls'), 'mr-rail');
    assert.equal(closer().getAttribute('aria-expanded'), undefined, 'the close control is no disclosure');
    tab.events.click();
    assert.ok(open());
    assert.equal(tab.getAttribute('aria-expanded'), 'true');
    assert.equal(document.activeElement, closer(), 'focus on the close control');
    closer().events.click();
    assert.ok(!open());
    assert.equal(document.activeElement, tab, 'focus back on the tab');

    tab.events.click();
    dialogOpen = true; fire('keydown', esc);
    assert.ok(open(), 'Esc belongs to an open dialog');
    dialogOpen = false; fire('keydown', esc);
    assert.ok(!open(), 'Esc closes');
    tab.events.click();
    rail.events.click({ target: { closest: (s) => s === '.r-files a[href]' ? {} : null } });
    assert.ok(!open(), 'a file closes');
    tab.events.click();
    fire('pointerdown', { target: outside });
    assert.ok(!open(), 'a click outside closes');
    tab.events.click();
    fire('focusin', { target: outside });
    assert.ok(!open(), 'focus outside closes');

    // a live refresh replaces the rail: it keeps the sheet open, focus unmoved
    tab.events.click();
    S.keep();
    rail = makeRail(); rail.id = 'mr-rail-2';
    document.activeElement = outside;
    S.attach();
    assert.ok(open(), 'still open on the fresh rail');
    assert.ok(closer(), 'the fresh rail has its close control');
    assert.equal(tab.getAttribute('aria-controls'), 'mr-rail-2');
    assert.equal(closer().getAttribute('aria-controls'), 'mr-rail-2');
    assert.equal(document.activeElement, outside, 'focus unmoved');
    closer().events.click();
    S.keep(); S.attach();
    assert.equal(rail.kids.filter((k) => k.className?.includes('mr-sheet-close')).length, 1, 'one close control');
    tab.events.click();
    assert.ok(open(), 'the tab opens the new rail');

    media.matches = false; media.change();
    assert.ok(!open(), 'growing past the breakpoint closes');
    tab.events.click();
    assert.ok(!open(), 'wide: no sheet');
    assert.equal(tab.getAttribute('aria-expanded'), 'false', 'wide toggle hides the column');
    assert.equal(railPrefs.get('ermite.rail.hidden'), '1');
    tab.events.click();
    assert.equal(tab.getAttribute('aria-expanded'), 'true', 'wide toggle restores the column');
    assert.equal(railPrefs.get('ermite.rail.hidden'), '0');
    assert.equal(tab.getAttribute('aria-label'), 'Hide the side panel');
    document.activeElement = outside; // finder input outside the panel
    const ignored = { key: 'Escape', preventDefault() { assert.fail('not the panel escape'); }, stopImmediatePropagation() { assert.fail('must bubble'); } };
    fire('keydown', ignored);
    const nested = rail.kids.find(k => k.className === 'r-files');
    nested.closest = () => ({}); nested.focus();
    fire('keydown', ignored); // open details.menu owns it
    nested.closest = () => null;
    fire('keydown', { ...ignored, defaultPrevented: true });
    assert.equal(tab.getAttribute('aria-expanded'), 'true');
    fire('keydown', { key: 'Escape', preventDefault() {}, stopImmediatePropagation() { assert.fail('wide must bubble'); } });
    assert.equal(tab.getAttribute('aria-expanded'), 'false');
    tab.events.click();
    nested.focus(); media.matches = true; media.change();
    assert.equal(document.activeElement, tab, 'shrinking moves focus out of the hidden rail');
    media.matches = false; media.change();
    document.activeElement = outside; media.matches = true; media.change();
    assert.equal(document.activeElement, outside, 'unrelated focus stays put');
    media.matches = false; media.change();

    fire('pointerdown', { target: outside });
    assert.equal(tab.getAttribute('aria-expanded'), 'true', 'outside clicks leave the wide rail alone');
    closer().events.click();
    assert.equal(tab.getAttribute('aria-expanded'), 'false');
    media.matches = true; media.change();
    tab.events.click();
    assert.ok(open(), 'narrow drawer remains usable after hiding the wide rail');
    media.matches = true;
  } finally { globalThis.document = saved.document; globalThis.matchMedia = saved.matchMedia; globalThis.addEventListener = saved.add; }
});

test('sheet: l opens it with focus in the files, h hides it', () => {
  const saved = { document: globalThis.document, matchMedia: globalThis.matchMedia };
  const attrs = {};
  const rail = {
    attrs, querySelector: (s) => s === '.r-files' ? {} : s === '.mr-sheet-close' ? {} : null,
    hasAttribute: (k) => k in attrs, toggleAttribute: (k, on) => { if (on) attrs[k] = ''; else delete attrs[k]; },
    contains: () => true,
  };
  const link = { getAttribute: () => 'true', focus() { globalThis.document.activeElement = link; }, scrollIntoView() {} };
  const main = { focus() { globalThis.document.activeElement = main; } };
  globalThis.matchMedia = () => ({ matches: true });
  globalThis.document = {
    documentElement: { classList: { toggle() {} } },
    body: { style: { setProperty() {} } },
    querySelector: (s) => s === '.work > .rail' ? rail : null,
    querySelectorAll: (s) => s.includes('.files') ? [link] : [],
    getElementById: () => main,
  };
  try {
    const M = window.ermiteMR;
    assert.ok(M.focusFiles());
    assert.ok('data-sheet-open' in attrs, 'l opens the sheet');
    assert.equal(document.activeElement, link);
    assert.ok(M.leaveFiles());
    assert.ok(!('data-sheet-open' in attrs), 'h hides it');
    assert.equal(document.activeElement, main);
  } finally { globalThis.document = saved.document; globalThis.matchMedia = saved.matchMedia; }
});

test('actions invalidate older snapshots and coalesce a fresh refresh after settling', async () => {
  const c = clock(); let resolve, fetched = 0; const applied = [];
  const r = window.ermiteMR.refresher({ blocked: () => false,
    fetchPage: () => { fetched++; return new Promise(r => { resolve = r; }); },
    apply: page => applied.push(page), notice() {}, failed: () => assert.fail('unexpected failure') });
  try {
    r.request(); const old = c.tick();
    const a = {}, b = {};
    r.actionStarted(a); r.actionStarted(b);
    r.actionFinished(a);
    resolve('before comment'); await old;
    assert.deepEqual(applied, []);
    r.request(); await c.tick();
    assert.equal(fetched, 1, 'no GET while any action is in flight');
    r.actionFinished(b); r.actionFinished(b);
    assert.equal(c.size(), 1, 'one refresh after the last action settles');
    const fresh = c.tick(); resolve('after comment'); await fresh;
    assert.deepEqual(applied, ['after comment']);
    assert.equal(r.pending(), false);
  } finally { c.restore(); }
});

test('an action completed during GET invalidates it even with no actions left in flight', async () => {
  const c = clock(); let resolve; let applied = 0;
  const r = window.ermiteMR.refresher({ blocked: () => false,
    fetchPage: () => new Promise(r => { resolve = r; }), apply: () => applied++,
    notice() {}, failed: () => assert.fail('unexpected failure') });
  try {
    r.request(); const old = c.tick();
    const action = {}; r.actionStarted(action); r.actionFinished(action);
    resolve('stale'); await old;
    assert.equal(applied, 0);
    const fresh = c.tick(); resolve('fresh'); await fresh;
    assert.equal(applied, 1);
  } finally { c.restore(); }
});
