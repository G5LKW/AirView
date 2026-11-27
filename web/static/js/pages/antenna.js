// pages/antenna.js - Antenna Lab: how well does this station hear, where, and why.
//
// Data (all GET, refreshed every 2 minutes; the server caches each analysis for 5 minutes):
//   /api/antenna/score      station score, sub-scores, findings
//   /api/antenna/coverage   polar range per bearing and altitude band, radio horizon, skyline, obstructions
//   /api/antenna/report     signal-vs-distance, azimuth / elevation patterns, modelled antennas, yagi fit, link budget
//   /api/antenna/daynight   day / twilight / night comparison
//   /api/antenna/heat       all-time position grid (density, mean RSSI, lowest altitude) for the mini-map
//   /api/antenna/horizon    ground elevation used by the what-if calculator
//   /api/antenna/budget, /api/radio/transponders   calibration of the client-side what-if model
//   /api/antenna/coverage.geojson                  "show on live map" (published as ctx.shared 'map.coverage')
//
// Tabs: "Station analysis" (everything above) and "Terrain & line of sight" (#/antenna/terrain, built lazily by
// ../antenna/terrain.js over /api/terrain/* and /api/rf/summary; it reuses the /api/antenna/coverage reply above
// for the measured skyline and range, so the period / antenna selectors apply to both tabs).

import { basemapById, styleFor, makeTransformRequest } from '../map/basemaps.js';
import { calibrate, predict, DEFAULTS } from '../antenna/linkmodel.js';
import { itemText } from '../rf/advice.js';

const REFRESH_MS = 120000;
const API_TIMEOUT = 60000;
const BAND_EDGES_FT = [0, 1000, 5000, 10000, 20000, 30000, 40000, null];
const PERIODS = [{ value: 1, label: '24 h' }, { value: 7, label: '7 d' }, { value: 30, label: '30 d' }, { value: 90, label: '90 d' }];
const GRADE = {
  excellent: { color: '#34d399', badge: 'green' }, good: { color: '#22d3ee', badge: 'cyan' },
  fair: { color: '#fbbf24', badge: 'amber' }, poor: { color: '#ff4d5e', badge: 'red' }, 'no data': { color: '#4a5d82', badge: 'other' },
};
const FLOOR_RAMP = [[0, '#22d3ee'], [3000, '#34d399'], [8000, '#a3e635'], [15000, '#fde047'], [25000, '#fb923c'], [40000, '#ff4d5e']];

const isNum = (v) => typeof v === 'number' && Number.isFinite(v);
const norm360 = (d) => ((d % 360) + 360) % 360;
const clamp = (v, lo, hi) => Math.max(lo, Math.min(hi, v));
const r1 = (v) => Math.round(v * 10) / 10;

export default {
  id: 'antenna',
  title: 'Antenna Lab',

  async mount(container, ctx) {
    const { api, units, ui, charts, config } = ctx;
    const { h } = ui;
    const C = charts.colors;
    await ui.loadCss(new URL('../../css/antenna.css', import.meta.url));

    const prefs = ui.prefs('airview.antenna.v1', { days: 30, mapMode: 'density' });
    const S = {
      ant: '', days: PERIODS.some((p) => p.value === prefs.get().days) ? prefs.get().days : 30, band: -1,
      src: { cov: {}, rep: {}, score: {}, dn: {}, heat: {}, hor: {} },
      seq: 0, alive: true, map: null, mapMode: prefs.get().mapMode, mapSeq: 0,
      model: { ...DEFAULTS }, classes: null, whatIf: null,
    };
    ctx.onCleanup(() => { S.alive = false; if (S.map) { try { S.map.remove(); } catch { /* already gone */ } S.map = null; } });

    // ---- small helpers --------------------------------------------------------------------------
    const dist = (km) => (isNum(km) ? r1(units.value('dist', km)) : null);
    const distFmt = (v) => `${units.num(v, 0)} ${units.label('dist')}`;
    const bandLabel = (band) => {
      if (band < 0) return 'All altitudes';
      const lo = BAND_EDGES_FT[band];
      const hi = BAND_EDGES_FT[band + 1];
      if (hi === null) return `above ${units.alt(lo)}`;
      if (lo === 0) return `below ${units.alt(hi)}`;
      return `${units.format('alt', lo, { unit: false })}–${units.alt(hi)}`;
    };
    const antennas = () => config.current?.antennas || [];
    const antName = (id) => antennas().find((a) => a.id === id)?.name || id || 'Combined';
    const brgAxis = { type: 'value', min: 0, max: 360, interval: 45, axisLabel: { formatter: (v) => ['N', 'NE', 'E', 'SE', 'S', 'SW', 'W', 'NW', 'N'][Math.round(v / 45)] ?? '' } };

    /** A card whose body is either a chart or custom content, with loading / empty / error overlays. */
    function panel(o) {
      const main = o.content || h('div.chart', { class: o.chartClass, style: o.height ? { '--h': o.height + 'px' } : null });
      const overlay = h('div.al-overlay');
      const extra = h('div.al-extra');
      const card = ui.card({
        title: o.title, icon: o.icon, sub: o.sub ?? '', actions: o.actions, class: o.class,
        body: [o.top, h('div.al-wrap', main, overlay), o.caption ? h('p.al-caption', o.caption) : null, extra],
      });
      let chart = null;
      const set = (node) => { ui.mount(overlay, node); overlay.classList.add('on'); };
      return {
        card, main, extra,
        get chart() { if (!chart) chart = charts.create(main); return chart; },
        loading: () => set(ui.loading('Analysing…', { compact: true })),
        empty: (title, text, icon) => set(ui.empty({ icon: icon || 'radar', title, text, compact: true })),
        error: (err) => set(ui.error(err, { retry: () => refresh(), compact: true, title: 'Analysis failed' })),
        ready: () => { overlay.classList.remove('on'); ui.clear(overlay); },
      };
    }
    /** Drive a panel from one data source: loading -> error -> empty -> ready. */
    function show(p, src, render) {
      if (src.error) return p.error(src.error);
      if (!src.data) return p.loading();
      let res;
      try { res = render(src.data); } catch (e) { console.error('[antenna] render failed', e); return p.error(e); }
      if (res && res.empty) p.empty(res.empty[0], res.empty[1], res.empty[2]); else p.ready();
      return undefined;
    }
    const needLocation = ['Receiver location needed', 'This analysis needs to know where the antenna is. Set it in Settings, or let Locate Me estimate it from the traffic.', 'pin'];
    const needPositions = (what) => ['Not enough data yet', `${what} appears once aircraft positions have been received in the selected period. A first picture forms within minutes; a trustworthy one needs a few days of traffic from all directions.`];

    // ---- header ---------------------------------------------------------------------------------
    const subEl = h('div.sub', 'Coverage, patterns and link budget of this station');
    const antSeg = h('div.al-antsel.hidden');
    const periodSeg = ui.segmented({ options: PERIODS, value: S.days, onChange: (v) => { S.days = v; prefs.set({ days: v }); refresh(); } });
    const refreshBtn = ui.button({ icon: 'refresh', tip: 'Refresh now', variant: 'ghost', onClick: () => refresh() });
    const banner = h('div.al-banner.hidden');
    const tiles = {
      best: ui.stat({ label: 'Best range', icon: 'radar', accent: 'cyan', tip: 'Farthest position received in the selected period, and its bearing' }),
      pct: ui.stat({ label: 'Of radio horizon', icon: 'globe', tip: 'Reliable range as a share of the theoretical line of sight, averaged around the compass (all altitudes)' }),
      pos: ui.stat({ label: 'Positions analysed', icon: 'plane', accent: 'violet' }),
      sky: ui.stat({ label: 'Skyline', icon: 'compass', accent: 'amber', tip: 'Median of the lowest elevation angle at which aircraft are still received' }),
    };

    function buildAntSelector() {
      const list = antennas();
      const feeds = config.current?.feeds || [];
      const measured = (S.src.rep.data?.antennas || []).some((a) => a.measured);
      const usable = list.length > 1 && (ctx.status?.demo || measured || S.ant || feeds.some((f) => f.antennaId));
      antSeg.classList.toggle('hidden', !usable);
      if (!usable) return;
      const key = list.map((a) => a.id + a.name).join('|');
      if (antSeg.dataset.key === key) return;
      antSeg.dataset.key = key;
      ui.mount(antSeg, ui.segmented({
        options: [{ value: '', label: 'Combined', tip: 'All antennas together' }, ...list.map((a) => ({ value: a.id, label: a.name || a.id, tip: 'Only data attributed to this antenna' }))],
        value: S.ant, onChange: (v) => { S.ant = v; refresh(); },
      }));
    }

    // ---- panels ---------------------------------------------------------------------------------
    // score
    const dialEl = h('div.chart.al-dial');
    const subsEl = h('div.al-subs');
    const P = {};
    P.score = panel({
      title: 'Station score', icon: 'bolt', class: 'card--accent',
      content: h('div.al-score', dialEl, subsEl),
      caption: 'One number for the whole receive chain: range against the radio horizon, evenness around the compass, sensitivity, decode quality and uptime. 85+ is an excellent site.',
    });
    P.findings = panel({ title: 'Findings & advice', icon: 'info', content: h('div.al-findings') });
    let dial = null;

    // coverage
    const bandSel = ui.select({ size: 'sm', options: [{ value: -1, label: 'All altitudes' }], value: -1, onChange: (v) => { S.band = Number(v); renderCoverage(); renderPct(); } });
    const mapBtn = ui.button({ label: 'Show on live map', icon: 'map', size: 'sm', variant: 'primary', onClick: showOnMap, tip: 'Draw these coverage outlines on the Live Map' });
    P.cov = panel({
      title: 'Coverage', icon: 'radar', actions: [bandSel, mapBtn], chartClass: 'al-covchart',
      caption: 'How far the station hears in every direction. Filled: the reliable (robust) envelope; thin line: the absolute maximum; dashed ring: the theoretical radio horizon for your antenna height. Good = the envelope fills the ring evenly; dents point at obstructions. Antenna shapes are the modelled patterns scaled to the horizon, the wedge is the yagi beam.',
    });
    P.pct = panel({
      title: 'Share of the radio horizon reached', icon: 'chart', height: 240,
      caption: 'Reliable range as a percentage of line of sight, per bearing. Above 80 % is very good; a dip that stays in one direction is a physical obstruction, not a receiver problem.',
    });
    P.sky = panel({
      title: 'Measured skyline', icon: 'compass', height: 240,
      caption: 'Your horizon as the radio sees it: the lowest elevation angle at which aircraft are still received, around the compass. Flat and close to 0° is ideal; humps are buildings, trees or terrain (marked sectors are detected obstructions).',
    });

    // signal
    P.sig = panel({
      title: 'Signal level against distance', icon: 'signal', height: 340, class: 'span-2',
      caption: 'Every received position by distance and signal level (brighter = more). In free space the level drops 20 dB per tenfold distance (dashed). A fitted line that is much flatter means weak far signals are being lost; points crowding the top edge mean overload - reduce gain.',
    });
    const fitEl = h('div.al-fit');

    // patterns
    P.az = panel({
      title: 'Azimuth gain pattern', icon: 'compass', height: 380,
      caption: 'Measured: relative signal strength per bearing after removing the effect of distance (0 dB = best direction). Dashed: the modelled pattern of each configured antenna. For an omni the measured curve should be a circle; a directional antenna should show its lobe where it points.',
    });
    P.el = panel({
      title: 'Elevation pattern', icon: 'forecast', height: 380,
      caption: 'Relative signal strength by elevation angle above the horizon, measured against the modelled antenna. Most traffic sits below 5°, so a pattern that keeps its gain right down to the horizon is what brings range.',
    });
    P.yagi = panel({ title: 'Inferred yagi beam', icon: 'target', content: h('div.al-yagi'), class: 'span-2' });

    // link budget
    P.link = panel({
      title: 'Link budget: predicted against achieved', icon: 'ruler', content: h('div.al-link'), class: 'span-2',
      caption: 'Predicted: how far the configured antennas should reach per altitude band - limited either by the radio horizon or by signal power. Achieved: what was actually received. Reaching 85 % or more of the prediction is a healthy installation.',
    });
    const whatIfBody = h('div.al-whatif');
    P.whatif = panel({
      title: 'What-if calculator', icon: 'bolt', content: whatIfBody, class: 'span-2',
      caption: 'Try a different antenna height, gain, cable or preamplifier before buying anything. Smooth-earth model with free-space loss - real terrain and buildings only ever reduce these numbers.',
    });

    // day / night
    const dnTiles = h('div.grid.cols-3.al-dn-tiles');
    const dnChart = h('div.chart', { style: { '--h': '240px' } });
    const dnText = h('div.al-dn-text');
    P.dn = panel({
      title: 'Day against night', icon: 'sun', content: h('div', dnTiles, dnChart), class: 'span-2',
      caption: 'Typical far range (95th percentile) by hour of day. "Traffic-normalised" removes the effect of having more aircraft to hear, so what remains is propagation and noise. A flat line means the station performs the same around the clock.',
    });
    let dnChartH = null;

    // mini map
    const mapEl = h('div.al-map');
    const mapTip = h('div.al-maptip.hidden');
    const mapLegend = h('div.al-maplegend');
    const mapMode = ui.segmented({
      options: [{ value: 'density', label: 'Positions', tip: 'Where the received positions are' }, { value: 'rssi', label: 'Signal', tip: 'Mean signal level per cell' }, { value: 'floor', label: 'Coverage floor', tip: 'Lowest altitude ever received per cell' }],
      value: S.mapMode, onChange: (v) => { S.mapMode = v; prefs.set({ mapMode: v }); paintMap(); },
    });
    P.map = panel({
      title: 'Position heat-map & coverage floor', icon: 'map', actions: mapMode, class: 'span-2',
      content: h('div.al-mapwrap', mapEl, mapTip), sub: 'All-time',
      caption: 'Positions: where traffic was received. Signal: mean level per cell. Coverage floor: the lowest altitude ever heard in each cell - low values far out mean a clear view, a floor that rises quickly with distance in one direction means something blocks the horizon there.',
    });
    P.map.extra.append(mapLegend);

    const notesEl = h('div.al-notes');

    const stationGrid = h('div',
      h('div.grid.cols-2.al-grid',
        P.score.card, P.findings.card,
        P.cov.card,
        h('div.al-stack', P.pct.card, P.sky.card),
        P.sig.card,
        P.az.card, P.el.card,
        P.yagi.card,
        P.link.card,
        P.whatif.card,
        P.dn.card,
        P.map.card),
      notesEl);
    // "Terrain & line of sight" lives in its own module, built the first time its tab is opened
    let terrain = null;
    const startTab = ctx.route.args[0] === 'terrain' ? 'terrain' : 'station';
    const tabsEl = ui.tabs({
      class: 'al-tabs', active: startTab,
      tabs: [
        { id: 'station', label: 'Station analysis', icon: 'radar', content: stationGrid, onShow: () => { if (terrain) terrain.hidden(); } },
        {
          id: 'terrain', label: 'Terrain & line of sight', icon: 'mountain',
          render: (panelEl) => {
            panelEl.append(h('div.al-tabload', ui.loading('Loading…', { compact: true })));
            import('../antenna/terrain.js').then(({ createTerrainLab }) => {
              if (!S.alive) return;
              terrain = createTerrainLab(ctx, panelEl, {
                measured: () => S.src.cov,
                pageAnt: () => S.ant,
                periodLabel: () => `${S.ant ? antName(S.ant) : 'all antennas'}, last ${PERIODS.find((p) => p.value === S.days)?.label || ''}`,
                bandLabel, bandEdges: BAND_EDGES_FT, niceMax,
              });
              if (tabsEl.active === 'terrain') terrain.shown();
            }).catch((e) => { console.error('[antenna] terrain module failed', e); ui.mount(panelEl, ui.error(e, { title: 'The terrain tab could not be loaded' })); });
          },
          onShow: () => { if (terrain) terrain.shown(); },
        },
      ],
      onChange: (id) => ctx.navigate(id === 'terrain' ? 'antenna/terrain' : 'antenna', { replace: true }),
    });
    S.selectTab = (id) => { if (tabsEl.active !== id) tabsEl.select(id); };

    container.append(h('div.page-inner.al',
      h('div.page-head',
        h('div', h('h2', 'Antenna Lab'), subEl),
        h('div.actions', antSeg, periodSeg, refreshBtn)),
      banner,
      h('div.grid.cols-4.al-tiles', tiles.best, tiles.pct, tiles.pos, tiles.sky),
      tabsEl));
    active = S;
    ctx.onCleanup(() => { if (active === S) active = null; });
    P.sig.extra.append(fitEl); // the path-loss fit summary lives under the heat-map
    P.dn.extra.append(dnText);

    // ---- renderers ------------------------------------------------------------------------------
    function renderHead() {
      const cov = S.src.cov.data;
      const period = PERIODS.find((p) => p.value === S.days)?.label || '';
      ui.setText(subEl, `${S.ant ? antName(S.ant) + ' antenna' : 'All antennas combined'} · last ${period}`
        + (cov ? ` · ${units.si(cov.positions)} positions on ${cov.days} day${cov.days === 1 ? '' : 's'}` : ''));
      const located = cov ? cov.located : ctx.status ? ctx.status.located : true;
      banner.classList.toggle('hidden', located !== false);
      if (located === false && !banner.firstChild) {
        ui.mount(banner, ui.icon('pin'),
          h('div.grow', h('b', 'Receiver location not set'), h('span', 'Range, horizon, skyline and pattern analyses need the antenna position.')),
          ui.button({ label: 'Locate Me', icon: 'target', size: 'sm', href: '#/guess' }),
          ui.button({ label: 'Settings', icon: 'settings', size: 'sm', variant: 'primary', href: '#/settings' }));
      }
      if (!cov) { for (const t of Object.values(tiles)) { t.set(null, ''); t.setSub(S.src.cov.error ? 'unavailable' : 'loading…'); } return; }
      const o = cov.overall || {};
      tiles.best.set(units.format('dist', o.bestKm, { unit: false, digits: 0 }), units.label('dist'));
      tiles.best.setSub(isNum(o.bestBrg) ? `towards ${units.bearing(o.bestBrg)}` : 'no positions yet');
      tiles.pct.set(cov.positions ? units.num(o.meanPct, 0) : null, '%');
      tiles.pct.setSub(isNum(o.horizonKm) ? `horizon ${units.dist(o.horizonKm)} · antenna ${units.height(cov.antennaAmslM, { digits: 0 })} AMSL` : '');
      tiles.pos.set(units.si(cov.positions), '');
      tiles.pos.setSub(`${o.bearings ?? 0} of 180 bearings with data`);
      const sk = cov.skyline || {};
      tiles.sky.set(isNum(sk.medianDeg) ? units.num(sk.medianDeg, 1, 1) : null, '°');
      const nObs = (cov.obstructions || []).length;
      tiles.sky.setSub(isNum(sk.medianDeg) ? (nObs ? `${nObs} obstruction${nObs === 1 ? '' : 's'} detected` : 'no obstructions detected') : 'not measured yet');
    }

    function renderScore() {
      show(P.score, S.src.score, (sc) => {
        const g = GRADE[sc.grade] || GRADE['no data'];
        const has = isNum(sc.score);
        if (!dial) dial = charts.create(dialEl);
        dial.set({
          series: [{
            type: 'gauge', startAngle: 220, endAngle: -40, min: 0, max: 100, radius: '84%', center: ['50%', '57%'],
            progress: { show: has, width: 14, roundCap: true, itemStyle: { color: { type: 'linear', x: 0, y: 1, x2: 1, y2: 0, colorStops: [{ offset: 0, color: '#1a4fd0' }, { offset: 1, color: g.color }] }, shadowColor: g.color, shadowBlur: 14 } },
            axisLine: { roundCap: true, lineStyle: { width: 14, color: [[1, 'rgba(120,165,255,0.12)']] } },
            axisTick: { show: false }, pointer: { show: false }, anchor: { show: false },
            splitLine: { distance: -22, length: 5, lineStyle: { color: C.line2, width: 1 } },
            axisLabel: { distance: -14, color: C.text4, fontSize: 9, formatter: (v) => (v % 50 === 0 ? v : '') },
            title: { offsetCenter: [0, '36%'], color: g.color, fontSize: 12, fontWeight: 650 },
            detail: { valueAnimation: has, offsetCenter: [0, '-2%'], fontSize: 46, fontWeight: 500, color: '#fff', fontFamily: charts.theme.gauge.detail.fontFamily, formatter: (v) => (has ? String(Math.round(v)) : '—') },
            data: [{ value: has ? sc.score : 0, name: String(sc.grade || '').toUpperCase() }],
          }],
        }, { replace: true });
        ui.mount(subsEl, (sc.subs || []).map((s) => {
          const m = ui.meter(isNum(s.score) ? s.score : 0);
          return h('div.al-sub' + (s.available ? '' : '.off'),
            h('div.al-sub-head', h('span.al-sub-label', s.label), h('span.al-sub-weight', { tip: 'Weight in the total score' }, `${units.num(s.weight, 0)} %`), h('span.num.al-sub-score', isNum(s.score) ? units.num(s.score, 0) : '—')),
            m, h('div.al-sub-detail', itemText({ text: s.detail || '', data: s.data }, units)));
        }));
        return null;
      });
      show(P.findings, S.src.score, (sc) => {
        const list = sc.findings || [];
        if (!list.length) return { empty: ['Nothing to report yet', 'Findings and advice appear here as soon as there is enough data to judge the station.', 'info'] };
        const ICON = { good: 'check-circle', warn: 'alert', info: 'info' };
        const order = { warn: 0, info: 1, good: 2 };
        ui.mount(P.findings.main, [...list].sort((a, b) => (order[a.level] ?? 1) - (order[b.level] ?? 1)).map((f) =>
          h('div.al-finding.' + (ICON[f.level] ? f.level : 'info'), ui.icon(ICON[f.level] || 'info'), h('div', itemText(f, units)))));
        return null;
      });
    }

    function currentBand(cov) {
      if (S.band < 0) return cov.overall;
      return (cov.bands || []).find((b) => b.band === S.band) || cov.overall;
    }

    function renderCoverage() {
      show(P.cov, S.src.cov, (cov) => {
        // band selector follows the data and the unit system
        const opts = [{ value: -1, label: 'All altitudes' }, ...(cov.bands || []).map((b) => ({ value: b.band, label: bandLabel(b.band) + (b.positions ? '' : ' (no data)') }))];
        bandSel.setOptions(opts);
        bandSel.value = String(S.band);
        mapBtn.disabled = !cov.located || !cov.positions;
        if (!cov.located) return { empty: needLocation };
        if (!cov.positions) return { empty: needPositions('The coverage plot') };
        const b = currentBand(cov);
        const brg = cov.bearingDeg;
        const env = (arr) => brg.map((d, i) => [dist(arr[i]) ?? 0, d]);
        const hz = dist(b.horizonKm);
        const peak = Math.max(hz || 0, ...b.maxKm.map((v) => dist(v) || 0));
        const maxR = niceMax(peak * 1.04);
        const series = [];
        if (hz) series.push({ name: 'Radio horizon', data: brg.map((d) => [hz, d]), color: '#c9d8f5', width: 1.2, _dashed: true });
        series.push({ name: 'Maximum', data: env(b.maxKm), color: C.cyan, width: 1 });
        series.push({ name: 'Reliable (robust)', data: env(b.robustKm), color: C.blue, width: 2, area: 0.3 });
        const models = S.src.rep.data?.antennas || [];
        const ref = hz || peak;
        for (const a of models) {
          if (S.ant && a.id !== S.ant) continue;
          const col = a.color || C.amber;
          if (Array.isArray(a.azRelDb) && a.azRelDb.length === brg.length) {
            series.push({ name: `${a.name} pattern`, data: brg.map((d, i) => [r1(ref * 10 ** ((a.azRelDb[i] ?? -40) / 20)), d]), color: col, width: 1.3, _dashed: true });
          }
          if (a.info?.directional && isNum(a.info.azimuthDeg) && a.info.hBeamwidthDeg > 0 && a.info.hBeamwidthDeg < 360) {
            const w = a.info.hBeamwidthDeg;
            const pts = [[0, norm360(a.info.azimuthDeg)]];
            for (let d = -w / 2; d <= w / 2 + 1e-6; d += Math.max(1, w / 24)) pts.push([maxR, norm360(a.info.azimuthDeg + d)]);
            pts.push([0, norm360(a.info.azimuthDeg)]);
            series.push({ name: `${a.name} beam ${Math.round(w)}°`, data: pts, color: col, width: 1, area: 0.1, closed: false });
          }
        }
        const yagi = S.src.rep.data?.yagi;
        if (yagi?.ok && isNum(yagi.azimuthDeg)) {
          series.push({ name: 'Measured boresight', data: [[0, norm360(yagi.azimuthDeg)], [maxR, norm360(yagi.azimuthDeg)]], color: '#ffffff', width: 1.6, closed: false, _dashed: true });
        }
        const opt = charts.polar({ series, maxR, rFormat: distFmt, legend: true });
        opt.series.forEach((s, i) => { if (series[i]._dashed) s.lineStyle.type = 'dashed'; s.z = series[i].area ? 2 : 3; });
        P.cov.chart.set(opt, { replace: true });
        P.cov.card.setSub(`${bandLabel(S.band)} · best ${units.dist(b.bestKm)} at ${units.bearing(b.bestBrg)} · horizon ${units.dist(b.horizonKm)}`);
        return null;
      });
    }

    function renderPct() {
      show(P.pct, S.src.cov, (cov) => {
        if (!cov.located) return { empty: needLocation };
        if (!cov.positions) return { empty: needPositions('The horizon comparison') };
        const b = currentBand(cov);
        if (!b.positions) return { empty: ['No traffic in this altitude band', 'Nothing was received in the selected band during this period. Pick another band or a longer period.'] };
        const data = cov.bearingDeg.map((d, i) => [d, isNum(b.pctOfHorizon[i]) ? r1(b.pctOfHorizon[i]) : 0]);
        const top = Math.max(100, ...data.map((d) => d[1]));
        const opt = charts.line({
          xType: 'value', legend: false, yMin: 0, yMax: Math.ceil(top / 10) * 10, yFormat: (v) => v + '%',
          tooltip: (ps) => `${units.bearing(ps[0].value[0])}<br><b>${units.pct(ps[0].value[1], 0)}</b> of the radio horizon`,
          series: [{
            name: '% of horizon', data, width: 1.6, color: C.blue,
            markLine: isNum(b.meanPct) ? { silent: true, symbol: 'none', data: [{ yAxis: r1(b.meanPct) }], label: { formatter: `mean ${units.pct(b.meanPct, 0)}`, position: 'insideEndTop' } } : undefined,
          }],
        });
        Object.assign(opt.xAxis, brgAxis);
        opt.series[0].areaStyle = { opacity: 0.22 };
        opt.visualMap = { show: false, dimension: 1, seriesIndex: 0, pieces: [{ lt: 50, color: C.red }, { gte: 50, lt: 80, color: C.amber }, { gte: 80, color: C.green }] };
        P.pct.chart.set(opt, { replace: true });
        P.pct.card.setSub(`${bandLabel(S.band)} · horizon ${units.dist(b.horizonKm)}`);
        return null;
      });
    }

    function renderSkyline() {
      show(P.sky, S.src.cov, (cov) => {
        const sk = cov.skyline || {};
        if (!cov.located) return { empty: needLocation };
        if (!sk.bearings) return { empty: ['Skyline not measured yet', 'The skyline is built from the lowest aircraft received in each direction. It needs low, distant traffic all around - typically a day or two of data.'] };
        const data = cov.bearingDeg.map((d, i) => [d, isNum(sk.elevDeg[i]) ? Math.round(sk.elevDeg[i] * 100) / 100 : null]);
        const vals = data.map((d) => d[1]).filter(isNum);
        const lo = Math.floor(Math.min(...vals, 0) - 0.5);
        const hi = Math.max(3, Math.ceil(Math.max(...vals) + 0.5));
        const areas = [];
        for (const ob of cov.obstructions || []) {
          const label = { name: `${ob.compass || ''} ${ob.severity || ''}`.trim() };
          const from = norm360(ob.fromDeg);
          const to = norm360(ob.toDeg);
          if (to >= from) areas.push([{ ...label, xAxis: from }, { xAxis: to }]);
          else areas.push([{ ...label, xAxis: from }, { xAxis: 360 }], [{ xAxis: 0 }, { xAxis: to }]);
        }
        const opt = charts.line({
          xType: 'value', legend: false, yMin: lo, yMax: hi, yFormat: (v) => v + '°',
          tooltip: (ps) => `${units.bearing(ps[0].value[0])}<br>lowest reception at <b>${isNum(ps[0].value[1]) ? units.num(ps[0].value[1], 2, 2) + '°' : '—'}</b> elevation`,
          series: [{
            name: 'Skyline', data, color: C.amber, width: 1.6, step: 'middle',
            markLine: isNum(sk.medianDeg) ? { silent: true, symbol: 'none', data: [{ yAxis: sk.medianDeg }], label: { formatter: `median ${units.num(sk.medianDeg, 2, 2)}°`, position: 'insideEndTop' } } : undefined,
            markArea: areas.length ? { silent: true, itemStyle: { color: 'rgba(255,77,94,0.14)' }, label: { color: C.red, fontSize: 10 }, data: areas } : undefined,
          }],
        });
        Object.assign(opt.xAxis, brgAxis);
        opt.series[0].areaStyle = { origin: 'start', color: charts.fade(C.amber, 0.05, 0.4) };
        P.sky.chart.set(opt, { replace: true });
        P.sky.card.setSub(sk.source === 'coverage' ? 'derived from range per altitude band' : 'from the lowest aircraft per map cell');
        ui.mount(P.sky.extra, (cov.obstructions || []).map((ob) => h('div.al-finding.warn', ui.icon('alert'), h('div', ob.text || `${ob.compass}: ${ob.severity} obstruction`))));
        return null;
      });
    }

    function renderSignal() {
      show(P.sig, S.src.rep, (rep) => {
        const sg = rep.signal;
        ui.clear(fitEl);
        if (!sg || !sg.total || !(sg.cells || []).length) return { empty: needPositions('The signal-against-distance picture') };
        const bin = sg.distBinKm || 10;
        const k = units.value('dist', 1);
        const logMax = Math.log10(Math.max(2, sg.maxCount || 1));
        const cells = sg.cells.map((c) => [c[0] * bin * k, c[1], Math.log10(Math.max(1, c[2])) / logMax, c[2]]);
        const xMax = Math.max(...sg.cells.map((c) => (c[0] + 1) * bin)) * k;
        const yMin = Math.min(...sg.cells.map((c) => c[1]));
        const yMax = Math.max(...sg.cells.map((c) => c[1])) + 1;
        const lineOf = (xs, ys) => (xs || []).map((x, i) => [x * k, ys?.[i]]).filter((p) => isNum(p[0]) && isNum(p[1]));
        const fit = sg.fit;
        const series = [{
          type: 'custom', name: 'Positions', data: cells, encode: { x: 0, y: 1 }, z: 1, silent: false,
          renderItem(params, a) {
            const p0 = a.coord([a.value(0), a.value(1) + 1]);
            const p1 = a.coord([a.value(0) + bin * k, a.value(1)]);
            return { type: 'rect', shape: { x: p0[0], y: p0[1], width: Math.max(1, p1[0] - p0[0] - 0.5), height: Math.max(1, p1[1] - p0[1] - 0.5) }, style: { fill: a.visual('color') } };
          },
          tooltip: { formatter: (p) => `${distFmt(p.value[0])}–${distFmt(p.value[0] + bin * k)}<br>${units.dbfs(p.value[1], { digits: 0 })} … ${units.dbfs(p.value[1] + 1, { digits: 0 })}<br><b>${units.int(p.value[3])}</b> positions` },
        }];
        const ln = (name, data, color, dashed, width = 1.8) => series.push({ type: 'line', name, data, color, showSymbol: false, z: 3, lineStyle: { width, type: dashed ? 'dashed' : 'solid' }, tooltip: { show: false } });
        ln('Median', lineOf(sg.distKm, sg.median), '#ffffff', false, 1.4);
        if (fit) {
          ln('Fitted path loss', lineOf(fit.lineKm, fit.measuredDb), C.amber, false, 2.2);
          ln('Free space (20 dB/decade)', lineOf(fit.lineKm, fit.freeSpaceDb), '#ff8fa0', true, 1.6);
        }
        P.sig.chart.set({
          animation: false,
          grid: { left: 8, right: 16, top: 52, bottom: 28, containLabel: true },
          legend: { show: true, top: 0, left: 0, type: 'scroll', data: series.slice(1).map((s) => s.name) },
          tooltip: { trigger: 'item', confine: true },
          xAxis: { type: 'value', min: 0, max: niceMax(xMax), name: `Distance (${units.label('dist')})`, nameLocation: 'middle', nameGap: 24, splitLine: { show: false } },
          yAxis: { type: 'value', min: yMin - 1, max: Math.min(0, yMax + 1), name: 'dBFS', axisLabel: { formatter: (v) => String(v).replace('-', '−') } },
          visualMap: { show: false, type: 'continuous', seriesIndex: 0, dimension: 2, min: 0, max: 1, inRange: { color: charts.ramps.heat } },
          series,
        }, { replace: true });
        P.sig.card.setSub(`${units.si(sg.total)} positions · 1st–99th percentile ${units.dbfs(sg.rssiP01, { unit: false })} … ${units.dbfs(sg.rssiP99)}`);

        const rows = [
          ['Typical level (median)', units.dbfs(sg.rssiP50)],
          ['Strong signals (above −3 dBFS)', isNum(sg.strongPct) ? units.num(sg.strongPct, 1, 1) + ' %' : '—', { tip: 'More than a few percent suggests the receiver is being overdriven - reduce gain or add attenuation' }],
        ];
        if (fit) {
          rows.push(
            ['Measured slope', isNum(fit.slopeDbPerDecade) ? `${units.num(fit.slopeDbPerDecade, 1, 1)} dB / decade` : '—', { tip: 'Loss per tenfold increase of distance; free space is 20' }],
            ['Fit quality (R²)', units.num(fit.r2, 2, 2)],
            ['Fitted over', `${units.dist(fit.minKm)} – ${units.dist(fit.maxKm)}`],
            ['System level at 1 km', units.dbfs(fit.systemOffsetDb), { tip: 'The level a free-space link would give at 1 km. It moves with receiver gain, cable loss and antenna gain - compare it before and after a hardware change.' }]);
        }
        ui.mount(fitEl, h('div.al-fitgrid', ui.kv(rows), h('p.al-text', fit?.text || 'Not enough distance bins with data yet for a path-loss fit (it needs traffic spread over a wide range of distances).')));
        return null;
      });
    }

    function renderAzimuth() {
      show(P.az, S.src.rep, (rep) => {
        const az = rep.azimuth;
        const models = (rep.antennas || []).filter((a) => !S.ant || a.id === S.ant);
        ui.clear(P.az.extra);
        if (!rep.located) return { empty: needLocation };
        if (!az || !az.bearings) {
          return { empty: ['No measured azimuth pattern yet', (rep.notes || [])[0] || 'The pattern needs positions with signal levels spread around the compass. Give it a few hours of traffic.'] };
        }
        const shift = isNum(az.peakDb) ? az.peakDb : 0;
        const meas = az.bearingDeg.map((d, i) => (isNum(az.relDb[i]) ? [az.relDb[i] - shift, d] : null)).filter(Boolean);
        const hasDirectional = models.some((a) => a.info?.directional);
        const floor = hasDirectional ? -24 : Math.min(-6, Math.floor(Math.min(...meas.map((m) => m[0])) - 2));
        const cl = (v) => r1(Math.max(floor, v));
        const series = [{ name: 'Measured', data: meas.map((m) => [cl(m[0]), m[1]]), color: C.cyan, width: 2.2, area: 0.14 }];
        const fit = rep.yagi;
        if (fit && Array.isArray(fit.fitDb) && fit.fitDb.length === az.bearingDeg.length) {
          series.push({ name: fit.ok ? 'Fitted lobe' : 'Fitted lobe (not significant)', data: az.bearingDeg.map((d, i) => [cl((fit.fitDb[i] ?? floor) - shift), d]), color: '#ffffff', width: 1.2 });
        }
        for (const a of models) {
          if (!Array.isArray(a.azRelDb)) continue;
          series.push({ name: `${a.name} (model)`, data: az.bearingDeg.map((d, i) => [cl(a.azRelDb[i] ?? floor), d]), color: a.color || C.amber, width: 1.3, _dashed: true });
        }
        const opt = charts.polar({ series, minR: floor, maxR: 1, legend: true, rFormat: (v) => `${String(v).replace('-', '−')} dB` });
        opt.series.forEach((s, i) => { if (series[i]._dashed) s.lineStyle.type = 'dashed'; });
        opt.radiusAxis.interval = floor <= -18 ? 6 : 2;
        P.az.chart.set(opt, { replace: true });
        P.az.card.setSub(`${units.si(az.samples)} samples · ${az.bearings} bearings · ${az.method === 'heat-residual' ? 'distance-corrected map cells' : az.method || ''}`);
        const cmp = models.length === 1 ? models[0].compare : null;
        ui.mount(P.az.extra, ui.kv([
          ['Strongest direction', `${units.bearing(az.peakBrg)} (${units.db(az.peakDb)})`],
          ['Weakest direction', `${units.bearing(az.minBrg)} (${units.db(az.minDb)})`],
          ['Spread around the compass', units.db(az.spreadDb), { tip: 'Typical variation of the measured gain with bearing. Below about 2 dB is an even, omnidirectional station.' }],
          cmp ? ['Deviation from the model (r.m.s.)', units.db(cmp.rmsDb)] : null,
        ], { columns: 2 }));
        return null;
      });
    }

    function renderElevation() {
      show(P.el, S.src.rep, (rep) => {
        const el = rep.elevation;
        if (!rep.located) return { empty: needLocation };
        if (!el || !el.total) return { empty: needPositions('The elevation pattern') };
        const n = el.elevDeg.length;
        let last = 0;
        const minCount = Math.max(1, el.total * 2e-5);
        for (let i = 0; i < n; i++) if (el.count[i] >= minCount) last = i;
        const end = Math.min(n, Math.max(last + 2, 24));
        const xs = el.elevDeg.slice(0, end);
        const series = [
          { name: 'Measured', data: xs.map((x, i) => [x, isNum(el.relDb[i]) ? r1(el.relDb[i]) : null]), color: C.cyan, width: 2.2 },
        ];
        for (const a of (rep.antennas || []).filter((m) => !S.ant || m.id === S.ant)) {
          if (!Array.isArray(a.elRelDb) || a.elRelDb.length !== n) continue;
          series.push({ name: `${a.name} (model)`, data: xs.map((x, i) => [x, isNum(a.elRelDb[i]) ? r1(a.elRelDb[i]) : null]), color: a.color || C.amber, dashed: true, width: 1.4 });
        }
        series.push({ name: 'Positions', data: xs.map((x, i) => [x, el.count[i]]), color: C.text4, y2: true, area: 0.25, width: 0.8 });
        const lows = series.slice(0, -1).flatMap((s) => s.data.map((d) => d[1])).filter(isNum);
        const opt = charts.line({
          xType: 'value', series, yName: 'dB rel. peak', yMin: Math.max(-30, Math.floor(Math.min(-6, ...lows) - 1)), yMax: 1,
          y2Name: 'positions', xName: 'Elevation above the horizon (°)',
          valueFormat: (v) => (isNum(v) ? units.num(v, Math.abs(v) < 100 && !Number.isInteger(v) ? 1 : 0) : '—'),
        });
        opt.grid.bottom = 26;
        opt.grid.top = 52; // keep the axis names clear of the legend
        opt.xAxis.min = xs[0] - 0.25;
        opt.xAxis.max = xs[xs.length - 1] + 0.25;
        opt.xAxis.axisLabel = { formatter: (v) => v + '°' };
        opt.yAxis[1].axisLabel = { formatter: (v) => units.si(v, 0) };
        opt.series[opt.series.length - 1].z = 0;
        P.el.chart.set(opt, { replace: true });
        P.el.card.setSub(`strongest at ${units.num(el.peakDeg, 1, 1)}° · lowest reception ${units.num(el.lowestDeg, 1, 1)}°`);
        return null;
      });
    }

    function needle(deg, color, len, dashed) {
      const a = (deg - 90) * Math.PI / 180;
      return `<line x1="60" y1="60" x2="${(60 + Math.cos(a) * len).toFixed(1)}" y2="${(60 + Math.sin(a) * len).toFixed(1)}" stroke="${color}" stroke-width="2.4" stroke-linecap="round"${dashed ? ' stroke-dasharray="4 4"' : ''}/>`;
    }
    function wedge(deg, width, color) {
      const pt = (d) => { const a = (d - 90) * Math.PI / 180; return `${(60 + Math.cos(a) * 46).toFixed(1)} ${(60 + Math.sin(a) * 46).toFixed(1)}`; };
      return `<path d="M60 60 L${pt(deg - width / 2)} A46 46 0 ${width > 180 ? 1 : 0} 1 ${pt(deg + width / 2)} Z" fill="${color}" fill-opacity="0.16" stroke="${color}" stroke-opacity="0.5" stroke-width="1"/>`;
    }

    function renderYagi() {
      show(P.yagi, S.src.rep, (rep) => {
        const y = rep.yagi;
        const hasYagiCfg = antennas().some((a) => a.type === 'yagi' || a.type === 'patch');
        if (!y) {
          return { empty: ['No directional antenna to look for', 'Add a yagi (or patch) antenna in Settings and AirView will try to find its beam in the reception statistics.', 'antenna'] };
        }
        const cfg = y.configured;
        const conf = y.confidenceLabel || 'low';
        const confKind = conf === 'high' ? 'green' : conf === 'medium' ? 'amber' : 'other';
        const signed = (v, pos, neg, unit) => (isNum(v) ? `${units.num(Math.abs(v), 1)}${unit} ${v >= 0 ? pos : neg}` : '—');
        const col = antennas().find((a) => a.id === cfg?.antennaId)?.color || '#f59e0b';
        const svg = `<svg viewBox="0 0 120 120" class="al-compass" aria-hidden="true">
          <circle cx="60" cy="60" r="50" fill="none" stroke="rgba(120,165,255,0.3)"/><circle cx="60" cy="60" r="2.5" fill="#fff"/>
          ${[0, 90, 180, 270].map((d) => { const a = (d - 90) * Math.PI / 180; return `<text x="${60 + Math.cos(a) * 56}" y="${60 + Math.sin(a) * 56 + 3}" text-anchor="middle" font-size="8" fill="#7388ad">${'NESW'[d / 90]}</text>`; }).join('')}
          ${cfg && cfg.hBeamwidthDeg > 0 ? wedge(cfg.azimuthDeg, cfg.hBeamwidthDeg, col) : ''}
          ${cfg ? needle(cfg.azimuthDeg, col, 46, true) : ''}
          ${isNum(y.azimuthDeg) ? needle(y.azimuthDeg, y.ok ? '#ffffff' : '#7388ad', 40, false) : ''}
        </svg>`;
        const tbl = h('table.table.al-yagitable',
          h('thead', h('tr', h('th', ''), h('th.right', 'Inferred from data'), h('th.right', cfg ? `Configured (${cfg.name})` : 'Configured'), h('th.right', 'Difference'))),
          h('tbody',
            h('tr', h('td', 'Boresight azimuth'), h('td.num.right', isNum(y.azimuthDeg) ? units.bearing(y.azimuthDeg) : '—'), h('td.num.right', cfg ? units.bearing(cfg.azimuthDeg) : '—'),
              h('td.num.right', { tip: 'Pointing error: measured minus configured' }, signed(y.pointingErrorDeg, 'clockwise', 'anticlockwise', '°'))),
            h('tr', h('td', 'Beamwidth (−3 dB)'), h('td.num.right', isNum(y.beamwidthDeg) ? units.num(y.beamwidthDeg, 0) + '°' : '—'), h('td.num.right', cfg && cfg.hBeamwidthDeg ? units.num(cfg.hBeamwidthDeg, 0) + '°' : '—'),
              h('td.num.right', signed(y.beamwidthErrorDeg, 'wider', 'narrower', '°'))),
            h('tr', h('td', 'Gain advantage over the omni'), h('td.num.right', units.db(y.gainOverOmniDb)), h('td.num.right', cfg ? units.db(cfg.gainOverOmniDb) : '—'),
              h('td.num.right', signed(y.gainErrorDb, 'more', 'less', ' dB')))));
        ui.mount(P.yagi.main,
          h('div.al-yagi-top',
            h('div.al-yagi-dial', { html: svg }, ui.legend([{ label: 'configured', color: col }, { label: 'inferred', color: y.ok ? '#ffffff' : '#7388ad' }])),
            h('div.al-yagi-main',
              h('div.row.al-yagi-status',
                ui.badge(y.ok ? 'Beam found' : 'No clear beam', y.ok ? 'green' : 'other', { large: true }),
                ui.badge(`confidence: ${conf}${isNum(y.confidence) ? ' (' + units.num(y.confidence * 100, 0) + ' %)' : ''}`, confKind, { tip: 'How much of the variation around the compass the lobe explains, how far it stands above the omni level and how complete the data is' }),
                isNum(y.explainedPct) ? h('span.muted', `explains ${units.num(y.explainedPct, 0)} % of the azimuth variation · residual ${units.db(y.rmsDb, 2)} r.m.s.`) : null),
              h('p.al-text', y.text || ''),
              !y.ok && y.reason ? h('p.al-text.muted', `Why: ${y.reason}.`) : null,
              h('div.table-wrap', tbl))),
          h('div.al-honest', ui.icon('info'), h('div',
            h('b', 'How far to trust this. '),
            'A Radarcape 2CH decodes both antenna inputs, but its data stream does not say which antenna heard a frame. AirView therefore cannot measure the yagi on its own: it looks at the combined signal statistics around the compass and fits "an even omni level plus one directional lobe" to them. ',
            'The result is an inference, not a measurement - it is only as good as the traffic is evenly spread, and a lobe that barely rises above the omni (low gain advantage, low confidence) may just be a busy airway. ',
            'For a real per-antenna measurement, feed each antenna through its own receiver (one feed per antenna, attributed in Settings) and select it at the top of this page.',
            hasYagiCfg ? '' : ' No yagi or patch antenna is configured at the moment, so there is nothing to compare the lobe with.')));
        return null;
      });
    }

    function renderLink() {
      show(P.link, S.src.rep, (rep) => {
        const rows = rep.linkBudget || [];
        if (!rows.length) return { empty: ['No link budget available', 'The prediction needs the receiver location and at least one configured antenna.', 'ruler'] };
        const km = (v) => units.format('dist', v, { digits: 0, unit: false });
        const tbl = ui.table({
          columns: [
            { key: 'band', label: 'Altitude band', render: (r) => bandLabel(r.band) },
            { key: 'horizonKm', label: `Horizon (${units.label('dist')})`, align: 'right', num: true, render: (r) => km(r.horizonKm), tip: 'Smooth-earth line of sight at the typical altitude of the band' },
            { key: 'predictedKm', label: `Predicted (${units.label('dist')})`, align: 'right', num: true, render: (r) => km(r.predictedKm) },
            { key: 'limit', label: 'Limited by', render: (r) => ui.badge(r.limit === 'power' ? 'signal power' : r.limit === 'horizon' ? 'horizon' : '—', r.limit === 'power' ? 'amber' : 'blue'), tip: 'What ends the range first: the earth\'s curvature or the signal becoming too weak' },
            { key: 'marginDb', label: 'Margin', align: 'right', num: true, render: (r) => units.db(r.marginDb), tip: 'Signal reserve above the decoding threshold at the predicted range (250 W transponder)' },
            { key: 'antenna', label: 'Best antenna', render: (r) => antName(r.antenna) },
            { key: 'achievedMaxKm', label: `Max (${units.label('dist')})`, align: 'right', num: true, render: (r) => km(r.achievedMaxKm), tip: 'Farthest single position' },
            { key: 'achievedRobustKm', label: `Reliable (${units.label('dist')})`, align: 'right', num: true, render: (r) => km(r.achievedRobustKm), tip: 'Best bearing of the robust envelope' },
            { key: 'achievedTypicalKm', label: `Typical (${units.label('dist')})`, align: 'right', num: true, render: (r) => km(r.achievedTypicalKm), tip: 'Median over all bearings' },
            { key: 'pctOfPredicted', label: 'Achieved', align: 'right', render: (r) => {
              if (!isNum(r.pctOfPredicted)) return h('span.muted', 'no data');
              const m = ui.meter(r.pctOfPredicted);
              return h('div.al-pct', h('span.num', units.pct(r.pctOfPredicted, 0)), m);
            } },
          ],
          rows,
        });
        ui.mount(P.link.main, tbl);
        return null;
      });
      renderWhatIf();
    }

    // ---- what-if calculator ----------------------------------------------------------------------
    function buildWhatIf() {
      const list = antennas();
      const groundM = () => (isNum(S.src.hor.data?.groundElevM) ? S.src.hor.data.groundElevM : config.current?.receiver?.groundElevM || 0);
      const w = { base: list[0] || null };
      const fromAnt = (a) => ({
        heightAglM: a ? a.heightAglM : 8, gainDbi: a ? a.gainDbi : 5, lossDb: a ? (a.cableLossDb || 0) + (a.filterLossDb || 0) : 0,
        lna: a ? a.lnaGainDb > 0 : false, lnaGainDb: a && a.lnaGainDb > 0 ? a.lnaGainDb : 20, lnaNfDb: a && a.lnaNoiseFigDb > 0 ? a.lnaNoiseFigDb : 1,
      });
      let v = fromAnt(w.base);
      let altFt = 35000;
      let txDbm = null; // null = model default (250 W)

      const out = h('div.al-wi-out');
      const chartEl = h('div.chart', { style: { '--h': '250px' } });
      let chart = null;
      const sl = {
        height: ui.slider({ min: 0, max: 60, step: 0.5, value: v.heightAglM, format: (x) => units.height(x, { digits: units.system === 'imperial' ? 0 : 1 }), onInput: (x) => { v.heightAglM = x; calc(); } }),
        gain: ui.slider({ min: 0, max: 20, step: 0.5, value: v.gainDbi, format: (x) => `${units.num(x, 1, 1)} dBi`, onInput: (x) => { v.gainDbi = x; calc(); } }),
        loss: ui.slider({ min: 0, max: 15, step: 0.5, value: v.lossDb, format: (x) => units.db(x), onInput: (x) => { v.lossDb = x; calc(); } }),
        lnaGain: ui.slider({ min: 5, max: 40, step: 1, value: v.lnaGainDb, format: (x) => units.db(x, 0), onInput: (x) => { v.lnaGainDb = x; calc(); } }),
        lnaNf: ui.slider({ min: 0.3, max: 5, step: 0.1, value: v.lnaNfDb, format: (x) => units.db(x), onInput: (x) => { v.lnaNfDb = x; calc(); } }),
        alt: ui.slider({ min: 500, max: 45000, step: 500, value: altFt, format: (x) => units.alt(x, { fl: true }), onInput: (x) => { altFt = x; calc(); } }),
      };
      const lnaToggle = ui.toggle({ label: 'Preamplifier (LNA) at the antenna', checked: v.lna, onChange: (on) => { v.lna = on; syncLna(); calc(); } });
      const lnaFields = h('div.al-wi-lna',
        ui.field({ label: 'LNA gain', control: sl.lnaGain }),
        ui.field({ label: 'LNA noise figure', control: sl.lnaNf, hint: 'Good 1090 MHz preamps: 0.5 – 1.5 dB' }));
      const syncLna = () => lnaFields.classList.toggle('hidden', !v.lna);
      const baseSel = ui.select({
        size: 'sm', options: list.length ? list.map((a) => ({ value: a.id, label: a.name || a.id })) : [{ value: '', label: 'Generic antenna' }],
        value: w.base?.id, onChange: (id) => { w.base = antennas().find((a) => a.id === id) || null; reset(); loadCalibration(); },
      });
      const txSel = ui.select({ size: 'sm', options: [{ value: '', label: '250 W (airliner)' }], onChange: (val) => { txDbm = val === '' ? null : Number(val); calc(); } });
      const resetBtn = ui.button({ label: 'Reset', icon: 'refresh', size: 'sm', variant: 'ghost', onClick: () => reset(), tip: 'Back to the configured values' });

      function reset() {
        v = fromAnt(w.base);
        sl.height.value = v.heightAglM; sl.gain.value = v.gainDbi; sl.loss.value = v.lossDb; sl.lnaGain.value = v.lnaGainDb; sl.lnaNf.value = v.lnaNfDb;
        lnaToggle.checked = v.lna;
        syncLna();
        calc();
      }
      const setupOf = (x) => ({
        antennaAmslM: groundM() + x.heightAglM, gainDbi: x.gainDbi, cableLossDb: x.lossDb, filterLossDb: 0,
        lnaGainDb: x.lna ? x.lnaGainDb : 0, lnaNoiseFigDb: x.lnaNfDb, txPowerDbm: txDbm ?? S.model.txPowerDbm,
      });
      function calc() {
        const base = fromAnt(w.base);
        const now = predict(setupOf(v), altFt, S.model);
        const ref = predict(setupOf(base), altFt, S.model);
        const dKm = now.rangeKm - ref.rangeKm;
        const dMargin = now.marginAtHorizonDb - ref.marginAtHorizonDb;
        const delta = (val, fmt) => h('span.al-delta.' + (Math.abs(val) < 0.05 ? 'same' : val > 0 ? 'up' : 'down'), Math.abs(val) < 0.05 ? 'unchanged' : `${val > 0 ? '+' : '−'}${fmt(Math.abs(val))}`);
        const verdict = now.limit === 'horizon'
          ? (now.marginAtHorizonDb >= 10 ? 'Horizon-limited with a comfortable reserve: more gain will not add range here - only more height (or a clearer view) will.'
            : 'Horizon-limited, but with little reserve: weak or badly oriented transponders near the horizon will drop out. A few dB more (gain, LNA, shorter cable) makes the edge of coverage more solid.')
          : 'Power-limited: the signal becomes too weak before the horizon is reached. More antenna gain, less cable loss or a preamplifier at the antenna will extend the range.';
        ui.mount(out,
          h('div.al-wi-big',
            h('div.label', `Predicted range at ${units.alt(altFt, { fl: true })}`),
            h('div.al-wi-value', h('span.num', units.format('dist', now.rangeKm, { unit: false, digits: 0 })), h('span.unit', units.label('dist')),
              ui.badge(now.limit === 'power' ? 'limited by signal power' : 'limited by the horizon', now.limit === 'power' ? 'amber' : 'blue')),
            h('div.al-wi-delta', 'against the configured antenna: ', delta(dKm, (x) => units.format('dist', x, { digits: 0 })))),
          ui.kv([
            ['Radio horizon', units.dist(now.horizonKm)],
            ['Reserve at the horizon', h('span', units.db(now.marginAtHorizonDb), ' ', delta(dMargin, (x) => units.db(x))), { tip: 'Signal margin above the decoding threshold for an aircraft right at the horizon. Negative = out of reach.' }],
            ['Sensitivity (at the antenna)', `${units.num(now.noise.sensitivityDbm, 1, 1)} dBm`],
            ['System noise figure', units.db(now.noise.noiseFigDb), { tip: 'Cable loss in front of the first amplifier adds directly to the noise figure; an LNA at the antenna hides it.' }],
            ['Antenna above sea level', units.height(groundM() + v.heightAglM, { digits: 0 })],
          ]),
          h('p.al-text', verdict));

        const rows = S.src.rep.data?.linkBudget || [];
        const bands = rows.length ? rows : BAND_EDGES_FT.slice(0, 7).map((lo, i) => ({ band: i, altFt: i === 0 ? 500 : (lo + (BAND_EDGES_FT[i + 1] ?? 46000)) / 2 }));
        const x = bands.map((b) => bandLabel(b.band));
        const series = [
          { name: 'What-if', data: bands.map((b) => dist(predict(setupOf(v), b.altFt, S.model).rangeKm)), color: C.cyan },
          { name: `Configured (${w.base?.name || 'default'})`, data: bands.map((b) => dist(predict(setupOf(base), b.altFt, S.model).rangeKm)), color: C.blue },
        ];
        if (rows.some((r) => isNum(r.achievedRobustKm))) series.push({ name: 'Achieved (reliable)', data: rows.map((r) => dist(r.achievedRobustKm)), color: C.green });
        if (!chart) chart = charts.create(chartEl);
        const opt = charts.bar({ x, series, yName: units.label('dist'), valueFormat: (val) => (isNum(val) ? distFmt(val) : '—') });
        opt.xAxis.axisLabel = { interval: 0, fontSize: 9.5, hideOverlap: true };
        opt.grid.top = 52;
        chart.set(opt, { replace: true });
      }

      ui.mount(whatIfBody,
        h('div.al-wi-controls',
          h('div.al-wi-row',
            ui.field({ label: 'Start from', control: baseSel }),
            ui.field({ label: 'Transponder power', control: txSel }),
            h('div.al-wi-reset', resetBtn)),
          ui.field({ label: 'Antenna height above ground', control: sl.height, hint: 'Height is what moves the radio horizon' }),
          ui.field({ label: 'Antenna gain', control: sl.gain, hint: 'Typical: 2 dBi quarter-wave, 5 – 8 dBi collinear, 10 – 14 dBi yagi' }),
          ui.field({ label: 'Cable + filter loss before the first amplifier', control: sl.loss, hint: 'About 0.2 dB per metre of thin coax at 1090 MHz' }),
          lnaToggle, lnaFields,
          ui.field({ label: 'Aircraft altitude', control: sl.alt })),
        h('div.al-wi-result', out, chartEl));
      syncLna();

      async function loadCalibration() {
        try {
          const params = w.base ? { ant: w.base.id, km: 250, altFt: 35000 } : { km: 250, altFt: 35000 };
          const reply = await api.get('/api/antenna/budget', { params, signal: ctx.signal });
          if (!S.alive) return;
          S.model = calibrate(reply, w.base);
        } catch (e) { if (!e.aborted) console.warn('[antenna] link model calibration unavailable, using defaults', e.message); }
        if (S.alive) calc();
      }
      async function loadClasses() {
        try {
          const reply = await api.get('/api/radio/transponders', { signal: ctx.signal });
          if (!S.alive || !Array.isArray(reply?.classes)) return;
          txSel.setOptions(reply.classes.map((c) => ({ value: Math.abs(c.powerW - 250) < 1 ? '' : String(c.powerDbm), label: `${c.name}${c.powerW === 70 ? ' (light aircraft)' : c.powerW === 250 ? ' (airliner)' : ''}` })));
          txSel.value = '';
        } catch (e) { if (!e.aborted) console.warn('[antenna] transponder classes unavailable', e.message); }
      }
      loadCalibration();
      loadClasses();
      return { calc, refreshUnits() { for (const s of Object.values(sl)) s.refresh(); calc(); } };
    }
    function renderWhatIf() {
      if (!S.whatIf) S.whatIf = buildWhatIf();
      else S.whatIf.calc();
      P.whatif.ready();
    }

    // ---- day / night -----------------------------------------------------------------------------
    function renderDayNight() {
      show(P.dn, S.src.dn, (dn) => {
        ui.mount(dnText, (dn.conclusions || []).map((t) => h('div.al-finding.info', ui.icon('info'), h('div', t))));
        const byHour = dn.byHour || [];
        if (!dn.hours || dn.hours < 6 || !byHour.some((x) => isNum(x.p95Km))) {
          ui.clear(dnText);
          return { empty: ['Not enough hours yet', (dn.conclusions || [])[0] || 'The day/night comparison needs at least two full days of range statistics.', 'moon'] };
        }
        const PH = { day: ['Day', 'sun', 'amber'], twilight: ['Twilight', 'twilight', 'violet'], night: ['Night', 'moon', null] };
        ui.mount(dnTiles, (dn.phases || []).map((p) => {
          const meta = PH[p.phase] || [p.phase, 'sun', null];
          const t = ui.stat({ label: meta[0], icon: meta[1], accent: meta[2], tip: 'Far range (95th percentile of the hourly values), traffic-normalised' });
          t.set(units.format('dist', p.p95NormKm ?? p.p95Km, { unit: false, digits: 0 }), units.label('dist'));
          t.setSub(p.hours ? `raw ${units.dist(p.p95Km)} · max ${units.dist(p.maxKm)} · ${units.num(p.aircraft, 0)} aircraft/h · ${p.hours} h` : 'no hours in this phase' + (dn.located ? '' : ' (needs the receiver location)'));
          return t;
        }));
        const x = byHour.map((b) => String(b.hour).padStart(2, '0'));
        if (!dnChartH) dnChartH = charts.create(dnChart);
        const opt = charts.line({
          x, yName: units.label('dist'), y2Name: 'aircraft / h', yScale: true,
          series: [
            { name: 'Far range (p95)', data: byHour.map((b) => dist(b.p95Km)), color: C.blue, width: 2, smooth: true },
            { name: 'Traffic-normalised', data: byHour.map((b) => dist(b.p95NormKm)), color: C.cyan, dashed: true, smooth: true },
            { name: 'Aircraft per hour', data: byHour.map((b) => (isNum(b.aircraft) ? r1(b.aircraft) : null)), color: C.text4, y2: true, area: 0.25, width: 0.8 },
          ],
        });
        opt.xAxis.name = `Hour of day (${dn.utcOffsetSec ? 'UTC' + (dn.utcOffsetSec > 0 ? '+' : '−') + units.num(Math.abs(dn.utcOffsetSec) / 3600, 1) : 'UTC'})`;
        opt.grid.bottom = 26;
        opt.grid.top = 52;
        opt.series[2].z = 0;
        dnChartH.set(opt, { replace: true });
        const c = dn.nightVsDay;
        P.dn.card.setSub(`${dn.hours} hour${dn.hours === 1 ? '' : 's'} analysed` + (c && isNum(c.diffKm) ? ` · night ${c.diffKm >= 0 ? '+' : '−'}${units.dist(Math.abs(c.diffKm))} against day (${c.significant ? 'significant' : 'not significant'}, p = ${units.num(c.p, 2, 2)})` : ''));
        return null;
      });
    }

    // ---- mini map --------------------------------------------------------------------------------
    function heatGeoJson(heat) {
      const half = (heat.cellDeg || 0.05) / 2;
      const logMax = Math.log10(Math.max(2, heat.maxCount || 1));
      const features = [];
      for (let i = 0; i < heat.lat.length; i++) {
        const la = heat.lat[i];
        const lo = heat.lon[i];
        features.push({
          type: 'Feature',
          properties: { d: Math.log10(Math.max(1, heat.count[i])) / logMax, n: heat.count[i], rssi: heat.rssi[i] ?? -99, lo: heat.minAlt[i] ?? -1, hi: heat.maxAlt[i] ?? -1 },
          geometry: { type: 'Polygon', coordinates: [[[lo - half, la - half], [lo + half, la - half], [lo + half, la + half], [lo - half, la + half], [lo - half, la - half]]] },
        });
      }
      return { type: 'FeatureCollection', features };
    }
    function rssiRange(heat) {
      const vals = heat.rssi.filter(isNum).sort((a, b) => a - b);
      if (!vals.length) return [-40, 0];
      const lo = Math.floor(vals[Math.floor(vals.length * 0.02)]);
      const hi = Math.ceil(vals[Math.floor(vals.length * 0.98)]);
      return [lo, hi > lo ? hi : lo + 1];
    }
    function rampExpr(prop, stops) {
      return ['interpolate', ['linear'], ['get', prop], ...stops.flatMap(([v, c]) => [v, c])];
    }
    function paintMap() {
      const heat = S.src.heat.data;
      const map = S.map;
      if (!heat || !map || !map.getLayer('al-heat')) return;
      const mode = S.mapMode;
      let stops;
      let legend;
      if (mode === 'rssi') {
        const [lo, hi] = rssiRange(heat);
        const ramp = charts.ramps.signal;
        stops = ramp.map((c, i) => [lo + ((hi - lo) * i) / (ramp.length - 1), c]);
        legend = { colors: ramp, min: units.dbfs(lo, { digits: 0 }), max: units.dbfs(hi, { digits: 0 }), title: 'Mean signal level' };
        map.setPaintProperty('al-heat', 'fill-color', rampExpr('rssi', stops));
        map.setFilter('al-heat', ['>', ['get', 'rssi'], -90]);
      } else if (mode === 'floor') {
        legend = { colors: FLOOR_RAMP.map((s) => s[1]), min: units.alt(0), max: units.alt(40000) + '+', title: 'Lowest altitude received' };
        map.setPaintProperty('al-heat', 'fill-color', rampExpr('lo', FLOOR_RAMP));
        map.setFilter('al-heat', ['>=', ['get', 'lo'], 0]);
      } else {
        const ramp = charts.ramps.heat.slice(1);
        stops = ramp.map((c, i) => [i / (ramp.length - 1), c]);
        legend = { colors: ramp, min: '1', max: units.si(heat.maxCount), title: 'Positions per cell (log scale)' };
        map.setPaintProperty('al-heat', 'fill-color', rampExpr('d', stops));
        map.setFilter('al-heat', null);
      }
      ui.mount(mapLegend, h('span.al-leg-title', legend.title), h('span.num', legend.min),
        h('span.al-leg-bar', { style: { background: `linear-gradient(90deg, ${legend.colors.join(',')})` } }), h('span.num', legend.max),
        h('span.muted', `· ${units.int(heat.cells)} cells of ${units.num(heat.cellDeg, 2)}° · ${units.si(heat.positions)} positions`));
    }
    async function ensureMap(heat) {
      const my = ++S.mapSeq;
      const st = ctx.status?.station;
      const rx = S.src.cov.data?.receiver || (st && st.source !== 'none' && isNum(st.lat) && isNum(st.lon) ? st : null) || (config.current?.receiver?.located ? config.current.receiver : null);
      const data = heatGeoJson(heat);
      let w = Infinity; let s = Infinity; let e = -Infinity; let n = -Infinity;
      for (let i = 0; i < heat.lat.length; i++) { w = Math.min(w, heat.lon[i]); e = Math.max(e, heat.lon[i]); s = Math.min(s, heat.lat[i]); n = Math.max(n, heat.lat[i]); }
      const fit = () => { if (Number.isFinite(w)) S.map.fitBounds([[w, s], [e, n]], { padding: 24, duration: 0, maxZoom: 9 }); };
      if (S.map) {
        if (S.map.getSource('al-heat')) { S.map.getSource('al-heat').setData(data); paintMap(); }
        else S.map.once('load', () => ensureMap(heat));
        return;
      }
      const [maplibregl] = await Promise.all([
        import('../../vendor/maplibre/maplibre-gl.mjs'),
        ui.loadCss(new URL('../../vendor/maplibre/maplibre-gl.css', import.meta.url)),
      ]);
      if (!S.alive || my !== S.mapSeq || S.map) return;
      const basemap = basemapById(config.current?.ui?.mapStyle || 'airview-dark');
      const map = new maplibregl.Map({
        container: mapEl, style: styleFor(basemap), center: rx ? [rx.lon, rx.lat] : [8, 48], zoom: 4.5, minZoom: 1.5, maxZoom: 12,
        dragRotate: false, pitchWithRotate: false, touchPitch: false, attributionControl: false, fadeDuration: 0, cooperativeGestures: true,
        transformRequest: makeTransformRequest(() => !!ctx.live.status?.tileCache),
      });
      S.map = map;
      map.touchZoomRotate.disableRotation();
      map.addControl(new maplibregl.AttributionControl({ compact: true }), 'bottom-right');
      map.addControl(new maplibregl.NavigationControl({ showCompass: false }), 'top-right');
      let warned = 0;
      map.on('error', (ev) => { if (warned++ < 2) console.warn('[antenna map]', ev?.error?.message || ev); });
      map.on('load', () => {
        if (!S.alive) return;
        map.addSource('al-heat', { type: 'geojson', data: heatGeoJson(S.src.heat.data || heat), tolerance: 0 });
        map.addLayer({ id: 'al-heat', type: 'fill', source: 'al-heat', paint: { 'fill-color': '#22d3ee', 'fill-opacity': 0.72, 'fill-antialias': false } });
        if (rx) {
          map.addSource('al-rx', { type: 'geojson', data: { type: 'Feature', properties: {}, geometry: { type: 'Point', coordinates: [rx.lon, rx.lat] } } });
          map.addLayer({ id: 'al-rx-glow', type: 'circle', source: 'al-rx', paint: { 'circle-radius': 14, 'circle-color': '#2f7dff', 'circle-opacity': 0.35, 'circle-blur': 0.7 } });
          map.addLayer({ id: 'al-rx', type: 'circle', source: 'al-rx', paint: { 'circle-radius': 4, 'circle-color': '#ffffff', 'circle-stroke-color': '#2f7dff', 'circle-stroke-width': 2.5 } });
        }
        fit();
        paintMap();
      });
      map.on('mousemove', 'al-heat', ui.rafThrottle((ev) => {
        const f = ev.features && ev.features[0];
        if (!f || !S.alive) return;
        const p = f.properties;
        ui.mount(mapTip,
          h('b', `${units.int(p.n)} positions`),
          h('span', p.rssi > -90 ? `mean ${units.dbfs(p.rssi)}` : 'no signal level'),
          h('span', p.lo >= 0 ? `altitudes ${units.format('alt', p.lo, { unit: false })} – ${units.alt(p.hi)}` : ''));
        mapTip.style.left = Math.min(ev.point.x + 14, mapEl.clientWidth - 190) + 'px';
        mapTip.style.top = Math.max(4, ev.point.y - 10) + 'px';
        mapTip.classList.remove('hidden');
      }));
      map.on('mouseleave', 'al-heat', () => mapTip.classList.add('hidden'));
    }
    function renderMap() {
      show(P.map, S.src.heat, (heat) => {
        if (!heat.cells || !heat.lat?.length) {
          ui.clear(mapLegend);
          return { empty: ['No positions on the map yet', 'The heat-map fills in as positions are received and written to the statistics (every few minutes). It covers all time, not just the selected period.', 'map'] };
        }
        ensureMap(heat).catch((e) => { console.error('[antenna] mini-map failed', e); P.map.error(new Error('The map could not be started (' + (e?.message || 'WebGL unavailable') + ').')); });
        P.map.card.setSub(`All-time${S.ant ? ' · ' + antName(S.ant) : ''}`);
        return null;
      });
    }

    function renderNotes() {
      const notes = S.src.rep.data?.notes || [];
      ui.mount(notesEl, notes.length ? [ui.section('Analysis notes', 'info'), notes.map((t) => h('p.al-text.muted', t))] : null);
    }

    const RENDER = {
      cov: () => { renderHead(); renderCoverage(); renderPct(); renderSkyline(); if (terrain) terrain.measuredChanged(); },
      rep: () => { buildAntSelector(); renderSignal(); renderAzimuth(); renderElevation(); renderYagi(); renderLink(); renderNotes(); if (S.src.cov.data) renderCoverage(); },
      score: renderScore,
      dn: renderDayNight,
      heat: renderMap,
      hor: () => { if (S.whatIf) S.whatIf.calc(); },
    };
    const renderAll = () => { for (const fn of Object.values(RENDER)) fn(); };

    // ---- data ------------------------------------------------------------------------------------
    function refresh(o = {}) {
      const quiet = !!o.quiet;
      const my = ++S.seq;
      const range = { days: S.days, ...(S.ant ? { ant: S.ant } : {}) };
      const antOnly = S.ant ? { ant: S.ant } : {};
      const jobs = {
        cov: ['/api/antenna/coverage', range], rep: ['/api/antenna/report', range], score: ['/api/antenna/score', range],
        dn: ['/api/antenna/daynight', range], heat: ['/api/antenna/heat', { ...antOnly, maxCells: 6000 }], hor: ['/api/antenna/horizon', antOnly],
      };
      if (!quiet) { for (const k of Object.keys(jobs)) S.src[k] = {}; renderAll(); refreshBtn.setLoading(true); }
      const all = Object.entries(jobs).map(([k, [path, params]]) =>
        api.get(path, { params, signal: ctx.signal, timeout: API_TIMEOUT }).then((data) => {
          if (my !== S.seq || !S.alive) return;
          S.src[k] = { data };
          RENDER[k]();
        }).catch((error) => {
          if (error?.aborted || my !== S.seq || !S.alive) return;
          if (quiet && S.src[k].data) return; // keep showing the last good analysis
          S.src[k] = { error };
          RENDER[k]();
        }));
      Promise.all(all).finally(() => { if (my === S.seq && S.alive) refreshBtn.setLoading(false); });
    }

    async function showOnMap() {
      mapBtn.setLoading(true);
      try {
        const params = { days: S.days, ...(S.ant ? { ant: S.ant } : {}), ...(S.band >= 0 ? { band: S.band } : {}) };
        const fc = await api.get('/api/antenna/coverage.geojson', { params, signal: ctx.signal, timeout: API_TIMEOUT });
        let feats = (fc?.features || []).filter((f) => f.geometry && (S.band >= 0 ? f.properties?.band === S.band : f.properties?.band === -1));
        if (!feats.length) feats = fc?.features || [];
        if (!feats.length) { ui.toast.warn('There is no coverage outline to show yet for this selection.'); return; }
        const col = S.ant ? antennas().find((a) => a.id === S.ant)?.color : null;
        const features = feats.map((f) => ({
          ...f,
          properties: { ...f.properties, color: f.properties?.kind === 'robust' ? (col || '#3b8bff') : '#22d3ee', fillOpacity: f.properties?.kind === 'robust' ? 0.14 : 0.04 },
        }));
        ctx.shared.set('map.coverage', { type: 'FeatureCollection', features });
        ui.toast.success(`Coverage (${bandLabel(S.band)}${S.ant ? ', ' + antName(S.ant) : ''}) is now on the Live Map. Toggle it under Layers.`);
        ctx.navigate('map');
      } catch (e) {
        if (!e.aborted) ui.toast.error(e.message || String(e), { title: 'Coverage outline unavailable' });
      } finally {
        mapBtn.setLoading(false);
      }
    }

    // ---- wiring ----------------------------------------------------------------------------------
    buildAntSelector();
    refresh();
    ctx.every(REFRESH_MS, () => { if (document.visibilityState === 'visible') refresh({ quiet: true }); });
    ctx.subscribe('status', () => { buildAntSelector(); if (!S.src.cov.data) renderHead(); }, { immediate: false });
    ctx.onCleanup(units.onChange(() => { renderAll(); if (S.whatIf) S.whatIf.refreshUnits(); paintMap(); if (terrain) terrain.unitsChanged(); }));
    ctx.onCleanup(config.subscribe(() => { antSeg.dataset.key = ''; buildAntSelector(); }));
  },

  onRoute(route) {
    if (active?.selectTab) active.selectTab(route.args[0] === 'terrain' ? 'terrain' : 'station');
  },
};

let active = null; // state of the mounted instance (for onRoute)

/** Round a radius / axis maximum up to a pleasant number. */
function niceMax(v) {
  if (!(v > 0)) return 10;
  const p = 10 ** Math.floor(Math.log10(v));
  const m = v / p;
  const step = m <= 1 ? 1 : m <= 1.5 ? 1.5 : m <= 2 ? 2 : m <= 2.5 ? 2.5 : m <= 3 ? 3 : m <= 4 ? 4 : m <= 5 ? 5 : m <= 6 ? 6 : m <= 8 ? 8 : 10;
  return step * p;
}
