// files.js: the Files view. A directory opens in place: its entries are
// fetched once as table rows (the tree page with ?rows), inserted under it
// and kept in the DOM, so closing and opening it again needs no request.
//
// Rows opened in place carry data-depth and the --depth custom property
// (set through the CSSOM: the CSP forbids style attributes). Rows of the
// page itself have depth 0. Nothing here touches the document at load; the
// handlers are declared in the templates (hx-on:click on #tree-rows).
window.ermiteTree = (() => {
  const depthOf = (tr) => Number(tr.dataset.depth || 0);
  const linkOf = (tr) => tr.querySelector('a[data-expand]');
  const isOpen = (a) => a.getAttribute('aria-expanded') === 'true';

  function setOpen(tr, a, open) {
    a.setAttribute('aria-expanded', open ? 'true' : 'false');
    const kind = tr.querySelector('.c-kind');
    if (kind) kind.textContent = open ? '▾' : '▸';
  }

  // collapse hides every row under tr. The cursor of a row it hides moves to
  // tr, so it never rests on a row that cannot be seen.
  function collapse(tr, a) {
    setOpen(tr, a, false);
    const d = depthOf(tr);
    for (let r = tr.nextElementSibling; r && depthOf(r) > d; r = r.nextElementSibling) {
      r.hidden = true;
      if (r.hasAttribute('data-cursor')) {
        r.removeAttribute('data-cursor');
        tr.setAttribute('data-cursor', '');
      }
    }
  }

  // expand shows the rows under tr that no collapsed directory between hides.
  function expand(tr, a) {
    setOpen(tr, a, true);
    const d = depthOf(tr);
    let closedAt = Infinity; // depth of the collapsed directory whose rows are skipped
    for (let r = tr.nextElementSibling; r && depthOf(r) > d; r = r.nextElementSibling) {
      const rd = depthOf(r);
      if (rd <= closedAt) closedAt = Infinity;
      r.hidden = rd > closedAt;
      const la = linkOf(r);
      if (closedAt === Infinity && la && !isOpen(la)) closedAt = rd;
    }
  }

  async function load(tr, a) {
    tr.setAttribute('aria-busy', 'true');
    try {
      const res = await fetch(a.dataset.expand, { headers: { Accept: 'text/html' } });
      if (!res.ok) throw new Error('HTTP ' + res.status);
      const t = document.createElement('template');
      t.innerHTML = await res.text();
      const d = depthOf(tr) + 1;
      const rows = Array.from(t.content.children).filter((r) => r.tagName === 'TR');
      if (!rows.length) throw new Error('no rows');
      // An ancestor closed while the request was out: tr is hidden, and its
      // rows come in hidden, for the expand of that ancestor to show.
      const covered = tr.hidden;
      for (const r of rows) {
        r.dataset.depth = String(d);
        r.style.setProperty('--depth', String(d));
        if (covered) r.hidden = true;
      }
      tr.after(...rows);
      tr.dataset.loaded = '1';
      tr.removeAttribute('aria-busy');
      if (covered) setOpen(tr, a, true);
      else expand(tr, a);
    } catch (e) {
      location.href = a.href;
    }
  }

  function toggle(tr) {
    const a = linkOf(tr);
    if (!a || tr.getAttribute('aria-busy') === 'true') return;
    if (isOpen(a)) collapse(tr, a);
    else if (tr.dataset.loaded) expand(tr, a);
    else load(tr, a);
  }

  return {
    // click opens or closes the directory under a plain left click; any
    // modified or other click keeps the link's own navigation.
    click(event) {
      const a = event.target.closest('a[data-expand]');
      if (!a || event.button !== 0 || event.ctrlKey || event.metaKey || event.shiftKey || event.altKey) return;
      event.preventDefault();
      toggle(a.closest('tr'));
    },
  };
})();

// ermiteFinder: the fuzzy finder of the Files view and the file hits of the
// ":" palette. The core (index, search, positions, visible) is pure and runs
// in Node; the DOM glue below it runs only from the handlers declared in the
// templates. A path reaches the page through textContent, never as HTML.
window.ermiteFinder = (() => {
  // ---- core ---------------------------------------------------------------

  const NEG = -1e9;
  const MAXLEN = 1024; // longer paths are scored on their greedy match
  const B_OTHER = 1 << 30;

  // BIT is the mask bit of an ASCII code: a-z 0-25, digit 26, "/" 27, "." 28,
  // "-" and "_" 29, anything else 30.
  const BIT = new Uint32Array(128).fill(B_OTHER);
  for (let c = 97; c <= 122; c++) BIT[c] = 1 << (c - 97);
  for (let c = 48; c <= 57; c++) BIT[c] = 1 << 26;
  BIT[47] = 1 << 27;
  BIT[46] = 1 << 28;
  BIT[45] = BIT[95] = 1 << 29;
  const bitOf = (c) => (c < 128 ? BIT[c] : B_OTHER);

  const UPPER = /[A-Z]/;
  // fold lowercases A-Z only: the length is kept, so a position in the folded
  // string is a position in the original.
  const fold = (s) => (UPPER.test(s) ? s.replace(/[A-Z]/g, (c) => String.fromCharCode(c.charCodeAt(0) | 32)) : s);

  // index prepares the paths for search.
  function index(paths) {
    const n = paths.length;
    const lower = new Array(n);
    const mask = new Uint32Array(n);
    const base = new Int32Array(n);
    for (let i = 0; i < n; i++) {
      const p = paths[i];
      const l = fold(p);
      lower[i] = l;
      let m = 0;
      for (let j = 0; j < l.length; j++) m |= bitOf(l.charCodeAt(j));
      mask[i] = m;
      base[i] = p.lastIndexOf('/') + 1;
    }
    return { paths, lower, mask, base };
  }

  // bonusAt rewards a match at a boundary: after "/" or at the start, after a
  // separator, at a camelCase hump or a letter after a digit.
  function bonusAt(orig, j) {
    if (j === 0) return 10;
    const p = orig.charCodeAt(j - 1);
    if (p === 47) return 10;
    if (p === 95 || p === 45 || p === 46 || p === 32) return 8;
    const c = orig.charCodeAt(j);
    const upper = c >= 65 && c <= 90;
    if (p >= 97 && p <= 122 && upper) return 7;
    if (p >= 48 && p <= 57 && (upper || (c >= 97 && c <= 122))) return 7;
    return 0;
  }

  // Scratch space of matchTerm.
  let sLo = new Int32Array(64);
  let sHi = new Int32Array(64);
  let sOut = new Int32Array(64);
  let sPtr = new Int32Array(0);
  let rowA = new Int32Array(MAXLEN + 2);
  let rowB = new Int32Array(MAXLEN + 2);

  // matchTerm scores the best placement of the characters of t in s (the
  // folded path; orig is the path as it is), starting at from or later: NEG
  // when there is none. With trace, the positions of the best placement are
  // left in sOut[0..t.length). It is the DP of fzf v2: a matched character is
  // worth 16 plus its boundary bonus (doubled for the first), one that follows
  // the previous match gains max(bonus, 4), and a gap costs 3 to open and 1
  // per further skipped character.
  function matchTerm(orig, s, t, from, trace) {
    const n = s.length;
    const m = t.length;
    if (sLo.length < m) {
      sLo = new Int32Array(m * 2);
      sHi = new Int32Array(m * 2);
      sOut = new Int32Array(m * 2);
    }
    let pos = from - 1;
    for (let i = 0; i < m; i++) {
      pos = s.indexOf(t[i], pos + 1);
      if (pos < 0) return NEG;
      sLo[i] = pos;
    }
    pos = n;
    for (let i = m - 1; i >= 0; i--) {
      pos = s.lastIndexOf(t[i], pos - 1);
      sHi[i] = pos;
    }

    if (n > MAXLEN) {
      // The greedy match, scored by the same rules.
      let score = 0;
      for (let i = 0; i < m; i++) {
        const j = sLo[i];
        const b = bonusAt(orig, j);
        score += 16 + (i === 0 ? 2 * b : b);
        if (i > 0) {
          const gap = j - sLo[i - 1] - 1;
          score += gap === 0 ? Math.max(b, 4) : -(gap + 2);
        }
        if (trace) sOut[i] = j;
      }
      return score;
    }

    const width = n + 1;
    if (trace && sPtr.length < m * width) sPtr = new Int32Array(m * width);
    let prev = rowA;
    let cur = rowB;
    for (let i = 0; i < m; i++) {
      cur.fill(NEG, 0, width);
      const c = t.charCodeAt(i);
      const hi = sHi[i];
      let rm = NEG; // best of prev[k] + k over the k <= j-2 seen so far
      let rmK = -1;
      let kk = i > 0 ? sLo[i - 1] : 0;
      for (let j = sLo[i]; j <= hi; j++) {
        if (s.charCodeAt(j) !== c) continue;
        const b = bonusAt(orig, j);
        let sc = 16 + (i === 0 ? 2 * b : b);
        let fromK = -1;
        if (i > 0) {
          let best = NEG;
          if (j > 0 && prev[j - 1] > NEG) {
            best = prev[j - 1] + (b > 4 ? b : 4);
            fromK = j - 1;
          }
          for (; kk <= j - 2; kk++) {
            if (prev[kk] > NEG && prev[kk] + kk > rm) {
              rm = prev[kk] + kk;
              rmK = kk;
            }
          }
          if (rm > NEG && rm - j - 1 > best) {
            best = rm - j - 1;
            fromK = rmK;
          }
          if (best <= NEG) continue;
          sc += best;
        }
        cur[j] = sc;
        if (trace) sPtr[i * width + j] = fromK;
      }
      const tmp = prev;
      prev = cur;
      cur = tmp;
    }
    // prev is the row of the last character.
    let bestScore = NEG;
    let bestJ = -1;
    for (let j = sLo[m - 1]; j <= sHi[m - 1]; j++) {
      if (prev[j] > bestScore) {
        bestScore = prev[j];
        bestJ = j;
      }
    }
    if (trace && bestJ >= 0) {
      for (let i = m - 1, j = bestJ; i >= 0; i--) {
        sOut[i] = j;
        j = sPtr[i * width + j];
      }
    }
    return bestScore;
  }

  // termScore is matchTerm plus, for the last term, the path bonuses: 24 when
  // its match lies in the basename, 16 when the basename starts with it. With
  // trace, the winning positions are in `got`.
  let got = [];
  function termScore(orig, s, t, base, isLast, trace) {
    if (trace) got = [];
    let best = matchTerm(orig, s, t, 0, trace);
    if (best <= NEG) return NEG;
    if (trace) got = Array.from(sOut.subarray(0, t.length));
    if (!isLast) return best;
    if (base === 0) {
      best += 24;
    } else {
      const r = matchTerm(orig, s, t, base, trace);
      if (r > NEG && r + 24 > best) {
        best = r + 24;
        if (trace) got = Array.from(sOut.subarray(0, t.length));
      }
    }
    if (s.startsWith(t, base)) best += 16;
    return best;
  }

  const termsOf = (query) => fold(query).split(/\s+/).filter(Boolean);

  // search returns the k best paths for the query, and `all`, the indices of
  // every path that matches. prev, the result of an earlier search of the same
  // index, narrows the candidates when the new query extends it.
  function search(idx, query, k = 50, prev = null) {
    const terms = termsOf(query);
    const res = { q: query, terms, hits: [], all: new Int32Array(0), idx };
    if (!terms.length) return res;
    let qmask = 0;
    for (const t of terms) for (let j = 0; j < t.length; j++) qmask |= bitOf(t.charCodeAt(j));

    let cand = null;
    if (prev && prev.idx === idx && prev.terms.length > 0 && prev.terms.length <= terms.length
      && prev.terms.every((t, n) => terms[n].startsWith(t))) cand = prev.all;
    const count = cand ? cand.length : idx.paths.length;
    const { lower, mask } = idx;
    const all = new Int32Array(count);
    let na = 0;
    for (let c = 0; c < count; c++) {
      const i = cand ? cand[c] : c;
      if ((mask[i] & qmask) !== qmask) continue;
      const s = lower[i];
      let ok = true;
      for (const t of terms) {
        let pos = -1;
        for (let n = 0; n < t.length; n++) {
          pos = s.indexOf(t[n], pos + 1);
          if (pos < 0) { ok = false; break; }
        }
        if (!ok) break;
      }
      if (ok) all[na++] = i;
    }
    res.all = all.subarray(0, na);

    // The k best, by sorted insertion: score desc, length asc, path asc.
    const { paths, base } = idx;
    const topI = new Int32Array(k);
    const topS = new Int32Array(k);
    let nt = 0;
    const last = terms.length - 1;
    for (let c = 0; c < na; c++) {
      const i = all[c];
      const orig = paths[i];
      const s = lower[i];
      let score = 0;
      for (let n = 0; n <= last; n++) {
        const sc = termScore(orig, s, terms[n], base[i], n === last, false);
        if (sc <= NEG) { score = NEG; break; }
        score += sc;
      }
      if (score <= NEG) continue;
      if (nt === k) {
        const w = topI[k - 1];
        if (score < topS[k - 1]) continue;
        if (score === topS[k - 1]) {
          const a = paths[w];
          if (orig.length > a.length || (orig.length === a.length && orig >= a)) continue;
        }
      }
      let at = nt < k ? nt : k - 1;
      while (at > 0) {
        const o = topI[at - 1];
        const os = topS[at - 1];
        const a = paths[o];
        if (os > score || (os === score && (a.length < orig.length || (a.length === orig.length && a < orig)))) break;
        topI[at] = o;
        topS[at] = os;
        at--;
      }
      topI[at] = i;
      topS[at] = score;
      if (nt < k) nt++;
    }
    for (let h = 0; h < nt; h++) res.hits.push({ i: topI[h], score: topS[h] });
    return res;
  }

  // positions are the indices of path i that the query matches, ascending.
  function positions(idx, i, query) {
    const terms = termsOf(query);
    const orig = idx.paths[i];
    const s = idx.lower[i];
    const set = new Set();
    for (let n = 0; n < terms.length; n++) {
      termScore(orig, s, terms[n], idx.base[i], n === terms.length - 1, true);
      for (const p of got) set.add(p);
    }
    return Array.from(set).sort((a, b) => a - b);
  }

  // visible replaces the characters that read as nothing (the set of
  // invisible in text.go) by their <U+XXXX> marker.
  const INVISIBLE = /[\p{Cc}\p{Cf}\p{Zl}\p{Zp}\u115F\u1160\u3164\uFFA0]/gu;
  const visible = (s) => s.replace(INVISIBLE, (c) => '<U+' + c.codePointAt(0).toString(16).toUpperCase().padStart(4, '0') + '>');

  // ---- DOM glue -----------------------------------------------------------

  const $ = (id) => document.getElementById(id);
  const loads = new Map(); // URL -> promise of {idx, truncated}
  let rendered = null; // the result the finder list shows
  let frame = 0;
  let treeCursor = null; // the row of the tree that had the cursor when the results came
  let pendingEnter = null; // the value of the field at an Enter pressed before its results

  // load fetches the path list of the page once and indexes it.
  function load() {
    const url = document.body.dataset.paths;
    if (!url) return Promise.resolve(null);
    let p = loads.get(url);
    if (!p) {
      p = fetch(url, { headers: { Accept: 'application/json' } })
        .then((res) => {
          if (!res.ok) throw new Error('HTTP ' + res.status);
          return res.json();
        })
        .then((j) => ({ idx: index(j.paths || []), truncated: !!j.truncated }));
      p.catch(() => loads.delete(url)); // a failure is retried at the next use
      loads.set(url, p);
    }
    return p;
  }

  function blobHref(path) {
    try {
      return document.body.dataset.blob + '/' + path.split('/').map(encodeURIComponent).join('/');
    } catch (e) {
      return '#';
    }
  }

  const isHigh = (c) => c >= 0xd800 && c <= 0xdbff;
  const isLow = (c) => c >= 0xdc00 && c <= 0xdfff;

  // runs appends text[from, to) to parent, the marked characters in <mark>. A
  // surrogate pair is marked whole or not at all, so it is never split.
  function runs(parent, text, from, to, marks) {
    const marked = (j) => {
      if (marks.has(j)) return true;
      const c = text.charCodeAt(j);
      if (isLow(c) && isHigh(text.charCodeAt(j - 1))) return marks.has(j - 1);
      if (isHigh(c) && isLow(text.charCodeAt(j + 1))) return marks.has(j + 1);
      return false;
    };
    for (let j = from; j < to;) {
      const m = marked(j);
      let e = j;
      while (e < to && marked(e) === m) e++;
      const part = text.slice(j, e);
      if (m) {
        const mk = document.createElement('mark');
        mk.textContent = part;
        parent.append(mk);
      } else {
        parent.append(document.createTextNode(part));
      }
      j = e;
    }
  }

  // label is the span of a path: the directory dimmed, the name strong, the
  // matched characters marked. A path with an invisible character shows its
  // markers and no highlight.
  function label(path, pos) {
    const lbl = document.createElement('span');
    lbl.className = 'lbl';
    const shown = visible(path);
    if (shown !== path) {
      lbl.textContent = shown;
      return lbl;
    }
    const cut = path.lastIndexOf('/') + 1;
    const marks = new Set(pos);
    if (cut > 0) {
      const dir = document.createElement('span');
      dir.className = 'dir';
      runs(dir, path, 0, cut, marks);
      lbl.append(dir);
    }
    const name = document.createElement('strong');
    runs(name, path, cut, path.length, marks);
    lbl.append(name);
    return lbl;
  }

  function option(res, hit, n) {
    const path = res.idx.paths[hit.i];
    const li = document.createElement('li');
    li.setAttribute('role', 'option');
    li.setAttribute('aria-selected', 'false');
    li.id = 'finder-o' + n;
    const a = document.createElement('a');
    a.href = blobHref(path);
    a.tabIndex = -1;
    a.append(label(path, positions(res.idx, hit.i, res.q)));
    li.append(a);
    return li;
  }

  const status = (text) => { $('finder-status').textContent = text; };

  // setActive makes li the active option (null: none) of the listbox.
  function setActive(el, li) {
    $('finder-list').querySelectorAll(':scope > li[aria-selected="true"]').forEach((o) => o.setAttribute('aria-selected', 'false'));
    if (li) {
      li.setAttribute('aria-selected', 'true');
      el.setAttribute('aria-activedescendant', li.id);
    } else {
      el.removeAttribute('aria-activedescendant');
    }
  }

  // showTree: no query, the directory again, with j/k and Enter on its rows.
  function showTree(el) {
    rendered = null;
    pendingEnter = null;
    $('finder').hidden = true;
    $('tree-view').hidden = false;
    el.setAttribute('aria-expanded', 'false');
    setActive(el, null);
    const list = $('finder-list');
    list.removeAttribute('data-rows');
    list.replaceChildren();
    const rows = $('tree-rows');
    rows.setAttribute('data-rows', '');
    const keep = treeCursor;
    treeCursor = null;
    if (!rows.querySelector(':scope > tr[data-cursor]')) {
      const row = keep && keep.parentElement === rows && !keep.hidden ? keep : rows.querySelector(':scope > tr:not([hidden])');
      row?.setAttribute('data-cursor', '');
    }
  }

  // showResults: j/k and Enter act on the results, not on the rows behind.
  function showResults(el) {
    $('finder').hidden = false;
    $('tree-view').hidden = true;
    el.setAttribute('aria-expanded', 'true');
    const rows = $('tree-rows');
    rows.removeAttribute('data-rows');
    rows.querySelectorAll(':scope > tr[data-cursor]').forEach((r) => {
      treeCursor = r;
      r.removeAttribute('data-cursor');
    });
    $('finder-list').setAttribute('data-rows', '');
  }

  async function run(el) {
    const q = el.value;
    if (pendingEnter !== null && pendingEnter !== q) pendingEnter = null; // the field moved on
    if (!q.trim()) return showTree(el);
    showResults(el);
    if (!rendered) status('loading the file list…');
    let loaded;
    try {
      loaded = await load();
    } catch (e) {
      if (el.value === q) {
        pendingEnter = null;
        status('could not load the file list');
      }
      return;
    }
    if (el.value !== q) return; // the field changed during the wait
    if (!loaded) {
      pendingEnter = null;
      status('file search is not available here');
      return;
    }
    const res = search(loaded.idx, q, 50, rendered);
    const items = res.hits.map((h, n) => option(res, h, n));
    const list = $('finder-list');
    list.replaceChildren(...items);
    items[0]?.setAttribute('data-cursor', '');
    setActive(el, items[0]);
    rendered = res;
    let msg = res.all.length === 0 ? 'no file matches' : res.all.length + (res.all.length === 1 ? ' file matches' : ' files match');
    if (loaded.truncated) msg += ' · only the first 100,000 files are searched';
    status(msg);
    const open = pendingEnter === q;
    pendingEnter = null;
    if (open) items[0]?.querySelector('a').click();
  }

  function palette(el) {
    if (!document.body.dataset.paths) return Promise.resolve();
    const clear = () => document.querySelectorAll('#cmd-list > li.finder-hit').forEach((li) => li.remove());
    clear();
    const q = el.value;
    if (!q.trim()) return Promise.resolve();
    return load().then((loaded) => {
      if (el.value !== q || !loaded) return;
      clear();
      const res = search(loaded.idx, q, 20);
      const list = $('cmd-list');
      for (const h of res.hits) {
        const path = res.idx.paths[h.i];
        const li = document.createElement('li');
        li.className = 'finder-hit';
        const a = document.createElement('a');
        a.href = blobHref(path);
        a.tabIndex = -1;
        const k = document.createElement('span');
        k.className = 'k';
        k.textContent = 'file';
        a.append(label(path, positions(res.idx, h.i, q)), k);
        li.append(a);
        list.append(li);
      }
      const shown = Array.from(list.querySelectorAll(':scope > li:not([hidden])'));
      if (shown.length && !shown.some((li) => li.hasAttribute('data-cursor'))) {
        list.querySelectorAll(':scope > li[data-cursor]').forEach((li) => li.removeAttribute('data-cursor'));
        shown[0].setAttribute('data-cursor', '');
      }
    }, () => {});
  }

  // input runs the search of the field, once per frame.
  function input(el) {
    if (frame) cancelAnimationFrame(frame);
    if (!el.value.trim()) {
      frame = 0;
      run(el);
      return;
    }
    frame = requestAnimationFrame(() => { frame = 0; run(el); });
  }

  return {
    index, search, positions, visible, load, palette, input,
    // key moves the cursor of the results, opens one, or clears the field.
    key(el, e) {
      if (e.isComposing) return; // the Enter or Escape of an input method is its own
      const results = !$('finder').hidden;
      if (e.key === 'Escape') {
        el.value = '';
        input(el);
        return;
      }
      if (!results) return;
      const list = $('finder-list');
      const cur = list.querySelector(':scope > li[data-cursor]');
      if (e.key === 'ArrowDown' || e.key === 'ArrowUp') {
        e.preventDefault();
        const next = e.key === 'ArrowDown' ? cur?.nextElementSibling : cur?.previousElementSibling;
        if (!next) return;
        cur.removeAttribute('data-cursor');
        next.setAttribute('data-cursor', '');
        setActive(el, next);
        next.scrollIntoView({ block: 'nearest' });
      } else if (e.key === 'Enter') {
        e.preventDefault();
        if (rendered && rendered.q === el.value) cur?.querySelector('a')?.click();
        else pendingEnter = el.value; // the results are stale: run opens the top hit
      }
    },
  };
})();
