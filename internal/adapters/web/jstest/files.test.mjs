// Tests of the pure core of the file finder (static/js/files.js). Run with
//   node --test 'internal/adapters/web/jstest/*.test.mjs'
import assert from 'node:assert/strict';
import { test } from 'node:test';

globalThis.window = globalThis;
await import('../static/js/files.js');
const F = window.ermiteFinder;

const REPO = [
  'README.md',
  'cmd/ermite/main.go',
  'internal/adapters/web/tree.go',
  'internal/adapters/web/tree.templ',
  'internal/adapters/web/tree_templ.go',
  'internal/adapters/web/static/css/30-list.css',
  'internal/adapters/web/static/js/files.js',
  'internal/adapters/web/jstest/files.test.mjs',
  'internal/adapters/gitcli/read.go',
  'internal/domain/tree_path.go',
  'docs/web/tree-view.md',
  'src/deeply/nested/dir/with/a/treehouse.txt',
];
const idx = F.index(REPO);
const top = (q, k = 50, prev = null) => F.search(idx, q, k, prev);
const names = (r) => r.hits.map((h) => REPO[h.i]);

test('index: ASCII-only folding keeps positions', () => {
  const x = F.index(['İndex/Straße.go', 'ABC/Def.TXT']);
  assert.equal(x.lower[1], 'abc/def.txt');
  assert.equal(x.lower[0].length, x.paths[0].length);
  assert.equal(x.lower[0][0], 'İ', 'non-ASCII is not folded');
  assert.equal(x.base[0], 'İndex/'.length);
  assert.equal(x.base[1], 4);
});

test('index: mask bits', () => {
  const x = F.index(['a', 'z', '5', '/', '.', '-', '_', 'é', 'A']);
  assert.deepEqual(Array.from(x.mask), [1, 1 << 25, 1 << 26, 1 << 27, 1 << 28, 1 << 29, 1 << 29, 1 << 30, 1]);
});

test('search: rejects what is not a subsequence', () => {
  assert.deepEqual(names(top('zzz')), []);
  assert.deepEqual(names(top('gnirt')), [], 'order matters');
  assert.equal(top('zzz').all.length, 0);
  assert.deepEqual(names(top('')), []);
  assert.deepEqual(names(top('   ')), []);
  // The mask rejects a character no path has.
  assert.equal(top('q').all.length, 0);
});

test('search: ranking', () => {
  const r = names(top('tree'));
  assert.ok(r.indexOf('internal/adapters/web/tree.go') >= 0);
  assert.ok(r.indexOf('internal/adapters/web/tree.go') < r.indexOf('internal/adapters/web/static/css/30-list.css'),
    'basename match beats a scattered one: ' + r.join(', '));
  // Equal scores: the shorter path first, then the path.
  assert.ok(r.indexOf('internal/adapters/web/tree.go') < r.indexOf('internal/adapters/web/tree.templ'), r.join(', '));
  assert.ok(r.indexOf('internal/adapters/web/tree.templ') < r.indexOf('internal/adapters/web/tree_templ.go'), r.join(', '));
  assert.ok(r.indexOf('internal/adapters/web/tree.templ') < r.indexOf('src/deeply/nested/dir/with/a/treehouse.txt'), r.join(', '));
});

test('search: wbtree matches web/tree.templ', () => {
  const r = names(top('wbtree'));
  assert.ok(r.includes('internal/adapters/web/tree.templ'), r.join(', '));
  assert.ok(r.includes('internal/adapters/web/tree.go'));
});

test('search: basename prefix beats a deep match', () => {
  const x = F.index(['a/b/c/d/e/main_helper.go', 'x/m/a/i/n.go', 'main.go', 'pkg/domain/admin.go']);
  const r = F.search(x, 'main').hits.map((h) => x.paths[h.i]);
  assert.equal(r[0], 'main.go');
  assert.ok(r.indexOf('a/b/c/d/e/main_helper.go') < r.indexOf('x/m/a/i/n.go'), r.join(', '));
  assert.ok(r.indexOf('a/b/c/d/e/main_helper.go') < r.indexOf('pkg/domain/admin.go'), r.join(', '));
});

test('search: camelCase and separators earn bonuses', () => {
  const x = F.index(['pkg/fooBarBaz.go', 'pkg/foobarbaz.go', 'pkg/fxoxoxbxa.go']);
  const r = F.search(x, 'fbb').hits.map((h) => x.paths[h.i]);
  assert.equal(r[0], 'pkg/fooBarBaz.go', r.join(', '));
});

test('search: several terms', () => {
  const r = names(top('web tree'));
  assert.ok(r.length > 0);
  assert.ok(r.slice(0, 3).every((p) => p.includes('web') && p.includes('tree')), r.join(', '));
  assert.ok(!r.includes('README.md'));
  // Every term must match.
  assert.deepEqual(names(top('web zzz')), []);
  // The order of the terms is free.
  assert.deepEqual(new Set(names(top('tree web'))), new Set(names(top('web tree'))));
});

test('search: case is folded for ASCII only', () => {
  assert.ok(names(top('README')).includes('README.md'));
  assert.ok(names(top('readme')).includes('README.md'));
  const x = F.index(['İndex/Straße.go']);
  assert.equal(F.search(x, 'index').hits.length, 0, 'İ is not an ASCII capital I');
  assert.equal(F.search(x, 'ndex').hits.length, 1);
});

test('search: top k, ties by length then path', () => {
  const ps = [];
  for (let i = 0; i < 200; i++) ps.push('d/file' + i + '.txt');
  const x = F.index(ps);
  const r = F.search(x, 'file', 50);
  assert.equal(r.hits.length, 50);
  assert.equal(r.all.length, 200);
  const got = r.hits.map((h) => x.paths[h.i]);
  for (let i = 1; i < got.length; i++) {
    const a = got[i - 1];
    const b = got[i];
    assert.ok(a.length < b.length || (a.length === b.length && a < b), a + ' before ' + b);
  }
  assert.equal(got[0], 'd/file0.txt');
});

test('search: incremental results equal a cold search', () => {
  const big = [];
  let seed = 7;
  const rnd = () => (seed = (seed * 1103515245 + 12345) & 0x7fffffff) / 0x7fffffff;
  const words = ['web', 'tree', 'file', 'test', 'main', 'util', 'core', 'index', 'view', 'model'];
  for (let i = 0; i < 3000; i++) {
    const depth = 1 + Math.floor(rnd() * 4);
    const segs = [];
    for (let d = 0; d < depth; d++) segs.push(words[Math.floor(rnd() * words.length)] + (rnd() < 0.3 ? 'Case' : '') + Math.floor(rnd() * 20));
    big.push(segs.join('/') + ['.go', '.js', '.md'][Math.floor(rnd() * 3)]);
  }
  const x = F.index(big);
  const queries = ['t', 'tr', 'tre', 'tree', 'tree w', 'tree we', 'tree web', 'tre web', 'tree web m'];
  let prev = null;
  for (const q of queries) {
    const warm = F.search(x, q, 50, prev);
    const cold = F.search(x, q, 50, null);
    assert.deepEqual(warm.hits, cold.hits, q);
    assert.deepEqual(Array.from(warm.all), Array.from(cold.all), q);
    prev = warm;
  }
  // A query that does not extend the previous one searches everything.
  const a = F.search(x, 'tree', 50, null);
  assert.deepEqual(F.search(x, 'web', 50, a).hits, F.search(x, 'web', 50, null).hits);
  assert.deepEqual(F.search(x, 'tre', 50, a).hits, F.search(x, 'tre', 50, null).hits, 'shortening');
  // A result of another index is not a source of candidates.
  const y = F.index(['zzz']);
  assert.deepEqual(F.search(y, 'z', 50, a).hits.length, 1);
});

test('positions', () => {
  const p = 'internal/adapters/web/tree.go';
  const x = F.index([p]);
  assert.deepEqual(F.positions(x, 0, 'tree'), [p.indexOf('tree.go'), p.indexOf('tree.go') + 1, p.indexOf('tree.go') + 2, p.indexOf('tree.go') + 3]);
  const y = F.index(['internal/adapters/web/tree.templ']);
  const pos = F.positions(y, 0, 'wbtree');
  const s = y.paths[0];
  assert.equal(pos.map((i) => s[i]).join(''), 'wbtree');
  assert.deepEqual(pos, [...pos].sort((a, b) => a - b));
  assert.equal(pos[0], s.indexOf('web'), 'w of the web directory');
  // Two terms.
  const z = F.index(['src/web/tree.go']);
  const both = F.positions(z, 0, 'web tree');
  assert.equal(both.map((i) => z.paths[0][i]).join(''), 'webtree');
});

test('positions on a non-ASCII path index the original string', () => {
  const x = F.index(['İndex/Straße.go']);
  const pos = F.positions(x, 0, 'strasse');
  assert.deepEqual(pos, [], 'ß is not ss: nothing is claimed');
  const q = F.positions(x, 0, 'stra');
  const s = x.paths[0];
  assert.equal(q.map((i) => s[i]).join(''), 'Stra');
  const r = F.positions(x, 0, 'ndex');
  assert.equal(r.map((i) => s[i]).join(''), 'ndex');
  assert.equal(r[0], 1);
});

test('positions of a long path (greedy)', () => {
  const long = 'a/'.repeat(600) + 'target.txt';
  const x = F.index([long]);
  const r = F.search(x, 'target');
  assert.equal(r.hits.length, 1);
  const pos = F.positions(x, 0, 'target');
  assert.equal(pos.map((i) => long[i]).join(''), 'target');
});

test('visible', () => {
  assert.equal(F.visible('a\u202eb'), 'a<U+202E>b');
  assert.equal(F.visible('x\u200by'), 'x<U+200B>y');
  assert.equal(F.visible('a\u0007b'), 'a<U+0007>b');
  assert.equal(F.visible('\u3164'), '<U+3164>');
  assert.equal(F.visible('\u2028'), '<U+2028>');
  assert.equal(F.visible('plain/ünï.go'), 'plain/ünï.go');
  assert.equal(F.visible('tab\tname'), 'tab<U+0009>name', 'the finder shows a control character; Go refuses such paths anyway');
});

test('performance on 100,000 paths (AC6)', () => {
  const dirs = ['internal', 'cmd', 'pkg', 'src', 'lib', 'docs', 'test', 'web', 'adapters', 'domain', 'app', 'static'];
  const names = ['tree', 'blob', 'commit', 'diff', 'index', 'main', 'util', 'handler', 'service', 'reader', 'writer', 'config'];
  const exts = ['.go', '.js', '.md', '.templ', '.css', '.txt'];
  const paths = [];
  for (let i = 0; paths.length < 100000; i++) {
    const d = [dirs[i % 12], dirs[(i * 7) % 12], dirs[(i * 13 + 5) % 12], 'pkg' + (i % 311)];
    paths.push(d.join('/') + '/' + names[(i * 5) % 12] + '_' + (i % 977) + names[(i >> 3) % 12] + exts[i % 6]);
  }
  let t0 = performance.now();
  const x = F.index(paths);
  const build = performance.now() - t0;

  const timings = [];
  let prev = null;
  let worst = 0;
  for (const q of ['t', 'tr', 'tre', 'tree', 'tree h', 'wbtree', 'internal handler', 'zzzq']) {
    t0 = performance.now();
    const r = F.search(x, q, 50, prev);
    const ms = performance.now() - t0;
    worst = Math.max(worst, ms);
    timings.push(q + ': ' + ms.toFixed(1) + ' ms (' + r.all.length + ' matches)');
    prev = r;
  }
  // The cold query of a single character is the worst case: every path matches.
  t0 = performance.now();
  F.search(x, 'e');
  const cold = performance.now() - t0;
  t0 = performance.now();
  F.search(x, 'inthand');
  const cold2 = performance.now() - t0;
  t0 = performance.now();
  const r = F.search(x, 'tree h');
  for (const h of r.hits) F.positions(x, h.i, 'tree h');
  const withPos = performance.now() - t0;
  console.log(`AC6 index of ${paths.length} paths: ${build.toFixed(1)} ms (target < 150, asserted < 1500)`);
  console.log(`AC6 queries (incremental): ${timings.join('; ')}`);
  console.log(`AC6 cold 'e': ${cold.toFixed(1)} ms; cold 'inthand': ${cold2.toFixed(1)} ms; 'tree h' with positions of the hits: ${withPos.toFixed(1)} ms (target < 30, asserted < 300)`);
  assert.ok(build < 1500, 'index ' + build);
  assert.ok(Math.max(worst, cold, cold2, withPos) < 300, 'query ' + Math.max(worst, cold, cold2, withPos));
});

// ---- a brute-force reference of the scoring --------------------------------

// The reference tries every placement of the term in the path and scores it by
// the documented rules; the DP of files.js must find the same best score.
function refBonus(orig, j) {
  if (j === 0) return 10;
  const p = orig[j - 1];
  const c = orig[j];
  if (p === '/') return 10;
  if ('_-. '.includes(p)) return 8;
  const upper = c >= 'A' && c <= 'Z';
  const lower = c >= 'a' && c <= 'z';
  if (p >= 'a' && p <= 'z' && upper) return 7;
  if (p >= '0' && p <= '9' && (upper || lower)) return 7;
  return 0;
}

function refPlacement(orig, pos) {
  let score = 0;
  pos.forEach((j, i) => {
    const b = refBonus(orig, j);
    score += 16 + (i === 0 ? 2 * b : b);
    if (i > 0) {
      const gap = j - pos[i - 1] - 1;
      score += gap === 0 ? Math.max(b, 4) : -(gap + 2);
    }
  });
  return score;
}

// refBest is the best score of the placements of t at or after from (null: none).
function refBest(orig, lower, t, from) {
  let best = null;
  const go = (i, start, pos) => {
    if (i === t.length) {
      const sc = refPlacement(orig, pos);
      if (best === null || sc > best) best = sc;
      return;
    }
    for (let j = start; j < lower.length; j++) if (lower[j] === t[i]) go(i + 1, j + 1, [...pos, j]);
  };
  go(0, from, []);
  return best;
}

function refTerm(orig, t, isLast) {
  const lower = orig.replace(/[A-Z]/g, (c) => c.toLowerCase());
  const all = refBest(orig, lower, t, 0);
  if (all === null) return null;
  if (!isLast) return all;
  const base = orig.lastIndexOf('/') + 1;
  let best = all;
  if (base === 0) best += 24;
  else {
    const r = refBest(orig, lower, t, base);
    if (r !== null && r + 24 > best) best = r + 24;
  }
  if (lower.startsWith(t, base)) best += 16;
  return best;
}

test('search and positions agree with a brute-force reference', () => {
  let seed = 12345;
  const rnd = (n) => {
    seed = (Math.imul(seed, 1103515245) + 12345) & 0x7fffffff;
    return Math.floor((seed / 0x80000000) * n);
  };
  const alphabet = 'abcabcABC//..__-1';
  const paths = new Set();
  while (paths.size < 3000) {
    let p = '';
    for (let n = 1 + rnd(14); n > 0; n--) p += alphabet[rnd(alphabet.length)];
    paths.add(p);
  }
  const list = Array.from(paths);
  const x = F.index(list);
  const queries = [];
  for (let n = 0; n < 40; n++) {
    let q = '';
    for (let k = 1 + rnd(3); k > 0; k--) q += 'abc/._-1'[rnd(8)];
    queries.push(q);
  }
  for (let n = 0; n < 20; n++) queries.push('ab'[rnd(2)] + 'c/.'[rnd(3)] + ' ' + 'abc'[rnd(3)] + 'a_'[rnd(2)]);
  let compared = 0;
  for (const q of queries) {
    const terms = q.split(/\s+/).filter(Boolean);
    const res = F.search(x, q, list.length);
    const got = new Map(res.hits.map((h) => [h.i, h.score]));
    for (let i = 0; i < list.length; i++) {
      let total = 0;
      for (let n = 0; n < terms.length && total !== null; n++) {
        const sc = refTerm(list[i], terms[n], n === terms.length - 1);
        total = sc === null ? null : total + sc;
      }
      if (total === null) {
        assert.ok(!got.has(i), `${q}: ${list[i]} cannot match`);
        continue;
      }
      assert.equal(got.get(i), total, `${q}: score of ${list[i]}`);
      compared++;
      if (terms.length === 1) {
        // The positions are a placement of the term that scores the best.
        const pos = F.positions(x, i, q);
        assert.equal(pos.map((j) => list[i][j]).join('').toLowerCase(), terms[0], `${q}: characters of ${list[i]}`);
        const lower = list[i].toLowerCase();
        const base = list[i].lastIndexOf('/') + 1;
        const all = refBest(list[i], lower, terms[0], 0);
        const inBase = base === 0 ? null : refBest(list[i], lower, terms[0], base);
        const want = inBase !== null && inBase + 24 > all ? inBase : all;
        assert.equal(refPlacement(list[i], pos), want, `${q}: placement of ${list[i]}`);
      }
    }
  }
  assert.ok(compared > 2000, 'compared ' + compared);
});
