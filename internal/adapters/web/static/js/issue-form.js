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
    // state is what the button and the status line show.
    //   {solved, now, minAt, sending, unsupported, failed, overs}
    state({ solved, now, minAt, sending, unsupported, failed, overs }) {
      if (unsupported) return { disabled: true, status: 'Your browser cannot run the check.' };
      if (failed) return { disabled: true, status: 'The check failed. Reload the page.' };
      if (sending) return { disabled: true, status: 'Sending…' };
      if (!solved) return { disabled: true, status: 'Checking your browser… (this takes a few seconds)' };
      if (now < minAt) return { disabled: true, status: 'Almost ready…' };
      if (overs) return { disabled: true, status: 'A field is too long.' };
      return { disabled: false, status: 'Ready.' };
    },
  };

  function init(form) {
    const $ = (sel) => form.querySelector(sel);
    const button = $('button[type="submit"]');
    const status = $('[data-status]');
    const solution = $('input[name="solution"]');
    const minAt = Number(form.dataset.minSubmitAt) || 0;
    const ctx = { solved: false, sending: false, unsupported: false, failed: false, overs: false };
    let timer = null;

    function render() {
      const s = core.state({ ...ctx, now: Date.now(), minAt });
      button.disabled = s.disabled;
      if (status) status.textContent = s.status;
      clearTimeout(timer);
      if (ctx.solved && Date.now() < minAt && !ctx.sending) timer = setTimeout(render, minAt - Date.now() + 20);
    }

    for (const out of form.querySelectorAll('output[data-for]')) {
      const field = form.querySelector('#' + out.dataset.for);
      const max = Number(out.dataset.max);
      const update = () => {
        const n = core.length(field.value);
        out.textContent = n + ' / ' + max;
        out.className = core.counterClass(n, max);
        ctx.overs = [...form.querySelectorAll('output[data-for]')].some((o) => o.className === 'over');
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
