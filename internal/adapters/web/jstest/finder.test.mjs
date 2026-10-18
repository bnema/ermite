// Tests of the DOM glue of the file finder and of the ":" palette
// (static/js/files.js) on the small DOM of dom.mjs. Run with
//   node --test 'internal/adapters/web/jstest/*.test.mjs'
import assert from 'node:assert/strict';
import { test } from 'node:test';
import { dom, el, flushFrames, installDom, settle } from './dom.mjs';

installDom();
await import('../static/js/files.js');
const F = window.ermiteFinder;

const PATHS = [
  'README.md',
  'cmd/ermite/main.go',
  'internal/adapters/web/tree.go',
  'internal/adapters/web/tree.templ',
  'docs/web/tree-view.md',
  'src/treehouse.txt',
];
for (let i = 0; i < 1194; i++) PATHS.push('gen/f' + i + '.dat'); // 1,200 in all

// The path list answers when the test says so (once: the finder keeps it).
let release;
const gate = new Promise((r) => { release = r; });
let fetches = 0;
globalThis.fetch = async () => {
  fetches++;
  await gate;
  return { ok: true, json: async () => ({ paths: PATHS, truncated: true }) };
};

// page builds the parts of the Files view and of the palette that files.js
// reaches, around a query field and a tree of three rows (li).
function page() {
  const rows = ['r1', 'r2', 'r3'].map((n) => el('li', { attrs: { 'data-nm': n } }));
  rows[1].setAttribute('data-cursor', '');
  const q = el('input', { id: 'finder-q' });
  const cmdQ = el('input', { id: 'cmd-q' });
  const treeRows = el('ul', { id: 'tree-rows', attrs: { 'data-rows': '' } }, ...rows);
  const finderList = el('ul', { id: 'finder-list' });
  const cmdList = el('ul', { id: 'cmd-list' }, el('li', { attrs: { 'data-k': 'section' }, cls: 'entry' }));
  const finder = el('div', { id: 'finder', hidden: true }, el('p', { id: 'finder-status' }), finderList);
  const treeView = el('div', { id: 'tree-view' }, treeRows);
  dom.body.replaceChildren(q, cmdQ, treeView, finder, cmdList);
  dom.body.setAttribute('data-paths', '/paths.json');
  dom.body.setAttribute('data-blob', '/blob/main');
  dom.clicked = [];
  return { rows, q, cmdQ, treeRows, finderList, cmdList, finder, treeView };
}

const key = (k, extra = {}) => {
  const e = { key: k, isComposing: false, prevented: false, preventDefault() { this.prevented = true; }, stopPropagation() {}, ...extra };
  return e;
};
const status = () => document.getElementById('finder-status').textContent;
const hrefs = (list) => list.children.map((li) => li.querySelector('a').href);

test('Enter before the results opens the top hit once they come', async () => {
  const p = page();
  p.q.value = 'tree';
  F.input(p.q);
  assert.equal(p.finder.hidden, false, 'the results replace the tree at once');
  F.key(p.q, key('Enter'));
  assert.equal(dom.clicked.length, 0, 'nothing to open yet');
  release();
  await settle();
  assert.equal(p.finderList.children.length > 0, true);
  assert.equal(dom.clicked.length, 1);
  assert.equal(dom.clicked[0].href, hrefs(p.finderList)[0]);
  assert.match(dom.clicked[0].href, /^\/blob\/main\/.*tree/);
  assert.equal(fetches, 1);
});

test('the status counts the files and names the real size of a truncated list', async () => {
  const p = page();
  p.q.value = 'tree';
  F.input(p.q);
  await settle();
  assert.equal(status(), '4 files match · only the first 1,200 files are searched');
  p.q.value = 'zzzz';
  F.input(p.q);
  await settle();
  assert.match(status(), /^no file matches · /);
});

test('an Enter is dropped when the field changed since', async () => {
  const p = page();
  p.q.value = 'tree';
  F.input(p.q);
  F.key(p.q, key('Enter')); // pending on "tree"
  p.q.value = 'tre';
  F.input(p.q);
  await settle();
  assert.equal(p.finderList.children.length > 0, true, 'the results of "tre" are shown');
  assert.equal(dom.clicked.length, 0);
});

test('Enter on current results opens the cursor row, whichever way the cursor moved', async () => {
  const p = page();
  p.q.value = 'tree';
  F.input(p.q);
  await settle();
  const items = p.finderList.children;
  assert.equal(items[0].getAttribute('aria-selected'), 'true');
  assert.equal(p.q.getAttribute('aria-activedescendant'), items[0].id);
  // j/k (keys.templ) move data-cursor while the field is blurred.
  items[0].removeAttribute('data-cursor');
  items[2].setAttribute('data-cursor', '');
  const e = key('Enter');
  F.key(p.q, e);
  assert.equal(e.prevented, true);
  assert.equal(items[2].getAttribute('aria-selected'), 'true');
  assert.equal(items[0].getAttribute('aria-selected'), 'false');
  assert.equal(p.q.getAttribute('aria-activedescendant'), items[2].id);
  assert.equal(dom.clicked[0].href, items[2].querySelector('a').href);
  // The arrows move from there.
  F.key(p.q, key('ArrowUp'));
  assert.equal(p.q.getAttribute('aria-activedescendant'), items[1].id);
  assert.ok(items[1].hasAttribute('data-cursor') && !items[2].hasAttribute('data-cursor'));
});

test('an input method composing does not drive the finder', async () => {
  const p = page();
  p.q.value = 'tree';
  F.input(p.q);
  await settle();
  for (const k of ['Enter', 'Escape', 'ArrowDown']) {
    const e = key(k, { isComposing: true });
    F.key(p.q, e);
    assert.equal(e.prevented, false, k);
  }
  assert.equal(p.q.value, 'tree');
  assert.equal(dom.clicked.length, 0);
});

test('Escape clears the field and puts the cursor back on the tree', async () => {
  const cases = [
    { name: 'the row that had it', hide: false, want: 'r2' },
    { name: 'the first visible row when it is hidden', hide: true, want: 'r1' },
  ];
  for (const c of cases) {
    const p = page();
    p.q.value = 'tree';
    F.input(p.q);
    await settle();
    assert.equal(p.treeView.hidden, true);
    assert.ok(!p.rows[1].hasAttribute('data-cursor'), 'the tree gives up its cursor to the results');
    assert.ok(p.finderList.hasAttribute('data-rows'));
    if (c.hide) p.rows[1].hidden = true;
    F.key(p.q, key('Escape'));
    assert.equal(p.q.value, '', c.name);
    assert.equal(p.finder.hidden, true);
    assert.equal(p.treeView.hidden, false);
    assert.equal(p.finderList.children.length, 0);
    const got = p.rows.filter((r) => r.hasAttribute('data-cursor')).map((r) => r.getAttribute('data-nm'));
    assert.deepEqual(got, [c.want], c.name);
    assert.ok(p.treeRows.hasAttribute('data-rows') && !p.finderList.hasAttribute('data-rows'));
  }
});

test('label marks a surrogate pair whole', () => {
  for (const pos of [[2], [3], [2, 3]]) {
    const lbl = F.label('a/😀b', pos);
    const marks = lbl.querySelectorAll('mark');
    assert.deepEqual(marks.map((m) => m.textContent), ['😀'], JSON.stringify(pos));
    assert.equal(lbl.textContent, 'a/😀b');
  }
  const lbl = F.label('a/😀b', [4]);
  assert.deepEqual(lbl.querySelectorAll('mark').map((m) => m.textContent), ['b']);
  assert.equal(F.label('x\u202ey', [0]).querySelectorAll('mark').length, 0, 'no highlight on a marked path');
  assert.equal(F.label('x\u202ey', [0]).textContent, 'x<U+202E>y');
});

test('the palette lists the file hits under its entries', async () => {
  const p = page();
  p.cmdQ.value = 'tree';
  F.palette(p.cmdQ);
  await settle();
  const hits = p.cmdList.children.filter((li) => li.className === 'finder-hit');
  assert.equal(hits.length, 4);
  assert.equal(p.cmdList.children[0].className, 'entry', 'the entries stay first');
  assert.deepEqual(hits.map((li) => li.querySelector('a').href).sort(), [
    '/blob/main/docs/web/tree-view.md',
    '/blob/main/internal/adapters/web/tree.go',
    '/blob/main/internal/adapters/web/tree.templ',
    '/blob/main/src/treehouse.txt',
  ]);
  assert.equal(hits[0].querySelector('a').tabIndex, -1);
  assert.equal(hits[0].querySelector('.k').textContent, 'file');
  assert.equal(p.cmdList.children.filter((li) => li.hasAttribute('data-cursor')).length, 1, 'one cursor');

  // A longer query narrows the hits; an emptied field removes them.
  p.cmdQ.value = 'treeh';
  F.palette(p.cmdQ);
  await settle();
  assert.deepEqual(hrefs({ children: p.cmdList.children.filter((li) => li.className === 'finder-hit') }), ['/blob/main/src/treehouse.txt']);
  p.cmdQ.value = '';
  F.palette(p.cmdQ);
  assert.equal(p.cmdList.children.length, 1);
});

test('Enter typed before the frame of the query runs opens the top hit', async () => {
  const p = page();
  release();
  await F.load(); // the path list is loaded: only the frame is pending
  dom.holdFrames = true;
  try {
    p.q.value = 'maingo';
    F.input(p.q);
    const e = key('Enter');
    F.key(p.q, e);
    assert.equal(e.prevented, true);
    assert.equal(dom.clicked.length, 0, 'nothing to open before the frame');
    flushFrames();
    await settle();
    assert.deepEqual(dom.clicked.map((a) => a.href), ['/blob/main/cmd/ermite/main.go']);
  } finally {
    dom.holdFrames = false;
  }
});

test('the palette keeps one search per frame and drops a search when it closes', async () => {
  const p = page();
  release();
  await F.load();
  dom.holdFrames = true;
  try {
    for (const q of ['t', 'tr', 'tre', 'tree']) {
      p.cmdQ.value = q;
      F.palette(p.cmdQ);
    }
    assert.equal(dom.frames.size, 1, 'the keystrokes of a frame share one search');
    flushFrames();
    await settle();
    assert.equal(p.cmdList.children.filter((li) => li.className === 'finder-hit').length, 4);

    p.cmdQ.value = 'readme';
    F.palette(p.cmdQ);
    F.paletteClose();
    assert.equal(dom.frames.size, 0, 'closing cancels the pending frame');

    // A search past its frame, still awaiting the list, is dropped at close.
    F.palette(p.cmdQ);
    flushFrames();
    F.paletteClose();
    await settle();
    assert.equal(p.cmdList.children.filter((li) => li.className === 'finder-hit').length, 0);
  } finally {
    dom.holdFrames = false;
  }
});

test('the palette drops the results of a query that is stale', async () => {
  const p = page();
  p.cmdQ.value = 'tree';
  F.palette(p.cmdQ);
  p.cmdQ.value = 'readme'; // typed before the search of "tree" finished
  F.palette(p.cmdQ);
  await settle();
  const got = p.cmdList.children.filter((li) => li.className === 'finder-hit').map((li) => li.querySelector('a').href);
  assert.deepEqual(got, ['/blob/main/README.md']);

  // A field changed without a new call: the pending search is dropped.
  p.cmdQ.value = 'main';
  F.palette(p.cmdQ);
  p.cmdQ.value = 'zzzz';
  await settle();
  assert.deepEqual(p.cmdList.children.filter((li) => li.className === 'finder-hit'), []);
});
