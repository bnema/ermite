// Tests of the "live updates paused" delay of static/js/ermite.js
// (window.ermiteLive.lost/back) with mocked timers. Only the ermiteLive part of
// the file is evaluated: the rest needs a page. Run with
//   node --test 'internal/adapters/web/jstest/*.test.mjs'
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { test } from 'node:test';

const src = readFileSync(new URL('../static/js/ermite.js', import.meta.url), 'utf8');
const live = src.slice(src.indexOf('window.ermiteLive = '));

function setup(t) {
  t.mock.timers.enable({ apis: ['setTimeout'] });
  const hint = { dataset: { state: 'connecting' } };
  globalThis.window = {};
  globalThis.document = { querySelector: (sel) => (sel === '.hint-live' ? hint : null) };
  new Function(live)();
  return { live: globalThis.window.ermiteLive, hint };
}

test('a drop shorter than the delay says nothing', (t) => {
  const { live, hint } = setup(t);
  live.back();
  live.lost();
  t.mock.timers.tick(4000);
  live.back();
  t.mock.timers.tick(10000);
  assert.equal(hint.dataset.state, 'live');
});

test('a drop that lasts shows offline, once, until the stream is back', (t) => {
  const { live, hint } = setup(t);
  live.lost();
  t.mock.timers.tick(3000);
  live.lost(); // a second error does not restart the delay
  t.mock.timers.tick(2000);
  assert.equal(hint.dataset.state, 'offline');
  live.back();
  assert.equal(hint.dataset.state, 'live');
});
