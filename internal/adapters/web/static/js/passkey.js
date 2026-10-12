// passkey.js: sign in with a passkey (/-/login) and add one (/-/passkeys).
//
// One delegated click handler on [data-passkey="login"|"register"]. The button
// carries the two endpoints (data-begin, data-finish). The flow is the one of
// the WebAuthn JSON API: POST begin, hand options.publicKey to the browser
// (parse*OptionsFromJSON, then navigator.credentials.get|create), POST the
// result (toJSON()) to finish. Nothing is inline: the CSP only allows scripts
// from this origin. The core is tested in Node by jstest/passkey.test.mjs.
window.ermitePasskey = (() => {
  const UNSUPPORTED = 'this browser does not support passkeys';

  // The parse function each ceremony needs; absent in a browser that predates
  // the WebAuthn JSON API.
  const parser = { login: 'parseRequestOptionsFromJSON', register: 'parseCreationOptionsFromJSON' };
  const supported = (kind) => {
    const pkc = window.PublicKeyCredential;
    return !!pkc && typeof pkc[parser[kind]] === 'function' && !!navigator.credentials;
  };

  const statusEl = () => document.querySelector('[data-passkey-status]');
  const say = (text) => { const s = statusEl(); if (s) s.textContent = text; };

  // messageOf turns what a step threw into one line for the owner.
  function messageOf(e) {
    if (e && e.name === 'NotAllowedError') return 'cancelled, or it took too long; try again';
    if (e && e.name === 'InvalidStateError') return 'this authenticator already holds a passkey for this site';
    if (e && e.name === 'AbortError') return 'cancelled';
    return (e && e.message) || 'something went wrong; try again';
  }

  // failure reads the error of a refused request: the server's one-line
  // reason when it sent one, the status otherwise.
  async function failure(res) {
    let reason = '';
    try { reason = (await res.json()).error || ''; } catch (_) { /* not JSON */ }
    return new Error(reason || 'the server answered ' + res.status);
  }

  async function run(btn) {
    const kind = btn.dataset.passkey;
    if (!parser[kind]) return;
    if (!supported(kind)) { say(UNSUPPORTED); return; }
    btn.disabled = true;
    say('');
    let done = false;
    try {
      const begun = await fetch(btn.dataset.begin, { method: 'POST', credentials: 'same-origin' });
      if (!begun.ok) throw await failure(begun);
      const { ceremony, options } = await begun.json();
      const publicKey = window.PublicKeyCredential[parser[kind]](options.publicKey);
      const cred = kind === 'login'
        ? await navigator.credentials.get({ publicKey })
        : await navigator.credentials.create({ publicKey });
      if (!cred) throw new Error('no passkey was returned');
      const nameEl = kind === 'register' ? document.querySelector('[data-passkey-name]') : null;
      const finished = await fetch(btn.dataset.finish, {
        method: 'POST',
        credentials: 'same-origin',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ ceremony, name: nameEl ? nameEl.value.trim() : '', credential: cred.toJSON() }),
      });
      if (!finished.ok) throw await failure(finished);
      done = true;
      if (kind === 'login') location.assign('/');
      else location.reload();
    } catch (e) {
      say(messageOf(e));
    } finally {
      if (!done) btn.disabled = false;
    }
  }

  function click(ev) {
    const btn = ev.target && ev.target.closest && ev.target.closest('[data-passkey]');
    if (!btn || btn.disabled) return;
    if (ev.preventDefault) ev.preventDefault();
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
    document.addEventListener('click', click);
    init();
  }
  return { click, init, run };
})();
