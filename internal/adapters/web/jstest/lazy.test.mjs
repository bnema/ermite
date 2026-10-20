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

async function setup(phs, io = true, opts = {}) {
  const observed = [];
  const urls = [];
  if (io) globalThis.IntersectionObserver = class { observe(p) { observed.push(p); } unobserve() {} disconnect() {} };
  else delete globalThis.IntersectionObserver;
  const reply = opts.reply || { status: 200, ok: true, body: '' };
  globalThis.fetch = async (u) => { urls.push(u); return { status: reply.status, ok: reply.ok, text: async () => reply.body }; };
  globalThis.document = {
    readyState: 'complete',
    querySelector: (s) => (s === '#diffs' ? { dataset: {} } : null),
    querySelectorAll: (s) => (s.includes(':not([data-manual])') ? phs.filter((p) => !('manual' in p.dataset)) : phs),
    getElementById: opts.getElementById || (() => null),
    createElement: () => ({ content: { querySelectorAll: () => opts.files || [] } }),
    addEventListener() {},
  };
  delete globalThis.ermiteLazy;
  (0, eval)(SRC);
  return { observed, urls };
}
const settle = () => new Promise((r) => setTimeout(r, 0));
const restore = () => {
  delete globalThis.ermiteLazy;
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


// A file of the page (old) or of a fragment (new), with the queries swap makes.
function fileEl(id, { lazy = false, manual = false, open = false } = {}) {
  const f = { id, open, queried: [] };
  f.querySelector = (sel) => {
    if (sel === '.dlazy[data-manual]') return manual ? {} : null;
    if (sel === '.dlazy') return lazy || manual ? {} : null;
    if (sel === '.ln a') return f.line || null;
    return null;
  };
  f.querySelectorAll = () => [];
  return f;
}

test('a loaded manual file opens and takes the focus without scrolling', async () => {
  const a = placeholder('/f?one=1&from=0', true);
  const old = fileEl('f1', { manual: true });
  const fresh = fileEl('f1');
  const focused = [];
  fresh.line = { focus: (o) => focused.push(o) };
  let current = old;
  old.replaceWith = (n) => { current = n; };
  try {
    await setup([a.ph], true, { reply: { status: 200, ok: true, body: '<x>' }, files: [fresh], getElementById: () => current });
    window.ermiteLazy.onClick({ target: a.link, button: 0, preventDefault() {} });
    await settle(); await settle();
    assert.equal(current, fresh);
    assert.equal(fresh.open, true);
    assert.deepEqual(focused, [{ preventScroll: true }]);
  } finally { await settle(); restore(); }
});

test('a window that still holds a collapsed file keeps it closed', async () => {
  const w = placeholder('/w?from=25', false);
  const old = fileEl('f26', { lazy: true, open: true });
  const fresh = fileEl('f26', { manual: true, open: true });
  let replaced = null;
  old.replaceWith = (n) => { replaced = n; };
  try {
    await setup([w.ph], true, { reply: { status: 200, ok: true, body: '<x>' }, files: [fresh], getElementById: () => old });
    await window.ermiteLazy.load(w.ph);
    assert.equal(replaced, fresh);
    assert.equal(fresh.open, false);
  } finally { await settle(); restore(); }
});

test('a 409 makes the page stale: no later load fetches', async () => {
  const a = placeholder('/f?one=1&from=0', true);
  const b = placeholder('/f?one=1&from=1', true);
  try {
    const { urls } = await setup([a.ph, b.ph], true, { reply: { status: 409, ok: false, body: '<p>moved</p>' } });
    await window.ermiteLazy.load(a.ph);
    assert.equal(a.ph.innerHTML, '<p>moved</p>');
    assert.equal(urls.length, 1);
    await window.ermiteLazy.load(b.ph);
    assert.equal(urls.length, 1, 'stale: nothing is fetched');
  } finally { await settle(); restore(); }
});

test('a failed load clears the loading mark and can be retried', async () => {
  const a = placeholder('/f?one=1&from=0', true);
  const marks = new Set();
  a.ph.setAttribute = (n) => marks.add(n);
  a.ph.removeAttribute = (n) => marks.delete(n);
  try {
    const { urls } = await setup([a.ph], true, { reply: { status: 500, ok: false, body: 'boom' } });
    const p = window.ermiteLazy.load(a.ph);
    assert.ok(marks.has('data-loading'));
    await p;
    assert.equal(marks.has('data-loading'), false);
    await window.ermiteLazy.load(a.ph);
    assert.equal(urls.length, 2, 'a retry fetches again');
  } finally { await settle(); restore(); }
});

test('Space is prevented on the link; a held key is ignored', async () => {
  const a = placeholder('/f?one=1&from=1', true);
  try {
    const { urls } = await setup([a.ph]);
    let prevented = 0;
    const ev = (extra) => ({ target: a.link, key: ' ', preventDefault() { prevented++; }, ...extra });
    assert.equal(window.ermiteLazy.onKey(ev({ repeat: true })), false);
    assert.equal(prevented, 0);
    assert.equal(urls.length, 0);
    assert.equal(window.ermiteLazy.onKey(ev({})), true);
    assert.equal(prevented, 1, 'Space does not scroll the page');
    assert.equal(urls.length, 1);
  } finally { await settle(); restore(); }
});
