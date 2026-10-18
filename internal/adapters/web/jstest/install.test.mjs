// Tests of the install block of static/js/ermite.js (ermiteInstall): the
// service worker registration. Run with
//   node --test 'internal/adapters/web/jstest/*.test.mjs'
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { test } from 'node:test';
import { installDom } from './dom.mjs';

const doc = installDom();
const setNavigator = (v) => Object.defineProperty(globalThis, 'navigator', { value: v, configurable: true, writable: true });
const listeners = {};
globalThis.addEventListener = (type, fn) => { listeners[type] = fn; };
doc.readyState = 'loading';
const registered = [];
setNavigator({ serviceWorker: { register: async (url, opts) => { registered.push([url, opts]); return {}; } } });

// Only the ermiteInstall block is evaluated: the rest needs a page.
const src = readFileSync(new URL('../static/js/ermite.js', import.meta.url), 'utf8');
const start = src.indexOf('  window.ermiteInstall = ');
const end = src.indexOf('  })();', start) + '  })();'.length;
new Function(src.slice(start, end))();
const I = window.ermiteInstall;

test('the service worker is registered at the root scope once the page has loaded', async () => {
  assert.equal(registered.length, 0, 'not before load');
  await listeners.load();
  assert.deepEqual(registered, [['/-/sw.js', { scope: '/' }]]);
});

test('a refused registration is not an error', async () => {
  setNavigator({ serviceWorker: { register: () => Promise.reject(new Error('insecure')) } });
  assert.equal(await I.register(), null);
  setNavigator({});
  assert.equal(await I.register(), null);
});

test('the page leaves the browser its own install offer', () => {
  assert.equal(listeners.beforeinstallprompt, undefined, 'no preventDefault on beforeinstallprompt');
});
