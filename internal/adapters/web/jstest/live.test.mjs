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

test('stream teardown handles the reader cancellation that hx-sse leaves uncaught', async (t) => {
  const { live, listeners } = setup(t);
  assert.equal(listeners.unhandledrejection, undefined, 'no global rejection filter');
  const abort = Object.assign(new Error('signal is aborted without reason'), { name: 'AbortError' });
  let cancelled = 0;
  const connection = { reader: { cancel: () => { cancelled++; return Promise.reject(abort); } } };
  listeners['htmx:sse:after:connection']({ target: { matches: () => true }, detail: { connection } });
  const escaped = [];
  const onRejection = (reason) => escaped.push(reason);
  process.on('unhandledRejection', onRejection);
  try {
    // remove stands for hx-sse's cleanup: r.reader?.cancel?.(), result ignored.
    await live.teardown(() => { connection.reader?.cancel?.(); });
    await new Promise((resolve) => setImmediate(resolve));
    await new Promise((resolve) => setImmediate(resolve));
  } finally {
    process.off('unhandledRejection', onRejection);
  }
  assert.equal(cancelled, 1, 'the real reader is still cancelled');
  assert.deepEqual(escaped, [], 'no AbortError escapes the teardown');
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
