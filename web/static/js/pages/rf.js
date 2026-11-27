// pages/rf.js - RF Lab: a radio engineer's view of the 1090 MHz receive chain.
//
// Data (all GET; every section fetches only while its tab is visible and the browser tab is not hidden):
//   /api/rf/summary          headline tiles (5 s, always)
//   /api/rf/health           overview + gain verdict (10 s)          /api/rf/history   hourly trends (60 s)
//   /api/rf/signal           RSSI, floors, aspect, fading, multipath  /api/rf/detection Pd curves, update gaps
//   /api/rf/channel          occupancy, DF mix, CRC, garble model     /api/rf/interrogators  II/SI census, Comm-B, ACAS
//   /api/rf/timing           GPS squitter timing                      /api/rf/fleet     equipage + GNSS interference
//   /api/prediction/stats    prediction skill
// All /api/rf/* answer 409 when config.rfStat.enabled is off; /api/prediction/* when prediction is off.
// Sub-routes: #/rf/<section>  (health signal detection channel interrogators timing fleet prediction)

import { makeEnv, ok, statusOf, scoreStatus } from '../rf/common.js';
import health from '../rf/health.js';
import signal from '../rf/signal.js';
import detection from '../rf/detection.js';
import channel from '../rf/channel.js';
import interrogation from '../rf/interrogation.js';
import timing from '../rf/timing.js';
import fleet from '../rf/fleet.js';
import prediction from '../rf/prediction.js';

const SECTIONS = [health, signal, detection, channel, interrogation, timing, fleet, prediction];
const SUMMARY = { path: '/api/rf/summary', every: 5000 };
const TIMEOUT = 20000;

let instance = null; // the mounted page, for onRoute()

export default {
  id: 'rf',
  title: 'RF Lab',

  async mount(container, ctx) {
    const { api, units, ui } = ctx;
    const { h } = ui;
    await ui.loadCss(new URL('../../css/rf.css', import.meta.url));

    const store = new Map(); // key -> { data, error, disabled, t, busy, every, path, params }
    let alive = true;
    let active = null;
    ctx.onCleanup(() => { alive = false; if (instance && instance.ctx === ctx) instance = null; });

    const env = makeEnv(ctx, () => refresh(true));
    const { f } = env;

    // ---- header -------------------------------------------------------------------------------
    const subEl = h('div.sub', 'Signal, detection, channel load and interrogators of the 1090 MHz receive chain');
    const liveDot = h('span.rf-live', { tip: 'Statistics refresh automatically while this page is open' });
    const refreshBtn = ui.button({ icon: 'refresh', variant: 'ghost', tip: 'Refresh now', onClick: () => refresh(true) });
    const reportBtn = ui.button({ icon: 'download', label: 'Report', size: 'sm', variant: 'ghost', href: '/api/rf/report', target: '_blank', tip: 'The complete RF statistics as one JSON document' });
    const banner = h('div.rf-banner.hidden');

    // ---- headline tiles -----------------------------------------------------------------------
    const T = {
      health: ui.stat({ label: 'RF health', icon: 'bolt', accent: 'green', tip: 'Overall score 0–100 of the receive chain (see Overview)' }),
      fps: ui.stat({ label: 'Frame rate', icon: 'chart', tip: 'Decoded Mode S frames per second, averaged over the statistics window' }),
      occ: ui.stat({ label: 'Channel occupancy', icon: 'wave', accent: 'cyan', tip: 'Share of time 1090 MHz carries a decoded reply' }),
      floor: ui.stat({ label: 'Noise floor (est.)', icon: 'signal', accent: 'violet', tip: 'Estimated: decode floor minus an assumed decoder SNR - not a direct measurement' }),
      pd: ui.stat({ label: 'Position Pd', icon: 'target', accent: 'cyan', tip: 'Share of transmitted position squitters that were decoded' }),
      radars: ui.stat({ label: 'Mode S radars', icon: 'antenna', accent: 'amber', tip: 'Interrogator codes (other than II 0) with at least 10 replies in 10 minutes' }),
      gnss: ui.stat({ label: 'GNSS interference', icon: 'globe', tip: 'Share of ADS-B aircraft with collapsed navigation integrity right now' }),
    };
    const tilesEl = h('div.rf-tiles', Object.values(T));

    function setAccent(tile, level) {
      for (const c of ['stat--green', 'stat--amber', 'stat--red']) tile.classList.remove(c);
      if (level) tile.classList.add('stat--' + level);
    }

    function paintTiles() {
      const e = store.get('summary');
      const s = e && e.data;
      if (!s) {
        if (e && (e.error || e.disabled)) for (const t of Object.values(T)) { t.set(null, ''); t.setSub(e.disabled ? 'switched off' : 'unavailable'); }
        return;
      }
      const hasH = ok(s.health) && s.health >= 0;
      T.health.set(hasH ? units.int(s.health) : null, hasH ? '/100' : '');
      T.health.setSub(hasH ? statusOf(scoreStatus(s.health)).label : 'collecting data');
      setAccent(T.health, hasH ? { ok: 'green', warn: 'amber', bad: 'red' }[scoreStatus(s.health)] : null);
      T.fps.set(ok(s.fps) ? units.num(s.fps, s.fps < 100 ? 1 : 0) : null, ok(s.fps) ? '/s' : '');
      T.fps.setSub(`CRC failures ${f.frac(s.failFrac, 1)}`);
      T.occ.set(ok(s.occupancyPct) ? units.num(s.occupancyPct, 2) : null, ok(s.occupancyPct) ? '%' : '');
      T.occ.setSub(`peak ${f.pct(s.peakOccupancyPct, 2)} (10 min)`);
      T.floor.set(ok(s.noiseFloorEstDbfs) ? units.num(s.noiseFloorEstDbfs, 1) : null, ok(s.noiseFloorEstDbfs) ? 'dBFS' : '');
      T.floor.setSub(`decode floor ${f.dbfs(s.decodeFloorDbfs)} · margin ${f.db(s.snrMarginDb)}`);
      T.pd.set(ok(s.pdPos) ? units.num(s.pdPos * 100, 1) : null, ok(s.pdPos) ? '%' : '');
      T.pd.setSub(ok(s.range90Km) ? `90 % to ${units.dist(s.range90Km)}` : '90 % range: not enough data');
      T.radars.set(units.int(s.activeRadars), '');
      T.radars.setSub(`${units.int(s.adsbAircraft)} ADS-B aircraft`);
      T.gnss.set(ok(s.gnssIndexNow) ? units.num(s.gnssIndexNow, 1) : null, ok(s.gnssIndexNow) ? '%' : '');
      T.gnss.setSub(ok(s.gnssIndexNow) ? (s.gnssIndexNow >= 10 ? 'aircraft GNSS degraded' : 'normal') : 'no integrity data yet');
      setAccent(T.gnss, ok(s.gnssIndexNow) ? (s.gnssIndexNow >= 20 ? 'red' : s.gnssIndexNow >= 5 ? 'amber' : 'green') : null);
      const win = ok(s.windowSec) ? units.age(s.windowSec) : units.DASH;
      ui.setText(subEl, `Sliding window ${win} of up to 1 h · updated ${units.clock(s.t)}${s.timingAvailable ? ' · GPS timestamps' : ''}`);
    }

    function paintBanner() {
      const e = store.get('summary');
      let node = null;
      if (e && e.disabled) {
        node = [ui.icon('power'), h('div.grow', h('b', 'RF statistics are switched off'), h('span', 'Turn on "RF statistics" (rfStat) in Settings to collect signal, detection and channel statistics. Prediction skill is independent of it.')),
          ui.button({ label: 'Open settings', icon: 'settings', size: 'sm', href: '#/settings' })];
      } else if (e && e.error && !e.data) {
        node = [ui.icon('alert'), h('div.grow', h('b', 'The station does not answer'), h('span', e.error.message || 'Network error'))];
      }
      banner.classList.toggle('hidden', !node);
      if (node) ui.mount(banner, node); else ui.clear(banner);
      liveDot.classList.toggle('off', !!node);
    }

    // ---- sections -----------------------------------------------------------------------------
    const built = new Map(); // id -> { def, view }
    const tabs = ui.tabs({
      tabs: SECTIONS.map((s) => ({
        id: s.id, label: s.label, icon: s.icon,
        render(panelEl) {
          const view = s.build(env);
          built.set(s.id, { def: s, view });
          panelEl.append(view.el);
        },
        onShow() { const b = built.get(s.id); if (b && b.view.onShow) b.view.onShow(); },
      })),
      active: pickSection(ctx.route),
      onChange: (id) => { select(id); ctx.navigate('rf/' + id, { replace: true }); },
    });
    tabs.classList.add('rf-tabs');

    function pickSection(route) {
      const id = route && route.args && route.args[0];
      return SECTIONS.some((s) => s.id === id) ? id : 'health';
    }

    function sectionState(def) {
      const st = {};
      for (const k of Object.keys(def.endpoints)) st[k] = store.get(k);
      return st;
    }

    function renderActive() {
      const b = built.get(active);
      if (!b || !alive) return;
      try { b.view.render(sectionState(b.def)); } catch (e) { console.error('[rf] section render failed', e); }
    }

    function select(id) {
      if (active === id) return;
      active = id;
      if (tabs.active !== id) tabs.select(id);
      renderActive();
      refresh(false);
    }

    // ---- fetching -----------------------------------------------------------------------------
    async function fetchOne(key, ep) {
      let e = store.get(key);
      if (!e) { e = { data: null, error: null, disabled: null, t: 0, busy: false }; store.set(key, e); }
      if (e.busy) return false;
      e.busy = true;
      try {
        const data = await api.get(ep.path, { params: ep.params, signal: ctx.signal, timeout: TIMEOUT });
        e.data = data; e.error = null; e.disabled = null;
      } catch (err) {
        if (err && err.aborted) { e.busy = false; return false; }
        if (err && err.status === 409) {
          e.data = null;
          e.disabled = ep.path.startsWith('/api/prediction/') ? 'Trajectory prediction is switched off in Settings.' : 'RF statistics are switched off in Settings (rfStat).';
        } else {
          e.error = err;
        }
      }
      e.t = Date.now();
      e.busy = false;
      return alive;
    }

    function due(key, ep, force) {
      const e = store.get(key);
      if (!e) return true;
      if (e.busy) return false;
      if (force) return true;
      const every = e.disabled ? Math.max(ep.every, 30000) : e.error ? Math.min(ep.every, 15000) : ep.every;
      return Date.now() - e.t >= every;
    }

    function refresh(force) {
      if (!alive || (document.hidden && !force)) return;
      if (due('summary', SUMMARY, force)) fetchOne('summary', SUMMARY).then((ch) => { if (ch) { paintTiles(); paintBanner(); } });
      const b = built.get(active);
      if (!b) return;
      for (const [k, ep] of Object.entries(b.def.endpoints)) {
        if (!due(k, ep, force)) continue;
        // A 409 on the summary means every /api/rf/* endpoint is off: do not hammer them.
        const sum = store.get('summary');
        if (!force && sum && sum.disabled && ep.path.startsWith('/api/rf/') && store.get(k)) continue;
        fetchOne(k, ep).then((ch) => { if (ch && active === b.def.id) renderActive(); });
      }
      if (force) renderActive();
    }

    ctx.every(1000, () => refresh(false));
    const onVis = () => { if (!document.hidden) refresh(false); };
    document.addEventListener('visibilitychange', onVis);
    ctx.onCleanup(() => document.removeEventListener('visibilitychange', onVis));
    ctx.onCleanup(units.onChange(() => { paintTiles(); renderActive(); }));
    ctx.subscribe('state', (s) => liveDot.classList.toggle('off', s === 'offline'), { immediate: true });

    container.append(h('div.page-inner.rf',
      h('div.page-head',
        h('div', h('h2', 'RF Lab'), subEl),
        h('div.actions', liveDot, reportBtn, refreshBtn)),
      banner,
      tilesEl,
      tabs));

    instance = { ctx, select: (id) => select(id), pick: pickSection };
    active = null;
    select(tabs.active || pickSection(ctx.route));
    paintTiles();
  },

  onRoute(route) {
    if (instance) instance.select(instance.pick(route));
  },

  unmount() {
    instance = null;
  },
};
