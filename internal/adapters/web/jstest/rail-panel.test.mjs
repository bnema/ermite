// SPDX-License-Identifier: AGPL-3.0-or-later
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import vm from 'node:vm';

function setup(phone = false, { wide = false, storage = new Map(), refused = false } = {}) {
  let document, observer;
  class Element {
    constructor(tag) { this.tagName = tag.toUpperCase(); this.children = []; this.attrs = {}; this.events = {}; this.id = ''; this.inert = false; this.isConnected = true; this.tabIndex = 0; this.classes = new Set(); this.classList = { add: k => this.classes.add(k), remove: k => this.classes.delete(k) }; }
    append(...els) { for (const el of els) { if (el.parentElement) el.parentElement.children = el.parentElement.children.filter(c => c !== el); el.parentNode = el.parentElement = this; this.children.push(el); } }
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
    hasAttribute(k) { return k in this.attrs; }
    cloneNode() { return this; }
  }
  const body = new Element('body');
  body.classes = new Set();
  body.classList = { toggle(k, on) { if (on) body.classes.add(k); else body.classes.delete(k); } };
  const work = new Element('div'), main = new Element('main'), rail = new Element('aside');
  const header = new Element('header');
  body.append(header);
  body.append(work); work.append(main, rail);
  const section = (id, label) => { const s = new Element('section'), h = new Element('h2'); s.id = id; h.textContent = label; s.append(h); return s; };
  rail.setAttribute('aria-label', 'Context');
  rail.setAttribute('role', 'complementary');
  rail.append(section('about', 'About'), section('files', 'Files 12'));
  let current = rail;
  document = { body, documentElement: { classList: { toggle() {} } }, querySelector: s => s === '.hdr-acts' ? header : s === '.work > .rail' ? current : document.nativeDialog, createElement: t => new Element(t), createTextNode: t => ({ textContent: t }), querySelectorAll: () => [], addEventListener(k, fn) { const hs = (this.handlers ||= {})[k] ||= []; hs.push(fn); this[k] = e => { for (const h of hs) h(e); }; } };
  const media = { matches: !wide, addEventListener(k, fn) { this.change = fn; } };
  const mobile = { matches: phone, addEventListener(k, fn) { this.change = fn; } };
  // ermite.js defines ermiteRail (the keys of the rail) before this file runs.
  const keys = { step() {} };
  // store.js (ermiteStore) survives refused storage itself; a missing store is the refused case here.
  const window = { ermiteRail: keys, ermiteStore: refused ? undefined : { get: k => storage.get(k) ?? null, set: (k, v) => storage.set(k, v) } };
  vm.runInNewContext(readFileSync(new URL('../static/js/rail.js', import.meta.url), 'utf8'), {
    window, document, matchMedia: s => s.includes("720") ? mobile : media,
    MutationObserver: class { constructor(fn) { observer = fn; } observe() {} },
  });
  assert.equal(window.ermiteRail, keys, 'the panel must not replace the keys of the rail');
  const api = window.ermiteRailPanel;
  return { api, rail, trigger: header.children[0], closeButton: rail.children[0].children[0], main, document, media, mobile, body,
    refresh: () => observer(),
    replace() { const next = new Element('aside'); next.setAttribute('aria-label', 'Context'); work.children = [main, next]; next.parentElement = work; current = next; observer(); return next; } };
}

test('tablet toggle controls the whole rail without a modal or focus isolation', () => {
  const s = setup();
  assert.equal(s.trigger.children.length, 1, 'icon only');
  assert.equal(s.trigger.getAttribute('aria-controls'), s.rail.id);
  assert.equal(s.trigger.getAttribute('aria-expanded'), 'false');
  s.trigger.events.click();
  assert.equal(s.trigger.getAttribute('aria-expanded'), 'true');
  assert.ok(s.body.classes.has('rail-open'));
  assert.equal(s.rail.getAttribute('role'), 'complementary');
  assert.equal(s.main.inert, false);
  assert.equal(s.rail.children.filter(c => c.tagName === 'SECTION').length, 2);
  s.trigger.events.click();
  assert.equal(s.trigger.getAttribute('aria-expanded'), 'false');
});

test('phone opens a modal, closes with Escape or close button, and restores focus', () => {
  const s = setup(true);
  s.trigger.events.click();
  assert.equal(s.document.activeElement, s.closeButton);
  assert.equal(s.rail.getAttribute('aria-modal'), 'true');
  assert.equal(s.main.inert, true);
  s.document.keydown({ key: 'Escape', preventDefault() {}, stopImmediatePropagation() {} });
  assert.equal(s.trigger.getAttribute('aria-expanded'), 'false');
  assert.equal(s.document.activeElement, s.trigger);
  assert.equal(s.main.inert, false);
  s.trigger.events.click(); s.closeButton.events.click();
  assert.equal(s.rail.getAttribute('role'), 'complementary');
});

test('native dialogs own Escape; desktop breakpoint restores wide default', () => {
  const s = setup(true);
  s.trigger.events.click(); s.document.nativeDialog = {};
  s.document.keydown({ key: 'Escape', preventDefault() { assert.fail(); } });
  assert.equal(s.trigger.getAttribute('aria-expanded'), 'true');
  s.media.matches = false; s.media.change();
  assert.equal(s.trigger.getAttribute('aria-expanded'), 'true');
  assert.equal(s.main.inert, false);
});

for (const phone of [false, true]) test(`live refresh retains open and closed state (${phone ? 'phone' : 'tablet'})`, () => {
  const s = setup(phone);
  s.trigger.events.click();
  const next = s.replace();
  assert.equal(s.trigger.getAttribute('aria-expanded'), 'true');
  assert.equal(s.trigger.getAttribute('aria-controls'), next.id);
  assert.equal(next.children[0].children[0], s.closeButton);
  assert.equal(next.getAttribute('aria-modal'), phone ? 'true' : undefined);
  s.refresh();
  assert.equal(next.children.length, 1, 'controls are not duplicated');
  s.api.close(false); s.replace();
  assert.equal(s.trigger.getAttribute('aria-expanded'), 'false');
});

test('phone Tab wraps within the panel controls', () => {
  const s = setup(true);
  const last = { focus() { s.document.activeElement = last; }, getClientRects: () => [1], closest: () => null };
  s.closeButton.getClientRects = () => [1];
  s.closeButton.closest = () => null;
  s.rail.querySelectorAll = () => [s.closeButton, last];
  s.trigger.events.click();
  let prevented = 0;
  s.document.keydown({ key: 'Tab', shiftKey: true, preventDefault() { prevented++; } });
  assert.equal(s.document.activeElement, last);
  s.document.keydown({ key: 'Tab', preventDefault() { prevented++; } });
  assert.equal(s.document.activeElement, s.closeButton);
  assert.equal(prevented, 2);
});

test('wide toggle is present, starts expanded, collapses and expands with persistence', () => {
  const storage = new Map();
  const s = setup(false, { wide: true, storage });
  assert.equal(s.trigger.hidden, false);
  assert.equal(s.trigger.children.length, 1);
  assert.equal(s.trigger.getAttribute('aria-expanded'), 'true');
  assert.ok(s.body.classes.has('rail-open'));
  s.trigger.events.click();
  assert.equal(s.trigger.getAttribute('aria-expanded'), 'false');
  assert.ok(!s.body.classes.has('rail-open'));
  assert.equal(storage.get('ermite.rail.hidden'), '1');
  const next = setup(false, { wide: true, storage });
  assert.equal(next.trigger.getAttribute('aria-expanded'), 'false');
  next.trigger.events.click();
  assert.equal(next.trigger.getAttribute('aria-expanded'), 'true');
  assert.equal(storage.get('ermite.rail.hidden'), '0');
  assert.equal(setup(false, { wide: true, storage }).trigger.getAttribute('aria-expanded'), 'true');
});

test('resize keeps wide preference separate from narrow state and updates aria', () => {
  const s = setup(false, { wide: true });
  s.trigger.events.click(); // wide collapsed
  s.media.matches = true; s.media.change();
  assert.equal(s.trigger.getAttribute('aria-expanded'), 'false');
  s.trigger.events.click(); // tablet expanded
  s.media.matches = false; s.media.change();
  assert.equal(s.trigger.getAttribute('aria-expanded'), 'false', 'wide stays collapsed');
  s.trigger.events.click(); // wide expanded
  s.media.matches = true; s.media.change();
  assert.equal(s.trigger.getAttribute('aria-expanded'), 'false', 'tablet defaults closed');
  s.mobile.matches = true; s.mobile.change();
  s.trigger.events.click();
  assert.equal(s.trigger.getAttribute('aria-expanded'), 'true');
  assert.equal(s.rail.getAttribute('aria-modal'), 'true');
  s.mobile.matches = false; s.mobile.change();
  assert.equal(s.trigger.getAttribute('aria-expanded'), 'true');
  assert.equal(s.rail.getAttribute('aria-modal'), undefined);
  s.media.matches = false; s.media.change();
  assert.equal(s.trigger.getAttribute('aria-expanded'), 'true', 'wide restores expanded');
});

test('refused storage still allows wide toggling', () => {
  const s = setup(false, { wide: true, refused: true });
  assert.equal(s.trigger.getAttribute('aria-expanded'), 'true');
  s.trigger.events.click();
  assert.equal(s.trigger.getAttribute('aria-expanded'), 'false');
});

test('wide: transient close (dialogs, h) keeps the panel and the saved choice', () => {
  const storage = new Map();
  const s = setup(false, { wide: true, storage });
  s.api.close(false);
  assert.equal(s.trigger.getAttribute('aria-expanded'), 'true');
  assert.equal(storage.get('ermite.rail.hidden'), undefined);
  s.api.open(false);
  assert.equal(storage.get('ermite.rail.hidden'), undefined, 'only the toggle saves a choice');
});

test('opening the column keeps the focus on its toggle', () => {
  const s = setup(false);
  s.trigger.focus();
  s.trigger.events.click();
  assert.equal(s.trigger.getAttribute('aria-expanded'), 'true');
  assert.equal(s.document.activeElement, s.trigger);
});

test('narrowing moves the focus out of a panel it hides, and only then', () => {
  const s = setup(false, { wide: true });
  const inside = s.rail.children.at(-1);
  inside.focus();
  s.media.matches = true; s.media.change();
  assert.equal(s.trigger.getAttribute('aria-expanded'), 'false');
  assert.equal(s.document.activeElement, s.trigger);
  const t = setup(false, { wide: true });
  t.main.focus();
  t.media.matches = true; t.media.change();
  assert.equal(t.document.activeElement, t.main);
});

test('wide Escape collapses from the panel or its toggle, not from elsewhere or once handled', () => {
  const storage = new Map();
  const s = setup(false, { wide: true, storage });
  const esc = (extra = {}) => s.document.keydown({ key: 'Escape', preventDefault() { this.defaultPrevented = true; }, stopImmediatePropagation() {}, ...extra });
  s.main.focus(); esc();
  assert.equal(s.trigger.getAttribute('aria-expanded'), 'true');
  s.trigger.focus(); esc({ defaultPrevented: true });
  assert.equal(s.trigger.getAttribute('aria-expanded'), 'true');
  s.trigger.focus(); esc({ isComposing: true });
  assert.equal(s.trigger.getAttribute('aria-expanded'), 'true');
  s.trigger.focus(); esc();
  assert.equal(s.trigger.getAttribute('aria-expanded'), 'false');
  assert.equal(storage.get('ermite.rail.hidden'), '1');
});

test('live refresh keeps the phone sheet still and its close control focusable by id', () => {
  const s = setup(true);
  s.trigger.events.click();
  assert.ok(s.rail.classes.has('rail-enter'), 'opening slides in');
  const next = s.replace();
  assert.ok(!next.classes.has('rail-enter'), 'a refresh does not slide in again');
  assert.equal(s.closeButton.id, 'rail-close');
});
