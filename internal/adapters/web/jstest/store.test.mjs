// Tests of static/js/store.js (window.ermiteStore): the localStorage client,
// evaluated alone against a fake storage. Run with
//   node --test 'internal/adapters/web/jstest/*.test.mjs'
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { test } from 'node:test';

const src = readFileSync(new URL('../static/js/store.js', import.meta.url), 'utf8');

// load evaluates the file on a page whose storage is the given object (a
// Map-backed one by default) and answers the store and the "storage" listeners.
function load(storage = fakeStorage()) {
  const listeners = new Set();
  const g = globalThis;
  g.window = g;
  if (storage === 'missing') delete g.localStorage;
  else g.localStorage = storage;
  g.addEventListener = (type, f) => { if (type === 'storage') listeners.add(f); };
  g.removeEventListener = (type, f) => { if (type === 'storage') listeners.delete(f); };
  new Function(src)();
  const emit = (e) => [...listeners].forEach((f) => f({ storageArea: g.localStorage, ...e }));
  return { store: g.ermiteStore, listeners, emit };
}

function fakeStorage() {
  const m = new Map();
  return {
    m,
    getItem: (k) => (m.has(k) ? m.get(k) : null),
    setItem: (k, v) => { m.set(k, String(v)); },
    removeItem: (k) => { m.delete(k); },
  };
}

const refusing = () => ({
  getItem: () => { throw new Error('denied'); },
  setItem: () => { throw new Error('quota'); },
  removeItem: () => { throw new Error('denied'); },
});

test('get answers the stored value, else the fallback (null by default)', () => {
  const st = fakeStorage();
  const { store } = load(st);
  assert.equal(store.get('k'), null);
  assert.equal(store.get('k', 'x'), 'x');
  st.m.set('k', 'v');
  assert.equal(store.get('k', 'x'), 'v');
  st.m.set('k', '');
  assert.equal(store.get('k', 'x'), '', 'an empty string is a value');
});

test('set stores strings; null and undefined remove; remove forgets', () => {
  const st = fakeStorage();
  const { store } = load(st);
  store.set('k', 'v');
  assert.equal(st.m.get('k'), 'v');
  store.set('n', 3);
  assert.equal(st.m.get('n'), '3');
  store.set('k', null);
  assert.equal(st.m.has('k'), false);
  store.set('k', 'v');
  store.set('k', undefined);
  assert.equal(st.m.has('k'), false);
  store.remove('n');
  assert.equal(store.get('n'), null);
  store.remove('nothing'); // no error
});

test('without storage nothing throws and the value holds for the page', () => {
  const { store } = load('missing');
  assert.equal(store.get('k', 'x'), 'x');
  store.set('k', 'v');
  assert.equal(store.get('k', 'x'), 'v');
  store.remove('k');
  assert.equal(store.get('k', 'x'), 'x');
});

test('a storage that refuses reads and writes does not throw either', () => {
  const { store } = load(refusing());
  assert.equal(store.get('k', 'x'), 'x');
  store.set('k', 'v');
  assert.equal(store.get('k', 'x'), 'v', 'held in memory');
  store.remove('k');
  assert.equal(store.get('k', 'x'), 'x', 'removed for the page');
});

test('a quota error on write keeps the value for the page; a later write that works wins', () => {
  const st = fakeStorage();
  const { store } = load(st);
  let full = true;
  st.setItem = (k, v) => { if (full) throw new Error('quota'); st.m.set(k, v); };
  store.set('k', 'a');
  assert.equal(store.get('k'), 'a');
  assert.equal(st.m.has('k'), false);
  full = false;
  store.set('k', 'b');
  assert.equal(st.m.get('k'), 'b');
  st.m.set('k', 'c'); // changed elsewhere: the held value is gone
  assert.equal(store.get('k'), 'c');
});

test('a getter of localStorage that throws (sandboxed frame) is survived', () => {
  const { store } = load('missing');
  Object.defineProperty(globalThis, 'localStorage', { get() { throw new Error('SecurityError'); }, configurable: true });
  try {
    assert.equal(store.get('k', 'x'), 'x');
    store.set('k', 'v');
    assert.equal(store.get('k'), 'v');
  } finally {
    delete globalThis.localStorage;
  }
});

test('watch calls back with the new value of its key only, and stops when told', () => {
  const { store, emit, listeners } = load();
  const seen = [];
  const stop = store.watch('k', (v) => seen.push(v));
  emit({ key: 'other', newValue: 'z' });
  emit({ key: 'k', newValue: 'v' });
  emit({ key: 'k', newValue: null });
  emit({ key: null, newValue: null }); // the other tab cleared everything
  assert.deepEqual(seen, ['v', null, null]);
  stop();
  assert.equal(listeners.size, 0);
  emit({ key: 'k', newValue: 'w' });
  assert.deepEqual(seen, ['v', null, null]);
});

test('watch ignores the events of another storage area (sessionStorage)', () => {
  const { store, emit } = load();
  const seen = [];
  store.watch('k', (v) => seen.push(v));
  emit({ key: 'k', newValue: 'v', storageArea: {} });
  assert.deepEqual(seen, []);
});

test('an event from another tab replaces a value held for this page', () => {
  const { store, emit } = load(refusing());
  store.set('k', 'mine');
  store.watch('k', () => {});
  emit({ key: 'k', newValue: 'theirs', storageArea: undefined });
  assert.equal(store.get('k'), null, 'the held value is dropped; storage is the truth');
});

test('pref answers only values of its list, else the fallback', () => {
  const st = fakeStorage();
  const { store } = load(st);
  const p = store.pref('layout', { values: ['a', 'b'], fallback: 'a' });
  assert.equal(p.get(), 'a', 'nothing stored');
  p.set('b');
  assert.equal(st.m.get('layout'), 'b');
  assert.equal(p.get(), 'b');
  p.set('zzz'); // not allowed: ignored
  assert.equal(p.get(), 'b');
  st.m.set('layout', 'old-value');
  assert.equal(p.get(), 'a', 'an invalid stored value falls back');
});

test('pref works with the empty string as a value (keys-off)', () => {
  const st = fakeStorage();
  const { store } = load(st);
  const p = store.pref('keys-off', { values: ['1', ''], fallback: '' });
  assert.equal(p.get(), '');
  p.set('1');
  assert.equal(st.m.get('keys-off'), '1');
  p.set('');
  assert.equal(st.m.get('keys-off'), '');
  assert.equal(p.get(), '');
});

test('pref.watch gives validated values', () => {
  const { store, emit } = load();
  const p = store.pref('layout', { values: ['a', 'b'], fallback: 'a' });
  const seen = [];
  const stop = p.watch((v) => seen.push(v));
  emit({ key: 'layout', newValue: 'b' });
  emit({ key: 'layout', newValue: 'junk' });
  emit({ key: 'layout', newValue: null });
  assert.deepEqual(seen, ['b', 'a', 'a']);
  stop();
  emit({ key: 'layout', newValue: 'b' });
  assert.equal(seen.length, 3);
});
