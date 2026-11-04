import assert from 'node:assert/strict';
import { test } from 'node:test';
globalThis.window = globalThis;
await import('../static/js/comments.js');
const C = window.ermiteComments.core;

test('ctrl+enter and cmd+enter send, other keys do not', () => {
  assert.equal(C.submits({ key: 'Enter', ctrlKey: true }), true);
  assert.equal(C.submits({ key: 'Enter', metaKey: true }), true);
  assert.equal(C.submits({ key: 'Enter' }), false);
  assert.equal(C.submits({ key: 'Enter', ctrlKey: true, shiftKey: true }), false);
  assert.equal(C.submits({ key: 'Enter', ctrlKey: true, isComposing: true }), false);
  assert.equal(C.submits({ key: 'b', ctrlKey: true }), false);
});

test('a menu button names the disclosure it opens', () => {
  const d = { id: 'edit-4' };
  const root = { getElementById: (id) => (id === 'edit-4' ? d : null) };
  assert.equal(C.target({ dataset: { cOpen: 'edit-4' } }, root), d);
  assert.equal(C.target({ dataset: {} }, root), null);
  assert.equal(C.target(null, root), null);
});

test('only a resolved root thread folds, until it is opened', () => {
  const root = { dataset: { resolved: 'true' }, parentElement: { closest: () => null }, hasAttribute: () => false };
  assert.equal(C.folds(root), true);
  const opened = { ...root, hasAttribute: (a) => a === 'data-open' };
  assert.equal(C.folds(opened), false);
  const reply = { ...root, parentElement: { closest: () => ({}) } };
  assert.equal(C.folds(reply), false);
  assert.equal(C.folds({ ...root, dataset: { resolved: 'false' } }), false);
});

test('the send button names the verdict', () => {
  assert.equal(C.submitLabel('approve'), 'Approve');
  assert.equal(C.submitLabel('changes_requested'), 'Request changes');
  assert.equal(C.submitLabel(''), 'Send');
});

test('unfolding and folding synchronize the accessible button and note', () => {
  const attrs = new Map();
  const button = { setAttribute: (k, v) => attrs.set(k, v), textContent: 'show thread' };
  let opened = false;
  const note = {
    setAttribute: (k) => { assert.equal(k, 'data-open'); opened = true; },
    removeAttribute: (k) => { assert.equal(k, 'data-open'); opened = false; },
    querySelector: () => button,
  };
  window.ermiteComments.setOpen(note, true);
  assert.equal(opened, true);
  assert.equal(attrs.get('aria-expanded'), 'true');
  assert.equal(button.textContent, 'hide thread');
  window.ermiteComments.setOpen(note, false);
  assert.equal(opened, false);
  assert.equal(attrs.get('aria-expanded'), 'false');
  assert.equal(button.textContent, 'show thread');
});

test('reset and pageshow restore labels from the actual selected verdict', async () => {
  const { readFileSync } = await import('node:fs');
  const { runInNewContext } = await import('node:vm');
  const listeners = {};
  const button = { textContent: 'stale' };
  let selected = 'changes_requested';
  const form = { querySelector: (selector) => selector.startsWith('button') ? button : { value: selected } };
  const document = { getElementById: () => null, querySelectorAll: () => [form] };
  runInNewContext(readFileSync(new URL('../static/js/comments.js', import.meta.url), 'utf8'), {
    window: {}, document, location: { hash: '' }, queueMicrotask,
    addEventListener: (name, handler) => { listeners[name] = handler; },
  });
  listeners.pageshow({ persisted: true });
  assert.equal(button.textContent, 'Request changes');
  selected = 'approve';
  listeners.pageshow({ persisted: false });
  assert.equal(button.textContent, 'Approve');
  listeners.reset({ target: form });
  // Simulate the browser resetting its controls after dispatching reset.
  selected = '';
  assert.equal(button.textContent, 'Approve');
  await Promise.resolve();
  assert.equal(button.textContent, 'Send');
});

test('Quote reply closes its menu after opening and filling the reply', async () => {
  const { readFileSync } = await import('node:fs');
  const { runInNewContext } = await import('node:vm');
  const window = { ermiteLazy: { onSwap() {} }, ermiteComments: { reveal() {} } };
  const document = { readyState: 'loading', addEventListener() {} };
  runInNewContext(readFileSync(new URL('../static/js/mr.js', import.meta.url), 'utf8'), { window, document, Event });
  const menu = { open: true };
  let focused = false;
  const field = { value: '', dispatchEvent() {}, focus() { focused = true; } };
  const reply = { open: false, querySelector: () => field };
  const thread = { querySelector: () => reply };
  const button = {
    dataset: { quote: '> quoted text' },
    closest: (selector) => selector === 'details.menu' ? menu : { closest: () => thread },
  };
  assert.equal(window.ermiteMR.core.quote(button), true);
  assert.equal(reply.open, true);
  assert.equal(field.value, '> quoted text');
  assert.equal(focused, true);
  assert.equal(menu.open, false);
});

test('refresh preserves unfolded resolved notes by ID in comments and both diff layouts', () => {
  function note(id, resolved, open = false) {
    const attrs = new Map([['aria-expanded', 'false']]);
    const button = { textContent: 'show thread', setAttribute: (k, v) => attrs.set(k, v) };
    return {
      id, resolved, open, button, attrs,
      setAttribute(k) { assert.equal(k, 'data-open'); this.open = true; },
      querySelector: () => button,
    };
  }
  const region = (notes) => ({ querySelectorAll: (selector) => notes.filter((n) => n.resolved && (!selector.includes('[data-open]') || n.open)) });
  for (const ids of [['c1', 'c2'], ['c3', 'c3-s']]) {
    const old = region([note(ids[0], true, true), note(ids[1], true), note('deleted', true, true), note('reopened', true, true)]);
    const fresh = [note(ids[0], true), note(ids[1], true), note('new', true), note('reopened', false)];
    window.ermiteComments.preserveOpen(old, region(fresh));
    assert.equal(fresh[0].open, true);
    assert.equal(fresh[0].attrs.get('aria-expanded'), 'true');
    assert.equal(fresh[0].button.textContent, 'hide thread');
    for (const folded of fresh.slice(1)) {
      assert.equal(folded.open, false);
      assert.equal(folded.attrs.get('aria-expanded'), 'false');
      assert.equal(folded.button.textContent, 'show thread');
    }
  }
});

test('htmx note replacement keeps unfolded state and accessible labels in both swap modes', async () => {
  const { readFileSync } = await import('node:fs');
  const { runInNewContext } = await import('node:vm');
  for (const style of ['outerMorph', 'outerHTML']) {
    const listeners = {};
    const button = { textContent: 'show thread', setAttribute(k, v) { this[k] = v; } };
    const incoming = {
      id: 'c42-s', open: false,
      matches: () => true,
      setAttribute(k) { assert.equal(k, 'data-open'); this.open = true; },
      querySelector: () => button,
    };
    const old = { id: 'c42-s', matches: () => true };
    const ctx = {};
    runInNewContext(readFileSync(new URL('../static/js/comments.js', import.meta.url), 'utf8'), {
      window: {}, location: { hash: '' },
      document: { getElementById: () => incoming, querySelector: () => old },
      addEventListener: (name, handler) => { listeners[name] = handler; },
    });
    listeners['htmx:before:swap']({ detail: { ctx, tasks: [{ target: '#c42-s', swapSpec: { style }, fragment: { children: [incoming] } }] } });
    assert.equal(incoming.open, true);
    assert.equal(button['aria-expanded'], 'true');
    assert.equal(button.textContent, 'hide thread');
    button.textContent = 'show thread'; // Morph processing can replace the header children.
    listeners['htmx:after:swap']({ detail: { ctx } });
    assert.equal(button.textContent, 'hide thread');
    listeners['htmx:finally:swap']({ detail: { ctx } });
    button.textContent = 'unchanged';
    listeners['htmx:after:swap']({ detail: { ctx } });
    assert.equal(button.textContent, 'unchanged', 'completed contexts do not retain state');
  }
});

test('edit and delete unfold the resolved root before focusing their forms', () => {
  const saved = globalThis.document;
  for (const action of ['edit', 'delete']) {
    let expanded = false;
    const root = { setAttribute() { expanded = true; }, querySelector: () => null };
    const field = { focus() { assert.equal(expanded, true); }, tagName: 'BUTTON' };
    const disclosure = { closest: () => root, querySelector: () => field, open: false };
    globalThis.document = { getElementById: () => disclosure };
    assert.equal(window.ermiteComments.open({ dataset: { cOpen: action }, closest: () => null }), true);
    assert.equal(disclosure.open, true);
  }
  globalThis.document = saved;
});

test('malformed hash does not interrupt listener registration', async () => {
  const { readFileSync } = await import('node:fs');
  const { runInNewContext } = await import('node:vm');
  const listeners = {};
  runInNewContext(readFileSync(new URL('../static/js/comments.js', import.meta.url), 'utf8'), {
    window: {}, document: { getElementById(id) { assert.equal(id, 'bad%'); return null; } },
    location: { hash: '#bad%' }, addEventListener: (name, fn) => { listeners[name] = fn; },
  });
  for (const event of ['change', 'reset', 'pageshow', 'keydown']) assert.equal(typeof listeners[event], 'function');
});

test('reveal unfolds the actual split note root and synchronizes its button', () => {
  const button = { setAttribute(k, v) { this[k] = v; } };
  let unfolded = false;
  const root = { setAttribute() { unfolded = true; }, querySelector: () => button };
  window.ermiteComments.reveal({ closest: () => root });
  assert.equal(unfolded, true);
  assert.equal(button['aria-expanded'], 'true');
  assert.equal(button.textContent, 'hide thread');
});
