// Tests of static/js/passkey.js on a tiny hand-rolled DOM, with fetch and
// navigator.credentials stubbed. Run with
//   node --test 'internal/adapters/web/jstest/*.test.mjs'
import assert from 'node:assert/strict';
import { test as nodeTest, beforeEach, afterEach } from 'node:test';
import { dom, el, installDom } from './dom.mjs';

// Every test has two seconds: a ceremony that never settles fails the test
// instead of hanging the run.
const test = (title, fn) => nodeTest(title, { timeout: 2000 }, fn);

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
let answers; // url -> {ok, status, body} (body undefined: not JSON)
let parsed; // [kind, publicKey]
let nav; // where the page was sent: 'assign:/' or 'reload'
let status;
let name;

// page builds the DOM of the login page (a form with the sign-in button) or of
// the passkeys page (a form with the name field and the add button). Both
// buttons are submit buttons of a [data-passkey-form].
function page(kind) {
  dom.body = el('body');
  dom.focused = [];
  status = el('p', { attrs: { 'data-passkey-status': '' } });
  name = el('input', { attrs: { 'data-passkey-name': '' }, value: '  Pixel 9  ' });
  const btn = el('button', { attrs: { 'data-passkey': kind, 'data-begin': '/begin', 'data-finish': '/finish', type: 'submit' } });
  const form = el('form', { attrs: { 'data-passkey-form': '' } }, ...(kind === 'register' ? [name, btn] : [btn]));
  dom.body.append(status, form);
  btn.form = form;
  return btn;
}

// pending holds the resolvers of the authenticator prompts a test left open;
// afterEach answers them, so that no promise outlives its test.
let pending = [];
const prompt = (value) => new Promise((resolve) => { pending.push(() => resolve(value)); });
afterEach(() => { pending.forEach((release) => release()); pending = []; });

beforeEach(() => {
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

// submitOn sends a submit event from a form (a click on its submit button, or
// Enter in a field). It answers the promise of the handler and whether the
// browser's own navigation was kept (no preventDefault).
const submitOn = (target) => {
  let prevented = false;
  const p = P.submit({ target, preventDefault() { prevented = true; } });
  return { p, prevented: () => prevented };
};
const submitForm = (btn) => submitOn(btn.form).p;
const jsonBody = (i) => JSON.parse(fetches[i].init.body);
const beginCalls = () => fetches.filter((f) => f.url === '/begin').length;
const tick = () => new Promise((r) => setTimeout(r, 0));

test('the script registers exactly one submit listener and no click listener', () => {
  assert.deepEqual(listeners.map(([t]) => t), ['submit']);
  assert.equal(listeners[0][1], P.submit);
  assert.deepEqual(Object.keys(P).sort(), ['init', 'nameProblem', 'submit'], 'only what the tests need is exported');
});

test('login: begin, get, finish with toJSON(), then home', async () => {
  const btn = page('login');
  const { p, prevented } = submitOn(btn.form);
  await p;

  assert.equal(prevented(), true, 'the form never navigates by itself');
  assert.equal(fetches.length, 2);
  assert.equal(fetches[0].url, '/begin');
  assert.equal(fetches[0].init.method, 'POST');
  assert.equal(fetches[0].init.credentials, 'same-origin');
  assert.equal(fetches[0].init.body, undefined, 'a sign-in sends no name');
  assert.deepEqual(parsed, [['get', OPTIONS.publicKey]]);
  assert.deepEqual(navigator.got, { publicKey: { parsedGet: OPTIONS.publicKey } });

  assert.equal(fetches[1].url, '/finish');
  assert.equal(fetches[1].init.method, 'POST');
  assert.equal(fetches[1].init.credentials, 'same-origin');
  assert.equal(fetches[1].init.headers['Content-Type'], 'application/json');
  assert.deepEqual(jsonBody(1), { ceremony: 'c1', credential: { id: 'cid', type: 'public-key' } });
  assert.equal(nav, 'assign:/');
  assert.equal(btn.disabled, true, 'stays disabled while the page navigates');
});

test('register: the trimmed name goes to begin only, then reload', async () => {
  const btn = page('register');
  const { p, prevented } = submitOn(btn.form);
  await p;

  assert.equal(prevented(), true);
  assert.equal(fetches[0].url, '/begin');
  assert.equal(fetches[0].init.credentials, 'same-origin');
  assert.equal(fetches[0].init.headers['Content-Type'], 'application/json');
  assert.deepEqual(JSON.parse(fetches[0].init.body), { name: 'Pixel 9' });
  assert.deepEqual(parsed, [['create', OPTIONS.publicKey]]);
  assert.deepEqual(navigator.created, { publicKey: { parsedCreate: OPTIONS.publicKey } });
  assert.equal(fetches[1].init.credentials, 'same-origin');
  assert.deepEqual(jsonBody(1), { ceremony: 'c1', credential: { id: 'cid', type: 'public-key' } }, 'the finish body has no name');
  assert.equal(nav, 'reload');
});

test('a submit from another form is not ours and keeps its default', async () => {
  page('register');
  const other = el('form', {}, el('input'));
  dom.body.append(other);
  const { p, prevented } = submitOn(other);
  await p;
  assert.equal(fetches.length, 0);
  assert.equal(prevented(), false, 'other forms submit as they always did');
});

// While the ceremony waits for the authenticator the button is busy and
// disabled, the status says so, and a second submit starts nothing: the
// disabled button is the guard.
test('busy: aria-busy, the waiting text, and a double submit makes one begin', async () => {
  const btn = page('login');
  navigator.credentials.get = () => prompt(CRED);

  const first = submitForm(btn);
  await tick();
  assert.equal(btn.getAttribute('aria-busy'), 'true');
  assert.equal(btn.disabled, true);
  assert.equal(status.textContent, 'waiting for your passkey…');

  await submitForm(btn); // the double click, or Enter
  assert.equal(beginCalls(), 1, 'one begin only');

  pending.forEach((release) => release());
  await first;
  assert.equal(beginCalls(), 1);
  assert.equal(nav, 'assign:/');
});

test('a double submit of the add form makes one begin', async () => {
  const btn = page('register');
  navigator.credentials.create = () => prompt(CRED);
  const first = submitForm(btn);
  await tick();
  await submitForm(btn);
  assert.equal(beginCalls(), 1);
  pending.forEach((release) => release());
  await first;
  assert.equal(nav, 'reload');
});

test('after a failure the page is free for another try', async () => {
  const btn = page('register');
  answers['/finish'] = { ok: false, status: 400, body: { error: 'nope' } };
  await submitForm(btn);
  assert.equal(status.textContent, 'nope');
  assert.equal(btn.disabled, false);
  await submitForm(btn);
  assert.equal(beginCalls(), 2, 'the second try ran');
});

test('on failure the button is enabled again, not busy, and has the focus', async () => {
  const btn = page('login');
  answers['/finish'] = { ok: false, status: 401, body: { error: 'passkey not accepted' } };
  await submitForm(btn);
  assert.equal(status.textContent, 'passkey not accepted');
  assert.equal(btn.disabled, false);
  assert.equal(btn.hasAttribute('aria-busy'), false);
  assert.equal(nav, '');
  assert.deepEqual(dom.focused, [btn], 'the focus is given back');
});

test('on success the button stays busy and disabled, without stealing focus', async () => {
  const btn = page('login');
  await submitForm(btn);
  assert.equal(btn.disabled, true);
  assert.equal(btn.getAttribute('aria-busy'), 'true');
  assert.deepEqual(dom.focused, []);
});

test('begin refused: the server reason is shown, nothing else is called', async () => {
  const btn = page('login');
  answers['/begin'] = { ok: false, status: 429, body: { error: 'too many attempts' } };
  await submitForm(btn);
  assert.equal(status.textContent, 'too many attempts');
  assert.equal(fetches.length, 1);
  assert.equal(nav, '');
  assert.equal(btn.disabled, false);
  assert.deepEqual(dom.focused, [btn]);
});

// The server can refuse the name at begin: the field is what to fix, so it
// gets the focus. Any other refusal gives the button the focus.
test('a 400 from register/begin focuses the name field', async () => {
  const btn = page('register');
  answers['/begin'] = { ok: false, status: 400, body: { error: 'passkey name: text contains the control character U+0009' } };
  await submitForm(btn);
  assert.match(status.textContent, /control character/);
  assert.equal(navigator.created, undefined, 'create() is not called');
  assert.equal(btn.disabled, false);
  assert.deepEqual(dom.focused, [name]);
});

test('other refusals of register give the focus back to the button', async () => {
  for (const [at, code] of [['/begin', 429], ['/begin', 403], ['/begin', 500], ['/finish', 400], ['/finish', 409]]) {
    const btn = page('register');
    answers = {
      '/begin': { ok: true, status: 200, body: { ceremony: 'c1', options: OPTIONS } },
      '/finish': { ok: true, status: 204, body: undefined },
      [at]: { ok: false, status: code, body: { error: 'refused' } },
    };
    await submitForm(btn);
    assert.deepEqual(dom.focused, [btn], at + ' ' + code);
  }
});

test('a 400 at begin on the login page focuses the button: there is no name', async () => {
  const btn = page('login');
  answers['/begin'] = { ok: false, status: 400, body: { error: 'malformed request' } };
  await submitForm(btn);
  assert.deepEqual(dom.focused, [btn]);
});

test('a refusal without a JSON body falls back to the status', async () => {
  const btn = page('login');
  answers['/finish'] = { ok: false, status: 502, body: undefined };
  await submitForm(btn);
  assert.equal(status.textContent, 'the server answered 502');
  assert.equal(nav, '');
});

test('a 403 without a reason says the session ended', async () => {
  for (const body of [undefined, { error: 'forbidden' }, {}]) {
    fetches = [];
    const btn = page('register');
    answers['/begin'] = { ok: false, status: 403, body };
    await submitForm(btn);
    assert.equal(status.textContent, 'your session ended or this page is out of date; reload and sign in again');
    assert.equal(fetches.length, 1);
  }
  // At finish too.
  const btn = page('login');
  answers['/begin'] = { ok: true, status: 200, body: { ceremony: 'c1', options: OPTIONS } };
  answers['/finish'] = { ok: false, status: 403, body: undefined };
  await submitForm(btn);
  assert.match(status.textContent, /your session ended/);
});

test('a 403 with a reason of its own shows that reason', async () => {
  const btn = page('register');
  answers['/begin'] = { ok: false, status: 403, body: { error: 'the owner must sign in first' } };
  await submitForm(btn);
  assert.equal(status.textContent, 'the owner must sign in first');
});

test('the owner cancels the prompt', async () => {
  const btn = page('login');
  navigator.credentials.get = async () => { throw Object.assign(new Error('x'), { name: 'NotAllowedError' }); };
  await submitForm(btn);
  assert.match(status.textContent, /cancelled, or it took too long/);
  assert.equal(fetches.length, 1, 'finish is not called');
  assert.equal(btn.disabled, false);
  assert.deepEqual(dom.focused, [btn]);
});

test('AbortError says cancelled', async () => {
  const btn = page('login');
  navigator.credentials.get = async () => { throw Object.assign(new Error('The operation was aborted.'), { name: 'AbortError' }); };
  await submitForm(btn);
  assert.equal(status.textContent, 'cancelled');
  assert.equal(fetches.length, 1);
  assert.equal(btn.disabled, false);
});

test('an authenticator that already holds a passkey', async () => {
  const btn = page('register');
  navigator.credentials.create = async () => { throw Object.assign(new Error('x'), { name: 'InvalidStateError' }); };
  await submitForm(btn);
  assert.match(status.textContent, /already holds a passkey/);
  assert.equal(fetches.length, 1);
});

test('a network failure is reported', async () => {
  const btn = page('login');
  globalThis.fetch = async () => { throw new TypeError('Failed to fetch'); };
  await submitForm(btn);
  assert.equal(status.textContent, 'Failed to fetch');
  assert.equal(btn.disabled, false);
});

test('a null credential is not sent', async () => {
  const btn = page('login');
  navigator.credentials.get = async () => null;
  await submitForm(btn);
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
    await submitForm(btn);
    assert.match(status.textContent, /cannot send; update the browser/);
    assert.equal(fetches.length, 1, 'finish is not called');
    assert.equal(btn.disabled, false);
  }
});

// The begin answer is checked before it reaches the browser API.
test('a begin answer that is not JSON', async () => {
  const btn = page('login');
  answers['/begin'] = { ok: true, status: 200, body: undefined }; // a 200 whose body is not JSON
  await submitForm(btn);
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
    await submitForm(btn);
    assert.equal(status.textContent, 'the server sent an unexpected answer', JSON.stringify(body));
    assert.equal(parsed.length, 0, 'nothing reached the browser API');
    assert.equal(fetches.length, 1);
  }
});

// A name the server would refuse is caught before the owner is prompted.
test('a name over 64 bytes (UTF-8) stops before begin', async () => {
  const btn = page('register');
  name.value = 'é'.repeat(33); // 33 characters, 66 bytes
  await submitForm(btn);
  assert.match(status.textContent, /longer than 64 bytes/);
  assert.equal(fetches.length, 0, 'no request');
  assert.equal(navigator.created, undefined, 'create() is not called');
  assert.notEqual(btn.disabled, true, 'the button was never disabled');
  assert.deepEqual(dom.focused, [name], 'the field gets the focus');
  // The check is not a ceremony: the next try is not blocked.
  name.value = 'ok';
  await submitForm(btn);
  assert.equal(fetches.length, 2);
});

test('64 bytes is fine, in ASCII or in two-byte characters', async () => {
  for (const ok of ['a'.repeat(64), 'é'.repeat(32)]) {
    fetches = [];
    const btn = page('register'); // a success navigates away: each try is a new page
    name.value = ok;
    await submitForm(btn);
    assert.equal(fetches.length, 2, ok.length + ' characters');
  }
});

test('control and direction characters stop before begin', async () => {
  for (const bad of ['a\u0000b', 'a\tb', 'a\nb', 'a\x7fb', 'a\u0085b', 'evil \u202e txt', 'a\u2066b', 'a\u200eb', 'a\u061cb', 'a\u2028b', 'a\u2029b']) {
    fetches = [];
    const btn = page('register');
    name.value = bad;
    await submitForm(btn);
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
  await submitForm(btn);
  assert.deepEqual(JSON.parse(fetches[0].init.body), { name: '' });
  assert.equal(fetches.length, 2);
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
  await submitForm(login);
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

test('a disabled button ignores a submit', async () => {
  const btn = page('login');
  btn.disabled = true;
  await submitForm(btn);
  assert.equal(fetches.length, 0);
});

test('register needs parseCreationOptionsFromJSON', async () => {
  const btn = page('register');
  delete PublicKeyCredential.parseCreationOptionsFromJSON;
  await submitForm(btn);
  assert.equal(fetches.length, 0);
  assert.equal(status.textContent, 'this browser does not support passkeys');
});
