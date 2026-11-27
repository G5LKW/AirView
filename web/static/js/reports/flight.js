// reports/flight.js - one stored flight: facts, guessed departure / arrival airports, a mini map of the stored
// trace (aircraft/trailmap.js, read-only import), altitude / speed / signal profiles and KML / GeoJSON / CSV
// downloads. Data: GET /api/history/flights/{id}/trace.

import { createTrailMap } from '../aircraft/trailmap.js';
import { fin, srcMaskLabels } from './common.js';

/**
 * @param {HTMLElement} host
 * @param ctx page context
 * @param scope view scope
 * @param {string|number} id flight id
 */
export function mountFlight(host, ctx, scope, id) {
  const { ui, units, api, config } = ctx;
  const { h } = ui;
  const fid = String(id || '').replace(/[^0-9]/g, '');
  const back = ui.button({ label: 'Results', icon: 'list', variant: 'ghost', size: 'sm', onClick: () => ctx.navigate('reports') });
  const head = h('div.rpf-head', back, h('div.rpf-title', ui.skeleton({ lines: 1, title: true })));
  const body = h('div.rpf-body');
  host.append(h('div.rpf', head, body));
  if (!fid) { ui.mount(body, ui.empty({ icon: 'search', title: 'No flight selected', text: 'Pick a flight in the report.' })); return; }

  let trace = null;
  let mapApi = null;
  let mode = 'altitude';
  const charts = [];

  async function load() {
    ui.mount(body, h('div.rpf-grid', ui.card({ body: ui.skeleton({ block: 300, lines: 0 }) }), ui.card({ body: ui.skeleton({ lines: 10 }) })));
    try {
      trace = await api.get(`/api/history/flights/${fid}/trace`, { signal: scope.signal, timeout: 30000 });
    } catch (e) {
      if (e.aborted) return;
      ui.mount(head.lastChild, h('h3', 'Flight ' + fid));
      ui.mount(body, e.status === 404 ? ui.empty({ icon: 'search', title: 'Flight not found', text: 'It may have been pruned from the database by the retention settings.' })
        : ui.error(e, { title: 'The flight could not be loaded', retry: load }));
      return;
    }
    render();
  }

  function render() {
    const f = trace.flight || {};
    const pts = trace.points || [];
    if (mapApi) { mapApi.remove(); mapApi = null; }
    ctx.setTitle(`Flight ${f.callsign || f.icao || fid}`);
    const dl = (ext, label) => h('a.btn.btn--sm', { href: `/api/export/flight/${fid}.${ext}`, download: '', tip: `Download ${label}` }, ui.icon('download'), h('span', label));
    ui.mount(head,
      back,
      h('div.rpf-title',
        h('h3', h('span.num', f.callsign || f.icao), f.callsign ? h('span.rpf-icao.num', f.icao) : null),
        h('div.rpf-sub.muted', [f.operator, f.country, `${units.dateTime(f.start)} – ${units.clock(f.end)}`].filter(Boolean).join(' · '))),
      h('div.rpf-actions',
        f.icao ? ui.button({ label: 'Live Map', icon: 'map', size: 'sm', variant: 'ghost', href: '#/map/' + f.icao, tip: 'Open the aircraft on the Live Map (if it is still around)' }) : null,
        dl('kml', 'KML'), dl('geojson', 'GeoJSON'), dl('csv', 'CSV')));

    // ---- facts ----
    const srcs = srcMaskLabels(f.srcMask || 0);
    const facts = ui.kv([
      ['ICAO', f.icao || units.DASH],
      ['Callsign', f.callsign || units.DASH],
      ['Squawk', f.squawk || units.DASH],
      ['First seen', units.dateTime(f.start)],
      ['Last seen', units.dateTime(f.end)],
      ['Duration', fin(f.start) && fin(f.end) ? units.duration(f.end - f.start) : units.DASH],
      ['Messages', units.int(f.msgs)],
      ['Positions', units.int(f.positions)],
      ['Sources', srcs.length ? h('span.rpb-flags', srcs.map(([k, l]) => ui.badge(l, k))) : units.DASH],
      ['Farthest', fin(f.maxDistKm) ? `${units.dist(f.maxDistKm)} · ${units.bearing(f.maxDistBrg)}` : units.DASH],
      ['Closest', units.dist(f.minDistKm)],
      ['Altitude', fin(f.minAlt) || fin(f.maxAlt) ? `${units.format('alt', f.minAlt, { unit: false })} – ${units.alt(f.maxAlt)}` : units.DASH],
      ['Max speed', units.speed(f.maxGs)],
      ['Signal', fin(f.rssiMax) ? `max ${units.dbfs(f.rssiMax)} · avg ${units.dbfs(f.rssiAvg)}` : units.DASH],
      f.emergency ? ['Emergency', h('span.c-red', String(f.emergency))] : null,
    ]);
    const ap = trace.airports || {};
    const apRow = (label, g) => h('div.rpf-ap',
      h('div.rpf-ap-l', label),
      g && g.airport ? h('div.rpf-ap-v',
        h('b.num', g.airport.icao || g.airport.ident || g.airport.iata || '?'), ' ', h('span', g.airport.name || ''),
        h('div.muted.rpf-ap-s', [g.runway ? 'runway ' + g.runway : null, `${units.pct((g.confidence || 0) * 100)} confidence`, g.reason].filter(Boolean).join(' · ')))
        : h('div.rpf-ap-v.muted', 'not seen low near an airport'));
    const apCard = ui.card({ title: 'Airports', icon: 'pin', sub: 'Guessed from where the flight was first and last seen', body: h('div.col.gap-3', apRow('Departure', ap.departure), apRow('Arrival', ap.arrival)) });

    // ---- map ----
    const mapEl = h('div.rpf-map');
    const mapState = h('div.rpf-map-state');
    const modeSeg = ui.segmented({ options: [{ value: 'altitude', label: 'Altitude' }, { value: 'signal', label: 'Signal' }], value: mode, onChange: (v) => { mode = v; drawMap(); } });
    const mapCard = ui.card({ title: 'Stored trace', icon: 'route', sub: `${units.int(pts.length)} positions`, actions: modeSeg, body: h('div.rpf-map-wrap', mapEl, mapState), flush: true });

    // ---- profiles ----
    const altEl = h('div.chart.rpf-chart');
    const spdEl = h('div.chart.rpf-chart');
    const sigEl = h('div.chart.rpf-chart');
    const profCard = ui.card({ title: 'Profiles', icon: 'chart', sub: 'Altitude, ground speed and signal over the flight', body: h('div.rpf-profiles',
      h('div.rpf-prof', h('div.rpf-prof-l', 'Altitude · ' + units.label('alt')), altEl),
      h('div.rpf-prof', h('div.rpf-prof-l', 'Ground speed · ' + units.label('speed')), spdEl),
      h('div.rpf-prof', h('div.rpf-prof-l', 'Signal · dBFS'), sigEl),
      h('p.muted', { style: 'margin:8px 0 0;font-size:var(--fs-sm)' }, 'How the flight looked from this station over time: climb and descent, speed, and how strongly it was received (nearer 0 dBFS is stronger). Gaps are stretches with no stored position.')) });

    ui.mount(body, h('div.rpf-grid', mapCard, h('div.rpf-side', ui.card({ title: 'Flight', icon: 'plane', body: facts }), apCard)), profCard);

    // map
    const trail = pts.map((p) => [p[0], p[1], p[2], p[3], p[4], p[6], p[7]]);   // -> [t, lat, lon, alt, gs, rssi, src]
    const rx = config.current?.receiver?.located ? config.current.receiver : null;
    const far = (() => {
      if (!rx || !trail.length) return null;
      let best = null; let bd = -1;
      for (const p of trail) { const d = ctx.geo.distanceKm(rx.lat, rx.lon, p[1], p[2]); if (d > bd) { bd = d; best = p; } }
      return best ? { lat: best[1], lon: best[2] } : null;
    })();
    function drawMap() { if (mapApi) mapApi.set({ key: fid, trail, mode, receiver: rx, far }); }
    if (!trail.length) {
      ui.mount(mapState, ui.empty({ icon: 'route', title: 'No stored positions', text: 'This flight was received without positions (Mode S only), or its trail was pruned.', compact: true }));
    } else {
      ui.mount(mapState, ui.loading('Loading map…', { compact: true }));
      createTrailMap(mapEl, ctx, {
        describe: (p) => `${units.clock(p[0])} · ${units.alt(p[3])} · ${units.speed(p[4])} · ${units.dbfs(p[5])}`,
      }).then((m) => {
        if (!scope.alive) { m.remove(); return; }
        mapApi = m;
        scope.onCleanup(() => m.remove());
        ui.clear(mapState);
        mapState.classList.add('hidden');
        drawMap();
      }).catch((e) => {
        console.error('[reports] trace map failed', e);
        ui.mount(mapState, ui.error(new Error('The map could not be started (' + (e?.message || 'WebGL unavailable') + ').'), { compact: true }));
      });
    }

    // charts
    for (const c of charts.splice(0)) c.dispose();
    const series = (idx, kind) => pts.filter((p) => fin(p[idx])).map((p) => [p[0], kind ? units.value(kind, p[idx]) : p[idx]]);
    const prof = (el, data, color, name, fmt) => {
      if (data.length < 2) { ui.mount(el, ui.empty({ icon: 'chart', title: 'Not enough data yet', text: 'Fewer than two values were stored.', compact: true })); el.classList.add('is-empty'); return; }
      const c = ctx.charts.create(el);
      charts.push(c);
      c.set(lineOpt(ctx, data, color, name, fmt));
    };
    const C = ctx.charts.colors || {};
    prof(altEl, series(3, 'alt'), C.cyan || '#22d3ee', 'Altitude', (v) => units.format('alt', units.toBase('alt', v)));
    prof(spdEl, series(4, 'speed'), C.amber || '#fbbf24', 'Ground speed', (v) => units.format('speed', units.toBase('speed', v)));
    prof(sigEl, series(6, null), C.violet || '#a78bfa', 'Signal', (v) => units.dbfs(v));
  }

  scope.onCleanup(units.onChange(() => { if (trace) render(); }));
  scope.onCleanup(() => { for (const c of charts.splice(0)) c.dispose(); });
  load();
}

function lineOpt(ctx, data, color, name, fmt) {
  const { units } = ctx;
  const opt = ctx.charts.line({ series: [{ name, data, color, area: 0.22 }], valueFormat: fmt, yScale: true, legend: false });
  opt.grid = { left: 6, right: 10, top: 10, bottom: 4, containLabel: true };
  opt.xAxis = { ...(opt.xAxis || {}), type: 'time', axisLabel: { formatter: (v) => units.clock(v, { seconds: false }) } };
  opt.tooltip = { ...(opt.tooltip || {}), trigger: 'axis', confine: true, formatter: (ps) => {
    const p = Array.isArray(ps) ? ps[0] : ps;
    return p ? `${units.clock(p.value[0])}<br><b>${fmt(p.value[1])}</b>` : '';
  } };
  return opt;
}
