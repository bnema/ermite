// pow-worker.js: the proof of work of the public issue form, in a dedicated
// Worker (started by issue-form.js). It finds the smallest decimal counter n
// such that SHA-256(nonce bytes || ":" || n) has at least `difficulty` leading
// zero bits: the very check of the server (usecase/issue_submissions.go,
// powHash). The nonce arrives as base64url without padding, like the server
// sends it. Messages in: {nonce, difficulty}. Messages out:
// {progress: tried} now and then, then {solution: "<decimal>"} or {error}.
//
// window-less on purpose: ermitePow is also loaded by Node for the tests
// (jstest/issue-form.test.mjs).
(() => {
  const BATCH = 2048;
  const enc = new TextEncoder();

  // leadingZeroBits counts the zero bits at the start of the bytes.
  function leadingZeroBits(bytes) {
    let n = 0;
    for (const b of bytes) {
      if (b === 0) { n += 8; continue; }
      return n + Math.clz32(b) - 24;
    }
    return n;
  }

  // b64urlDecode turns unpadded base64url into bytes.
  function b64urlDecode(s) {
    const bin = atob(String(s).replace(/-/g, '+').replace(/_/g, '/'));
    const out = new Uint8Array(bin.length);
    for (let i = 0; i < bin.length; i++) out[i] = bin.charCodeAt(i);
    return out;
  }

  // input is nonce || ":" || decimal(n), the bytes the server hashes.
  function input(nonce, n) {
    const tail = enc.encode(':' + n);
    const out = new Uint8Array(nonce.length + tail.length);
    out.set(nonce, 0);
    out.set(tail, nonce.length);
    return out;
  }

  // solve returns the smallest solution as a decimal string. digest is
  // (Uint8Array) => Promise<ArrayBuffer-like>, crypto.subtle.digest by default.
  // Hashes are asked a batch at a time, in order, so the first hit of the first
  // batch that has one is the smallest. onProgress(tried) is called after each
  // batch; it may return true to stop (solve then returns null).
  async function solve(nonce, difficulty, { digest, batch = BATCH, onProgress } = {}) {
    const dig = digest || ((d) => globalThis.crypto.subtle.digest('SHA-256', d));
    for (let start = 0; ; start += batch) {
      const sums = await Promise.all(Array.from({ length: batch }, async (_, i) => new Uint8Array(await dig(input(nonce, start + i)))));
      for (let i = 0; i < batch; i++) {
        if (leadingZeroBits(sums[i]) >= difficulty) return String(start + i);
      }
      if (onProgress && onProgress(start + batch)) return null;
    }
  }

  globalThis.ermitePow = { leadingZeroBits, b64urlDecode, input, solve };

  if (typeof WorkerGlobalScope !== 'undefined' && typeof globalThis.postMessage === 'function') {
    globalThis.onmessage = async (e) => {
      try {
        if (!globalThis.crypto || !globalThis.crypto.subtle) { postMessage({ error: 'nocrypto' }); return; }
        const { nonce, difficulty } = e.data;
        const solution = await solve(b64urlDecode(nonce), difficulty, { onProgress: (tried) => { postMessage({ progress: tried }); } });
        postMessage({ solution });
      } catch (err) {
        postMessage({ error: String(err && err.message || err) });
      }
    };
  }
})();
