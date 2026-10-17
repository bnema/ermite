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
