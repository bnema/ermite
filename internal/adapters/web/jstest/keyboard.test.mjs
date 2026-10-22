// Exercise the production key handler without a browser or dependencies.
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { test } from 'node:test';
import { runInNewContext } from 'node:vm';
const source = readFileSync(new URL('../static/js/ermite.js', import.meta.url), 'utf8');
function keyboard({ owner = true, off = false, typing = false, dialog = false } = {}) {
  let handler;
  const fired = [];
  const timers = new Map();
  let timerID = 0;
  runInNewContext(source.slice(source.indexOf('  const single ='), source.indexOf('  // 2. A [data-fire]')), {
    addEventListener: (_, fn) => { handler = fn; },
    window: { ermiteKeys: { blocks: () => off } },
    ermite: { fire: name => fired.push(name) },
    $: selector => (owner && selector === '[data-inbox]') || (dialog && selector === 'dialog[open]'),
    closeMenus: () => false,
    clearTimeout: id => timers.delete(id),
    setTimeout: fn => { timers.set(++timerID, fn); return timerID; },
  });
  return { fired, expireTimers() {
    const callbacks = [...timers.values()];
    timers.clear();
    for (const callback of callbacks) callback();
  }, press(key, shiftKey = false) {
    handler({ key, shiftKey, target: { closest: selector => typing && selector.startsWith('input'), blur() {} }, preventDefault() {} });
  } };
}
test('sections require g and have no old t alias', () => {
  const k = keyboard();
  for (const key of ['s', 't', 'c', 'r', 'm', 'i']) k.press(key);
  assert.deepEqual(k.fired, []);
  for (const key of ['s', 'f', 'c', 'b', 'm', 'i', 'r', 'a', 'h', 'g', 't']) { k.press('g'); k.press(key); }
  assert.deepEqual(k.fired, ['s', 'f', 'c', 'b', 'm', 'i', 'r', 'a', 'gh', 'top']);
});
test('uppercase actions work with either reported casing and interrupt prefixes', () => {
  for (const [key, action] of Object.entries({ G: 'bottom', M: 'merge', R: 'read', E: 'expandAll', I: 'inbox' })) {
    for (const reported of [key, key.toLowerCase()]) {
      const k = keyboard();
      k.press('g'); k.press(reported, true); k.press(reported, true);
      assert.deepEqual(k.fired, [action, action]);
    }
  }
});
test('uppercase actions respect preference, fields and dialogs', () => {
  for (const options of [{ off: true }, { typing: true }, { dialog: true }]) {
    const k = keyboard(options); k.press('g', true);
    assert.deepEqual(k.fired, []);
  }
});

test('Shift+I opens inbox while issues need the g i sequence', () => {
  const k = keyboard();
  k.press('I', true); k.press('i');
  assert.deepEqual(k.fired, ['inbox']);
  k.press('g'); k.press('i');
  assert.deepEqual(k.fired, ['inbox', 'i']);
});
test('inbox shortcut respects preference, fields, dialogs, and owner target', () => {
  for (const options of [{ off: true }, { typing: true }, { dialog: true }, { owner: false }]) {
    const k = keyboard(options); k.press('I', true);
    assert.deepEqual(k.fired, [], JSON.stringify(options));
  }
});
test('shifted punctuation still opens help and the command palette', () => {
  const k = keyboard();
  k.press('?', true); k.press(':', true);
  assert.deepEqual(k.fired, ['help', 'cmd']);
});
test('g prefix expires without scheduling real timers', () => {
  const k = keyboard();
  k.press('g'); k.expireTimers(); k.press('s');
  assert.deepEqual(k.fired, []);
  k.press('g'); k.press('s');
  assert.deepEqual(k.fired, ['s']);
});
