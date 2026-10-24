// SPDX-License-Identifier: AGPL-3.0-or-later
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import vm from 'node:vm';

function setup() {
  let document, observer;
  class Element {
    constructor(tag) { this.tagName = tag.toUpperCase(); this.children = []; this.attrs = {}; this.events = {}; this.id = ''; this.inert = false; this.isConnected = true; this.tabIndex = 0; }
    append(...els) { for (const el of els) { el.parentNode = el.parentElement = this; this.children.push(el); } }
    prepend(el) { this.append(el); this.children.unshift(this.children.pop()); }
    insertBefore(el, ref) { this.append(el); this.children.pop(); this.children.splice(this.children.indexOf(ref), 0, el); }
    replaceChildren() { this.children.forEach(el => el.isConnected = false); this.children = []; }
    setAttribute(k, v) { this.attrs[k] = v; }
    getAttribute(k) { return this.attrs[k]; }
    removeAttribute(k) { delete this.attrs[k]; }
    toggleAttribute(k, v) { if (v) this.attrs[k] = ''; else delete this.attrs[k]; }
    addEventListener(k, fn) { this.events[k] = fn; }
    focus() { document.activeElement = this; }
    querySelector(s) { return this.children.find(el => el.tagName === s.toUpperCase()); }
    contains(el) { return this.children.some(child => child === el || child.contains(el)); }
    querySelectorAll() { return []; }
    cloneNode() { return this; }
  }
  const body = new Element('body');
  body.classList = { add() {}, remove() {} };
  const work = new Element('div'), main = new Element('main'), rail = new Element('aside');
  body.append(work); work.append(main, rail);
  const section = (id, label) => { const s = new Element('section'), h = new Element('h2'); s.id = id; h.textContent = label; s.append(h); return s; };
  rail.append(section('about', 'About'), section('files', 'Files 12'));
  document = { body, createElement: t => new Element(t), querySelectorAll: () => [], addEventListener(k, fn) { this[k] = fn; } };
  const media = { matches: true, addEventListener(k, fn) { this.change = fn; } };
  const window = {};
  vm.runInNewContext(readFileSync(new URL('../static/js/rail.js', import.meta.url), 'utf8'), {
    window, document, matchMedia: () => media,
    MutationObserver: class { constructor(fn) { observer = fn; } observe() {} },
  });
  const api = window.ermiteRail.mount(rail);
  return { api, rail, bar: work.children[0], header: rail.children[0], main, document, media, section, refresh: () => observer(), backdrop: body.children.at(-1) };
}

test('builds labels and controls without moving sections; opens, switches and closes', () => {
  const { rail, bar, header, main, document } = setup();
  assert.deepEqual(bar.children.map(b => b.textContent), ['About', 'Files 12']);
  assert.equal(bar.children[1].getAttribute('aria-controls'), 'files');
  bar.children[0].events.click();
  assert.equal(rail.getAttribute('role'), 'dialog');
  assert.equal(main.inert, true);
  assert.equal(document.activeElement, header.children[0].children[0]);
  header.children[0].children[1].events.click();
  assert.equal(rail.children[2].getAttribute('data-rail-selected'), '');
  header.children[0].children[1].events.click();
  assert.equal(rail.getAttribute('data-drawer-open'), undefined);
  assert.equal(main.inert, false);
  assert.equal(document.activeElement, bar.children[0]);
});

test('close button, backdrop, Escape and breakpoint close the drawer', () => {
  const s = setup();
  for (const close of [() => s.header.children[1].events.click(), () => s.backdrop.events.click(),
    () => s.document.keydown({ key: 'Escape', preventDefault() {}, stopImmediatePropagation() {} }),
    () => { s.media.matches = false; s.media.change(); }]) {
    s.bar.children[0].events.click(); close();
    assert.equal(s.rail.getAttribute('data-drawer-open'), undefined);
    assert.equal(s.main.inert, false);
  }
});

test('live section changes rebuild labels and close a removed selection', () => {
  const s = setup();
  s.bar.children[1].events.click();
  s.rail.children.pop();
  s.rail.append(s.section('ci', 'CI'));
  s.refresh();
  assert.deepEqual(s.bar.children.map(b => b.textContent), ['About', 'CI']);
  assert.equal(s.rail.getAttribute('data-drawer-open'), undefined);
});

test('MR file navigation closes without preventing the original link', () => {
  const s = setup(); s.bar.children[1].events.click();
  s.rail.events.click({ target: { closest: selector => selector === '.r-files a[href]' ? {} : null } });
  assert.equal(s.rail.getAttribute('data-drawer-open'), undefined);
});
