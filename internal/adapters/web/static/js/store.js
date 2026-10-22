// store.js: window.ermiteStore, a small client for what the pages remember in
// the browser (localStorage): the preferences. Loaded before ermite.js, which
// builds on it (ermiteKeys, ermiteLayout).
//
// It never throws. Without storage (disabled, private mode, sandboxed frame)
// or when the browser refuses a write (quota), the value is held in memory:
// the choice lasts for this page, and the page works as if it were saved.
// Values are strings, as in localStorage; null says "nothing stored".
//
//   get(key, fallback)   the value, or fallback (null) when there is none
//   set(key, value)      stores it; null or undefined removes the key
//   remove(key)          forgets the key
//   watch(key, fn)       fn(value or null) when another tab changes the key
//                        (the "storage" event); answers a function that stops
//   pref(key, opts)      a preference among a closed list:
//                        pref(key, { values, fallback }) answers
//                        { get, set, watch }, where get is always one of
//                        values (a stored value that is not falls back) and
//                        set ignores what is not
//
// Self-contained: jstest/store.test.mjs evaluates this file alone.
//
// It also sets, before the first paint, what the stored preferences change on
// <html>: the wide mode and the theme (window.ermiteTheme, below).
window.ermiteStore = (() => {
  // What storage could not keep, by key. A key mapped to null is removed for
  // this page although storage would not say so.
  const held = new Map();

  const area = () => {
    try { return localStorage; } catch { return null; }
  };

  function get(key, fallback = null) {
    if (held.has(key)) return held.get(key) ?? fallback;
    try { return area().getItem(key) ?? fallback; } catch { return fallback; }
  }

  function set(key, value) {
    if (value === null || value === undefined) return remove(key);
    value = String(value);
    try {
      area().setItem(key, value);
      held.delete(key);
    } catch { held.set(key, value); }
  }

  function remove(key) {
    try {
      area().removeItem(key);
      held.delete(key);
    } catch { held.set(key, null); }
  }

  function watch(key, fn) {
    const on = (e) => {
      // key is null when another tab clears the whole storage.
      if (e.key !== null && e.key !== key) return;
      if (e.storageArea && e.storageArea !== area()) return;
      held.delete(key); // the other tab's word is newer than ours
      fn(e.key === null ? null : (e.newValue ?? null));
    };
    addEventListener('storage', on);
    return () => removeEventListener('storage', on);
  }

  function pref(key, { values, fallback }) {
    const valid = (v) => (values.includes(v) ? v : fallback);
    return {
      get: () => valid(get(key)),
      set(v) {
        if (values.includes(v)) set(key, v);
      },
      watch: (fn) => watch(key, (v) => fn(valid(v))),
    };
  }

  return { get, set, remove, watch, pref };
})();

// The wide mode (ermiteWide in ermite.js) is set here, before the first paint:
// shell.templ loads this file without defer, so a wide page never draws narrow
// first.
if (window.ermiteStore.get('wide') === '1') document?.documentElement?.classList?.add('wide');

// window.ermiteTheme: the colour theme, "auto" (the system's, the default),
// "light" or "dark", kept by ermiteStore ("theme"). A forced theme is
// html[data-theme], which sets color-scheme (00-tokens.css); the
// color-scheme and theme-color metas follow so the browser's own UI matches.
// Applied here, before the first paint: the page never draws in the wrong
// theme first. The footer button (ermiteThemeSwitch in ermite.js) cycles it.
//
//   get()        the mode
//   next(mode)   the mode after it: auto → light → dark → auto
//   set(mode)    stores and applies it
//   apply()      shows the stored mode on the page
//   watch(fn)    fn(mode) when another tab changes it
//
// Tested in Node by jstest/theme.test.mjs.
window.ermiteTheme = (() => {
  const values = ['auto', 'light', 'dark'];
  const pref = window.ermiteStore.pref('theme', { values, fallback: 'auto' });
  const next = (mode) => values[(values.indexOf(mode) + 1) % values.length];

  function apply() {
    const mode = pref.get();
    const doc = globalThis.document;
    const root = doc?.documentElement;
    if (!root) return mode;
    if (mode === 'auto') root.removeAttribute('data-theme');
    else root.setAttribute('data-theme', mode);
    doc.querySelector('meta[name="color-scheme"]')
      ?.setAttribute('content', mode === 'auto' ? 'light dark' : mode);
    // Each theme-color meta keeps its rendered colour in data-auto. Forced,
    // both metas take the colour of the meta of that scheme (data-scheme),
    // whatever the system scheme their media matches.
    const metas = [...doc.querySelectorAll('meta[name="theme-color"]')];
    for (const m of metas) if (!m.hasAttribute('data-auto')) m.setAttribute('data-auto', m.getAttribute('content') ?? '');
    const forced = metas.find((m) => m.getAttribute('data-scheme') === mode);
    for (const m of metas) {
      m.setAttribute('content', mode === 'auto' || !forced ? m.getAttribute('data-auto') : forced.getAttribute('data-auto'));
    }
    return mode;
  }

  function set(mode) {
    pref.set(mode);
    apply();
  }

  apply();
  return { values, get: pref.get, next, set, apply, watch: pref.watch };
})();
