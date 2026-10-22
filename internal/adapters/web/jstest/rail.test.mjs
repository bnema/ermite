// Tests of the side panel keys of static/js/ermite.js (ermite.rail, and j / k
// inside the panel) on a tiny hand-rolled DOM. Run with
//   node --test 'internal/adapters/web/jstest/*.test.mjs'
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { test } from 'node:test';
import { el, installDom } from './dom.mjs';

// A page: <main>, and a rail with a hidden input, a closed-details command
// (not laid out), then the controls a key can reach.
const doc = installDom();
const main = el('main', { id: 'main' });
const hidden = el('input', { attrs: { type: 'hidden' } });
const folded = el('a', { href: '/x', attrs: { href: '/x' } });
folded.getClientRects = () => []; // inside a closed <details>
const markRead = el('button', { cls: 'btn' });
const ready = el('button', { cls: 'btn primary' });
const close = el('button', { cls: 'btn danger' });
const rail = el('aside', { cls: 'rail' }, hidden, folded, markRead, ready, close);
document.body.append(main, rail);
let active = document.body;
Object.defineProperty(doc, 'activeElement', { get: () => active });
for (const e of [main, markRead, ready, close, folded]) e.focus = () => { active = e; };
rail.contains = (e) => { for (let n = e; n; n = n.parent) if (n === rail) return true; return false; };

// Only the ermiteRail block of the file is evaluated: the rest needs a page.
const src = readFileSync(new URL('../static/js/ermite.js', import.meta.url), 'utf8');
const start = src.indexOf('  window.ermiteRail = ');
const end = src.indexOf('  })();', start) + '  })();'.length;
new Function(src.slice(start, end))();
const R = window.ermiteRail;
const E = { rail: (into) => (into ? R.enter() : R.leave()), move: (d) => R.step(d) };

test('l focuses the main action of the panel, j / k walk its shown controls, h goes back', () => {
  active = main;
  E.rail(true);
  assert.equal(active, ready, 'the primary button first');
  E.move(1);
  assert.equal(active, close);
  E.move(1);
  assert.equal(active, close, 'stops at the last control');
  E.move(-1);
  E.move(-1);
  assert.equal(active, markRead, 'the folded link is skipped');
  E.move(-1);
  assert.equal(active, markRead, 'stops at the first shown control');
  E.rail(false);
  assert.equal(active, main);
});

test('without a primary action, l takes the first shown control', () => {
  ready.className = 'btn';
  active = main;
  E.rail(true);
  assert.equal(active, markRead);
  ready.className = 'btn primary';
});

test('h outside the panel leaves the focus alone; j / k outside it are not taken', () => {
  active = main;
  E.rail(false);
  assert.equal(active, main);
  assert.equal(R.step(1), false, 'the row cursor keeps j / k');
});

test('j / k cross section and recent-commit table boundaries in document order', () => {
  const controls = ['clone', 'about', 'commit-1', 'commit-2', 'actions', 'outline'].map((id) => el('a', { id, attrs: { href: '#' + id } }));
  for (const e of controls) e.focus = () => { active = e; };
  rail.append(
    el('section', {}, controls[0]),
    el('section', {}, controls[1]),
    el('section', {}, el('table', {}, el('tbody', { attrs: { 'data-rows': '' } }, el('tr', {}, controls[2]), el('tr', {}, controls[3])))),
    el('section', {}, controls[4]),
    el('nav', {}, controls[5]),
  );
  active = close;
  for (const e of controls) { assert.equal(R.step(1), true); assert.equal(active, e); }
  R.step(1);
  assert.equal(active, controls.at(-1));
  for (const e of controls.slice(0, -1).reverse()) { R.step(-1); assert.equal(active, e); }
  R.leave();
  assert.equal(active, main);
  assert.equal(R.step(1), false);
});
