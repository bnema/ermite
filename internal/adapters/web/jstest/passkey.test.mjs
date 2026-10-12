// Tests of static/js/passkey.js on a tiny hand-rolled DOM, with fetch and
// navigator.credentials stubbed. Run with
//   node --test 'internal/adapters/web/jstest/*.test.mjs'
import assert from 'node:assert/strict';
import { test, beforeEach } from 'node:test';
import { dom, el, installDom, settle } from './dom.mjs';

const doc = installDom({ parseHTML: () => [] });
doc.addEventListener = () => { dom.listened = true; };
// Node defines navigator as a getter: replace it, as a browser test would.
const setNavigator = (v) => Object.defineProperty(globalThis, 'navigator', { value: v, configurable: true, writable: true });
setNavigator({ credentials: {} });
globalThis.PublicKeyCredential = undefined;
await import('../static/js/passkey.js');
const P = window.ermitePasskey;

const OPTIONS = { publicKey: { challenge: 'abc' } };
const CRED = { toJSON: () => ({ id: 'cid', type: 'public-key' }) };

let fetches; // {url, init}
let answers; // url -> {ok, status, body}
let parsed; // [kind, publicKey]
let nav; // where the page was sent: 'assign:/' or 'reload'
let status;
let name;

function page(kind) {
  dom.body = el('body');
  status = el('p', { attrs: { 'data-passkey-status': '' } });
  name = el('input', { attrs: { 'data-passkey-name': '' }, value: '  Pixel 9  ' });
  const btn = el('button', { attrs: { 'data-passkey': kind, 'data-begin': '/begin', 'data-finish': '/finish' } });
  dom.body.append(status, name, btn);
  return btn;
}

beforeEach(() => {
  fetches = [];
  parsed = [];
  nav = '';
  answers = {
    '/begin': { ok: true, status: 200, body: { ceremony: 'c1', options: OPTIONS } },
    '/finish': { ok: true, status: 204, body: null },
  };
  globalThis.fetch = async (url, init) => {
    fetches.push({ url, init });
    const a = answers[url];
    return { ok: a.ok, status: a.status, json: async () => { if (a.body === null) throw new Error('no body'); return a.body; } };
  };
  globalThis.PublicKeyCredential = {
    parseRequestOptionsFromJSON: (o) => { parsed.push(['get', o]); return { parsedGet: o }; },
    parseCreationOptionsFromJSON: (o) => { parsed.push(['create', o]); return { parsedCreate: o }; },
  };
  setNavigator({
    credentials: {
      get: async (o) => { navigator.got = o; return CRED; },
      create: async (o) => { navigator.created = o; return CRED; },
    },
  });
  globalThis.location = { assign: (u) => { nav = 'assign:' + u; }, reload: () => { nav = 'reload'; } };
});

const clickOn = (target) => P.click({ target, preventDefault() {} });

test('login: begin, get, finish with toJSON(), then home', async () => {
  const btn = page('login');
  await clickOn(btn);

  assert.equal(fetches.length, 2);
  assert.equal(fetches[0].url, '/begin');
  assert.equal(fetches[0].init.method, 'POST');
  assert.equal(fetches[0].init.credentials, 'same-origin');
  assert.deepEqual(parsed, [['get', OPTIONS.publicKey]]);
  assert.deepEqual(navigator.got, { publicKey: { parsedGet: OPTIONS.publicKey } });

  assert.equal(fetches[1].url, '/finish');
  assert.equal(fetches[1].init.method, 'POST');
  assert.equal(fetches[1].init.headers['Content-Type'], 'application/json');
  assert.deepEqual(JSON.parse(fetches[1].init.body), { ceremony: 'c1', name: '', credential: { id: 'cid', type: 'public-key' } });
  assert.equal(nav, 'assign:/');
  assert.equal(status.textContent, '');
  assert.equal(btn.disabled, true, 'stays disabled while the page navigates');
});

test('register: create with the trimmed name, then reload', async () => {
  const btn = page('register');
  await clickOn(btn);

  assert.deepEqual(parsed, [['create', OPTIONS.publicKey]]);
  assert.deepEqual(navigator.created, { publicKey: { parsedCreate: OPTIONS.publicKey } });
  assert.deepEqual(JSON.parse(fetches[1].init.body), { ceremony: 'c1', name: 'Pixel 9', credential: { id: 'cid', type: 'public-key' } });
  assert.equal(nav, 'reload');
});

test('a click inside the button reaches it, other clicks do nothing', async () => {
  const btn = page('login');
  const inner = el('span');
  btn.append(inner);
  await clickOn(inner);
  assert.equal(fetches.length, 2);

  fetches = [];
  await clickOn(el('div'));
  assert.equal(fetches.length, 0);
});

test('begin refused: the server reason is shown, nothing else is called', async () => {
  const btn = page('login');
  answers['/begin'] = { ok: false, status: 429, body: { error: 'too many attempts' } };
  await clickOn(btn);
  assert.equal(status.textContent, 'too many attempts');
  assert.equal(fetches.length, 1);
  assert.equal(nav, '');
  assert.equal(btn.disabled, false, 'the button is usable again');
});

test('finish refused: the reason is shown and the page stays', async () => {
  const btn = page('login');
  answers['/finish'] = { ok: false, status: 401, body: { error: 'passkey not accepted' } };
  await clickOn(btn);
  assert.equal(status.textContent, 'passkey not accepted');
  assert.equal(nav, '');
  assert.equal(btn.disabled, false);
});

test('a refusal without a JSON body falls back to the status', async () => {
  const btn = page('register');
  answers['/finish'] = { ok: false, status: 502, body: null };
  await clickOn(btn);
  assert.equal(status.textContent, 'the server answered 502');
  assert.equal(nav, '');
});

test('the owner cancels the prompt', async () => {
  const btn = page('login');
  navigator.credentials.get = async () => { throw Object.assign(new Error('x'), { name: 'NotAllowedError' }); };
  await clickOn(btn);
  assert.match(status.textContent, /cancelled, or it took too long/);
  assert.equal(fetches.length, 1, 'finish is not called');
  assert.equal(btn.disabled, false);
});

test('an authenticator that already holds a passkey', async () => {
  const btn = page('register');
  navigator.credentials.create = async () => { throw Object.assign(new Error('x'), { name: 'InvalidStateError' }); };
  await clickOn(btn);
  assert.match(status.textContent, /already holds a passkey/);
  assert.equal(fetches.length, 1);
});

test('a network failure is reported', async () => {
  const btn = page('login');
  globalThis.fetch = async () => { throw new TypeError('Failed to fetch'); };
  await clickOn(btn);
  assert.equal(status.textContent, 'Failed to fetch');
  assert.equal(btn.disabled, false);
});

test('a null credential is not sent', async () => {
  const btn = page('login');
  navigator.credentials.get = async () => null;
  await clickOn(btn);
  assert.equal(status.textContent, 'no passkey was returned');
  assert.equal(fetches.length, 1);
});

test('an old browser: the buttons are disabled with a reason, nothing is fetched', async () => {
  const login = page('login');
  const register = el('button', { attrs: { 'data-passkey': 'register' } });
  dom.body.append(register);
  globalThis.PublicKeyCredential = {}; // no parse*OptionsFromJSON
  P.init();
  assert.equal(login.disabled, true);
  assert.equal(register.disabled, true);
  assert.equal(status.textContent, 'this browser does not support passkeys');

  login.disabled = false;
  await clickOn(login);
  assert.equal(fetches.length, 0);
  assert.equal(status.textContent, 'this browser does not support passkeys');
});

test('no PublicKeyCredential at all', async () => {
  const btn = page('login');
  globalThis.PublicKeyCredential = undefined;
  P.init();
  assert.equal(btn.disabled, true);
  assert.equal(status.textContent, 'this browser does not support passkeys');
});

test('a supported browser keeps the buttons enabled', () => {
  const btn = page('login');
  P.init();
  assert.equal(btn.disabled, undefined);
  assert.equal(status.textContent, '');
});

test('a disabled button ignores clicks', async () => {
  const btn = page('login');
  btn.disabled = true;
  await clickOn(btn);
  assert.equal(fetches.length, 0);
});

test('only login supports needs parseRequestOptionsFromJSON, register parseCreationOptionsFromJSON', async () => {
  const btn = page('register');
  delete PublicKeyCredential.parseCreationOptionsFromJSON;
  await clickOn(btn);
  assert.equal(fetches.length, 0);
  assert.equal(status.textContent, 'this browser does not support passkeys');
});

test('the script registers one delegated click listener', () => {
  assert.equal(dom.listened, true);
  void settle;
});
