// map/aircraft-icons.js - aircraft silhouettes (original artwork, nose up, 32x32 box) and their
// conversion to signed-distance-field images so MapLibre can tint, outline, rotate and scale
// them crisply on the GPU from one small texture per shape.

/** Shape id -> { d: SVG path data (32x32 box), scale: relative on-map size }. */
export const SHAPES = {
  jet: {
    scale: 1,
    d: 'M16 1.500C17.100 1.500 17.800 3.400 17.800 5.600L17.800 11.600L29.500 18.600L29.500 21.200L17.800 17.600L17.600 24.600L21.800 27.600L21.800 29.400L16 28L10.200 29.400L10.200 27.600L14.400 24.600L14.200 17.600L2.500 21.200L2.500 18.600L14.200 11.600L14.200 5.600C14.200 3.400 14.900 1.500 16 1.500Z' +
      'M20.300 11.400h2.100v3.900h-2.100zM9.600 11.400h2.100v3.900H9.600z',
  },
  heavy: {
    scale: 1.24,
    d: 'M16 1C17.600 1 18.400 3.200 18.400 5.800L18.400 11L30.800 19.400L30.800 22L18.400 17.400L18.100 24.400L23 28L23 30L16 28.400L9 30L9 28L13.900 24.400L13.600 17.400L1.200 22L1.200 19.400L13.600 11L13.600 5.800C13.600 3.200 14.400 1 16 1Z' +
      'M20.100 10.300h2v4h-2zM24.500 13.300h1.900v3.800h-1.900zM9.900 10.300h2v4h-2zM5.600 13.300h1.900v3.800H5.600z',
  },
  light: {
    scale: 0.74,
    d: 'M16 3C16.900 3 17.400 4.200 17.500 5.600L17.700 10L29.500 10.600L29.500 14.200L17.600 15L17.100 23L21.600 24L21.600 26.800L16.700 26.500L16 28.200L15.300 26.500L10.400 26.800L10.400 24L14.900 23L14.400 15L2.500 14.200L2.500 10.600L14.300 10L14.500 5.600C14.600 4.200 15.100 3 16 3Z' +
      'M12.400 3.200h7.200v1.300h-7.200z',
  },
  turboprop: {
    // straight wing with two engine nacelles (regional / military turboprops, big pistons)
    scale: 0.95,
    d: 'M16 2C17 2 17.600 3.300 17.700 5L17.800 10.600L30.500 11.400L30.500 14.400L17.700 15.300L17.200 24.200L22.400 25.200L22.400 27.400L16.600 27.100L16 29L15.400 27.100L9.600 27.400L9.600 25.200L14.800 24.200L14.300 15.300L1.500 14.400L1.500 11.400L14.200 10.600L14.300 5C14.400 3.300 15 2 16 2Z' +
      'M9 7.500h2.800v8.500H9zM20.200 7.500h2.800v8.500h-2.800z',
  },
  fast: {
    scale: 0.92,
    d: 'M16 1.500L17.700 9L27.500 22L27.500 24.200L18.500 22.600L19.600 27.400L19.600 29.200L16 28L12.400 29.200L12.400 27.400L13.500 22.600L4.500 24.200L4.500 22L14.300 9Z',
  },
  helicopter: {
    scale: 0.86,
    d: 'M16 6.500C18.700 6.500 19.800 9 19.800 12.300C19.800 15.600 18.500 17.900 17.100 18.700L16.800 25.400L19.600 25.400L19.600 27.400L12.400 27.400L12.400 25.400L15.200 25.400L14.900 18.700C13.500 17.900 12.200 15.600 12.200 12.300C12.200 9 13.300 6.500 16 6.500Z' +
      'M4.700 2.300L6.300 0.700L16 10.400L25.700 0.700L27.300 2.300L17.600 12L27.300 21.700L25.700 23.300L16 13.600L6.300 23.300L4.700 21.700L14.400 12Z',
  },
  glider: {
    scale: 0.86,
    d: 'M16 3C16.800 3 17.200 4.400 17.200 6L17.200 11.200L31.200 12.300L31.200 14.300L17.100 14.900L16.700 24.800L20.200 25.600L20.200 27.500L16 27L11.800 27.500L11.800 25.600L15.300 24.800L14.900 14.900L0.800 14.300L0.800 12.300L14.800 11.200L14.800 6C14.800 4.400 15.200 3 16 3Z',
  },
  balloon: {
    scale: 0.8,
    d: 'M16 2.500C21.200 2.500 25 6.400 25 11.300C25 15.800 21.600 19.200 18.600 21.400L18.600 23.200L13.400 23.200L13.400 21.400C10.400 19.200 7 15.800 7 11.300C7 6.400 10.800 2.500 16 2.500ZM13.600 25h4.800v4.200h-4.800z',
  },
  ground: {
    scale: 0.62,
    d: 'M16 3.500L22.500 9.500L22.500 25.500C22.500 26.900 21.400 28 20 28L12 28C10.600 28 9.500 26.900 9.500 25.500L9.500 9.500Z',
  },
  unknown: {
    scale: 0.8,
    d: 'M16 2.500L26.500 28.500L16 23L5.500 28.500Z',
  },
  dot: {
    scale: 0.6,
    d: 'M16 5L27 16L16 27L5 16Z',
  },
};

// Type designator -> silhouette, checked in this order (first match wins). Every list is anchored
// (whole designator) so that e.g. A320 never matches a wide-body prefix, C130 never matches the
// Cessna piston range and AS21 (ASK 21 glider) never matches the Eurocopter AS range. The node test
// web/dev/test/aircraft-icons.test.mjs checks every designator of the embedded ICAO Doc 8643 table
// (internal/refdata/types_data.go) against its description code and wake category.
const TYPE_RULES = [
  ['balloon', /^(BALL|SHIP|BLMP)$/],
  ['glider', /^(GLID|AS(1[0-9]|2[0-9]|3[13])|ASK[0-9]{2}|ASW[0-9]{2}|ASG[0-9]{2}|ASH[0-9]{2}|DG[0-9]{2,3}|DG1T|LS[1-9]|DISC|DUOD|VENT|NIMB|JANU|ARCS|ARCP|G10[2-9]|PIK2|SZD[0-9]*|L13|L23|K7|K8|K21|SF2[57]|PUCH|LAK[0-9]{2})$/],
  ['helicopter', /^(EC[0-9]{2}|H1[0-9]{2}|H(47|53|60|64|269|500)|AS(32|3B|35|50|55|65)|SA(3[0-9]{2}|31|34|36|65)|R(22|44|66)|B(06|06T|407|412|427|429|430|47G|212|214|222|230|505)|UH1Y?|AH1Z?|A1(09|19|29|39|49|69|89)|S(61|64|76|92)|EH10|NH90|LYNX|PUMA|GAZL|MI(2|8|14|17|24|26|28|35|38)|KA(26|27|32|50|52)|K226|MD5[02]|MD60|EXPL|G2CA|BK17|EN28|EN48|ALO3|ALH|GYRO|V22)$/],
  ['fast', /^(F(4|5|14|15|16|18|18H|18S|22|35|117)|A(4|10)|EUFI|TOR|RFAL|MIR2|M2000|SB39|JAS3|HAWK|M346|L39|L159|T38|T45|T50|SU(2[4-7]|3[0-5]|57)|MG(2[3-9]|31)|JF17|J10|AJET|HAR|AV8B)$/],
  ['heavy', /^(A30B|A306|A310|A3ST|A33[0-9]|A34[0-9]|A35[0-9K]|A38[0-9]|B70[0-9]|B74[0-9A-Z]|BLCF|B76[0-9]|B77[0-9A-Z]|B78[0-9X]|DC(8[0-9]|10)|MD11|L101|CONC|A124|A225|IL(62|76|86|96)|C17|C5M?|K35[ER]|R135|C135|E3[CT]F|E6|B52|B1|B2|KC10|K46|E767)$/],
  ['turboprop', /^(AT[4-7][0-9]|DH8[A-D]|DHC[5-7]|SF34|SB20|F27|F50|JS(31|32|41)|ATP|A748|D228|D328|SW[34]|B190|L410|SH3[36]|SC7|C212|BN2[PT]|TRIS|MA60|Y12|AN(12|22|24|26|28|30|32|38)|IL18|L188|E1[12]0|A400|C130|C30J|C160|C27J|C295|CN35|G222|P3|C2|E2|BE(20|30|9L|9T|99|10)|B350|C441|C425|PAY[1-4]|MU2|AC(90|95)|P180|DC3|DC6|LANC|B17)$/],
  ['jet', /^(A(19N|2[01]N|31[89]|32[01])|BCS[13]|B3[789X]M|B7(1[27]|2[0-2]|3[2-9]|5[23])|MD(8[0-9]|90)|DC9[0-9]?|E(135|145|45X|170|175|75[SL]|190|195|29[05]|50P|55P|35L|545|550|737)|CRJ[12579X]|F(28|70|100)|B46[1-3]|RJ(70|85|1H)|J328|SU95|AJ27|C919|AN72|A1[45]8|T(134|154|204)|YK4[02]|C(25[ABCM]|5[0-9][0-9X]|6[58][0-9A]|7[05]0)|CL(30|35|60)|GL(EX|5T|7T|F[2-6])|GA[5-8]C|G(150|280|ALX)|ASTR|LJ[0-9]{2}|F(900|2TH|A[0-9][0-9X])|H25[ABC]|HA4T|BE40|PRM1|HDJT|SF50|EA50|PC24|P8|Q4|U2)$/],
  ['light', /^(C1[0-9]{2}|C2(0[5-8]|10)|C3[0-9]{2}|C4[0-2][0-9]|C42|PA[0-9]{2}T?|P28[A-Z]|P32[RT]|P46T|BE(19|23|24|33|35|36|55|58|60|76|95)|M20[A-Z]|SR2[02]|S22T|DV20|DA[2-6][0-9]|AA[15]|DR[0-9]{2}|TOBA|TB[0-9]{1,2}|G115|A210|RV[0-9]{1,2}|E300|PTS2|CP10|YK52|DH82|DHC[1-3]|T6|P51|SPIT|HURI|EV97|PIVI|ULAC|PC(6|7|9|12|21)|TBM[7-9]|KODI|TEX2|TUCA|AN2|P68|J3|CH7A|Q9|AT[38]T|GA8|C77R|M7)$/],
];

/** Silhouette from an ICAO Doc 8643 description code ("L2J") and wake category ("M"); "" when unusable. */
function shapeFromClass(desc, wtc) {
  const d = String(desc || '').toUpperCase();
  const w = String(wtc || '').toUpperCase();
  if (/^[HGT]/.test(d)) return 'helicopter'; // helicopter, gyrocopter, tilt-rotor
  const m = /^[LSA]([1-8C])([JTPER])$/.exec(d);
  if (m) {
    const engines = m[1] === 'C' ? 2 : Number(m[1]);
    if (m[2] === 'J') return w === 'H' || w === 'J' ? 'heavy' : 'jet';
    if (m[2] === 'T') return engines >= 2 ? 'turboprop' : 'light';
    return engines >= 3 ? 'turboprop' : 'light';
  }
  if (w === 'H' || w === 'J') return 'heavy';
  if (w === 'M') return 'jet';
  if (w === 'L') return 'light';
  return '';
}

/** Silhouette for an ADS-B emitter category (DO-260B), "" when the category says nothing. */
function shapeFromCategory(cat) {
  switch (cat) {
    case 'A1': case 'B4': return 'light';
    case 'A2': case 'A3': case 'A4': return 'jet';
    case 'A5': return 'heavy';
    case 'A6': case 'B7': return 'fast';
    case 'A7': return 'helicopter';
    case 'B1': return 'glider';
    case 'B2': return 'balloon';
    case 'C1': case 'C2': case 'C3': case 'C4': case 'C5': return 'ground';
    default: return '';
  }
}

/** Silhouette for an ICAO type designator alone, "" when the designator is not known. */
export function shapeForType(type) {
  const t = String(type || '').trim().toUpperCase();
  if (!t) return '';
  for (const [shape, re] of TYPE_RULES) if (re.test(t)) return shape;
  return '';
}

/**
 * Pick the silhouette for an aircraft. The ICAO type designator is the most specific source and
 * wins; then the Doc 8643 description / wake category when the caller has them (typeInfo from
 * /api/aircraft/{icao}, or desc8643 / wtc fields); then the ADS-B emitter category; finally a
 * neutral dart (track known) or diamond (no track). Surface vehicles (C1-C5) always stay "ground".
 * @param {{category?: string, type?: string, track?: number, onGround?: boolean,
 *   typeInfo?: {description?: string, wtc?: string}, desc8643?: string, wtc?: string}} a
 * @returns {keyof typeof SHAPES}
 */
export function shapeFor(a) {
  const cat = a.category || '';
  if (/^C[1-5]$/.test(cat)) return 'ground';
  const byType = shapeForType(a.type);
  if (byType) return byType;
  const ti = a.typeInfo || null;
  const wtc = (ti && ti.wtc) || (typeof a.wtc === 'string' && a.wtc.length <= 3 ? a.wtc : '');
  const byClass = shapeFromClass((ti && ti.description) || a.desc8643, wtc);
  if (byClass) return byClass;
  const byCat = shapeFromCategory(cat);
  if (byCat) return byCat;
  return typeof a.track === 'number' ? 'unknown' : 'dot';
}

// ---- signed distance field generation (Felzenszwalb & Huttenlocher squared EDT) ----
const INF = 1e20;

function edt1d(grid, offset, stride, length, f, v, z) {
  v[0] = 0;
  z[0] = -INF;
  z[1] = INF;
  f[0] = grid[offset];
  for (let q = 1, k = 0, s = 0; q < length; q++) {
    f[q] = grid[offset + q * stride];
    const q2 = q * q;
    do {
      const r = v[k];
      s = (f[q] - f[r] + q2 - r * r) / (q - r) / 2;
    } while (s <= z[k] && --k > -1);
    k++;
    v[k] = q;
    z[k] = s;
    z[k + 1] = INF;
  }
  for (let q = 0, k = 0; q < length; q++) {
    while (z[k + 1] < q) k++;
    const r = v[k];
    const qr = q - r;
    grid[offset + q * stride] = f[r] + qr * qr;
  }
}

function edt(grid, w, h, f, v, z) {
  for (let x = 0; x < w; x++) edt1d(grid, x, w, h, f, v, z);
  for (let y = 0; y < h; y++) edt1d(grid, y * w, 1, w, f, v, z);
}

/** SDF parameters understood by MapLibre's symbol shader (same as its glyph SDFs). */
const SDF_RADIUS = 8;
const SDF_CUTOFF = 0.25;

/**
 * Rasterise a 32x32-box path into an SDF image.
 * @param {string} d SVG path data
 * @param {number} sizePx edge length of the drawn shape in image pixels
 * @param {number} pad padding in image pixels (room for the distance field / halo)
 * @returns {{width: number, height: number, data: Uint8ClampedArray}}
 */
export function sdfFromPath(d, sizePx = 64, pad = 8) {
  const dim = sizePx + pad * 2;
  const canvas = document.createElement('canvas');
  canvas.width = dim;
  canvas.height = dim;
  const ctx = canvas.getContext('2d', { willReadFrequently: true });
  ctx.translate(pad, pad);
  ctx.scale(sizePx / 32, sizePx / 32);
  ctx.fillStyle = '#000';
  ctx.fill(new Path2D(d), 'nonzero');
  const src = ctx.getImageData(0, 0, dim, dim).data;
  const n = dim * dim;
  const outer = new Float64Array(n);
  const inner = new Float64Array(n);
  for (let i = 0; i < n; i++) {
    const a = src[i * 4 + 3] / 255;
    if (a === 0) { outer[i] = INF; inner[i] = 0; }
    else if (a === 1) { outer[i] = 0; inner[i] = INF; }
    else { const dd = 0.5 - a; outer[i] = dd > 0 ? dd * dd : 0; inner[i] = dd < 0 ? dd * dd : 0; }
  }
  const f = new Float64Array(dim);
  const v = new Uint16Array(dim);
  const z = new Float64Array(dim + 1);
  edt(outer, dim, dim, f, v, z);
  edt(inner, dim, dim, f, v, z);
  const data = new Uint8ClampedArray(n * 4);
  for (let i = 0; i < n; i++) {
    const dist = Math.sqrt(outer[i]) - Math.sqrt(inner[i]);
    data[i * 4 + 3] = Math.round(255 - 255 * (dist / SDF_RADIUS + SDF_CUTOFF));
  }
  return { width: dim, height: dim, data };
}

const ICON_PX = 34; // on-map size of a scale-1 icon in CSS pixels at icon-size 1
const RATIO = 2;
let cache = null;

/** Image name used in the style for a shape. */
export const imageName = (shape) => 'ac-' + shape;

/**
 * Register every aircraft shape with a MapLibre map as an SDF image (idempotent; call again
 * after each style load because setStyle() drops images).
 */
export function addAircraftImages(map) {
  if (!cache) {
    cache = {};
    for (const [id, s] of Object.entries(SHAPES)) cache[id] = sdfFromPath(s.d, ICON_PX * RATIO, 8);
  }
  for (const [id, img] of Object.entries(cache)) {
    const name = imageName(id);
    if (!map.hasImage(name)) map.addImage(name, img, { sdf: true, pixelRatio: RATIO });
  }
}

/** Small inline SVG of a shape (legend / detail header). */
export function shapeSvg(shape, color = 'currentColor', size = 20, rotate = 0) {
  const s = SHAPES[shape] || SHAPES.unknown;
  return `<svg viewBox="0 0 32 32" width="${size}" height="${size}" style="transform:rotate(${Math.round(rotate)}deg)" aria-hidden="true"><path d="${s.d}" fill="${color}" stroke="rgba(1,4,11,.8)" stroke-width="1" stroke-linejoin="round"/></svg>`;
}
