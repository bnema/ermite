// A DOM just big enough for the glue of static/js/files.js: elements with
// attributes, dataset, children and the few selectors the script uses
// ("tag.class[attr][attr=v]:not([attr])", optionally after ":scope >" or
// "#id >"). Not a test file: the tests import it.
import assert from 'node:assert/strict';

const kebab = (k) => 'data-' + k.replace(/[A-Z]/g, (c) => '-' + c.toLowerCase());

export class Text {
  constructor(data) {
    this.nodeType = 3;
    this.data = data;
    this.parent = null;
  }
  remove() {
    if (!this.parent) return;
    this.parent.nodes.splice(this.parent.nodes.indexOf(this), 1);
    this.parent = null;
  }
  get textContent() { return this.data; }
}

// A compound selector is checked on one element: tag, .class, #id, [attr],
// [attr=v], :not([attr]). `hidden` is a property of the element, as in a page.
function compound(sel) {
  const re = /([a-z][a-z0-9]*)|\.([\w-]+)|#([\w-]+)|:not\(\[([\w-]+)\]\)|\[([\w-]+)(?:=(?:"([^"]*)"|([^\]]*)))?\]/y;
  const want = { tag: null, cls: [], id: null, not: [], attrs: [] };
  while (re.lastIndex < sel.length) {
    const m = re.exec(sel);
    assert.ok(m, 'unsupported selector: ' + sel);
    if (m[1]) want.tag = m[1].toUpperCase();
    else if (m[2]) want.cls.push(m[2]);
    else if (m[3]) want.id = m[3];
    else if (m[4]) want.not.push(m[4]);
    else want.attrs.push([m[5], m[6] ?? m[7]]);
  }
  return (el) => {
    if (want.tag && el.tagName !== want.tag) return false;
    if (want.id && el.id !== want.id) return false;
    if (!want.cls.every((c) => el.className.split(/\s+/).includes(c))) return false;
    if (want.not.some((a) => el.hasAttribute(a))) return false;
    return want.attrs.every(([a, v]) => (v === undefined ? el.hasAttribute(a) : el.getAttribute(a) === v));
  };
}

export class El {
  constructor(tag) {
    this.nodeType = 1;
    this.tagName = tag.toUpperCase();
    this.attrs = new Map();
    this.nodes = [];
    this.parent = null;
    this.hidden = false;
    this.className = '';
    this.id = '';
    this.value = '';
    this.href = '';
    this.tabIndex = 0;
    this.text = '';
    this.style = { props: {}, setProperty(k, v) { this.props[k] = v; } };
    this.dataset = new Proxy({}, {
      get: (_, k) => this.getAttribute(kebab(k)) ?? undefined,
      set: (_, k, v) => { this.setAttribute(kebab(k), v); return true; },
      has: (_, k) => this.hasAttribute(kebab(k)),
    });
  }
  setAttribute(k, v) { this.attrs.set(k, String(v)); }
  getAttribute(k) { return this.attrs.has(k) ? this.attrs.get(k) : null; }
  hasAttribute(k) { return k === 'hidden' ? this.hidden : this.attrs.has(k); }
  removeAttribute(k) { this.attrs.delete(k); }

  get children() { return this.nodes.filter((n) => n.nodeType === 1); }
  get parentElement() { return this.parent; }
  get nextElementSibling() {
    if (!this.parent) return null;
    const sib = this.parent.children;
    return sib[sib.indexOf(this) + 1] || null;
  }
  get previousElementSibling() {
    if (!this.parent) return null;
    const sib = this.parent.children;
    return sib[sib.indexOf(this) - 1] || null;
  }
  get textContent() { return this.text + this.nodes.map((n) => n.textContent).join(''); }
  set textContent(v) {
    this.nodes.forEach((n) => { n.parent = null; });
    this.nodes = [];
    this.text = String(v);
  }

  adopt(list) {
    return list.map((n) => {
      const c = typeof n === 'string' ? new Text(n) : n;
      c.remove();
      c.parent = this;
      return c;
    });
  }
  append(...list) { this.nodes.push(...this.adopt(list)); }
  replaceChildren(...list) {
    this.nodes.forEach((n) => { n.parent = null; });
    this.nodes = [];
    this.append(...list);
  }
  after(...list) {
    const p = this.parent;
    const added = p.adopt(list);
    p.nodes.splice(p.nodes.indexOf(this) + 1, 0, ...added);
  }
  remove() {
    if (!this.parent) return;
    this.parent.nodes.splice(this.parent.nodes.indexOf(this), 1);
    this.parent = null;
  }
  closest(sel) {
    const ok = compound(sel);
    for (let e = this; e; e = e.parent) if (ok(e)) return e;
    return null;
  }
  matches(sel) { return compound(sel)(this); }

  // querySelectorAll: ":scope > c" and "#id > c" look at the children of the
  // scope or of the element with that id; anything else at the descendants.
  querySelectorAll(sel) {
    const m = /^(:scope|#[\w-]+)\s*>\s*(.+)$/.exec(sel);
    const ok = compound(m ? m[2] : sel);
    if (m) {
      const scope = m[1] === ':scope' ? this : this.find((e) => e.id === m[1].slice(1));
      return scope ? scope.children.filter(ok) : [];
    }
    const out = [];
    const walk = (e) => e.children.forEach((c) => { if (ok(c)) out.push(c); walk(c); });
    walk(this);
    return out;
  }
  querySelector(sel) { return this.querySelectorAll(sel)[0] || null; }
  find(pred) {
    for (const c of this.children) {
      if (pred(c)) return c;
      const d = c.find(pred);
      if (d) return d;
    }
    return null;
  }
  scrollIntoView() {}
  focus() { dom.focused.push(this); dom.active = this; }
  contains(n) {
    for (let e = n; e; e = e.parent) if (e === this) return true;
    return false;
  }
  click() { dom.clicked.push(this); }
}

export const dom = { clicked: [], focused: [], active: null, body: null, holdFrames: false, frames: new Map(), frameID: 0 };

// flushFrames runs the frames held since the last flush.
export function flushFrames() {
  const fs = [...dom.frames.values()];
  dom.frames.clear();
  fs.forEach((f) => f());
}

// installDom puts the globals files.js expects. parseHTML(text) answers the
// nodes that a template's innerHTML makes.
export function installDom({ parseHTML } = {}) {
  dom.body = new El('body');
  dom.clicked = [];
  dom.focused = [];
  dom.active = null;
  const doc = {
    body: dom.body,
    get activeElement() { return dom.active || dom.body; },
    createElement(tag) {
      const e = new El(tag);
      if (tag === 'template') {
        e.content = new El('#fragment');
        Object.defineProperty(e, 'innerHTML', { set(text) { e.content.replaceChildren(...parseHTML(text)); } });
      }
      return e;
    },
    createTextNode: (data) => new Text(data),
    getElementById: (id) => dom.body.find((e) => e.id === id),
    querySelectorAll: (sel) => dom.body.querySelectorAll(sel),
    querySelector: (sel) => dom.body.querySelector(sel),
  };
  globalThis.window = globalThis;
  globalThis.document = doc;
  globalThis.location = { href: '' };
  // A frame runs at once, unless a test holds the frames (dom.holdFrames) to
  // act between a keystroke and its frame, as in a browser; flushFrames runs them.
  globalThis.requestAnimationFrame = (f) => {
    if (!dom.holdFrames) { f(); return 0; }
    dom.frames.set(++dom.frameID, f);
    return dom.frameID;
  };
  globalThis.cancelAnimationFrame = (id) => { dom.frames.delete(id); };
  return doc;
}

// el makes an element: el('li', {id, cls, attrs}, ...children).
export function el(tag, { id = '', cls = '', attrs = {}, ...props } = {}, ...children) {
  const e = new El(tag);
  e.id = id;
  e.className = cls;
  Object.assign(e, props);
  for (const [k, v] of Object.entries(attrs)) e.setAttribute(k, v);
  e.append(...children);
  return e;
}

export const settle = () => new Promise((r) => setTimeout(r, 0));
