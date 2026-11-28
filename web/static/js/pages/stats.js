// pages/stats.js - the statistics hub.
//
// Layout: a live tile strip (SSE, 1 Hz) above seven tabs (#/stats/<tab>):
//   overview  range totals, message / position rate, aircraft by source, range & signal
//   patterns  week heat map, hour-of-day profile, daily totals, unique + new airframes, altitude and distance distributions
//   messages  downlink-format mix (donut + plain-language legend + stacked area), source share over time, Comm-B registers
//   fleet     top operators / countries / types / airframes
//   routes    busiest observed routes, airports and airlines, route coverage (GET /api/routes/stats)
//   records   all-time records wall, event feed
//   health    decode quality, feeds, antennas, process, database
//
// A global range (live / 24 h / 7 d / 30 d / all) and antenna selector drive every panel whose endpoint supports them.
// Only the visible tab polls; each panel has loading, "not enough data yet" and error states.

import { countryCode, learnCountry } from '../stats/countries.js';

const CSS = new URL('../../css/stats.css', import.meta.url);
const HOUR = 3600e3;

const RANGES = [
  { id: 'live', label: 'Live', text: 'last 60 minutes', p: { hours: 1 } },
  { id: '24h', label: '24 h', text: 'last 24 hours', p: { hours: 24 } },
  { id: '7d', label: '7 d', text: 'last 7 days', p: { days: 7 } },
  { id: '30d', label: '30 d', text: 'last 30 days', p: { days: 30 } },
  { id: 'all', label: 'All', text: 'all recorded data', p: null },
];
const rangeIndex = (id) => Math.max(0, RANGES.findIndex((r) => r.id === id));
const TAB_IDS = ['overview', 'patterns', 'messages', 'fleet', 'routes', 'records', 'health'];

// Downlink formats in plain words. `key` matches analytics.DFCount.df.
const DF_INFO = {
  DF17: { color: '#22d3ee', what: 'The aircraft broadcasts its own GPS position, speed, callsign and more, unprompted, about twice a second. These are the messages that put aircraft on the map.' },
  DF11: { color: '#3b8bff', what: 'A short "here I am" beacon carrying only the 24-bit address. Radars and collision-avoidance systems use it to find aircraft; MLAT networks time it to locate aircraft without ADS-B.' },
  DF4: { color: '#a78bfa', what: 'The answer to a ground radar asking "how high are you?" - barometric altitude only.' },
  DF5: { color: '#f472b6', what: 'The answer to a ground radar asking for the squawk (the four-digit transponder code set by the pilot).' },
  DF0: { color: '#fbbf24', what: 'Collision-avoidance (TCAS) replies exchanged between aircraft; carries altitude. Lots of these means busy airspace.' },
  DF16: { color: '#fb923c', what: 'Longer TCAS messages used while two aircraft coordinate an avoidance manoeuvre. Normally rare.' },
  DF20: { color: '#34d399', what: 'An altitude reply with an extra data block (Comm-B): selected altitude, airspeed, heading, sometimes wind and temperature.' },
  DF21: { color: '#a3e635', what: 'A squawk reply with the same kind of extra Comm-B data block.' },
  DF18: { color: '#2dd4bf', what: 'ADS-B style messages not sent by a normal transponder: ground stations rebroadcasting traffic (TIS-B / ADS-R), vehicles, obstacles.' },
  other: { color: '#8494b3', what: 'Rare or military formats and old Mode A/C replies that carry no address and are not decoded further.' },
};
const BDS_NAMES = {
  '1,0': 'Data link capability', '1,7': 'Common usage capability', '2,0': 'Aircraft identification', '3,0': 'ACAS resolution advisory',
  '4,0': 'Selected vertical intention', '4,4': 'Meteorological routine report', '4,5': 'Meteorological hazard report',
  '5,0': 'Track and turn report', '6,0': 'Heading and speed report',
};
const ALT_EDGES = [0, 1000, 5000, 10000, 20000, 30000, 40000]; // lower edge of altitude band 0..6, ft
const EVENT_KINDS = {
  emergency: { label: 'Emergency', badge: 'solid-red' }, military: { label: 'Military', badge: 'amber' },
  interesting: { label: 'Interesting', badge: 'violet' }, record: { label: 'Record', badge: 'cyan' },
  feed_up: { label: 'Feed up', badge: 'green' }, feed_down: { label: 'Feed down', badge: 'red' },
  alert: { label: 'Alert', badge: 'blue' },
};

let active = null; // the mounted instance (for onRoute)

export default {
  id: 'stats',
  title: 'Statistics',

  async mount(container, ctx) {
    const { api, live, units, ui, charts, config } = ctx;
    const { h } = ui;
    const C = charts.colors;
    await ui.loadCss(CSS);
    container.classList.add('st-page');

    // ------------------------------------------------------------------------------------------
    // state, fetch cache, scheduler
    // ------------------------------------------------------------------------------------------
    const prefs = ui.prefs('airview.stats', { range: '24h', ant: '', heat: 'aircraft', evKind: '' });
    const antennas = (config.current?.antennas || []).filter((a) => a && a.id);
    const st = {
      range: RANGES[rangeIndex(prefs.get().range)].id,
      ant: antennas.some((a) => a.id === prefs.get().ant) ? prefs.get().ant : '',
      since: 0,       // Unix ms of the first stored data (overview.allTime.since)
      located: true,
      tab: TAB_IDS.includes(ctx.route.args[0]) ? ctx.route.args[0] : 'overview',
    };
    let gen = 0;      // bumped whenever range / antenna / units change: stale responses are dropped

    const cache = new Map();
    /** GET with a short promise cache so panels that share an endpoint share one request. */
    const get = (path, params = {}, ttl = 4000) => {
      const key = path + '?' + JSON.stringify(params);
      const hit = cache.get(key);
      const now = performance.now();
      if (hit && now - hit.t < ttl) return hit.p;
      const p = api.get(path, { params, signal: ctx.signal });
      cache.set(key, { t: now, p });
      p.catch(() => cache.delete(key));
      return p;
    };

    /** The selected range, clamped to [floor, cap] for panels that need more / cannot take that much. */
    const eff = (floor, cap) => {
      let i = rangeIndex(st.range);
      if (floor) i = Math.max(i, rangeIndex(floor));
      if (cap) i = Math.min(i, rangeIndex(cap));
      return RANGES[i];
    };
    /** Query parameters for a range (+ antenna). */
    // While the station is younger than the server's minute-resolution limit (12 h), ask time series for
    // "since the first data" instead: the server then answers from minute rows instead of (still empty) hourly ones.
    const young = () => st.since > 0 && live.serverNow() - st.since < 11 * HOUR;
    const rp = (r, withAnt = true, fine = false) => {
      const p = fine && r.id !== 'live' && young() ? { from: Math.floor(st.since / 60e3) * 60e3 }
        : r.p ? { ...r.p } : st.since > 0 ? { from: Math.floor(st.since / HOUR) * HOUR } : { days: 3650 };
      if (withAnt && st.ant) p.ant = st.ant;
      return p;
    };
    const antName = () => (st.ant ? antennas.find((a) => a.id === st.ant)?.name || st.ant : 'all antennas');
    /** Card subtitle: effective range (+ antenna), flagged when it differs from the selected range. */
    const scope = (r, withAnt = true) => r.text + (r.id !== st.range ? ' (nearest supported)' : '')
      + (withAnt ? (st.ant ? ' · ' + antName() : '') : st.ant ? ' · all antennas (no per-antenna data)' : '');
    const youngNote = () => (st.range !== 'live' && young() ? ' · station is only ' + units.age((live.serverNow() - st.since) / 1000) + ' old, showing all of it' : '');
    const period = () => (st.range === 'live' ? 30e3 : st.range === '24h' ? 60e3 : 120e3);

    const tasks = { always: [] };
    for (const id of TAB_IDS) tasks[id] = [];
    /** Register a periodic job for a tab ('always' = whatever tab is shown). */
    const task = (tab, every, fn) => { tasks[tab].push({ every, fn, last: 0, busy: false }); };
    const runTask = async (t) => {
      if (t.busy) return;
      t.busy = true;
      t.last = Date.now();
      try { await t.fn(); } catch (e) { if (!e?.aborted) console.error('[stats]', e); } finally { t.busy = false; }
    };
    const tick = () => {
      if (ctx.signal.aborted || document.hidden) return;
      const now = Date.now();
      for (const t of [...tasks.always, ...tasks[st.tab]]) {
        const every = typeof t.every === 'function' ? t.every() : t.every;
        if (now - t.last >= every) runTask(t);
      }
      if (cache.size > 200) for (const [k, v] of cache) if (performance.now() - v.t > 300e3) cache.delete(k);
    };
    const panels = [];
    /** Range / antenna / units changed: forget everything and reload the visible tab. */
    const invalidate = () => {
      gen++;
      cache.clear();
      for (const p of panels) p.reset();
      for (const k of TAB_IDS) for (const t of tasks[k]) t.last = 0;
      updateHead();
      tick();
    };
    const refreshActive = () => { for (const t of tasks[st.tab]) t.last = 0; tick(); };

    // ------------------------------------------------------------------------------------------
    // small builders
    // ------------------------------------------------------------------------------------------

    /** Card with a content area and a loading / empty / error overlay. */
    function panel(o) {
      const content = h('div.st-content');
      const overlay = h('div.st-overlay', { hidden: true });
      const host = h('div.st-host', { style: { '--h': (o.h || 120) + 'px' } }, content, overlay);
      const card = ui.card({ title: o.title, sub: '', icon: o.icon, actions: o.actions, class: o.class, body: [o.top, host, o.caption && h('p.st-cap', o.caption)] });
      let loaded = false;
      const mask = (node) => { ui.mount(overlay, node); overlay.hidden = false; host.classList.add('is-masked'); };
      const p = {
        card, content,
        sub: (t) => card.setSub(t),
        loading() { if (!loaded) mask(ui.skeleton({ lines: 0, block: Math.max(48, (o.h || 120) - 8) })); },
        empty(title, text, icon) { loaded = false; mask(ui.empty({ icon: icon || o.icon || 'chart', title, text, compact: true })); },
        error(e) { loaded = false; mask(ui.error(e, { compact: true, title: 'Could not load this panel', retry: refreshActive })); },
        ok() { loaded = true; overlay.hidden = true; host.classList.remove('is-masked'); },
        reset() { loaded = false; },
        chart(height = o.h || 260) {
          const el = h('div.chart', { style: { '--h': height + 'px' } });
          content.append(el);
          return charts.create(el);
        },
      };
      panels.push(p);
      return p;
    }

    /** Fetch for a set of panels: skeleton -> render(data) (which calls ok()/empty()) or error state. */
    async function load(ps, loader, render) {
      const g = gen;
      for (const p of ps) p.loading();
      try {
        const d = await loader();
        if (g !== gen || ctx.signal.aborted) return;
        render(d);
      } catch (e) {
        if (e?.aborted || ctx.signal.aborted || g !== gen) return;
        console.error('[stats] load failed', e);
        for (const p of ps) p.error(e);
      }
    }

    /** Grid of labelled figures. defs: [[key, label, tip]] -> { el, set(key, value, sub, state) } */
    function figures(defs, cls = '') {
      const map = {};
      const el = h('div.st-figs' + cls, defs.map(([k, label, tip]) => {
        const v = h('div.st-fig-v.num', units.DASH);
        const s = h('div.st-fig-s');
        const box = h('div.st-fig', { tip }, h('div.st-fig-l', label), v, s);
        map[k] = { v, s, box };
        return box;
      }));
      return {
        el,
        set(k, val, sub, state) {
          const m = map[k];
          ui.setText(m.v, val ?? units.DASH);
          ui.setText(m.s, sub || '');
          for (const c of ['is-good', 'is-warn', 'is-bad']) ui.setClass(m.box, c, c === 'is-' + state);
        },
      };
    }

    /** Ranked bar list. rows: [{ name, sub, lead (node), value, right, rightSub, href, color, tip }] */
    function barList(rows, o = {}) {
      const max = Math.max(1e-9, ...rows.map((r) => r.value || 0));
      return h('div.st-bars' + (o.tight ? '.st-bars--tight' : ''), rows.map((r, i) => h(r.href ? 'a.st-bar' : 'div.st-bar',
        { href: r.href, tip: r.tip, style: { '--pct': Math.max(1, ((r.value || 0) / max) * 100).toFixed(1) + '%', '--c': r.color || C.blue } },
        h('span.st-bar-rank', o.rank === false ? '' : String(i + 1)),
        h('span.st-bar-lead', r.lead),
        h('span.st-bar-main', h('div.st-bar-n', r.name), r.sub && h('div.st-bar-s', r.sub)),
        h('span.st-bar-r', r.right, r.rightSub && h('small', r.rightSub)))));
    }

    const acHref = (icao) => '#/aircraft/' + encodeURIComponent(icao);
    const searchHref = (key, value) => '#/aircraft?' + key + '=' + encodeURIComponent(value);
    const pairs = (t, v, f) => t.map((x, i) => [x, v[i] === null || v[i] === undefined ? null : f ? f(v[i]) : v[i]]);
    const round1 = (v) => Math.round(v * 10) / 10;
    const distUser = (km) => round1(units.value('dist', km));
    /** Time-axis line chart in UTC; symbols are shown while there are too few points to form a line. */
    const timeChart = (o, n, d) => {
      const opt = charts.line(o);
      opt.useUTC = true;
      // pin the axis to the requested range so sparse data sits where it belongs in time
      if (d && d.from > 0 && d.to > d.from && (st.range !== 'all' || st.since > 0)) { opt.xAxis.min = d.from; opt.xAxis.max = d.to; }
      if (n < 3) for (const s of opt.series) { s.showSymbol = true; s.symbolSize = 6; }
      return opt;
    };
    const utcNote = 'Times are UTC.';
    const noStats = (ps, d) => {
      const text = d && d.resolution === 'hour'
        ? 'This range is drawn from hourly statistics and nothing has been stored for it yet. Switch to Live for the minute-by-minute view; the first point appears after one full minute of reception.'
        : 'Nothing stored for this range yet. The first point appears after one full minute of reception' + (st.ant ? ' on this antenna.' : '.');
      for (const p of ps) p.empty('Not enough data yet', text);
    };

    // ------------------------------------------------------------------------------------------
    // page head: range + antenna
    // ------------------------------------------------------------------------------------------
    const subEl = h('div.sub');
    const rangeSeg = ui.segmented({
      options: RANGES.map((r) => ({ value: r.id, label: r.label, tip: 'Show ' + r.text })),
      value: st.range,
      onChange: (v) => { st.range = v; prefs.set({ range: v }); invalidate(); },
    });
    const antSel = ui.select({
      options: [{ value: '', label: 'All antennas' }, ...antennas.map((a) => ({ value: a.id, label: a.name || a.id }))],
      value: st.ant,
      size: 'sm',
      disabled: antennas.length ? null : true,
      'aria-label': 'Antenna',
      style: 'width:auto;min-width:132px',
      onChange: (v) => { st.ant = v; prefs.set({ ant: v }); invalidate(); },
    });
    antSel.dataset.tip = antennas.length
      ? 'Statistics per antenna exist for feeds attributed to an antenna. Top lists, records and events always cover all antennas.'
      : 'No antennas are configured (Settings > Antennas).';
    function updateHead() {
      const r = eff();
      ui.mount(subEl, 'Showing ', h('b', r.text), ' · ', h('b', antName()),
        st.since > 0 ? [' · recording since ', h('b', units.date(st.since)), ' (' + units.ago(st.since, live.serverNow()) + ')'] : null);
    }
    updateHead();

    // ------------------------------------------------------------------------------------------
    // live tiles (SSE) + sparklines
    // ------------------------------------------------------------------------------------------
    const SRC = [['adsb', 'ADS-B', C.adsb], ['mlat', 'MLAT', C.mlat], ['tisb', 'TIS-B', C.tisb], ['modes', 'Mode S', C.modes]];
    const srcBar = h('div.st-src-bar', SRC.map(([, , c]) => h('i', { style: { '--c': c } })));
    const srcNums = SRC.map(() => h('b', '0'));
    const srcLeg = h('div.st-src-leg', SRC.map(([, label, c], i) => h('span', { style: { '--c': c } }, srcNums[i], label)));
    const tAc = ui.stat({ label: 'Aircraft now', icon: 'plane', spark: true, class: 'st-wide', tip: 'Aircraft heard in the last minute, split by where their position comes from: ADS-B (self-reported), MLAT (computed by a network), TIS-B (rebroadcast) or Mode S only (no position).' });
    tAc.setSub(h('div', srcBar, srcLeg));
    const tMsg = ui.stat({ label: 'Messages', icon: 'signal', accent: 'cyan', spark: true, unit: '/s', tip: 'Mode S frames decoded per second right now (all antennas).' });
    const tPos = ui.stat({ label: 'Positions', icon: 'pin', accent: 'teal', spark: true, unit: '/s', tip: 'Position fixes decoded per second right now.' });
    const tRng = ui.stat({ label: 'Max range now', icon: 'radar', accent: 'violet', spark: true, tip: 'Distance to the farthest aircraft currently tracked with a position.' });
    const tUni = ui.stat({ label: 'Unique today', icon: 'globe', accent: 'green', tip: 'Different airframes heard since local midnight.' });
    const tPeak = ui.stat({ label: 'Today\'s peak', icon: 'arrow-up', accent: 'amber', tip: 'Most aircraft tracked at the same time today.' });
    const tDay = ui.stat({ label: 'Messages today', icon: 'chart', tip: 'Frames decoded since local midnight.' });
    const tiles = h('div.st-tiles', tAc, tMsg, tPos, tRng, tUni, tPeak, tDay);

    const sparkDefs = [
      { tile: tAc, key: 'aircraft', color: C.blueHi, live: (c) => c.aircraft },
      { tile: tMsg, key: 'framesPerSec', color: C.cyan, live: (c) => c.msgRate },
      { tile: tPos, key: 'positionsPerSec', color: C.teal, live: (c) => c.posRate },
      { tile: tRng, key: 'maxKm', color: C.violet, live: (c) => c.maxRangeKm },
    ];
    for (const s of sparkDefs) { s.base = []; s.chart = charts.sparkline(s.tile.spark, [], { color: s.color, tooltip: false }); }
    let counts = null;
    const drawSparks = () => {
      for (const s of sparkDefs) {
        const tail = counts ? s.live(counts) : null;
        const data = typeof tail === 'number' ? [...s.base, tail] : s.base;
        if (data.length > 1) s.chart.set(charts.sparklineOption({ data, color: s.color, tooltip: false, min: s.key === 'maxKm' ? undefined : 0 }));
      }
    };
    const drawCounts = () => {
      const c = counts;
      if (!c) return;
      tAc.set(units.int(c.aircraft));
      SRC.forEach(([k], i) => {
        const n = c[k] || 0;
        ui.setText(srcNums[i], units.int(n));
        srcBar.children[i].style.flexGrow = String(n);
        ui.setClass(srcLeg.children[i], 'hidden', k === 'tisb' && !n);
      });
      tMsg.set(units.num(c.msgRate, c.msgRate < 10 ? 1 : 0));
      tPos.set(units.num(c.posRate, c.posRate < 10 ? 1 : 0));
      tPos.setSub(c.aircraft ? units.int(c.withPos) + ' of ' + units.int(c.aircraft) + ' located' : '');
      tRng.set(c.maxRangeKm > 0 ? units.format('dist', c.maxRangeKm, { unit: false, digits: 0 }) : units.DASH, units.label('dist'));
    };
    let beat = 0;
    ctx.subscribe('aircraft', (snap) => {
      if (!snap?.counts) return;
      counts = snap.counts;
      drawCounts();
      if (beat % 5 === 0) drawSparks();
      if (beat % 30 === 0) for (const a of snap.aircraft || []) if (a.country && a.countryCode) learnCountry(a.country, a.countryCode);
      beat++;
    });
    task('always', 60e3, async () => {
      const d = await get('/api/stats/rates', { hours: 1 }, 20e3);
      if (ctx.signal.aborted) return;
      for (const s of sparkDefs) s.base = (d.series[s.key] || []).filter((v) => typeof v === 'number');
      drawSparks();
    });

    let ov = null;
    const drawToday = () => {
      if (!ov) return;
      const t = ov.today || {};
      const d = ov.last24h || {};
      tUni.set(units.int(t.uniqueAircraft ?? 0));
      tUni.setSub(units.int(t.newAirframes ?? 0) + ' never seen before');
      // stored peaks lag by up to a minute; the aircraft in view right now are a lower bound
      tPeak.set(units.int(Math.max(t.peakAircraft ?? 0, counts?.aircraft ?? 0)));
      tPeak.setSub(d.peakAircraftT > 0 ? '24 h peak ' + units.int(d.peakAircraft) + ' at ' + units.clock(d.peakAircraftT, { seconds: false }) : '');
      tDay.set(units.si(t.frames ?? 0));
      tDay.setSub(units.si(t.positions ?? 0) + ' positions · ' + units.int(t.flights ?? 0) + ' flights');
      tMsg.setSub(d.framesPerSec !== null && d.framesPerSec !== undefined ? '24 h mean ' + units.rate(d.framesPerSec) : '');
      tRng.setSub(typeof d.maxKm === 'number' ? '24 h best ' + units.dist(d.maxKm) : '');
    };
    const overviewSubs = new Set();
    task('always', 30e3, async () => {
      ov = await get('/api/stats/overview', {}, 8e3);
      if (ctx.signal.aborted) return;
      drawToday();
      st.located = ov.located !== false;
      const since = ov.allTime?.since || 0;
      if (since !== st.since) {
        const first = !st.since;
        st.since = since;
        updateHead();
        if (first && (st.range === 'all' || young())) invalidate();
      }
      for (const fn of overviewSubs) fn(ov);
    });

    // ------------------------------------------------------------------------------------------
    // tab: overview
    // ------------------------------------------------------------------------------------------
    function buildOverview(root) {
      const figs = figures([
        ['frames', 'Messages', 'Mode S frames decoded in the range.'], ['positions', 'Positions', 'Position fixes decoded in the range.'],
        ['unique', 'Unique aircraft', 'Different airframes with a flight in the range.'], ['flights', 'Flights', 'Contact sessions: one aircraft followed from first to last message.'],
        ['peak', 'Peak aircraft', 'Most aircraft tracked at once.'], ['maxKm', 'Max range', 'Farthest position received.'],
        ['rssi', 'Mean signal', 'Average signal level of decoded frames (0 dBFS = receiver full scale; nearer 0 is stronger).'],
        ['uptime', 'Receiver uptime', 'Share of the range with data. Low values mean the receiver or the feed was down.'],
        ['decode', 'CRC pass rate', 'Frames whose checksum was valid. Healthy receivers stay above 95 %.'],
        ['allTime', 'All-time airframes', 'Every different airframe ever recorded by this station.'],
      ], '.st-figs--5');
      const pSum = panel({ title: 'Totals', icon: 'list', h: 110 });
      pSum.content.append(figs.el);
      const pRate = panel({ title: 'Message & position rate', icon: 'signal', h: 270,
        caption: ['How busy the airwaves are: decoded ', h('b', 'messages per second'), ' and how many of them are ', h('b', 'position fixes'), '. Expect a daily wave following air traffic; sudden drops to zero mean the receiver or feed stopped. ', utcNote] });
      const cRate = pRate.chart();
      const pAc = panel({ title: 'Aircraft in view by source', icon: 'plane', h: 250,
        caption: ['Aircraft tracked at once, stacked by where their position comes from. A healthy station is mostly ', h('b', 'ADS-B'), '; the dashed line is how many had a position at all - the closer to the top, the better. ', utcNote] });
      const cAc = pAc.chart();
      const pRng = panel({ title: 'Range & signal', icon: 'radar', h: 250,
        caption: ['Farthest aircraft heard per step and the mean signal level. A flat, high ', h('b', 'range'), ' line means good coverage whenever traffic is there; dips at night are usually missing traffic, not a fault. ', utcNote] });
      const cRng = pRng.chart();
      root.append(h('div.st-stack', pSum.card, pRate.card, h('div.grid.cols-2', pAc.card, pRng.card)));

      const drawAllTime = (o) => figs.set('allTime', units.int(o.allTime?.airframes ?? 0), units.int(o.allTime?.countries ?? 0) + ' countries · ' + units.si(o.allTime?.flights ?? 0) + ' flights');
      overviewSubs.add(drawAllTime);
      if (ov) drawAllTime(ov);

      task('overview', period, () => load([pSum, pRate, pAc, pRng], () => get('/api/stats/rates', rp(eff(), true, true)), (d) => {
        const s = d.series;
        const T = d.totals;
        const n = s.t.length;
        const sub = scope(eff()) + youngNote() + ' · one point per ' + d.resolution;
        for (const p of [pRate, pAc, pRng]) p.sub(sub);
        pSum.sub(scope(eff()) + youngNote());
        figs.set('frames', units.si(T.frames), units.rate(T.framesPerSec) + ' while up');
        figs.set('positions', units.si(T.positions), T.frames ? units.pct((T.positions / T.frames) * 100, 1) + ' of messages' : '');
        figs.set('unique', units.int(T.uniqueAircraft));
        figs.set('flights', units.int(T.flights));
        figs.set('peak', units.int(T.peakAircraft), T.peakAircraftT > 0 ? units.dateTime(T.peakAircraftT) : '');
        figs.set('maxKm', units.dist(T.maxKm));
        figs.set('rssi', units.dbfs(T.rssiAvg));
        figs.set('uptime', units.pct(T.uptimePct, 1), units.duration(T.minutesWithData * 60e3) + ' with data', T.uptimePct === null ? '' : T.uptimePct >= 99 ? 'good' : T.uptimePct >= 90 ? '' : 'warn');
        figs.set('decode', units.pct(T.decodePct, 2), units.int(T.crcBad) + ' failed', T.decodePct === null ? '' : T.decodePct >= 95 ? 'good' : T.decodePct >= 80 ? 'warn' : 'bad');
        pSum.ok();
        if (!n) { noStats([pRate, pAc, pRng], d); return; }

        cRate.set(timeChart({
          series: [
            { name: 'Messages/s', data: pairs(s.t, s.framesPerSec), color: C.blue, area: true },
            { name: 'Positions/s', data: pairs(s.t, s.positionsPerSec), color: C.cyan, area: 0.22 },
          ],
          yMin: 0, zoom: n > 240, valueFormat: (v) => units.rate(v),
        }, n, d), { replace: true });
        pRate.ok();

        cAc.set(timeChart({
          series: [
            { name: 'ADS-B', data: pairs(s.t, s.adsb), color: C.adsb, stack: 'src', area: 0.5, width: 1 },
            { name: 'MLAT', data: pairs(s.t, s.mlat), color: C.mlat, stack: 'src', area: 0.5, width: 1 },
            { name: 'Mode S only', data: pairs(s.t, s.modes), color: C.modes, stack: 'src', area: 0.5, width: 1 },
            { name: 'With position', data: pairs(s.t, s.aircraftPos), color: C.text, dashed: true, width: 1.2, z: 5 },
          ],
          yMin: 0, valueFormat: (v) => units.int(v),
        }, n, d), { replace: true });
        pAc.ok();

        const opt = timeChart({
          series: [
            { name: 'Max range (' + units.label('dist') + ')', data: pairs(s.t, s.maxKm, distUser), color: C.violet, area: 0.2 },
            { name: 'Mean signal (dBFS)', data: pairs(s.t, s.rssiAvg, round1), color: C.amber, y2: true, width: 1.2 },
          ],
          yMin: 0, y2Scale: true,
        }, n, d);
        opt.series[0].tooltip = { valueFormatter: (v) => (typeof v === 'number' ? units.num(v, 1) + ' ' + units.label('dist') : units.DASH) };
        opt.series[1].tooltip = { valueFormatter: (v) => units.dbfs(v) };
        cRng.set(opt, { replace: true });
        if (s.maxKm.some((v) => typeof v === 'number')) pRng.ok();
        else pRng.empty('No range data', st.located ? 'No positions were received in this range.' : 'Range needs the receiver location - set it in Settings.', 'radar');
      }));
    }

    // ------------------------------------------------------------------------------------------
    // tab: patterns
    // ------------------------------------------------------------------------------------------
    function buildPatterns(root) {
      let heatData = null;
      const heatSeg = ui.segmented({
        options: [{ value: 'aircraft', label: 'Aircraft' }, { value: 'framesPerSec', label: 'Messages/s' }],
        value: prefs.get().heat === 'framesPerSec' ? 'framesPerSec' : 'aircraft',
        onChange: (v) => { prefs.set({ heat: v }); drawHeat(); },
      });
      const pHeat = panel({ title: 'Traffic by weekday and hour', icon: 'history', h: 250, actions: heatSeg, class: 'span-2',
        caption: ['Each cell is the average for that hour of that weekday (station local time); brighter is busier. Look for the daily rhythm - quiet nights, morning and evening peaks - and weekday / weekend differences. Dark cells have no data yet.'] });
      const cHeat = pHeat.chart();
      const pHod = panel({ title: 'Average day', icon: 'timer', h: 250,
        caption: 'Average aircraft in view for each hour of the day (station local time) across the range: when it pays to be listening.' });
      const cHod = pHod.chart();
      const pDay = panel({ title: 'Daily totals', icon: 'chart', h: 250,
        caption: ['Bars: ', h('b', 'messages'), ' decoded per local day; line: the ', h('b', 'peak'), ' number of aircraft tracked at once. Steady bars mean a stable station; a short bar is a day with downtime or a partial (first / current) day.'] });
      const cDay = pDay.chart();
      const pNew = panel({ title: 'Unique & new airframes per day', icon: 'plane', h: 250,
        caption: ['How many different airframes were heard each day and how many of them had ', h('b', 'never been seen before'), '. New airframes start high on a fresh station and settle as you collect the regulars.'] });
      const cNew = pNew.chart();
      const pAlt = panel({ title: 'Positions by altitude band', icon: 'arrow-up', h: 250,
        caption: 'Share of received positions per altitude band. Most stations are dominated by cruise traffic; a good low-level share means a clear view down to the horizon (or an airport nearby).' });
      const cAlt = pAlt.chart();
      const pPeakAlt = panel({ title: 'Flight peak altitudes', icon: 'arrow-up', h: 250,
        caption: 'How high each flight got while you tracked it. The tall hump is airliner cruise; bars on the left are local, climbing or descending traffic.' });
      const cPeakAlt = pPeakAlt.chart();
      const pDist = panel({ title: 'Distance distribution', icon: 'radar', h: 250, class: 'span-2',
        caption: ['Share of positions received at each distance from the antenna. A long tail to the right is good; a sharp cliff shows where terrain or the radio horizon ends your coverage. Dashed lines: half of all positions (', h('b', 'median'), ') and the ', h('b', '95 %'), ' distance.'] });
      const cDist = pDist.chart();

      root.append(h('div.grid.cols-3', pHeat.card, pHod.card),
        h('div.grid.cols-2.mt-4', pDay.card, pNew.card),
        h('div.grid.cols-4.mt-4', pDist.card, pAlt.card, pPeakAlt.card));

      const HOURS = Array.from({ length: 24 }, (_, i) => String(i).padStart(2, '0'));
      function drawHeat() {
        const d = heatData;
        if (!d) return;
        const key = heatSeg.value;
        const grid = d[key] || [];
        const cells = [];
        grid.forEach((row, day) => (row || []).forEach((v, hr) => { if (typeof v === 'number') cells.push([hr, 6 - day, round1(v)]); }));
        if (!cells.length) {
          pHeat.empty('Not enough data yet', 'The heat map fills in one cell per completed hour. A full picture needs at least a week of reception.');
          return;
        }
        const days = [...d.days].reverse();
        const fmt = key === 'aircraft' ? (v) => units.num(v, 1) + ' aircraft' : (v) => units.rate(v);
        const opt = charts.heatmap({ x: HOURS, y: days, data: cells, ramp: charts.ramps.heat, min: 0,
          valueFormat: key === 'aircraft' ? (v) => units.num(v, 0) : (v) => units.si(v) });
        opt.tooltip.formatter = (p) => `${days[p.value[1]]} ${HOURS[p.value[0]]}:00–${HOURS[p.value[0]]}:59<br><b>${fmt(p.value[2])}</b> on average`;
        cHeat.set(opt, { replace: true });
        pHeat.ok();
      }

      task('patterns', () => Math.max(period(), 60e3), () => load([pHeat, pHod], () => get('/api/stats/heatmap', rp(eff('30d')), 30e3), (d) => {
        heatData = d;
        const busiest = d.busiestDay >= 0 && d.busiestHour >= 0 && d.days?.[d.busiestDay] ? ' · busiest: ' + d.days[d.busiestDay] + ' ' + HOURS[d.busiestHour] + ':00' : '';
        pHeat.sub(scope(eff('30d')) + busiest);
        pHod.sub(scope(eff('30d')));
        drawHeat();
        const hod = d.hourOfDay || [];
        if (!hod.some((v) => typeof v === 'number' && v > 0)) { pHod.empty('Not enough data yet', 'Needs at least one completed hour of reception; a meaningful profile needs a few days.'); return; }
        const opt = charts.bar({ x: HOURS, series: [{ name: 'Aircraft', data: hod.map((v) => (typeof v === 'number' ? round1(v) : null)), color: C.blue }], legend: false,
          valueFormat: (v) => units.num(v, 1) + ' aircraft' });
        cHod.set(opt, { replace: true });
        pHod.ok();
      }));

      task('patterns', () => Math.max(period(), 60e3), () => load([pDay, pNew], () => get('/api/stats/daily', rp(eff('30d')), 30e3), (d) => {
        const days = d.days || [];
        const bd = d.busiestDay;
        pDay.sub(scope(eff('30d')) + (bd ? ' · busiest day ' + bd.date + ' (' + units.int(bd.uniqueAircraft) + ' aircraft)' : ''));
        pNew.sub(scope(eff('30d')));
        if (!days.length) {
          for (const p of [pDay, pNew]) p.empty('Not enough data yet', 'Daily totals appear with the first stored hour of reception.');
          return;
        }
        const x = days.map((r) => r.date.slice(5));
        const opt = charts.bar({ x, series: [{ name: 'Messages', data: days.map((r) => r.frames), color: C.blue }, { name: 'Peak aircraft', data: days.map((r) => r.peakAircraft), color: C.amber, y2: true }],
          yFormat: (v) => units.si(v), y2Name: '' });
        Object.assign(opt.series[1], { type: 'line', showSymbol: true, symbolSize: 5, lineStyle: { width: 1.6 }, tooltip: { valueFormatter: (v) => units.int(v) + ' aircraft' } });
        opt.series[0].tooltip = { valueFormatter: (v) => units.int(v) };
        opt.tooltip.formatter = undefined;
        cDay.set(opt, { replace: true });
        pDay.ok();
        cNew.set(charts.bar({ x, series: [{ name: 'Unique airframes', data: days.map((r) => r.uniqueAircraft), color: C.blue }, { name: 'Never seen before', data: days.map((r) => r.newAirframes), color: C.cyan }],
          valueFormat: (v) => units.int(v) }), { replace: true });
        pNew.ok();
      }));

      task('patterns', () => Math.max(period(), 60e3), () => load([pAlt, pPeakAlt], () => get('/api/stats/altitudes', rp(eff('24h')), 30e3), (d) => {
        const bands = d.bands || [];
        const sub = scope(eff('24h')) + ' · whole local days';
        pAlt.sub(sub);
        if (!bands.some((b) => b.positions > 0)) pAlt.empty('Not enough data yet', 'Needs aircraft positions with an altitude. They are counted as soon as the first minute of reception is stored.');
        else {
          const labels = bands.map((b) => {
            const lo = ALT_EDGES[b.band];
            const hi = ALT_EDGES[b.band + 1];
            if (lo === undefined) return b.label;
            if (hi === undefined) return '≥ ' + units.alt(lo);
            return lo === 0 ? '< ' + units.alt(hi) : units.format('alt', lo, { unit: false }) + '–' + units.alt(hi);
          });
          const opt = charts.bar({ x: labels, horizontal: true, legend: false, series: [{ name: 'Positions', data: bands.map((b) => round1(b.pct)), color: C.blue }], yFormat: (v) => v + '%' });
          opt.tooltip.formatter = (ps) => {
            const b = bands[ps[0].dataIndex];
            return `${labels[ps[0].dataIndex]}<br><b>${units.pct(b.pct, 1)}</b> of positions (${units.int(b.positions)})<br>farthest ${units.dist(b.maxKm)}`;
          };
          cAlt.set(opt, { replace: true });
          pAlt.ok();
        }
        const hist = d.flightAlt || [];
        let last = hist.length - 1;
        while (last >= 0 && !hist[last]) last--;
        if (last < 0 || !d.flightsN) { pPeakAlt.sub(sub); pPeakAlt.empty('Not enough data yet', 'Needs at least one tracked flight with an altitude.'); return; }
        pPeakAlt.sub(units.int(d.flightsN) + ' flights · median ' + units.alt(d.medianFt, { fl: true }) + ' · ' + units.alt(d.lowestFt) + ' to ' + units.alt(d.highestFt));
        const bin = d.binFt || 2000;
        const x = hist.slice(0, last + 1).map((_, i) => units.format('alt', i * bin, { unit: false }));
        const opt = charts.bar({ x, legend: false, series: [{ name: 'Flights', data: hist.slice(0, last + 1), color: C.violet }], xName: 'peak altitude, ' + units.label('alt') });
        opt.grid.bottom = 26;
        opt.series[0].barCategoryGap = '12%';
        opt.tooltip.formatter = (ps) => `${units.alt(ps[0].dataIndex * bin)} – ${units.alt((ps[0].dataIndex + 1) * bin)}<br><b>${units.int(ps[0].value)}</b> flights`;
        cPeakAlt.set(opt, { replace: true });
        pPeakAlt.ok();
      }));

      task('patterns', () => Math.max(period(), 120e3), () => load([pDist], () => get('/api/stats/range', { ...rp(eff('24h', '30d')), hist: 1 }, 60e3), (d) => {
        const r = eff('24h', '30d');
        const sum = new Array(120).fill(0);
        let maxKm = null;
        for (const hr of d.hours || []) {
          if (typeof hr.maxKm === 'number') maxKm = Math.max(maxKm ?? 0, hr.maxKm);
          (hr.distHist || []).forEach((v, i) => { if (i < 120) sum[i] += v || 0; });
        }
        const total = sum.reduce((a, b) => a + b, 0);
        if (!total) {
          pDist.sub(scope(r));
          pDist.empty('Not enough data yet', st.located
            ? 'Distances are stored once per completed hour: the first bars appear a few minutes after the top of the next hour' + (st.ant ? ' (for this antenna).' : '.')
            : 'Distances need the receiver location - set it in Settings (or use the location guesser).', 'radar');
          return;
        }
        let last = 119;
        while (last > 0 && !sum[last]) last--;
        const q = (f) => { let acc = 0; for (let i = 0; i <= last; i++) { acc += sum[i]; if (acc >= total * f) return i; } return last; };
        const i50 = q(0.5);
        const i95 = q(0.95);
        pDist.sub(scope(r) + ' · median ' + units.dist((i50 + 0.5) * 5) + ' · 95 % within ' + units.dist((i95 + 1) * 5) + ' · farthest ' + units.dist(maxKm));
        const x = sum.slice(0, last + 1).map((_, i) => units.format('dist', i * 5, { unit: false, digits: 0 }));
        const opt = charts.bar({ x, legend: false, series: [{ name: 'Positions', data: sum.slice(0, last + 1).map((v) => Math.round((v / total) * 1e4) / 100), color: C.cyan }],
          yFormat: (v) => v + '%', xName: 'distance, ' + units.label('dist') });
        opt.grid.bottom = 26;
        opt.series[0].barCategoryGap = '8%';
        opt.series[0].markLine = { silent: true, symbol: 'none', data: [{ xAxis: i50, name: 'median' }, { xAxis: i95, name: '95 %' }], label: { formatter: '{b}', position: 'end', rotate: 0 } };
        opt.grid.top = 26;
        opt.tooltip.formatter = (ps) => `${units.dist(ps[0].dataIndex * 5, { digits: 0 })} – ${units.dist((ps[0].dataIndex + 1) * 5, { digits: 0 })}<br><b>${units.pct(ps[0].value, 2)}</b> of positions (${units.int(sum[ps[0].dataIndex])})`;
        cDist.set(opt, { replace: true });
        pDist.ok();
      }));
    }

    // ------------------------------------------------------------------------------------------
    // tab: messages
    // ------------------------------------------------------------------------------------------
    function buildMessages(root) {
      const pDf = panel({ title: 'Downlink formats', icon: 'signal', h: 300,
        caption: ['Every Mode S reply starts with a ', h('b', 'downlink format'), ' (DF) number saying what kind of message it is. A healthy ADS-B station is dominated by ', h('b', 'DF17'), '; lots of DF11 / DF4 / DF5 with little DF17 means radar-interrogated traffic without ADS-B.'] });
      const donutEl = h('div.chart', { style: { '--h': '250px' } });
      const dfList = h('div.st-df-list');
      pDf.content.append(h('div.st-df', donutEl, dfList));
      const cDonut = charts.create(donutEl);

      const pDfT = panel({ title: 'Downlink formats over time', icon: 'chart', h: 280,
        caption: ['Messages per second by downlink format, stacked. The bands should rise and fall together with traffic; one band vanishing (for example ', h('b', 'DF17'), ') while others stay points to a decoder or feed problem. ', utcNote] });
      const cDfT = pDfT.chart();
      const pSrc = panel({ title: 'ADS-B vs MLAT vs Mode S share', icon: 'layers', h: 250, class: 'span-2',
        caption: ['Of all aircraft in view, the share located by ', h('b', 'ADS-B'), ', by ', h('b', 'MLAT'), ' (position computed by a feeder network) or heard as ', h('b', 'Mode S only'), ' (no position). More ADS-B is better; Mode S only aircraft are invisible on the map. ', utcNote] });
      const cSrc = pSrc.chart();
      const pBds = panel({ title: 'Comm-B registers decoded', icon: 'database', h: 160,
        caption: 'Extra data blocks carried by DF20 / DF21 replies, counted since AirView started (all antennas). They provide selected altitude, airspeed, heading and the weather data used on the Weather page.' });

      root.append(h('div.st-stack', pDf.card, pDfT.card, h('div.grid.cols-3', pSrc.card, pBds.card)));

      task('messages', period, () => load([pDf, pBds], () => get('/api/stats/df', rp(eff(), true, true)), (d) => {
        const rows = d.df || [];
        pDf.sub(scope(eff()) + youngNote() + ' · ' + units.si(d.frames) + ' messages');
        const bds = Object.entries(d.liveBds || {}).filter(([, n]) => n > 0).sort((a, b) => b[1] - a[1]);
        pBds.sub('since start');
        if (bds.length) {
          ui.mount(pBds.content, barList(bds.map(([k, n]) => ({ name: 'BDS ' + k, sub: BDS_NAMES[k] || 'Comm-B register', value: n, right: units.si(n), color: C.green })), { tight: false, rank: false }));
          pBds.ok();
        } else pBds.empty('Nothing decoded yet', 'Comm-B registers arrive only where a ground radar interrogates aircraft (DF20 / DF21). None since AirView started.', 'database');
        if (!d.frames) { pDf.empty('Not enough data yet', 'No messages stored for this range yet. The first minute of reception fills this in' + (st.ant ? ' (for this antenna).' : '.')); return; }
        cDonut.set(charts.donut({
          data: rows.filter((r) => r.count > 0).map((r) => ({ name: r.df === 'other' ? 'Other' : r.df, value: r.count, color: DF_INFO[r.df]?.color })),
          centerText: units.si(d.frames), legend: false, valueFormat: (v) => units.int(v) + ' (' + units.pct((v / d.frames) * 100, 1) + ')',
        }), { replace: true });
        ui.mount(dfList, [...rows].sort((a, b) => b.count - a.count).map((r) => h('div.st-df-row' + (r.count ? '' : '.is-zero'), { style: { '--c': DF_INFO[r.df]?.color || C.grey } },
          h('span.st-df-sw'), h('span.st-df-code', r.df === 'other' ? '—' : r.df), h('span.st-df-name', r.name),
          h('span.st-df-num', units.pct(r.pct, 1), h('small', units.si(r.count))),
          h('span'), h('span'), h('span.st-df-what', DF_INFO[r.df]?.what || ''))));
        pDf.ok();
      }));

      // The API has no per-format time series, so the stacked area is assembled from /api/stats/df over
      // consecutive aligned windows (12..30 small requests, each cached by the server).
      const windows = () => {
        const now = live.serverNow();
        let step = HOUR;
        let n = 24;
        if (st.range === 'live') { step = 5 * 60e3; n = 12; }
        else if (st.range === '7d') { step = 6 * HOUR; n = 28; }
        else if (st.range === '30d') { step = 24 * HOUR; n = 30; }
        else if (st.range === 'all') {
          const span = Math.max(HOUR, now - (st.since || now - 30 * 24 * HOUR));
          step = Math.max(HOUR, Math.ceil(span / 30 / HOUR) * HOUR);
          n = Math.min(30, Math.ceil(span / step) + 1);
        }
        const minuteRes = step <= 12 * HOUR;      // the server answers spans up to 12 h from minute rows (complete minutes only)
        const end = minuteRes ? Math.floor(now / 60e3) * 60e3 : Math.ceil(now / HOUR) * HOUR;
        return Array.from({ length: n }, (_, i) => {
          const from = end - (n - i) * step;
          const to = from + step;
          return { from, to, secs: (Math.min(to, minuteRes ? to : now) - from) / 1000 };
        });
      };
      task('messages', () => (st.range === 'live' ? 60e3 : 300e3), () => load([pDfT], async () => {
        const ws = windows();
        const out = [];
        for (let i = 0; i < ws.length; i += 6) {
          const part = await Promise.all(ws.slice(i, i + 6).map((w) => {
            const p = { from: w.from, to: w.to };
            if (st.ant) p.ant = st.ant;
            return get('/api/stats/df', p, 50e3);
          }));
          if (ctx.signal.aborted) return out;
          out.push(...part);
        }
        return { ws, out };
      }, ({ ws, out }) => {
        const stepMin = Math.round((ws[0].to - ws[0].from) / 60e3);
        pDfT.sub(scope(eff()) + ' · ' + (stepMin >= 1440 ? (stepMin / 1440) + ' d' : stepMin >= 60 ? (stepMin / 60) + ' h' : stepMin + ' min') + ' steps');
        const withData = out.filter((d) => d.frames > 0).length;
        if (!withData) { noStats([pDfT], { resolution: 'minute' }); return; }
        const keys = (out.find((d) => d.df?.length)?.df || []).map((r) => r.df);
        const series = keys.map((k, ki) => ({
          name: k === 'other' ? 'Other' : k, color: DF_INFO[k]?.color, stack: 'df', area: 0.6, width: 0.8,
          data: out.map((d, i) => [ws[i].from + (ws[i].to - ws[i].from) / 2, d.frames > 0 && ws[i].secs > 0 ? Math.round(((d.df[ki]?.count || 0) / ws[i].secs) * 100) / 100 : null]),
        })).filter((s) => s.data.some((p) => p[1] > 0));
        cDfT.set(timeChart({ series, yMin: 0, valueFormat: (v) => units.rate(v) }, withData, { from: ws[0].from, to: ws[ws.length - 1].to }), { replace: true });
        pDfT.ok();
      }));

      task('messages', period, () => load([pSrc], () => get('/api/stats/rates', rp(eff(), true, true)), (d) => {
        const s = d.series;
        const S = d.sources || {};
        pSrc.sub(scope(eff()) + youngNote() + (typeof S.adsbPct === 'number' ? ' · ADS-B ' + units.pct(S.adsbPct, 1) + ' · MLAT ' + units.pct(S.mlatPct, 1) + ' · Mode S only ' + units.pct(S.modesPct, 1) : ''));
        if (!s.t.length) { noStats([pSrc], d); return; }
        const share = (arr) => s.t.map((t, i) => {
          const tot = (s.adsb[i] || 0) + (s.mlat[i] || 0) + (s.modes[i] || 0);
          return [t, tot > 0 ? Math.round(((arr[i] || 0) / tot) * 1000) / 10 : null];
        });
        cSrc.set(timeChart({
          series: [
            { name: 'ADS-B', data: share(s.adsb), color: C.adsb, stack: 'p', area: 0.55, width: 0.8 },
            { name: 'MLAT', data: share(s.mlat), color: C.mlat, stack: 'p', area: 0.55, width: 0.8 },
            { name: 'Mode S only', data: share(s.modes), color: C.modes, stack: 'p', area: 0.55, width: 0.8 },
          ],
          yMin: 0, yMax: 100, yFormat: (v) => v + '%', valueFormat: (v) => units.pct(v, 1),
        }, s.t.length, d), { replace: true });
        pSrc.ok();
      }));
    }

    // ------------------------------------------------------------------------------------------
    // tab: fleet
    // ------------------------------------------------------------------------------------------
    function buildFleet(root) {
      const mk = (title, icon, caption) => panel({ title, icon, h: 200, caption });
      const pOp = mk('Top operators', 'plane', 'Airlines and operators ranked by flights tracked. Click one to list its aircraft.');
      const pCo = mk('Top countries', 'globe', 'Country of registration (from the ICAO address block) ranked by flights tracked.');
      const pTy = mk('Top aircraft types', 'layers', 'ICAO type designators ranked by flights tracked.');
      const pAf = mk('Most-seen airframes', 'history', 'Individual aircraft ranked by messages received - your regulars. Click one for its history.');
      root.append(h('div.grid.cols-2', pOp.card, pCo.card, pTy.card, pAf.card));

      task('fleet', () => Math.max(period(), 60e3), () => load([pOp, pCo, pTy, pAf], () => get('/api/stats/top', { ...rp(eff(), false), limit: 15 }, 30e3), (d) => {
        const sub = scope(eff(), false) + ' · ' + units.int(d.flights) + ' flights, ' + units.int(d.aircraft) + ' aircraft';
        const none = 'No flights recorded in this range yet. A flight is stored about a minute after an aircraft is first heard.';
        const row = (r, extra) => ({ name: r.label || r.key, value: r.flights, right: units.int(r.flights), rightSub: units.pct(r.pct, 1) + ' of flights',
          sub: units.int(r.aircraft) + ' aircraft · ' + units.si(r.msgs) + ' messages', ...extra });
        const fill = (p, list, map, emptyText) => {
          p.sub(sub);
          if (!list?.length) { p.empty('Not enough data yet', emptyText || none); return; }
          ui.mount(p.content, barList(list.map(map)));
          p.ok();
        };
        fill(pOp, d.operators, (r) => row(r, { href: searchHref('q', r.key) }));
        fill(pCo, d.countries, (r) => row(r, { lead: ui.flag(countryCode(r.key), r.key), href: searchHref('country', r.key), color: C.cyan }));
        fill(pTy, d.types, (r) => row(r, { name: r.key, sub: (r.label ? r.label + ' · ' : '') + units.int(r.aircraft) + ' aircraft', href: searchHref('q', r.key), color: C.violet }),
          d.flights ? 'Aircraft types come from the aircraft database, which is not installed (or has no match for the traffic seen). Install it under Settings > Database.' : none);
        fill(pAf, d.airframes, (r) => ({
          name: r.label || r.key, lead: ui.flag(countryCode(r.country), r.country), value: r.msgs, color: C.amber, href: acHref(r.key),
          sub: [r.label ? r.key : null, r.operator, typeof r.maxKm === 'number' ? 'max ' + units.dist(r.maxKm) : null].filter(Boolean).join(' · '),
          right: units.si(r.msgs), rightSub: units.int(r.flights) + (r.flights === 1 ? ' flight' : ' flights'),
        }));
      }));
    }

    // ------------------------------------------------------------------------------------------
    // tab: routes (docs/ROUTES-DESIGN.md §6.6) - from the legs this station observed (take-off / landing guesses)
    // ------------------------------------------------------------------------------------------
    function buildRoutes(root) {
      const mk = (title, icon, caption) => panel({ title, icon, h: 200, caption });
      const pR = mk('Busiest routes', 'route', 'Departure-arrival pairs this station saw flown, both ends observed with at least 50 % confidence. Observed, not filed: a diversion counts where it landed.');
      const pA = mk('Busiest airports', 'pin', 'Take-offs and landings this station observed per airport. Click one for its live board.');
      const pL = mk('Airlines', 'plane', 'Flights by the airline prefix of the callsign (ICAO three-letter code).');
      const pC = mk('Route coverage', 'check', 'Which source supplied the route shown when each flight ended, and how many flights had both take-off and landing observed. Routes from online look-ups are not stored with the legs (their licence does not allow it), so those flights count as "No route shown" here.');
      root.append(h('div.grid.cols-2', pR.card, pA.card, pL.card, pC.card));
      const range = () => {
        const r = eff();
        const nowS = Math.floor(live.serverNow() / 1000);
        if (!r.p) return { from: 1 };
        return { from: nowS - (r.p.hours ? r.p.hours * 3600 : r.p.days * 86400) };
      };
      const none = 'No observed legs in this range yet. A leg is stored after a flight has ended (at the earliest 30 minutes after the aircraft was last heard), and only when AirView could tell an airport from its first or last positions.';
      task('routes', () => Math.max(period(), 60e3), () => load([pR, pA, pL, pC], async () => {
        const p = range();
        const [r, a, l, c] = await Promise.all(['routes', 'airports', 'airlines', 'coverage'].map((kind) => get('/api/routes/stats', { ...p, kind, limit: 15 }, 30e3)));
        return { r, a, l, c };
      }, (d) => {
        const sub = scope(eff(), false);
        const fill = (pn, rows, map) => {
          pn.sub(sub);
          if (!rows?.length) { pn.empty('Not enough data yet', none); return; }
          ui.mount(pn.content, barList(rows.map(map)));
          pn.ok();
        };
        fill(pR, d.r.rows, (x) => ({ name: x.key, value: x.n, right: units.int(x.n), rightSub: x.n === 1 ? 'flight' : 'flights', color: C.violet }));
        fill(pA, d.a.rows, (x) => ({ name: x.key, value: x.n, right: units.int(x.n), sub: units.int(x.dep) + ' departures · ' + units.int(x.arr) + ' arrivals', href: '#/airports/' + encodeURIComponent(x.key), color: C.cyan }));
        fill(pL, d.l.rows, (x) => ({ name: x.key, value: x.n, right: units.int(x.n), href: searchHref('q', x.key), color: C.amber }));
        const cov = (d.c.rows || []).filter((x) => x.key !== 'total');
        const total = (d.c.rows || []).find((x) => x.key === 'total')?.n || 0;
        const label = { none: 'No route shown', 'both-ends-observed': 'Take-off and landing observed', 'standing-data': 'Standing data (VRS)', learned: 'Learned by AirView', override: 'Operator override' };
        fill(pC, total ? cov : [], (x) => ({ name: label[x.key] || x.key, value: x.pct || 0, right: units.pct(x.pct || 0, 1), rightSub: units.int(x.n) + ' of ' + units.int(total), color: x.key === 'none' ? C.amber : C.green }));
      }));
    }

    // ------------------------------------------------------------------------------------------
    // tab: records + events
    // ------------------------------------------------------------------------------------------
    const RECORDS = [
      { key: 'max_dist_km', label: 'Farthest contact', icon: 'radar', color: C.cyan, fmt: (v) => units.dist(v) },
      { key: 'max_alt_ft', label: 'Highest aircraft', icon: 'arrow-up', color: C.violet, fmt: (v) => units.alt(v) },
      { key: 'max_gs_kt', label: 'Fastest ground speed', icon: 'bolt', color: C.amber, fmt: (v) => units.speed(v) },
      { key: 'min_rssi_pos_dbfs', label: 'Weakest signal decoded', icon: 'signal', color: C.teal, fmt: (v) => units.dbfs(v) },
      { key: 'max_flight_s', label: 'Longest tracked flight', icon: 'timer', color: C.green, fmt: (v) => units.age(v) },
      { key: 'max_aircraft', label: 'Busiest moment', icon: 'plane', color: C.blue, fmt: (v) => units.int(v) + ' aircraft' },
      { key: 'max_msg_rate', label: 'Peak message rate', icon: 'chart', color: C.blueHi, fmt: (v) => units.rate(v) },
    ];
    const recByKey = Object.fromEntries(RECORDS.map((r) => [r.key, r]));
    const recFmt = (key, v) => (typeof v !== 'number' ? units.DASH : recByKey[key] ? recByKey[key].fmt(v) : units.num(v, 1));

    function buildRecords(root) {
      const wall = h('div.st-wall');
      const pWall = panel({ title: 'All-time records', icon: 'bolt', h: 150,
        caption: 'The best this station has ever done. Tiles with an aircraft link to its history; a far "farthest contact" and a very low "weakest signal" both mean a sensitive receiver with a clear horizon.' });
      pWall.content.append(wall);
      const evSeg = ui.segmented({
        options: [{ value: '', label: 'All' }, { value: 'emergency', label: 'Emergency' }, { value: 'military', label: 'Military' }, { value: 'interesting', label: 'Interesting' }, { value: 'record', label: 'Records' }],
        value: ['emergency', 'military', 'interesting', 'record'].includes(prefs.get().evKind) ? prefs.get().evKind : '',
        onChange: (v) => { prefs.set({ evKind: v }); older = []; pEv.reset(); refreshActive(); },
      });
      evSeg.style.flexWrap = 'wrap'; // five filters do not fit a phone row: wrap instead of hiding "Records" off-screen
      const evList = h('div.st-evs');
      const moreBtn = ui.button({ label: 'Load older', icon: 'chevron-down', size: 'sm', variant: 'ghost', onClick: loadOlder });
      const pEv = panel({ title: 'Recent events', icon: 'history', h: 160, actions: evSeg,
        caption: 'Newest first: emergency squawks, military and otherwise interesting aircraft, broken records and feed connects / disconnects. Rows with an aircraft link to its history.' });
      pEv.content.append(evList, h('div.row.mt-3', { style: 'justify-content:center' }, moreBtn));
      root.append(h('div.st-stack', pWall.card, pEv.card));

      const recTile = (def, r, extra) => {
        const d = r?.detail || {};
        const who = [d.callsign && h('b', d.callsign), d.reg, d.type].filter(Boolean);
        const where = [
          def.key !== 'max_dist_km' && typeof d.distKm === 'number' ? units.dist(d.distKm) : null,
          typeof d.bearing === 'number' ? units.bearing(d.bearing) : null,
          def.key !== 'max_alt_ft' && typeof d.altFt === 'number' ? units.alt(d.altFt, { fl: true }) : null,
          def.key !== 'max_gs_kt' && typeof d.gsKt === 'number' ? units.speed(d.gsKt) : null,
          def.key !== 'min_rssi_pos_dbfs' && typeof d.rssi === 'number' ? units.dbfs(d.rssi) : null,
        ].filter(Boolean).join(' · ');
        const has = r && typeof r.value === 'number';
        return h(r?.icao ? 'a.st-rec' : 'div.st-rec' + (has ? '' : '.is-none'), { href: r?.icao ? acHref(r.icao) : null, style: { '--c': def.color } },
          h('div.st-rec-l', ui.icon(def.icon), def.label),
          h('div.st-rec-v', has ? def.fmt(r.value) : units.DASH),
          has ? [
            (r.icao || who.length > 0) && h('div.st-rec-who', who.length ? who.flatMap((w, i) => (i ? [' · ', w] : [w])) : null, r.icao && h('span.num.dim', r.icao)),
            (where || extra) && h('div.st-rec-x', extra || where),
            h('div.st-rec-t', { tip: units.dateTime(r.t) }, r.when || units.date(r.t) + ' · ' + units.ago(r.t, live.serverNow())),
          ] : h('div.st-rec-x', 'No record yet'));
      };

      task('records', 30e3, () => load([pWall], () => Promise.all([get('/api/stats/records', {}, 8e3), get('/api/stats/overview', {}, 8e3)]), ([rec, o]) => {
        const list = rec.records || [];
        const byKey = Object.fromEntries(list.map((r) => [r.key, r]));
        const tilesEl = RECORDS.map((def) => recTile(def, byKey[def.key]));
        for (const r of list) if (!recByKey[r.key]) tilesEl.push(recTile({ key: r.key, label: r.key.replace(/_/g, ' '), icon: 'bolt', color: C.grey, fmt: (v) => units.num(v, 1) }, r));
        const bd = o.busiest?.busiestDay;
        const bh = o.busiest?.busiestHour;
        if (bd?.t) tilesEl.push(recTile({ key: 'day', label: 'Busiest day', icon: 'chart', color: C.red, fmt: (v) => units.int(v) + ' aircraft' },
          { value: bd.uniqueAircraft, t: bd.t, when: bd.date + ' (station local day)' }, units.si(bd.frames) + ' messages · ' + units.int(bd.flights) + ' flights · peak ' + units.int(bd.peakAircraft)));
        if (bh?.t) tilesEl.push(recTile({ key: 'hour', label: 'Busiest hour', icon: 'timer', color: C.red, fmt: (v) => units.int(v) + ' at once' },
          { value: bh.aircraft, t: bh.t, when: 'hour starting ' + units.dateTime(bh.t) }, units.si(bh.frames) + ' messages in the hour'));
        pWall.sub('since ' + (o.allTime?.since > 0 ? units.date(o.allTime.since) : 'the first reception') + ' · all antennas');
        ui.mount(wall, tilesEl);
        if (!list.length && !bd?.t) pWall.empty('No records yet', 'Records are set as soon as the first aircraft with a position is tracked. The distance record needs the receiver location.', 'bolt');
        else pWall.ok();
      }));

      let newest = [];
      let older = [];
      const evRow = (e, now) => {
        const d = e.detail || {};
        const K = EVENT_KINDS[e.kind] || { label: e.kind, badge: 'other' };
        let text;
        if (e.kind === 'record') {
          text = ['New record · ', recByKey[d.key]?.label || d.key, ' ', h('b.num', recFmt(d.key, d.value)),
            typeof d.previous === 'number' ? ' (was ' + recFmt(d.key, d.previous) + ')' : '', d.callsign || d.reg ? ' · ' + (d.callsign || d.reg) : ''];
        } else if (e.kind === 'alert') {
          // alerts.Alert: numeric fields in the display units, the server text as the fallback
          const meta = [typeof d.altFt === 'number' ? units.alt(d.altFt, { fl: true }) : null, typeof d.distKm === 'number' ? units.dist(d.distKm) : null,
            typeof d.gsKt === 'number' ? units.speed(d.gsKt) : null].filter(Boolean).join(' · ');
          text = [h('b', d.ruleName || 'Alert'), ' ', units.alertText(d) || d.text || '', meta ? ' · ' + meta : ''];
        } else if (e.kind === 'feed_up' || e.kind === 'feed_down') {
          text = [h('b', d.name || d.feed || 'Feed'), e.kind === 'feed_up' ? ' connected' : ' disconnected', d.error ? ' · ' + d.error : ''];
        } else {
          text = [h('b', d.callsign || d.reg || e.icao || 'Unknown'), ' ',
            [d.squawk && 'squawk ' + d.squawk, d.emergency, d.reg && d.callsign ? d.reg : null, d.type, d.operator,
              typeof d.altFt === 'number' ? units.alt(d.altFt, { fl: true }) : null, typeof d.distKm === 'number' ? units.dist(d.distKm) : null,
              typeof d.gsKt === 'number' ? units.speed(d.gsKt) : null].filter(Boolean).join(' · '),
            d.country ? [' ', ui.flag(countryCode(d.country), d.country)] : null];
        }
        return h(e.icao ? 'a.st-ev' : 'div.st-ev', { href: e.icao ? acHref(e.icao) : null },
          h('span.st-ev-t.num', { tip: units.dateTime(e.t) }, units.ago(e.t, now)), ui.badge(K.label, K.badge), h('span.st-ev-x', text), h('span.st-ev-i.num', e.icao || ''));
      };
      const drawEvents = () => {
        const all = [...newest, ...older.filter((e) => !newest.some((n) => n.id === e.id))];
        if (!all.length) {
          pEv.empty('No events yet', evSeg.value ? 'No events of this kind have been logged.' : 'Events are logged when a feed connects, a record falls or a noteworthy aircraft shows up.', 'history');
          return;
        }
        const now = live.serverNow();
        ui.mount(evList, all.map((e) => evRow(e, now)));
        pEv.sub(units.int(all.length) + ' shown · all antennas');
        pEv.ok();
      };
      const evParams = (extra) => ({ limit: 60, ...(evSeg.value ? { kind: evSeg.value } : {}), ...extra });
      async function loadOlder() {
        const all = [...newest, ...older];
        if (!all.length) return;
        moreBtn.setLoading(true);
        try {
          const d = await api.get('/api/stats/events', { params: evParams({ before: Math.min(...all.map((e) => e.id)) }), signal: ctx.signal });
          const got = d.events || [];
          older.push(...got);
          moreBtn.classList.toggle('hidden', got.length < 60);
          drawEvents();
        } catch (e) { if (!e?.aborted) ui.toast.error(e.message || 'Could not load older events'); } finally { moreBtn.setLoading(false); }
      }
      task('records', 15e3, () => load([pEv], () => get('/api/stats/events', evParams(), 4e3), (d) => {
        newest = d.events || [];
        if (!older.length) moreBtn.classList.toggle('hidden', newest.length < 60);
        drawEvents();
      }));
    }

    // ------------------------------------------------------------------------------------------
    // tab: health
    // ------------------------------------------------------------------------------------------
    function buildHealth(root) {
      const FUN = [['accepted', 'Accepted', C.blue], ['duplicates', 'Duplicates', C.violet], ['crcBad', 'CRC failures', C.red], ['rejected', 'Rejected', C.amber], ['other', 'Unsupported / bad length', C.grey]];
      const funnel = h('div.st-funnel', FUN.map(([, , c]) => h('i', { style: { '--c': c } })));
      const qFigs = figures([
        ['frames', 'Frames received', 'Every Mode S frame delivered by the feeds since AirView started, including second copies.'],
        ['accepted', 'Accepted', 'Decoded and applied to an aircraft. Higher is better.'],
        ['dup', 'Duplicate rate', 'The same frame arriving again from another antenna or feed. Normal (and harmless) with two antennas; near zero with one.'],
        ['crc', 'CRC failures', 'Frames with a bad checksum: noise, interference or overload. Below 5 % is fine.'],
        ['fixed', 'Repaired', 'Frames accepted after correcting a single flipped bit.'],
        ['rejected', 'Rejected frames', 'Replies whose address could not be confirmed against a known aircraft - mostly noise that happened to pass the checksum.'],
        ['ppm', 'Positions per message', 'Position fixes per accepted frame. Around 0.15 - 0.3 is typical for ADS-B-rich traffic.'],
        ['posbad', 'Position rejects', 'Positions thrown away as implausible (failed decode or impossible jump). Should be a tiny fraction.'],
        ['mlat', 'MLAT positions', 'Positions that came from a multilateration feed.'],
        ['modeac', 'Mode A/C', 'Legacy replies without an address; counted, not tracked.'],
        ['tracked', 'Aircraft tracked', 'Shown now / held in memory.'],
        ['flights', 'Flights opened', 'Contact sessions started since AirView started.'],
      ]);
      const pQ = panel({ title: 'Decode quality', icon: 'shield', h: 180,
        top: null,
        caption: ['What happened to every frame since AirView started (all antennas). ', h('b', 'Good'), ': mostly accepted, CRC failures and rejects in the low single digits. Duplicates simply reflect how many antennas hear the same aircraft.'] });
      pQ.content.append(funnel, ui.legend(FUN.map(([, label, c]) => ({ label, color: c }))), h('div', { style: 'height:16px' }), qFigs.el);

      const feedsTable = ui.table({
        columns: [
          { key: 'name', label: 'Feed', render: (f) => h('span', { tip: f.addr }, f.name || f.id) },
          { key: 'type', label: 'Type', render: (f) => f.type || units.DASH },
          { key: 'state', label: 'State', render: (f) => (!f.enabled ? ui.badge('Disabled', 'other') : f.connected ? ui.badge('Connected', 'green') : ui.badge('Down', 'red', { tip: f.error || 'Not connected' })) },
          { key: 'msgRate', label: 'Rate', align: 'right', num: true, render: (f) => units.rate(f.msgRate) },
          { key: 'frames', label: 'Frames', align: 'right', num: true, render: (f) => units.si(f.frames) },
          { key: 'bytes', label: 'Data', align: 'right', num: true, render: (f) => units.bytes(f.bytes) },
          { key: 'lastFrame', label: 'Last frame', align: 'right', num: true, render: (f) => (f.lastFrame > 0 ? units.ago(f.lastFrame, live.serverNow()) : units.DASH) },
          { key: 'gps', label: 'GPS time', render: (f) => (f.gpsTime ? ui.badge('GPS', 'cyan') : units.DASH) },
        ],
        rows: [], empty: 'No feeds configured',
      });
      const antHost = h('div');
      const hubKv = ui.kv([['Data port (hub)', units.DASH], ['Hub clients', units.DASH], ['Mode', units.DASH, { text: true }]]);
      const pF = panel({ title: 'Feeds & antennas', icon: 'antenna', h: 160,
        caption: 'Where the data comes from, live. A feed that is enabled but "Down", or whose last frame is more than a few seconds old, is the first thing to check when the numbers drop.' });
      pF.content.append(h('div.st-scroll-x', feedsTable), ui.section('Per antenna since start', 'antenna'), antHost, ui.section('Sharing', 'link'), hubKv);

      const sysFigs = figures([
        ['uptime', 'Uptime'], ['mem', 'Memory in use', 'Go heap currently allocated / total obtained from the operating system.'],
        ['gor', 'Goroutines', 'Concurrent tasks inside AirView. Should stay roughly constant.'], ['gc', 'GC runs', 'Garbage collections since start and the last pause.'],
        ['clients', 'Live clients', 'Browsers connected to the live stream.'], ['cpus', 'CPU cores'],
      ]);
      const sysKv = ui.kv([['Version', units.DASH, { text: true }], ['Platform', units.DASH, { text: true }], ['Host', units.DASH, { text: true }], ['Time zone', units.DASH, { text: true }], ['Listening on', units.DASH], ['Forecast model', units.DASH, { text: true }]]);
      const dirEl = h('div.st-path');
      const pS = panel({ title: 'AirView process', icon: 'bolt', h: 160, caption: 'The program itself. Memory that keeps climbing for days, or a restart-required flag, are the things to look out for.' });
      pS.content.append(sysFigs.el, h('div', { style: 'height:16px' }), sysKv, h('div.mt-3', dirEl));

      const dbFigs = figures([
        ['size', 'Database size'], ['wal', 'Write-ahead log', 'Recent writes not yet merged into the main file. Shrinks at each checkpoint.'],
        ['queue', 'Write queue', 'Rows waiting to be written. Should hover near zero.'], ['flush', 'Last flush', 'How long the last batch write took and when.'],
        ['errors', 'Write errors', 'Failed flushes / operations and rows dropped under pressure. Anything above zero deserves a look at the log.'],
        ['free', 'Reclaimable', 'Free pages inside the file; "Compact" in Settings > Database returns them to the disk.'],
      ]);
      const tablesHost = h('div');
      const retKv = ui.kv([['Trail points', units.DASH, { text: true }], ['Flights', units.DASH, { text: true }], ['Minute statistics', units.DASH, { text: true }], ['Aircraft database', units.DASH, { text: true }]]);
      const pD = panel({ title: 'Database', icon: 'database', h: 160, caption: 'Everything the statistics are built from. Row counts grow with traffic until the retention limits (Settings > Database) start pruning.' });
      pD.content.append(dbFigs.el, ui.section('Rows per table', 'list'), tablesHost, ui.section('Retention', 'history'), retKv);

      root.append(h('div.grid.cols-2', pQ.card, pF.card, pS.card, pD.card));

      const pctOf = (a, b, digits = 2) => (b > 0 ? units.pct((a / b) * 100, digits) : units.DASH);
      task('health', 5e3, () => load([pQ], () => get('/api/stats/counters', {}, 2e3), (c) => {
        pQ.sub('since start · up ' + units.age(c.uptimeS));
        if (!c.frames) { pQ.empty('No frames received yet', 'Waiting for the first data from a feed. Check the feed state on the right.', 'shield'); return; }
        const other = (c.unsupported || 0) + (c.badLength || 0);
        const vals = { accepted: c.accepted, duplicates: c.duplicates, crcBad: c.crcBad, rejected: c.rejected, other };
        FUN.forEach(([k], i) => { funnel.children[i].style.flexGrow = String(vals[k] || 0); funnel.children[i].dataset.tip = FUN[i][1] + ': ' + units.int(vals[k]) + ' (' + pctOf(vals[k], c.frames) + ')'; });
        const crcPct = (c.crcBad / c.frames) * 100;
        const rejPct = (c.rejected / c.frames) * 100;
        qFigs.set('frames', units.si(c.frames), units.rate(c.msgRate) + ' now');
        qFigs.set('accepted', pctOf(c.accepted, c.frames, 1), units.si(c.accepted) + ' frames', 'good');
        qFigs.set('dup', pctOf(c.duplicates, c.frames, 1), units.si(c.duplicates) + ' frames');
        qFigs.set('crc', pctOf(c.crcBad, c.frames), units.si(c.crcBad) + ' frames', crcPct < 5 ? 'good' : crcPct < 20 ? 'warn' : 'bad');
        qFigs.set('fixed', units.si(c.fixed), pctOf(c.fixed, c.frames) + ' of frames');
        qFigs.set('rejected', pctOf(c.rejected, c.frames), units.si(c.rejected) + ' frames', rejPct < 5 ? '' : 'warn');
        qFigs.set('ppm', c.accepted > 0 ? units.num(c.positions / c.accepted, 3) : units.DASH, units.si(c.positions) + ' positions · ' + units.rate(c.posRate));
        qFigs.set('posbad', units.int((c.posRejected || 0) + (c.posJumps || 0)), units.int(c.posJumps) + ' jumps · ' + units.int(c.posPending) + ' pending');
        qFigs.set('mlat', units.si(c.mlatPositions), pctOf(c.mlatPositions, c.positions, 1) + ' of positions');
        qFigs.set('modeac', units.si(c.modeAC), units.si(c.status) + ' receiver status frames');
        qFigs.set('tracked', units.int(c.aircraft) + ' / ' + units.int(c.tracked), units.int(c.emergencies) + ' emergencies seen', c.emergencies ? 'warn' : '');
        qFigs.set('flights', units.int(c.flights));
        pQ.ok();
      }));

      task('health', 5e3, () => load([pF], () => get('/api/feeds', {}, 2e3), (d) => {
        const feeds = d.feeds || [];
        const up = feeds.filter((f) => f.enabled && f.connected).length;
        pF.sub(up + ' of ' + feeds.filter((f) => f.enabled).length + ' enabled feeds connected' + (d.demo ? ' · demo mode' : ''));
        feedsTable.setRows(feeds);
        const ants = (d.antennas || []).filter((a) => a.frames > 0 || a.id);
        ui.mount(antHost, ants.length
          ? barList(ants.map((a) => ({
            name: a.id ? antennas.find((x) => x.id === a.id)?.name || a.id : 'Combined / unattributed', value: a.frames, color: a.id ? antennas.find((x) => x.id === a.id)?.color : C.blue,
            sub: units.si(a.positions) + ' positions', right: units.rate(a.msgRate), rightSub: units.si(a.frames) + ' frames',
          })), { tight: true, rank: false })
          : h('div.muted', 'No per-antenna counters yet.'));
        hubKv.set(0, d.hub?.enabled ? 'port ' + d.hub.port : 'off');
        hubKv.set(1, d.hub?.enabled ? units.int(d.hub.clients) + (d.hub.droppedClients ? ' (' + units.int(d.hub.droppedClients) + ' dropped)' : '') : units.DASH);
        hubKv.set(2, (d.mode || units.DASH) + (d.demo ? ' (demo)' : ''));
        pF.ok();
      }));

      task('health', 10e3, () => load([pS], () => get('/api/system', {}, 4e3), (s) => {
        const m = s.memory || {};
        pS.sub((s.app || 'AirView') + ' ' + (s.version || '') + (s.restartRequired ? ' · restart required' : ''));
        sysFigs.set('uptime', units.age(s.uptimeS), 'since ' + units.dateTime(s.started));
        sysFigs.set('mem', units.bytes(m.allocBytes), units.bytes(m.sysBytes) + ' from the OS');
        sysFigs.set('gor', units.int(s.goroutines));
        sysFigs.set('gc', units.si(m.numGc), 'last pause ' + units.num(m.gcPauseMs, 2) + ' ms');
        sysFigs.set('clients', units.int(s.liveClients), units.int(s.sessions) + ' admin sessions');
        sysFigs.set('cpus', units.int(s.cpus), s.goVersion || '');
        sysKv.set(0, (s.version || units.DASH) + (s.gui ? ' (desktop)' : ' (headless)'));
        sysKv.set(1, s.platform);
        sysKv.set(2, (s.hostname || units.DASH) + (s.pid ? ' · pid ' + s.pid : ''));
        sysKv.set(3, (s.timeZone || 'UTC') + ' (UTC' + (s.utcOffsetSec >= 0 ? '+' : '−') + units.num(Math.abs(s.utcOffsetSec) / 3600, 2, 0) + ' h)');
        sysKv.set(4, s.httpAddr || ':' + s.httpPort);
        sysKv.set(5, s.forecaster?.fitted > 0 ? 'fitted ' + units.ago(s.forecaster.fitted, s.now) + ' on ' + units.int(s.forecaster.dataHours) + ' h of data' : 'not fitted yet');
        ui.setText(dirEl, 'Data directory: ' + (s.dataDir || units.DASH));
        pS.ok();
      }));

      task('health', 30e3, () => load([pD], () => get('/api/db/stats', {}, 10e3), (d) => {
        const s = d.stats || {};
        const errs = (s.flushErrors || 0) + (s.failedOps || 0);
        const dropped = (s.droppedLow || 0) + (s.droppedMid || 0) + (s.droppedHigh || 0);
        const rows = Object.entries(d.tables || {}).filter(([k]) => k !== 'schema_migrations').sort((a, b) => b[1] - a[1]);
        pD.sub(units.bytes((s.dbBytes || 0) + (s.walBytes || 0)) + ' on disk · ' + units.si(rows.reduce((a, r) => a + r[1], 0)) + ' rows');
        dbFigs.set('size', units.bytes(s.dbBytes), units.int(s.pageCount) + ' pages');
        dbFigs.set('wal', units.bytes(s.walBytes));
        dbFigs.set('queue', units.int(s.queuedRows), units.int(s.queuedOps) + ' operations');
        dbFigs.set('flush', units.num(s.lastFlushMs, 1) + ' ms', s.lastFlush > 0 ? units.ago(s.lastFlush, live.serverNow()) + ' · ' + units.si(s.flushes) + ' flushes' : 'none yet');
        dbFigs.set('errors', units.int(errs), units.int(dropped) + ' rows dropped', errs || dropped ? 'bad' : 'good');
        dbFigs.set('free', units.bytes((s.freelistPages || 0) * (s.pageSize || 0)), units.int(s.freelistPages) + ' free pages');
        const logMax = Math.log10(Math.max(10, rows[0]?.[1] || 10));
        ui.mount(tablesHost, barList(rows.map(([k, n]) => ({ name: k, value: n > 0 ? (Math.log10(n + 1) / logMax) * 100 : 0, right: units.int(n), color: C.blue })), { tight: true, rank: false }));
        const g = d.storage || {};
        const days = (n) => (n > 0 ? 'kept ' + units.int(n) + ' days' : 'kept forever');
        retKv.set(0, days(g.trailRetentionDays) + ' · one point per ' + units.int(g.trailIntervalSec) + ' s');
        retKv.set(1, days(g.flightRetentionDays));
        retKv.set(2, days(g.minuteRetentionDays) + ' (hourly statistics are kept)');
        retKv.set(3, d.acdb?.installed ? units.int(d.acdb.rows) + ' aircraft' : 'not installed');
        pD.ok();
      }));
    }

    // ------------------------------------------------------------------------------------------
    // assemble
    // ------------------------------------------------------------------------------------------
    const builders = { overview: buildOverview, patterns: buildPatterns, messages: buildMessages, fleet: buildFleet, routes: buildRoutes, records: buildRecords, health: buildHealth };
    const TABS = [
      { id: 'overview', label: 'Overview', icon: 'chart' }, { id: 'patterns', label: 'Patterns', icon: 'history' },
      { id: 'messages', label: 'Messages', icon: 'signal' }, { id: 'fleet', label: 'Fleet', icon: 'plane' }, { id: 'routes', label: 'Routes', icon: 'route' },
      { id: 'records', label: 'Records & events', icon: 'bolt' }, { id: 'health', label: 'Health', icon: 'shield' },
    ];
    const tabsEl = ui.tabs({
      active: st.tab,
      tabs: TABS.map((t) => ({ ...t, render: (el) => builders[t.id](el), onShow: () => { st.tab = t.id; tick(); } })),
      onChange: (id) => ctx.navigate('stats/' + id, { replace: true }),
    });

    container.append(h('div.page-inner',
      h('div.page-head', h('div', h('h2', 'Statistics'), subEl), h('div.actions', rangeSeg, antSel)),
      tiles, tabsEl));

    ctx.every(2500, tick);
    const onVis = () => { if (!document.hidden) tick(); };
    document.addEventListener('visibilitychange', onVis);
    ctx.onCleanup(() => document.removeEventListener('visibilitychange', onVis));
    ctx.onCleanup(units.onChange(() => { drawCounts(); drawToday(); invalidate(); }));
    tick();

    active = { select: (id) => { if (TAB_IDS.includes(id) && tabsEl.active !== id) tabsEl.select(id); } };
  },

  onRoute(route) {
    if (active) active.select(route.args[0] || 'overview');
  },

  unmount() {
    active = null;
  },
};
