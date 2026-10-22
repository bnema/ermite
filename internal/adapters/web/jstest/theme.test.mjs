// Tests of the theme: window.ermiteTheme of static/js/store.js (applied before
// the first paint) and the footer switch of static/js/ermite.js
// (window.ermiteThemeSwitch), on the small DOM of dom.mjs. Only the
// ermiteThemeSwitch block of ermite.js is evaluated: the rest needs a page.
// Run with
//   node --test 'internal/adapters/web/jstest/*.test.mjs'
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { beforeEach, test } from 'node:test';
import { el, installDom } from './dom.mjs';

const read = (f) => readFileSync(new URL('../static/js/' + f, import.meta.url), 'utf8');
const ermite = read('ermite.js');
const start = ermite.indexOf('  window.ermiteThemeSwitch = ');
const block = ermite.slice(start, ermite.indexOf('  })();', start) + '  })();'.length);
const storeSrc = read('store.js');

const doc = installDom();
let listeners;
let storage;
let root;
let scheme;
let colors;
let button;
let icon;

// A page as shell.templ renders it: the metas and the footer button in auto.
// Then store.js runs (in <head>) and, unless withSwitch is false, the block of
// ermite.js (deferred).
function load({ stored, withSwitch = true } = {}) {
  listeners = {};
  storage = new Map(stored ? Object.entries(stored) : []);
  globalThis.localStorage = {
    getItem: (k) => (storage.has(k) ? storage.get(k) : null),
    setItem: (k, v) => { storage.set(k, String(v)); },
    removeItem: (k) => { storage.delete(k); },
  };
  globalThis.addEventListener = (type, f) => (listeners[type] ||= []).push(f);
  globalThis.removeEventListener = (type, f) => { listeners[type] = (listeners[type] || []).filter((g) => g !== f); };
  root = el('html');
  doc.documentElement = root;
  scheme = el('meta', { attrs: { name: 'color-scheme', content: 'light dark' } });
  colors = [
    el('meta', { attrs: { name: 'theme-color', 'data-scheme': 'light', media: '(prefers-color-scheme: light)', content: '#ffffff' } }),
    el('meta', { attrs: { name: 'theme-color', 'data-scheme': 'dark', media: '(prefers-color-scheme: dark)', content: '#0c0c0c' } }),
  ];
  icon = el('span', { cls: 'ico i-sun-moon', attrs: { 'aria-hidden': 'true' } });
  button = el('button', { cls: 'foot-theme', attrs: { type: 'button', 'data-js': '', 'data-theme-switch': '' } }, icon);
  doc.body.replaceChildren(scheme, ...colors, el('footer', { cls: 'foot' }, button));
  new Function(storeSrc)();
  if (withSwitch) new Function(block)();
  return window.ermiteTheme;
}

const state = () => ({
  attr: root.getAttribute('data-theme'),
  scheme: scheme.getAttribute('content'),
  colors: colors.map((m) => m.getAttribute('content')),
});
const click = (target) => listeners.click.forEach((f) => f({ target }));
const pageshow = (persisted) => listeners.pageshow.forEach((f) => f({ persisted }));
const storageEvent = (newValue) => listeners.storage.forEach((f) => f({ key: 'theme', newValue, storageArea: localStorage }));

beforeEach(() => { delete globalThis.localStorage; });

test('nothing stored: auto, the page is left to the system', () => {
  const T = load({ withSwitch: false });
  assert.equal(T.get(), 'auto');
  assert.deepEqual(state(), { attr: null, scheme: 'light dark', colors: ['#ffffff', '#0c0c0c'] });
});

test('store.js applies a stored theme before ermite.js runs', () => {
  load({ stored: { theme: 'dark' }, withSwitch: false });
  assert.deepEqual(state(), { attr: 'dark', scheme: 'dark', colors: ['#0c0c0c', '#0c0c0c'] });
  load({ stored: { theme: 'light' }, withSwitch: false });
  assert.deepEqual(state(), { attr: 'light', scheme: 'light', colors: ['#ffffff', '#ffffff'] });
});

test('a stored value that is not a theme falls back to auto', () => {
  const T = load({ stored: { theme: 'sepia' } });
  assert.equal(T.get(), 'auto');
  assert.equal(root.getAttribute('data-theme'), null);
});

test('the switch shows the stored theme on load', () => {
  load({ stored: { theme: 'dark' } });
  assert.equal(icon.className, 'ico i-moon');
  assert.equal(button.getAttribute('aria-label'), 'theme: dark (switch to auto)');
});

test('a click on the button (or its icon) cycles, stores and shows the theme', () => {
  load();
  assert.equal(button.getAttribute('aria-label'), 'theme: auto (switch to light)');
  click(icon);
  assert.equal(storage.get('theme'), 'light');
  assert.deepEqual(state(), { attr: 'light', scheme: 'light', colors: ['#ffffff', '#ffffff'] });
  assert.equal(icon.className, 'ico i-sun');
  assert.equal(button.getAttribute('aria-label'), 'theme: light (switch to dark)');
  assert.equal(button.getAttribute('title'), 'theme: light (switch to dark)');
  click(button);
  assert.equal(storage.get('theme'), 'dark');
  assert.deepEqual(state(), { attr: 'dark', scheme: 'dark', colors: ['#0c0c0c', '#0c0c0c'] });
  assert.equal(icon.className, 'ico i-moon');
  click(button);
  assert.equal(storage.get('theme'), 'auto');
  assert.deepEqual(state(), { attr: null, scheme: 'light dark', colors: ['#ffffff', '#0c0c0c'] });
  assert.equal(icon.className, 'ico i-sun-moon');
  assert.equal(button.getAttribute('aria-label'), 'theme: auto (switch to light)');
});

test('a page back from the back/forward cache shows the stored theme', () => {
  load();
  storage.set('theme', 'light');
  pageshow(false);
  assert.equal(root.getAttribute('data-theme'), null, 'a fresh load: store.js already applied it');
  pageshow(true);
  assert.deepEqual(state(), { attr: 'light', scheme: 'light', colors: ['#ffffff', '#ffffff'] });
  assert.equal(icon.className, 'ico i-sun');
});

test('other clicks do nothing', () => {
  const T = load();
  click(doc.body);
  click({ closest: undefined });
  assert.equal(T.get(), 'auto');
});

test("another tab's change applies live", () => {
  const T = load();
  storage.set('theme', 'dark');
  storageEvent('dark');
  assert.equal(T.get(), 'dark');
  assert.equal(root.getAttribute('data-theme'), 'dark');
  assert.equal(icon.className, 'ico i-moon');
  storage.delete('theme');
  storageEvent(null);
  assert.equal(root.getAttribute('data-theme'), null, 'removed: back to auto');
  assert.equal(button.getAttribute('aria-label'), 'theme: auto (switch to light)');
});

test('without usable storage the choice holds for the page', () => {
  load();
  globalThis.localStorage = {
    getItem: () => { throw new Error('denied'); },
    setItem: () => { throw new Error('quota'); },
    removeItem: () => { throw new Error('denied'); },
  };
  click(button);
  assert.equal(window.ermiteTheme.get(), 'light');
  assert.equal(root.getAttribute('data-theme'), 'light');
});
