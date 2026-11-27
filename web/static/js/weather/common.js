// weather/common.js - helpers shared by the Weather & Propagation page and the Forecast page:
// panel state handling (loading / empty / error), polling sources, time and unit formatting,
// class colours, inline-SVG bits and a few ECharts option builders in the AirView look.

import { ui, h } from '../ui.js';
import { units } from '../units.js';
import { colors, fade } from '../charts.js';

export const HOUR = 3600e3;
export const FT_PER_M = 3.28084;
const NBSP = ' ';

export const ok = (v) => typeof v === 'number' && Number.isFinite(v);
export const clamp = (v, lo, hi) => Math.max(lo, Math.min(hi, v));
export const esc = ui.escapeHtml;

// ---------------------------------------------------------------------------------------------
// formatting
// ---------------------------------------------------------------------------------------------

const DAYS = ['Sun', 'Mon', 'Tue', 'Wed', 'Thu', 'Fri', 'Sat'];
const pad2 = (n) => (n < 10 ? '0' + n : '' + n);

/** Local clock time "03:00". */
export const hm = (ms) => (ok(ms) && ms > 0 ? units.clock(ms, { utc: false, seconds: false }) : units.DASH);
/** Local weekday + time "Thu 03:00". */
export const dayHm = (ms) => (ok(ms) && ms > 0 ? DAYS[new Date(ms).getDay()] + NBSP + hm(ms) : units.DASH);
/** Local day + month "Thu 12 Nov". */
export function dayLabel(ms) {
  if (!ok(ms) || ms <= 0) return units.DASH;
  const d = new Date(ms);
  return `${DAYS[d.getDay()]} ${d.getDate()} ${['Jan', 'Feb', 'Mar', 'Apr', 'May', 'Jun', 'Jul', 'Aug', 'Sep', 'Oct', 'Nov', 'Dec'][d.getMonth()]}`;
}
/** Local "Thu 12 Nov 03:00". */
export const dayTime = (ms) => (ok(ms) && ms > 0 ? dayLabel(ms) + ' ' + hm(ms) : units.DASH);
/** "Thu 03:00–07:00" (end on another day: "Thu 22:00 – Fri 02:00"). */
export function windowLabel(from, to) {
  const a = new Date(from);
  const b = new Date(to);
  return a.getDay() === b.getDay() && to - from < 24 * HOUR ? `${dayHm(from)}–${hm(to)}` : `${dayHm(from)} – ${dayHm(to)}`;
}
/** Short axis label for an hour: "03:00" or, at midnight, the weekday. */
export function axisHour(ms) {
  const d = new Date(ms);
  return d.getHours() === 0 ? DAYS[d.getDay()] : pad2(d.getHours()) + ':00';
}

/** Value of a difference (not an absolute reading) in the user's unit, e.g. a temperature bias. */
export const delta = (kind, v) => (ok(v) ? units.value(kind, v) - units.value(kind, 0) : null);
/** Formatted difference with sign and unit: "+1.2 °C". */
export function fmtDelta(kind, v, digits = 1) {
  const d = delta(kind, v);
  if (!ok(d)) return units.DASH;
  return (d > 0 ? '+' : '') + units.num(d, digits) + NBSP + units.label(kind);
}
/** Signed number "+0.42" / "−0.10". */
export const signed = (v, digits = 2) => (ok(v) ? (v > 0 ? '+' : '') + units.num(v, digits, digits) : units.DASH);
/** p-value text. */
export const pText = (p) => (!ok(p) ? units.DASH : p < 0.001 ? '<0.001' : units.num(p, 3, 3));
/** 0..1 probability as percent text. */
export const prob = (p, digits = 0) => (ok(p) ? units.pct(p * 100, p > 0 && p < 0.01 && digits === 0 ? 1 : digits) : units.DASH);
/** Number in the user's unit rounded for chart data (null when missing). */
export function uv(kind, v, digits = 1) {
  const x = units.value(kind, v);
  if (!ok(x)) return null;
  const f = 10 ** digits;
  return Math.round(x * f) / f;
}
export const round = (v, digits = 1) => (ok(v) ? Math.round(v * 10 ** digits) / 10 ** digits : null);

/** Dew point (degC) from temperature (degC) and relative humidity (%), Magnus formula. */
export function dewPoint(tC, rh) {
  if (!ok(tC) || !ok(rh) || rh <= 0) return null;
  const a = 17.625;
  const b = 243.04;
  const g = Math.log(Math.min(100, rh) / 100) + (a * tC) / (b + tC);
  return (b * g) / (a - g);
}

/** Smooth-earth radio horizon in km for two heights in metres and an effective earth-radius factor k. */
export function horizonKm(h1m, h2m, k = 4 / 3) {
  const R = 6371;
  return Math.sqrt(2 * k * R * Math.max(0, h1m) / 1000) + Math.sqrt(2 * k * R * Math.max(0, h2m) / 1000);
}

// ---------------------------------------------------------------------------------------------
// classes and labels
// ---------------------------------------------------------------------------------------------

/** Refraction classes (radio.Class*). */
export const CLASSES = {
  'sub-refraction': { label: 'Sub-refraction', color: colors.amber, badge: 'amber', text: 'radio waves bend away from the ground' },
  standard: { label: 'Standard', color: colors.blue, badge: 'blue', text: 'normal bending' },
  'super-refraction': { label: 'Super-refraction', color: colors.cyan, badge: 'cyan', text: 'waves bend further around the horizon' },
  ducting: { label: 'Ducting', color: colors.violet, badge: 'violet', text: 'waves can be trapped in a layer and travel far beyond the horizon' },
};
export const classInfo = (c) => CLASSES[c] || { label: c || 'Unknown', color: colors.grey, badge: 'other', text: '' };

/** Reception-index labels (predict.Label*). */
export const LABELS = {
  poor: { label: 'Poor', color: colors.amber, badge: 'amber' },
  normal: { label: 'Normal', color: colors.blue, badge: 'blue' },
  enhanced: { label: 'Enhanced', color: colors.cyan, badge: 'cyan' },
  'ducting likely': { label: 'Ducting likely', color: colors.violet, badge: 'violet' },
};
export const labelInfo = (l) => LABELS[l] || { label: l || 'Unknown', color: colors.grey, badge: 'other' };
export const isGoodLabel = (l) => l === 'enhanced' || l === 'ducting likely';

/** Zones of the 0..100 propagation index (anchors of radio.Assess: 30 = dN/dh 0, 65 = -79, 85 = -157 N/km). */
export const INDEX_ZONES = [
  { to: 30, name: 'sub-refraction', color: colors.amber },
  { to: 65, name: 'standard', color: colors.blue },
  { to: 85, name: 'super-refraction', color: colors.cyan },
  { to: 100, name: 'ducting', color: colors.violet },
];

export const confBadge = (c) => ui.badge('confidence ' + (c || 'n/a'), c === 'high' ? 'green' : c === 'medium' ? 'blue' : 'other');
const LEVEL_TEXT = { none: 'no model yet', physics: 'physics only', blend: 'physics + learned', learned: 'learned model' };
export const levelBadge = (l) => ui.badge(LEVEL_TEXT[l] || l || 'n/a', l === 'learned' ? 'green' : l === 'blend' ? 'teal' : l === 'physics' ? 'blue' : 'other');

// ---------------------------------------------------------------------------------------------
// panels and sources
// ---------------------------------------------------------------------------------------------

/** One-line chart explanation. */
export const caption = (...text) => h('p.wx-cap', ...text);

/**
 * A card body that is either a state (skeleton / empty / error) or the built content.
 *   const p = panel(host, { build(el) -> update(data), empty(data) -> null | { icon, title, text }, skeleton: px });
 *   p.set(data)   p.fail(err, retry)   p.rerender()   p.data
 * The content is built lazily on the first non-empty data, so charts are created in a visible host.
 */
export function panel(host, o) {
  const state = h('div.wx-state');
  const content = h('div.wx-content.hidden');
  host.append(state, content);
  ui.mount(state, ui.skeleton({ lines: o.lines ?? 3, block: o.skeleton || 0 }));
  let update = null;
  let data;
  let has = false;
  const showState = (node) => { content.classList.add('hidden'); state.classList.remove('hidden'); ui.mount(state, node); };
  const p = {
    get data() { return data; },
    set(d) {
      data = d;
      has = true;
      try {
        const e = o.empty ? o.empty(d) : null;
        if (e) { showState(ui.empty({ icon: 'history', compact: true, ...e })); return; }
        state.classList.add('hidden');
        ui.clear(state);
        content.classList.remove('hidden');
        if (!update) update = o.build(content);
        update(d);
      } catch (err) {
        console.error('[weather] panel render failed', err);
        showState(ui.error(err, { title: 'Could not draw this panel', compact: true }));
      }
    },
    fail(err, retry) { if (!has) showState(ui.error(err, { retry, compact: true })); },
    rerender() { if (has) p.set(data); },
  };
  return p;
}

/**
 * Poll a loader and feed panels.  source(ctx, { load(signal) -> data, every: ms, panels: [panel], onData(data), onError(err) })
 * Returns run() (a manual refresh). The first failure shows an error state with Retry in every panel; later
 * failures keep the previous content. Polls are skipped while the tab is hidden.
 */
export function source(ctx, o) {
  let busy = false;
  let loaded = false;
  const run = async (force) => {
    if (busy || ctx.signal.aborted) return;
    if (!force && loaded && document.hidden) return;
    busy = true;
    try {
      const d = await o.load(ctx.signal);
      if (ctx.signal.aborted) return;
      loaded = true;
      for (const p of o.panels || []) p.set(d);
      if (o.onData) o.onData(d);
    } catch (e) {
      if (ctx.signal.aborted || (e && e.aborted)) return;
      console.warn('[weather] load failed', e);
      for (const p of o.panels || []) p.fail(e, () => run(true));
      if (o.onError) o.onError(e);
    } finally {
      busy = false;
    }
  };
  run(true);
  if (o.every) ctx.every(o.every, () => run(false));
  return () => run(true);
}

/** Resolve to the value of a promise, or to `fallback` when it rejects (aborts are passed on). */
export async function soft(promise, fallback = null) {
  try { return await promise; } catch (e) { if (e && e.aborted) throw e; return fallback; }
}

/** Progress row: label, "x of y" text and a meter. */
export function progress(label, have, need, text, done) {
  const pct = need > 0 ? clamp((have / need) * 100, 0, 100) : 0;
  const m = ui.meter(pct);
  if (done ?? pct >= 100) m.classList.add('wx-meter--done');
  return h('div.wx-progress', h('div.wx-progress-head', h('span', label), h('span.num.muted', text)), m);
}

/** Bulleted notes list (strings). */
export const notesList = (items, cls = '') => h('ul.wx-notes' + cls, (items || []).map((t) => h('li', t)));

/** Collapsible "how this is calculated" block. */
export const details = (summary, ...body) => h('details.wx-details', h('summary', summary), h('div.wx-details-body', ...body));

/** Data attribution line. */
export function attribution(items) {
  return h('div.wx-attrib', ui.icon('info'), h('span', 'Data: '), items.filter(Boolean).map((it, i) => [
    i ? ' · ' : null,
    it.url ? h('a', { href: it.url, target: '_blank', rel: 'noopener noreferrer' }, it.text) : h('span', it.text),
    it.note ? h('span.muted', ' ' + it.note) : null,
  ]));
}

// ---------------------------------------------------------------------------------------------
// inline SVG
// ---------------------------------------------------------------------------------------------

/** Wind compass: arrow shows where the wind blows TO, the tail sits on the bearing it comes FROM. */
export function compassSvg(dirFrom, size = 58) {
  const has = ok(dirFrom);
  const ticks = [];
  for (let a = 0; a < 360; a += 30) {
    const r1 = a % 90 === 0 ? 20 : 22;
    const x1 = 30 + r1 * Math.sin((a * Math.PI) / 180);
    const y1 = 30 - r1 * Math.cos((a * Math.PI) / 180);
    const x2 = 30 + 25 * Math.sin((a * Math.PI) / 180);
    const y2 = 30 - 25 * Math.cos((a * Math.PI) / 180);
    ticks.push(`<line x1="${x1.toFixed(1)}" y1="${y1.toFixed(1)}" x2="${x2.toFixed(1)}" y2="${y2.toFixed(1)}" class="wx-compass-tick"/>`);
  }
  const arrow = has
    ? `<g transform="rotate(${(dirFrom + 180).toFixed(0)} 30 30)"><line x1="30" y1="44" x2="30" y2="19" class="wx-compass-shaft"/><path d="M30 12 L35.5 22 L30 19.5 L24.5 22 Z" class="wx-compass-head"/><circle cx="30" cy="44" r="2.2" class="wx-compass-tail"/></g>`
    : '';
  return h('div.wx-compass', {
    style: { width: size + 'px', height: size + 'px' },
    html: `<svg viewBox="0 0 60 60" role="img" aria-label="${has ? 'Wind from ' + Math.round(dirFrom) + ' degrees' : 'No wind direction'}"><circle cx="30" cy="30" r="26" class="wx-compass-ring"/>${ticks.join('')}<text x="30" y="9.5" class="wx-compass-n">N</text>${arrow}</svg>`,
  });
}

/** Trend arrow glyph for a signed change. */
export const trendArrow = (v, eps = 0.1) => (!ok(v) ? '' : v > eps * 3 ? '↑' : v > eps ? '↗' : v < -eps * 3 ? '↓' : v < -eps ? '↘' : '→');

// ---------------------------------------------------------------------------------------------
// ECharts option builders
// ---------------------------------------------------------------------------------------------

const MONO = "'JetBrains Mono Variable','Cascadia Mono',Consolas,monospace";

/** Half-circle gauge for the 0..100 propagation / reception index. */
export function gaugeOption(value, zones = INDEX_ZONES, name = '') {
  return {
    animationDuration: 400,
    series: [{
      type: 'gauge', startAngle: 205, endAngle: -25, min: 0, max: 100, radius: '96%', center: ['50%', '62%'],
      axisLine: { lineStyle: { width: 12, color: zones.map((z) => [z.to / 100, z.color]) } },
      progress: { show: false },
      pointer: { length: '58%', width: 4, itemStyle: { color: colors.text } },
      anchor: { show: true, size: 9, itemStyle: { color: colors.text, borderColor: '#050c1c', borderWidth: 2 } },
      axisTick: { distance: -12, length: 4, lineStyle: { color: 'rgba(2,6,16,0.8)', width: 1 } },
      splitLine: { distance: -12, length: 12, lineStyle: { color: 'rgba(2,6,16,0.9)', width: 2 } },
      axisLabel: { distance: 16, color: colors.text3, fontSize: 9.5, fontFamily: MONO },
      title: { show: !!name, offsetCenter: [0, '62%'], color: colors.text3, fontSize: 11 },
      detail: { valueAnimation: true, offsetCenter: [0, '34%'], fontSize: 26, fontWeight: 500, fontFamily: MONO, color: '#fff', formatter: (v) => (ok(v) ? String(Math.round(v)) : '—') },
      data: [{ value: ok(value) ? value : 0, name }],
    }],
  };
}

/** A dashed vertical "now" mark line for time axes. */
export const nowLine = (t, label = 'now') => ({
  silent: true, symbol: 'none', animation: false,
  lineStyle: { color: 'rgba(232,240,255,0.55)', type: 'dashed', width: 1 },
  label: { formatter: label, color: colors.text2, fontSize: 10, position: 'insideEndTop' },
  data: [{ xAxis: t }],
});

/** Two stacked series drawing a shaded band between lo and hi: pass rows [[t, lo, hi], ...]. */
export function bandSeries(name, rows, color, extra = {}) {
  const id = 'band-' + name;
  const base = { type: 'line', stack: id, symbol: 'none', silent: true, lineStyle: { opacity: 0 }, tooltip: { show: false }, legendHoverLink: false, z: 1, ...extra };
  return [
    { ...base, name: id + '-lo', data: rows.map((r) => [r[0], r[1]]) },
    { ...base, name: id + '-hi', data: rows.map((r) => [r[0], ok(r[1]) && ok(r[2]) ? r[2] - r[1] : null]), areaStyle: { color, opacity: 0.18 } },
  ];
}

/** Common pieces of a multi-grid time chart: returns { grid, xAxis, yAxis } for `rows` stacked panes. */
export function stackedGrids(rows, o = {}) {
  const top = o.top ?? 30;
  const gap = o.gap ?? 34;
  const bottom = o.bottom ?? 26;
  const total = rows.reduce((s, r) => s + (r.weight || 1), 0);
  const grid = [];
  const xAxis = [];
  const yAxis = [];
  let acc = 0;
  rows.forEach((r, i) => {
    const w = r.weight || 1;
    // positions in percent of the space left after the fixed paddings (computed by the caller's height)
    const H = o.height;
    const usable = H - top - bottom - gap * (rows.length - 1);
    const y0 = top + (usable * acc) / total + gap * i;
    const hh = (usable * w) / total;
    acc += w;
    grid.push({ left: 52, right: r.y2 ? 52 : 16, top: Math.round(y0), height: Math.round(hh), containLabel: false });
    xAxis.push({
      type: 'time', gridIndex: i, min: o.min, max: o.max,
      axisLabel: { show: i === rows.length - 1, hideOverlap: true },
      axisTick: { show: false }, splitLine: { show: true, lineStyle: { color: colors.line } },
      axisPointer: { label: { show: i === rows.length - 1, formatter: (p) => dayTime(p.value) } },
    });
    yAxis.push({
      type: 'value', gridIndex: i, name: r.name, scale: r.scale !== false, min: r.min, max: r.max, splitNumber: 3,
      nameTextStyle: { align: 'left', padding: [0, 0, 0, -44], color: colors.text3 }, nameGap: 8,
      axisLabel: { formatter: r.format },
    });
    if (r.y2) {
      yAxis.push({
        type: 'value', gridIndex: i, name: r.y2.name, position: 'right', scale: r.y2.scale !== false, min: r.y2.min, max: r.y2.max, splitNumber: 3,
        splitLine: { show: false }, nameTextStyle: { align: 'right', padding: [0, -44, 0, 0], color: colors.text3 }, nameGap: 8,
        axisLabel: { formatter: r.y2.format },
      });
    }
  });
  return { grid, xAxis, yAxis };
}

/** Index of the y axis of pane `i` (and its secondary axis) for stackedGrids(rows). */
export function yIndex(rows, i, second = false) {
  let n = 0;
  for (let k = 0; k < i; k++) n += rows[k].y2 ? 2 : 1;
  return n + (second ? 1 : 0);
}

/** Axis-trigger tooltip formatter listing visible series with a formatted time header. */
export function timeTooltip(fmt = {}) {
  return (ps) => {
    const list = (Array.isArray(ps) ? ps : [ps]).filter((p) => p.seriesName && !p.seriesName.startsWith('band-'));
    if (!list.length) return '';
    const t = Array.isArray(list[0].value) ? list[0].value[0] : list[0].axisValue;
    const rows = list.map((p) => {
      const v = Array.isArray(p.value) ? p.value[1] : p.value;
      if (v === null || v === undefined || v === '-') return '';
      const f = fmt[p.seriesName];
      const text = typeof f === 'function' ? f(v, p) : units.num(v, Math.abs(v) < 10 ? 1 : 0) + (f ? NBSP + f : '');
      return `<div class="wx-tt-row">${p.marker}<span>${esc(p.seriesName)}</span><b>${esc(text)}</b></div>`;
    }).join('');
    return `<div class="wx-tt"><div class="wx-tt-head">${esc(dayTime(t))}</div>${rows}</div>`;
  };
}

export { colors, fade };
