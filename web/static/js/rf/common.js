// rf/common.js - shared helpers of the RF Lab page (pages/rf.js): panels with state overlays, number
// formatting on top of units.js, histogram / axis helpers and the tiny inline sparkline.
//
// Every section module (rf/*.js) exports { id, label, icon, endpoints, build(env) } where
//   endpoints: { key: { path, every (ms), params? } }   fetched only while the tab is visible
//   build(env) -> { el, render(state) }                 state[key] = { data, error, t } (undefined = not loaded yet)

export const ok = (v) => typeof v === 'number' && Number.isFinite(v);
export const clamp = (v, lo, hi) => Math.max(lo, Math.min(hi, v));
export const sum = (a) => (a || []).reduce((s, v) => s + (ok(v) ? v : 0), 0);

/** Status -> badge kind / colour. */
export const STATUS = {
  ok: { badge: 'green', color: '#34d399', icon: 'check-circle', label: 'good' },
  warn: { badge: 'amber', color: '#fbbf24', icon: 'alert', label: 'check' },
  bad: { badge: 'red', color: '#ff4d5e', icon: 'x-circle', label: 'problem' },
  unknown: { badge: 'other', color: '#4a5d82', icon: 'info', label: 'no data' },
};
export const statusOf = (s) => STATUS[s] || STATUS.unknown;

/** Status of a 0..100 score (-1 = no data), same thresholds as the server's rfstat health. */
export function scoreStatus(score) {
  if (!ok(score) || score < 0) return 'unknown';
  if (score >= 80) return 'ok';
  if (score >= 50) return 'warn';
  return 'bad';
}

/**
 * Environment handed to every section: the page context plus formatting and panel helpers.
 * `retry` re-fetches the active tab.
 */
export function makeEnv(ctx, retry) {
  const { ui, units, charts } = ctx;
  const { h } = ui;
  const DASH = units.DASH;

  const f = {
    /** Fraction 0..1 -> "12.3 %". */
    frac: (v, d = 1) => (ok(v) ? units.pct(v * 100, d) : DASH),
    /** Percent value 0..100 -> "12.3 %". */
    pct: (v, d = 1) => (ok(v) ? units.pct(v, d) : DASH),
    db: (v, d = 1) => (ok(v) ? units.db(v, d) : DASH),
    dbs: (v, d = 1) => (ok(v) ? (v > 0 ? '+' : '') + units.db(v, d) : DASH),
    dbfs: (v, d = 1) => units.dbfs(v, { digits: d }),
    rate: (v, d) => units.rate(v, d),
    sec: (v, d) => (ok(v) ? units.num(v, d ?? (Math.abs(v) < 10 ? 2 : 1)) + ' s' : DASH),
    us: (v) => (ok(v) ? units.num(v, 1) + ' µs' : DASH),
    int: (v) => units.int(v),
    num: (v, d = 1) => units.num(v, d),
    dist: (km) => units.dist(km),
    /** Small error distances (prediction): metres / feet below 1 km, distance units above. */
    err: (km) => {
      if (!ok(km)) return DASH;
      return Math.abs(km) < 1 ? units.height(km * 1000, { digits: 0 }) : units.dist(km);
    },
    /** km band "50–100 km" in the user's distance unit; hi null = open. */
    band: (lo, hi) => {
      const u = units.label('dist');
      const v = (km) => units.num(units.value('dist', km), 0);
      if (!ok(hi)) return `> ${v(lo)} ${u}`;
      return `${v(lo ?? 0)}–${v(hi)} ${u}`;
    },
  };

  /**
   * A card whose body is a chart (default) or custom content, with loading / empty / error overlays.
   * o: { title, icon, sub, actions, class, height, content, top, caption, accent }
   */
  function panel(o) {
    const main = o.content || h('div.chart', { style: { '--h': (o.height || 260) + 'px' } });
    const overlay = h('div.rf-overlay');
    const extra = h('div.rf-extra');
    const card = ui.card({
      title: o.title, icon: o.icon, sub: o.sub ?? '', actions: o.actions, class: o.class, accent: o.accent,
      body: [o.top || null, h('div.rf-wrap', main, overlay), o.caption ? h('p.rf-caption', o.caption) : null, extra],
    });
    let chart = null;
    let shown = '';
    const set = (key, node) => {
      if (shown === key) return;
      shown = key;
      ui.mount(overlay, node);
      overlay.classList.add('on');
    };
    return {
      card, main, extra, overlay,
      get chart() {
        if (!chart) {
          chart = charts.create(main);
          const set = chart.set.bind(chart);
          chart.set = (opt, o) => set(roomForLegend(opt), o);
        }
        return chart;
      },
      loading: () => set('loading', ui.loading('Loading…', { compact: true })),
      empty: (title, text, icon) => set('empty:' + title + text, ui.empty({ icon: icon || 'wave', title, text, compact: true })),
      error: (err) => set('error:' + (err && err.message), ui.error(err, { retry, compact: true, title: 'Could not load' })),
      ready: () => { if (!shown) return; shown = ''; overlay.classList.remove('on'); ui.clear(overlay); },
    };
  }

  /**
   * Drive a panel from one data source: loading -> error -> render. render(data) may return
   * { empty: [title, text, icon] } to show the "not enough data yet" state.
   */
  function show(p, src, render) {
    if (src && src.disabled && !src.data) return p.empty('Switched off', src.disabled, 'power');
    if (!src || (!src.data && !src.error)) return p.loading();
    if (src.error && !src.data) return p.error(src.error);
    let res;
    try { res = render(src.data); } catch (e) { console.error('[rf] render failed', e); return p.error(e); }
    if (res && res.empty) p.empty(res.empty[0], res.empty[1], res.empty[2]); else p.ready();
    return undefined;
  }

  const notEnough = (what) => ['Not enough data yet', what];

  /** Inline SVG sparkline (cheap: no chart instance per table row). */
  function spark(values, o = {}) {
    const w = o.width || 120;
    const hgt = o.height || 22;
    const vals = (values || []).map((v) => (ok(v) ? v : 0));
    const max = Math.max(1, ...vals);
    const n = Math.max(1, vals.length - 1);
    const pts = vals.map((v, i) => `${((i / n) * w).toFixed(1)},${(hgt - 1 - (v / max) * (hgt - 3)).toFixed(1)}`).join(' ');
    const svg = document.createElementNS('http://www.w3.org/2000/svg', 'svg');
    svg.setAttribute('viewBox', `0 0 ${w} ${hgt}`);
    svg.setAttribute('class', 'rf-spark');
    svg.setAttribute('preserveAspectRatio', 'none');
    if (vals.length > 1) {
      const area = document.createElementNS(svg.namespaceURI, 'polygon');
      area.setAttribute('points', `0,${hgt} ${pts} ${w},${hgt}`);
      area.setAttribute('class', 'rf-spark-area');
      const line = document.createElementNS(svg.namespaceURI, 'polyline');
      line.setAttribute('points', pts);
      line.setAttribute('class', 'rf-spark-line');
      svg.append(area, line);
    }
    return svg;
  }

  /** Finding / advice row. level: ok|warn|bad|info */
  function finding(level, text) {
    const st = level === 'info' ? { icon: 'info' } : statusOf(level);
    return h('div.rf-finding.' + (level || 'info'), ui.icon(st.icon), h('div', text));
  }

  /** Meter with a coloured status (0..100). */
  function scoreMeter(score) {
    const m = ui.meter(ok(score) && score >= 0 ? score : 0);
    m.classList.add('rf-meter', 'rf-meter--' + scoreStatus(score));
    return m;
  }

  /** Value axis 0..100 % (Pd, shares). */
  const pctAxis = (o = {}) => ({ type: 'value', min: 0, max: 100, name: o.name, interval: o.interval, axisLabel: { formatter: (v) => units.pct(v, 0) } });
  /** Axis label formatter for numbers in the user's units. */
  const axNum = (d = 0) => (v) => units.num(v, d);

  return { ctx, ui, h, units, charts, C: charts.colors, DASH, f, panel, show, notEnough, spark, finding, scoreMeter, pctAxis, axNum, retry };
}

/** A visible top legend and axis names share the top edge: push the plot down so they do not overlap. */
function roomForLegend(opt) {
  if (opt && opt.legend && opt.legend.show && opt.grid && !Array.isArray(opt.grid) && (opt.grid.top ?? 0) < 48) opt.grid = { ...opt.grid, top: 48 };
  return opt;
}

/** Histogram -> [[binCentre, count], ...]; empty array when there is nothing. */
export function histPoints(hist, scale = 1) {
  if (!hist || !ok(hist.lo) || !ok(hist.step) || !Array.isArray(hist.counts)) return [];
  return hist.counts.map((c, i) => [hist.lo + (i + 0.5) * hist.step, ok(c) ? c * scale : 0]);
}

/** Total of a histogram's counts. */
export const histTotal = (hist) => sum(hist && hist.counts);

/** Bin centres of a PdAxis (lo/hi parallel arrays). Open bins (null) get a centre one step outside. */
export function axisCentres(ax) {
  const lo = ax?.lo || [];
  const hi = ax?.hi || [];
  return lo.map((l, i) => {
    const a = ok(l) ? l : null;
    const b = ok(hi[i]) ? hi[i] : null;
    if (a !== null && b !== null) return (a + b) / 2;
    if (a === null && b !== null) return b - 0.5;
    if (a !== null) return a + 0.5;
    return null;
  });
}

/** True when an array has at least one finite number. */
export const anyNum = (a) => Array.isArray(a) && a.some(ok);
