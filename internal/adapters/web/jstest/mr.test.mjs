// Tests of the pure core of the merge request page (static/js/mr.js). Run with
//   node --test 'internal/adapters/web/jstest/*.test.mjs'
import assert from 'node:assert/strict';
import { test } from 'node:test';

globalThis.window = globalThis;
globalThis.document = { readyState: 'complete', querySelector: () => null, querySelectorAll: () => [] };
await import('../static/js/mr.js');
const C = window.ermiteMR.core;

test('windowOf: the start of the window of a file', () => {
  assert.equal(C.windowOf('f1', 25), 0);
  assert.equal(C.windowOf('f25', 25), 0);
  assert.equal(C.windowOf('f26', 25), 25);
  assert.equal(C.windowOf('f84', 25), 75);
  assert.equal(C.windowOf('c3', 25), -1, 'not a file');
  assert.equal(C.windowOf('f0', 25), -1);
});

test('menuKey: modes by number, D toggles the deletion, nothing else', () => {
  assert.deepEqual(C.menuKey('1', 2), { mode: 0 });
  assert.deepEqual(C.menuKey('2', 2), { mode: 1 });
  assert.equal(C.menuKey('3', 2), null, 'no third mode');
  assert.equal(C.menuKey('0', 2), null);
  assert.deepEqual(C.menuKey('D', 2), { toggle: true });
  assert.equal(C.menuKey('d', 2), null, 'd is the split layout');
  assert.equal(C.menuKey('Enter', 2), null, 'the browser submits: the focus is on the button');
  assert.equal(C.menuKey('M', 2), null);
});

test('step: stays in the list', () => {
  assert.equal(C.step(0, 1, 3), 1);
  assert.equal(C.step(2, 1, 3), 2);
  assert.equal(C.step(0, -1, 3), 0);
  assert.equal(C.step(-1, 1, 3), 0);
  assert.equal(C.step(-1, -1, 3), 2);
  assert.equal(C.step(0, 1, 0), -1);
});

test('rangeURL requires the same file, side and MR and sorts endpoints', () => {
 const base = 'https://forge.test/demo/merge-requests/1';
 const a = base+'?on=new:24:dir%2Fa.go#f1';
 const b = base+'?from=25&on=new:10:dir%2Fa.go#f1';
 const u = new URL(C.rangeURL(a,b,base));
 assert.equal(u.searchParams.get('on'),'new:10-24:dir/a.go');
 assert.equal(u.searchParams.get('from'),'25');
 assert.equal(u.hash,'#f1');
 assert.equal(C.rangeURL(a,a,base),null);
 assert.equal(C.rangeURL(a,b.replace('new:','old:'),base),null);
 assert.equal(C.rangeURL(a,b.replace('a.go','b.go'),base),null);
 assert.equal(C.rangeURL(a,b.replace('/1?','/2?'),base),null);
});

test('quote opens the thread reply and prefills and focuses its textarea', () => {
 let focused=false;
 const field={value:'',focus(){focused=true;}};
 const details={open:false,querySelector(){return field;}};
 const thread={querySelector(){return details;}};
 const button={dataset:{quote:'> source\n\n^2 '},closest(){return {closest(){return thread;}};}};
 assert.equal(C.quote(button),true);
 assert.equal(details.open,true);
 assert.equal(field.value,'> source\n\n^2 ');
 assert.equal(focused,true);
});

for (const dataset of [{diff:'split'}, {view:'split'}]) test(`reveal selects the split note (${JSON.stringify(dataset)}), including after lazy loading`, async () => {
 const saved = globalThis.document;
 const savedRAF = globalThis.requestAnimationFrame;
 let scrolled = '';
 const details = {open:false};
 const node = id => ({id, querySelector(){return null;}, closest(){return details;}, scrollIntoView(){scrolled=id;}});
 const nodes = new Map([['c12',node('c12')],['c12-s',node('c12-s')]]);
 const ph = {dataset:{notes:'c12'}};
 globalThis.document = {
   querySelector: sel => sel === '#diffs' ? {dataset} : null,
   querySelectorAll: () => [ph],
   getElementById: id => nodes.get(id),
 };
 globalThis.requestAnimationFrame = fn => fn();
 try {
   assert.equal(window.ermiteMR.reveal('c12'),false);
   assert.equal(scrolled,'c12-s');
   assert.equal(details.open,true);
   nodes.clear(); scrolled=''; details.open=false;
   assert.equal(window.ermiteMR.reveal('c12'),true);
   nodes.set('c12',node('c12')); nodes.set('c12-s',node('c12-s'));
   await Promise.resolve();
   assert.equal(scrolled,'c12-s');
   assert.equal(details.open,true);
 } finally {
   globalThis.document=saved;
   globalThis.requestAnimationFrame=savedRAF;
 }
});
