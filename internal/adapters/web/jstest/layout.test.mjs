// Tests of the home page layout of static/js/ermite.js (window.ermiteLayout)
// with store.js, on the small DOM of dom.mjs. Only the ermiteLayout block is
// evaluated: the rest needs a page. Run with
//   node --test 'internal/adapters/web/jstest/*.test.mjs'
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { beforeEach, test } from 'node:test';
import { el, installDom } from './dom.mjs';

const read = (f) => readFileSync(new URL('../static/js/' + f, import.meta.url), 'utf8');
const ermite = read('ermite.js');
const start = ermite.indexOf('  window.ermiteLayout = ');
const block = ermite.slice(start, ermite.indexOf('  })();', start) + '  })();'.length);
const storeSrc = read('store.js');

const doc = installDom();
let listeners;
let storage;
let fits;
let home;
let buttons;

// A page: .home and the switch, with "cards" pressed as the server renders it.
// Then store.js and the block are evaluated, as a page load does.
function load({ stored, storageObj } = {}) {
  listeners = {};
  fits = 0;
  storage = storageObj ?? new Map(stored ? Object.entries(stored) : []);
  globalThis.localStorage = storageObj ?? {
    getItem: (k) => (storage.has(k) ? storage.get(k) : null),
    setItem: (k, v) => { storage.set(k, String(v)); },
    removeItem: (k) => { storage.delete(k); },
  };
  globalThis.addEventListener = (type, f) => (listeners[type] ||= []).push(f);
  globalThis.removeEventListener = (type, f) => { listeners[type] = (listeners[type] || []).filter((g) => g !== f); };
  home = el('div', { cls: 'home', attrs: { 'data-layout': 'cards' } });
  buttons = ['cards', 'split', 'compact'].map((v) => el('button', {
    cls: 'btn icon', attrs: { type: 'button', 'data-set-layout': v, 'aria-pressed': String(v === 'cards') },
  }));
  const sw = el('div', { cls: 'layout-switch', attrs: { role: 'group', 'data-js': '' } }, ...buttons);
  doc.body.replaceChildren(sw, home);
  window.ermiteFit = { all: () => { fits++; } };
  new Function(storeSrc)();
  new Function(block)();
  return window.ermiteLayout;
}

const pressed = () => buttons.map((b) => b.getAttribute('aria-pressed'));
const click = (target) => listeners.click.forEach((f) => f({ target }));

beforeEach(() => { delete globalThis.localStorage; });

test('with nothing stored the layout is cards, and the page is as rendered', () => {
  const L = load();
  assert.equal(L.get(), 'cards');
  assert.deepEqual(L.values, ['cards', 'split', 'compact']);
  assert.equal(home.getAttribute('data-layout'), 'cards');
  assert.deepEqual(pressed(), ['true', 'false', 'false']);
});

test('the stored layout is applied when the page loads', () => {
  const L = load({ stored: { 'home-layout': 'compact' } });
  assert.equal(L.get(), 'compact');
  assert.equal(home.getAttribute('data-layout'), 'compact');
  assert.deepEqual(pressed(), ['false', 'false', 'true']);
  assert.ok(fits > 0, 'the chips are fitted again');
});

test('a stored value that is not a layout falls back to cards', () => {
  const L = load({ stored: { 'home-layout': 'grid' } });
  assert.equal(L.get(), 'cards');
  assert.equal(home.getAttribute('data-layout'), 'cards');
  assert.deepEqual(pressed(), ['true', 'false', 'false']);
});

test('set stores the layout, shows it and fits the chips again', () => {
  const L = load();
  fits = 0;
  L.set('split');
  assert.equal(storage.get('home-layout'), 'split');
  assert.equal(home.getAttribute('data-layout'), 'split');
  assert.deepEqual(pressed(), ['false', 'true', 'false']);
  assert.equal(fits, 1);
  L.set('nope');
  assert.equal(L.get(), 'split', 'an unknown layout is ignored');
  assert.equal(storage.get('home-layout'), 'split');
});

test('pressing a layout button sets it; other clicks do nothing', () => {
  const L = load();
  click(buttons[2]);
  assert.equal(L.get(), 'compact');
  assert.equal(home.getAttribute('data-layout'), 'compact');
  assert.deepEqual(pressed(), ['false', 'false', 'true']);
  click(home);
  click({ closest: undefined });
  assert.equal(L.get(), 'compact');
  // a child of the button (an icon) counts as the button
  const icon = el('svg');
  buttons[1].append(icon);
  click(icon);
  assert.equal(L.get(), 'split');
});

test("another tab's change applies live", () => {
  const L = load();
  storage.set('home-layout', 'split');
  listeners.storage.forEach((f) => f({ key: 'home-layout', newValue: 'split', storageArea: localStorage }));
  assert.equal(L.get(), 'split');
  assert.equal(home.getAttribute('data-layout'), 'split');
  assert.deepEqual(pressed(), ['false', 'true', 'false']);
  storage.delete('home-layout');
  listeners.storage.forEach((f) => f({ key: 'home-layout', newValue: null, storageArea: localStorage }));
  assert.equal(home.getAttribute('data-layout'), 'cards', 'removed: back to the default');
  listeners.storage.forEach((f) => f({ key: 'keys-off', newValue: '1', storageArea: localStorage }));
  assert.equal(home.getAttribute('data-layout'), 'cards');
});

test('a page without .home or without ermiteFit still works', () => {
  const L = load();
  doc.body.replaceChildren();
  delete window.ermiteFit;
  L.set('split');
  assert.equal(L.get(), 'split');
  L.apply();
});

test('without usable storage the choice holds for the page', () => {
  const refusing = {
    getItem: () => { throw new Error('denied'); },
    setItem: () => { throw new Error('quota'); },
    removeItem: () => { throw new Error('denied'); },
  };
  const L = load({ storageObj: refusing });
  assert.equal(L.get(), 'cards');
  click(buttons[1]);
  assert.equal(L.get(), 'split');
  assert.equal(home.getAttribute('data-layout'), 'split');
});
