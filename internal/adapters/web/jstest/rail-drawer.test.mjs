// SPDX-License-Identifier: AGPL-3.0-or-later
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import vm from 'node:vm';

function setup({ wide = false, stored = null } = {}) {
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
  const header = new Element('header');
  body.append(header);
  body.append(work); work.append(main, rail);
  const section = (id, label) => { const s = new Element('section'), h = new Element('h2'); s.id = id; h.textContent = label; s.append(h); return s; };
  rail.setAttribute('aria-label', 'Context');
  rail.setAttribute('role', 'complementary');
  rail.append(section('about', 'About'), section('files', 'Files 12'));
  const classes = new Set();
  const values = new Map(stored ? [['ermite.rail.hidden', stored]] : []);
  document = { documentElement: { classList: { toggle(k, on) { if (on) classes.add(k); else classes.delete(k); } } }, body, querySelector: s => s === '.hdr' ? header : document.nativeDialog, createElement: t => new Element(t), createTextNode: t => ({ textContent: t }), querySelectorAll: () => [], addEventListener(k, fn) { const previous = this[k]; this[k] = e => { previous?.(e); fn(e); }; } };
  const media = { matches: !wide, addEventListener(k, fn) { this.change = fn; } };
  // ermite.js defines ermiteRail (the keys of the rail) before this file runs.
  const keys = { step() {} };
  const window = { ermiteRail: keys, ermiteStore: { get: k => values.get(k), set: (k, v) => values.set(k, v) } };
  vm.runInNewContext(readFileSync(new URL('../static/js/rail.js', import.meta.url), 'utf8'), {
    window, document, matchMedia: () => media,
    MutationObserver: class { constructor(fn) { observer = fn; } observe() {} },
  });
  assert.equal(window.ermiteRail, keys, 'the drawer must not replace the keys of the rail');
  const api = window.ermiteRailDrawer.mount(rail);
  return { api, classes, values, rail, trigger: header.children[0], bar: rail.children[0].children[0], closeButton: rail.children[0].children[1], main, document, media, section, refresh: () => observer(), backdrop: body.children.at(-1) };
}

test('builds labels and controls without moving sections; opens, switches and closes', () => {
  const { rail, bar, trigger, main, document } = setup();
  assert.deepEqual(bar.children.map(b => b.textContent), ['About', 'Files 12']);
  assert.equal(bar.children[1].getAttribute('aria-controls'), 'files');
  assert.equal(rail.children.filter(el => el.className === 'rail-tabs').length, 0, 'tabs are nested in the drawer controls');
  bar.children[0].events.click();
  assert.equal(rail.getAttribute('role'), 'dialog');
  assert.equal(rail.getAttribute('aria-label'), 'About');
  assert.equal(main.inert, true);
  assert.equal(bar.inert, false, 'drawer chips remain usable');
  assert.equal(trigger.parentElement.inert, true);
  // Chips switch the section, then close it.
  bar.children[1].events.click();
  assert.equal(rail.children[2].getAttribute('data-rail-selected'), '');
  bar.children[1].events.click();
  assert.equal(rail.getAttribute('data-drawer-open'), undefined);
  assert.equal(main.inert, false);
  assert.equal(document.activeElement, trigger);
});

test('close button, backdrop, Escape and breakpoint close the drawer', () => {
  const s = setup();
  for (const close of [() => s.closeButton.events.click(), () => s.backdrop.events.click(),
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

test('context trigger opens the first section and receives focus on close', () => {
  const s = setup();
  assert.equal(s.trigger.children.at(-1).textContent, 'Panel', 'the button names the side panel');
  assert.equal(s.trigger.children[0].className, 'ico i-panel-right-open');
  s.trigger.events.click();
  assert.equal(s.trigger.getAttribute('aria-expanded'), 'true');
  assert.equal(s.document.activeElement, s.bar.children[0]);
  s.closeButton.events.click();
  assert.equal(s.trigger.getAttribute('aria-expanded'), 'false');
  assert.equal(s.document.activeElement, s.trigger);
});

test('Escape defers to a native dialog and close restores the aside semantics', () => {
  const s = setup();
  s.trigger.events.click();
  s.document.nativeDialog = {};
  s.document.keydown({ key: 'Escape', preventDefault() { assert.fail('native dialog owns Escape'); } });
  assert.equal(s.rail.getAttribute('data-drawer-open'), '');
  s.document.nativeDialog = null;
  s.closeButton.events.click();
  assert.equal(s.rail.getAttribute('aria-label'), 'Context');
  assert.equal(s.rail.getAttribute('role'), 'complementary');
});

test('the merge request rail (with .r-files) is left to mr.js', () => {
  const header = { append() { throw new Error('no trigger on the MR page'); } };
  const mrRail = { querySelector: s => s === '.r-files' ? {} : null };
  const window = {};
  const document = { querySelector: () => header, querySelectorAll: s => s === '.work > .rail' ? [mrRail] : [] };
  vm.runInNewContext(readFileSync(new URL('../static/js/rail.js', import.meta.url), 'utf8'), { window, document, matchMedia: () => ({ matches: true, addEventListener() {} }) });
  assert.equal(typeof window.ermiteRailDrawer.mount, 'function');
});

test('wide rail defaults visible; toggles persist without modal semantics', () => {
  const s = setup({ wide: true });
  assert.equal(s.trigger.getAttribute('aria-expanded'), 'true');
  s.trigger.events.click();
  assert.equal(s.trigger.getAttribute('aria-expanded'), 'false');
  assert.equal(s.trigger.getAttribute('aria-label'), 'Show the side panel');
  assert.equal(s.values.get('ermite.rail.hidden'), '1');
  assert.ok(s.classes.has('rail-hidden'));
  assert.equal(s.main.inert, false);
  assert.equal(s.rail.getAttribute('role'), 'complementary');
  s.trigger.events.click();
  assert.equal(s.trigger.getAttribute('aria-expanded'), 'true');
  assert.equal(s.values.get('ermite.rail.hidden'), '0');
  s.trigger.focus();
  s.document.keydown({ key: 'Escape', preventDefault() {}, stopImmediatePropagation() { assert.fail('wide Escape must bubble'); } });
  assert.equal(s.trigger.getAttribute('aria-expanded'), 'false');
  assert.equal(s.document.activeElement, s.trigger);
});

test('stored hidden preference survives narrow drawer use and breakpoint changes', () => {
  const s = setup({ wide: true, stored: '1' });
  assert.equal(s.trigger.getAttribute('aria-expanded'), 'false');
  s.media.matches = true; s.media.change();
  s.trigger.events.click();
  assert.equal(s.trigger.getAttribute('aria-expanded'), 'true');
  assert.equal(s.values.get('ermite.rail.hidden'), '1', 'drawer state is not stored');
  s.media.matches = false; s.media.change();
  assert.equal(s.trigger.getAttribute('aria-expanded'), 'false');
  assert.equal(s.main.inert, false);
});

test('wide Escape leaves finder and open menus alone, and respects handled events', () => {
  const s = setup({ wide: true });
  const ignored = { key: 'Escape', preventDefault() { assert.fail('not the rail escape'); }, stopImmediatePropagation() { assert.fail('must bubble'); } };
  s.document.activeElement = { closest: () => null }; // finder input outside rail
  s.document.keydown(ignored);
  const inside = s.bar.children[0]; inside.closest = () => ({}); inside.focus();
  s.document.keydown(ignored); // nested open details.menu
  inside.closest = () => null;
  s.document.keydown({ ...ignored, defaultPrevented: true });
  assert.equal(s.trigger.getAttribute('aria-expanded'), 'true');
  s.document.keydown({ key: 'Escape', preventDefault() {}, stopImmediatePropagation() { assert.fail('must bubble'); } });
  assert.equal(s.trigger.getAttribute('aria-expanded'), 'false');
});

test('breakpoint restores focus only when the newly hidden rail contains it', () => {
  const s = setup({ wide: true });
  s.bar.children[0].focus();
  s.media.matches = true; s.media.change();
  assert.equal(s.document.activeElement, s.trigger);
  s.media.matches = false; s.media.change();
  s.main.focus();
  s.media.matches = true; s.media.change();
  assert.equal(s.document.activeElement, s.main);
});
