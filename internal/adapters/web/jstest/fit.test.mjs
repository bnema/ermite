// Tests of the one-line chip lists of static/js/ermite.js (window.ermiteFit).
// Only that block is evaluated, on a list whose layout is computed here:
// chips flow left to right in a row of a given width and wrap, as flexbox
// does. Run with
//   node --test 'internal/adapters/web/jstest/*.test.mjs'
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { test } from 'node:test';

const src = readFileSync(new URL('../static/js/ermite.js', import.meta.url), 'utf8');
const start = src.indexOf('  window.ermiteFit = ');
const block = src.slice(start, src.indexOf('})();', start) + 5);

const GAP = 6;
const CHAR = 8;
const PAD = 20;

// A row of chips of the given width (ul.width, which a test may change); a
// chip is as wide as its text. Only what the block uses is there.
function list(width, names) {
  const attrs = {};
  const ul = {
    width,
    children: [],
    hasAttribute: (k) => k in attrs,
    setAttribute: (k, v) => { attrs[k] = v; },
    querySelector(sel) {
      assert.equal(sel, ':scope > li.more');
      return this.children.find((c) => c.className === 'more') || null;
    },
    append(li) { li.parent = this; this.children.push(li); },
  };
  const layout = () => {
    let x = 0, y = 0;
    for (const c of ul.children) {
      if (c.hidden) { c.top = 0; continue; }
      const w = c.textContent.length * CHAR + PAD;
      if (!('data-open' in attrs) && x > 0 && x + w > ul.width) { x = 0; y += 28; }
      c.top = y;
      x += w + GAP;
    }
  };
  const el = (tag, text = '') => {
    const e = {
      tag, className: '', hidden: false, title: '', attrs: {}, kids: [], parent: null, focused: false,
      get textContent() { return this.kids.length ? this.kids.map((k) => k.textContent).join('') : text; },
      set textContent(v) { text = v; },
      get offsetTop() { layout(); return this.top; },
      setAttribute(k, v) { this.attrs[k] = v; },
      get firstElementChild() { return this.kids[0] || null; },
      append(k) { k.parent = this; this.kids.push(k); },
      remove() { ul.children.splice(ul.children.indexOf(this), 1); },
      focus() { this.focused = true; globalThis.document.activeElement = this; },
      closest(sel) {
        if (sel === '[data-fit]') return ul;
        if (sel === 'li') return tag === 'li' ? this : this.parent;
        return null;
      },
    };
    return e;
  };
  for (const n of names) ul.append(el('li', n));
  globalThis.document = { createElement: (tag) => el(tag), activeElement: null };
  return ul;
}

function load() {
  globalThis.window = {};
  new Function(block)();
  return globalThis.window.ermiteFit;
}

const shown = (ul) => ul.children.filter((c) => !c.hidden).map((c) => c.textContent);
const moreBtn = (ul) => ul.children.find((c) => c.className === 'more')?.kids[0];

test('a list that fits is left alone', () => {
  const fit = load();
  const ul = list(400, ['go', 'git', 'forge']);
  fit.fit(ul);
  assert.deepEqual(shown(ul), ['go', 'git', 'forge']);
});

test('the chips past the line give way to a … button that names them', () => {
  const fit = load();
  // each chip is 6 chars: 68px, 74px with the gap. In 380px, 5 fit (364px)
  // but not with the 28px … chip after them: the 5th makes way.
  const ul = list(380, ['aaaaaa', 'bbbbbb', 'cccccc', 'dddddd', 'eeeeee', 'ffffff', 'gggggg']);
  fit.fit(ul);
  assert.deepEqual(shown(ul), ['aaaaaa', 'bbbbbb', 'cccccc', 'dddddd', '…']);
  const btn = moreBtn(ul);
  assert.equal(btn.tag, 'button');
  assert.equal(btn.type, 'button');
  assert.equal(btn.title, 'eeeeee, ffffff, gggggg');
  assert.equal(btn.attrs['aria-label'], 'show 3 more: eeeeee, ffffff, gggggg');
  for (const c of ul.children.filter((c) => !c.hidden)) assert.equal(c.offsetTop, 0, 'all on the first line');

  // In 400px, the 5th chip and the … chip both fit.
  const roomy = list(400, ['aaaaaa', 'bbbbbb', 'cccccc', 'dddddd', 'eeeeee', 'ffffff']);
  fit.fit(roomy);
  assert.deepEqual(shown(roomy), ['aaaaaa', 'bbbbbb', 'cccccc', 'dddddd', 'eeeeee', '…']);
});

test('fitting again after a resize shows what fits now, with one … button', () => {
  const fit = load();
  const names = ['aaaaaa', 'bbbbbb', 'cccccc', 'dddddd', 'eeeeee', 'ffffff', 'gggggg'];
  const ul = list(380, names);
  fit.fit(ul);
  fit.fit(ul);
  assert.equal(ul.children.filter((c) => c.className === 'more').length, 1, 'idempotent');

  ul.width = 2000; // wider: everything shows, no … button
  fit.fit(ul);
  assert.deepEqual(shown(ul), names);
  assert.equal(moreBtn(ul), undefined);

  ul.width = 250; // narrower again: fewer chips, the label follows
  fit.fit(ul);
  assert.deepEqual(shown(ul), ['aaaaaa', 'bbbbbb', 'cccccc', '…']);
  assert.equal(moreBtn(ul).attrs['aria-label'], 'show 4 more: dddddd, eeeeee, ffffff, gggggg');
});

test('a refit keeps the … button, and its focus', () => {
  const fit = load();
  const names = ['aaaaaa', 'bbbbbb', 'cccccc', 'dddddd', 'eeeeee', 'ffffff', 'gggggg'];
  const ul = list(380, names);
  fit.fit(ul);
  const btn = moreBtn(ul);
  btn.focus();
  ul.width = 250;
  fit.fit(ul);
  assert.equal(moreBtn(ul), btn, 'the same button');
  assert.equal(document.activeElement, btn);
  ul.width = 2000; // all fit: the focus goes to the last chip
  fit.fit(ul);
  assert.equal(document.activeElement.textContent, 'gggggg');
});

test('pressing … shows every chip and moves the focus to the first one it showed', () => {
  const fit = load();
  const names = ['aaaaaa', 'bbbbbb', 'cccccc', 'dddddd', 'eeeeee', 'ffffff', 'gggggg'];
  const ul = list(380, names);
  fit.fit(ul);
  fit.open(moreBtn(ul));
  assert.deepEqual(shown(ul), names, 'all shown, no … left');
  const e = ul.children.find((c) => c.textContent === 'eeeeee');
  assert.ok(e.focused);
  assert.equal(e.attrs.tabindex, '-1');
  fit.fit(ul); // a resize keeps it open
  assert.deepEqual(shown(ul), names);
});
