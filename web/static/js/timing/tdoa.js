// timing/tdoa.js - "Time difference of arrival" section of the Timing page (pages/timing.js).
//
// The latency chain of every ADS-B position: time of applicability -> transmit -> arrival at the antenna
// (Radarcape GPS time stamp) -> socket read (NTP-corrected host clock) -> handled by the tracker.
// See internal/latency/WIRING.md section 4 for the eight parts shown here.
//
// Endpoints:  chain + histograms come with /api/timing/latency (polled by the page, handed in via setLatency),
//             /api/timing/latency/aircraft (5 s), /api/timing/latency/aircraft/{icao} (5 s, selected aircraft),
//             /api/timing/ntpcheck (10 s), /api/timing/latency/feeds/{id}?since= (60 s, minute series).
// Everything is polled only while the browser tab is visible.

import { chainRef } from './ref.js';

const ok = (v) => typeof v === 'number' && Number.isFinite(v);
const DAY_MS = 24 * 3600 * 1000;
const SETTINGS_TIME = '#/settings/time';

/** Stage colours (match charts.colors / tokens.css). */
const SC = {
  positionAge: '#a78bfa', propagation: '#22d3ee', receiver: '#3b8bff', processing: '#fbbf24',
  transponder: '#2dd4bf', ageDelta: '#a78bfa', airToSystem: '#34d399', endToEnd: '#6aa8ff',
  aircraftTransponder: '#2dd4bf', aircraftAgeJitter: '#a78bfa',
};
const RED = '#ff4d5e';
const GREEN = '#34d399';

const METHOD = {
  exact: { kind: 'green', tip: 'Exact: derived from GPS time stamps and geometry, no model involved' },
  measured: { kind: 'cyan', tip: 'Measured directly on this station (host clock, NTP-corrected where it matters)' },
  computed: { kind: 'blue', tip: 'Computed from other measured values' },
  estimated: { kind: 'amber', tip: 'Estimated from a model (hatched): not directly observable for this message' },
  assumed: { kind: 'amber', tip: 'Assumed typical value (hatched): nothing on this station can observe it' },
  gated: { kind: 'other', tip: 'Measured but rejected by a plausibility gate' },
  unavailable: { kind: 'other', tip: 'Not available for this message' },
};
const HATCHED = new Set(['estimated', 'assumed']);

const FLAGS = {
  't1-not-synchronised': ['T=1 not synchronised', 'red', 'The aircraft sets T=1 but its transmit times spread over the whole ±200 ms ambiguity window: the transponder is not really coupled to UTC'],
  't1-out-of-tolerance': ['T=1 > 150 ms', 'red', 'Mean transponder latency outside the ±150 ms tolerance'],
  't1-outliers': ['T=1 outliers', 'amber', 'Some T=1 positions are far from the aircraft\'s usual latency'],
  't1-unexpected-tc': ['T=1 unexpected TC', 'amber', 'T=1 seen in a message type that should not carry it'],
  'age-over-0.6s-est': ['age > 0.6 s (est.)', 'amber', 'Estimated position age above the 0.6 s uncompensated limit (model estimate)'],
  'age-jitter-high': ['age jitter high', 'amber', 'Position-age scatter above 100 ms (a 200 ms update register alone gives about 58 ms)'],
  'stale-positions': ['stale positions', 'amber', 'The same position was repeated while the aircraft was moving'],
};

const QUALITY = {
  good: ['good', 'green', 'The NTP reference is accurate enough for sub-millisecond receiver-delay figures'],
  degraded: ['degraded', 'amber', 'Usable, but the offset uncertainty widens every absolute number below'],
  bad: ['bad', 'red', 'The NTP reference is not trustworthy: absolute delays below are unreliable'],
  unknown: ['unknown', 'other', 'No NTP reference to judge (none configured or no answer yet)'],
};
const FEED_STATUS = { ok: ['ok', 'green'], warn: ['check', 'amber'], bad: ['problem', 'red'], unknown: ['no data', 'other'] };
const FINDING = { info: ['info', 'blue-hi'], warn: ['alert', 'amber'], bad: ['x-circle', 'red'] };

/** Histogram order and captions. */
const HISTS = [
  ['receiver', 'Receiver → host: GPS arrival time at the antenna (start of the frame) to the NTP-corrected socket read. Negative bins (red) are impossible physically: the host clock is behind GPS.', true],
  ['propagation', 'Radio propagation: slant range / radio speed, 3.34 µs per km. Exact geometry.', false],
  ['transponder', 'T=1 only: transmit time minus the 0.2 s UTC epoch the position applies to. Shaded: the ±150 ms tolerance.', false],
  ['ageDelta', 'T=0: change of the position age between consecutive positions (kinematic pair method). Its spread is the observable part of the age.', false],
  ['aircraftTransponder', 'Mean T=1 transponder latency per aircraft (10 ms bins). Shaded: the ±150 ms tolerance; outside it the aircraft is flagged.', false],
  ['processing', 'AirView\'s own time from socket read to tracker handled (both host clock).', true],
  ['airToSystem', 'Propagation + receiver → host + processing: transmit to handled.', true],
  ['endToEnd', 'T=1 only: time of applicability to handled by the tracker.', false],
  ['aircraftAgeJitter', 'Position-age jitter (σ) per aircraft, 5 ms bins. A 200 ms update register alone gives about 58 ms.', false],
];
const HIST_EMPTY = {
  transponder: 'No T=1 position in the window. Only about 1 % of aircraft couple their transponder to UTC.',
  aircraftTransponder: 'Needs at least 3 T=1 positions from an aircraft. Only about 1 % of aircraft set T=1.',
  endToEnd: 'Needs T=1 positions (about 1 % of aircraft).',
};

// symmetric log transform for signed log-binned histograms: linear within ±100 ns, one unit per decade above
const LIN0 = 1e-4; // ms
const sl = (x) => (Math.abs(x) <= LIN0 ? x / LIN0 : Math.sign(x) * (Math.log10(Math.abs(x)) + 5));
const isl = (v) => (Math.abs(v) <= 1 ? v * LIN0 : Math.sign(v) * Math.pow(10, Math.abs(v) - 5));

/** "2026-11-13T00:35:01.675460718Z" -> "00:35:01.675 460 718" */
function utcText(s) {
  const m = /T(\d\d:\d\d:\d\d)(?:\.(\d+))?/.exec(String(s || ''));
  if (!m) return '';
  const f = (m[2] || '').padEnd(9, '0').slice(0, 9);
  return `${m[1]}.${f.slice(0, 3)} ${f.slice(3, 6)} ${f.slice(6, 9)}`;
}

/**
 * Build the section.  env: { fmtMs(ms, {sign}), chartBox(title, caption) } from pages/timing.js
 * Returns { el, setLatency(latReply), refresh() }.
 */
export function createTdoa(ctx, env) {
  const { api, units, ui, charts } = ctx;
  const { h } = ui;
  const { fmtMs, chartBox } = env;
  const DASH = units.DASH;
  const prefs = ui.prefs('airview.timing.tdoa', { feed: '' });
  let alive = true;
  ctx.onCleanup(() => { alive = false; });

  const pm = (v, unc) => (ok(unc) && unc > 0 ? `${fmtMs(v)} ± ${fmtMs(unc)}` : fmtMs(v));
  const signed = (v) => fmtMs(v, { sign: true });
  const yMs = (v) => { const a = Math.abs(v); return units.num(v, a === 0 ? 0 : a < 0.1 ? 3 : a < 1 ? 2 : a < 10 ? 1 : 0); };

  // ---- state ----
  let chain = null;          // latency.Summary (null = analyzer missing)
  let hists = [];            // []latency.Histogram
  let rows = null;           // []AircraftRow
  let selected = '';         // '' = automatic choice
  let shown = '';            // icao of the waterfall on screen
  let detail = null;         // {row, waterfall}
  let check = null;          // latency.NTPCheck
  let feedIds = [];
  let feed = prefs.get().feed || '';
  let series = null;         // feed minute series

  // ==========================================================================================
  // reference banner: the NTP server is the yardstick of every absolute number here
  // ==========================================================================================
  const refHost = h('div.tdoa-ref');
  function paintRef() {
    const n = (check && check.ntp) || {};
    const q = QUALITY[(check && check.quality) || 'unknown'] || QUALITY.unknown;
    const on = !!(n.enabled && n.server);
    const kind = !on || !n.synced ? 'warn' : q[1] === 'red' ? 'bad' : q[1] === 'amber' ? 'warn' : 'ok';
    refHost.className = 'tdoa-ref tdoa-ref--' + kind;
    const head = on
      ? h('div.tdoa-ref-title', 'Reference: ', h('span.num', n.server), ' ', ui.badge(n.synced ? 'quality ' + q[0] : 'not answering', n.synced ? q[1] : 'red', { tip: q[2] }),
        n.synced && ok(n.stratum) ? h('span.muted.tdoa-ref-meta', ` stratum ${units.int(n.stratum)}${n.refId ? ' · ' + n.refId : ''}${check && ok(check.offsetUncertaintyMs) ? ' · offset ± ' + fmtMs(check.offsetUncertaintyMs) : ''}`) : null)
      : h('div.tdoa-ref-title', 'No NTP reference entered');
    ui.mount(refHost, ui.icon(kind === 'ok' ? 'check-circle' : 'alert'),
      h('div.grow', head,
        h('div.tdoa-ref-text', 'Every absolute number in this section - receiver delay, transponder latency, end-to-end time - compares the Radarcape\'s GPS time stamps with this computer\'s clock corrected by the NTP server you entered by hand under Settings → Time. The figures are only as good as that server: use one that is itself GPS-locked (stratum 1, reference GPS or PPS) on your own network, for example the Radarcape\'s own NTP service. Propagation and the T=1 transponder latency come from GPS time and geometry alone and do not depend on it.')),
      ui.button({ label: 'Settings → Time', icon: 'settings', size: 'sm', href: SETTINGS_TIME }));
  }

  // ==========================================================================================
  // 1. stage waterfall
  // ==========================================================================================
  const acSelect = ui.select({ size: 'sm', options: [{ value: '', label: 'Automatic' }], onChange: (v) => { selected = v; loadDetail(); }, 'aria-label': 'Aircraft for the waterfall' });
  const wfHost = h('div.tdoa-wf');
  const wfCard = ui.card({ title: 'Stage waterfall', icon: 'route', sub: 'Latest position message of the selected aircraft', actions: [acSelect], body: wfHost });

  /** Log-compressed segment width: 1 µs and 100 ms both stay visible. */
  const weight = (ms) => (ok(ms) ? Math.max(0.7, Math.log10(1 + Math.abs(ms) / 0.001)) : 0.7);

  function paintWaterfall() {
    if (!detail || !detail.waterfall) {
      const none = rows && !rows.length;
      ui.mount(wfHost, ui.empty({ icon: 'route', compact: true, title: none ? 'No positions yet' : 'Waiting for data',
        text: none ? 'The waterfall appears as soon as an aircraft with an ADS-B position has been heard. It needs a feed with GPS time stamps (a Radarcape in GPS mode).' : 'Loading the latest position message…' }));
      return;
    }
    const w = detail.waterfall;
    const r = detail.row || {};
    const stages = w.stages || [];
    const events = w.events || [];
    wfCard.setSub(`${w.icao} · DF${w.df} TC ${w.tc} · ${w.cprOdd ? 'odd' : 'even'} CPR · feed ${w.feed || DASH} · ${units.dist(w.slantKm)} slant, ${ok(w.elevDeg) ? units.num(w.elevDeg, 1) + '°' : DASH} elevation · ${units.ago(w.t, ctx.live.serverNow())}`);

    const weights = stages.map((s) => weight(s.ms));
    const total = weights.reduce((a, b) => a + b, 0) || 1;
    const absSum = stages.reduce((a, s) => a + (ok(s.ms) ? Math.abs(s.ms) : 0), 0) || 1;

    const segs = stages.map((s, i) => {
      const m = METHOD[s.method] || METHOD.unavailable;
      const na = s.method === 'unavailable' || s.method === 'gated' || !ok(s.ms);
      const uncFrac = ok(s.uncMs) && ok(s.ms) && Math.abs(s.ms) > 0 ? Math.min(0.5, s.uncMs / Math.abs(s.ms)) : 0;
      return h('div.tdoa-seg' + (HATCHED.has(s.method) ? '.tdoa-seg--hatch' : '') + (na ? '.tdoa-seg--na' : '') + (ok(s.ms) && s.ms < 0 ? '.tdoa-seg--neg' : ''),
        { style: { flexGrow: String(weights[i]), '--c': SC[s.key] || '#8494b3' }, tip: `${s.label}: ${na ? 'not available' : pm(s.ms, s.uncMs)} · ${s.method}. ${m.tip}` },
        uncFrac > 0 ? h('span.tdoa-seg-unc', { style: { width: (uncFrac * 100).toFixed(1) + '%' } }) : null,
        h('span.tdoa-seg-text', na ? DASH : fmtMs(s.ms)));
    });
    let acc = 0;
    const numbered = events.length === stages.length + 1; // tick i = event i
    const ticks = [0, ...weights].map((wt, i) => { acc += i ? wt : 0; return h('span.tdoa-tick', { style: { left: ((acc / total) * 100).toFixed(2) + '%' }, tip: numbered ? events[i].label : null }, h('i'), numbered ? h('b', String(i + 1)) : null); });
    const scale = stages.map((s) => h('div.tdoa-scale-seg', { style: { width: ((ok(s.ms) ? Math.abs(s.ms) : 0) / absSum * 100).toFixed(3) + '%', '--c': SC[s.key] || '#8494b3' } }));

    const legend = h('div.tdoa-wf-legend', stages.map((s) => {
      const m = METHOD[s.method] || METHOD.unavailable;
      const share = ok(s.ms) ? Math.abs(s.ms) / absSum * 100 : null;
      return h('div.tdoa-leg-row',
        h('span.tdoa-swatch' + (HATCHED.has(s.method) ? '.tdoa-swatch--hatch' : ''), { style: { '--c': SC[s.key] || '#8494b3' } }),
        h('span.tdoa-leg-label', s.label),
        ui.badge(s.method, m.kind, { tip: m.tip }),
        h('span.num.tdoa-leg-val', ok(s.ms) ? fmtMs(s.ms) : DASH),
        h('span.num.muted.tdoa-leg-unc', ok(s.uncMs) && s.uncMs > 0 ? '± ' + fmtMs(s.uncMs) : ok(s.ms) ? '± ?' : ''),
        h('span.num.muted.tdoa-leg-share', ok(share) ? units.num(share, share < 1 ? 2 : 0) + ' %' : ''));
    }));

    const evList = h('ol.tdoa-events', events.map((e, i) => h('li',
      h('b.tdoa-ev-n', String(i + 1)),
      h('span.tdoa-ev-label', e.label),
      h('span.num.tdoa-ev-at', i === 0 ? '0' : '+' + fmtMs(e.atMs)),
      h('span.num.muted.tdoa-ev-utc', utcText(e.utc) + ' UTC'),
      h('span.num.muted.tdoa-ev-unc', ok(e.uncMs) ? (e.uncMs > 0 ? '± ' + fmtMs(e.uncMs) : 'exact') : '± ?'))));

    const xp = ok(r.xpdrMeanMs) && r.xpdrN > 0;
    ui.mount(wfHost,
      h('div.tdoa-wf-head',
        h('div.tdoa-wf-total', h('span.tdoa-wf-total-v.num', fmtMs(w.totalMs)), h('span.tdoa-wf-total-u.num', ok(w.totalUncMs) ? '± ' + fmtMs(w.totalUncMs) : '± ?')),
        h('div.tdoa-wf-what', h('div', 'Time of applicability → handled by AirView'),
          h('div.row.gap-2.tdoa-wf-badges',
            w.tFlag ? ui.badge('T=1 · UTC-coupled', 'teal', { tip: 'The position applies to an exact 0.2 s UTC epoch: transponder latency measured exactly' }) : ui.badge('T=0', 'other', { tip: 'Time of applicability not transmitted: the position age is a model estimate' }),
            w.exact ? ui.badge('exact chain', 'green') : ui.badge('contains estimates', 'amber', { tip: 'Hatched segments are model estimates or assumptions' }),
            xp ? ui.badge(`aircraft mean ${signed(r.xpdrMeanMs)}`, 'teal', { tip: `Mean T=1 transponder latency over ${units.int(r.xpdrN)} positions, σ ${fmtMs(r.xpdrStdMs)}` }) : null))),
      h('div.tdoa-wf-track', h('div.tdoa-wf-bar', segs), h('div.tdoa-ticks', ticks)),
      h('div.tdoa-scale', { tip: 'The same stages drawn to scale (linear): the microsecond stages vanish next to the millisecond ones' }, h('span.tdoa-scale-label', 'to scale'), h('div.tdoa-scale-bar', scale)),
      h('div.tm-cap', 'Segment widths are log-compressed (propagation is microseconds, the other stages milliseconds); the thin strip beneath is the linear truth. Hatched = estimated or assumed; the lighter end of a segment is its ± uncertainty.'),
      h('div.grid.cols-2.tdoa-wf-grid', legend, evList),
      (w.notes || []).length ? h('ul.tdoa-notes', w.notes.map((n) => h('li', n))) : null);
  }

  // ==========================================================================================
  // 2. stage cards
  // ==========================================================================================
  const stageHost = h('div.grid-auto.tdoa-stages', { style: { '--min': '200px' } });
  const stageCards = new Map();
  function stageCard(key) {
    const v = h('span.tdoa-st-v.num');
    const u = h('span.tdoa-st-u');
    const label = h('span');
    const badge = h('span');
    const unc = h('span.num');
    const cnt = h('span.num');
    const small = [h('span.num'), h('span.num'), h('span.num')];
    const el = h('div.tdoa-stage', { style: { '--c': SC[key] || '#8494b3' } },
      h('div.tdoa-st-head', label, badge),
      h('div.tdoa-st-main', v, u),
      h('div.tdoa-st-small', h('span', 'min ', small[0]), h('span', 'p95 ', small[1]), h('span', 'p99 ', small[2])),
      h('div.tdoa-st-foot', cnt, unc));
    return {
      el,
      paint(s) {
        // the receiver stage starts at the start of the frame (the per-feed latency above at the receiver's raw stamp)
        const ref = key === 'receiver' ? chainRef(chain) : null;
        ui.setText(label, ref ? `${s.label} (from ${ref === 'start' ? 'start' : 'end'} of frame)` : s.label);
        el.setAttribute('data-tip', s.description || '');
        const has = s.count > 0 && ok(s.p50Ms);
        const parts = has ? fmtMs(s.p50Ms).split(' ') : [DASH, ''];
        ui.setText(v, parts[0]); ui.setText(u, parts[1] || '');
        ui.setText(small[0], has ? fmtMs(s.minMs) : DASH);
        ui.setText(small[1], has ? fmtMs(s.p95Ms) : DASH);
        ui.setText(small[2], has ? fmtMs(s.p99Ms) : DASH);
        ui.setText(cnt, `${units.int(s.count)} sample${s.count === 1 ? '' : 's'}`);
        ui.setText(unc, ok(s.uncertaintyMs) ? '± ' + fmtMs(s.uncertaintyMs) : '± ?');
        const sig = (s.exact ? 'x' : 'e') + has;
        if (badge.__s !== sig) { badge.__s = sig; ui.mount(badge, s.exact ? ui.badge('exact', 'green', { tip: 'Measured from GPS time stamps, the NTP-corrected clock or geometry' }) : ui.badge('est.', 'amber', { tip: 'Model estimate' })); }
        el.classList.toggle('tdoa-stage--empty', !has);
      },
    };
  }
  function paintStages() {
    if (!chain) {
      stageCards.clear();
      ui.mount(stageHost, ui.empty({ icon: 'timer', compact: true, title: 'Latency analysis not available', text: 'The server did not return a latency chain.' }));
      return;
    }
    if (!stageCards.size) stageHost.replaceChildren();
    for (const s of chain.stages || []) {
      let c = stageCards.get(s.key);
      if (!c) { c = stageCard(s.key); stageCards.set(s.key, c); stageHost.appendChild(c.el); }
      c.paint(s);
    }
  }

  // ==========================================================================================
  // 3. histograms
  // ==========================================================================================
  const histBoxes = new Map();
  const histGrid = h('div.grid.cols-3.tdoa-hists');
  for (const [key, cap] of HISTS) {
    const b = chartBox(key, cap);
    b.el.querySelector('.chart').style.setProperty('--h', '200px');
    histBoxes.set(key, b);
    histGrid.appendChild(b.el);
  }
  const histInfo = h('span.muted.tm-info');

  function histOption(hg, redNeg) {
    const log = hg.scale === 'log';
    let bins = (hg.bins || []).filter((b) => ok(b.lo) && ok(b.hi));
    const tol = hg.key === 'aircraftTransponder' || hg.key === 'transponder';
    if (!log && hg.key === 'aircraftAgeJitter') {
      let last = 0; bins.forEach((b, i) => { if (b.n > 0) last = i; });
      bins = bins.slice(0, Math.max(last + 2, 12));
    }
    const color = SC[hg.key] || charts.colors.blue;
    const data = bins.map((b) => {
      const neg = redNeg && b.hi <= 0;
      return { value: log ? [sl(b.lo), sl(b.hi), b.n, b.lo, b.hi] : [b.lo, b.hi, b.n, b.lo, b.hi], itemStyle: { color: neg ? RED : color } };
    });
    const x = { type: 'value', scale: true, axisLabel: { formatter: (v) => fmtMs(log ? isl(v) : v) }, splitLine: { show: false } };
    if (log && bins.length) {
      let lo = sl(bins[0].lo);
      let hi = sl(bins[bins.length - 1].hi);
      if (tol) { lo = Math.min(lo, sl(-200)); hi = Math.max(hi, sl(200)); }
      const span = hi - lo;
      const iv = span > 4 ? 1 : span > 1.5 ? 0.5 : 0.25;
      x.min = Math.floor(lo / iv) * iv; x.max = Math.ceil(hi / iv) * iv; x.interval = iv;
    } else if (bins.length) {
      x.min = bins[0].lo; x.max = bins[bins.length - 1].hi;
      if (hg.key === 'aircraftTransponder') x.interval = 50;
    }
    const s = {
      type: 'custom', name: hg.label, data, encode: { x: [0, 1], y: 2 },
      renderItem: (params, a) => {
        const p0 = a.coord([a.value(0), a.value(2)]);
        const p1 = a.coord([a.value(1), 0]);
        const wpx = p1[0] - p0[0];
        return { type: 'rect', shape: { x: p0[0] + (wpx > 3 ? 0.5 : 0), y: p0[1], width: Math.max(1, wpx - (wpx > 3 ? 1 : 0)), height: p1[1] - p0[1] }, style: { fill: a.visual('color') } };
      },
    };
    if (tol) {
      const f = log ? sl : (v) => v;
      s.markArea = { silent: true, itemStyle: { color: 'rgba(52,211,153,0.09)', borderColor: 'rgba(52,211,153,0.35)', borderWidth: 1, borderType: 'dashed' },
        label: { show: true, position: 'insideTop', color: GREEN, fontSize: 10, formatter: '±150 ms tolerance' }, data: [[{ xAxis: f(-150) }, { xAxis: f(150) }]] };
    }
    if (redNeg && bins.some((b) => b.hi <= 0 && b.n > 0)) {
      s.markLine = { silent: true, symbol: 'none', lineStyle: { color: RED, type: 'dashed' }, label: { color: RED, fontSize: 10, formatter: '0' }, data: [{ xAxis: 0 }] };
    }
    return {
      animation: false,
      grid: { left: 8, right: 14, top: 14, bottom: 8, containLabel: true },
      tooltip: { trigger: 'item', confine: true, formatter: (p) => `${p.seriesName}<br>${fmtMs(p.value[3])} … ${fmtMs(p.value[4])}<br><b>${units.int(p.value[2])}</b>` },
      xAxis: x,
      yAxis: { type: 'value', minInterval: 1, axisLabel: { formatter: (v) => units.si(v) } },
      series: [s],
    };
  }

  function paintHists() {
    const by = new Map((hists || []).map((x) => [x.key, x]));
    let any = 0;
    for (const [key, , redNeg] of HISTS) {
      const b = histBoxes.get(key);
      const hg = by.get(key);
      b.el.querySelector('.label').textContent = hg ? `${hg.label}${hg.scale === 'log' ? ' · log axis' : ''}` : key;
      const has = hg && hg.count > 0 && (hg.bins || []).some((x) => x.n > 0);
      if (!has) {
        b.chart.set({ series: [] }, { replace: true });
        b.note(h('div', h('div.tm-note-title', 'No data yet'), h('div', HIST_EMPTY[key] || (chain ? 'Nothing measured in the window yet.' : 'The latency analyzer is not available.'))));
        continue;
      }
      any++;
      b.note(null);
      b.chart.set(histOption(hg, redNeg), { replace: true });
    }
    ui.setText(histInfo, chain ? `last ${units.age(chain.windowSec)} · ${any} of ${HISTS.length} with data` : '');
  }

  // ==========================================================================================
  // 4. per-feed receiver delay (24 h)
  // ==========================================================================================
  const feedBox = chartBox('Receiver → host delay per minute', 'Median per minute (line; red markers below zero), the min…p95 band (red when the minute had negative delays) of the minute and the clock-filter floor (dashed green, minimum of 10 s minima). Good: a flat, narrow band a little above zero. Negative minutes mean the NTP-corrected host clock was behind GPS: check the NTP server.');
  feedBox.el.querySelector('.chart').style.setProperty('--h', '280px');
  const feedSegHost = h('div');
  const feedInfo = h('span.muted.tm-info');
  let feedSegSig = '';

  function paintFeedSeg() {
    const sig = feedIds.join('|') + '#' + feed;
    if (sig === feedSegSig) return;
    feedSegSig = sig;
    if (feedIds.length < 2) { feedSegHost.replaceChildren(); return; }
    ui.mount(feedSegHost, ui.segmented({ options: feedIds.map((id) => ({ value: id, label: id })), value: feed, onChange: (v) => { feed = v; prefs.set({ feed }); series = null; loadFeed(); } }));
  }

  function paintFeed() {
    paintFeedSeg();
    const s = (series && series.series) || [];
    if (!feed) {
      feedBox.chart.set({ series: [] }, { replace: true });
      feedBox.note(h('div', h('div.tm-note-title', 'No feed with GPS time stamps'), h('div', 'The per-minute receiver delay needs a feed whose frames carry GPS time (a Radarcape in GPS mode).')));
      ui.setText(feedInfo, '');
      return;
    }
    if (!s.length) {
      feedBox.chart.set({ series: [] }, { replace: true });
      feedBox.note(h('div', h('div.tm-note-title', 'Not enough data yet'), h('div', 'One point is added per minute (kept for 24 h and restored after a restart).')));
      ui.setText(feedInfo, feed);
      return;
    }
    feedBox.note(null);
    const neg = s.some((m) => (ok(m.minMs) && m.minMs < 0) || m.neg > 0);
    const few = s.length < 30;
    const band = s.map((m) => [m.t, ok(m.minMs) ? m.minMs : null, ok(m.p95Ms) ? m.p95Ms : null, m.t + 60000]);
    const opt = {
      animation: false,
      grid: { left: 8, right: 14, top: 34, bottom: s.length > 240 ? 44 : 8, containLabel: true },
      legend: { show: true, top: 0, left: 0, type: 'scroll', data: ['min … p95', 'median', 'floor', ...(neg ? ['negative'] : [])] },
      tooltip: {
        trigger: 'axis', confine: true,
        formatter: (ps) => {
          const i = ps && ps.length ? ps[0].dataIndex : -1;
          const m = s[i];
          if (!m) return '';
          const row = (k, v, c) => `<div style="display:flex;gap:14px;justify-content:space-between"><span${c ? ` style="color:${c}"` : ''}>${k}</span><b>${v}</b></div>`;
          return `${units.clock(m.t, { z: true })} · ${units.int(m.n)} frames` + row('min', fmtMs(m.minMs), m.minMs < 0 ? RED : null) + row('median', fmtMs(m.p50Ms), m.p50Ms < 0 ? RED : null)
            + row('p95', fmtMs(m.p95Ms)) + row('max', fmtMs(m.maxMs)) + row('floor', fmtMs(m.floorMs)) + row('jitter', fmtMs(m.jitterMs))
            + (m.neg || m.absurd ? row('negative / absurd', `${units.int(m.neg)} / ${units.int(m.absurd)}`, RED) : '');
        },
      },
      xAxis: { type: 'time', splitLine: { show: false } },
      yAxis: { type: 'value', scale: true, name: 'ms', axisLabel: { formatter: yMs } },
      series: [
        {
          type: 'custom', name: 'min … p95', data: band, encode: { x: [0, 3], y: [1, 2] }, color: 'rgba(59,139,255,0.5)', z: 1,
          renderItem: (params, a) => {
            if (!ok(a.value(1)) || !ok(a.value(2))) return null;
            const p0 = a.coord([a.value(0), a.value(2)]);
            const p1 = a.coord([a.value(3), a.value(1)]);
            const negMin = a.value(1) < 0;
            return { type: 'rect', shape: { x: p0[0], y: p0[1], width: Math.max(1, p1[0] - p0[0] - (p1[0] - p0[0] > 3 ? 1 : 0)), height: Math.max(1, p1[1] - p0[1]) }, style: { fill: negMin ? 'rgba(255,77,94,0.32)' : 'rgba(59,139,255,0.28)' } };
          },
        },
        { type: 'line', name: 'median', data: s.map((m) => [m.t, ok(m.p50Ms) ? m.p50Ms : null]), color: charts.colors.cyan, showSymbol: few, symbolSize: 4, lineStyle: { width: 1.8 }, z: 3 },
        { type: 'line', name: 'floor', data: s.map((m) => [m.t, ok(m.floorMs) ? m.floorMs : null]), color: GREEN, showSymbol: few, symbolSize: 3, lineStyle: { width: 1.2, type: 'dashed' }, z: 2 },
        // negatives in red: the band of such a minute turns red, and these markers sit on its median and minimum
        ...(neg ? [{ type: 'scatter', name: 'negative', data: s.map((m) => [m.t, ok(m.p50Ms) && m.p50Ms < 0 ? m.p50Ms : (ok(m.minMs) && m.minMs < 0) || m.neg > 0 ? m.minMs : null]), color: RED, symbolSize: 7, z: 4 }] : []),
      ],
    };
    if (neg) opt.series[1].markLine = { silent: true, symbol: 'none', lineStyle: { color: RED, type: 'dashed' }, label: { color: RED, fontSize: 10, formatter: '0 ms' }, data: [{ yAxis: 0 }] };
    if (s.length > 240) opt.dataZoom = [{ type: 'inside', throttle: 50 }, { type: 'slider', height: 18, bottom: 8, showDetail: false }];
    feedBox.chart.set(opt, { replace: true });
    const negMin = s.reduce((n, m) => n + (m.neg > 0 || (ok(m.minMs) && m.minMs < 0) ? 1 : 0), 0);
    ui.setText(feedInfo, `${feed} · ${units.int(s.length)} min${negMin ? ` · ${units.int(negMin)} with negative delays` : ''}`);
  }

  // ==========================================================================================
  // 5. NTP / GPS consistency
  // ==========================================================================================
  const qualityBadge = h('span');
  const ntpKv = h('div');
  const ntpIssues = h('div');
  const ntpFeeds = h('div.col.gap-3');
  const ntpExplain = h('div.tdoa-help');
  const sparks = new Map(); // feedId -> {el, chart, info}

  function paintCheck() {
    paintRef();
    if (!check) return;
    const n = check.ntp || {};
    const q = QUALITY[check.quality] || QUALITY.unknown;
    ui.mount(qualityBadge, ui.badge(q[0], q[1], { tip: q[2], large: true }));
    const on = !!(n.enabled && n.server);
    ui.mount(ntpKv, ui.kv([
      ['Server', on ? n.server : 'not configured', { text: true }],
      ['State', !on ? ui.badge('off', 'other') : n.synced ? ui.badge('synchronised', 'green') : ui.badge(n.lastError ? 'failing' : 'waiting', n.lastError ? 'red' : 'amber'), { text: true }],
      ['Stratum', on && n.synced ? units.int(n.stratum) + (n.stratum === 1 ? ' (reference clock attached)' : '') : DASH, { text: true }],
      ['Reference ID', (on && n.refId) || DASH, { tip: 'GPS or PPS for a GPS-locked stratum 1 server' }],
      ['Offset ± uncertainty', on && n.synced ? `${signed(n.offsetMs)} ± ${ok(check.offsetUncertaintyMs) ? fmtMs(check.offsetUncertaintyMs) : '?'}` : DASH, { tip: 'True UTC minus the host clock, and how far that correction can be off (half the round trip plus root dispersion and jitter)' }],
      ['Round trip / jitter', on && n.synced ? `${fmtMs(n.delayMs)} / ${fmtMs(n.jitterMs)}` : DASH],
      ['Root delay / dispersion', on && n.synced ? `${fmtMs(n.rootDelayMs)} / ${fmtMs(n.rootDispMs)}` : DASH, { tip: 'The server\'s own distance and accumulated error bound to its reference clock' }],
      ['Last answer', n.lastOk > 0 ? units.ago(n.lastOk, ctx.live.serverNow()) : DASH, { text: true }],
      n.lastError ? ['Last error', n.lastError, { text: true, class: 'tm-wrap' }] : null,
    ], { columns: 2 }));

    const issues = check.issues || [];
    ui.mount(ntpIssues, issues.length ? h('ul.tdoa-issues', issues.map((t) => h('li', ui.icon('alert'), h('span', t)))) : null);
    ui.setText(ntpExplain, check.explanation || '');

    const seen = new Set();
    const feeds = check.feeds || [];
    if (!feeds.length) {
      for (const sp of sparks.values()) sp.chart.dispose();
      sparks.clear();
      ui.mount(ntpFeeds, h('div.tm-cap', 'No feed with GPS time stamps: the receiver-delay clock filter needs a Radarcape in GPS mode.'));
      return;
    }
    if (!sparks.size) ntpFeeds.replaceChildren();
    for (const f of feeds) {
      seen.add(f.feedId);
      let sp = sparks.get(f.feedId);
      if (!sp) {
        const chartEl = h('div.tdoa-spark');
        const info = h('div.tdoa-feed-info');
        const head = h('div.tdoa-feed-head');
        const iss = h('div');
        const el = h('div.tdoa-feed', head, chartEl, info, iss);
        sp = { el, head, info, iss, chart: charts.create(chartEl) };
        sparks.set(f.feedId, sp);
        ntpFeeds.appendChild(el);
      }
      const st = FEED_STATUS[f.status] || FEED_STATUS.unknown;
      ui.mount(sp.head, h('span.tdoa-feed-name', 'Feed ', h('span.num', f.feedId)), ui.badge(st[0], st[1]), h('span.muted.tm-info', `10 s minima · ${units.int((f.minima || []).length)} points`));
      const mins = (f.minima || []).filter((p) => ok(p.ms));
      const o = charts.sparklineOption({ data: mins.map((p) => [p.t, p.ms]), color: mins.some((p) => p.ms < 0) ? RED : charts.colors.cyan, tooltip: (v, x) => `${units.clock(x, { z: true })} · ${fmtMs(v)}` });
      if (ok(f.floorMs)) o.series[0].markLine = { silent: true, symbol: 'none', lineStyle: { color: GREEN, type: 'dashed', width: 1 }, label: { show: false }, data: [{ yAxis: f.floorMs }] };
      sp.chart.set(o, { replace: true });
      const drift = ok(f.driftUsPerMin) ? `${f.driftUsPerMin > 0 ? '+' : ''}${units.num(f.driftUsPerMin, 1)} µs/min` : DASH;
      ui.mount(sp.info,
        h('span', 'floor ', h('b.num' + (ok(f.floorMs) && f.floorMs < 0 ? '.tdoa-red' : ''), fmtMs(f.floorMs))),
        h('span', 'jitter ', h('b.num', fmtMs(f.jitterMs))),
        h('span', { tip: 'Slope of the floor: wander of the host clock between NTP polls' }, 'drift ', h('b.num', drift)),
        h('span', 'last ', h('b.num', fmtMs(f.lastMs))),
        h('span', 'negative ', h('b.num' + (f.negative ? '.tdoa-red' : ''), units.int(f.negative))),
        h('span', { tip: 'Delays outside the plausible range (receiver time base not UTC, clock seconds off)' }, 'absurd ', h('b.num' + (f.absurd ? '.tdoa-red' : ''), units.int(f.absurd))));
      ui.mount(sp.iss, (f.issues || []).length ? h('ul.tdoa-issues', f.issues.map((t) => h('li', ui.icon('alert'), h('span', t)))) : null);
    }
    for (const [id, sp] of sparks) if (!seen.has(id)) { sp.chart.dispose(); sp.el.remove(); sparks.delete(id); }
  }

  const ntpCard = ui.card({ title: 'NTP / GPS consistency', icon: 'globe', sub: 'Is the manually entered NTP server good enough to judge the Radarcape\'s GPS time stamps?', actions: [qualityBadge],
    body: h('div.col.gap-4', ntpKv, ntpIssues, ntpFeeds, ntpExplain, h('div.tdoa-help', 'The reference is exactly the server entered under ', h('a', { href: SETTINGS_TIME }, 'Settings → Time'), '. AirView never sets the system clock; it measures the offset and corrects its own figures.')) });

  // ==========================================================================================
  // 6. aircraft table
  // ==========================================================================================
  const tableHost = h('div');
  const tableInfo = h('span.muted.tm-info');
  const table = ui.table({
    class: 'tdoa-table',
    columns: [
      { key: 'icao', label: 'ICAO', num: true, sort: true, render: (r) => h('span.tdoa-icao', r.icao) },
      { key: 'positions', label: 'Msgs / pos.', align: 'right', num: true, sort: (r) => r.positions, sortDir: 'desc', render: (r) => `${units.int(r.n)} / ${units.int(r.positions)}` },
      { key: 'tFlag', label: 'T bit', sort: (r) => (r.t1 > 0 ? 1 : 0), sortDir: 'desc', render: (r) => (r.t1 > 0 ? ui.badge(r.t0 > 0 ? `T=1 ${units.int(r.t1)}/${units.int(r.t1 + r.t0)}` : 'T=1', 'teal') : ui.badge('T=0', 'other')) },
      {
        key: 'lat', label: 'Transponder / age', align: 'right', num: true,
        tip: 'T=1: measured mean transponder latency ± σ. T=0: estimated position age (model, marked est.)',
        sort: (r) => (r.xpdrN > 0 && ok(r.xpdrMeanMs) ? r.xpdrMeanMs : ok(r.positionAgeMs) ? r.positionAgeMs : null), sortDir: 'desc',
        render: (r) => (r.xpdrN > 0 && ok(r.xpdrMeanMs)
          ? h('span', signed(r.xpdrMeanMs), h('span.muted', ok(r.xpdrStdMs) ? ' ± ' + fmtMs(r.xpdrStdMs) : ''))
          : ok(r.positionAgeMs) ? h('span', fmtMs(r.positionAgeMs), r.ageExact ? null : h('span.tdoa-est', ' est.')) : DASH),
      },
      { key: 'ageJitterMs', label: 'Age jitter', align: 'right', num: true, sort: (r) => (ok(r.ageJitterMs) ? r.ageJitterMs : null), sortDir: 'desc', render: (r) => fmtMs(r.ageJitterMs), tip: 'Scatter (σ) of the position age: observable for every aircraft' },
      { key: 'prop', label: 'Propagation', align: 'right', num: true, sort: (r) => (ok(r.propMeanUs) ? r.propMeanUs : null), sortDir: 'desc', render: (r) => (ok(r.propMeanUs) ? fmtMs(r.propMeanUs / 1000) : DASH) },
      { key: 'slantKm', label: 'Slant', align: 'right', num: true, sort: (r) => (ok(r.slantKm) ? r.slantKm : null), sortDir: 'desc', render: (r) => units.dist(r.slantKm) },
      { key: 'flags', label: 'Flags', sort: (r) => (r.flags || []).length, sortDir: 'desc', render: (r) => ((r.flags || []).length ? h('span.tdoa-chips', r.flags.map((f) => { const d = FLAGS[f] || [f, 'other', f]; return ui.badge(d[0], d[1], { tip: d[2] }); })) : '') },
    ],
    rows: [],
    sort: { key: 'positions', dir: 'desc' },
    rowClass: (r) => (r.icao === shown ? 'tdoa-sel' : ''),
    onRowClick: (r) => { selected = r.icao; acSelect.value = r.icao; loadDetail(); wfCard.scrollIntoView({ behavior: 'smooth', block: 'start' }); },
    empty: 'No aircraft with positions yet',
  });
  tableHost.append(table);

  function autoPick() {
    if (!rows || !rows.length) return '';
    const t1 = rows.filter((r) => r.t1 > 0).sort((a, b) => b.positions - a.positions)[0];
    return (t1 || [...rows].sort((a, b) => b.positions - a.positions)[0]).icao;
  }

  function paintTable() {
    if (!rows) return;
    table.setRows(rows);
    const t1 = rows.filter((r) => r.t1 > 0).length;
    ui.setText(tableInfo, `${units.int(rows.length)} aircraft · ${units.int(t1)} with T=1`);
    const opts = [{ value: '', label: `Automatic${shown && !selected ? ' (' + shown + ')' : ''}` }, ...rows.slice(0, 200).map((r) => ({ value: r.icao, label: `${r.icao}${r.t1 > 0 ? ' · T=1' : ''} · ${units.int(r.positions)} pos.` }))];
    if (selected && !rows.some((r) => r.icao === selected)) opts.push({ value: selected, label: selected + ' (gone)' });
    acSelect.setOptions(opts);
    acSelect.value = selected;
  }

  // ==========================================================================================
  // 7. findings  ·  8. explanation
  // ==========================================================================================
  const findHost = h('div');
  function paintFindings() {
    const list = (chain && chain.findings) || [];
    if (!list.length) { ui.mount(findHost, ui.empty({ icon: 'check-circle', compact: true, title: chain ? 'No findings yet' : 'Not available', text: chain ? 'Findings appear after the first positions with GPS time stamps.' : '' })); return; }
    const order = { bad: 0, warn: 1, info: 2 };
    ui.mount(findHost, h('ul.tdoa-findings', [...list].sort((a, b) => (order[a.level] ?? 3) - (order[b.level] ?? 3)).map((f) => {
      const d = FINDING[f.level] || FINDING.info;
      return h('li.tdoa-finding.tdoa-finding--' + (f.level || 'info'), ui.icon(d[0]), h('span', f.text));
    })));
  }

  const ex = (term, ...text) => h('div.tm-explain-item', h('dt', term), h('dd', ...text));
  const explainCard = ui.card({
    title: 'How the latency chain is measured', icon: 'help',
    body: h('dl.tm-explain',
      ex('T=1: exact', 'An ADS-B position with the T bit set (T=1) applies to an exact 0.2 s UTC epoch; the even/odd CPR format bit says which one. With the Radarcape\'s GPS time of arrival and the computed propagation time, the transponder\'s own delay (transmit minus epoch) is measured exactly. Only about 1 % of aircraft use it.'),
      ex('T=0: only the jitter is observable', 'For T=0 positions the aircraft does not say when its position was valid. Only the variation of the position age can be seen in its own reports (consecutive positions against ground speed and GPS arrival times). The mean age shown is a model estimate (marked "est.", hatched in the waterfall), not a measurement.'),
      ex('Receiver → host', 'GPS arrival time stamp, moved back to the start of the frame (the Radarcape stamps its end; 64 µs for short, 120 µs for long frames), against the moment AirView read the frame, with the host clock corrected by NTP. The per-feed "Feed latency" above uses the raw end-of-frame stamp, so it is shorter by that frame length. Accurate only as far as the NTP server you entered under ', h('a', { href: SETTINGS_TIME }, 'Settings → Time'), ' is accurate: it must itself be GPS-locked (stratum 1, reference GPS/PPS), ideally on the local network.'),
      ex('Limits for context', '14 CFR 91.227 (FAA ADS-B Out performance): total latency of the position at most 2.0 s, of which at most 0.6 s uncompensated. DO-260B budgets the uncompensated latency at −200 … +400 ms. A ground station only sees part of these: the aircraft-internal delays before transmission are invisible for T=0.')),
  });

  // ==========================================================================================
  // layout
  // ==========================================================================================
  ui.mount(wfHost, ui.skeleton({ lines: 4 }));
  ui.mount(stageHost, ui.skeleton({ lines: 2 }));
  ui.mount(findHost, ui.skeleton({ lines: 3 }));
  ui.mount(ntpKv, ui.skeleton({ lines: 4 }));
  paintRef();

  const el = h('div.tdoa',
    h('div.tm-section', ui.section('Time difference of arrival', 'target')),
    h('div.tm-cap.tdoa-intro', 'Where the time goes between the moment an aircraft\'s position was valid and the moment AirView shows it: position age in the transponder, radio propagation, receiver and network, and AirView itself. Built from the Radarcape\'s GPS time stamps against the NTP-corrected host clock.'),
    refHost,
    h('div.tm-gap', wfCard),
    h('div.tm-gap', stageHost),
    h('div.grid.cols-2.tm-gap', ui.card({ title: 'Findings', icon: 'info', sub: 'What the numbers say, in plain language', body: findHost }), explainCard),
    ui.card({ title: 'Distributions', icon: 'chart', sub: 'Rolling window of the latency analysis', actions: [histInfo], class: 'tm-gap', body: histGrid }),
    h('div.grid.cols-2.tm-gap',
      ui.card({ title: 'Receiver delay, 24 h', icon: 'history', sub: 'Per feed, one point per minute', actions: [feedInfo, feedSegHost], body: feedBox.el }),
      ntpCard),
    ui.card({ title: 'Aircraft', icon: 'plane', sub: 'Click a row to show its waterfall', actions: [tableInfo], class: 'tm-gap', body: tableHost }));

  // ==========================================================================================
  // loaders
  // ==========================================================================================
  const visible = () => !document.hidden;

  async function loadRows() {
    try {
      const d = await api.get('/api/timing/latency/aircraft', { signal: ctx.signal });
      if (!alive) return;
      rows = (d && d.aircraft) || [];
      if (!selected && autoPick() !== shown) loadDetail();
      paintTable();
      if (!rows.length && !detail) paintWaterfall();
    } catch (e) {
      if (!alive || (e && e.aborted) || rows) return;
      ui.mount(tableHost, ui.error(e, { title: 'Could not load the aircraft latency table', retry: () => { ui.mount(tableHost, table); loadRows(); }, compact: true }));
    }
  }

  let detSeq = 0;
  async function loadDetail() {
    const icao = selected || autoPick();
    const my = ++detSeq;
    if (!icao) { detail = null; shown = ''; paintWaterfall(); return; }
    try {
      const d = await api.get('/api/timing/latency/aircraft/' + encodeURIComponent(icao), { signal: ctx.signal });
      if (!alive || my !== detSeq) return;
      detail = d; shown = icao;
      paintWaterfall();
      if (rows) paintTable();
    } catch (e) {
      if (!alive || my !== detSeq || (e && e.aborted)) return;
      if (e && e.status === 404 && selected) {
        ui.toast.info(`${selected} has left the latency table; showing the automatic choice again.`, { title: 'Waterfall' });
        selected = '';
        acSelect.value = '';
        loadDetail();
        return;
      }
      if (!detail) ui.mount(wfHost, ui.error(e, { title: 'Could not load the waterfall', retry: loadDetail, compact: true }));
    }
  }

  async function loadCheck() {
    try {
      const d = await api.get('/api/timing/ntpcheck', { signal: ctx.signal });
      if (!alive) return;
      check = d;
      paintCheck();
    } catch (e) {
      if (!alive || (e && e.aborted) || check) return;
      ui.mount(ntpKv, ui.error(e, { title: 'Could not load the NTP / GPS check', retry: loadCheck, compact: true }));
    }
  }

  let feedSeq = 0;
  async function loadFeed() {
    if (!feed) { paintFeed(); return; }
    const my = ++feedSeq;
    try {
      const d = await api.get('/api/timing/latency/feeds/' + encodeURIComponent(feed), { params: { since: Math.round(ctx.live.serverNow() - DAY_MS) }, signal: ctx.signal });
      if (!alive || my !== feedSeq) return;
      series = d;
      if (Array.isArray(d.feeds) && d.feeds.length) mergeFeedIds(d.feeds);
      paintFeed();
    } catch (e) {
      if (!alive || my !== feedSeq || (e && e.aborted)) return;
      if (e && e.status === 404) { series = { series: [] }; paintFeed(); return; }
      if (!series) { feedBox.chart.set({ series: [] }, { replace: true }); feedBox.note(ui.error(e, { title: 'Could not load the feed series', retry: loadFeed, compact: true })); }
    }
  }

  function mergeFeedIds(ids) {
    const next = [...new Set([...ids])].sort();
    if (next.join('|') !== feedIds.join('|')) feedIds = next;
    if (!feed || !feedIds.includes(feed)) {
      const prev = feed;
      feed = feedIds[0] || '';
      if (feed !== prev) { series = null; loadFeed(); }
    }
  }

  // ---- polling (only while the tab is visible) ----
  loadRows(); loadCheck();
  ctx.every(5000, () => { if (visible()) { loadRows(); if (selected || shown) loadDetail(); } });
  ctx.every(10000, () => { if (visible()) loadCheck(); });
  ctx.every(60000, () => { if (visible()) loadFeed(); });
  ctx.onCleanup(units.onChange(() => { paintWaterfall(); paintTable(); paintCheck(); paintFeed(); paintStages(); paintHists(); }));

  return {
    el,
    /** Called by the page with every /api/timing/latency reply. */
    setLatency(d) {
      chain = (d && d.chain) || null;
      hists = (d && d.histograms) || [];
      paintStages(); paintHists(); paintFindings();
      const ids = [...((chain && chain.feeds) || []).map((f) => f.feedId), ...((d && d.feeds) || []).filter((f) => f.samples > 0).map((f) => f.feedId)];
      const before = feed;
      if (ids.length) mergeFeedIds(ids);
      if (!series && feed && feed === before) loadFeed();
      if (!feed) paintFeed();
    },
    /** Reload everything (tab became visible again). */
    refresh() { loadRows(); loadCheck(); loadFeed(); if (selected || shown) loadDetail(); },
  };
}
