// Tests of the pure core of the merge request page (static/js/mr.js). Run with
//   node --test 'internal/adapters/web/jstest/*.test.mjs'
import assert from 'node:assert/strict';
import { test } from 'node:test';

globalThis.window = globalThis;
globalThis.document = { readyState: 'complete', querySelector: () => null, querySelectorAll: () => [] };
await import('../static/js/mr.js');
const C = window.ermiteMR.core;

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
 let focused=false;
 const field={value:'',focus(){focused=true;}};
 const details={open:false,querySelector(){return field;}};
 const thread={querySelector(){return details;}};
 const button={dataset:{quote:'> source\n\n^2 '},closest(){return {closest(){return thread;}};}};
 assert.equal(C.quote(button),true);
 assert.equal(details.open,true);
 assert.equal(field.value,'> source\n\n^2 ');
 assert.equal(focused,true);
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

test('footer tabs switch panels, support arrows, and open on checks hash', () => {
  const oldDocument = globalThis.document;
  const oldLocation = globalThis.location;
  const oldAdd = globalThis.addEventListener;
  const oldHistory = globalThis.history;
  const listeners = {};
  const replaced = [];
  globalThis.history = { replaceState: (...args) => replaced.push(args) };
  const node = (id) => ({ id, attrs: {}, events: {}, hidden: false,
    setAttribute(k,v) { this.attrs[k]=v; },
    addEventListener(k,v) { this.events[k]=v; }, focus() { this.focused=true; } });
  const tabs = [node('mr-tab-commits'), node('mr-tab-checks')];
  const panels = [node('mr-commits'), node('mr-checks-panel')];
  const footer = node('mr-panels'); footer.open=false;
  const nav = node('tabs'); nav.querySelectorAll=()=>tabs;
  footer.querySelectorAll=()=>panels;
  globalThis.document = {querySelector: (s) => s === '#mr-panels' ? footer : s === '[data-mr-tabs]' ? nav : null,
    addEventListener(k,v) {listeners[k]=v;} };
  globalThis.location = {hash:''};
  globalThis.addEventListener = (k,v) => {listeners[k]=v;};
  try {
    window.ermiteMR.panels();
    assert.equal(footer.open,false);
    assert.equal(panels[0].hidden,false);
    assert.equal(panels[1].hidden,true);
    assert.deepEqual(replaced, [], 'initial selection leaves the URL alone');
    tabs[1].events.click({preventDefault(){}});
    assert.deepEqual(replaced, [[null, '', '#mr-checks-panel']]);
    assert.equal(panels[0].hidden,true);
    assert.equal(tabs[1].attrs['aria-selected'],'true');
    tabs[1].events.keydown({key:'ArrowRight',preventDefault(){}});
    assert.equal(tabs[0].focused,true);
    assert.equal(panels[0].hidden,false);
    assert.deepEqual(replaced.at(-1), [null, '', '#mr-commits']);
    location.hash='#mr-checks'; replaced.length = 0; listeners.hashchange();
    assert.deepEqual(replaced, [], 'hash navigation does not rewrite the URL');
    assert.equal(footer.open,true);
    assert.equal(panels[1].hidden,false);
    footer.open=false;
    listeners.click({target:{closest:()=>true}});
    assert.equal(footer.open,true);
    assert.equal(C.tabIndex('Home',1,2),0);
    assert.equal(C.tabIndex('End',0,2),1);
    assert.equal(C.tabIndex('ArrowLeft',0,2),1);
    assert.equal(C.tabIndex('Escape',0,2),null);
  } finally {
    globalThis.document=oldDocument; globalThis.location=oldLocation; globalThis.addEventListener=oldAdd;
    if (oldHistory === undefined) delete globalThis.history; else globalThis.history = oldHistory;
  }
});

test('footer: #mr-ck syncs the checks label and a click opens the footer on Checks', () => {
  const saved = Object.fromEntries(['document', 'location', 'addEventListener', 'history', 'MutationObserver'].map((k) => [k, globalThis[k]]));
  const node = (id) => ({ id, attrs: {}, events: {}, hidden: false,
    setAttribute(k, v) { this.attrs[k] = v; }, addEventListener(k, v) { this.events[k] = v; }, focus() {} });
  const tabs = [node('t0'), node('t1')];
  const panels = [node('mr-commits'), node('mr-checks-panel')];
  const footer = node('mr-panels'); footer.open = false; footer.querySelectorAll = () => panels;
  const label = { textContent: '' };
  const nav = node('tabs'); nav.querySelectorAll = () => tabs;
  nav.querySelector = (s) => s === '[data-checks-label]' ? label : null;
  const summary = { title: '1/2 failed', textContent: ' 1/2 ', parentElement: {} };
  const listeners = {};
  let observed = null, callback = null;
  globalThis.MutationObserver = class { constructor(fn) { callback = fn; } observe(target, opts) { observed = [target, opts]; } };
  globalThis.document = { querySelector: (s) => s === '#mr-panels' ? footer : s === '[data-mr-tabs]' ? nav : s === '#mr-ck' ? summary : null,
    addEventListener(k, v) { listeners[k] = v; } };
  globalThis.location = { hash: '' };
  globalThis.addEventListener = () => {};
  globalThis.history = { replaceState() {} };
  try {
    window.ermiteMR.panels();
    assert.equal(label.textContent, '1/2 failed', 'initial label sync');
    assert.equal(observed[0], summary.parentElement);
    summary.title = ''; callback();
    assert.equal(label.textContent, '1/2', 'observer re-syncs from the text');
    summary.textContent = ' '; callback();
    assert.equal(label.textContent, 'no checks');
    listeners.click({ target: { closest: (s) => s === '#mr-ck' ? summary : null } });
    assert.equal(footer.open, true);
    assert.equal(panels[1].hidden, false);
    assert.equal(panels[0].hidden, true);
    assert.equal(tabs[1].attrs['aria-selected'], 'true');
  } finally {
    for (const [k, v] of Object.entries(saved)) { if (v === undefined) delete globalThis[k]; else globalThis[k] = v; }
  }
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
