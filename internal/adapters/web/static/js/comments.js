// comments.js: the notes of a merge request or an issue (window.ermiteComments).
//
// The server renders each note with its actions working without scripts:
// Edit and Delete are disclosures under the body, Reply is a disclosure under
// the thread. With scripts:
//  - the ⋯ menu of a note opens the Edit or Delete disclosure it names
//    (data-c-open) and focuses its first field; Cancel (data-c-close) folds
//    it again and restores the text the note had;
//  - ctrl+enter (cmd+enter) in a comment field sends its form;
//  - a resolved root thread folds to its header line; its show/hide thread button
//    toggles it (data-open on the note).
window.ermiteComments = (() => {
  const core = {
    // target is the disclosure a menu button opens, or null.
    target(button, root = document) {
      const id = button?.dataset?.cOpen;
      return id ? root.getElementById(id) : null;
    },
    // submits says whether a keydown sends the form of its field.
    submits(e) {
      return e.key === 'Enter' && Boolean(e.ctrlKey || e.metaKey) && !e.altKey && !e.shiftKey && !e.isComposing;
    },
    // submitLabel is the label of the send button for a verdict.
    submitLabel(verdict) {
      return { approve: 'Approve', changes_requested: 'Request changes' }[verdict] || 'Send';
    },
    // folds says whether a note is a resolved root thread shown folded.
    folds(note) {
      return note?.dataset?.resolved === 'true' && !note.parentElement?.closest?.('.cmt') && !note.hasAttribute('data-open');
    },
  };

  function open(button) {
    const d = core.target(button);
    if (!d) return false;
    const menu = button.closest('details.menu');
    if (menu) menu.open = false;
    reveal(d);
    d.open = true;
    const field = d.querySelector('textarea, button[type="submit"]');
    field?.focus();
    if (field?.tagName === 'TEXTAREA') field.setSelectionRange(field.value.length, field.value.length);
    return true;
  }

  function close(button) {
    const d = button.closest('details');
    if (!d) return false;
    const field = d.querySelector('textarea');
    if (field) {
      field.value = field.defaultValue;
      field.dispatchEvent(new Event('input', { bubbles: true }));
    }
    d.open = false;
    d.closest('.cmt')?.querySelector('.c-menu > summary')?.focus();
    return true;
  }

  function setOpen(note, expanded) {
    if (expanded) note.setAttribute('data-open', '');
    else note.removeAttribute('data-open');
    const button = note.querySelector(':scope > .c-head .c-unfold');
    if (button) {
      button.setAttribute('aria-expanded', String(expanded));
      button.textContent = expanded ? 'hide thread' : 'show thread';
    }
  }

  // Preserve only surviving resolved notes, independently for each layout ID.
  function preserveOpen(old, fresh) {
    const opened = new Set([...old.querySelectorAll('.cmt[data-resolved="true"][data-open]')]
      .map((note) => note.id).filter(Boolean));
    for (const note of fresh.querySelectorAll('.cmt[data-resolved="true"]')) {
      if (opened.has(note.id)) setOpen(note, true);
    }
  }

  function fragmentID(hash) {
    const raw = hash.slice(1);
    try { return decodeURIComponent(raw); } catch { return raw; }
  }

  // Reveal the resolved root containing the actual selected layout/note.
  function reveal(target) {
    const root = target?.closest?.('.cmt[data-resolved="true"]:not(.cmt .cmt)');
    if (root) setOpen(root, true);
  }

  function syncLabel(form) {
    const button = form.querySelector('button[type="submit"][data-label]');
    if (button) {
      const verdict = form.querySelector('.f-verdict input[name="verdict"]:checked');
      button.textContent = core.submitLabel(verdict?.value);
    }
  }

  function start() {
    // htmx 4 includes main and OOB replacement tasks in the before-swap event.
    // Key by request context so concurrent note requests cannot share state.
    const unfoldedSwaps = new WeakMap();
    addEventListener('htmx:before:swap', (e) => {
      const { ctx, tasks = [] } = e.detail;
      const ids = [];
      for (const task of tasks) {
        const target = typeof task.target === 'string' ? document.querySelector(task.target) : task.target;
        const style = typeof task.swapSpec === 'string' ? task.swapSpec.split(/\s+/)[0] : task.swapSpec?.style;
        if (!['outerMorph', 'outerHTML'].includes(style) || !target?.matches?.('.cmt[data-open]') || !target.id) continue;
        const incoming = [...task.fragment.children].find((node) => node.id === target.id && node.matches('.cmt[data-resolved="true"]'));
        if (!incoming) continue;
        setOpen(incoming, true);
        ids.push(target.id);
      }
      if (ids.length) unfoldedSwaps.set(ctx, ids);
    });
    addEventListener('htmx:after:swap', (e) => {
      for (const id of unfoldedSwaps.get(e.detail.ctx) || []) {
        const note = document.getElementById(id);
        if (note?.matches('.cmt[data-resolved="true"]')) setOpen(note, true);
      }
      unfoldedSwaps.delete(e.detail.ctx);
    });
    addEventListener('htmx:finally:swap', (e) => unfoldedSwaps.delete(e.detail.ctx));
    addEventListener('click', (e) => {
      const opener = e.target.closest?.('[data-c-open]');
      if (opener) { e.preventDefault(); open(opener); return; }
      const closer = e.target.closest?.('[data-c-close]');
      if (closer) { e.preventDefault(); close(closer); return; }
      const unfold = e.target.closest?.('.c-unfold');
      if (unfold) {
        const note = unfold.closest('.cmt');
        setOpen(note, !note.hasAttribute('data-open'));
      }
    });
    // A link to a note inside a folded thread unfolds it.
    const revealHash = () => {
      const id = fragmentID(location.hash);
      reveal(id && document.getElementById(id));
    };
    addEventListener('hashchange', revealHash);
    revealHash();
    // The send button says what it sends: a comment, an approval or a
    // request for changes.
    addEventListener('change', (e) => {
      const radio = e.target.closest?.('.f-verdict input[name="verdict"]');
      if (radio?.form) syncLabel(radio.form);
    });
    // Reset events fire before controls return to their defaults.
    addEventListener('reset', (e) => {
      queueMicrotask(() => syncLabel(e.target));
    });
    addEventListener('pageshow', () => {
      document.querySelectorAll('form').forEach(syncLabel);
    });
    addEventListener('keydown', (e) => {
      if (!core.submits(e)) return;
      const field = e.target.closest?.('form textarea');
      const form = field?.form;
      if (!form || !form.closest('.cform, .cmt')) return;
      e.preventDefault();
      form.requestSubmit();
    });
  }

  if (typeof addEventListener === 'function' && typeof document !== 'undefined' && document.getElementById) start();
  return { core, open, close, setOpen, preserveOpen, reveal, fragmentID };
})();
