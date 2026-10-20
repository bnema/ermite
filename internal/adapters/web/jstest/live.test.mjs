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
  const listeners = {};
  globalThis.window = { addEventListener: (name, fn) => { listeners[name] = fn; } };
  globalThis.document = { querySelector: (sel) => (sel === '.hint-live' ? hint : null) };
  new Function(live)();
  return { live: globalThis.window.ermiteLive, hint, listeners };
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

test('controlled stream deletion suppresses only teardown AbortError rejections', async (t) => {
  const { live, listeners } = setup(t);
  const rejection = (name) => {
    const event = { reason: { name }, prevented: false, preventDefault() { this.prevented = true; } };
    listeners.unhandledrejection(event);
    return event.prevented;
  };
  assert.equal(rejection('AbortError'), false);
  await live.teardown(async () => {
    assert.equal(rejection('AbortError'), true, 'no AbortError escapes the delete swap');
    assert.equal(rejection('TypeError'), false);
    await Promise.resolve();
  });
  assert.equal(rejection('AbortError'), true, 'async cancellation reporting is covered');
  t.mock.timers.tick(1);
  assert.equal(rejection('AbortError'), false, 'unrelated later aborts remain visible');
});

test('replacement stream URL carries the last connection id, including id-only events', (t) => {
  const { live, listeners } = setup(t);
  const connection = { lastEventId: 'boot-7' };
  listeners['htmx:sse:after:connection']({ target: { matches: () => true }, detail: { connection } });
  connection.lastEventId = 'boot-8'; // hx-sse updates this even for id-only events.
  const saved = globalThis.location;
  globalThis.location = { href: 'https://forge.test/demo/merge-requests/1' };
  const element = { url: '/-/events?topic=repo%3Ademo&head=new', getAttribute() { return this.url; },
    setAttribute(name, value) { assert.equal(name, 'hx-sse:connect'); this.url = value; } };
  try {
    live.resumeStream(element);
    const url = new URL(element.url, location.href);
    assert.equal(url.searchParams.get('after'), 'boot-8');
    assert.equal(url.searchParams.get('head'), 'new');
    assert.equal(url.searchParams.get('topic'), 'repo:demo');
  } finally { globalThis.location = saved; }
});
