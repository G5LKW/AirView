// net/qr.js - a small QR Code encoder (ISO/IEC 18004, byte mode, versions 1-40, error correction L/M/Q/H)
// for the authenticator enrolment (otpauth:// URIs) and enrolment-token hand-over. Written from the
// standard's description; no third-party code. No DOM: qrMatrix() is unit-tested in node, qrSvg() returns
// markup that the pages insert.
//
//   import { qrSvg, qrMatrix } from '../net/qr.js';
//   el.innerHTML = qrSvg('otpauth://totp/...', { ecl: 'M', scale: 4, margin: 4 });
//   const { size, version, modules } = qrMatrix(text);    // modules[y][x] === true -> dark

// error correction codewords per block and number of blocks, indexed [ecl][version] (index 0 unused)
const ECC_PER_BLOCK = {
  L: [-1, 7, 10, 15, 20, 26, 18, 20, 24, 30, 18, 20, 24, 26, 30, 22, 24, 28, 30, 28, 28, 28, 28, 30, 30, 26, 28, 30, 30, 30, 30, 30, 30, 30, 30, 30, 30, 30, 30, 30, 30],
  M: [-1, 10, 16, 26, 18, 24, 16, 18, 22, 22, 26, 30, 22, 22, 24, 24, 28, 28, 26, 26, 26, 26, 28, 28, 28, 28, 28, 28, 28, 28, 28, 28, 28, 28, 28, 28, 28, 28, 28, 28, 28],
  Q: [-1, 13, 22, 18, 26, 18, 24, 18, 22, 20, 24, 28, 26, 24, 20, 30, 24, 28, 28, 26, 30, 28, 30, 30, 30, 30, 28, 30, 30, 30, 30, 30, 30, 30, 30, 30, 30, 30, 30, 30, 30],
  H: [-1, 17, 28, 22, 16, 22, 28, 26, 26, 24, 28, 24, 28, 22, 24, 24, 30, 28, 28, 26, 28, 30, 24, 30, 30, 30, 30, 30, 30, 30, 30, 30, 30, 30, 30, 30, 30, 30, 30, 30, 30],
};
const NUM_BLOCKS = {
  L: [-1, 1, 1, 1, 1, 1, 2, 2, 2, 2, 4, 4, 4, 4, 4, 6, 6, 6, 6, 7, 8, 8, 9, 9, 10, 12, 12, 12, 13, 14, 15, 16, 17, 18, 19, 19, 20, 21, 22, 24, 25],
  M: [-1, 1, 1, 1, 2, 2, 4, 4, 4, 5, 5, 5, 8, 9, 9, 10, 10, 11, 13, 14, 16, 17, 17, 18, 20, 21, 23, 25, 26, 28, 29, 31, 33, 35, 37, 38, 40, 43, 45, 47, 49],
  Q: [-1, 1, 1, 2, 2, 4, 4, 6, 6, 8, 8, 8, 10, 12, 16, 12, 17, 16, 18, 21, 20, 23, 23, 25, 27, 29, 34, 34, 35, 38, 40, 43, 45, 48, 51, 53, 56, 59, 62, 65, 68],
  H: [-1, 1, 1, 2, 4, 4, 4, 5, 6, 8, 8, 11, 11, 16, 16, 18, 16, 19, 21, 25, 25, 25, 34, 30, 32, 35, 37, 40, 42, 45, 48, 51, 54, 57, 60, 63, 66, 70, 74, 77, 81],
};
/** Format-information bits of each level (the standard's two-bit codes). */
export const ECL_BITS = { L: 1, M: 0, Q: 3, H: 2 };

/** Number of modules available for data and error correction (everything but function patterns). */
export function rawDataModules(ver) {
  let n = (16 * ver + 128) * ver + 64;
  if (ver >= 2) {
    const align = Math.floor(ver / 7) + 2;
    n -= (25 * align - 10) * align - 55;
    if (ver >= 7) n -= 36;
  }
  return n;
}

/** Data codewords of a version / level. */
export function dataCodewords(ver, ecl) {
  return Math.floor(rawDataModules(ver) / 8) - ECC_PER_BLOCK[ecl][ver] * NUM_BLOCKS[ecl][ver];
}

/** Centre coordinates of the alignment patterns of a version (rows and columns use the same list). */
export function alignmentPositions(ver) {
  if (ver === 1) return [];
  const size = ver * 4 + 17;
  const n = Math.floor(ver / 7) + 2;
  const step = ver === 32 ? 26 : Math.ceil((ver * 4 + 4) / (n * 2 - 2)) * 2;
  const out = [6];
  for (let pos = size - 7; out.length < n; pos -= step) out.splice(1, 0, pos);
  return out;
}

// ---- GF(256) with the QR polynomial x^8 + x^4 + x^3 + x^2 + 1 ----
export function gfMul(x, y) {
  let z = 0;
  for (let i = 7; i >= 0; i--) {
    z = (z << 1) ^ ((z >>> 7) * 0x11d);
    z ^= ((y >>> i) & 1) * x;
  }
  return z & 0xff;
}

/** Generator polynomial of the given degree (coefficients, highest power first, leading 1 omitted). */
function rsDivisor(degree) {
  const r = new Array(degree).fill(0);
  r[degree - 1] = 1;
  let root = 1;
  for (let i = 0; i < degree; i++) {
    for (let j = 0; j < r.length; j++) {
      r[j] = gfMul(r[j], root);
      if (j + 1 < r.length) r[j] ^= r[j + 1];
    }
    root = gfMul(root, 0x02);
  }
  return r;
}

/** Reed-Solomon error-correction codewords of `data` for a divisor. */
export function rsRemainder(data, divisor) {
  const r = new Array(divisor.length).fill(0);
  for (const b of data) {
    const f = b ^ r.shift();
    r.push(0);
    for (let i = 0; i < divisor.length; i++) r[i] ^= gfMul(divisor[i], f);
  }
  return r;
}

/** UTF-8 bytes of a string. */
function utf8(text) {
  if (typeof TextEncoder !== 'undefined') return Array.from(new TextEncoder().encode(text));
  return Array.from(unescape(encodeURIComponent(text)), (c) => c.charCodeAt(0));
}

/** The smallest version that holds `n` bytes in byte mode at a level, or 0 when none does. */
export function fitVersion(n, ecl, minVer = 1) {
  for (let v = Math.max(1, minVer); v <= 40; v++) {
    const bits = 4 + (v <= 9 ? 8 : 16) + 8 * n;
    if (bits <= dataCodewords(v, ecl) * 8) return v;
  }
  return 0;
}

/** Data codewords (mode, count, payload, terminator, padding) for a version. */
function encodeData(bytes, ver, ecl) {
  const bits = [];
  const put = (val, len) => { for (let i = len - 1; i >= 0; i--) bits.push((val >>> i) & 1); };
  put(0b0100, 4);
  put(bytes.length, ver <= 9 ? 8 : 16);
  for (const b of bytes) put(b, 8);
  const cap = dataCodewords(ver, ecl) * 8;
  put(0, Math.min(4, cap - bits.length));
  put(0, (8 - (bits.length % 8)) % 8);
  for (let pad = 0xec; bits.length < cap; pad ^= 0xec ^ 0x11) put(pad, 8);
  const out = [];
  for (let i = 0; i < bits.length; i += 8) {
    let b = 0;
    for (let j = 0; j < 8; j++) b = (b << 1) | bits[i + j];
    out.push(b);
  }
  return out;
}

/** Split into blocks, add error correction and interleave (final codeword sequence). */
export function interleave(data, ver, ecl) {
  const nBlocks = NUM_BLOCKS[ecl][ver];
  const eccLen = ECC_PER_BLOCK[ecl][ver];
  const raw = Math.floor(rawDataModules(ver) / 8);
  const nShort = nBlocks - (raw % nBlocks);
  const shortLen = Math.floor(raw / nBlocks);
  const div = rsDivisor(eccLen);
  const blocks = [];
  for (let i = 0, k = 0; i < nBlocks; i++) {
    const len = shortLen - eccLen + (i < nShort ? 0 : 1);
    const dat = data.slice(k, k + len);
    k += len;
    const ecc = rsRemainder(dat, div);
    if (i < nShort) dat.push(0);
    blocks.push(dat.concat(ecc));
  }
  const out = [];
  for (let i = 0; i < blocks[0].length; i++) {
    for (let j = 0; j < blocks.length; j++) {
      if (i !== shortLen - eccLen || j >= nShort) out.push(blocks[j][i]);
    }
  }
  return out;
}

const MASKS = [
  (x, y) => (x + y) % 2 === 0,
  (x, y) => y % 2 === 0,
  (x) => x % 3 === 0,
  (x, y) => (x + y) % 3 === 0,
  (x, y) => (Math.floor(x / 3) + Math.floor(y / 2)) % 2 === 0,
  (x, y) => ((x * y) % 2) + ((x * y) % 3) === 0,
  (x, y) => (((x * y) % 2) + ((x * y) % 3)) % 2 === 0,
  (x, y) => (((x + y) % 2) + ((x * y) % 3)) % 2 === 0,
];

/** 15 format bits (level + mask, BCH protected, XOR-masked). */
export function formatBits(ecl, mask) {
  const data = (ECL_BITS[ecl] << 3) | mask;
  let rem = data;
  for (let i = 0; i < 10; i++) rem = (rem << 1) ^ ((rem >>> 9) * 0x537);
  return ((data << 10) | rem) ^ 0x5412;
}

/** 18 version bits (versions 7+). */
export function versionBits(ver) {
  let rem = ver;
  for (let i = 0; i < 12; i++) rem = (rem << 1) ^ ((rem >>> 11) * 0x1f25);
  return (ver << 12) | rem;
}

function grid(size, v) { return Array.from({ length: size }, () => new Array(size).fill(v)); }

/** Draw function patterns; returns { modules, fn } (fn[y][x] = reserved). */
function functionPatterns(ver) {
  const size = ver * 4 + 17;
  const m = grid(size, false);
  const fn = grid(size, false);
  const set = (x, y, dark) => { m[y][x] = dark; fn[y][x] = true; };
  for (let i = 0; i < size; i++) { set(6, i, i % 2 === 0); set(i, 6, i % 2 === 0); }
  const finder = (cx, cy) => {
    for (let dy = -4; dy <= 4; dy++) {
      for (let dx = -4; dx <= 4; dx++) {
        const x = cx + dx;
        const y = cy + dy;
        if (x < 0 || y < 0 || x >= size || y >= size) continue;
        const d = Math.max(Math.abs(dx), Math.abs(dy));
        set(x, y, d !== 2 && d !== 4);
      }
    }
  };
  finder(3, 3); finder(size - 4, 3); finder(3, size - 4);
  const al = alignmentPositions(ver);
  const last = al.length - 1;
  for (let i = 0; i < al.length; i++) {
    for (let j = 0; j < al.length; j++) {
      if ((i === 0 && j === 0) || (i === 0 && j === last) || (i === last && j === 0)) continue;
      for (let dy = -2; dy <= 2; dy++) for (let dx = -2; dx <= 2; dx++) set(al[i] + dx, al[j] + dy, Math.max(Math.abs(dx), Math.abs(dy)) !== 1);
    }
  }
  drawFormat(m, fn, size, 0); // reserve (real bits later)
  if (ver >= 7) {
    const bits = versionBits(ver);
    for (let i = 0; i < 18; i++) {
      const dark = ((bits >>> i) & 1) === 1;
      const a = size - 11 + (i % 3);
      const b = Math.floor(i / 3);
      set(a, b, dark);
      set(b, a, dark);
    }
  }
  return { m, fn, size };
}

function drawFormat(m, fn, size, bits) {
  const set = (x, y, dark) => { m[y][x] = dark; fn[y][x] = true; };
  const bit = (i) => ((bits >>> i) & 1) === 1;
  for (let i = 0; i <= 5; i++) set(8, i, bit(i));
  set(8, 7, bit(6));
  set(8, 8, bit(7));
  set(7, 8, bit(8));
  for (let i = 9; i < 15; i++) set(14 - i, 8, bit(i));
  for (let i = 0; i < 8; i++) set(size - 1 - i, 8, bit(i));
  for (let i = 8; i < 15; i++) set(8, size - 15 + i, bit(i));
  set(8, size - 8, true); // the always-dark module
}

function placeCodewords(m, fn, size, cw) {
  let i = 0;
  const total = cw.length * 8;
  for (let right = size - 1; right >= 1; right -= 2) {
    if (right === 6) right = 5;
    const upward = ((right + 1) & 2) === 0;
    for (let v = 0; v < size; v++) {
      const y = upward ? size - 1 - v : v;
      for (let j = 0; j < 2; j++) {
        const x = right - j;
        if (fn[y][x]) continue;
        if (i < total) { m[y][x] = ((cw[i >>> 3] >>> (7 - (i & 7))) & 1) === 1; i++; }
      }
    }
  }
}

function applyMask(m, fn, size, k) {
  const f = MASKS[k];
  for (let y = 0; y < size; y++) for (let x = 0; x < size; x++) if (!fn[y][x] && f(x, y)) m[y][x] = !m[y][x];
}

/** Penalty score of a finished symbol (the standard's four rules), used to pick the mask. */
export function penalty(m) {
  const size = m.length;
  let score = 0;
  // rule 1: runs of 5+ equal modules in rows and columns
  for (let pass = 0; pass < 2; pass++) {
    for (let a = 0; a < size; a++) {
      let run = 1;
      for (let b = 1; b < size; b++) {
        const cur = pass ? m[b][a] : m[a][b];
        const prev = pass ? m[b - 1][a] : m[a][b - 1];
        if (cur === prev) { run++; if (run === 5) score += 3; else if (run > 5) score += 1; } else run = 1;
      }
    }
  }
  // rule 2: 2x2 blocks of one colour
  for (let y = 0; y < size - 1; y++) for (let x = 0; x < size - 1; x++) {
    const c = m[y][x];
    if (c === m[y][x + 1] && c === m[y + 1][x] && c === m[y + 1][x + 1]) score += 3;
  }
  // rule 3: 1:1:3:1:1 finder-like patterns with 4 light modules on one side
  const pat = [true, false, true, true, true, false, true];
  const at = (pass, a, b) => (b < 0 || b >= size ? false : pass ? m[b][a] : m[a][b]);
  for (let pass = 0; pass < 2; pass++) {
    for (let a = 0; a < size; a++) {
      for (let b = 0; b + 7 <= size; b++) {
        let ok = true;
        for (let k = 0; k < 7 && ok; k++) if (at(pass, a, b + k) !== pat[k]) ok = false;
        if (!ok) continue;
        let before = true;
        let after = true;
        for (let k = 1; k <= 4; k++) { if (at(pass, a, b - k)) before = false; if (at(pass, a, b + 6 + k)) after = false; }
        if (before || after) score += 40;
      }
    }
  }
  // rule 4: balance of dark modules
  let dark = 0;
  for (const row of m) for (const v of row) if (v) dark++;
  const total = size * size;
  const k = Math.ceil(Math.abs(dark * 20 - total * 10) / total) - 1;
  score += Math.max(0, k) * 10;
  return score;
}

/**
 * Encode text as a QR symbol.
 * @param {string} text
 * @param {{ecl?: 'L'|'M'|'Q'|'H', minVersion?: number, mask?: number}} [opts]
 * @returns {{size: number, version: number, ecl: string, mask: number, modules: boolean[][]}}
 */
export function qrMatrix(text, opts = {}) {
  const ecl = ECC_PER_BLOCK[opts.ecl] ? opts.ecl : 'M';
  const bytes = utf8(String(text ?? ''));
  const ver = fitVersion(bytes.length, ecl, opts.minVersion || 1);
  if (!ver) throw new Error('Text too long for a QR code');
  const cw = interleave(encodeData(bytes, ver, ecl), ver, ecl);
  let best = null;
  const masks = Number.isInteger(opts.mask) && opts.mask >= 0 && opts.mask < 8 ? [opts.mask] : [0, 1, 2, 3, 4, 5, 6, 7];
  for (const k of masks) {
    const { m, fn, size } = functionPatterns(ver);
    placeCodewords(m, fn, size, cw);
    applyMask(m, fn, size, k);
    drawFormat(m, fn, size, formatBits(ecl, k));
    const p = masks.length > 1 ? penalty(m) : 0;
    if (!best || p < best.p) best = { p, m, size, k };
  }
  return { size: best.size, version: ver, ecl, mask: best.k, modules: best.m };
}

/**
 * SVG markup of a QR symbol (dark modules as one path; light background with a quiet zone).
 * @param {string} text
 * @param {{ecl?: string, scale?: number, margin?: number, dark?: string, light?: string, title?: string}} [opts]
 */
export function qrSvg(text, opts = {}) {
  const q = qrMatrix(text, opts);
  const margin = Number.isFinite(opts.margin) ? opts.margin : 4;
  const scale = opts.scale || 4;
  const dim = q.size + margin * 2;
  let d = '';
  for (let y = 0; y < q.size; y++) for (let x = 0; x < q.size; x++) if (q.modules[y][x]) d += `M${x + margin} ${y + margin}h1v1h-1z`;
  const esc = (s) => String(s).replace(/[&<>"]/g, (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;' }[c]));
  return `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 ${dim} ${dim}" width="${dim * scale}" height="${dim * scale}" shape-rendering="crispEdges" role="img" aria-label="${esc(opts.title || 'QR code')}">`
    + `<rect width="${dim}" height="${dim}" fill="${esc(opts.light || '#ffffff')}"/><path d="${d}" fill="${esc(opts.dark || '#000000')}"/></svg>`;
}

export default { qrMatrix, qrSvg };
