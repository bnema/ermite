// Run with node --test 'internal/adapters/web/jstest/*.test.mjs'.
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { test } from 'node:test';
import { El, el, installDom, dom, flushFrames } from './dom.mjs';

const src = readFileSync(new URL('../static/js/ermite.js', import.meta.url), 'utf8');
const block = src.slice(src.indexOf('// Daily pulse values:'), src.indexOf('// ermiteLive'));

function setup() {
  const doc = installDom();
  dom.holdFrames = true;
  dom.frames.clear();
  const listeners = {};
  doc.addEventListener = (name, fn) => { listeners[name] = fn; };
  globalThis.addEventListener = () => {};
  let observe;
  globalThis.MutationObserver = class {
    constructor(fn) { observe = fn; }
    observe() {}
  };
  globalThis.innerWidth = 1000;
  globalThis.innerHeight = 800;
  globalThis.DOMPoint = class {
    constructor(x, y) { this.x = x; this.y = y; }
    matrixTransform() { return this; }
  };
  const makeFigure = () => {
    const svg = el('svg');
    svg.getScreenCTM = () => ({ inverse: () => ({}) });
    const fig = el('figure', { cls: 'repo-pulse', isConnected: true, attrs: {
      'data-days-commits': '1,14,3', 'data-days-visitors': '2,37,4',
      'data-end': '2026-11-14', 'data-end-x': '600', 'aria-describedby': 'existing',
    } }, svg);
    fig.getBoundingClientRect = () => ({ left: 0, top: 100, width: 600 });
    const card = el('div', { cls: 'repo' }, fig);
    card.getBoundingClientRect = () => ({ left: 0, right: 700 });
    doc.body.append(card);
    return fig;
  };
  const create = doc.createElement;
  doc.createElement = (tag) => {
    const node = create(tag);
    node.getBoundingClientRect = () => ({ width: 100, height: 80 });
    return node;
  };
  doc.createElementNS = (_, tag) => {
    const node = new El(tag);
    node.classList = { add: (cls) => { node.className = cls; } };
    return node;
  };
  const fig = makeFigure();
  let scans = 0;
  const query = doc.querySelectorAll;
  doc.querySelectorAll = (sel) => { scans++; return query(sel); };
  new Function(block)();
  const key = (key, target = fig) => listeners.keydown({ target, key, preventDefault() {}, stopPropagation() {} });
  const pointer = (x, target = fig.querySelector('svg')) => listeners.pointermove({ target, clientX: x, clientY: 200, pointerType: 'mouse' });
  return { doc, fig, key, pointer, makeFigure, observe: (...args) => observe(...args), scans: () => scans };
}

test('keyboard days update an associated polite status; pointer moves stay silent', () => {
  const { fig, key, pointer } = setup();
  const status = fig.querySelector('[aria-live]');
  const hint = fig.querySelector('span');
  assert.equal(status.className, 'sr');
  assert.equal(status.getAttribute('aria-live'), 'polite');
  assert.equal(hint.textContent, 'Use left and right arrow keys to read each day');
  assert.equal(fig.getAttribute('aria-describedby'), `existing ${hint.id}`);
  assert.equal(fig.tabIndex, 0);
  assert.equal(status.textContent, '');
  pointer(0);
  key('ArrowRight');
  const date = new Date('2026-11-13T00:00:00Z').toLocaleDateString(undefined, { day: 'numeric', month: 'short', timeZone: 'UTC' });
  assert.equal(status.textContent, `${date}: 14 commits, 37 visitors`);
  pointer(600);
  assert.equal(status.textContent, `${date}: 14 commits, 37 visitors`);
  key('ArrowLeft');
  assert.equal(status.textContent, `${date}: 14 commits, 37 visitors`);
  key('Escape');
  assert.equal(status.textContent, '');
  pointer(0);
  assert.equal(status.textContent, '');
});

test('same-day pointer movement preserves tooltip content and positions once per frame', () => {
  const { doc, pointer, key } = setup();
  pointer(10);
  const bubble = doc.querySelector('.pulse-tooltip');
  const heading = bubble.children[0];
  pointer(20);
  pointer(30);
  assert.equal(bubble.children[0], heading);
  assert.equal(dom.frames.size, 1);
  assert.equal(bubble.style.left, undefined);
  flushFrames();
  assert.equal(bubble.style.left, '42px');
  pointer(300);
  assert.notEqual(bubble.children[0], heading);
  assert.equal(dom.frames.size, 1);
  key('Escape');
  assert.equal(dom.frames.size, 0);
  assert.equal(doc.querySelector('.pulse-tooltip'), null);
});

test('observer scans only added subtrees and ignores tooltip mutations', () => {
  const { doc, fig, pointer, observe, scans, makeFigure } = setup();
  assert.equal(scans(), 1);
  pointer(0);
  const bubble = doc.querySelector('.pulse-tooltip');
  const original = fig.getAttribute('aria-describedby');
  bubble.querySelectorAll = () => { throw new Error('tooltip must not be scanned'); };
  observe([{ target: doc.body, addedNodes: [bubble] }, { target: bubble, addedNodes: bubble.children }]);
  assert.equal(scans(), 1);
  assert.equal(fig.getAttribute('aria-describedby'), original);
  const next = makeFigure();
  observe([{ target: doc.body, addedNodes: [next.parentElement] }]);
  assert.equal(next.dataset.js, '');
  assert.ok(next.querySelector('[aria-live]'));
  observe([{ target: next, addedNodes: next.querySelectorAll('span') }]);
  assert.equal(next.querySelectorAll('[aria-live]').length, 1);
  const direct = makeFigure();
  observe([{ target: direct.parentElement, addedNodes: [direct] }]);
  assert.ok(direct.querySelector('[aria-live]'), 'an added figure itself is upgraded');
  assert.equal(scans(), 1);
  fig.isConnected = false;
  observe([{ target: doc.body, addedNodes: [] }]);
  assert.equal(doc.querySelector('.pulse-tooltip'), null);
});
