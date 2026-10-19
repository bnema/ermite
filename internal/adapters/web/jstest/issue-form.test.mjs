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
