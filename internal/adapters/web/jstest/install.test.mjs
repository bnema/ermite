// Tests of the install block of static/js/ermite.js (ermiteInstall): the
// service worker registration and the "install app" button. Run with
//   node --test 'internal/adapters/web/jstest/*.test.mjs'
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { test } from 'node:test';
import { dom, el, installDom } from './dom.mjs';

const doc = installDom();
const button = el('button', { attrs: { 'data-install': '' } });
button.hidden = true;
document.body.append(button);

const setNavigator = (v) => Object.defineProperty(globalThis, 'navigator', { value: v, configurable: true, writable: true });
const listeners = {};
globalThis.addEventListener = (type, fn) => { listeners[type] = fn; };
const fire = (type, ev) => listeners[type](ev);
let standalone = false;
globalThis.matchMedia = () => ({ matches: standalone });
doc.readyState = 'loading';
const registered = [];
setNavigator({ serviceWorker: { register: async (url, opts) => { registered.push([url, opts]); return {}; } } });

// Only the ermiteInstall block is evaluated: the rest needs a page.
const src = readFileSync(new URL('../static/js/ermite.js', import.meta.url), 'utf8');
const start = src.indexOf('  window.ermiteInstall = ');
const end = src.indexOf('  })();', start) + '  })();'.length;
new Function('$$', src.slice(start, end))((sel) => document.querySelectorAll(sel));
const I = window.ermiteInstall;

// promptEvent is a beforeinstallprompt event.
function promptEvent(outcome = 'accepted') {
  const e = { prevented: false, prompts: 0, preventDefault() { this.prevented = true; } };
  e.prompt = async () => { e.prompts++; };
  e.userChoice = Promise.resolve({ outcome });
  return e;
}

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

test('the button shows on beforeinstallprompt, and a click prompts once', async () => {
  const e = promptEvent();
  assert.equal(button.hidden, true);
  fire('beforeinstallprompt', e);
  assert.equal(e.prevented, true, 'the browser banner is replaced by the button');
  assert.equal(button.hidden, false);
  const done = listeners.click({ target: { closest: (s) => (s === '[data-install]' ? button : null) } });
  assert.equal(done, undefined);
  assert.equal(await I.pending, 'accepted');
  assert.equal(await I.prompt(), '', 'the event was used by the click');
  assert.equal(e.prompts, 1);
  assert.equal(button.hidden, true);
});

test('the button stays while the dialog is up; its focus then moves to the search', async () => {
  const search = el('button', { attrs: { 'aria-controls': 'panel-cmd' } });
  document.body.append(search);
  const e = promptEvent('dismissed');
  let shownDuring = null;
  e.prompt = async () => { shownDuring = !button.hidden; };
  fire('beforeinstallprompt', e);
  button.focus();
  dom.focused = [];
  assert.equal(await I.prompt(), 'dismissed');
  assert.equal(shownDuring, true, 'not hidden under the dialog');
  assert.equal(button.hidden, true);
  assert.deepEqual(dom.focused, [search]);
  dom.active = null;
  search.remove();
});

test('prompt without an event does nothing; a dismissal reports its outcome', async () => {
  assert.equal(await I.prompt(), '');
  const e = promptEvent('dismissed');
  fire('beforeinstallprompt', e);
  assert.equal(await I.prompt(), 'dismissed');
  assert.equal(button.hidden, true);
});

test('appinstalled hides the button', () => {
  fire('beforeinstallprompt', promptEvent());
  assert.equal(button.hidden, false);
  fire('appinstalled', {});
  assert.equal(button.hidden, true);
});

test('an app that runs installed never shows the button', () => {
  standalone = true;
  const e = promptEvent();
  fire('beforeinstallprompt', e);
  assert.equal(button.hidden, true);
  assert.equal(e.prevented, false);
  standalone = false;
});
