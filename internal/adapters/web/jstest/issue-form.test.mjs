// Tests of the public issue form: the proof-of-work solver (static/js/pow-worker.js)
// and the pure core of the form (static/js/issue-form.js). Run with
//   node --test 'internal/adapters/web/jstest/*.test.mjs'
import assert from 'node:assert/strict';
import { createHash } from 'node:crypto';
import { test } from 'node:test';

globalThis.window = globalThis;
globalThis.document = { readyState: 'complete', querySelector: () => null, querySelectorAll: () => [], addEventListener() {} };
await import('../static/js/pow-worker.js');
await import('../static/js/issue-form.js');
const P = globalThis.ermitePow;
const C = window.ermiteIssueForm.core;

// The vector of usecase/issue_pow_vector_test.go: the smallest solution for this
// nonce at this difficulty, found by the server's own powHash.
const VECTOR = { nonce: 'AAECAwQFBgcICQoLDA0ODw', difficulty: 12, solution: '2491' };

test('leadingZeroBits counts the zero bits at the start', () => {
  assert.equal(P.leadingZeroBits(Uint8Array.of(0xff)), 0);
  assert.equal(P.leadingZeroBits(Uint8Array.of(0x80)), 0);
  assert.equal(P.leadingZeroBits(Uint8Array.of(0x40)), 1);
  assert.equal(P.leadingZeroBits(Uint8Array.of(0x01)), 7);
  assert.equal(P.leadingZeroBits(Uint8Array.of(0x00, 0x80)), 8);
  assert.equal(P.leadingZeroBits(Uint8Array.of(0x00, 0x00, 0x1f)), 19);
  assert.equal(P.leadingZeroBits(Uint8Array.of(0, 0)), 16);
  assert.equal(P.leadingZeroBits(Uint8Array.of()), 0);
});

test('b64urlDecode reads the nonce the server sends', () => {
  assert.deepEqual([...P.b64urlDecode(VECTOR.nonce)], [...Array(16).keys()]);
  assert.deepEqual([...P.b64urlDecode('-_8')], [0xfb, 0xff]);
});

test('the hashed bytes are nonce || ":" || decimal solution', () => {
  const nonce = P.b64urlDecode(VECTOR.nonce);
  const got = P.input(nonce, 2491);
  assert.equal(got.length, 16 + 1 + 4);
  assert.deepEqual([...got.slice(0, 16)], [...nonce]);
  assert.equal(Buffer.from(got.slice(16)).toString('latin1'), ':2491');
});

test('the solver finds the very solution of the Go vector', async () => {
  const sol = await P.solve(P.b64urlDecode(VECTOR.nonce), VECTOR.difficulty);
  assert.equal(sol, VECTOR.solution);
  const sum = createHash('sha256').update(Buffer.concat([Buffer.from(P.b64urlDecode(VECTOR.nonce)), Buffer.from(':' + sol)])).digest();
  assert.ok(P.leadingZeroBits(sum) >= VECTOR.difficulty);
  // and no smaller counter works
  for (let n = 0; n < Number(sol); n++) {
    const s = createHash('sha256').update(Buffer.concat([Buffer.from(P.b64urlDecode(VECTOR.nonce)), Buffer.from(':' + n)])).digest();
    assert.ok(P.leadingZeroBits(s) < VECTOR.difficulty, 'counter ' + n);
  }
});

test('the solver is independent of the batch size and reports progress', async () => {
  const nonce = P.b64urlDecode(VECTOR.nonce);
  const seen = [];
  const sol = await P.solve(nonce, VECTOR.difficulty, { batch: 100, onProgress: (n) => { seen.push(n); } });
  assert.equal(sol, VECTOR.solution);
  assert.deepEqual(seen.slice(0, 2), [100, 200]);
  const stopped = await P.solve(nonce, 40, { batch: 10, onProgress: (n) => n >= 30 });
  assert.equal(stopped, null, 'stopped by the caller');
});

test('counterClass: warn from 90 %, over above the maximum', () => {
  assert.equal(C.counterClass(0, 120), '');
  assert.equal(C.counterClass(107, 120), '');
  assert.equal(C.counterClass(108, 120), 'warn');
  assert.equal(C.counterClass(120, 120), 'warn');
  assert.equal(C.counterClass(121, 120), 'over');
  assert.equal(C.counterClass(9000, 10000), 'warn');
  assert.equal(C.counterClass(8999, 10000), '');
  assert.equal(C.counterClass(41, 40), 'over');
});

test('length counts characters, not UTF-16 units', () => {
  assert.equal(C.length('abc'), 3);
  assert.equal(C.length('a😀b'), 3);
  assert.equal(C.length(''), 0);
});

test('state: the button opens only when solved, waited and not over', () => {
  const base = { solved: false, now: 1000, minAt: 5000, sending: false, unsupported: false, failed: false, overs: false };
  const s = (o) => C.state({ ...base, ...o });
  assert.deepEqual(s({}), { disabled: true, status: 'Checking your browser… (this takes a few seconds)' });
  assert.equal(s({ solved: true }).disabled, true, 'solved too early');
  assert.equal(s({ solved: true, now: 4999 }).disabled, true);
  assert.equal(s({ now: 6000 }).disabled, true, 'waited but not solved');
  assert.equal(s({ solved: true, now: 5000 }).disabled, false);
  assert.equal(s({ solved: true, now: 6000, overs: true }).disabled, true, 'a field is too long');
  assert.equal(s({ solved: true, now: 6000, sending: true }).disabled, true, 'no double submit');
  assert.equal(s({ solved: true, now: 6000, sending: true }).status, 'Sending…');
  assert.equal(s({ unsupported: true }).status, 'Your browser cannot run the check.');
  assert.equal(s({ solved: true, now: 6000, unsupported: true }).disabled, true);
  assert.equal(s({ failed: true, solved: true, now: 6000 }).disabled, true);
});

test('serverLength counts NFC code points like the server', () => {
  assert.equal(C.serverLength('title', 'e\u0301'.repeat(61)), 61);
  assert.equal(C.serverLength('title', '\u0344'.repeat(61)), 122);
  assert.equal(C.counterClass(122, 120), 'over');
  assert.equal(C.serverLength('title', '😀'.repeat(120)), 120);
  assert.equal(C.counterClass(120, 120), 'warn');
  assert.equal(C.serverLength('title', '😀'.repeat(121)), 121);
  assert.equal(C.counterClass(121, 120), 'over');
});

test('serverLength drops what the server strips and trims', () => {
  assert.equal(C.serverLength('title', '  ab\u202e\u200f\u061c\u2066\u0007\u0085  '), 2);
  assert.equal(C.serverLength('title', ' \u202e '), 0);
  assert.equal(C.serverLength('author', ' me '), 2);
  assert.equal(C.serverLength('body', 'a\nb\tc\u0007\u202a\r\n'), 6);
  assert.equal(C.serverLength('body', ' a '), 3);
  assert.equal(C.serverLength('title', 'a\tb'), 2);
});

test('state keeps the button disabled when a title is empty or a field is over', () => {
  const base = { solved: true, now: 2, minAt: 1, sending: false, unsupported: false, failed: false, overs: false, empty: false };
  assert.equal(C.state(base).disabled, false);
  assert.equal(C.state({ ...base, overs: true }).disabled, true);
  assert.equal(C.state({ ...base, empty: true }).disabled, true);
});

function fakeStorage() {
  const m = new Map();
  return { getItem: (k) => (m.has(k) ? m.get(k) : null), setItem: (k, v) => m.set(k, String(v)), removeItem: (k) => m.delete(k), m };
}

test('drafts are saved, restored once, and scoped to the path', () => {
  const st = fakeStorage();
  const k = C.draftKey('/r/issues/new');
  assert.notEqual(k, C.draftKey('/other/issues/new'));
  C.saveDraft(st, k, { title: 't', body: 'b\nc', author: 'a', solution: 'x' });
  assert.deepEqual(C.restoreDraft(st, k), { title: 't', body: 'b\nc', author: 'a' });
  assert.equal(st.m.size, 0);
  assert.equal(C.restoreDraft(st, k), null);
  st.setItem(k, 'not json');
  assert.equal(C.restoreDraft(st, k), null);
});

test('onPageShow reloads only for a bfcache restore', () => {
  assert.equal(C.onPageShow(true), 'reload');
  assert.equal(C.onPageShow(false), 'none');
  assert.equal(C.onPageShow(undefined), 'none');
});

test('init restores a draft and reloads on a bfcache pageshow', () => {
  const listeners = {};
  const mk = (o) => ({ listeners: {}, events: [], dispatchEvent(e) { this.events.push(e.type); }, addEventListener(t, f) { this.listeners[t] = f; }, ...o });
  const title = mk({ id: 'issue-title', name: 'title', value: '' });
  const body = mk({ id: 'issue-body', name: 'body', value: '' });
  const author = mk({ id: 'issue-author', name: 'author', value: '' });
  const outs = [['issue-title', 120], ['issue-body', 10000], ['issue-author', 40]].map(([f, max]) => ({ dataset: { for: f, max: String(max) }, textContent: '', className: '' }));
  const button = { disabled: false };
  const solution = { value: '' };
  const sel = { 'button[type="submit"]': button, '[data-status]': { textContent: '' }, 'input[name="solution"]': solution,
    '[name="title"]': title, '[name="body"]': body, '[name="author"]': author,
    '#issue-title': title, '#issue-body': body, '#issue-author': author };
  const form = { dataset: { minSubmitAt: '0' }, hidden: true, querySelector: (s) => sel[s] ?? null,
    querySelectorAll: () => outs, addEventListener() {}, removeAttribute() {} };
  const st = fakeStorage();
  let reloads = 0;
  const saved = { sessionStorage: window.sessionStorage, location: window.location, addEventListener: window.addEventListener, Worker: window.Worker };
  Object.assign(window, { sessionStorage: st, location: { pathname: '/r/issues/new', reload() { reloads++; } },
    addEventListener(t, f) { listeners[t] = f; }, Worker: undefined });
  try {
    C.saveDraft(st, C.draftKey('/r/issues/new'), { title: 'Hello', body: 'B', author: 'me' });
    window.ermiteIssueForm.init(form);
    assert.equal(title.value, 'Hello');
    assert.deepEqual(body.events, ['input']);
    assert.equal(outs[0].textContent, '5 / 120');
    assert.equal(st.m.size, 0);
    assert.equal(button.disabled, true);
    listeners.pageshow({ persisted: false });
    assert.equal(reloads, 0);
    title.value = 'Edited';
    listeners.pageshow({ persisted: true });
    assert.equal(reloads, 1);
    assert.deepEqual(JSON.parse(st.getItem(C.draftKey('/r/issues/new'))), { title: 'Edited', body: 'B', author: 'me' });
    assert.equal(button.disabled, true);
  } finally {
    for (const [k, v] of Object.entries(saved)) { if (v === undefined) delete window[k]; else window[k] = v; }
  }
});
