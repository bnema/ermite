// Tests of the loader of the files of a diff (static/js/lazy.js). Run with
//   node --test 'internal/adapters/web/jstest/*.test.mjs'
import assert from 'node:assert/strict';
import { test } from 'node:test';
import { readFileSync } from 'node:fs';

const SRC = readFileSync(new URL('../static/js/lazy.js', import.meta.url), 'utf8');

globalThis.window = globalThis;
const saved = { document: globalThis.document, fetch: globalThis.fetch, IO: globalThis.IntersectionObserver };

// A placeholder: an element with a dataset; its link finds it back.
function placeholder(src, manual) {
  const details = { id: 'f1', querySelector: () => null };
  const ph = { dataset: manual ? { src, manual: '' } : { src }, setAttribute() {}, removeAttribute() {}, closest: (s) => (s === 'details' ? details : null) };
  const link = { closest: (s) => (s === '.dlazy' ? ph : s === '.dlazy[data-manual] > a' && manual ? link : null) };
  return { ph, link, details };
}

async function setup(phs, io = true) {
  const observed = [];
  const urls = [];
  if (io) globalThis.IntersectionObserver = class { observe(p) { observed.push(p); } unobserve() {} disconnect() {} };
  else delete globalThis.IntersectionObserver;
  globalThis.fetch = async (u) => { urls.push(u); return { status: 200, ok: true, text: async () => '' }; };
  globalThis.document = {
    readyState: 'complete',
    querySelector: (s) => (s === '#diffs' ? { dataset: {} } : null),
    querySelectorAll: (s) => (s.includes(':not([data-manual])') ? phs.filter((p) => !('manual' in p.dataset)) : phs),
    getElementById: () => null,
    createElement: () => ({ content: { querySelectorAll: () => [] } }),
    addEventListener() {},
  };
  delete globalThis.ermiteLazy;
  (0, eval)(SRC);
  return { observed, urls };
}
const settle = () => new Promise((r) => setTimeout(r, 0));
const restore = () => {
  for (const [k, v] of [['document', saved.document], ['fetch', saved.fetch], ['IntersectionObserver', saved.IO]]) {
    if (v === undefined) delete globalThis[k]; else globalThis[k] = v;
  }
};

test('manual placeholders are not observed', async () => {
  const a = placeholder('/r/commit/abc/files?one=1&from=0', true);
  const b = placeholder('/r/merge-requests/1/files?from=25', false);
  try {
    const { observed, urls } = await setup([a.ph, b.ph]);
    assert.deepEqual(observed, [b.ph]);
    assert.deepEqual(urls, []);
  } finally { await settle(); restore(); }
});

test('without IntersectionObserver only the automatic placeholders load', async () => {
  const a = placeholder('/m?one=1&from=0', true);
  const b = placeholder('/w?from=25', false);
  try {
    const { urls } = await setup([a.ph, b.ph], false);
    assert.deepEqual(urls, ['/w?from=25']);
  } finally { await settle(); restore(); }
});

test('a click on "load diff" loads that one file and does not navigate', async () => {
  const a = placeholder('/r/commit/abc/files?one=1&from=3', true);
  try {
    const { urls } = await setup([a.ph]);
    let prevented = false;
    const used = window.ermiteLazy.onClick({ target: a.link, button: 0, preventDefault() { prevented = true; } });
    assert.equal(used, true);
    assert.equal(prevented, true);
    assert.deepEqual(urls, ['/r/commit/abc/files?one=1&from=3']);
  } finally { await settle(); restore(); }
});

test('Enter and Space on the link load it; other keys and modified clicks do not', async () => {
  const a = placeholder('/f?one=1&from=1', true);
  try {
    const { urls } = await setup([a.ph]);
    const key = (k, extra = {}) => window.ermiteLazy.onKey({ target: a.link, key: k, preventDefault() {}, ...extra });
    assert.equal(key('a'), false);
    assert.equal(key('Enter', { ctrlKey: true }), false);
    assert.equal(window.ermiteLazy.onClick({ target: a.link, button: 0, ctrlKey: true, preventDefault() {} }), false);
    assert.equal(urls.length, 0);
    assert.equal(key('Enter'), true);
    assert.equal(key(' '), true);
    assert.equal(urls.length, 1, 'a file is fetched once');
  } finally { await settle(); restore(); }
});

test('a manual placeholder without an address is a plain link', async () => {
  const a = placeholder('', true);
  a.ph.dataset.src = '';
  try {
    await setup([a.ph]);
    assert.equal(window.ermiteLazy.onClick({ target: a.link, button: 0, preventDefault() { assert.fail(); } }), false);
  } finally { await settle(); restore(); }
});

test('onSwap runs after a window came in', async () => {
  const b = placeholder('/w?from=25', false);
  try {
    await setup([b.ph]);
    let n = 0;
    window.ermiteLazy.onSwap(() => n++);
    await window.ermiteLazy.load(b.ph);
    assert.equal(n, 1);
  } finally { await settle(); restore(); }
});

test('reset drops the response of a load in flight and lets the same window load again', async () => {
  const b = placeholder('/w?from=25', false);
  try {
    await setup([b.ph]);
    let release;
    const gate = new Promise((r) => { release = r; });
    let fetches = 0;
    globalThis.fetch = async () => { fetches++; await gate; return { status: 200, ok: true, text: async () => '' }; };
    let n = 0;
    window.ermiteLazy.onSwap(() => n++);
    const first = window.ermiteLazy.load(b.ph);
    window.ermiteLazy.reset();
    release();
    await first;
    assert.equal(n, 0, 'the old generation does not swap');
    await window.ermiteLazy.load(b.ph);
    assert.equal(fetches, 2);
    assert.equal(n, 1);
  } finally { await settle(); restore(); }
});

test('reset clears the stale state; watch disconnects the previous observer', async () => {
  const b = placeholder('/w?from=25', false);
  try {
    await setup([b.ph]);
    const made = [];
    globalThis.IntersectionObserver = class {
      constructor() { this.off = false; made.push(this); }
      observe() {} unobserve() {} disconnect() { this.off = true; }
    };
    globalThis.fetch = async () => ({ status: 409, ok: false, text: async () => 'moved' });
    await window.ermiteLazy.load(b.ph);
    const urls = [];
    globalThis.fetch = async (u) => { urls.push(u); return { status: 200, ok: true, text: async () => '' }; };
    await window.ermiteLazy.load(b.ph);
    assert.deepEqual(urls, [], 'stale: nothing loads');
    window.ermiteLazy.watch();
    window.ermiteLazy.watch();
    assert.equal(made.length, 2);
    assert.ok(made[0].off && !made[1].off);
    window.ermiteLazy.reset();
    assert.ok(made[1].off);
    await window.ermiteLazy.load(b.ph);
    assert.equal(urls.length, 1);
  } finally { await settle(); restore(); }
});
