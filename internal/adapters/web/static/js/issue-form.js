// issue-form.js: the public issue form (/<repo>/issues/new for a visitor).
// The server sends a proof of work with the form; this script starts the
// worker (pow-worker.js) when the page loads, so it solves while the visitor
// writes, shows the length counters, and enables the button once the solution
// is found and the minimum fill time has passed. Without scripts the form
// stays hidden (the page says why).
//
// window.ermiteIssueForm.core is pure (jstest/issue-form.test.mjs).
window.ermiteIssueForm = (() => {
  const core = {
    // counterClass is the class of a length counter: "over" above max, "warn"
    // from 90 % of it, else none.
    counterClass(n, max) {
      if (n > max) return 'over';
      if (n * 10 >= max * 9) return 'warn';
      return '';
    },
    // length counts code points, like the server.
    length(s) { return [...String(s)].length; },
    // serverLength is the length the server will see for field ('title',
    // 'body' or 'author'): NFC, control and bidi characters removed (body
    // keeps \n and \t), title and author trimmed (domain.cleanText).
    serverLength(field, value) {
      const multiline = field === 'body';
      let s = String(value).normalize('NFC');
      if (multiline) s = s.replace(/\r\n/g, '\n');
      let out = '';
      for (const ch of s) {
        const c = ch.codePointAt(0);
        if (multiline && (c === 0x0a || c === 0x09)) { out += ch; continue; }
        if (c < 0x20 || (c >= 0x7f && c <= 0x9f) || c === 0x2028 || c === 0x2029) continue;
        if (c === 0x200e || c === 0x200f || c === 0x061c ||
            (c >= 0x202a && c <= 0x202e) || (c >= 0x2066 && c <= 0x2069)) continue;
        out += ch;
      }
      if (!multiline) out = out.trim();
      return [...out].length;
    },
    draftKey(path) { return 'ermite:issue-draft:' + path; },
    // saveDraft keeps the text fields in storage (never the solution).
    saveDraft(storage, key, draft) {
      try { storage.setItem(key, JSON.stringify({ title: draft.title, body: draft.body, author: draft.author })); } catch (_) { /* storage unavailable */ }
    },
    // restoreDraft returns the saved draft, or null, and clears it.
    restoreDraft(storage, key) {
      try {
        const raw = storage.getItem(key);
        if (raw === null || raw === undefined) return null;
        storage.removeItem(key);
        const d = JSON.parse(raw);
        if (!d || typeof d !== 'object') return null;
        return { title: String(d.title || ''), body: String(d.body || ''), author: String(d.author || '') };
      } catch (_) { return null; }
    },
    // onPageShow decides what to do on pageshow: a page restored from the
    // back/forward cache has a challenge that may be spent or expired.
    onPageShow(persisted) { return persisted === true ? 'reload' : 'none'; },
    // state is what the button and the status line show.
    //   {solved, now, minAt, sending, unsupported, failed, overs, empty}
    state({ solved, now, minAt, sending, unsupported, failed, overs, empty }) {
      if (unsupported) return { disabled: true, status: 'Your browser cannot run the check.' };
      if (failed) return { disabled: true, status: 'The check failed. Reload the page.' };
      if (sending) return { disabled: true, status: 'Sending…' };
      if (!solved) return { disabled: true, status: 'Checking your browser… (this takes a few seconds)' };
      if (now < minAt) return { disabled: true, status: 'Almost ready…' };
      if (overs) return { disabled: true, status: 'A field is too long.' };
      if (empty) return { disabled: true, status: 'Enter a title.' };
      return { disabled: false, status: 'Ready.' };
    },
  };

  function init(form) {
    const $ = (sel) => form.querySelector(sel);
    const button = $('button[type="submit"]');
    const status = $('[data-status]');
    const solution = $('input[name="solution"]');
    const minAt = Number(form.dataset.minSubmitAt) || 0;
    const ctx = { solved: false, sending: false, unsupported: false, failed: false, overs: false, empty: true };
    let timer = null;

    function render() {
      const s = core.state({ ...ctx, now: Date.now(), minAt });
      button.disabled = s.disabled;
      if (status) status.textContent = s.status;
      clearTimeout(timer);
      if (ctx.solved && Date.now() < minAt && !ctx.sending) timer = setTimeout(render, minAt - Date.now() + 20);
    }

    const storage = (() => { try { return window.sessionStorage; } catch (_) { return null; } })();
    const draftKey = core.draftKey(window.location ? window.location.pathname : '');
    const fields = { title: $('[name="title"]'), body: $('[name="body"]'), author: $('[name="author"]') };
    if (storage) {
      const d = core.restoreDraft(storage, draftKey);
      if (d) for (const k of Object.keys(fields)) if (fields[k]) fields[k].value = d[k];
    }
    window.addEventListener('pageshow', (e) => {
      if (core.onPageShow(e.persisted) !== 'reload') return;
      // The old challenge may be spent or expired: never reuse its solution.
      if (button) button.disabled = true;
      if (storage) {
        core.saveDraft(storage, draftKey, {
          title: fields.title ? fields.title.value : '',
          body: fields.body ? fields.body.value : '',
          author: fields.author ? fields.author.value : '',
        });
      }
      window.location.reload();
    });

    const outs = [...form.querySelectorAll('output[data-for]')];
    for (const out of outs) {
      const field = form.querySelector('#' + out.dataset.for);
      const name = field.name;
      const max = Number(out.dataset.max);
      const update = () => {
        const n = core.serverLength(name, field.value);
        out.textContent = n + ' / ' + max;
        out.className = core.counterClass(n, max);
        ctx.overs = outs.some((o) => o.className === 'over');
        if (name === 'title') ctx.empty = n === 0;
        render();
      };
      field.addEventListener('input', update);
      update();
    }

    form.addEventListener('submit', (e) => {
      if (ctx.sending || !ctx.solved || Date.now() < minAt) { e.preventDefault(); return; }
      ctx.sending = true;
      render();
    });

    form.hidden = false;
    form.removeAttribute('hidden');

    if (!window.Worker || !window.crypto || !window.crypto.subtle || !form.dataset.worker) {
      ctx.unsupported = true;
      render();
      return;
    }
    render();
    let worker;
    try {
      worker = new window.Worker(form.dataset.worker);
    } catch (_) {
      ctx.unsupported = true;
      render();
      return;
    }
    worker.onmessage = (e) => {
      const m = e.data || {};
      if (m.solution !== undefined) {
        solution.value = m.solution;
        ctx.solved = true;
        worker.terminate();
      } else if (m.error === 'nocrypto') {
        ctx.unsupported = true;
      } else if (m.error) {
        ctx.failed = true;
      } else {
        return;
      }
      render();
    };
    worker.onerror = () => { ctx.failed = true; render(); };
    worker.postMessage({ nonce: form.dataset.nonce, difficulty: Number(form.dataset.difficulty) });
  }

  return { core, init };
})();

document.addEventListener('DOMContentLoaded', () => {
  const form = document.querySelector('form[data-issue-form]');
  if (form) window.ermiteIssueForm.init(form);
});
