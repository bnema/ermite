// Exercise the production keydown handler without loading the rest of the page.
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { test } from 'node:test';
const src = readFileSync(new URL('../static/js/ermite.js', import.meta.url), 'utf8');
const block = src.slice(src.indexOf('  // 1. Keys.'), src.indexOf('  // 2. A [data-fire]'));

function page({ owner = true, off = false, typing = false, dialog = false } = {}) {
  let handler;
  const fired = [];
  const window = { ermiteKeys: { blocks: () => off } };
  const $ = (s) => s === 'dialog[open]' ? dialog : s === '[data-inbox]' ? owner : null;
  new Function('window', '$', 'ermite', 'addEventListener', block)(window, $, { fire: (n) => fired.push(n) }, (_, f) => { handler = f; });
  return (key) => {
    const e = { key, shiftKey: key === 'I', preventDefault() {}, target: { closest: (s) => s.startsWith('input') && typing } };
    handler(e);
    return fired;
  };
}
test('Shift+I opens inbox while lowercase i retains the issues tab', () => {
  const press = page();
  assert.deepEqual(press('I'), ['inbox']);
  assert.deepEqual(press('i'), ['inbox', 'i']);
});
test('inbox shortcut respects preference, fields, dialogs, and owner target', () => {
  for (const options of [{ off: true }, { typing: true }, { dialog: true }, { owner: false }]) {
    assert.deepEqual(page(options)('I'), [], JSON.stringify(options));
  }
});
