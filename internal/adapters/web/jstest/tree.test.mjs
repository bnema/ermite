// Tests of the tree rows of static/js/files.js (window.ermiteTree) on a tiny
// hand-rolled DOM. Run with
//   node --test 'internal/adapters/web/jstest/*.test.mjs'
import assert from 'node:assert/strict';
import { test } from 'node:test';

// ---- a DOM just big enough for ermiteTree ----------------------------------

class El {
  constructor(tag) {
    this.tagName = tag.toUpperCase();
    this.attrs = new Map();
    this.dataset = {};
    this.children = [];
    this.parent = null;
    this.hidden = false;
    this.style = { setProperty() {} };
  }
  setAttribute(k, v) { this.attrs.set(k, String(v)); }
  getAttribute(k) { return this.attrs.has(k) ? this.attrs.get(k) : null; }
  hasAttribute(k) { return this.attrs.has(k); }
  removeAttribute(k) { this.attrs.delete(k); }
  append(c) { c.parent = this; this.children.push(c); }
  get nextElementSibling() {
    if (!this.parent) return null;
    return this.parent.children[this.parent.children.indexOf(this) + 1] || null;
  }
  after(...rows) {
    const sib = this.parent.children;
    for (const r of rows) r.parent = this.parent;
    sib.splice(sib.indexOf(this) + 1, 0, ...rows);
  }
  closest(sel) {
    for (let e = this; e; e = e.parent) if (e.matches(sel)) return e;
    return null;
  }
  matches(sel) {
    if (sel === 'tr') return this.tagName === 'TR';
    if (sel === 'a[data-expand]') return this.tagName === 'A' && 'expand' in this.dataset;
    return false;
  }
  querySelector(sel) {
    for (const c of this.children) {
      if (c.matches(sel) || (sel === '.c-kind' && c.cls === 'c-kind')) return c;
      const d = c.querySelector(sel);
      if (d) return d;
    }
    return null;
  }
}

// row makes a table row; a name ending in "/" is a directory with a link.
function row(name, depth) {
  const tr = new El('tr');
  tr.dataset.depth = String(depth);
  tr.name = name;
  const kind = new El('span');
  kind.cls = 'c-kind';
  kind.textContent = '▸';
  tr.append(kind);
  if (name.endsWith('/')) {
    const a = new El('a');
    a.dataset.expand = '/rows/' + name;
    a.href = '/tree/' + name;
    a.setAttribute('aria-expanded', 'false');
    tr.append(a);
  }
  return tr;
}

// A fragment is "x,y/,!z": ! makes a div, not a row.
globalThis.document = {
  createElement(tag) {
    assert.equal(tag, 'template');
    const t = { content: { children: [] } };
    Object.defineProperty(t, 'innerHTML', {
      set(text) {
        t.content.children = text.split(',').filter(Boolean).map((n) => (n[0] === '!' ? new El('div') : row(n, 0)));
      },
    });
    return t;
  },
};
globalThis.location = { href: '' };
globalThis.window = globalThis;
await import('../static/js/files.js');
const T = window.ermiteTree;

// fetches: URL -> resolve function, to answer a request when the test says so.
const pending = new Map();
globalThis.fetch = (url) => new Promise((resolve) => {
  pending.set(url, (text, ok = true) => resolve({ ok, status: ok ? 200 : 500, text: async () => text }));
});
const answer = async (url, text, ok) => {
  pending.get(url)(text, ok);
  await new Promise((r) => setTimeout(r, 0));
};

const click = (tr) => {
  const a = tr.querySelector('a[data-expand]');
  T.click({ target: a, button: 0, preventDefault() {} });
};
const kindOf = (tr) => tr.querySelector('.c-kind').textContent;

function table(...names) {
  const body = new El('tbody');
  for (const n of names) body.append(row(n, 0));
  return body;
}
const byName = (body, n) => body.children.find((r) => r.name === n);
const shown = (body) => body.children.filter((r) => !r.hidden).map((r) => r.name);

test('a directory opens in place and closes again without a request', async () => {
  const body = table('P/', 'f');
  click(body.children[0]);
  assert.equal(body.children[0].getAttribute('aria-busy'), 'true');
  await answer('/rows/P/', 'a,b');
  assert.deepEqual(shown(body), ['P/', 'a', 'b', 'f']);
  assert.equal(byName(body, 'a').dataset.depth, '1');
  assert.equal(kindOf(body.children[0]), '▾');
  click(body.children[0]);
  assert.deepEqual(shown(body), ['P/', 'f']);
  pending.clear();
  click(body.children[0]);
  assert.deepEqual(shown(body), ['P/', 'a', 'b', 'f']);
  assert.equal(pending.size, 0, 'no new request');
});

test('TS-001/TS-002: a directory that loads under a closed ancestor comes in hidden', async () => {
  pending.clear();
  const body = table('P/', 'f');
  const P = body.children[0];
  click(P);
  await answer('/rows/P/', 'C/,x');
  const C = byName(body, 'C/');
  click(C); // C starts loading
  assert.equal(C.getAttribute('aria-busy'), 'true');
  C.setAttribute('data-cursor', '');
  click(P); // P closes meanwhile
  assert.deepEqual(shown(body), ['P/', 'f']);
  assert.ok(P.hasAttribute('data-cursor'), 'the cursor of a hidden row moves to the closed row');
  assert.ok(!C.hasAttribute('data-cursor'));

  await answer('/rows/C/', 'c1,c2');
  assert.deepEqual(shown(body), ['P/', 'f'], 'no visible row under a closed directory');
  assert.deepEqual(body.children.map((r) => r.name), ['P/', 'C/', 'c1', 'c2', 'x', 'f']);
  assert.equal(byName(body, 'c1').hidden, true);
  assert.equal(C.getAttribute('aria-busy'), null);
  assert.equal(C.querySelector('a[data-expand]').getAttribute('aria-expanded'), 'true', 'C is open and loaded');
  assert.equal(kindOf(C), '▾');

  pending.clear();
  click(P); // P opens: C's rows show with it
  assert.deepEqual(shown(body), ['P/', 'C/', 'c1', 'c2', 'x', 'f']);
  assert.equal(pending.size, 0, 'C was not fetched again');
});

test('a closed directory under a closed ancestor stays closed when the ancestor opens', async () => {
  pending.clear();
  const body = table('P/');
  click(body.children[0]);
  await answer('/rows/P/', 'C/');
  const C = byName(body, 'C/');
  click(C);
  await answer('/rows/C/', 'c1');
  click(C); // C closed
  click(body.children[0]);
  click(body.children[0]);
  assert.deepEqual(shown(body), ['P/', 'C/']);
});

test('TS-006: only rows are kept; no row falls back to navigating', async () => {
  pending.clear();
  const body = table('P/');
  click(body.children[0]);
  await answer('/rows/P/', '!junk,a');
  assert.deepEqual(body.children.map((r) => r.name), ['P/', 'a']);

  const other = table('Q/');
  globalThis.location.href = '';
  click(other.children[0]);
  await answer('/rows/Q/', '!junk');
  assert.equal(globalThis.location.href, '/tree/Q/');
  assert.deepEqual(other.children.map((r) => r.name), ['Q/']);
});
