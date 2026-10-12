// Tests of the live job log of static/js/log.js (window.ermiteLog) with a stub
// EventSource on the tiny hand-rolled DOM. Run with
//   node --test 'internal/adapters/web/jstest/*.test.mjs'
import assert from 'node:assert/strict';
import { test } from 'node:test';
import { el, installDom } from './dom.mjs';

// A line is "text" or "+text" for a piece that continues the line before it.
function line(text) {
  const cont = text.startsWith('+');
  const li = el('li', { cls: 'lg', attrs: cont ? { 'data-cont': '' } : {} }, el('span', { cls: 't', text: cont ? text.slice(1) : text }));
  return li;
}

installDom({ parseHTML: (data) => data.split('|').filter(Boolean).map(line) });
globalThis.document.addEventListener = () => {}; // log.js starts itself on DOMContentLoaded

class StubEventSource {
  static CLOSED = 2;
  static last = null;
  constructor(url) {
    this.url = url;
    this.readyState = 1;
    this.handlers = new Map();
    StubEventSource.last = this;
  }
  addEventListener(name, fn) { this.handlers.set(name, fn); }
  close() { this.readyState = StubEventSource.CLOSED; }
  emit(name, data) { this.handlers.get(name)?.({ data }); }
}
globalThis.EventSource = StubEventSource;

let scrolls = [];
globalThis.scrollTo = (x, y) => scrolls.push(y);
Object.assign(globalThis, { innerHeight: 800, scrollY: 0 });

await import('../static/js/log.js');
const { follow } = window.ermiteLog;

// page makes a job page body: the box with its list and the lines already there.
function page(attrs, ...lines) {
  const list = el('ol', { cls: 'log' }, ...lines.map(line));
  const box = el('div', { id: 'job-log', attrs }, list);
  const body = document.body;
  body.replaceChildren(box);
  Object.defineProperty(globalThis.document.body, 'scrollHeight', { value: 1000, configurable: true });
  return { box, list };
}
const running = { 'data-follow': '', 'data-stream': '/r/job/stream', 'data-next': '3' };
const texts = (list) => list.children.map((li) => li.querySelector('.t').textContent);

test('it does nothing for a job that is over, or a page without a feed', () => {
  assert.equal(follow(null), null);
  assert.equal(follow(page({ 'data-stream': '/s' }).box), null, 'no data-follow: the job ended');
  assert.equal(follow(page({ 'data-follow': '' }).box), null, 'no stream URL');
});

test('it opens the feed after the last line the page holds', () => {
  const { box } = page(running, 'a', 'b', 'c');
  const es = follow(box);
  assert.equal(es.url, '/r/job/stream?from=3');
});

test('it appends the lines of a batch', () => {
  const { box, list } = page(running, 'a');
  const es = follow(box);
  es.emit('ci-log', 'b|c');
  es.emit('ci-log', 'd');
  assert.deepEqual(texts(list), ['a', 'b', 'c', 'd']);
});

test('a piece that continues a line of an earlier batch joins that line', () => {
  const { box, list } = page(running, 'start of a long');
  const es = follow(box);
  es.emit('ci-log', '+ line|next');
  assert.deepEqual(texts(list), ['start of a long line', 'next']);
});

test('it scrolls with the log only while the reader is at its end', () => {
  const { box } = page(running, 'a');
  const es = follow(box);
  scrolls = [];
  Object.assign(globalThis, { scrollY: 190 }); // 800 + 190 >= 1000 - 80: at the end
  es.emit('ci-log', 'b');
  assert.deepEqual(scrolls, [1000]);
  scrolls = [];
  Object.assign(globalThis, { scrollY: 0 }); // reading above
  es.emit('ci-log', 'c');
  assert.deepEqual(scrolls, []);
});

test('ci-end closes the feed, and a closed connection stays closed', () => {
  const { box } = page(running, 'a');
  const es = follow(box);
  assert.equal(es.readyState, 1);
  es.emit('ci-end', 'end');
  assert.equal(es.readyState, StubEventSource.CLOSED);

  const again = follow(page(running, 'a').box);
  again.readyState = StubEventSource.CLOSED;
  again.onerror();
  assert.equal(again.readyState, StubEventSource.CLOSED);
});
