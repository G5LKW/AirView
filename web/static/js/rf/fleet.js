// rf/fleet.js - RF Lab "Fleet & GNSS": ADS-B version / NIC / NACp / SIL distributions, emitter categories,
// GNSS interference index over time with a map of affected cells, and GNSS-minus-baro altitude.
// /api/rf/fleet carries the GNSS series and cells as well, so this tab needs only that one request.

import { ok, histPoints, histTotal, anyNum } from './common.js';
import { createGnssMap, INDEX_RAMP } from './gnssmap.js';

const CATEGORY = {
  A1: 'Light (< 7 t)', A2: 'Small (7–34 t)', A3: 'Large (34–136 t)', A4: 'High-vortex large (B757)', A5: 'Heavy (> 136 t)',
  A6: 'High performance', A7: 'Rotorcraft', B1: 'Glider', B2: 'Lighter than air', B3: 'Parachutist', B4: 'Ultralight',
  B6: 'UAV', B7: 'Space vehicle', C1: 'Emergency vehicle', C2: 'Service vehicle', C3: 'Obstacle',
};

/** Server band label in feet ("<1000", "20-30k", ">40k") -> the user's altitude unit. */
function altBand(s, units) {
  const m = /^([<>])?\s*(\d+(?:\.\d+)?)(k)?(?:\s*[-–]\s*(\d+(?:\.\d+)?)(k)?)?$/.exec(String(s || '').trim());
  if (!m) return s || '';
  const range = m[4] !== undefined;
  const lo = Number(m[2]) * (m[3] || (range && m[5]) ? 1000 : 1);
  if (range) return `${units.format('alt', lo, { unit: false })}–${units.alt(Number(m[4]) * (m[5] ? 1000 : 1))}`;
  return `${m[1] === '<' ? '< ' : m[1] === '>' ? '> ' : ''}${units.alt(lo)}`;
}

export default {
  id: 'fleet',
  label: 'Fleet & GNSS',
  icon: 'globe',
  endpoints: {
    fleet: { path: '/api/rf/fleet', every: 30000 },
  },

  build(env) {
    const { h, ui, units, charts, C, f, panel, show, notEnough } = env;
    const { ctx } = env;
    let map = null;
    let mapPending = false;
    ctx.onCleanup(() => { if (map) { map.remove(); map = null; } });

    const P = {};
    const eqHosts = { versions: h('div.chart'), nic: h('div.chart'), nacp: h('div.chart'), sil: h('div.chart') };
    const eqCharts = {};
    const eqKv = h('div');
    const EQ = [
      ['versions', 'ADS-B version', 'v0 = DO-260, v1 = DO-260A, v2 = DO-260B (mandatory in Europe and the US). Version 2 reports integrity correctly; v0 cannot.'],
      ['nic', 'NIC (containment)', 'Navigation Integrity Category: radius the true position is guaranteed to be inside. 7 = 0.2 nm, 8 = 0.1 nm, 10+ = 25 m or better.'],
      ['nacp', 'NACp (accuracy)', 'Navigation Accuracy Category for position: 8 = 93 m, 9 = 30 m, 10 = 10 m, 11 = 3 m (95 %).'],
      ['sil', 'SIL (integrity level)', 'Source Integrity Level: chance the containment radius is exceeded without warning. 3 = 1 in 10 million per hour, the norm for GNSS.'],
    ];
    P.equip = panel({
      title: 'Equipage census', icon: 'plane', class: 'span-2',
      content: h('div.rf-eq', EQ.map(([k, title, tip]) => h('div.rf-eq-cell', h('div.rf-eq-title', { tip }, title), eqHosts[k]))),
      caption: 'What the ADS-B fleet you hear transmits, per aircraft (share of ADS-B aircraft in the window). "Poor integrity" = NACp below 8, NIC below 7 or SIL below 3: such positions should not be trusted for separation. TC28/29/31 = aircraft that also send status, target-state and operational-status messages.',
    });
    P.equip.extra.append(eqKv);
    P.cat = panel({
      title: 'Emitter categories', icon: 'layers', height: 240,
      caption: 'The wake-turbulence / vehicle category every aircraft broadcasts about itself. A3 (airliners like A320 / B737) and A5 (wide-bodies) dominate near airways.',
    });
    P.gnss = panel({
      title: 'GNSS interference index', icon: 'alert', height: 240,
      caption: 'Share of ADS-B aircraft whose navigation integrity suddenly collapsed (NACp or NIC dropping from good to below 5) per minute over 24 h. Near 0 % is normal. Sustained values show GPS jamming or spoofing affecting aircraft in your area - it is the aircraft\'s receivers that are affected, not yours.',
    });
    const mapEl = h('div.rf-map');
    const mapWrap = h('div.rf-mapwrap', mapEl);
    const mapLegend = h('div.rf-maplegend', h('span.muted', 'Index'),
      h('span.rf-leg-bar', { style: { background: `linear-gradient(90deg, ${INDEX_RAMP.map(([v, c]) => `${c} ${(v / 35) * 100}%`).join(', ')})` } }),
      h('span.num', units.pct(0, 0)), h('span.num', units.pct(35, 0) + '+'));
    P.map = panel({
      title: 'Where integrity degrades', icon: 'map', content: mapWrap,
      caption: 'Polar cells of 10° × 50 km around the station, coloured by the interference index of the aircraft in them (brighter = more aircraft-minutes). Jamming shows up as a cluster of orange / red cells in one direction.',
    });
    P.map.extra.append(mapLegend);
    const deltaKv = h('div');
    P.delta = panel({
      title: 'GNSS versus barometric altitude', icon: 'mountain', height: 220,
      caption: 'Aircraft report both a pressure altitude and a GNSS height. Their difference follows the weather (warm air = GNSS higher) and is the same for all aircraft at one level; single aircraft far off, or jumps, point at faulty or spoofed navigation.',
    });
    P.delta.extra.append(deltaKv);

    // ------------------------------------------------------------------------------------------
    function countChart(key, list, color) {
      if (!eqCharts[key]) eqCharts[key] = charts.create(eqHosts[key]);
      const rows = (list || []).filter((c) => c.count > 0);
      eqCharts[key].set({
        animation: false,
        grid: { left: 4, right: 8, top: 8, bottom: 4, containLabel: true },
        tooltip: { trigger: 'axis', confine: true, axisPointer: { type: 'shadow' }, formatter: (ps) => { const c = rows[ps[0].dataIndex]; return `${c.label}: <b>${f.frac(c.share, 0)}</b> · ${units.int(c.count)} aircraft`; } },
        xAxis: { type: 'category', data: rows.map((c) => c.label), axisLabel: { interval: 0, fontSize: 10 } },
        yAxis: { type: 'value', min: 0, max: 100, interval: 50, axisLabel: { formatter: (v) => units.pct(v, 0) } },
        series: [{ type: 'bar', data: rows.map((c) => ({ value: ok(c.share) ? c.share * 100 : 0, itemStyle: { color: c.label === 'unknown' ? C.grey : color } })), barMaxWidth: 22 }],
      }, { replace: true });
    }

    function paintEquip(d) {
      if (!d.adsb) return { empty: notEnough('Needs ADS-B aircraft that have sent their operational status (about once every 2.5 s).') };
      countChart('versions', d.versions, C.blue);
      countChart('nic', d.nic, C.cyan);
      countChart('nacp', d.nacp, C.teal);
      countChart('sil', d.sil, C.violet);
      ui.mount(eqKv, ui.kv([
        ['ADS-B aircraft', `${units.int(d.adsb)} of ${units.int(d.aircraft)}`],
        ['Poor integrity', f.frac(d.poorIntegrityShare, 0), { class: (d.poorIntegrityShare ?? 0) > 0.25 ? 'c-red' : '' }],
        ['Send TC28 (status)', f.frac(d.tc28Share, 0)],
        ['Send TC29 (target state)', f.frac(d.tc29Share, 0)],
        ['Send TC31 (op. status)', f.frac(d.tc31Share, 0)],
        ['Report GNSS height', f.frac(d.gnssAltShare, 0)],
      ], { columns: 2 }));
      return null;
    }

    function paintCat(d) {
      const rows = (d.categories || []).filter((c) => c.count > 0).sort((a, b) => b.count - a.count);
      if (!rows.length) return { empty: notEnough('Needs identification squitters from ADS-B aircraft.') };
      P.cat.chart.set({
        animation: false,
        grid: { left: 8, right: 44, top: 8, bottom: 8, containLabel: true },
        tooltip: { trigger: 'item', confine: true, formatter: (p) => { const c = rows[p.dataIndex]; return `${c.label} · ${CATEGORY[c.label] || 'other'}<br><b>${units.int(c.count)}</b> aircraft · ${f.frac(c.share, 0)}`; } },
        xAxis: { type: 'value', minInterval: 1, axisLabel: { formatter: (v) => units.int(v) } },
        yAxis: { type: 'category', inverse: true, data: rows.map((c) => `${c.label} ${CATEGORY[c.label] ? '· ' + CATEGORY[c.label] : ''}`), axisLabel: { fontSize: 10.5 } },
        series: [{ type: 'bar', data: rows.map((c) => c.count), color: C.blue, barMaxWidth: 14, itemStyle: { borderRadius: [0, 2, 2, 0] }, label: { show: true, position: 'right', color: C.text2, fontSize: 10.5, formatter: (p) => f.frac(rows[p.dataIndex].share, 0) } }],
      }, { replace: true });
      return null;
    }

    function paintGnss(d) {
      const series = (d.gnssSeries || []).filter((p) => ok(p.t));
      P.gnss.card.setSub(`now ${ok(d.gnssIndexNow) ? f.pct(d.gnssIndexNow, 1) : units.DASH} · degraded ${units.int(d.degradedNow)} · position jumps ${units.int(d.jumpAircraft)}`);
      if (!series.length || !anyNum(series.map((p) => p.index))) return { empty: notEnough('One point per minute once ADS-B aircraft with integrity data are heard.') };
      const opt = charts.line({
        series: [
          { name: 'Index', data: series.map((p) => [p.t, p.index]), color: C.amber, area: true, step: 'end' },
          { name: 'ADS-B aircraft', data: series.map((p) => [p.t, p.tot]), color: C.text3, y2: true, width: 1, dashed: true },
        ],
        yName: '%', yMin: 0, yMax: (v) => Math.max(10, Math.ceil(v.max)), yFormat: (v) => units.num(v, 0), y2Name: 'aircraft', y2Min: 0, y2Format: (v) => units.int(v), zoom: series.length > 120,
      });
      opt.tooltip.formatter = (ps) => {
        const p = series[ps[0].dataIndex];
        return `${units.clock(p.t, { seconds: false })}<br>index <b>${f.pct(p.index, 1)}</b><br>${units.int(p.deg)} of ${units.int(p.tot)} degraded · ${units.int(p.jumps)} jumps`;
      };
      opt.animation = false;
      P.gnss.chart.set(opt, { replace: true });
      const list = d.degradedList || [];
      ui.mount(P.gnss.extra, list.length ? h('div.rf-chips', h('span.muted', 'Degraded now:'), list.slice(0, 30).map((icao) => h('a.rf-chip.num', { href: '#/map/' + icao }, icao))) : null);
      return null;
    }

    /**
     * Station position: the effective one every analysis uses (/api/status `station`: located receiver,
     * demo site or receiver GPS), else the configured one (older servers), else back-projected from the
     * innermost cell's centre.
     */
    function stationOf(cells) {
      const st = ctx.status?.station;
      if (st && st.source !== 'none' && ok(st.lat) && ok(st.lon)) return { lat: st.lat, lon: st.lon };
      const r = ctx.config.current?.receiver;
      if (r && r.located && ok(r.lat) && ok(r.lon)) return { lat: r.lat, lon: r.lon };
      const c = [...cells].filter((x) => ok(x.lat) && ok(x.lon)).sort((a, b) => a.ring - b.ring)[0];
      if (!c) return null;
      const [lon, lat] = ctx.geo.destination(c.lat, c.lon, (c.sector * 10 + 5 + 180) % 360, c.ring * 50 + 25);
      return ok(lat) && ok(lon) ? { lat, lon } : null;
    }

    function paintMap(d) {
      const cells = (d.gnssCells || []).filter((c) => c.tot > 0);
      if (!cells.length) return { empty: notEnough('Cells fill as ADS-B aircraft with integrity data are seen around the station.') };
      const rx = stationOf(cells);
      if (!rx) return { empty: ['Receiver location needed', 'The interference map is drawn around the station. Set its location in Settings.', 'pin'] };
      if (map) { map.set(rx, cells); return null; }
      if (!mapPending) {
        mapPending = true;
        createGnssMap(mapEl, ctx, {
          describe: (c) => `${units.deg(c.sector * 10)}–${units.deg(c.sector * 10 + 10)} · ${f.band(c.ring * 50, c.ring * 50 + 50)}\nindex ${f.pct(c.index, 1)} · ${units.int(c.deg)} of ${units.int(c.tot)} aircraft-minutes degraded`,
        }).then((m) => {
          mapPending = false;
          if (!mapEl.isConnected) { m.remove(); return; }
          map = m;
          const cur = (lastData && (lastData.gnssCells || []).filter((c) => c.tot > 0)) || cells;
          map.set(stationOf(cur) || rx, cur);
        }).catch((e) => { mapPending = false; console.error('[rf] map failed', e); P.map.error(e); });
      }
      return null;
    }

    function paintDelta(d) {
      const hist = d.gnssDelta;
      if (!histTotal(hist)) return { empty: notEnough('Needs aircraft that report both altitudes.') };
      const pts = histPoints(hist).map(([x, c]) => [units.value('alt', x), c]);
      P.delta.chart.set({
        animation: false,
        grid: { left: 8, right: 14, top: 18, bottom: 8, containLabel: true },
        tooltip: { trigger: 'axis', confine: true, axisPointer: { type: 'shadow' }, formatter: (ps) => `${units.alt(units.toBase('alt', ps[0].value[0]))}<br><b>${units.int(ps[0].value[1])}</b> reports` },
        xAxis: { type: 'value', name: `GNSS − baro (${units.label('alt')})`, nameLocation: 'middle', nameGap: 24, scale: true, axisLabel: { formatter: (v) => units.num(v, 0) } },
        yAxis: { type: 'value', name: 'reports', axisLabel: { formatter: (v) => units.si(v) } },
        series: [{ type: 'bar', data: pts, color: C.teal, barWidth: '90%' }],
      }, { replace: true });
      const rows = d.gnssDeltaByAlt || [];
      ui.mount(deltaKv, h('div',
        ui.kv([['Mean difference', units.format('alt', d.gnssDeltaMeanFt, { sign: true })]], {}),
        rows.length ? ui.table({
          columns: [
            { key: 'band', label: 'Altitude band', render: (r) => altBand(r.band, units) },
            { key: 'meanFt', label: 'Mean', align: 'right', num: true, render: (r) => units.format('alt', r.meanFt, { sign: true }) },
            { key: 'stdFt', label: 'Spread', align: 'right', num: true, render: (r) => units.alt(r.stdFt) },
            { key: 'n', label: 'Reports', align: 'right', num: true, render: (r) => units.int(r.n) },
          ],
          rows,
        }) : null));
      return null;
    }

    let lastData = null;
    return {
      el: h('div.grid.cols-2.rf-grid', P.equip.card, P.cat.card, P.delta.card, P.gnss.card, P.map.card),
      onShow() { if (map) map.resize(); },
      render(state) {
        const s = state.fleet;
        lastData = s && s.data;
        show(P.equip, s, paintEquip);
        show(P.cat, s, paintCat);
        show(P.gnss, s, paintGnss);
        show(P.map, s, paintMap);
        show(P.delta, s, paintDelta);
      },
    };
  },
};
