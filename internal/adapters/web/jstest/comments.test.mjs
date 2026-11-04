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
  assert.equal(C.submitLabel(''), 'Comment');
});
