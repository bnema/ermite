// Tests of static/js/passkey.js on a tiny hand-rolled DOM, with fetch and
// navigator.credentials stubbed. Run with
//   node --test 'internal/adapters/web/jstest/*.test.mjs'
import assert from 'node:assert/strict';
import { test, beforeEach } from 'node:test';
import { dom, el, installDom } from './dom.mjs';

const doc = installDom({ parseHTML: () => [] });
// The script registers its listeners when it loads: record them.
const listeners = [];
doc.addEventListener = (type, fn) => { listeners.push([type, fn]); };
// Node defines navigator as a getter: replace it, as a browser test would.
const setNavigator = (v) => Object.defineProperty(globalThis, 'navigator', { value: v, configurable: true, writable: true });
setNavigator({ credentials: {} });
globalThis.PublicKeyCredential = undefined;
await import('../static/js/passkey.js');
const P = window.ermitePasskey;

const OPTIONS = { publicKey: { challenge: 'abc' } };
const CRED = { toJSON: () => ({ id: 'cid', type: 'public-key' }) };

let fetches; // {url, init}
let answers; // url -> {ok, status, body} (body: object, or a function that throws, or undefined for no JSON)
let parsed; // [kind, publicKey]
let nav; // where the page was sent: 'assign:/' or 'reload'
let status;
let name;

// page builds the DOM of the login page (a button) or the passkeys page (a form
// with the name field and a submit button).
function page(kind) {
  dom.body = el('body');
  dom.focused = [];
  status = el('p', { attrs: { 'data-passkey-status': '' } });
  name = el('input', { attrs: { 'data-passkey-name': '' }, value: '  Pixel 9  ' });
  const btn = el('button', { attrs: { 'data-passkey': kind, 'data-begin': '/begin', 'data-finish': '/finish' } });
  if (kind === 'register') {
    const form = el('form', { attrs: { 'data-passkey-form': '' } }, name, btn);
    dom.body.append(status, form);
    btn.form = form;
  } else {
    dom.body.append(status, btn);
  }
  return btn;
}

beforeEach(() => {
  P.reset();
  fetches = [];
  parsed = [];
  nav = '';
  answers = {
    '/begin': { ok: true, status: 200, body: { ceremony: 'c1', options: OPTIONS } },
    '/finish': { ok: true, status: 204, body: undefined },
  };
  globalThis.fetch = async (url, init) => {
    fetches.push({ url, init });
    const a = answers[url];
    return {
      ok: a.ok,
      status: a.status,
      json: async () => {
        if (a.body === undefined) throw new SyntaxError('Unexpected end of JSON input');
        return a.body;
      },
    };
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
// submitOn sends a submit event from the form of btn (a click on the submit
// button, or Enter in the field); it answers whether the page's own navigation
// was kept (no preventDefault).
const submitOn = (target) => {
  let prevented = false;
  const p = P.submit({ target, preventDefault() { prevented = true; } });
  return { p, prevented: () => prevented };
};
const formOf = (btn) => btn.form;
const jsonBody = (i) => JSON.parse(fetches[i].init.body);

test('the script registers exactly one click and one submit listener', () => {
  assert.deepEqual(listeners.map(([t]) => t).sort(), ['click', 'submit']);
  assert.equal(listeners.find(([t]) => t === 'click')[1], P.click);
  assert.equal(listeners.find(([t]) => t === 'submit')[1], P.submit);
});

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
  assert.equal(fetches[1].init.credentials, 'same-origin');
  assert.equal(fetches[1].init.headers['Content-Type'], 'application/json');
  assert.deepEqual(jsonBody(1), { ceremony: 'c1', name: '', credential: { id: 'cid', type: 'public-key' } });
  assert.equal(nav, 'assign:/');
  assert.equal(btn.disabled, true, 'stays disabled while the page navigates');
});

test('register: the trimmed name goes to begin and to finish, then reload', async () => {
  const btn = page('register');
  const { p, prevented } = submitOn(formOf(btn));
  await p;

  assert.equal(prevented(), true, 'the form never navigates by itself');
  assert.equal(fetches[0].url, '/begin');
  assert.equal(fetches[0].init.credentials, 'same-origin');
  assert.equal(fetches[0].init.headers['Content-Type'], 'application/json');
  assert.deepEqual(JSON.parse(fetches[0].init.body), { name: 'Pixel 9' });
  assert.deepEqual(parsed, [['create', OPTIONS.publicKey]]);
  assert.deepEqual(navigator.created, { publicKey: { parsedCreate: OPTIONS.publicKey } });
  assert.equal(fetches[1].init.credentials, 'same-origin');
  assert.deepEqual(jsonBody(1), { ceremony: 'c1', name: 'Pixel 9', credential: { id: 'cid', type: 'public-key' } });
  assert.equal(nav, 'reload');
});

test('Enter in the name field submits: the event comes from the form or the field', async () => {
  const btn = page('register');
  await submitOn(name).p; // an event whose target is inside the form
  assert.equal(fetches.length, 2);
  assert.equal(nav, 'reload');
  void btn;
});

test('the add button is not a click target: only the submit runs it', async () => {
  const btn = page('register');
  await clickOn(btn);
  assert.equal(fetches.length, 0, 'the click becomes a submit in a browser');
});

test('a submit from another form is not ours', async () => {
  page('register');
  const other = el('form', {}, el('input'));
  dom.body.append(other);
  const { p, prevented } = submitOn(other);
  await p;
  assert.equal(fetches.length, 0);
  assert.equal(prevented(), false, 'other forms submit as they always did');
});

test('a click inside the login button reaches it, other clicks do nothing', async () => {
  const btn = page('login');
  const inner = el('span');
  btn.append(inner);
  await clickOn(inner);
  assert.equal(fetches.length, 2);

  fetches = [];
  await clickOn(el('div'));
  assert.equal(fetches.length, 0);
});

// While the ceremony waits for the authenticator the button is busy, the status
// says so, and a second click or Enter starts nothing.
test('busy: aria-busy, the waiting text, and a double click makes one begin', async () => {
  const btn = page('login');
  let release;
  navigator.credentials.get = () => new Promise((resolve) => { release = () => resolve(CRED); });

  const first = clickOn(btn);
  await new Promise((r) => setTimeout(r, 0));
  assert.equal(btn.getAttribute('aria-busy'), 'true');
  assert.equal(btn.disabled, true);
  assert.equal(status.textContent, 'waiting for your passkey…');

  await clickOn(btn); // the double click
  assert.equal(fetches.filter((f) => f.url === '/begin').length, 1, 'one begin only');

  release();
  await first;
  assert.equal(fetches.filter((f) => f.url === '/begin').length, 1);
  assert.equal(nav, 'assign:/');
});

test('a double submit makes one begin', async () => {
  const btn = page('register');
  let release;
  navigator.credentials.create = () => new Promise((resolve) => { release = () => resolve(CRED); });
  const a = submitOn(formOf(btn)).p;
  await new Promise((r) => setTimeout(r, 0));
  await submitOn(formOf(btn)).p;
  assert.equal(fetches.filter((f) => f.url === '/begin').length, 1);
  release();
  await a;
  assert.equal(nav, 'reload');
});

test('after a failure the page is free for another try', async () => {
  const btn = page('register');
  answers['/finish'] = { ok: false, status: 400, body: { error: 'nope' } };
  await submitOn(formOf(btn)).p;
  assert.equal(status.textContent, 'nope');
  assert.equal(btn.disabled, false);
  await submitOn(formOf(btn)).p;
  assert.equal(fetches.filter((f) => f.url === '/begin').length, 2, 'the second try ran');
});

test('on failure the button is enabled again, not busy, and has the focus', async () => {
  const btn = page('login');
  answers['/finish'] = { ok: false, status: 401, body: { error: 'passkey not accepted' } };
  await clickOn(btn);
  assert.equal(status.textContent, 'passkey not accepted');
  assert.equal(btn.disabled, false);
  assert.equal(btn.hasAttribute('aria-busy'), false);
  assert.equal(nav, '');
  assert.deepEqual(dom.focused, [btn], 'the focus is given back');
});

test('on success the button stays busy and disabled, without stealing focus', async () => {
  const btn = page('login');
  await clickOn(btn);
  assert.equal(btn.disabled, true);
  assert.equal(btn.getAttribute('aria-busy'), 'true');
  assert.deepEqual(dom.focused, []);
});

test('begin refused: the server reason is shown, nothing else is called', async () => {
  const btn = page('login');
  answers['/begin'] = { ok: false, status: 429, body: { error: 'too many attempts' } };
  await clickOn(btn);
  assert.equal(status.textContent, 'too many attempts');
  assert.equal(fetches.length, 1);
  assert.equal(nav, '');
  assert.equal(btn.disabled, false);
});

test('a refusal without a JSON body falls back to the status', async () => {
  const btn = page('login');
  answers['/finish'] = { ok: false, status: 502, body: undefined };
  await clickOn(btn);
  assert.equal(status.textContent, 'the server answered 502');
  assert.equal(nav, '');
});

test('a 403 without a reason says the session ended', async () => {
  for (const body of [undefined, { error: 'forbidden' }, {}]) {
    const btn = page('register');
    answers['/begin'] = { ok: false, status: 403, body };
    await submitOn(formOf(btn)).p;
    assert.equal(status.textContent, 'your session ended or this page is out of date; reload and sign in again');
    assert.equal(fetches.length, 1);
    fetches = [];
  }
  // At finish too.
  const btn = page('login');
  answers['/begin'] = { ok: true, status: 200, body: { ceremony: 'c1', options: OPTIONS } };
  answers['/finish'] = { ok: false, status: 403, body: undefined };
  await clickOn(btn);
  assert.match(status.textContent, /your session ended/);
});

test('a 403 with a reason of its own shows that reason', async () => {
  const btn = page('register');
  answers['/begin'] = { ok: false, status: 403, body: { error: 'the owner must sign in first' } };
  await submitOn(formOf(btn)).p;
  assert.equal(status.textContent, 'the owner must sign in first');
});

test('the owner cancels the prompt', async () => {
  const btn = page('login');
  navigator.credentials.get = async () => { throw Object.assign(new Error('x'), { name: 'NotAllowedError' }); };
  await clickOn(btn);
  assert.match(status.textContent, /cancelled, or it took too long/);
  assert.equal(fetches.length, 1, 'finish is not called');
  assert.equal(btn.disabled, false);
  assert.deepEqual(dom.focused, [btn]);
});

test('AbortError says cancelled', async () => {
  const btn = page('login');
  navigator.credentials.get = async () => { throw Object.assign(new Error('The operation was aborted.'), { name: 'AbortError' }); };
  await clickOn(btn);
  assert.equal(status.textContent, 'cancelled');
  assert.equal(fetches.length, 1);
  assert.equal(btn.disabled, false);
});

test('an authenticator that already holds a passkey', async () => {
  const btn = page('register');
  navigator.credentials.create = async () => { throw Object.assign(new Error('x'), { name: 'InvalidStateError' }); };
  await submitOn(formOf(btn)).p;
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

test('a credential without toJSON is reported, not sent', async () => {
  for (const kind of ['login', 'register']) {
    fetches = [];
    const btn = page(kind);
    const bare = { id: 'x' };
    navigator.credentials.get = async () => bare;
    navigator.credentials.create = async () => bare;
    if (kind === 'login') await clickOn(btn); else await submitOn(formOf(btn)).p;
    assert.match(status.textContent, /cannot send; update the browser/);
    assert.equal(fetches.length, 1, 'finish is not called');
    assert.equal(btn.disabled, false);
  }
});

// The begin answer is checked before it reaches the browser API.
test('a begin answer that is not JSON', async () => {
  const btn = page('login');
  answers['/begin'] = { ok: true, status: 200, body: undefined }; // a 200 whose body is not JSON
  await clickOn(btn);
  assert.equal(status.textContent, 'the server sent an unexpected answer');
  assert.equal(parsed.length, 0);
  assert.equal(btn.disabled, false);
});

test('a begin answer with the wrong shape', async () => {
  for (const body of [null, 'x', [], {}, { ceremony: 'c1' }, { ceremony: 'c1', options: {} }, { ceremony: 'c1', options: { publicKey: 'x' } },
    { ceremony: '', options: OPTIONS }, { ceremony: 5, options: OPTIONS }, { options: OPTIONS }, { ceremony: 'c1', options: null }]) {
    fetches = [];
    parsed = [];
    const btn = page('login');
    answers['/begin'] = { ok: true, status: 200, body };
    await clickOn(btn);
    assert.equal(status.textContent, 'the server sent an unexpected answer', JSON.stringify(body));
    assert.equal(parsed.length, 0, 'nothing reached the browser API');
    assert.equal(fetches.length, 1);
  }
});

// A name the server would refuse is caught before the owner is prompted.
test('a name over 64 bytes (UTF-8) stops before begin', async () => {
  const btn = page('register');
  name.value = 'é'.repeat(33); // 33 characters, 66 bytes
  await submitOn(formOf(btn)).p;
  assert.match(status.textContent, /longer than 64 bytes/);
  assert.equal(fetches.length, 0, 'no request');
  assert.equal(navigator.created, undefined, 'create() is not called');
  assert.notEqual(btn.disabled, true, 'the button was never disabled');
  assert.deepEqual(dom.focused, [name], 'the field gets the focus');
  // The check is not a ceremony: the next try is not blocked.
  name.value = 'ok';
  await submitOn(formOf(btn)).p;
  assert.equal(fetches.length, 2);
});

test('64 bytes is fine, in ASCII or in two-byte characters', async () => {
  for (const ok of ['a'.repeat(64), 'é'.repeat(32)]) {
    fetches = [];
    const btn = page('register'); // a success navigates away: each try is a new page
    name.value = ok;
    await submitOn(formOf(btn)).p;
    assert.equal(fetches.length, 2, ok.length + ' characters');
    P.reset();
  }
});

test('control and direction characters stop before begin', async () => {
  for (const bad of ['a\u0000b', 'a\tb', 'a\nb', 'a\x7fb', 'a\u0085b', 'evil \u202e txt', 'a\u2066b', 'a\u200eb', 'a\u061cb', 'a\u2028b', 'a\u2029b']) {
    fetches = [];
    const btn = page('register');
    name.value = bad;
    await submitOn(formOf(btn)).p;
    assert.match(status.textContent, /control or direction character/, JSON.stringify(bad));
    assert.equal(fetches.length, 0, JSON.stringify(bad));
  }
});

test('plain text with a zero width joiner, accents and emoji is fine', () => {
  for (const ok of ['Pixel 9', 'clé de ma mère', 'a\u200db', '🔑 key', '']) {
    assert.equal(P.nameProblem(ok), '', JSON.stringify(ok));
  }
});

test('an empty name is allowed: the server names the passkey', async () => {
  const btn = page('register');
  name.value = '   ';
  await submitOn(formOf(btn)).p;
  assert.deepEqual(JSON.parse(fetches[0].init.body), { name: '' });
  assert.equal(fetches.length, 2);
});

test('the server can still refuse the name at begin: its reason is shown', async () => {
  const btn = page('register');
  answers['/begin'] = { ok: false, status: 400, body: { error: 'passkey name: text contains the control character U+0009' } };
  await submitOn(formOf(btn)).p;
  assert.match(status.textContent, /control character/);
  assert.equal(navigator.created, undefined);
  assert.equal(btn.disabled, false);
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

test('no PublicKeyCredential at all', () => {
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

test('register needs parseCreationOptionsFromJSON', async () => {
  const btn = page('register');
  delete PublicKeyCredential.parseCreationOptionsFromJSON;
  await submitOn(formOf(btn)).p;
  assert.equal(fetches.length, 0);
  assert.equal(status.textContent, 'this browser does not support passkeys');
});
