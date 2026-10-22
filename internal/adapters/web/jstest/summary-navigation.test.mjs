// SPDX-License-Identifier: AGPL-3.0-or-later

import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { test } from 'node:test';
import { runInNewContext } from 'node:vm';
import { el, installDom } from './dom.mjs';

const source = readFileSync(new URL('../static/js/ermite.js', import.meta.url), 'utf8');

test('main navigation ignores rail rows, but still drives main tables', () => {
  const doc = installDom();
  const main = el('main', { id: 'main' });
  const recent = el('tr');
  const rail = el('aside', { cls: 'rail' }, el('tbody', { attrs: { 'data-rows': '' } }, recent));
  doc.body.append(main, rail);
  const scrolls = [];
  main.scrollBy = (_, y) => scrolls.push(['main-by', y]);
  main.scrollTo = (_, y) => scrolls.push(['main-to', y]);
  const context = {
    window: { ermiteRail: { step: () => false } },
    $: (s) => doc.querySelector(s),
    $$: (s) => {
      const child = s.startsWith('[data-rows] > ') ? s.slice('[data-rows] > '.length) : null;
      if (child) return [...doc.querySelectorAll('[data-rows]')].flatMap((table) => table.children.filter((row) => row.matches(child)));
      return [...doc.querySelectorAll(s)];
    },
    body: () => doc.body,
    flash() {}, setTimeout() {},
    scrollBy: (_, y) => scrolls.push(['page-by', y]),
    scrollTo: (_, y) => scrolls.push(['page-to', y]),
  };
  runInNewContext(source.slice(source.indexOf('  const visibleRows ='), source.indexOf('  window.ermite = ermite;')) + '\nwindow.ermite = ermite;', context);
  const E = context.window.ermite;
  E.move(1); E.move(-1); E.edge(false); E.edge(true);
  assert.deepEqual(scrolls, [['page-by', 180], ['main-by', 180], ['page-by', -180], ['main-by', -180], ['page-to', 0], ['main-to', 0], ['page-to', 1e9], ['main-to', 1e9]]);
  assert.equal(recent.hasAttribute('data-cursor'), false);
  const first = el('tr');
  const last = el('tr');
  main.append(el('tbody', { attrs: { 'data-rows': '' } }, first, last));
  E.move(1);
  assert.equal(first.hasAttribute('data-cursor'), true);
  E.edge(true);
  assert.equal(last.hasAttribute('data-cursor'), true);
  assert.equal(first.hasAttribute('data-cursor'), false);
  assert.equal(recent.hasAttribute('data-cursor'), false);
});
