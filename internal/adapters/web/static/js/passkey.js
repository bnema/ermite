// passkey.js: sign in with a passkey (/-/login) and add one (/-/passkeys).
//
// One delegated submit handler for both pages: the sign-in button and the "add
// a passkey" button are the submit buttons of a [data-passkey-form], so that a
// click, Enter in the name field and the keyboard all end in the same place.
// The button carries the two endpoints (data-begin, data-finish). While a
// ceremony runs the button is disabled, and a disabled submit button makes the
// form submit nothing: that is the only guard against a second prompt. The flow
// is the one of the WebAuthn JSON API: POST begin, hand options.publicKey to the
// browser (parse*OptionsFromJSON, then navigator.credentials.get|create), POST
// the result (toJSON()) to finish. Nothing is inline: the CSP only allows
// scripts from this origin. The core is tested in Node by
// jstest/passkey.test.mjs.
window.ermitePasskey = (() => {
  const UNSUPPORTED = 'this browser does not support passkeys';
  const UNEXPECTED = 'the server sent an unexpected answer';
  const SESSION_ENDED = 'your session ended or this page is out of date; reload and sign in again';
  const WAITING = 'waiting for your passkey…';
  const MAX_NAME_BYTES = 64;

  // The parse function each ceremony needs; absent in a browser that predates
  // the WebAuthn JSON API.
  const parser = { login: 'parseRequestOptionsFromJSON', register: 'parseCreationOptionsFromJSON' };
  const supported = (kind) => {
    const pkc = window.PublicKeyCredential;
    return !!pkc && typeof pkc[parser[kind]] === 'function' && !!navigator.credentials;
  };

  const statusEl = () => document.querySelector('[data-passkey-status]');
  const say = (text) => { const s = statusEl(); if (s) s.textContent = text; };

  // nameProblem says why a passkey name would be refused, or '' when it is fine.
  // It mirrors the server (domain.ParsePasskeyName): at most 64 bytes of UTF-8,
  // one line, no control character and no bidirectional mark, override or isolate.
  // The server stays the judge; this only saves the owner a prompt that would end
  // in a refusal.
  function nameProblem(name) {
    if (new TextEncoder().encode(name).length > MAX_NAME_BYTES) {
      return 'the name is longer than ' + MAX_NAME_BYTES + ' bytes';
    }
    // C0 and C1 controls (this includes tab and line breaks), the line and
    // paragraph separators, and the bidirectional marks (U+200E, U+200F, U+061C,
    // U+202A to U+202E, U+2066 to U+2069).
    if (/[\u0000-\u001f\u007f-\u009f\u2028\u2029\u200e\u200f\u061c\u202a-\u202e\u2066-\u2069]/.test(name)) {
      return 'the name has a control or direction character';
    }
    return '';
  }

  // messageOf turns what a step threw into one line for the owner.
  function messageOf(e) {
    if (e && e.name === 'NotAllowedError') return 'cancelled, or it took too long; try again';
    if (e && e.name === 'InvalidStateError') return 'this authenticator already holds a passkey for this site';
    if (e && e.name === 'AbortError') return 'cancelled';
    return (e && e.message) || 'something went wrong; try again';
  }

  // failure reads the error of a refused request: the server's one-line
  // reason when it sent one, the status otherwise. A 403 without a reason of
  // its own is a session that ended, or a page older than the server's state.
  async function failure(res) {
    let reason = '';
    try { reason = (await res.json()).error || ''; } catch (_) { /* not JSON */ }
    const e = new Error(reason || 'the server answered ' + res.status);
    if (res.status === 403 && (!reason || reason === 'forbidden')) e.message = SESSION_ENDED;
    e.status = res.status;
    return e;
  }

  // begin posts to the begin endpoint and checks the shape of the answer.
  async function begin(btn, kind, name) {
    const init = { method: 'POST', credentials: 'same-origin' };
    if (kind === 'register') {
      init.headers = { 'Content-Type': 'application/json' };
      init.body = JSON.stringify({ name });
    }
    const res = await fetch(btn.dataset.begin, init);
    if (!res.ok) {
      const e = await failure(res);
      e.atBegin = true; // a 400 here is the server refusing the name
      throw e;
    }
    let answer;
    try { answer = await res.json(); } catch (_) { throw new Error(UNEXPECTED); }
    const ok = answer && typeof answer.ceremony === 'string' && answer.ceremony !== '' &&
      answer.options && typeof answer.options === 'object' &&
      answer.options.publicKey && typeof answer.options.publicKey === 'object';
    if (!ok) throw new Error(UNEXPECTED);
    return answer;
  }

  async function run(btn) {
    const kind = btn.dataset.passkey;
    if (!parser[kind]) return;
    if (!supported(kind)) { say(UNSUPPORTED); return; }

    const nameEl = kind === 'register' ? document.querySelector('[data-passkey-name]') : null;
    const name = nameEl ? nameEl.value.trim() : '';
    if (nameEl) {
      const problem = nameProblem(name);
      if (problem) { say(problem); nameEl.focus(); return; }
    }

    btn.disabled = true;
    btn.setAttribute('aria-busy', 'true');
    say(WAITING);
    let done = false;
    let failed = null;
    try {
      const { ceremony, options } = await begin(btn, kind, name);
      const publicKey = window.PublicKeyCredential[parser[kind]](options.publicKey);
      const cred = kind === 'login'
        ? await navigator.credentials.get({ publicKey })
        : await navigator.credentials.create({ publicKey });
      if (!cred) throw new Error('no passkey was returned');
      if (typeof cred.toJSON !== 'function') throw new Error('this browser returned a passkey this page cannot send; update the browser');
      const finished = await fetch(btn.dataset.finish, {
        method: 'POST',
        credentials: 'same-origin',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ ceremony, credential: cred.toJSON() }),
      });
      if (!finished.ok) throw await failure(finished);
      done = true;
      if (kind === 'login') location.assign('/');
      else location.reload();
    } catch (e) {
      say(messageOf(e));
      failed = e;
    } finally {
      if (!done) {
        btn.removeAttribute('aria-busy');
        btn.disabled = false;
        // A disabled button cannot hold the focus. When the server refused the
        // name the field is what to fix; otherwise the button, to try again.
        if (nameEl && failed && failed.atBegin && failed.status === 400) nameEl.focus();
        else btn.focus();
      }
    }
  }

  // submit handles both forms. The event comes from the form itself.
  function submit(ev) {
    const form = ev.target;
    if (!form || typeof form.matches !== 'function' || !form.matches('[data-passkey-form]')) return;
    if (ev.preventDefault) ev.preventDefault(); // never a real navigation
    const btn = form.querySelector('[data-passkey]');
    if (!btn || btn.disabled) return;
    return run(btn);
  }

  // init disables the buttons of a browser that cannot do passkeys, and says why.
  function init() {
    let missing = false;
    document.querySelectorAll('[data-passkey]').forEach((btn) => {
      if (parser[btn.dataset.passkey] && !supported(btn.dataset.passkey)) {
        btn.disabled = true;
        missing = true;
      }
    });
    if (missing) say(UNSUPPORTED);
  }

  if (typeof document.addEventListener === 'function') {
    document.addEventListener('submit', submit);
    init();
  }
  return { submit, init, nameProblem };
})();
