// map/palette.js - perceptual colour ramps shared by aircraft icons, trails, the list and the legend.
// Ramps are built in OKLCH so that lightness stays even while the hue carries the value.

const clamp01 = (x) => (x < 0 ? 0 : x > 1 ? 1 : x);
const toHex = (v) => Math.round(clamp01(v) * 255).toString(16).padStart(2, '0');
const gamma = (x) => (x <= 0.0031308 ? 12.92 * x : 1.055 * Math.pow(x, 1 / 2.4) - 0.055);

function oklchToLinear(L, C, hDeg) {
  const h = (hDeg * Math.PI) / 180;
  const a = C * Math.cos(h);
  const b = C * Math.sin(h);
  const l = (L + 0.3963377774 * a + 0.2158037573 * b) ** 3;
  const m = (L - 0.1055613458 * a - 0.0638541728 * b) ** 3;
  const s = (L - 0.0894841775 * a - 1.291485548 * b) ** 3;
  return [
    4.0767416621 * l - 3.3077115913 * m + 0.2309699292 * s,
    -1.2684380046 * l + 2.6097574011 * m - 0.3413193965 * s,
    -0.0041960863 * l - 0.7034186147 * m + 1.707614701 * s,
  ];
}

/** OKLCH -> "#rrggbb", reducing chroma until the colour fits sRGB. */
export function oklch(L, C, hDeg) {
  let c = C;
  let rgb = oklchToLinear(L, c, hDeg);
  for (let i = 0; i < 24 && rgb.some((v) => v < -0.0005 || v > 1.0005); i++) {
    c *= 0.93;
    rgb = oklchToLinear(L, c, hDeg);
  }
  return '#' + rgb.map((v) => toHex(gamma(clamp01(v)))).join('');
}

/** Piecewise-linear interpolation over [[x, y], ...] sorted by x. */
function lerpStops(stops, x) {
  if (x <= stops[0][0]) return stops[0][1];
  for (let i = 1; i < stops.length; i++) {
    if (x <= stops[i][0]) {
      const [x0, y0] = stops[i - 1];
      const [x1, y1] = stops[i];
      return y0 + ((y1 - y0) * (x - x0)) / (x1 - x0);
    }
  }
  return stops[stops.length - 1][1];
}

export const NO_DATA = '#8fa3c4';
export const GROUND = '#b9c6dc';

// ---- altitude: amber (low) -> green -> cyan -> blue -> violet -> pink (high), constant lightness ----
const ALT_MAX = 45000;
const ALT_STEP = 500;
const ALT_HUE = [[0, 42], [3000, 72], [8000, 125], [14000, 165], [22000, 208], [30000, 252], [38000, 295], [45000, 338]];
const ALT_COLORS = [];
for (let ft = 0; ft <= ALT_MAX; ft += ALT_STEP) ALT_COLORS.push(oklch(0.8, 0.165, lerpStops(ALT_HUE, ft)));

// ---- speed: violet (slow) -> magenta -> orange -> yellow (fast), lightness rises ----
const SPD_MAX = 600;
const SPD_STEP = 20;
const SPD_COLORS = [];
for (let kt = 0; kt <= SPD_MAX; kt += SPD_STEP) {
  const t = kt / SPD_MAX;
  SPD_COLORS.push(oklch(0.58 + 0.32 * t, 0.17, 285 + 170 * t));
}

// ---- signal: indigo (weak) -> blue -> teal -> green -> yellow (strong), lightness rises ----
const SIG_MIN = -40;
const SIG_COLORS = [];
for (let db = SIG_MIN; db <= 0; db += 1) {
  const t = (db - SIG_MIN) / -SIG_MIN;
  SIG_COLORS.push(oklch(0.5 + 0.42 * t, 0.16, 285 - 180 * t));
}

/** Colour of Mode A/C MLAT targets (addrType "modeac"), in every colour mode. */
export const MODEAC_COLOR = '#f472b6';
export const SOURCE_COLORS = { adsb: '#22d3ee', mlat: '#fbbf24', tisb: '#2dd4bf', adsr: '#2dd4bf', modes: '#a78bfa', other: '#8fa3c4' };
const SRC_CODE_COLORS = [SOURCE_COLORS.adsb, SOURCE_COLORS.mlat, SOURCE_COLORS.tisb, SOURCE_COLORS.other];

const ok = (v) => typeof v === 'number' && Number.isFinite(v);

export const altColor = (ft) => (ok(ft) ? ALT_COLORS[Math.max(0, Math.min(ALT_COLORS.length - 1, Math.round(ft / ALT_STEP)))] : NO_DATA);
export const speedColor = (kt) => (ok(kt) ? SPD_COLORS[Math.max(0, Math.min(SPD_COLORS.length - 1, Math.round(kt / SPD_STEP)))] : NO_DATA);
export const signalColor = (db) => (ok(db) ? SIG_COLORS[Math.max(0, Math.min(SIG_COLORS.length - 1, Math.round(db - SIG_MIN)))] : NO_DATA);
export const sourceColor = (src) => SOURCE_COLORS[src] || SOURCE_COLORS.other;

/** Colour modes offered in the UI. */
export const COLOR_MODES = [
  { value: 'altitude', label: 'Altitude' },
  { value: 'speed', label: 'Speed' },
  { value: 'signal', label: 'Signal' },
  { value: 'source', label: 'Source' },
];

/** Colour of a live aircraft DTO under a colour mode. */
export function aircraftColor(mode, a) {
  // Mode A/C MLAT targets (no Mode S address) always get their own colour.
  if (a.addrType === 'modeac') return MODEAC_COLOR;
  switch (mode) {
    case 'speed': return speedColor(a.gs);
    case 'signal': return signalColor(a.rssiAvg ?? a.rssi);
    case 'source': return sourceColor(a.source);
    default:
      if (a.onGround) return GROUND;
      return altColor(a.altBaro ?? a.altGeom);
  }
}

/**
 * Colour of a trail point [tMs, lat, lon, altFt, gsKt, rssi, src] under a colour mode.
 * Consecutive points with the same colour are merged into one line, so the ramp granularity
 * (500 ft / 20 kt / 1 dB) is also the trail gradient granularity.
 */
export function trailColor(mode, p) {
  switch (mode) {
    case 'speed': return speedColor(p[4]);
    case 'signal': return signalColor(p[5]);
    case 'source': return SRC_CODE_COLORS[p[6]] || SOURCE_COLORS.other;
    default: return altColor(p[3]);
  }
}

/**
 * Legend description for a colour mode.
 * @returns {{title: string, gradient?: string, ticks?: Array<{pos: number, label: string}>, items?: Array<{label: string, color: string}>}}
 */
export function legend(mode, units) {
  const grad = (colors) => `linear-gradient(90deg, ${colors.map((c, i) => `${c} ${((i / (colors.length - 1)) * 100).toFixed(1)}%`).join(', ')})`;
  if (mode === 'source') {
    return { title: 'Source', items: [{ label: 'ADS-B', color: SOURCE_COLORS.adsb }, { label: 'MLAT', color: SOURCE_COLORS.mlat }, { label: 'TIS-B', color: SOURCE_COLORS.tisb }, { label: 'Mode S', color: SOURCE_COLORS.modes }] };
  }
  if (mode === 'speed') {
    const ticks = [0, 150, 300, 450, 600].map((kt) => ({ pos: kt / SPD_MAX, label: units.format('speed', kt, { unit: false }) }));
    return { title: `Ground speed · ${units.label('speed')}`, gradient: grad(SPD_COLORS), ticks };
  }
  if (mode === 'signal') {
    const ticks = [-40, -30, -20, -10, 0].map((db) => ({ pos: (db - SIG_MIN) / -SIG_MIN, label: String(db).replace('-', '−') }));
    return { title: 'Signal · dBFS', gradient: grad(SIG_COLORS), ticks };
  }
  const metric = units.system === 'metric';
  const marks = metric ? [0, 9842.5, 19685, 29527.6, 39370] : [0, 10000, 20000, 30000, 40000];
  const ticks = marks.map((ft) => ({ pos: ft / ALT_MAX, label: metric ? Math.round((ft * 0.3048) / 1000) + 'k' : ft / 1000 + 'k' }));
  ticks[0].label = '0';
  return { title: `Altitude · ${units.label('alt')}`, gradient: grad(ALT_COLORS), ticks };
}
