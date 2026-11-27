// QR encoder tests: every symbol is read back by an independent decoder written here from the standard
// (function-pattern map, zig-zag order, unmasking, block de-interleaving, Reed-Solomon syndromes via
// log/antilog tables, byte-mode parsing). Run: node --test web/dev/test
import test from 'node:test';
import assert from 'node:assert/strict';
import { qrMatrix, qrSvg, formatBits, versionBits, alignmentPositions, dataCodewords } from '../../static/js/net/qr.js';

// ---- GF(256) by tables (independent of the encoder's shift-and-add multiply) ----
const EXP = new Array(512);
const LOG = new Array(256);
{
  let x = 1;
  for (let i = 0; i < 255; i++) { EXP[i] = x; LOG[x] = i; x <<= 1; if (x & 0x100) x ^= 0x11d; }
  for (let i = 255; i < 512; i++) EXP[i] = EXP[i - 255];
}
const mul = (a, b) => (a && b ? EXP[LOG[a] + LOG[b]] : 0);

// Block structure for level M, versions 1-10 (ISO/IEC 18004 table 9): [count, dataCodewords] groups, ecc per block.
const BLOCKS_M = {
  1: { ecc: 10, groups: [[1, 16]] }, 2: { ecc: 16, groups: [[1, 28]] }, 3: { ecc: 26, groups: [[1, 44]] },
  4: { ecc: 18, groups: [[2, 32]] }, 5: { ecc: 24, groups: [[2, 43]] }, 6: { ecc: 16, groups: [[4, 27]] },
  7: { ecc: 18, groups: [[4, 31]] }, 8: { ecc: 22, groups: [[2, 38], [2, 39]] }, 9: { ecc: 22, groups: [[3, 36], [2, 37]] },
  10: { ecc: 26, groups: [[4, 43], [1, 44]] },
};
const ALIGN = { 1: [], 2: [6, 18], 3: [6, 22], 4: [6, 26], 5: [6, 30], 6: [6, 34], 7: [6, 22, 38], 8: [6, 24, 42], 9: [6, 26, 46], 10: [6, 28, 50] };

/** All 32 valid format words, computed by polynomial long division (generator 10100110111). */
function validFormats() {
  const out = new Map();
  for (let d = 0; d < 32; d++) {
    let v = d << 10;
    for (let b = 14; b >= 10; b--) if (v & (1 << b)) v ^= 0b10100110111 << (b - 10);
    out.set(((d << 10) | v) ^ 0b101010000010010, d);
  }
  return out;
}

function isFunction(ver, size, r, c) {
  const inFinder = (r0, c0) => r >= r0 && r < r0 + 8 && c >= c0 && c < c0 + 8;
  if (inFinder(0, 0) || inFinder(0, size - 8) || inFinder(size - 8, 0)) return true;
  if (r === 6 || c === 6) return true;
  // format areas (row 8 / column 8 next to the finders) and the dark module
  if (r === 8 && (c <= 8 || c >= size - 8)) return true;
  if (c === 8 && (r <= 8 || r >= size - 8)) return true;
  if (ver >= 7 && ((r < 6 && c >= size - 11 && c < size - 8) || (c < 6 && r >= size - 11 && r < size - 8))) return true;
  const al = ALIGN[ver];
  for (const ar of al) for (const ac of al) {
    if ((ar === 6 && ac === 6) || (ar === 6 && ac === al[al.length - 1]) || (ar === al[al.length - 1] && ac === 6)) continue;
    if (Math.abs(r - ar) <= 2 && Math.abs(c - ac) <= 2) return true;
  }
  return false;
}

const MASK = [
  (i, j) => (i + j) % 2 === 0, (i) => i % 2 === 0, (i, j) => j % 3 === 0, (i, j) => (i + j) % 3 === 0,
  (i, j) => (Math.floor(i / 2) + Math.floor(j / 3)) % 2 === 0, (i, j) => ((i * j) % 2) + ((i * j) % 3) === 0,
  (i, j) => (((i * j) % 2) + ((i * j) % 3)) % 2 === 0, (i, j) => (((i * j) % 3) + ((i + j) % 2)) % 2 === 0,
];

/** Decode a symbol produced by qrMatrix; throws on any inconsistency. */
function decode(q) {
  const m = q.modules;
  const size = m.length;
  const ver = (size - 17) / 4;
  assert.ok(Number.isInteger(ver) && ver >= 1 && ver <= 10, 'size');
  // format information, both copies
  const b1 = [[0, 8], [1, 8], [2, 8], [3, 8], [4, 8], [5, 8], [7, 8], [8, 8], [8, 7], [8, 5], [8, 4], [8, 3], [8, 2], [8, 1], [8, 0]];
  let f1 = 0; b1.forEach(([r, c], i) => { if (m[r][c]) f1 |= 1 << i; });
  let f2 = 0;
  for (let i = 0; i < 8; i++) if (m[8][size - 1 - i]) f2 |= 1 << i;
  for (let i = 8; i < 15; i++) if (m[size - 15 + i][8]) f2 |= 1 << i;
  assert.equal(f1, f2, 'format copies differ');
  const fmt = validFormats().get(f1);
  assert.notEqual(fmt, undefined, 'format word not a valid BCH code word');
  assert.ok(m[size - 8][8], 'dark module');
  const level = ['M', 'L', 'H', 'Q'][fmt >> 3];
  const mask = fmt & 7;
  // finder centre and timing
  for (const [r, c] of [[3, 3], [3, size - 4], [size - 4, 3]]) { assert.ok(m[r][c] && !m[r - 2][c] && m[r - 3][c], 'finder'); assert.ok(!m[r][c + (c < 8 ? 4 : -4)], 'separator'); }
  for (let i = 8; i < size - 8; i++) assert.equal(m[6][i], i % 2 === 0, 'timing row');
  // read the data bits in the zig-zag order
  const bits = [];
  let up = true;
  for (let col = size - 1; col > 0; col -= 2) {
    if (col === 6) col--;
    for (let k = 0; k < size; k++) {
      const r = up ? size - 1 - k : k;
      for (const c of [col, col - 1]) {
        if (isFunction(ver, size, r, c)) continue;
        bits.push(m[r][c] !== MASK[mask](r, c) ? 1 : 0);
      }
    }
    up = !up;
  }
  const bytes = [];
  for (let i = 0; i + 8 <= bits.length; i += 8) bytes.push(bits.slice(i, i + 8).reduce((a, b) => (a << 1) | b, 0));
  assert.equal(level, 'M', 'tests use level M');
  const spec = BLOCKS_M[ver];
  const blocks = [];
  for (const [n, d] of spec.groups) for (let i = 0; i < n; i++) blocks.push({ d, data: [], ecc: [] });
  const maxD = Math.max(...blocks.map((b) => b.d));
  let p = 0;
  for (let i = 0; i < maxD; i++) for (const b of blocks) if (i < b.d) b.data.push(bytes[p++]);
  for (let i = 0; i < spec.ecc; i++) for (const b of blocks) b.ecc.push(bytes[p++]);
  // syndromes: the codeword polynomial vanishes at alpha^0 .. alpha^(ecc-1)
  for (const b of blocks) {
    const cw = b.data.concat(b.ecc);
    for (let s = 0; s < spec.ecc; s++) {
      let acc = 0;
      for (const c of cw) acc = mul(acc, EXP[s]) ^ c;
      assert.equal(acc, 0, `syndrome ${s} not zero`);
    }
  }
  const data = blocks.flatMap((b) => b.data);
  const dbits = data.flatMap((b) => [7, 6, 5, 4, 3, 2, 1, 0].map((k) => (b >> k) & 1));
  const take = (n) => { let v = 0; for (let i = 0; i < n; i++) v = (v << 1) | dbits.shift(); return v; };
  assert.equal(take(4), 4, 'byte mode');
  const len = take(ver <= 9 ? 8 : 16);
  const out = [];
  for (let i = 0; i < len; i++) out.push(take(8));
  return { text: new TextDecoder().decode(new Uint8Array(out)), ver, mask, level };
}

test('round trip of typical authenticator URIs and short texts', () => {
  const samples = [
    'HELLO',
    'otpauth://totp/AirView%20Network:alice?secret=JBSWY3DPEHPK3PXPJBSWY3DPEHPK3PXP&issuer=AirView%20Network&algorithm=SHA1&digits=6&period=30',
    'otpauth://totp/AirView:admin?secret=GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ&issuer=AirView',
    'https://example.org/claim?code=ABCD2345',
    'Grüße ✈ 1234',
  ];
  for (const s of samples) {
    const q = qrMatrix(s, { ecl: 'M' });
    const r = decode(q);
    assert.equal(r.text, s);
    assert.equal(r.ver, q.version);
    assert.equal(r.mask, q.mask);
  }
});

test('every mask decodes', () => {
  for (let k = 0; k < 8; k++) {
    const q = qrMatrix('mask test ' + k, { ecl: 'M', mask: k });
    assert.equal(q.mask, k);
    assert.equal(decode(q).text, 'mask test ' + k);
  }
});

test('every version 1-10 at level M decodes', () => {
  for (let v = 1; v <= 10; v++) {
    const cap = dataCodewords(v, 'M') - (v <= 9 ? 2 : 3); // mode + count bits
    const s = 'x'.repeat(cap).slice(0, Math.max(1, cap));
    const q = qrMatrix(s, { ecl: 'M', minVersion: v });
    assert.equal(q.version, v);
    assert.equal(decode(q).text, s);
  }
});

test('tables agree with the standard', () => {
  assert.deepEqual(alignmentPositions(7), [6, 22, 38]);
  assert.deepEqual(alignmentPositions(10), [6, 28, 50]);
  assert.deepEqual(alignmentPositions(32), [6, 34, 60, 86, 112, 138]);
  assert.equal(dataCodewords(1, 'M'), 16);
  assert.equal(dataCodewords(40, 'L'), 2956);
  assert.equal(versionBits(7), 0x07c94);
  assert.equal(formatBits('M', 0), 0x5412);
  assert.ok(validFormats().has(formatBits('L', 4)) && validFormats().has(formatBits('H', 7)));
});

test('svg output', () => {
  const svg = qrSvg('abc', { title: 'a "q" <code>' });
  assert.match(svg, /^<svg [^>]*viewBox="0 0 29 29"/);
  assert.ok(!svg.includes('<code>'));
  assert.throws(() => qrMatrix('x'.repeat(3000)));
});
