// antenna/terrain.js - "Terrain & line of sight" tab of the Antenna Lab (pages/antenna.js).
//
// Data:
//   /api/terrain/status     job / download progress (1 s while running, else every 20 s while the tab is visible)
//   /api/terrain/coverage   full analysis (horizon panorama, LOS + radio rings per altitude, site advice), once per key
//   /api/terrain/los        line of sight to one point with the terrain profile (profile tool, debounced); a k-factor
//                           other than the station's is sent as ?k= (server-exact); the browser estimate (los.js)
//                           only bridges the request or stands in when the server ignores ?k=
//   /api/terrain/advice?ant= site advice of the selected antenna when it is not the one in the coverage reply
//   POST /api/terrain/prefetch | recompute | cancel   (admin; a 401 asks for the password and retries)
//   /api/rf/summary         RF chain & noise headline (every 30 s), links to #/rf
// The measured side (skyline, robust range per altitude band) is the page's /api/antenna/coverage reply,
// handed in through opts.measured() so the period / antenna selectors at the top of the page apply.

import { basemapById, styleFor, makeTransformRequest } from '../map/basemaps.js';
import { createAdmin } from '../admin.js';
import { itemText } from '../rf/advice.js';
import { evaluate, decimate, horizonKm, effectiveRadiusKm, bulgeM } from './los.js';

const STATUS_IDLE_MS = 20000;
const MIN_BIN_POSITIONS = 20; // measured skyline bins with fewer positions are drawn but not judged
const RF_MS = 30000;
const API_TIMEOUT = 60000;
const LOS_TIMEOUT = 75000; // the server may download the path tiles first (up to 60 s)
const ALT_COLORS = ['#22d3ee', '#34d399', '#a3e635', '#fde047', '#fb923c', '#f87171', '#e879f9', '#c4b5fd'];
const K_PRESETS = [
  { value: '0.67', k: 2 / 3, label: '2/3', tip: 'Sub-refraction: the beam bends less than usual or even upwards (fog, warm air over cold sea). Rare, shortens the range.' },
  { value: '1', k: 1, label: '1', tip: 'No refraction: the geometric line of sight of light in vacuum.' },
  { value: '1.33', k: 4 / 3, label: '4/3', tip: 'Standard atmosphere: the beam follows the earth a little. The textbook value for VHF/UHF radio.' },
  { value: '2', k: 2, label: '2', tip: 'Super-refraction: strong bending (temperature inversion, moist air near the ground). Extends the range.' },
  { value: '4', k: 4, label: '4', tip: 'Close to ducting: the beam almost follows the curvature of the earth; aircraft far beyond the normal horizon become audible.' },
];

const isNum = (v) => typeof v === 'number' && Number.isFinite(v);
const norm360 = (d) => ((d % 360) + 360) % 360;
const clamp = (v, lo, hi) => Math.max(lo, Math.min(hi, v));
const r2 = (v) => Math.round(v * 100) / 100;

/**
 * Build the tab into `host`.
 * @param ctx page context
 * @param {HTMLElement} host the (empty) tab panel
 * @param {object} opts
 *   measured(): { data, error } of /api/antenna/coverage (page state)
 *   pageAnt(): antenna id selected at the top of the page ('' = combined)
 *   bandLabel(band), bandEdges: number[] (ft, last null), niceMax(v)
 * @returns {{ measuredChanged(): void, unitsChanged(): void, shown(): void }}
 */
export function createTerrainLab(ctx, host, opts) {
  const { api, units, ui, charts, config, geo } = ctx;
  const { h } = ui;
  const C = charts.colors;
  const admin = createAdmin(ctx);
  const prefs = ui.prefs('airview.antenna.terrain.v1', { altFt: 10000, kind: 'los', brg: 45, km: 120, losAlt: 5000 });
  const p0 = prefs.get();
  const T = {
    alive: true, visible: false,
    status: {}, cov: {}, rf: {}, los: {},
    covKey: '', covLoading: false, lastStatus: 0, lastRf: 0,
    ant: '', lastPageAnt: null,
    altFt: isNum(p0.altFt) ? p0.altFt : 10000, kind: p0.kind === 'radio' ? 'radio' : 'los',
    q: { brg: isNum(p0.brg) ? norm360(p0.brg) : 45, km: isNum(p0.km) ? p0.km : 120, altFt: isNum(p0.losAlt) ? p0.losAlt : 5000 },
    k: null, // null = the server's k (station setting)
    kUnsupported: false, // the server ignored / rejected ?k= on /api/terrain/los: estimate other k in the browser
    adv: {}, // per-antenna site advice from /api/terrain/advice?ant= : { key, data, error, loading }
    losSeq: 0, losCtl: null, losTimer: 0,
    map: null, mapSeq: 0, mapFitted: false,
  };
  ctx.onCleanup(() => {
    T.alive = false;
    clearTimeout(T.losTimer);
    if (T.losCtl) T.losCtl.abort();
    if (T.map) { try { T.map.remove(); } catch { /* gone */ } T.map = null; }
  });

  // ---- helpers -------------------------------------------------------------------------------
  const distV = (km) => (isNum(km) ? Math.round(units.value('dist', km) * 10) / 10 : null);
  const distFmt = (v) => `${units.num(v, 0)} ${units.label('dist')}`;
  const hgtV = (m) => (isNum(m) ? Math.round(units.value('height', m) * 10) / 10 : null);
  const degFmt = (v, d = 2) => (isNum(v) ? `${units.num(v, d, d)}°` : units.DASH);
  const area = (km2) => {
    if (!isNum(km2)) return units.DASH;
    const f = units.value('dist', 1) ** 2;
    return `${units.si(km2 * f)} ${units.label('dist')}²`;
  };
  const brgAxis = { type: 'value', min: 0, max: 360, interval: 45, axisLabel: { formatter: (v) => ['N', 'NE', 'E', 'SE', 'S', 'SW', 'W', 'NW', 'N'][Math.round(v / 45)] ?? '' } };
  const antennas = () => T.cov.data?.antennas || [];
  const curAnt = () => antennas().find((a) => a.id === T.ant) || antennas()[0] || null;
  const cfgAntName = (id) => (config.current?.antennas || []).find((a) => a.id === id)?.name || id;
  /** The station k-factor (Settings → Terrain): what the coverage analysis was computed with. */
  const serverK = () => (isNum(T.cov.data?.k) ? T.cov.data.k : isNum(T.los.data?.k) && !T.los.kSent ? T.los.data.k : config.current?.terrain?.kFactor || 4 / 3);
  const kNow = () => (isNum(T.k) ? T.k : serverK());
  /** k to send with /api/terrain/los: only a user choice that differs from the station value. */
  const kParam = () => (!T.kUnsupported && isNum(T.k) && Math.abs(T.k - serverK()) >= 0.005 ? r2(T.k) : null);
  const rx = () => {
    const c = T.cov.data;
    if (c && isNum(c.lat) && isNum(c.lon)) return { lat: c.lat, lon: c.lon };
    const m = opts.measured().data?.receiver;
    if (m && isNum(m.lat) && isNum(m.lon)) return m;
    // the effective station every analysis uses (/api/status: located receiver, demo site or receiver GPS)
    const st = ctx.status?.station;
    if (st && st.source !== 'none' && isNum(st.lat) && isNum(st.lon)) return { lat: st.lat, lon: st.lon };
    return null;
  };
  /** Measured altitude band whose upper edge covers altFt. */
  const bandFor = (altFt) => {
    const e = opts.bandEdges;
    for (let i = 0; i < e.length - 1; i++) if (e[i + 1] === null || altFt <= e[i + 1]) return i;
    return e.length - 2;
  };
  const altColor = (altFt) => {
    const list = T.cov.data?.altitudesFt || [];
    const i = list.indexOf(altFt);
    return ALT_COLORS[(i < 0 ? 0 : i) % ALT_COLORS.length];
  };

  /** Card with a state overlay (same classes as the station panels). */
  function panel(o) {
    const main = o.content || h('div.chart', { style: o.height ? { '--h': o.height + 'px' } : null });
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
      get chart() { if (!chart) chart = charts.create(o.chartEl || main); return chart; },
      loading: (text) => set(ui.loading(text || 'Loading…', { compact: true })),
      empty: (title, text, icon, action) => set(ui.empty({ icon: icon || 'mountain', title, text, compact: true, action })),
      error: (err, retry) => set(ui.error(err, { retry, compact: true, title: 'Not available' })),
      ready: () => { overlay.classList.remove('on'); ui.clear(overlay); },
    };
  }
  /** Common gate for everything that needs the terrain analysis. Returns true when the caller may render. */
  function gate(p, what) {
    const st = T.status.data;
    if (T.cov.data) return true;
    if (T.status.error && !st) { p.error(T.status.error, () => pollStatus(true)); return false; }
    if (!st) { p.loading(); return false; }
    if (!st.enabled) { p.empty('Terrain analysis is switched off', 'Switch it on in Settings (Terrain) to compare the real horizon with what the station hears.', 'mountain', ui.button({ label: 'Settings', icon: 'settings', size: 'sm', href: '#/settings' })); return false; }
    if (!st.located) { p.empty('Receiver location needed', `${what} needs the antenna position. Set it in Settings, or let Locate Me estimate it from the traffic.`, 'pin'); return false; }
    if (st.job?.state === 'running') { p.loading(`Terrain analysis running… ${units.pct((st.job.progress || 0) * 100, 0)}`); return false; }
    if (T.cov.error && T.cov.error.status !== 503) { p.error(T.cov.error, () => loadCoverage(true)); return false; }
    if (!st.available) {
      p.empty('No terrain analysis yet', st.job?.state === 'error' ? `The last run failed: ${st.job.error || st.job.message}` : 'Download the elevation data and run the analysis with the button under "Terrain data" (about 30–40 MB, once).', 'mountain');
      return false;
    }
    p.loading('Loading terrain analysis…');
    return false;
  }
  const safe = (name, fn) => { try { fn(); } catch (e) { console.error(`[antenna/terrain] ${name} failed`, e); } };

  // ---- layout --------------------------------------------------------------------------------
  const antSeg = h('div.tl-antsel');
  const attrEl = h('div.tl-attr');
  const subEl = h('div.tl-sub');

  // terrain data / job
  const jobBody = h('div.tl-job');
  const P = {};
  P.job = panel({ title: 'Terrain data', icon: 'mountain', content: jobBody });
  const rfBody = h('div.tl-rf');
  P.rf = panel({ title: 'RF chain & noise', icon: 'wave', content: rfBody, actions: ui.button({ label: 'RF Lab', icon: 'wave', size: 'sm', variant: 'ghost', href: '#/rf', tip: 'Open the RF Lab: signal levels, noise floor, channel occupancy, interrogators' }) });

  // horizon panorama
  P.pano = panel({
    title: 'Horizon panorama: terrain against measured', icon: 'compass', height: 340, class: 'span-2',
    caption: 'Blue: the skyline of the terrain itself, as seen from the antenna (elevation angle of the highest ridge per bearing, earth curvature and refraction included). Amber: the measured skyline - the lowest elevation at which aircraft are actually received. Where the amber line sits clearly above the terrain (red bars), something close to the antenna that is not in the elevation model blocks the view: trees, buildings, the roof, the mast. Where both lines meet, the terrain is the limit and only height helps. With little low traffic the measured skyline still reads too high; give it a few days.',
  });
  // coverage polar
  const altSel = ui.select({ size: 'sm', options: [], onChange: (v) => { T.altFt = Number(v); prefs.set({ altFt: T.altFt }); renderPolar(); paintMap(); } });
  const kindSeg = ui.segmented({
    value: T.kind,
    options: [{ value: 'los', label: 'Line of sight', tip: 'Geometric line of sight over the terrain (with refraction)' }, { value: 'radio', label: 'Radio', tip: 'Link budget including diffraction over ridges and the receiver sensitivity' }],
    onChange: (v) => { T.kind = v; prefs.set({ kind: v }); renderPolar(); paintMap(); },
  });
  P.polar = panel({
    title: 'Theoretical against measured coverage', icon: 'radar', actions: [altSel, kindSeg], height: 420,
    caption: 'Dashed: what the terrain allows at the chosen altitude (line of sight, and the radio prediction that adds diffraction and the receiver sensitivity). Filled: the reliable range actually achieved in the matching altitude band. Measured close to theoretical = the installation is as good as the site; a measured dent where the theory is round = an obstruction or antenna problem in that direction.',
  });
  // mini map
  const mapEl = h('div.al-map');
  const mapLegend = h('div.al-maplegend');
  P.map = panel({
    title: 'Coverage map', icon: 'map', content: h('div.al-mapwrap.tl-mapwrap', mapEl),
    caption: 'Rings: theoretical coverage per altitude (the chosen one bold and filled). White: measured reliable outline for the matching altitude band. Click anywhere to draw the line-of-sight profile to that point.',
  });
  P.map.extra.append(mapLegend);
  // advice
  const adviceBody = h('div.tl-advice');
  const adviceChartEl = h('div.chart', { style: { '--h': '220px' } });
  let adviceChart = null;
  P.advice = panel({
    title: 'Site advice: what a higher mast would bring', icon: 'antenna', content: adviceBody, class: 'span-2',
    caption: 'Coverage area gained per altitude if the antenna were mounted higher, from the same terrain analysis. Low altitudes gain most because the horizon there is set by the nearest ridges. The worst sectors list the ridge that limits each direction - open its profile to see how much clearance is missing.',
  });
  // LOS profile tool
  const losCtl = h('div.tl-los-ctl');
  const losChartEl = h('div.chart.tl-los-chart');
  const losOut = h('div.tl-los-out');
  P.los = panel({
    title: 'Line-of-sight profile', icon: 'ruler', class: 'span-2',
    content: h('div.tl-los-main', losChartEl, losOut), chartEl: losChartEl,
    caption: 'The terrain between the antenna and one point, drawn the way radio planners do: the earth\'s curvature is added to the terrain as a bulge so that the radio path stays a straight line. The shaded band is the first Fresnel zone - the region that has to be at least 60 % free for an undisturbed signal. Ridges that cut into it cost diffraction loss (knife-edge model, ITU-R P.526 with the Deygout construction).',
  });

  { // controls stay usable while the profile itself shows a loading / empty / error state
    const wrap = P.los.main.parentElement;
    const grid = h('div.tl-los');
    wrap.before(grid);
    grid.append(losCtl, wrap);
  }

  ui.mount(host, h('div.tl',
    h('div.tl-head', h('div.grow', subEl), antSeg),
    h('div.grid.cols-2.al-grid',
      P.job.card, P.rf.card,
      P.pano.card,
      P.polar.card, P.map.card,
      P.advice.card,
      P.los.card),
    attrEl));

  // ---- job / status --------------------------------------------------------------------------
  function renderJob() {
    const st = T.status.data;
    if (T.status.error && !st) return P.job.error(T.status.error, () => pollStatus(true));
    if (!st) return P.job.loading();
    P.job.ready();
    const job = st.job || {};
    const running = job.state === 'running';
    const STATE = { idle: ['Idle', 'other'], running: ['Running', 'cyan'], done: ['Up to date', 'green'], error: ['Failed', 'red'], cancelled: ['Cancelled', 'amber'] };
    const [label, kind] = !st.enabled ? ['Switched off', 'other'] : !st.located ? ['Location needed', 'amber'] : STATE[job.state] || [job.state || 'Unknown', 'other'];
    const PHASE = { tiles: 'Downloading elevation tiles', terrain: 'Sampling the terrain', coverage: 'Computing coverage' };
    const t = job.tiles || {};
    const meter = ui.meter((job.progress || 0) * 100);
    const run = (path, params, what) => async (ev) => {
      const btn = ev?.currentTarget?.closest?.('.btn');
      btn?.setLoading?.(true);
      try {
        await admin.run(() => api.post(path, undefined, { params, signal: ctx.signal }), what);
        await pollStatus(true);
      } catch (e) {
        if (!e.cancelled && !e.aborted) ui.toast.error(e.message || String(e), { title: 'Terrain' });
      } finally { btn?.setLoading?.(false); }
    };
    const canRun = st.enabled && st.located;
    const btns = [];
    if (running) btns.push(ui.button({ label: 'Cancel', icon: 'x', size: 'sm', variant: 'danger', onClick: run('/api/terrain/cancel', null, 'Cancelling the terrain job needs the admin password.') }));
    else {
      btns.push(ui.button({
        label: st.available ? 'Download & analyse again' : 'Download & analyse', icon: 'download', size: 'sm', variant: st.available ? 'ghost' : 'primary', disabled: !canRun,
        tip: 'Fetch the elevation tiles around the station (about 30–40 MB the first time, cached afterwards) and run the analysis', onClick: run('/api/terrain/prefetch', st.available ? { force: 1 } : null, 'Starting the terrain download needs the admin password.'),
      }));
      if (st.available) btns.push(ui.button({ label: 'Recompute', icon: 'refresh', size: 'sm', variant: 'ghost', disabled: !canRun, tip: 'Re-run the last analysis ignoring caches', onClick: run('/api/terrain/recompute', null, 'Recomputing the terrain analysis needs the admin password.') }));
    }
    ui.mount(jobBody,
      h('div.row.tl-job-head', ui.badge(label, kind, { large: true }), h('span.muted.grow', running ? (PHASE[job.phase] || job.phase || '') : job.message || ''), ...btns),
      running || job.state === 'error' || job.state === 'cancelled' ? h('div.tl-job-prog', meter, h('span.num', units.pct((job.progress || 0) * 100, 0))) : null,
      job.state === 'error' && job.error ? h('div.al-finding.warn', ui.icon('alert'), h('div', job.error)) : null,
      ui.kv([
        ['Analysis', st.available ? `${units.dateTime(st.computedMs, { utc: false })} (${units.ago(st.computedMs, ctx.live.serverNow())})` : 'none yet'],
        ['Elevation tiles', t.total ? `${units.int(t.done)} of ${units.int(t.total)}${t.downloaded ? ` · ${units.int(t.downloaded)} downloaded (${units.bytes(t.bytes)})` : ''}${t.failed ? ` · ${units.int(t.failed)} failed` : ''}` : units.DASH],
        ['Cache on disk', st.provider ? `${units.int(st.provider.diskTiles)} tiles · ${units.bytes(st.provider.diskBytes)}` : units.DASH],
        ['Radius · k-factor', T.cov.data ? `${units.dist(T.cov.data.radiusKm)} · k = ${units.num(T.cov.data.k, 2, 2)}` : config.current?.terrain ? `${units.dist(config.current.terrain.radiusKm)} · k = ${units.num(config.current.terrain.kFactor, 2, 2)}` : units.DASH],
        ['Automatic download', st.autoPrefetch ? 'on' : 'off (only on request)'],
      ]),
      st.available && st.dataComplete === false ? h('div.al-finding.warn', ui.icon('alert'), h('div', 'Some elevation tiles were missing and are read as sea level, so the horizon may look lower than it is in places.')) : null,
      t.offline || st.provider?.offline ? h('div.al-finding.warn', ui.icon('alert'), h('div', 'The elevation server is not reachable at the moment; AirView retries later.')) : null);
    P.job.card.setSub(running ? `${units.pct((job.progress || 0) * 100, 0)} · ${PHASE[job.phase] || ''}` : '');
    if (st.attribution) ui.mount(attrEl, ui.icon('mountain'), st.attributionUrl ? h('a', { href: st.attributionUrl, target: '_blank', rel: 'noopener noreferrer' }, st.attribution) : h('span', st.attribution));
    return undefined;
  }

  async function pollStatus(force) {
    T.lastStatus = Date.now();
    try {
      const st = await api.get('/api/terrain/status', { signal: ctx.signal });
      if (!T.alive) return;
      const wasRunning = T.status.data?.job?.state === 'running';
      T.status = { data: st };
      safe('job', renderJob);
      if (st.available && st.key && st.key !== T.covKey) loadCoverage();
      else if (!T.cov.data || wasRunning !== (st.job?.state === 'running')) renderAll();
    } catch (e) {
      if (e.aborted || !T.alive) return;
      if (!force && T.status.data) return; // keep the last good status
      T.status = { error: e };
      renderAll();
    }
  }

  async function loadCoverage(force) {
    if (T.covLoading && !force) return;
    T.covLoading = true;
    const key = T.status.data?.key || '';
    try {
      const cov = await api.get('/api/terrain/coverage', { signal: ctx.signal, timeout: API_TIMEOUT });
      if (!T.alive) return;
      T.cov = { data: cov };
      T.covKey = cov?.key || key;
      pickAntenna();
      T.mapFitted = false;
    } catch (e) {
      if (e.aborted || !T.alive) return;
      if (!T.cov.data) T.cov = { error: e };
    } finally { T.covLoading = false; }
    renderAll();
    scheduleLos(0);
  }

  // ---- RF chain ------------------------------------------------------------------------------
  async function pollRf() {
    T.lastRf = Date.now();
    try {
      const d = await api.get('/api/rf/summary', { signal: ctx.signal });
      if (!T.alive) return;
      T.rf = { data: d };
    } catch (e) {
      if (e.aborted || !T.alive) return;
      if (T.rf.data && e.status !== 409) return;
      T.rf = { error: e };
    }
    safe('rf', renderRf);
  }
  function renderRf() {
    const { data: d, error } = T.rf;
    if (error) {
      if (error.status === 409) return P.rf.empty('RF statistics are switched off', 'Switch on "RF statistics" in Settings to see the noise floor, decode margin and channel load here.', 'wave', ui.button({ label: 'Settings', icon: 'settings', size: 'sm', href: '#/settings' }));
      return P.rf.error(error, () => pollRf());
    }
    if (!d) return P.rf.loading();
    if (!(d.health >= 0) && !isNum(d.fps)) return P.rf.empty('Not enough data yet', 'The RF statistics need a few minutes of received frames.', 'wave');
    P.rf.ready();
    const hk = d.healthStatus === 'ok' || d.healthStatus === 'good' ? 'green' : d.healthStatus === 'warn' || d.healthStatus === 'fair' ? 'amber' : d.healthStatus ? 'red' : 'other';
    const pct = (v, dg = 1) => (isNum(v) ? units.pct(v, dg) : units.DASH);
    ui.mount(rfBody,
      h('div.row.tl-rf-head',
        h('div.tl-rf-score', h('span.num', d.health >= 0 ? units.int(d.health) : units.DASH), h('span.unit', '/ 100')),
        ui.badge(d.healthStatus || 'no data', hk, { large: true }),
        h('span.muted', isNum(d.windowSec) ? `last ${units.age(d.windowSec)}` : '')),
      ui.kv([
        ['Noise floor (estimate)', units.dbfs(d.noiseFloorEstDbfs), { tip: 'Level of the weakest frames the receiver still sees; the decode floor sits above it' }],
        ['Decode floor', units.dbfs(d.decodeFloorDbfs)],
        ['Median level', units.dbfs(d.p50Dbfs)],
        ['SNR margin', units.db(d.snrMarginDb), { tip: 'Typical signal level above the decode floor. More margin = weak, far aircraft still decode.' }],
        ['Channel occupancy', pct(d.occupancyPct, 2), { tip: '1090 MHz air time taken by received replies (peak of the last 10 min in brackets)' }],
        ['Frames', units.rate(d.fps)],
        ['Position reception', isNum(d.pdPos) ? units.pct(d.pdPos * 100, 0) : units.DASH, { tip: 'Share of the expected ADS-B position squitters that were received' }],
      ], { columns: 2 }),
      ((Array.isArray(d.adviceItems) && d.adviceItems.length ? d.adviceItems : d.advice) || []).filter((t) => t && (typeof t === 'string' || t.text)).slice(0, 2).map((t) => h('div.al-finding.info', ui.icon('info'), h('div', itemText(t, units)))));
    P.rf.card.setSub(isNum(d.range90Km) ? `90 % detection out to ${units.dist(d.range90Km)}` : '');
    return undefined;
  }

  // ---- antenna selector ----------------------------------------------------------------------
  function pickAntenna() {
    const list = antennas();
    const pa = opts.pageAnt();
    if (pa !== T.lastPageAnt) { T.lastPageAnt = pa; if (pa && list.some((a) => a.id === pa)) T.ant = pa; }
    if (!list.some((a) => a.id === T.ant)) T.ant = list[0]?.id || '';
    if (list.length < 2) { ui.clear(antSeg); antSeg.dataset.key = ''; return; }
    const key = list.map((a) => a.id).join('|') + '#' + T.ant;
    if (antSeg.dataset.key === key) return;
    antSeg.dataset.key = key;
    ui.mount(antSeg, h('span.muted', 'Antenna'), ui.segmented({
      options: list.map((a) => ({ value: a.id, label: a.name || cfgAntName(a.id), tip: `${units.height(a.heightAglM, { digits: 1 })} above ground · ${units.height(a.antennaM, { digits: 0 })} AMSL` })),
      value: T.ant, onChange: (v) => { T.ant = v; antSeg.dataset.key = ''; pickAntenna(); renderAll(); scheduleLos(0); },
    }));
  }

  function renderHead() {
    const c = T.cov.data;
    const a = curAnt();
    if (!c || !a) { ui.setText(subEl, 'Real terrain around the station against what the antenna actually hears'); return; }
    ui.setText(subEl, `${a.name || cfgAntName(a.id)} at ${units.height(a.heightAglM, { digits: 1 })} above ground (${units.height(a.antennaM, { digits: 0 })} AMSL) · terrain ${units.dist(c.radiusKm)} around · k = ${units.num(c.k, 2, 2)} · measured: ${opts.periodLabel()}`);
  }

  // ---- horizon panorama ----------------------------------------------------------------------
  /** Compare the terrain skyline with the measured one per measured bin. */
  function skylineGaps(hz, meas) {
    const brg = meas?.bearingDeg || [];
    const el = meas?.skyline?.elevDeg || [];
    const n = brg.length;
    if (!n || !hz?.bearingDeg?.length) return [];
    const w = meas.binDeg || 360 / n;
    const cnt = meas.overall?.count; // positions per bin: a skyline from a handful of aircraft is not evidence
    const step = hz.stepDeg || 360 / hz.bearingDeg.length;
    const out = [];
    for (let i = 0; i < n; i++) {
      const m = el[i];
      if (!isNum(m) || !isNum(brg[i]) || (cnt && !(cnt[i] >= MIN_BIN_POSITIONS))) continue;
      const from = brg[i] - w / 2;
      const to = brg[i] + w / 2;
      let tmax = -Infinity; let dist = null;
      for (let b = from; b < to - 1e-9; b += step) {
        const j = Math.round(norm360(b) / step) % hz.bearingDeg.length;
        const t = hz.elevationDeg[j];
        if (isNum(t) && t > tmax) { tmax = t; dist = hz.distanceKm?.[j]; }
      }
      if (!Number.isFinite(tmax)) continue;
      out.push({ from: norm360(from), to: norm360(from) + w, mid: norm360(brg[i]), meas: m, terr: tmax, gap: m - tmax, ridgeKm: dist });
    }
    return out;
  }
  function gapSectors(gaps) {
    const bad = gaps.filter((g) => g.gap > 0.3).sort((a, b) => a.mid - b.mid);
    const groups = [];
    for (const g of bad) {
      const last = groups[groups.length - 1];
      if (last && g.from - last.to <= 2.01) { last.to = g.to; last.items.push(g); } else groups.push({ from: g.from, to: g.to, items: [g] });
    }
    if (groups.length > 1 && groups[0].from <= 0.01 && groups[groups.length - 1].to >= 359.99) {
      const tail = groups.pop();
      groups[0] = { from: tail.from, to: groups[0].to + 360, items: [...tail.items, ...groups[0].items] };
    }
    return groups.map((gr) => {
      const max = gr.items.reduce((a, b) => (b.gap > a.gap ? b : a));
      return { from: gr.from, to: gr.to, mid: norm360((gr.from + gr.to) / 2), maxGap: max.gap, meanMeas: gr.items.reduce((s, x) => s + x.meas, 0) / gr.items.length, bins: gr.items.length };
    }).sort((a, b) => b.maxGap - a.maxGap);
  }

  function renderPano() {
    if (!gate(P.pano, 'The horizon panorama')) { ui.clear(P.pano.extra); return; }
    const a = curAnt();
    const hz = a?.horizon;
    if (!hz || !hz.bearingDeg?.length) { P.pano.empty('No horizon for this antenna', 'The terrain analysis has no panorama for the selected antenna.'); return; }
    const measSrc = opts.measured();
    const meas = measSrc.data;
    const measOk = meas?.located && meas.skyline?.bearings > 0;
    const gaps = measOk ? skylineGaps(hz, meas) : [];
    const terr = hz.bearingDeg.map((b, i) => [b, isNum(hz.elevationDeg[i]) ? Math.round(hz.elevationDeg[i] * 1000) / 1000 : null]);
    terr.push([360, terr[0][1]]);
    const measLine = measOk ? meas.bearingDeg.map((b, i) => [b, isNum(meas.skyline.elevDeg[i]) ? r2(meas.skyline.elevDeg[i]) : null]) : [];
    const vals = [...terr.map((d) => d[1]), ...measLine.map((d) => d[1]), hz.smoothDipDeg].filter(isNum);
    const lo = Math.floor((Math.min(...vals) - 0.15) * 4) / 4;
    // a few very high measured bins (no low traffic in that direction yet) must not flatten the terrain
    const terrMax = Math.max(...terr.map((d) => d[1]).filter(isNum), 0.5);
    const hi = Math.ceil((Math.min(Math.max(...vals, 0.5), Math.max(terrMax + 2, 3)) + 0.25) * 4) / 4;
    const gapData = gaps.filter((g) => g.gap > 0.05).map((g) => [g.from, g.to, g.terr, g.meas, g.gap]);
    const findHz = (brg) => { const step = hz.stepDeg || 0.5; return Math.round(norm360(brg) / step) % hz.bearingDeg.length; };
    const tooltip = (ps) => {
      const brg = ps[0].value[0];
      const j = findHz(brg);
      const g = gaps.find((x) => brg >= x.from && brg < x.to) || gaps.find((x) => brg + 360 >= x.from && brg + 360 < x.to);
      const ridge = hz.distanceKm?.[j];
      const rows = [`<b>${units.bearing(brg)}</b>`,
        `Terrain skyline ${degFmt(hz.elevationDeg[j])}${isNum(ridge) && ridge > 0 ? ` · ridge ${units.dist(ridge)} away, ${units.height(hz.terrainM?.[j], { digits: 0 })}` : ''}`];
      if (g) {
        rows.push(`Measured ${degFmt(g.meas)} · gap <b>${units.num(g.gap, 2, 2)}°</b>`);
        rows.push(g.gap > 0.3 ? 'local obstruction (not terrain)' : g.gap > 0.1 ? 'slightly above the terrain' : g.gap < -0.1 ? 'heard below the terrain skyline (diffraction / refraction)' : 'terrain is the limit');
      }
      return rows.join('<br>');
    };
    const series = [
      {
        type: 'line', name: 'Terrain skyline', data: terr, color: C.blueHi, showSymbol: false, step: false, z: 2,
        lineStyle: { width: 1.4 }, areaStyle: { origin: 'start', color: charts.fade(C.blue, 0.06, 0.5) },
        markLine: isNum(hz.smoothDipDeg) ? { silent: true, symbol: 'none', lineStyle: { color: '#c9d8f5', type: 'dashed', width: 1 }, data: [{ yAxis: hz.smoothDipDeg }], label: { formatter: `smooth earth ${units.num(hz.smoothDipDeg, 2, 2)}°`, position: 'insideStartTop', color: C.text3, fontSize: 10 } } : undefined,
      },
      {
        type: 'custom', name: 'Local obstruction', data: gapData, z: 3, clip: true, color: C.red, encode: { x: [0, 1], y: [2, 3] }, silent: true,
        renderItem(params, api2) {
          const p0 = api2.coord([api2.value(0), api2.value(3)]);
          const p1 = api2.coord([api2.value(1), api2.value(2)]);
          const gap = api2.value(4);
          return { type: 'rect', shape: { x: p0[0], y: p0[1], width: Math.max(1, p1[0] - p0[0] - 0.5), height: Math.max(1, p1[1] - p0[1]) }, style: { fill: gap > 0.3 ? 'rgba(255,77,94,0.42)' : 'rgba(251,191,36,0.22)' } };
        },
      },
    ];
    if (measLine.length) series.push({ type: 'line', name: 'Measured skyline', data: measLine, color: C.amber, showSymbol: true, symbolSize: 3, connectNulls: false, step: 'middle', z: 4, lineStyle: { width: 1.8 } });
    P.pano.chart.set({
      animation: false,
      grid: { left: 8, right: 14, top: 34, bottom: 8, containLabel: true },
      legend: { show: true, top: 0, left: 0, data: series.map((s) => s.name) },
      tooltip: { trigger: 'axis', confine: true, formatter: tooltip, axisPointer: { type: 'line' } },
      xAxis: { ...brgAxis },
      yAxis: { type: 'value', min: lo, max: hi, axisLabel: { formatter: (v) => `${String(v).replace('-', '−')}°` } },
      series,
    }, { replace: true });
    P.pano.ready();
    P.pano.card.setSub(`terrain: mean ${degFmt(hz.meanSkylineDeg)} · highest ${degFmt(hz.maxSkylineDeg)} towards ${units.bearing(hz.maxSkylineBrgDeg)} · smooth-earth horizon ${units.dist(hz.smoothHorizonKm)}`);

    // findings under the chart
    const items = [];
    if (measSrc.error) items.push(h('div.al-finding.warn', ui.icon('alert'), h('div', `Measured skyline unavailable: ${measSrc.error.message}`)));
    else if (!meas) items.push(h('div.al-finding.info', ui.icon('info'), h('div', 'Loading the measured skyline…')));
    else if (!measOk) items.push(h('div.al-finding.info', ui.icon('info'), h('div', 'Not enough data yet for a measured skyline: it needs low, distant traffic in many directions (typically a day or two). Until then only the terrain is shown.')));
    else {
      const cmp = gaps.length;
      const sectors = gapSectors(gaps);
      const terrLimited = gaps.filter((g) => Math.abs(g.gap) <= 0.15).length;
      items.push(ui.kv([
        ['Directions compared', `${units.int(cmp)} of ${units.int(meas.bearingDeg.length)} bearings with enough measured traffic (${MIN_BIN_POSITIONS}+ positions)`],
        ['Terrain-limited', cmp ? `${units.pct((terrLimited / cmp) * 100, 0)} of them (measured within 0.15° of the terrain)` : units.DASH],
        ['Median gap', cmp ? `${units.num(median(gaps.map((g) => g.gap)), 2, 2)}°` : units.DASH, { tip: 'Measured minus terrain skyline. Around 0 means the terrain is what limits the station.' }],
      ], { columns: 2 }));
      if (!sectors.length) items.push(h('div.al-finding.good', ui.icon('check-circle'), h('div', 'No local obstruction found: wherever there is measured data, the station hears down to the terrain skyline.')));
      for (const s of sectors.slice(0, 4)) {
        items.push(h('div.al-finding.warn', ui.icon('alert'),
          h('div.grow', h('b', `${units.deg(s.from)}–${units.deg(s.to)} (${units.cardinal(s.mid)}): `),
            `aircraft only appear from ${degFmt(s.meanMeas)} up, ${units.num(s.maxGap, 2, 2)}° above the terrain skyline. Something near the antenna (tree, building, roof edge) shadows this sector.`),
          ui.button({ label: 'Profile', icon: 'ruler', size: 'sm', variant: 'ghost', tip: 'Line-of-sight profile in this direction', onClick: () => openProfile(s.mid, Math.min(T.cov.data.radiusKm, 80), 3000) })));
      }
    }
    ui.mount(P.pano.extra, items);
  }

  // ---- theoretical vs measured polar ---------------------------------------------------------
  function syncAltSel() {
    const list = T.cov.data?.altitudesFt || [];
    if (list.length && !list.includes(T.altFt)) T.altFt = list.includes(10000) ? 10000 : list[Math.floor(list.length / 2)];
    altSel.setOptions(list.map((ft) => ({ value: ft, label: units.alt(ft, { fl: true }) })));
    altSel.value = String(T.altFt);
  }
  function renderPolar() {
    syncAltSel();
    if (!gate(P.polar, 'The coverage comparison')) { ui.clear(P.polar.extra); return; }
    const a = curAnt();
    const los = (a?.los || []).find((x) => x.altFt === T.altFt);
    const radio = (a?.radio || []).find((x) => x.altFt === T.altFt);
    if (!los && !radio) { P.polar.empty('No theoretical coverage for this altitude', 'Pick another altitude.'); return; }
    const theo = T.kind === 'radio' ? radio || los : los || radio;
    const step = a.horizon?.stepDeg || 360 / theo.rangeKm.length;
    const ring = (l) => {
      const out = [];
      const every = Math.max(1, Math.round(1 / step));
      for (let i = 0; i < l.rangeKm.length; i += every) out.push([distV(l.rangeKm[i]) ?? 0, r2(i * step)]);
      return out;
    };
    const meas = opts.measured().data;
    const bandIdx = bandFor(T.altFt);
    const band = meas?.located ? (meas.bands || []).find((b) => b.band === bandIdx) : null;
    const hasMeas = band && band.positions > 0;
    const series = [];
    if (hasMeas) {
      series.push({ name: `Measured reliable (${opts.bandLabel(bandIdx)})`, data: meas.bearingDeg.map((d, i) => [distV(band.robustKm[i]) ?? 0, d]), color: C.blue, width: 2, area: 0.32 });
      series.push({ name: 'Measured maximum', data: meas.bearingDeg.map((d, i) => [distV(band.maxKm[i]) ?? 0, d]), color: C.cyan, width: 1 });
    }
    if (los) series.push({ name: 'Terrain line of sight', data: ring(los), color: altColor(T.altFt), width: T.kind === 'los' ? 2 : 1.2, _dashed: true });
    if (radio) series.push({ name: 'Radio prediction', data: ring(radio), color: '#ffffff', width: T.kind === 'radio' ? 1.8 : 1, _dashed: true });
    if (isNum(los?.smoothKm)) series.push({ name: 'Smooth earth', data: [0, 90, 180, 270].flatMap((q) => Array.from({ length: 30 }, (_, i) => [distV(los.smoothKm), q + i * 3])), color: C.text4, width: 1, _dotted: true });
    const peak = Math.max(...series.flatMap((s) => s.data.map((d) => d[0] || 0)));
    const opt = charts.polar({ series, maxR: opts.niceMax(peak * 1.04), rFormat: distFmt, legend: true });
    opt.series.forEach((s, i) => { if (series[i]._dashed) s.lineStyle.type = 'dashed'; if (series[i]._dotted) s.lineStyle.type = 'dotted'; s.z = series[i].area ? 2 : 3; });
    P.polar.chart.set(opt, { replace: true });
    P.polar.ready();
    P.polar.card.setSub(`${units.alt(T.altFt, { fl: true })} · ${a.name || cfgAntName(a.id)}`);

    // numbers
    const rows = [
      ['Theoretical mean range', `${units.dist(theo.meanKm)} (${units.dist(theo.minKm)} – ${units.dist(theo.maxKm)})`],
      ['Theoretical area', area(theo.areaKm2)],
    ];
    if (hasMeas) {
      const perBin = (brg) => { const j = Math.round(norm360(brg) / step) % theo.rangeKm.length; return theo.rangeKm[j]; };
      const ratios = [];
      meas.bearingDeg.forEach((d, i) => { const m = band.robustKm[i]; const t = perBin(d); if (isNum(m) && m > 0 && isNum(t) && t > 0) ratios.push(Math.min(1.5, m / t)); });
      rows.push(['Measured reliable, best', `${units.dist(band.bestKm)} towards ${units.bearing(band.bestBrg)}`]);
      rows.push(['Achieved', ratios.length ? `${units.pct(median(ratios) * 100, 0)} of the ${T.kind === 'radio' ? 'radio prediction' : 'line of sight'} (median over ${units.int(ratios.length)} bearings)` : units.DASH,
        { tip: 'Per bearing: measured reliable range divided by the theoretical one. Bearings without traffic are left out.' }]);
    }
    ui.mount(P.polar.extra, ui.kv(rows),
      !meas ? null : !hasMeas ? h('div.al-finding.info', ui.icon('info'), h('div', `Not enough data yet in the ${opts.bandLabel(bandIdx)} band for a measured outline - only the theory is shown.`)) : null);
  }

  // ---- mini map ------------------------------------------------------------------------------
  function ringCoords(r, rangeKm, step, every = 2) {
    const ring = [];
    for (let i = 0; i < rangeKm.length; i += every) ring.push(geo.destination(r.lat, r.lon, i * step, Math.max(0, rangeKm[i] || 0)));
    ring.push(ring[0]);
    return unwrapRing(ring);
  }
  function mapData() {
    const r = rx();
    const a = curAnt();
    const empty = { type: 'FeatureCollection', features: [] };
    if (!r || !a) return { rings: empty, sel: empty, meas: empty };
    const step = a.horizon?.stepDeg || 0.5;
    const list = a[T.kind] || a.los || [];
    const rings = { type: 'FeatureCollection', features: list.map((l) => ({ type: 'Feature', properties: { alt: l.altFt, color: altColor(l.altFt), sel: l.altFt === T.altFt ? 1 : 0 }, geometry: { type: 'LineString', coordinates: ringCoords(r, l.rangeKm, step) } })) };
    const s = list.find((l) => l.altFt === T.altFt);
    const sel = s ? { type: 'Feature', properties: { color: altColor(s.altFt) }, geometry: { type: 'Polygon', coordinates: [ringCoords(r, s.rangeKm, step)] } } : empty;
    const meas = opts.measured().data;
    const band = meas?.located ? (meas.bands || []).find((b) => b.band === bandFor(T.altFt)) : null;
    let measF = empty;
    if (band && band.positions > 0) {
      const ring = meas.bearingDeg.map((d, i) => geo.destination(r.lat, r.lon, d, Math.max(0, band.robustKm[i] || 0)));
      ring.push(ring[0]);
      measF = { type: 'Feature', properties: {}, geometry: { type: 'Polygon', coordinates: [unwrapRing(ring)] } };
    }
    return { rings, sel, meas: measF };
  }
  function losData() {
    const r = rx();
    const empty = { type: 'FeatureCollection', features: [] };
    if (!r) return { line: empty, pts: empty };
    const tgt = geo.destination(r.lat, r.lon, T.q.brg, T.q.km);
    const ev = currentEval();
    const color = !ev ? '#c9d8f5' : ev.clear ? (ev.fresnel60Clear ? C.green : C.amber) : C.red;
    const pts = [{ type: 'Feature', properties: { color, r: 5 }, geometry: { type: 'Point', coordinates: tgt } }];
    if (ev?.obstruction) pts.push({ type: 'Feature', properties: { color: C.red, r: 4 }, geometry: { type: 'Point', coordinates: [ev.obstruction.lon, ev.obstruction.lat] } });
    return {
      line: { type: 'Feature', properties: { color }, geometry: { type: 'LineString', coordinates: unwrapRing([[r.lon, r.lat], tgt]) } },
      pts: { type: 'FeatureCollection', features: pts },
    };
  }
  function paintMap() {
    const map = T.map;
    if (!map || !map.getSource('tl-rings')) return;
    const d = mapData();
    map.getSource('tl-rings').setData(d.rings);
    map.getSource('tl-sel').setData(d.sel);
    map.getSource('tl-meas').setData(d.meas);
    paintLos();
    const a = curAnt();
    const list = (a?.[T.kind] || []);
    ui.mount(mapLegend, h('span.al-leg-title', T.kind === 'radio' ? 'Radio prediction' : 'Line of sight'),
      list.map((l) => h('span.tl-leg', { class: l.altFt === T.altFt ? 'on' : '' }, h('i', { style: { background: altColor(l.altFt) } }), units.alt(l.altFt, { fl: true }))),
      d.meas.geometry ? h('span.tl-leg', h('i.tl-leg-meas'), 'measured') : null);
    if (!T.mapFitted && d.rings.features.length) {
      let w = Infinity; let s = Infinity; let e = -Infinity; let n = -Infinity;
      for (const f of d.rings.features) for (const [lo, la] of f.geometry.coordinates) { w = Math.min(w, lo); e = Math.max(e, lo); s = Math.min(s, la); n = Math.max(n, la); }
      if (Number.isFinite(w)) { map.fitBounds([[w, s], [e, n]], { padding: 16, duration: 0, maxZoom: 9 }); T.mapFitted = true; }
    }
  }
  function paintLos() {
    const map = T.map;
    if (!map || !map.getSource('tl-ray')) return;
    const d = losData();
    map.getSource('tl-ray').setData(d.line);
    map.getSource('tl-pts').setData(d.pts);
  }
  async function ensureMap() {
    if (T.map || !T.visible) return;
    const r = rx();
    if (!r) return;
    const my = ++T.mapSeq;
    const [maplibregl] = await Promise.all([
      import('../../vendor/maplibre/maplibre-gl.mjs'),
      ui.loadCss(new URL('../../vendor/maplibre/maplibre-gl.css', import.meta.url)),
    ]);
    if (!T.alive || my !== T.mapSeq || T.map) return;
    const basemap = basemapById(config.current?.ui?.mapStyle || 'airview-dark');
    const map = new maplibregl.Map({
      container: mapEl, style: styleFor(basemap), center: [r.lon, r.lat], zoom: 5, minZoom: 2, maxZoom: 12,
      dragRotate: false, pitchWithRotate: false, touchPitch: false, attributionControl: false, fadeDuration: 0, cooperativeGestures: true,
      transformRequest: makeTransformRequest(() => !!ctx.live.status?.tileCache),
    });
    T.map = map;
    map.touchZoomRotate.disableRotation();
    map.addControl(new maplibregl.AttributionControl({ compact: true }), 'bottom-right');
    map.addControl(new maplibregl.NavigationControl({ showCompass: false }), 'top-right');
    let warned = 0;
    map.on('error', (ev) => { if (warned++ < 2) console.warn('[antenna terrain map]', ev?.error?.message || ev); });
    map.on('load', () => {
      if (!T.alive) return;
      const empty = { type: 'FeatureCollection', features: [] };
      for (const id of ['tl-rings', 'tl-sel', 'tl-meas', 'tl-ray', 'tl-pts', 'tl-rx']) map.addSource(id, { type: 'geojson', data: empty });
      map.addLayer({ id: 'tl-sel-fill', type: 'fill', source: 'tl-sel', paint: { 'fill-color': ['get', 'color'], 'fill-opacity': 0.1 } });
      map.addLayer({ id: 'tl-meas-fill', type: 'fill', source: 'tl-meas', paint: { 'fill-color': '#3b8bff', 'fill-opacity': 0.22 } });
      map.addLayer({ id: 'tl-rings', type: 'line', source: 'tl-rings', paint: { 'line-color': ['get', 'color'], 'line-width': ['case', ['==', ['get', 'sel'], 1], 2.4, 1], 'line-opacity': ['case', ['==', ['get', 'sel'], 1], 1, 0.45], 'line-dasharray': [3, 2] } });
      map.addLayer({ id: 'tl-meas-line', type: 'line', source: 'tl-meas', paint: { 'line-color': '#ffffff', 'line-width': 1.4, 'line-opacity': 0.85 } });
      map.addLayer({ id: 'tl-ray', type: 'line', source: 'tl-ray', paint: { 'line-color': ['get', 'color'], 'line-width': 2.2 } });
      map.addLayer({ id: 'tl-pts', type: 'circle', source: 'tl-pts', paint: { 'circle-radius': ['get', 'r'], 'circle-color': ['get', 'color'], 'circle-stroke-color': '#04102a', 'circle-stroke-width': 1.5 } });
      map.getSource('tl-rx').setData({ type: 'Feature', properties: {}, geometry: { type: 'Point', coordinates: [r.lon, r.lat] } });
      map.addLayer({ id: 'tl-rx-glow', type: 'circle', source: 'tl-rx', paint: { 'circle-radius': 14, 'circle-color': '#2f7dff', 'circle-opacity': 0.35, 'circle-blur': 0.7 } });
      map.addLayer({ id: 'tl-rx', type: 'circle', source: 'tl-rx', paint: { 'circle-radius': 4, 'circle-color': '#ffffff', 'circle-stroke-color': '#2f7dff', 'circle-stroke-width': 2.5 } });
      paintMap();
    });
    map.on('click', (ev) => {
      const p = rx();
      if (!p || !T.cov.data) return;
      const km = geo.distanceKm(p.lat, p.lon, ev.lngLat.lat, ev.lngLat.lng);
      if (km < 0.5) return;
      openProfile(geo.bearing(p.lat, p.lon, ev.lngLat.lat, ev.lngLat.lng), Math.min(km, T.cov.data.radiusKm), T.q.altFt, false);
    });
    map.getCanvas().style.cursor = 'crosshair';
  }
  function renderMap() {
    if (!gate(P.map, 'The coverage map')) return;
    P.map.ready();
    if (!T.map) ensureMap().catch((e) => { console.error('[antenna/terrain] map failed', e); P.map.error(new Error('The map could not be started (' + (e?.message || 'WebGL unavailable') + ').')); });
    else paintMap();
  }

  // ---- site advice ---------------------------------------------------------------------------
  /**
   * Site advice for the selected antenna. The coverage reply carries the advice of one antenna; for any other
   * antenna /api/terrain/advice?ant= is fetched once per analysis key. Falls back to the coverage advice
   * (with a note) when the server has none for that antenna.
   */
  function adviceFor() {
    const base = T.cov.data?.advice;
    const ant = T.ant;
    if (!ant || !base || base.antennaId === ant || antennas().length < 2) return { adv: base };
    const key = `${T.covKey}#${ant}`;
    if (T.adv.key !== key) {
      T.adv = { key, loading: true };
      api.get('/api/terrain/advice', { params: { ant }, signal: ctx.signal, timeout: API_TIMEOUT })
        .then((d) => { if (T.adv.key === key) T.adv = { key, data: d }; })
        .catch((e) => { if (!e.aborted && T.adv.key === key) T.adv = { key, error: e }; })
        .finally(() => { if (T.alive && T.adv.key === key && T.ant === ant) safe('advice', renderAdvice); });
    }
    if (T.adv.data) return { adv: T.adv.data };
    if (T.adv.loading) return { loading: true };
    return { adv: base, fallback: true };
  }

  function renderAdvice() {
    if (!gate(P.advice, 'The site advice')) return;
    const pick = adviceFor();
    if (pick.loading) { P.advice.loading(`Computing the advice for the ${cfgAntName(T.ant)} antenna…`); return; }
    const adv = pick.adv;
    if (!adv || !(adv.heights || []).length) { P.advice.empty('No site advice available', 'The analysis did not produce advice for this station.'); return; }
    P.advice.ready();
    const alts = adv.altitudesFt || [];
    const base = adv.heights.find((x) => x.extraM === 0) || adv.heights[0];
    const ups = adv.heights.filter((x) => x.extraM > 0).sort((a, b) => a.extraM - b.extraM);
    const up = (x) => `+${units.height(x.extraM, { digits: 0 })}`;
    const tbl = ui.table({
      columns: [
        { key: 'alt', label: 'Altitude', render: (r) => units.alt(r.alt, { fl: true }) },
        { key: 'area', label: 'Coverage now', align: 'right', num: true, render: (r) => area(r.area) },
        { key: 'range', label: 'Mean range', align: 'right', num: true, render: (r) => units.dist(r.range) },
        ...ups.map((u, k) => ({
          key: 'u' + k, label: `${up(u)} (${units.height(u.antennaAglM, { digits: 0 })})`, align: 'right', num: true,
          render: (r) => {
            const g = u.areaGainPct?.[r.i];
            const d = isNum(u.areaKm2?.[r.i]) && isNum(r.area) ? u.areaKm2[r.i] - r.area : null;
            return h('span.tl-gain', { class: g >= 5 ? 'big' : g >= 1 ? 'mid' : '' }, isNum(g) ? `+${units.num(g, 1, 1)} %` : units.DASH, isNum(d) ? h('small', ` +${area(d)}`) : null);
          },
        })),
        { key: 'per', label: `per ${units.label('height')}`, align: 'right', num: true, render: (r) => (isNum(adv.km2PerMetre?.[r.i]) ? area(adv.km2PerMetre[r.i] / units.value('height', 1)) : units.DASH), tip: 'Area gained per unit of extra antenna height (average over the steps)' },
      ],
      rows: alts.map((ft, i) => ({ i, alt: ft, area: base.areaKm2?.[i], range: base.meanRangeKm?.[i] })),
    });
    if (!adviceChart) adviceChart = charts.create(adviceChartEl);
    const opt = charts.bar({
      x: alts.map((ft) => units.alt(ft, { fl: true })),
      series: ups.map((u, k) => ({ name: up(u), data: alts.map((_, i) => (isNum(u.areaGainPct?.[i]) ? r2(u.areaGainPct[i]) : null)), color: [C.blue, C.cyan, C.green, C.amber][k % 4] })),
      yName: '% more area', valueFormat: (v) => (isNum(v) ? `+${units.num(v, 1, 1)} %` : units.DASH),
    });
    opt.xAxis.axisLabel = { interval: 0, fontSize: 9.5, hideOverlap: true };
    opt.grid.top = 40;
    adviceChart.set(opt, { replace: true });

    // headline sentences built from the numbers (unit-aware)
    const refI = Math.max(0, alts.indexOf(adv.refAltFt));
    const best = ups[ups.length - 1];
    const lowI = 0;
    const lines = [];
    if (best) {
      lines.push(`Raising the antenna by ${units.height(best.extraM, { digits: 0 })} adds ${units.num(best.areaGainPct?.[lowI], 1, 1)} % coverage area at ${units.alt(alts[lowI])} and ${units.num(best.areaGainPct?.[refI], 1, 1)} % at ${units.alt(alts[refI], { fl: true })}.`);
      const g = best.areaGainPct?.[refI];
      lines.push(g >= 5 ? 'A higher mast is worth it: the horizon is clearly limited by nearby terrain.' : g >= 1.5 ? 'A higher mast brings a moderate gain, mostly for low traffic.' : 'The site is already high relative to its surroundings: a higher mast brings little; gain and noise matter more.');
    }
    const sectors = (adv.worstSectors || []).map((i) => (adv.sectors || []).find((s) => s.sector === i) || adv.sectors?.[i]).filter(Boolean);
    const r = rx();
    const sectorRows = sectors.map((s) => {
      const brg = r && isNum(s.limitLat) ? geo.bearing(r.lat, r.lon, s.limitLat, s.limitLon) : (s.fromDeg + s.toDeg) / 2;
      const below = isNum(s.smoothKm) && s.smoothKm > 0 && isNum(s.rangeKm) ? (1 - s.rangeKm / s.smoothKm) * 100 : null;
      return h('div.al-finding.warn', ui.icon('mountain'),
        h('div.grow',
          h('b', `${units.deg(s.fromDeg)}–${units.deg(s.toDeg)} (${units.cardinal((s.fromDeg + s.toDeg) / 2)}): `),
          `skyline up to ${degFmt(s.maxSkylineDeg)} at ${units.dist(s.limitDistKm)} (ridge ${units.height(s.limitTerrainM, { digits: 0 })})`,
          isNum(below) ? `; range at ${units.alt(adv.refAltFt, { fl: true })} ${units.num(below, 0)} % below the smooth-earth horizon` : '',
          isNum(s.blockedPct) ? `; ${units.num(s.blockedPct, 0)} % of the sector blocked` : '',
          isNum(s.nearestObstacleKm) && s.nearestObstacleKm > 0 ? `. Nearest obstacle above the horizontal: ${units.dist(s.nearestObstacleKm)} towards ${units.deg(s.nearestObstacleBrg)}.` : '.'),
        ui.button({ label: 'Profile', icon: 'ruler', size: 'sm', variant: 'ghost', tip: 'Line-of-sight profile across this ridge', onClick: () => openProfile(brg, Math.min(T.cov.data.radiusKm, Math.max(s.limitDistKm * 1.6, (s.rangeKm || 0) * 1.1)), adv.refAltFt) }));
    });
    const advAnt = adv.antennaId && adv.antennaId !== T.ant ? h('div.al-finding.info', ui.icon('info'), h('div', `The advice is computed for the ${cfgAntName(adv.antennaId)} antenna${pick.fallback ? ' - this server has no separate advice for the selected one' : ''}.`)) : null;
    ui.mount(adviceBody,
      h('div.tl-advice-top', h('div.tl-advice-text', lines.map((t) => h('p.al-text', t)), advAnt), adviceChartEl),
      h('div.table-wrap', tbl),
      sectorRows.length ? [ui.section('Worst sectors', 'mountain'), h('div.al-findings', sectorRows)] : null);
    P.advice.card.setSub(`${cfgAntName(adv.antennaId)} · now ${units.height(base.antennaAglM, { digits: 0 })} above ground`);
  }

  // ---- LOS profile tool ----------------------------------------------------------------------
  const sl = {
    brg: ui.slider({ min: 0, max: 359.5, step: 0.5, value: T.q.brg, format: (x) => units.bearing(x), onInput: (x) => { T.q.brg = x; onQuery(); } }),
    km: ui.slider({ min: 1, max: 450, step: 0.5, value: T.q.km, format: (x) => units.dist(x), onInput: (x) => { T.q.km = x; onQuery(); } }),
    alt: ui.slider({ min: 500, max: 45000, step: 250, value: T.q.altFt, format: (x) => units.alt(x, { fl: true }), onInput: (x) => { T.q.altFt = x; onQuery(); } }),
    k: ui.slider({ min: 0.5, max: 4, step: 0.01, value: 4 / 3, format: (x) => `k = ${units.num(x, 2, 2)}`, onInput: (x) => { T.k = x; kSeg.value = presetFor(x); kChanged(); } }),
  };
  const presetFor = (k) => K_PRESETS.find((p) => Math.abs(p.k - k) < 0.006)?.value ?? null;
  const kSeg = ui.segmented({
    value: '1.33', options: K_PRESETS.map((p) => ({ value: p.value, label: p.label, tip: p.tip })),
    onChange: (v) => { const p = K_PRESETS.find((x) => x.value === v); if (p) { T.k = p.k; sl.k.value = p.k; kChanged(); } },
  });
  const kReset = ui.button({ label: 'Station value', icon: 'refresh', size: 'sm', variant: 'ghost', tip: 'Back to the k-factor of the station (Settings → Terrain)', onClick: () => { T.k = null; syncK(); kChanged(); } });
  /** New k: draw the browser estimate at once, then ask the server for the exact profile at that k. */
  function kChanged() {
    drawLos();
    const want = kParam();
    const have = T.los.kSent ? T.los.data?.k : null;
    if (T.los.data && (want === null ? have !== null : !(isNum(have) && Math.abs(have - want) < 0.005))) scheduleLos(400);
  }
  const kInfo = h('div.tl-k-info');
  function syncK() { const k = kNow(); sl.k.value = k; kSeg.value = presetFor(k); }
  ui.mount(losCtl,
    ui.field({ label: 'Bearing', control: sl.brg }),
    ui.field({ label: 'Distance', control: sl.km, hint: 'or click on the coverage map' }),
    ui.field({ label: 'Aircraft altitude', control: sl.alt }),
    h('div.tl-kbox',
      ui.field({ label: 'Refraction (k-factor)', control: sl.k }),
      h('div.row.tl-k-row', kSeg, kReset),
      kInfo));

  function onQuery() {
    prefs.set({ brg: T.q.brg, km: T.q.km, losAlt: T.q.altFt });
    paintLos();
    scheduleLos(350);
  }
  function openProfile(brg, km, altFt, scroll = true) {
    T.q = { brg: Math.round(norm360(brg) * 2) / 2, km: clamp(Math.round(km * 2) / 2, 1, T.cov.data?.radiusKm || 450), altFt: clamp(Math.round(altFt / 250) * 250, 500, 45000) };
    sl.brg.value = T.q.brg; sl.km.value = T.q.km; sl.alt.value = T.q.altFt;
    onQuery();
    scheduleLos(0);
    if (scroll) P.los.card.scrollIntoView({ behavior: 'smooth', block: 'start' });
  }
  function scheduleLos(ms) {
    clearTimeout(T.losTimer);
    T.losTimer = setTimeout(fetchLos, ms);
  }
  async function fetchLos() {
    const r = rx();
    if (!T.alive || !T.cov.data || !r) { drawLos(); return; }
    const my = ++T.losSeq;
    if (T.losCtl) T.losCtl.abort();
    const ctl = new AbortController();
    T.losCtl = ctl;
    const onAbort = () => ctl.abort();
    ctx.signal.addEventListener('abort', onAbort, { once: true });
    const [lon, lat] = geo.destination(r.lat, r.lon, T.q.brg, T.q.km);
    T.los = { ...T.los, loading: true };
    if (!T.los.data) drawLos();
    else P.los.card.classList.add('tl-busy');
    const kSent = kParam();
    try {
      const params = { lat: lat.toFixed(5), lon: lon.toFixed(5), altFt: T.q.altFt };
      if (T.ant) params.ant = T.ant;
      if (kSent !== null) params.k = kSent;
      const d = await api.get('/api/terrain/los', { params, signal: ctl.signal, timeout: LOS_TIMEOUT });
      if (my !== T.losSeq || !T.alive) return;
      // an older server ignores ?k= and answers for the station k: fall back to the browser estimate
      if (kSent !== null && !(isNum(d?.k) && Math.abs(d.k - kSent) < 0.005)) T.kUnsupported = true;
      T.los = { data: d, kSent: kSent !== null && !T.kUnsupported };
    } catch (e) {
      if (e.aborted || my !== T.losSeq || !T.alive) return;
      if (kSent !== null && e.status === 400) { T.kUnsupported = true; scheduleLos(0); return; }
      T.los = { error: e };
    } finally {
      ctx.signal.removeEventListener('abort', onAbort);
      if (my === T.losSeq) P.los.card.classList.remove('tl-busy');
    }
    if (T.k === null) syncK();
    drawLos();
  }
  let evalCache = { los: null, k: null, ev: null };
  function currentEval() {
    const d = T.los.data;
    if (!d) return null;
    const k = kNow();
    if (evalCache.los !== d || evalCache.k !== k) evalCache = { los: d, k, ev: evaluate(d, k) };
    return evalCache.ev;
  }

  function drawLos() {
    renderKInfo();
    if (!gate(P.los, 'The line-of-sight tool')) { ui.clear(losOut); return; }
    const { data: d, error, loading } = T.los;
    if (error) { P.los.error(error, () => scheduleLos(0)); ui.clear(losOut); return; }
    if (!d) { if (loading) P.los.loading('Reading the terrain profile…'); else P.los.empty('Choose a direction', 'Set bearing, distance and altitude, or click on the coverage map.', 'ruler'); return; }
    const ev = currentEval();
    if (!ev) { P.los.empty('No profile for this point', 'The server returned no terrain profile.', 'ruler'); ui.clear(losOut); return; }
    P.los.ready();
    paintLos();
    const atServerK = Math.abs(ev.k - d.k) < 0.005;
    const res = atServerK ? { clear: d.clear, minClearanceM: d.minClearanceM, minClearanceDistKm: d.minClearanceDistKm, fresnelRatio: d.fresnelRatio, fresnel60Clear: d.fresnel60Clear, obstruction: d.obstruction, edges: d.edges || [], diffractionDb: d.diffractionDb } : ev;
    const pts = decimate(ev.points, 900);
    const X = (km) => Math.round(units.value('dist', km) * 1000) / 1000;
    const Y = (m) => Math.round(units.value('height', m) * 10) / 10;
    const terr = pts.map((p) => [X(p.x), Y(p.eff)]);
    const earth = pts.map((p) => [X(p.x), Y(p.bulge)]);
    const ray = [[X(0), Y(d.rxAntennaM)], [X(pts[pts.length - 1].x), Y(d.tgtAltM)]];
    const fUp = pts.map((p) => [X(p.x), Y(p.ray + p.fres)]);
    const fLo = pts.map((p) => [X(p.x), Y(p.ray - p.fres)]);
    const fBand = [...fUp, ...fLo.slice().reverse()];
    const stationK = serverK();
    const refK = Math.abs(ev.k - stationK) >= 0.005 ? pts.map((p) => [X(p.x), Y(p.terrain + bulgeM(p.x * 1000, (pts[pts.length - 1].x - p.x) * 1000, stationK))]) : null;
    const yVals = [...terr.map((t) => t[1]), ...ray.map((t) => t[1]), ...fUp.map((t) => t[1])];
    const yMax = Math.max(...yVals);
    const yMin = Math.min(0, ...terr.map((t) => t[1]));
    const marks = [];
    if (res.obstruction) {
      const o = res.obstruction;
      const p = ev.points.reduce((a, b) => (Math.abs(b.x - o.distKm) < Math.abs(a.x - o.distKm) ? b : a));
      marks.push({ name: 'Obstruction', coord: [X(o.distKm), Y(p.eff)], symbol: 'pin', symbolSize: 34, itemStyle: { color: C.red }, label: { formatter: '!', color: '#fff', fontWeight: 700 } });
    }
    for (const e of res.edges || []) {
      if (res.obstruction && Math.abs(e.distKm - res.obstruction.distKm) < 0.05) continue;
      const p = ev.points.reduce((a, b) => (Math.abs(b.x - e.distKm) < Math.abs(a.x - e.distKm) ? b : a));
      marks.push({ name: `Edge ${units.db(e.lossDb)}`, coord: [X(e.distKm), Y(p.eff)], symbol: 'diamond', symbolSize: 10, itemStyle: { color: C.amber }, label: { show: true, position: 'top', formatter: units.db(e.lossDb), color: C.amber, fontSize: 10 } });
    }
    const series = [
      { type: 'line', name: 'Terrain + earth bulge', data: terr, color: '#8a9a7b', showSymbol: false, z: 2, lineStyle: { width: 1.2 }, areaStyle: { origin: 'start', color: charts.fade('#6b7f5e', 0.25, 0.85) }, markPoint: marks.length ? { data: marks, animation: false } : undefined },
      { type: 'line', name: 'Earth curvature', data: earth, color: C.blue, showSymbol: false, z: 3, lineStyle: { width: 1, type: 'dashed' }, areaStyle: { origin: 'start', color: 'rgba(11,42,107,0.85)' } },
      {
        type: 'custom', name: 'First Fresnel zone', data: [[0, 0]], z: 4, color: C.cyan, silent: true,
        renderItem(params, api2) {
          return { type: 'polygon', shape: { points: fBand.map((q) => api2.coord(q)) }, style: { fill: 'rgba(34,211,238,0.14)', stroke: 'rgba(34,211,238,0.5)', lineWidth: 0.8, lineDash: [3, 3] } };
        },
      },
      { type: 'line', name: 'Radio path', data: ray, color: res.clear ? '#ffffff' : C.red, showSymbol: true, symbol: 'circle', symbolSize: 7, z: 5, lineStyle: { width: 2 } },
    ];
    if (refK) series.push({ type: 'line', name: `Terrain at station k = ${units.num(stationK, 2, 2)}`, data: refK, color: C.text3, showSymbol: false, z: 3, lineStyle: { width: 1, type: 'dotted' } });
    const nearest = (xv) => { let lo = 0; let hi = pts.length - 1; while (hi - lo > 1) { const m = (lo + hi) >> 1; if (X(pts[m].x) < xv) lo = m; else hi = m; } return Math.abs(X(pts[lo].x) - xv) < Math.abs(X(pts[hi].x) - xv) ? pts[lo] : pts[hi]; };
    P.los.chart.set({
      animation: false,
      grid: { left: 8, right: 16, top: 40, bottom: 26, containLabel: true },
      legend: { show: true, top: 0, left: 0, type: 'scroll', data: series.map((s) => s.name) },
      tooltip: {
        trigger: 'axis', confine: true, axisPointer: { type: 'line' },
        formatter: (ps) => {
          const p = nearest(ps[0].axisValue ?? ps[0].value[0]);
          const c = p.ray - p.eff;
          return [`<b>${units.dist(p.x)}</b> from the antenna`,
            `Terrain ${units.height(p.terrain, { digits: 0 })} + bulge ${units.height(p.bulge, { digits: 0 })}`,
            `Radio path ${units.height(p.ray, { digits: 0 })}`,
            `Clearance <b>${units.height(c, { digits: 0 })}</b>${p.fres > 0 ? ` (${units.pct((c / p.fres) * 100, 0)} of F1 = ${units.height(p.fres, { digits: 0 })})` : ''}`].join('<br>');
        },
      },
      xAxis: { type: 'value', min: 0, max: X(pts[pts.length - 1].x), name: `Distance (${units.label('dist')})`, nameLocation: 'middle', nameGap: 22, splitLine: { show: false } },
      yAxis: { type: 'value', min: Math.floor(yMin), max: Math.ceil(yMax * 1.04), name: `${units.label('height')} AMSL`, axisLabel: { formatter: (v) => units.num(v, 0) } },
      series,
    }, { replace: true });

    // verdict + numbers
    const verdict = !res.clear ? ['Blocked by terrain', 'red', 'mountain'] : !res.fresnel60Clear ? ['Fresnel zone obstructed', 'amber', 'alert'] : ['Clear line of sight', 'green', 'check-circle'];
    const diff = res.diffractionDb || 0;
    const total = isNum(d.fsplDb) ? d.fsplDb + (d.gasDb || 0) + diff : null;
    const sens = curAnt()?.sensitivityDbm;
    const txDbm = 53.98; // 250 W transponder, same reference as the link budget
    const gain = (config.current?.antennas || []).find((a) => a.id === (d.antennaId || T.ant))?.gainDbi;
    const margin = isNum(total) && isNum(sens) ? txDbm + (isNum(gain) ? gain : 0) - total - sens : null;
    const text = !res.clear
      ? `The terrain ${units.dist(res.obstruction?.distKm)} out rises ${units.height(res.obstruction?.aboveRayM, { digits: 0 })} above the direct path. The signal only arrives by diffraction over the ridge, costing about ${units.db(diff, 0)} - ${isNum(margin) && margin > 0 ? 'still enough for reception with a strong transponder' : 'usually too much for reliable reception'}. The aircraft becomes visible from about ${floorText(ev, d)} at this distance.`
      : !res.fresnel60Clear
        ? `There is a direct line, but terrain reaches into the first Fresnel zone (${units.pct((res.fresnelRatio || 0) * 100, 0)} clearance at the tightest point; ${units.pct(60)} is needed). Expect ${units.db(diff, 1)} extra loss and weaker, fading signals.`
        : `The path clears the terrain by at least ${units.height(res.minClearanceM, { digits: 0 })}, with ${units.pct((res.fresnelRatio || 0) * 100, 0)} of the first Fresnel zone free at the tightest point - free-space conditions.`;
    ui.mount(losOut,
      h('div.row.tl-verdict', ui.icon(verdict[2]), ui.badge(verdict[0], verdict[1], { large: true }),
        !atServerK ? ui.badge(T.kUnsupported ? `client estimate for k = ${units.num(ev.k, 2, 2)}` : `estimate for k = ${units.num(ev.k, 2, 2)} · updating…`, 'violet', { tip: T.kUnsupported ? 'This server computes the path only for the station k-factor; with another k the clearance and diffraction loss are re-estimated in the browser.' : 'Re-estimated in the browser while the server computes the exact profile for this k.' })
          : Math.abs(d.k - stationK) >= 0.005 ? ui.badge(`computed for k = ${units.num(d.k, 2, 2)}`, 'cyan', { tip: `Exact server profile for this k-factor (the station uses k = ${units.num(stationK, 2, 2)}).` }) : null),
      h('p.al-text', text),
      ui.kv([
        ['Target', `${units.bearing(d.bearingDeg)} · ${units.dist(d.groundKm)} · ${units.alt(d.tgtAltM / 0.3048, { fl: true })}`],
        ['Elevation of the aircraft', degFmt(d.elevationDeg)],
        ['Least clearance', isNum(res.minClearanceM) ? `${units.height(res.minClearanceM, { digits: 0 })} at ${units.dist(res.minClearanceDistKm)}` : units.DASH],
        ['Fresnel clearance', !res.clear ? 'none (path blocked)' : isNum(res.fresnelRatio) ? `${units.pct(res.fresnelRatio * 100, 0)} of F1` : units.DASH, { tip: 'Clearance divided by the radius of the first Fresnel zone at the tightest point. 60 % or more = no extra loss.' }],
        ['Diffraction loss', `${units.db(diff, 1)}${(res.edges || []).length ? ` (${(res.edges || []).length} edge${res.edges.length === 1 ? '' : 's'})` : ''}`],
        ['Path loss', isNum(total) ? `${units.db(total, 1)} (free space ${units.db(d.fsplDb, 1)}, gases ${units.db(d.gasDb, 1)})` : units.DASH],
        isNum(margin) ? ['Margin (250 W transponder)', units.db(margin, 1), { tip: 'Transmit power + antenna gain - path loss - receiver sensitivity of this antenna, as in the terrain radio model' }] : null,
        d.noDataSamples > 0 ? ['Missing elevation', `${units.int(d.noDataSamples)} of ${units.int(d.samples)} samples read as sea level`] : null,
      ], { columns: 2 }));
    P.los.card.setSub(`${units.bearing(T.q.brg)} · ${units.dist(T.q.km)} · ${units.alt(T.q.altFt, { fl: true })}`);
  }
  /** Lowest altitude at which the target point would be in line of sight (bisection on the evaluated profile). */
  function floorText(ev, d) {
    const pts = ev.points;
    const D = pts[pts.length - 1].x;
    if (!(D > 0)) return units.DASH;
    // the needed end height is set by the point that requires the steepest ray
    let need = -Infinity;
    for (let i = 1; i < pts.length - 1; i++) {
      const p = pts[i];
      if (p.x < 0.3) continue;
      const slope = (p.eff - d.rxAntennaM) / p.x;
      need = Math.max(need, d.rxAntennaM + slope * D);
    }
    return Number.isFinite(need) ? units.alt(Math.max(0, need) / 0.3048, { fl: true }) : units.DASH;
  }
  function renderKInfo() {
    const k = kNow();
    const a = curAnt();
    const hAnt = a?.antennaM ?? T.los.data?.rxAntennaM;
    const hAc = T.q.altFt * 0.3048;
    const rEff = effectiveRadiusKm(k);
    const mid = T.q.km * 500;
    const reach = isNum(hAnt) ? horizonKm(hAnt, k) + horizonKm(hAc, k) : null;
    const reach1 = isNum(hAnt) ? horizonKm(hAnt, 1) + horizonKm(hAc, 1) : null;
    ui.mount(kInfo,
      ui.kv([
        ['Effective earth radius', units.dist(rEff, { digits: 0 })],
        ['Earth bulge at mid-path', units.height(bulgeM(mid, mid, k), { digits: 0 })],
        ['Smooth-earth reach', isNum(reach) ? `${units.dist(reach)} at ${units.alt(T.q.altFt, { fl: true })}` : units.DASH, { tip: 'Antenna horizon + aircraft horizon over a smooth sea-level earth' }],
      ]),
      h('p.al-text.muted',
        'Air gets thinner with height, so radio waves bend slightly downwards and follow the earth\'s curve a little. Planners model this by pretending the earth is larger: k times its real radius. ',
        Math.abs(k - 1) < 0.01 ? 'At k = 1 there is no bending at all: radio behaves like light in vacuum'
          : `At k = ${units.num(k, 2, 2)} the earth "looks" ${k > 1 ? units.num(k, 2, 2) + ' times larger' : 'smaller'} than it is`,
        isNum(reach) && isNum(reach1) && Math.abs(k - 1) > 0.01 ? `, which moves the reach ${reach >= reach1 ? 'out' : 'in'} by ${units.dist(Math.abs(reach - reach1))} compared with straight (geometric) rays. ` : '. ',
        'Inversions and humid air raise k (sometimes into ducting, see the ',
        h('a', { href: '#/forecast' }, 'propagation forecast'),
        '); the coverage panels above always use the station setting.'));
  }

  // ---- orchestration -------------------------------------------------------------------------
  function renderAll() {
    safe('head', renderHead);
    safe('job', renderJob);
    safe('pano', renderPano);
    safe('polar', renderPolar);
    safe('map', renderMap);
    safe('advice', renderAdvice);
    safe('los', drawLos);
    safe('rf', renderRf);
  }

  function tick() {
    if (!T.alive || !T.visible || document.visibilityState !== 'visible') return;
    const running = T.status.data?.job?.state === 'running';
    if (running || Date.now() - T.lastStatus >= STATUS_IDLE_MS) pollStatus();
    if (Date.now() - T.lastRf >= RF_MS) pollRf();
  }
  ctx.every(1000, tick);

  // initial
  syncK();
  renderAll();

  return {
    shown() {
      T.visible = true;
      if (!T.lastStatus) pollStatus(true);
      if (!T.lastRf) pollRf();
      if (T.cov.data) ensureMap().catch(() => { /* reported by renderMap */ });
      if (T.map) requestAnimationFrame(() => { try { T.map?.resize(); } catch { /* gone */ } });
    },
    hidden() { T.visible = false; },
    measuredChanged() {
      let antChanged = false;
      if (T.cov.data) { const before = T.ant; pickAntenna(); antChanged = before !== T.ant; if (antChanged) scheduleLos(0); }
      safe('head', renderHead); safe('pano', renderPano); safe('polar', renderPolar); safe('map', paintMap);
      if (antChanged) safe('advice', renderAdvice);
    },
    unitsChanged() {
      for (const s of Object.values(sl)) s.refresh();
      antSeg.dataset.key = '';
      if (T.cov.data) pickAntenna();
      renderAll();
      paintMap();
    },
  };
}

function median(arr) {
  const v = arr.filter((x) => typeof x === 'number' && Number.isFinite(x)).sort((a, b) => a - b);
  if (!v.length) return null;
  const m = v.length >> 1;
  return v.length % 2 ? v[m] : (v[m - 1] + v[m]) / 2;
}

/** Keep longitudes continuous so rings across the antimeridian draw correctly. */
function unwrapRing(ring) {
  for (let i = 1; i < ring.length; i++) {
    const d = ring[i][0] - ring[i - 1][0];
    if (d > 180) ring[i][0] -= 360 * Math.round(d / 360);
    else if (d < -180) ring[i][0] += 360 * Math.round(-d / 360);
  }
  return ring;
}
