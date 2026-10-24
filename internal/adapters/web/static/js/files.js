// files.js: the Files view. The entries of a directory are the rows of a tree
// (li, flat in one ul, each with its depth). A directory opens in place: its
// entries are fetched once as rows (the tree page with ?rows), inserted under
// it and kept in the DOM, so closing and opening it again needs no request.
//
// Rows opened in place carry data-depth, aria-level and the --depth custom
// property (set through the CSSOM: the CSP forbids style attributes), which
// the stylesheet turns into indentation guides. Rows of the page itself have
// depth 0.
//
// Focus model: a roving tabindex on the rows. The row under the cursor
// (data-cursor) is the only one in the tab order (tabindex 0) and it has the
// DOM focus whenever the keyboard or a click moved the cursor, so j k l h, the
// arrows, Enter and Tab all act on the row that is highlighted. The name link
// of a row is out of the tab order (the row stands for it: Enter opens it);
// the link to the last commit is tabbable only in the row under the cursor
// (Tab from the row reaches it, the next Tab leaves the tree). Without
// scripts nothing is taken out of the tab order. The handlers are declared in
// the templates (hx-on:click on #tree-rows); init() runs once at load (a page
// without a tree: nothing); ermite.js calls ermiteTree.key and open for h, l
// and Enter, and ermiteTree.sync for the cursor moves of j k gg G.
window.ermiteTree = (() => {
  const depthOf = (li) => Number(li.dataset.depth || 0);
  const isDir = (li) => li.hasAttribute('data-expand');
  const isOpen = (li) => li.getAttribute('aria-expanded') === 'true';
  // A directory is loaded when rows deeper than it follow it.
  const isLoaded = (li) => { const n = li.nextElementSibling; return !!n && depthOf(n) > depthOf(li); };
  const nameLink = (li) => li.querySelector('a.t-name');

  let restoring = false;
  const stateKey = () => 'ermite:tree:' + location.pathname + location.search;

  // Storage is best effort: a private window or a full quota must not stop
  // navigation. DOM order keeps parents before children, even when hidden.
  function save() {
    if (restoring || !rowsEl()) return;
    try {
      const rows = Array.from(rowsEl().children);
      sessionStorage.setItem(stateKey(), JSON.stringify({
        dirs: rows.filter((r) => isDir(r) && (isLoaded(r) || isOpen(r)))
          .map((r) => ({ path: r.dataset.path, open: isOpen(r) })),
        cursor: cursorRow()?.dataset.path,
        x: window.scrollX, y: window.scrollY,
      }));
    } catch (e) { /* Storage may be unavailable. */ }
  }

  // Replay loads before closing anything: an open descendant of a collapsed
  // directory still needs its rows, but must finish hidden under its parent.
  async function restore(state) {
    restoring = true;
    try {
      for (const dir of state.dirs) {
        const li = Array.from(rowsEl().children).find((r) => r.dataset.path === dir.path && isDir(r));
        if (!li) continue;
        if (!isLoaded(li)) await load(li, true);
        else expand(li);
      }
      for (const dir of state.dirs) {
        const li = Array.from(rowsEl().children).find((r) => r.dataset.path === dir.path && isDir(r));
        if (li && !dir.open) collapse(li);
      }
      const cur = Array.from(rowsEl().children).find((r) => r.dataset.path === state.cursor && !r.hidden);
      if (cur) mark(cur, false);
      window.scrollTo(state.x || 0, state.y || 0);
    } finally {
      restoring = false;
    }
  }

  // tab puts a row in or out of the tab order, with its link to the last commit.
  function tab(li, on) {
    li.setAttribute('tabindex', on ? '0' : '-1');
    for (const a of li.querySelectorAll('a.t-commit')) a.setAttribute('tabindex', on ? '0' : '-1');
  }

  // prep takes a row out of the tab order: the one under the cursor comes back.
  function prep(li) {
    tab(li, false);
    nameLink(li)?.setAttribute('tabindex', '-1');
  }

  // mark makes li the row under the cursor, the one stop of the tab order,
  // and with focus gives it the DOM focus (without scrolling: the caller does).
  function mark(li, focus) {
    for (const r of Array.from(li.parentElement.children)) {
      if (r === li || !(r.hasAttribute('data-cursor') || r.getAttribute('tabindex') === '0')) continue;
      r.removeAttribute('data-cursor');
      tab(r, false);
    }
    li.setAttribute('data-cursor', '');
    tab(li, true);
    if (focus) li.focus({ preventScroll: true });
    save();
  }

  // setCursor moves the cursor, and so the focus, to li, and shows it.
  function setCursor(li) {
    mark(li, true);
    li.scrollIntoView({ block: 'nearest' });
  }

  // collapse hides every row under li. The cursor, and the focus, of a row it
  // hides move to li, so they never rest on a row that cannot be seen.
  function collapse(li) {
    li.setAttribute('aria-expanded', 'false');
    const d = depthOf(li);
    let lostCursor = false;
    let lostFocus = false;
    for (let r = li.nextElementSibling; r && depthOf(r) > d; r = r.nextElementSibling) {
      r.hidden = true;
      if (r.hasAttribute('data-cursor')) lostCursor = true;
      if (r.contains(document.activeElement)) lostFocus = true;
    }
    if (lostCursor || lostFocus) mark(li, lostFocus);
    save();
  }

  // expand shows the rows under li that no collapsed directory between hides.
  function expand(li) {
    li.setAttribute('aria-expanded', 'true');
    const d = depthOf(li);
    let closedAt = Infinity; // depth of the collapsed directory whose rows are skipped
    for (let r = li.nextElementSibling; r && depthOf(r) > d; r = r.nextElementSibling) {
      const rd = depthOf(r);
      if (rd <= closedAt) closedAt = Infinity;
      r.hidden = rd > closedAt;
      if (closedAt === Infinity && isDir(r) && !isOpen(r)) closedAt = rd;
    }
    save();
  }

  async function load(li, quiet = false) {
    li.setAttribute('aria-busy', 'true');
    try {
      const res = await fetch(li.dataset.expand, { headers: { Accept: 'text/html' } });
      if (!res.ok) throw new Error('HTTP ' + res.status);
      const t = document.createElement('template');
      t.innerHTML = await res.text();
      const d = depthOf(li) + 1;
      const rows = Array.from(t.content.children).filter((r) => r.tagName === 'LI');
      if (!rows.length) throw new Error('no rows');
      // An ancestor closed while the request was out: li is hidden, and its
      // rows come in hidden, for the expand of that ancestor to show.
      const covered = li.hidden;
      for (const r of rows) {
        r.dataset.depth = String(d);
        r.setAttribute('aria-level', String(d + 1));
        r.style.setProperty('--depth', String(d));
        prep(r);
        if (covered) r.hidden = true;
      }
      li.after(...rows);
      li.removeAttribute('aria-busy');
      if (covered) li.setAttribute('aria-expanded', 'true');
      else expand(li);
      save();
    } catch (e) {
      li.removeAttribute('aria-busy');
      if (!quiet) location.href = nameLink(li).href;
    }
  }

  function toggle(li) {
    if (!isDir(li) || li.getAttribute('aria-busy') === 'true') return;
    if (isOpen(li)) collapse(li);
    else if (isLoaded(li)) expand(li);
    else load(li);
  }

  const rowsEl = () => document.getElementById('tree-rows');
  const cursorRow = () => rowsEl()?.querySelector(':scope > [data-cursor]') ?? null;
  const flash = (m) => window.ermite?.flash?.(m);

  // parentOf is the visible row of the directory that holds li: the closest
  // row above it that is less deep.
  function parentOf(li) {
    const d = depthOf(li);
    for (let r = li.previousElementSibling; r; r = r.previousElementSibling) if (depthOf(r) < d) return r;
    return null;
  }

  return {
    // click puts the cursor and the focus on the row under any click, and
    // opens or closes the directory under a plain left click on its name or its
    // node; any modified or other click, and a click on the last commit, keep
    // their own navigation.
    click(event) {
      const li = event.target.closest('li');
      if (!li || !li.parentElement) return;
      mark(li, true);
      if (event.button !== 0 || event.ctrlKey || event.metaKey || event.shiftKey || event.altKey) return;
      if (!event.target.closest('.t-main') || !isDir(li)) return;
      event.preventDefault();
      toggle(li);
    },

    // init takes the rows out of the tab order but the one under the cursor
    // (the first when none has it). It runs once the page has loaded, and
    // not at all without a tree.
    init() {
      const rows = typeof document === 'undefined' ? null : rowsEl();
      if (!rows) return;
      let state;
      try {
        state = JSON.parse(sessionStorage.getItem(stateKey()));
        if (!Array.isArray(state?.dirs) || !state.dirs.every((d) => d && typeof d.path === 'string' && typeof d.open === 'boolean')) state = null;
      } catch (e) { /* Storage may be unavailable or from an older page. */ }
      restoring = !!state;
      const lis = Array.from(rows.children);
      for (const li of lis) prep(li);
      const cur = cursorRow() ?? lis[0];
      if (cur) mark(cur, false);
      // A bfcache return keeps this document and does not run init again.
      // Only a fresh document replays the saved requests and scroll.
      window.addEventListener?.('pagehide', save);
      window.addEventListener?.('scroll', save, { passive: true });
      if (state) return restore(state);
    },

    // sync follows a cursor that ermite.js moved (j k gg G): the focus goes
    // with it, and the tab stop.
    sync(li) {
      if (li.parentElement && li.parentElement.id === 'tree-rows') mark(li, true);
    },

    // mark is for the finder, which takes the cursor away and gives it back.
    mark(li) { mark(li, false); },

    // active says that h and l (and the arrows) walk the tree: it is shown,
    // not replaced by the results of the finder.
    active() {
      const rows = rowsEl();
      return !!rows && rows.hasAttribute('data-rows');
    },

    // open is Enter on the row under the cursor: a directory opens or closes
    // in place, a file opens. It answers whether the tree took the key.
    open() {
      const li = this.active() && cursorRow();
      if (!li) return false;
      if (isDir(li)) toggle(li);
      else if (nameLink(li)) nameLink(li).click();
      else flash('a submodule has no page here');
      return true;
    },

    // arrow is a key of the tree pattern (WAI-ARIA APG Tree View) pressed with
    // the focus on a row: it only moves the focus or the expansion, and never
    // opens a file or leaves the page. It answers whether the key is one the
    // tree handles (the caller then keeps the browser from scrolling).
    //  Down, Up: the next, the previous visible row. Home, End: the first, the last.
    //  Right: a closed directory opens, an open one moves to its first entry, a
    //         file does nothing.
    //  Left:  an open directory closes, any other row moves to its directory,
    //         and a top-level row does nothing.
    arrow(key) {
      if (!this.active()) return false;
      const li = cursorRow();
      if (!li) return false;
      const rows = Array.from(li.parentElement.children).filter((r) => !r.hidden);
      const i = rows.indexOf(li);
      switch (key) {
        case 'ArrowDown': if (rows[i + 1]) setCursor(rows[i + 1]); break;
        case 'ArrowUp': if (i > 0) setCursor(rows[i - 1]); break;
        case 'Home': if (rows[0]) setCursor(rows[0]); break;
        case 'End': if (rows.length) setCursor(rows[rows.length - 1]); break;
        case 'ArrowRight':
          if (!isDir(li)) break;
          if (!isOpen(li)) toggle(li);
          else if (rows[i + 1] && depthOf(rows[i + 1]) > depthOf(li)) setCursor(rows[i + 1]);
          break;
        case 'ArrowLeft':
          if (isDir(li) && isOpen(li)) collapse(li);
          else if (parentOf(li)) setCursor(parentOf(li));
          break;
        default: return false;
      }
      return true;
    },

    // key is l (into = true) or h on the row under the cursor, the Vim keys
    // with their richer behaviour; it answers whether the tree took the key.
    //  l: a closed directory opens, an open one moves to its first entry, a
    //     file is opened.
    //  h: an open directory closes, any other row goes to its directory, and a
    //     top-level row to the parent directory's page.
    key(into) {
      if (!this.active()) return false;
      const li = cursorRow();
      if (!li) return false;
      // The focus joins the cursor row first (it may still be on the page, as
      // after a load): Enter then acts on the row the key acted on.
      if (document.activeElement !== li && !li.contains(document.activeElement)) mark(li, true);
      if (into) {
        if (isDir(li)) {
          if (!isOpen(li)) return toggle(li), true;
          const n = li.nextElementSibling;
          if (n && !n.hidden && depthOf(n) > depthOf(li)) setCursor(n);
          return true;
        }
        const a = nameLink(li);
        if (a) a.click();
        else flash('a submodule has no page here');
        return true;
      }
      if (isDir(li) && isOpen(li)) {
        collapse(li);
        return true;
      }
      const up = parentOf(li);
      if (up) setCursor(up);
      else if (rowsEl().dataset.up) location.href = rowsEl().dataset.up;
      else flash('top of the tree');
      return true;
    },
  };
})();

window.ermiteTree.init();

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
  let loading = null; // the promise of {idx, truncated}; null before the first load and after a failure
  let rendered = null; // the result the finder list shows
  let frame = 0;
  let treeCursor = null; // the row of the tree that had the cursor when the results came
  let pendingEnter = null; // the value of the field at an Enter pressed before its results

  // load fetches the path list of the page once and indexes it.
  function load() {
    const url = document.body.dataset.paths;
    if (!url) return Promise.resolve(null);
    if (!loading) {
      const p = fetch(url, { headers: { Accept: 'application/json' } })
        .then((res) => {
          if (!res.ok) throw new Error('HTTP ' + res.status);
          return res.json();
        })
        .then((j) => ({ idx: index(j.paths || []), truncated: !!j.truncated }));
      loading = p;
      p.catch(() => { if (loading === p) loading = null; }); // a failure is retried at the next use
    }
    return loading;
  }

  // The server's JSON never carries a lone surrogate, so encodeURIComponent
  // cannot throw here.
  const blobHref = (path) => document.body.dataset.blob + '/' + path.split('/').map(encodeURIComponent).join('/');

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

  // hitLink is the presentation wrapper of path i; the option owns activation.
  function hitLink(res, i) {
    const path = res.idx.paths[i];
    const a = document.createElement('a');
    a.dataset.href = blobHref(path);
    a.setAttribute('role', 'presentation');
    a.append(label(path, positions(res.idx, i, res.q)));
    return a;
  }

  function option(res, hit, n) {
    const li = document.createElement('li');
    li.setAttribute('role', 'option');
    li.setAttribute('aria-selected', 'false');
    li.id = 'finder-o' + n;
    li.append(hitLink(res, hit.i));
    return li;
  }

  const status = (text) => { $('finder-status').textContent = text; };

  // syncActive makes the option that has data-cursor the active one: j/k
  // (keys.templ) move the cursor while the field is blurred.
  function syncActive(el) {
    const cur = $('finder-list').querySelector(':scope > li[data-cursor]');
    if (cur && cur.getAttribute('aria-selected') !== 'true') setActive(el, cur);
    return cur;
  }

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

  // sync is syncActive for whoever moves the cursor of the results from
  // outside (j / k and gg / G, ermite.js): the options and the field follow it.
  function sync() {
    const el = $('finder-q');
    if (el && $('finder') && !$('finder').hidden) syncActive(el);
  }

  // showTree: no query, the directory again, with j/k and Enter on its rows.
  function showTree(el) {
    status('');
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
    if (!rows.querySelector(':scope > [data-cursor]')) {
      const row = keep && keep.parentElement === rows && !keep.hidden ? keep : rows.querySelector(':scope > li:not([hidden])');
      if (row) window.ermiteTree.mark(row);
    }
  }

  // showResults: j/k and Enter act on the results, not on the rows behind.
  function showResults(el) {
    $('finder').hidden = false;
    $('tree-view').hidden = true;
    el.setAttribute('aria-expanded', 'true');
    const rows = $('tree-rows');
    rows.removeAttribute('data-rows');
    rows.querySelectorAll(':scope > [data-cursor]').forEach((r) => {
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
    if (loaded.truncated) msg += ' · only the first ' + loaded.idx.paths.length.toLocaleString('en') + ' files are searched';
    status(msg);
    const open = pendingEnter === q;
    pendingEnter = null;
    if (open) items[0]?.querySelector('a').click();
  }

  // The palette keeps its own frame and previous result: the field of the
  // dialog and the finder are searched independently.
  let pFrame = 0;
  let pPrev = null;
  let pGen = 0; // bumped at close: a search still loading then is dropped
  const clearHits = () => document.querySelectorAll('#cmd-list > li.finder-hit').forEach((li) => li.remove());

  // palette lists the file hits of the ":" field under its entries, once per frame.
  function palette(el) {
    if (!document.body.dataset.paths) return;
    clearHits();
    window.ermiteCmd?.sync(); // the cursor may have been on a hit
    if (pFrame) cancelAnimationFrame(pFrame);
    pFrame = 0;
    if (!el.value.trim()) {
      pPrev = null;
      return;
    }
    pFrame = requestAnimationFrame(() => { pFrame = 0; paletteRun(el); });
  }

  // paletteClose drops the frame and the result kept for the next keystroke.
  function paletteClose() {
    if (pFrame) cancelAnimationFrame(pFrame);
    pFrame = 0;
    pPrev = null;
    pGen++;
  }

  async function paletteRun(el) {
    const q = el.value;
    const gen = pGen;
    let loaded;
    try {
      loaded = await load();
    } catch (e) {
      return;
    }
    if (el.value !== q || gen !== pGen || !loaded) return; // the field changed or the dialog closed during the wait
    const res = search(loaded.idx, q, 20, pPrev);
    pPrev = res;
    clearHits();
    const list = $('cmd-list');
    for (const h of res.hits) {
      const li = document.createElement('li');
      li.className = 'finder-hit';
      const a = hitLink(res, h.i);
      const k = document.createElement('span');
      k.className = 'k';
      k.textContent = 'file';
      a.append(k);
      li.append(a);
      list.append(li);
    }
    const shown = Array.from(list.querySelectorAll(':scope > li:not([hidden])'));
    if (shown.length && !shown.some((li) => li.hasAttribute('data-cursor'))) {
      list.querySelectorAll(':scope > li[data-cursor]').forEach((li) => li.removeAttribute('data-cursor'));
      shown[0].setAttribute('data-cursor', '');
    }
    window.ermiteCmd?.sync(); // roles, ids and the active option of the new hits
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
    index, search, positions, visible, label, load, palette, paletteClose, input, sync,
    // key moves the cursor of the results, opens one, or clears the field.
    key(el, e) {
      if (e.isComposing) return; // the Enter or Escape of an input method is its own
      const results = !$('finder').hidden;
      if (e.key === 'Escape') {
        // With a query, Escape only clears it: the field keeps the focus (the
        // global handler of ermite.js would blur it). Without one, it leaves the field.
        if (!el.value) return;
        e.preventDefault();
        e.stopPropagation();
        el.value = '';
        input(el);
        return;
      }
      if (!results) {
        // A query whose first frame has not run yet: open its top hit once it does.
        if (e.key === 'Enter' && el.value.trim()) {
          e.preventDefault();
          pendingEnter = el.value;
        }
        return;
      }
      const cur = syncActive(el);
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
