// pages/forecast.js - Forecast: reception conditions, traffic and local weather for the next 72 h,
// with their verification.
//
// Endpoints: /api/forecast/nowcast, /reception, /traffic, /weather, /verification, /refit (POST).
// Shared helpers and styles: ../weather/common.js and css/weather.css (wx- / fx- classes).

import { createAdmin } from '../admin.js';
import {
  HOUR, ok, clamp, esc, hm, dayHm, dayTime, windowLabel, fmtDelta, signed, prob, uv, round, classInfo, labelInfo, isGoodLabel,
  confBadge, levelBadge, caption, panel, source, progress, notesList, details, attribution, trendArrow, gaugeOption, nowLine,
  bandSeries, stackedGrids, yIndex, timeTooltip, colors,
} from '../weather/common.js';

/** Zones of the reception index as labelled by the server (predict.receptionLabel). */
const RX_ZONES = [
  { to: 35, name: 'poor', color: colors.amber },
  { to: 62, name: 'normal', color: colors.blue },
  { to: 85, name: 'enhanced', color: colors.cyan },
  { to: 100, name: 'ducting likely', color: colors.violet },
];
const FACTOR_NAMES = { refraction: 'Refraction', duct: 'Duct', traffic: 'Traffic', timeOfDay: 'Time of day', humidity: 'Humidity', wind: 'Wind', pressure: 'Pressure' };
const PATTERN_ICON = { pressure: 'timer', 'front-approaching': 'wind', 'front-passage': 'wind', fog: 'cloud', inversion: 'layers', convection: 'bolt' };
const SEVERITY = [{ label: 'info', color: colors.text4, badge: 'other' }, { label: 'low', color: colors.blue, badge: 'blue' }, { label: 'moderate', color: colors.amber, badge: 'amber' }, { label: 'high', color: colors.red, badge: 'red' }];
const STATUS = { ok: { label: 'trained', badge: 'green' }, learning: { label: 'learning', badge: 'amber' }, insufficient: { label: 'waiting for data', badge: 'other' } };

export default {
  id: 'forecast',
  title: 'Forecast',

  async mount(container, ctx) {
    const { api, units, ui, charts } = ctx;
    const { h } = ui;
    const admin = createAdmin(ctx);
    await ui.loadCss(new URL('../../css/weather.css', import.meta.url));

    const panels = [];
    const reg = (p) => { panels.push(p); return p; };
    const refreshers = [];
    const emptyBox = (title, text, icon = 'history') => ui.empty({ icon, compact: true, title, text });

    // ------------------------------------------------------------------ head
    const statusEl = h('div.sub', 'Reception, traffic and weather for the next 72 hours');
    const refitBtn = ui.button({
      label: 'Refit models', icon: 'refresh', size: 'sm', variant: 'ghost', tip: 'Refit all forecast models on the newest data now (normally done every hour)',
      onClick: async () => {
        refitBtn.setLoading(true);
        try {
          await admin.run(() => api.post('/api/forecast/refit', null, { signal: ctx.signal, timeout: 60000 }), 'Refitting the forecast models needs the admin password.');
          ui.toast.success('Forecast models refitted');
        } catch (e) {
          if (!e.aborted && !e.cancelled) ui.toast.error(e.message || 'Refit failed');
        } finally {
          refitBtn.setLoading(false);
          for (const r of refreshers) r();
        }
      },
    });

    // ------------------------------------------------------------------ nowcast hero
    const heroCard = ui.card({ title: 'Reception now-cast', icon: 'radar', accent: true, sub: 'Conditions this hour and the trend for the next six' });
    const heroPanel = reg(panel(heroCard.body, {
      skeleton: 170,
      empty: (n) => (!n || !ok(n.index) ? { icon: 'radar', title: 'No now-cast yet', text: 'The forecaster builds its first products a few seconds after start.' } : null),
      build(el) {
        const gaugeEl = h('div.chart.fx-hero-gauge');
        const labelEl = h('div.fx-hero-label');
        const sentence = h('p.fx-sentence');
        const strip = h('div.fx-strip');
        const kvHost = h('div');
        const meta = h('div.fx-meta');
        el.append(h('div.fx-hero',
          h('div', gaugeEl),
          h('div', labelEl, sentence, h('div.label', 'Index for the next hours'), strip, meta),
          h('div.fx-hero-side', kvHost)),
        caption('The reception index runs from 0 to 100: 50 is an ordinary day, above 62 the range is enhanced, 85 and more means ducting is likely and unusually distant aircraft can appear. It is derived from the refractivity of the lower atmosphere; treat it as a hint, not a promise.'));
        const gauge = charts.create(gaugeEl);
        return (n) => {
          const li = labelInfo(n.label);
          gauge.set(gaugeOption(n.index, RX_ZONES, 'reception index'), { replace: true });
          const arrow = n.trend === 'improving' ? '↗' : n.trend === 'deteriorating' ? '↘' : '→';
          ui.mount(labelEl, ui.badge(li.label, li.badge), h('span.fx-trend', `${arrow} ${n.trend || 'steady'}${ok(n.trendDelta) && n.trendDelta !== 0 ? ` (${signed(n.trendDelta, 0)} in 6 h)` : ''}`));
          ui.setText(sentence, localiseSentence(n, units));
          ui.mount(strip, (n.next6h || []).map((p) => h('div.fx-strip-cell', { style: { '--c': labelInfo(p.label).color }, tip: `${dayHm(p.t)} · ${labelInfo(p.label).label}` }, h('b', String(p.index)), h('span', hm(p.t)))));
          const e = n.p95Km || {};
          ui.mount(kvHost, ui.kv([
            ['Expected p95 range', units.dist(e.value, { digits: 0 }), { tip: '95th percentile of this hour’s position distances: the robust “how far do I reach” number' }],
            ['80 % interval', ok(e.lo) && ok(e.hi) ? `${units.format('dist', e.lo, { digits: 0, unit: false })} – ${units.dist(e.hi, { digits: 0 })}` : units.DASH],
            ['Long-range chance (6 h)', prob(n.farProbability), { tip: 'Highest probability in the next six hours of an hourly maximum beyond the far-reception threshold' }],
            ['Expected aircraft', ok(n.aircraft) ? units.num(n.aircraft, 0) : 'no traffic history yet', { text: !ok(n.aircraft) }],
            ['Valid for', dayHm(n.t) + ' hour'],
          ]));
          ui.mount(meta, levelBadge(n.level), confBadge(n.confidence));
          ui.setText(statusEl, `Reception, traffic and weather for the next 72 hours · built ${units.ago(n.generated, ctx.live.serverNow())} · times are browser local`);
        };
      },
    }));
    refreshers.push(source(ctx, { every: 60000, panels: [heroPanel], load: (signal) => api.get('/api/forecast/nowcast', { signal }) }));

    // ------------------------------------------------------------------ reception forecast
    const rxCard = ui.card({ title: 'Reception conditions, next 72 h', icon: 'signal', sub: 'Index, expected range and long-range probability per hour' });
    const RX_H = 520;
    const rxPanel = reg(panel(rxCard.body, {
      skeleton: 380,
      empty: (d) => (!(d.hours || []).length ? { icon: 'signal', title: 'No reception forecast yet', text: d.method || 'The forecast is built after the first model fit.' } : null),
      build(el) {
        const winEl = h('div.fx-windows');
        const chartEl = h('div.chart', { style: { '--h': RX_H + 'px' } });
        const detail = h('div.fx-detail');
        const metaEl = h('div');
        const bandsEl = h('div');
        el.append(winEl,
          h('div.fx-split', h('div', chartEl), detail),
          caption('Top: reception index with its class bands (highlighted columns are the best windows). Middle: expected p95 range with its 80 % interval, the expected maximum, and what was actually measured in the last two days. Bottom: probability of anomalous long-range reception. A good forecast keeps the measured line inside the shaded band about four times out of five. Hover for the hour’s explanation, click to pin it.'),
          metaEl, bandsEl);
        const chart = charts.create(chartEl);
        let data = null;
        let pinned = null;
        let shown = null;
        const hourAt = (t) => {
          if (!data) return null;
          let best = null;
          for (const x of data.hours) if (!best || Math.abs(x.t - t) < Math.abs(best.t - t)) best = x;
          return best && Math.abs(best.t - t) <= HOUR ? best : null;
        };
        const show = (x) => {
          if (!x || !data) return;
          shown = x.t;
          ui.mount(detail, hourDetail(x, data, pinned !== null, ctx));
        };
        chart.on('updateAxisPointer', (e) => {
          const v = e.axesInfo && e.axesInfo[0] && e.axesInfo[0].value;
          if (pinned !== null || !ok(v)) return;
          const x = hourAt(v);
          if (x && x.t !== shown) show(x);
        });
        chart.whenReady.then((inst) => {
          if (!inst) return;
          inst.getZr().on('click', (ev) => {
            const pt = [ev.offsetX, ev.offsetY];
            let v = null;
            for (let i = 0; i < 3 && v === null; i++) if (inst.containPixel({ gridIndex: i }, pt)) v = inst.convertFromPixel({ gridIndex: i }, pt)[0];
            const x = v === null ? null : hourAt(v);
            if (!x) { if (pinned !== null) { pinned = null; show(hourAt(shown)); draw(); } return; }
            pinned = pinned === x.t ? null : x.t;
            show(x);
            draw();
          });
        });
        const pin = (t) => { pinned = t; show(hourAt(t)); draw(); };
        function draw() {
          chart.set(receptionOption(data, units, RX_H, pinned), { replace: true });
        }
        return (d) => {
          data = d;
          if (pinned !== null && !hourAt(pinned)) pinned = null;
          draw();
          ui.mount(winEl, windowCards(d, ctx, pin));
          show(hourAt(pinned ?? shown ?? -1) || (bestWindows(d.hours)[0]?.peak) || d.hours[0]);
          ui.mount(metaEl,
            h('div.fx-meta', levelBadge(d.level), confBadge(d.confidence), ui.badge(`${units.int(d.dataHours)} h of history`, 'other'), ui.badge(`${units.int(d.weatherHours)} of ${d.hours.length} h backed by a weather forecast`, d.weatherHours > 0 ? 'blue' : 'amber')),
            (d.notes || []).length ? notesList(d.notes, '.wx-notes--muted.wx-gap-top') : null,
            d.method ? details('How this forecast is calculated', d.method, d.model ? ` Learned model: ${units.int(d.model.trainingHours)} training hours, ${units.int(d.model.farEvents)} far-reception hours, weight in the blend ${prob(d.model.weight)}${ok(d.model.gradientEffectKm) ? `, learned effect of super-refraction ${signed(units.value('dist', d.model.gradientEffectKm), 0)} ${units.label('dist')}` : ''}.` : '') : null);
          ui.mount(bandsEl, (d.bands || []).length ? [
            h('div.wx-gap-top', ui.section('Radio horizon per altitude band (first forecast hour)', 'ruler')),
            ui.table({
              columns: [
                { key: 'label', label: 'Band', render: (r) => bandLabel(r, units) },
                { key: 'altFt', label: 'Reference altitude', align: 'right', num: true, render: (r) => units.alt(r.altFt, { fl: true }) },
                { key: 'standardKm', label: 'Standard atmosphere', align: 'right', num: true, render: (r) => units.dist(r.standardKm, { digits: 0 }) },
                { key: 'nowKm', label: 'Now', align: 'right', num: true, render: (r) => units.dist(r.nowKm, { digits: 0 }) },
                { key: 'factor', label: 'Change', align: 'right', num: true, render: (r) => (ok(r.factor) ? signed((r.factor - 1) * 100, 1) + ' %' : units.DASH) },
              ],
              rows: d.bands,
            }),
            caption(`Smooth-earth line of sight for the antenna at ${units.height(d.antennaAmslM, { digits: 0 })} above sea level. “Far reception” in this forecast means an hourly maximum beyond ${units.dist(d.farKm, { digits: 0 })}; a typical hour reaches ${units.dist(d.typicalP95Km, { digits: 0 })} (p95) and ${units.dist(d.typicalMaxKm, { digits: 0 })} (max).`),
          ] : null);
        };
      },
    }));
    refreshers.push(source(ctx, { every: 120000, panels: [rxPanel], load: (signal) => api.get('/api/forecast/reception', { signal }) }));

    // ------------------------------------------------------------------ traffic forecast
    const trCard = ui.card({ title: 'Traffic forecast', icon: 'plane', sub: 'Aircraft in view and message rate, next 72 h' });
    const TR_H = 380;
    const trPanel = reg(panel(trCard.body, {
      skeleton: 300,
      empty: (d) => (!(d.hours || []).some((x) => ok(x.aircraft?.value) || ok(x.msgRate?.value)) ? {
        icon: 'plane', title: 'Not enough data yet',
        text: `${d.method || 'No traffic history yet.'} ${(d.notes || []).map(capFirst).join('. ')}${(d.notes || []).length ? '.' : ''}`,
      } : null),
      build(el) {
        const busyEl = h('div.fx-windows');
        const chartEl = h('div.chart', { style: { '--h': TR_H + 'px' } });
        const metaEl = h('div');
        el.append(busyEl, chartEl,
          caption('Solid lines left of “now” are what the receiver counted; right of it the expected value with its 80 % interval. Traffic follows a daily and weekly rhythm, so the forecast sharpens visibly after one day and again after eight days of data.'),
          metaEl);
        const chart = charts.create(chartEl);
        return (d) => {
          chart.set(trafficOption(d, units, TR_H), { replace: true });
          const busy = (d.busiest || []).slice(0, 4);
          ui.mount(busyEl,
            busy.map((b, i) => h('div.fx-window' + (i === 0 ? '.fx-window--best' : ''), { style: { '--c': colors.amber } },
              h('div.fx-window-kicker', i === 0 ? 'Busiest upcoming hour' : `Busy hour #${i + 1}`),
              h('div.fx-window-time', windowLabel(b.t, b.t + HOUR)),
              h('div.fx-window-meta', `${units.num(b.aircraft, 0)} aircraft · ${units.rate(b.msgRate)} · in ${b.inHours} h`))),
            d.quietest ? h('div.fx-window.fx-window--none',
              h('div.fx-window-kicker', 'Quietest hour'),
              h('div.fx-window-time', windowLabel(d.quietest.t, d.quietest.t + HOUR)),
              h('div.fx-window-meta', `${units.num(d.quietest.aircraft, 0)} aircraft · ${units.rate(d.quietest.msgRate)} · good slot for maintenance`)) : null);
          ui.mount(metaEl,
            h('div.fx-meta', levelBadge(d.level), confBadge(d.confidence), ui.badge(`${units.int(d.dataHours)} h of history`, 'other'),
              d.skill && ok(d.skill.vsPersistence) ? ui.badge(`aircraft: ${signed(d.skill.vsPersistence * 100, 0)} % vs persistence`, d.skill.vsPersistence > 0 ? 'green' : 'amber') : null,
              d.msgRateSkill && ok(d.msgRateSkill.vsPersistence) ? ui.badge(`message rate: ${signed(d.msgRateSkill.vsPersistence * 100, 0)} % vs persistence`, d.msgRateSkill.vsPersistence > 0 ? 'green' : 'amber') : null),
            (d.notes || []).length ? notesList(d.notes, '.wx-notes--muted.wx-gap-top') : null,
            d.method ? details('How this forecast is calculated', d.method) : null);
        };
      },
    }));
    refreshers.push(source(ctx, { every: 120000, panels: [trPanel], load: (signal) => api.get('/api/forecast/traffic', { signal }) }));

    // ------------------------------------------------------------------ weather outlook
    const wxCard = ui.card({ title: 'Local weather outlook', icon: 'forecast', sub: 'Open-Meteo forecast, corrected with the station’s own verification where that helps' });
    const WX_H = 700;
    const noWx = (d) => (!(d.hours || []).length ? { icon: 'cloud', title: 'No weather forecast yet', text: d.method || 'Needs the first weather download.' } : null);
    const wxPanel = reg(panel(wxCard.body, {
      skeleton: 420, empty: noWx,
      build(el) {
        const nowEl = h('div.fx-now');
        const chartEl = h('div.chart', { style: { '--h': WX_H + 'px' } });
        const metaEl = h('div');
        el.append(nowEl, chartEl,
          caption('From top: temperature and dew point (a closing gap means fog or low cloud), sea-level pressure, wind and gusts, cloud cover with precipitation, and the derived risks. A high inversion risk on a calm, clear night is the classic recipe for extended ADS-B range the next morning.'),
          metaEl);
        const chart = charts.create(chartEl);
        return (d) => {
          chart.set(outlookOption(d, units, WX_H), { replace: true });
          const c = d.current;
          const cell = (label, value, small) => h('div', h('span.label', label), h('b', value), h('small', small || ' '));
          ui.mount(nowEl, c ? [
            cell('Now', c.text || units.DASH, dayHm(c.t)),
            cell('Temperature', units.temp(c.tempC), 'dew point ' + units.temp(c.dewPointC)),
            cell('Pressure', units.pressure(c.pressureMsl), `${trendArrow(c.change3h, 0.5)} ${fmtDelta('pressure', c.change3h, units.system === 'imperial' ? 2 : 1)} / 3 h · ${c.tendency || ''}`),
            cell('Pressure in 6 h', units.pressure(c.pressure6h?.value), ok(c.pressure6h?.lo) ? `${units.format('pressure', c.pressure6h.lo, { unit: false })} – ${units.format('pressure', c.pressure6h.hi, { unit: false })} (Kalman)` : ''),
            cell('Wind', units.wind(c.windKmh), ok(c.windDir) ? 'from ' + units.bearing(c.windDir) : ''),
            cell('Cloud / visibility', units.pct(c.cloud), units.visibility(c.visibilityM)),
          ] : null);
          const corrected = (d.hours || []).filter((x) => x.corrected).length;
          ui.mount(metaEl,
            h('div.fx-meta', levelBadge(d.level), confBadge(d.confidence), ui.badge(`${units.int(d.dataHours)} h of weather history`, 'other'),
              ui.badge(corrected ? `bias correction applied to ${corrected} h` : 'raw model forecast (no verified bias yet)', corrected ? 'green' : 'other')),
            (d.notes || []).length ? notesList(d.notes, '.wx-notes--muted.wx-gap-top') : null,
            d.method ? details('How this outlook is calculated', d.method) : null);
        };
      },
    }));

    const zamCard = ui.card({ title: 'Barometer outlook (Zambretti)', icon: 'timer', sub: 'Independent of any weather model' });
    const zamPanel = reg(panel(zamCard.body, {
      lines: 4,
      empty: (d) => (!d.zambretti ? { icon: 'timer', title: 'Not enough data yet', text: 'The barometric outlook needs a pressure reading and about three hours of pressure history for the tendency.' } : null),
      build(el) {
        const top = h('div.fx-zam');
        const kvHost = h('div.wx-gap-top');
        el.append(top, kvHost, caption('The Zambretti forecaster is a 1915 rule of thumb: pressure, its trend, wind direction and season give one of 26 outlooks (A = settled fine … Z = stormy). It knows nothing about the model forecast, which makes agreement between the two a useful confidence check.'));
        return (d) => {
          const z = d.zambretti;
          ui.mount(top, h('div.fx-zam-letter', { tip: `Zambretti number ${z.number} of 0–25` }, z.letter || '?'),
            h('div', h('div.fx-zam-text', z.text || ''), h('p.fx-zam-outlook', z.outlook || '')));
          ui.mount(kvHost, ui.kv([
            ['Pressure (MSL)', units.pressure(z.pressureMsl)],
            ['Tendency', `${trendArrow(z.change3h, 0.5)} ${fmtDelta('pressure', z.change3h, units.system === 'imperial' ? 2 : 1)} / 3 h · ${z.tendency || z.trend || ''}`, { text: true }],
            ['Wind', ok(z.windDir) ? `${z.wind || units.cardinal(z.windDir)} (${units.deg(z.windDir)})` : units.DASH],
            ['Season rule', `${z.season || ''}, ${z.hemisphere || ''}ern hemisphere`, { text: true }],
            ['Confidence', prob(z.confidence)],
            z.exceptional ? ['Note', 'pressure outside the 950–1050 hPa scale: outlook unreliable', { text: true }] : null,
          ]));
        };
      },
    }));

    const patCard = ui.card({ title: 'Detected patterns', icon: 'alert', sub: 'Fronts, fog, inversions, convection' });
    const patPanel = reg(panel(patCard.body, {
      lines: 4,
      empty: (d) => (!(d.patterns || []).length ? { icon: 'check', title: 'Nothing detected', text: 'No front, fog, inversion or convective signal in the record and the forecast. Patterns need a few hours of pressure, wind and temperature history.' } : null),
      build(el) {
        const list = h('div.fx-patterns');
        el.append(list, caption('Patterns are recognised from the station’s own pressure, wind and temperature record together with the forecast. Probabilities are rough; severity says how much attention the pattern deserves.'));
        return (d) => {
          const ps = [...d.patterns].sort((a, b) => (b.severity || 0) - (a.severity || 0));
          ui.mount(list, ps.map((p) => {
            const sv = SEVERITY[clamp(p.severity || 0, 0, 3)];
            return h('div.fx-pattern', { style: { '--c': sv.color } },
              h('div.fx-pattern-head', ui.icon(PATTERN_ICON[p.kind] || 'info'), p.title || p.kind,
                p.severity > 0 ? ui.badge(sv.label, sv.badge) : null,
                ok(p.probability) ? h('span.num', prob(p.probability) + ' likely') : null,
                p.from ? h('span.num', p.to && p.to !== p.from ? windowLabel(p.from, p.to) : dayHm(p.from)) : null),
              h('div.fx-pattern-detail', p.detail || ''));
          }));
        };
      },
    }));

    const anaCard = ui.card({ title: 'Analogue outlook', icon: 'history', sub: 'What followed the most similar past situations' });
    const anaPanel = reg(panel(anaCard.body, {
      lines: 4,
      empty: (d) => {
        if (d.analogue && (d.analogue.hours || []).length) return null;
        const note = (d.notes || []).find((n) => /analogue/i.test(n));
        return { icon: 'history', title: 'Not enough data yet', text: `The analogue method searches the station’s own weather record for past situations that resemble the present one and reports what followed them. ${note ? capFirst(note) : 'It needs roughly ten days of history'}.` };
      },
      build(el) {
        const sum = h('p.fx-zam-outlook');
        const chartEl = h('div.chart', { style: { '--h': '240px' } });
        const tableEl = h('div.wx-gap-top');
        el.append(sum, chartEl, caption('Temperature that followed the K most similar past weather situations (line = their average, band = their spread), against “no change” persistence. A narrow band means the analogues agree.'), tableEl);
        const chart = charts.create(chartEl);
        return (d) => {
          const a = d.analogue;
          ui.setText(sum, a.summary || '');
          const tl = units.label('temp');
          const rows = a.hours.map((x) => [x.t, uv('temp', x.tempC?.lo), uv('temp', x.tempC?.hi)]);
          const opt = charts.line({
            yName: tl, yScale: true, legend: true,
            series: [
              { name: 'Analogue mean', data: a.hours.map((x) => [x.t, uv('temp', x.tempC?.value)]), color: colors.amber, width: 2.2 },
              { name: 'Persistence', data: a.hours.map((x) => [x.t, uv('temp', x.persistTempC)]), color: colors.grey, dashed: true, width: 1.2 },
            ],
            tooltip: timeTooltip({ 'Analogue mean': tl, Persistence: tl }),
          });
          opt.series.push(...bandSeries('analogue', rows, colors.amber));
          chart.set(opt, { replace: true });
          ui.mount(tableEl, ui.section(`The ${a.k} closest of ${units.int(a.candidates)} past states`, 'list'), ui.table({
            columns: [
              { key: 't', label: 'Past state', render: (r) => units.dateTime(r.t, { utc: false, z: false }) },
              { key: 'distance', label: 'Distance', align: 'right', num: true, render: (r) => units.num(r.distance, 2, 2), tip: 'Standardised distance to the present state: 0 = identical' },
              { key: 'tempChangeC', label: 'Temp. +24 h', align: 'right', num: true, render: (r) => fmtDelta('temp', r.tempChangeC) },
              { key: 'pressureChange', label: 'Pressure +24 h', align: 'right', num: true, render: (r) => fmtDelta('pressure', r.pressureChange, units.system === 'imperial' ? 2 : 1) },
              { key: 'precipMm', label: 'Precip. 24 h', align: 'right', num: true, render: (r) => units.precip(r.precipMm) },
            ],
            rows: a.analogues || [],
          }));
        };
      },
    }));

    let wxAttrib = null;
    refreshers.push(source(ctx, {
      every: 120000, panels: [wxPanel, zamPanel, patPanel, anaPanel],
      load: (signal) => api.get('/api/forecast/weather', { signal }),
      onData(d) { wxAttrib = d; renderAttrib(); },
    }));

    // ------------------------------------------------------------------ verification
    const vfCard = ui.card({ title: 'Verification', icon: 'shield', sub: 'How good are these forecasts, and how much history does each model have?' });
    let vfModel = null;
    const vfPanel = reg(panel(vfCard.body, {
      skeleton: 260,
      build(el) {
        const sumEl = h('p.fx-zam-outlook', { style: { marginBottom: 'var(--s-4)' } });
        const modelsEl = h('div.fx-models');
        const seriesHead = h('div.wx-subhead.wx-gap-top');
        const seriesWrap = h('div');
        const seriesEl = h('div.chart', { style: { '--h': '280px' } });
        const leadEl = h('div.chart', { style: { '--h': '280px' } });
        const liveWrap = h('div.wx-gap-top');
        const biasWrap = h('div.wx-gap-top');
        const anaWrap = h('div.wx-gap-top');
        el.append(sumEl, ui.section('Models and data sufficiency', 'database'), modelsEl, seriesHead, seriesWrap, liveWrap, biasWrap, anaWrap);
        let sChart = null;
        let lChart = null;
        let data = null;
        function draw() {
          const d = data;
          ui.setText(sumEl, d.summary || '');
          ui.mount(modelsEl, (d.models || []).map((m) => modelCard(m, ctx)));
          // forecast vs actual
          const withSeries = (d.models || []).filter((m) => (m.series || []).length || (m.byLead || []).length);
          const key = (m) => m.product + '.' + m.target;
          if (!withSeries.length) {
            if (sChart) { sChart.dispose(); sChart = null; }
            if (lChart) { lChart.dispose(); lChart = null; }
            ui.mount(seriesHead, ui.section('Forecast versus actual', 'chart'));
            ui.mount(seriesWrap, emptyBox('Not enough data yet', 'Backtests replay the forecast from past points in time and compare it with what was then measured. The first backtest needs about four days of reception; the progress bars above show how far each model is.'));
          } else {
            if (!vfModel || !withSeries.some((m) => key(m) === vfModel)) vfModel = key(withSeries[0]);
            ui.mount(seriesHead, ui.section('Forecast versus actual (backtest)', 'chart'),
              ui.segmented({ options: withSeries.map((m) => ({ value: key(m), label: m.title })), value: vfModel, onChange: (v) => { vfModel = v; draw(); } }));
            if (!sChart) {
              ui.mount(seriesWrap, h('div.wx-grid.wx-grid--2', h('div', seriesEl, caption('Each forecast point was made with data available before that hour only. The closer the forecast line hugs the actual line, the better; systematic offsets show bias.')),
                h('div', leadEl, caption('Typical error (RMSE) by how far ahead the forecast looks. A good model starts low and rises slowly; where it meets the baseline error, the forecast stops adding value.'))));
              sChart = charts.create(seriesEl);
              lChart = charts.create(leadEl);
            }
            const m = withSeries.find((x) => key(x) === vfModel);
            const conv = targetConv(m, units);
            sChart.set(charts.line({
              yName: conv.unit, yScale: true, legend: true,
              series: [
                { name: 'Actual', data: (m.series || []).map((p) => [p.t, conv.f(p.actual)]), color: colors.text2, width: 1.6 },
                { name: 'Forecast', data: (m.series || []).map((p) => [p.t, conv.f(p.forecast)]), color: colors.cyan, width: 2 },
              ],
              tooltip: timeTooltip({ Actual: conv.unit, Forecast: conv.unit }),
            }), { replace: true });
            const lo = charts.line({
              xType: 'value', xName: 'lead time, h', yName: 'RMSE' + (conv.unit ? ', ' + conv.unit : ''), legend: false,
              series: [{ name: 'RMSE', data: (m.byLead || []).map((p) => [p.leadH, conv.f(p.rmse)]), color: colors.amber, width: 2 }],
              valueFormat: (v) => units.num(v, 1) + ' ' + conv.unit,
            });
            lo.grid.bottom = 30;
            if (m.skill && ok(m.skill.persistence)) {
              lo.series[0].markLine = { silent: true, symbol: 'none', lineStyle: { type: 'dashed', color: colors.text3 }, label: { formatter: 'persistence', position: 'insideEndTop', color: colors.text3, fontSize: 10 }, data: [{ yAxis: conv.f(m.skill.persistence) }] };
            }
            lChart.set(lo, { replace: true });
          }
          // live verification
          ui.mount(liveWrap, ui.section('Issued forecasts against what happened', 'check'), (d.live || []).length ? [ui.table({
            columns: [
              { key: 'product', label: 'Product', render: (r) => LIVE_NAMES[r.product] || r.product, sort: true },
              { key: 'leadH', label: 'Lead', align: 'right', num: true, render: (r) => r.leadH + ' h', sort: true },
              { key: 'n', label: 'n', align: 'right', num: true, render: (r) => units.int(r.n), sort: true },
              { key: 'mae', label: 'MAE', align: 'right', num: true, render: (r) => liveVal(r, r.mae, units) },
              { key: 'rmse', label: 'RMSE', align: 'right', num: true, render: (r) => liveVal(r, r.rmse, units) },
              { key: 'bias', label: 'Bias', align: 'right', num: true, render: (r) => liveVal(r, r.bias, units, true), tip: 'Mean of forecast minus actual: positive = the forecast is too high' },
              { key: 'coverage', label: '80 % interval hit', align: 'right', num: true, render: (r) => prob(r.coverage), tip: 'Share of the measured values inside the forecast’s 80 % interval. Close to 80 % = honest intervals.' },
            ],
            rows: d.live,
          }), caption('Unlike the backtests, this compares the forecasts that were really published (including the error of the weather forecast they were built on) with the measurements that followed.')]
            : emptyBox('Not enough data yet', 'Every forecast issued is logged and scored once its hour has passed. The first rows appear a few hours after the first forecast with real traffic history.'));
          // Open-Meteo bias
          ui.mount(biasWrap, ui.section('Open-Meteo forecast against the station record', 'cloud'), (d.bias || []).length ? [ui.table({
            columns: [
              { key: 'variable', label: 'Variable', render: (r) => WX_VAR[r.variable]?.label || r.variable, sort: true },
              { key: 'leadFrom', label: 'Lead', align: 'right', num: true, render: (r) => `${r.leadFrom}–${r.leadTo} h`, sort: true },
              { key: 'n', label: 'n', align: 'right', num: true, render: (r) => units.int(r.n) },
              { key: 'bias', label: 'Bias', align: 'right', num: true, render: (r) => wxDelta(r.variable, r.bias, units, true) },
              { key: 'mae', label: 'MAE', align: 'right', num: true, render: (r) => wxDelta(r.variable, r.mae, units) },
              { key: 'maeHoldout', label: 'MAE raw (hold-out)', align: 'right', num: true, render: (r) => wxDelta(r.variable, r.maeHoldout, units) },
              { key: 'maeCorrected', label: 'MAE corrected', align: 'right', num: true, render: (r) => wxDelta(r.variable, r.maeCorrected, units) },
              { key: 'applied', label: '', render: (r) => (r.applied ? ui.badge('applied', 'green') : ui.badge('not applied', 'other')) },
            ],
            rows: d.bias,
          }), caption('A correction is applied to the outlook only where it lowers the error on the newest 30 % of the logged forecasts - otherwise the raw model value is shown unchanged.')]
            : emptyBox('Not enough data yet', 'Needs about 24 logged forecast/observation pairs per variable and lead-time class (roughly one to two days of running).'));
          // analogue skill
          ui.mount(anaWrap, (d.analogue || []).length ? [ui.section('Analogue method against persistence and Open-Meteo', 'history'), ui.table({
            columns: [
              { key: 'variable', label: 'Variable', render: (r) => WX_VAR[r.variable]?.label || r.variable, sort: true },
              { key: 'leadH', label: 'Lead', align: 'right', num: true, render: (r) => r.leadH + ' h', sort: true },
              { key: 'n', label: 'n', align: 'right', num: true, render: (r) => units.int(r.n) },
              { key: 'analogueMae', label: 'MAE analogue', align: 'right', num: true, render: (r) => wxDelta(r.variable, r.analogueMae, units) },
              { key: 'persistenceMae', label: 'MAE persistence', align: 'right', num: true, render: (r) => wxDelta(r.variable, r.persistenceMae, units) },
              { key: 'vsPersistence', label: 'Skill', align: 'right', num: true, render: (r) => skillText(r.vsPersistence) },
              { key: 'openMeteoMae', label: 'MAE Open-Meteo', align: 'right', num: true, render: (r) => (r.nOpenMeteo ? wxDelta(r.variable, r.openMeteoMae, units) : units.DASH) },
            ],
            rows: d.analogue,
          })] : null);
        }
        return (d) => { data = d; draw(); };
      },
    }));
    refreshers.push(source(ctx, { every: 120000, panels: [vfPanel], load: (signal) => api.get('/api/forecast/verification', { signal }) }));

    // ------------------------------------------------------------------ layout
    const attribEl = h('div');
    function renderAttrib() {
      ui.mount(attribEl, attribution([
        { text: wxAttrib?.attribution || 'Weather data by Open-Meteo.com (CC BY 4.0)', url: wxAttrib?.attributionUrl || 'https://open-meteo.com/' },
        { text: 'Reception and traffic forecasts are computed on this station from its own statistics' },
      ]));
    }
    renderAttrib();
    container.append(h('div.page-inner.wx',
      h('div.page-head', h('div', h('h2', 'Forecast'), statusEl), h('div.actions', refitBtn)),
      heroCard, rxCard, trCard, wxCard,
      h('div.wx-grid.wx-grid--3', zamCard, patCard, anaCard),
      vfCard, attribEl));

    ctx.onCleanup(units.onChange(() => { for (const p of panels) p.rerender(); }));
  },
};

// -------------------------------------------------------------------------------------------------
// helpers
// -------------------------------------------------------------------------------------------------

const capFirst = (s) => (s ? s[0].toUpperCase() + s.slice(1) : s);
const LIVE_NAMES = { 'traffic.aircraft': 'Aircraft count', 'traffic.msgRate': 'Message rate', 'reception.p95': 'p95 range', 'reception.max': 'Max range' };
const WX_VAR = {
  tempC: { label: 'Temperature', kind: 'temp' }, dewPointC: { label: 'Dew point', kind: 'temp' }, pressureMsl: { label: 'Pressure', kind: 'pressure' },
  windKmh: { label: 'Wind speed', kind: 'wind' }, gustKmh: { label: 'Gusts', kind: 'wind' }, rh: { label: 'Humidity', unit: '%' }, cloud: { label: 'Cloud cover', unit: '%' },
  precipMm: { label: 'Precipitation', kind: 'precip' },
};

/** Error / bias of a weather variable as a difference in the user's unit. */
function wxDelta(variable, v, units, sign = false) {
  if (!ok(v)) return units.DASH;
  const w = WX_VAR[variable];
  if (w && w.kind) { const s = fmtDelta(w.kind, v, w.kind === 'pressure' && units.system === 'imperial' ? 2 : 1); return sign ? s : s.replace('+', ''); }
  return (sign && v > 0 ? '+' : '') + units.num(v, 1) + (w && w.unit ? ' ' + w.unit : '');
}

function liveVal(r, v, units, sign = false) {
  if (!ok(v)) return units.DASH;
  if (r.product.startsWith('reception')) return (sign && v > 0 ? '+' : '') + units.dist(v);
  return (sign && v > 0 ? '+' : '') + units.num(v, 1) + (r.product === 'traffic.msgRate' ? '/s' : '');
}

const skillText = (v) => (ok(v) ? signed(v * 100, 0) + ' %' : '—');

/** Conversion of a model's target quantity (series, errors) to the user's unit. */
function targetConv(m, units) {
  const u = m.skill && m.skill.unit;
  if (m.target === 'p95Km' || u === 'km') return { f: (v) => uv('dist', v, 1), unit: units.label('dist') };
  if (m.target === 'msgRate') return { f: (v) => round(v, 1), unit: 'msg/s' };
  if (m.target === 'aircraft') return { f: (v) => round(v, 1), unit: 'aircraft' };
  if (u === 'K') return { f: (v) => (ok(v) ? round(units.value('temp', v) - units.value('temp', 0), 2) : null), unit: units.label('temp') };
  return { f: (v) => round(v, 3), unit: u || '' };
}

function bandLabel(r, units) {
  const lo = [0, 1000, 5000, 10000, 20000, 30000, 40000][r.band];
  const hi = [1000, 5000, 10000, 20000, 30000, 40000, null][r.band];
  if (lo === undefined) return r.label;
  if (r.band === 0) return 'below ' + units.alt(hi);
  if (hi === null) return 'above ' + units.alt(lo);
  return `${units.format('alt', lo, { unit: false })} – ${units.alt(hi)}`;
}

/** The server's now-cast sentence is in km; rebuild it in the user's distance unit. */
function localiseSentence(n, units) {
  const li = labelInfo(n.label);
  const e = n.p95Km || {};
  const trend = n.trend === 'improving' ? 'improving over' : n.trend === 'deteriorating' ? 'deteriorating over' : 'steady for';
  let s = `Reception ${li.label.toLowerCase()} (index ${n.index}), ${trend} the next 6 h`;
  if (ok(e.value)) s += `; expected range about ${units.dist(e.value, { digits: 0 })}`;
  return s + '.';
}

/** Runs of consecutive hours labelled enhanced / ducting likely, best first. */
function bestWindows(hours) {
  const runs = [];
  let cur = null;
  for (const x of hours || []) {
    if (isGoodLabel(x.label)) {
      if (cur && x.t - cur.last.t <= HOUR) { cur.hours.push(x); cur.last = x; } else { cur = { hours: [x], last: x }; runs.push(cur); }
    } else cur = null;
  }
  for (const r of runs) {
    r.from = r.hours[0].t;
    r.to = r.last.t + HOUR;
    r.peak = r.hours.reduce((m, x) => (x.index > m.index ? x : m), r.hours[0]);
    r.far = Math.max(...r.hours.map((x) => (ok(x.farProbability) ? x.farProbability : 0)));
    r.ducting = r.hours.some((x) => x.label === 'ducting likely');
  }
  return runs.sort((a, b) => b.peak.index - a.peak.index || b.far - a.far || b.hours.length - a.hours.length);
}

function windowCards(d, ctx, pin) {
  const { ui, units } = ctx;
  const { h } = ui;
  const wins = bestWindows(d.hours).slice(0, 4);
  if (!wins.length) {
    const b = d.best;
    const flat = d.hours.every((x) => x.source === 'climatology');
    return [
      h('div.fx-window.fx-window--none',
        h('div.fx-window-kicker', 'Best DX window'),
        h('div.fx-window-time', 'none in the next 72 h'),
        h('div.fx-window-meta', flat ? 'No weather forecast is available, so a standard atmosphere is assumed for every hour.' : 'No hour reaches the “enhanced” class: expect ordinary, geometry-limited range.')),
      b && !flat ? h('div.fx-window', { role: 'button', tabindex: '0', onclick: () => pin(b.t), style: { '--c': labelInfo(b.label).color } },
        h('div.fx-window-kicker', 'Nominally best hour'),
        h('div.fx-window-time', windowLabel(b.t, b.t + HOUR)),
        h('div.fx-window-meta', `index ${b.index} (${labelInfo(b.label).label.toLowerCase()}) · p95 about ${units.dist(b.p95Km?.value, { digits: 0 })} · only marginally better than the rest`)) : null,
    ];
  }
  return wins.map((w, i) => h('div.fx-window' + (i === 0 ? '.fx-window--best' : ''), { role: 'button', tabindex: '0', onclick: () => pin(w.peak.t), style: { '--c': w.ducting ? colors.violet : colors.cyan } },
    h('div.fx-window-kicker', i === 0 ? 'Best DX window' : `Window #${i + 1}`),
    h('div.fx-window-time', windowLabel(w.from, w.to)),
    h('div.fx-window-meta', `${w.ducting ? 'ducting likely' : 'enhanced'} · peak index ${w.peak.index} at ${hm(w.peak.t)} · p95 about ${units.dist(w.peak.p95Km?.value, { digits: 0 })} · long-range chance ${prob(w.far)}`)));
}

/** Explanation panel for one forecast hour. */
function hourDetail(x, d, pinned, ctx) {
  const { ui, units } = ctx;
  const { h } = ui;
  const li = labelInfo(x.label);
  const a = x.atmosphere || {};
  const ci = classInfo(a.class);
  const p = x.p95Km || {};
  const m = x.maxKm || {};
  const iv = (e) => (ok(e.lo) && ok(e.hi) ? ` (${units.format('dist', e.lo, { digits: 0, unit: false })} – ${units.format('dist', e.hi, { digits: 0, unit: false })})` : '');
  const factors = [...(x.factors || [])].sort((f, g) => Math.abs(g.effectKm || 0) - Math.abs(f.effectKm || 0)).slice(0, 5);
  const lead = x.label === 'poor' ? 'Reduced range' : x.label === 'normal' ? 'Normal conditions' : x.label === 'enhanced' ? 'Enhanced range' : 'Ducting likely';
  return [
    h('div.fx-detail-head', h('span.fx-detail-time', dayHm(x.t)), ui.badge(li.label, li.badge), h('span.muted', x.leadH > 0 ? `in ${x.leadH} h` : 'this hour')),
    h('p', `${lead}: p95 range about ${units.dist(p.value, { digits: 0 })}.`, x.source === 'climatology' ? ' No weather forecast for this hour - a standard atmosphere is assumed.' : ''),
    ui.kv([
      ['Reception index', String(x.index)],
      ['p95 range', units.dist(p.value, { digits: 0 }) + iv(p)],
      ['Max range', units.dist(m.value, { digits: 0 }) + iv(m)],
      ['Long-range chance', prob(x.farProbability), { tip: `Probability that the hour’s furthest position lies beyond ${units.dist(d.farKm, { digits: 0 })}` }],
      ['Expected aircraft', ok(x.aircraft) ? units.num(x.aircraft, 0) : units.DASH],
      ['Atmosphere', ci.label + (a.estimated ? ' (assumed)' : ''), { text: true }],
      ['dN/dh · k', ok(a.dNdh) ? `${signed(a.dNdh, 0)} N/km · ${a.k > 50 ? '∞' : units.num(a.k, 2, 2)}` : units.DASH],
      ['Radio horizon', ok(a.horizonFactor) ? signed((a.horizonFactor - 1) * 100, 1) + ' %' : units.DASH],
      a.ducts > 0 ? ['Duct', `${units.height(a.ductBaseM, { digits: 0 })} – ${units.height(a.ductTopM, { digits: 0 })}${a.ducting1090 ? ' · traps 1090 MHz' : ''}`] : null,
      ['Confidence', prob(x.confidence)],
      ['Based on', x.source === 'forecast' ? 'weather forecast' : x.source === 'observation' ? 'current analysis' : 'climatology', { text: true }],
    ]),
    ui.section('Top factors', 'list'),
    factors.length
      ? h('ul.fx-factors', factors.map((f) => h('li', h('span', h('b', { style: { color: 'var(--text)', fontFamily: 'var(--font-ui)' } }, (FACTOR_NAMES[f.key] || f.key) + ': '), f.text || ''),
        ok(f.effectKm) ? h('b' + (f.effectKm > 0 ? '.pos' : f.effectKm < 0 ? '.neg' : ''), `${signed(units.value('dist', f.effectKm), Math.abs(f.effectKm) < 10 ? 1 : 0)} ${units.label('dist')}`) : null)))
      : h('p.muted', 'No factor moves this hour away from a typical one.'),
    h('div.fx-pin', pinned ? 'Pinned - click the chart again to release.' : 'Hover the chart to explore, click to pin an hour.'),
  ];
}

function modelCard(m, ctx) {
  const { ui, units } = ctx;
  const { h } = ui;
  const st = STATUS[m.status] || { label: m.status || 'unknown', badge: 'other' };
  const s = m.skill;
  const cls = (v) => (!ok(v) ? '' : v > 0.02 ? '.pos' : v < -0.02 ? '.neg' : '.zero');
  const conv = targetConv(m, units);
  const days = (hrs) => (hrs >= 48 ? units.num(hrs / 24, hrs % 24 ? 1 : 0) + ' d' : units.int(hrs) + ' h');
  return h('div.fx-model',
    h('div.fx-model-head', h('b', m.title || `${m.product} ${m.target}`), ui.badge(st.label, st.badge)),
    progress('History', m.dataHours, m.needHours, `${days(m.dataHours)} of ${days(m.needHours)}`, m.status === 'ok'),
    m.needs ? h('div.fx-model-needs', capFirst(m.needs) + '.') : h('div.fx-model-needs', 'Has all the history it needs.'),
    s ? h('div.fx-skill',
      h('div', { tip: 'Skill score 1 − error(model) / error(persistence): positive = better than assuming “same as the last value”' }, h('span.label', 'vs persistence'), h('b' + cls(s.vsPersistence), skillText(s.vsPersistence))),
      h('div', { tip: 'Skill score against “same as this hour yesterday”' }, h('span.label', 'vs seasonal-naive'), h('b' + cls(s.vsSeasonalNaive), skillText(s.vsSeasonalNaive))),
      h('div', h('span.label', (s.metric || 'rmse').toUpperCase() + ' model'), h('b', s.metric === 'brier' ? units.num(s.model, 3, 3) : `${units.num(conv.f(s.model), 1)} ${conv.unit}`),
        h('small', s.metric === 'brier' ? `persistence ${units.num(s.persistence, 3, 3)}` : `persistence ${units.num(conv.f(s.persistence), 1)} · naive ${units.num(conv.f(s.seasonalNaive), 1)}`)),
      h('div', h('span.label', 'Scored'), h('b', units.int(s.n)), h('small', `${units.int(s.origins)} origins · ${s.horizonH} h horizon`)),
      s.note ? h('small', { style: { gridColumn: '1 / -1' } }, s.note) : null) : null,
    m.method ? details('Method', m.method) : null);
}

// ---------------------------------------------------------------------------------------------
// chart options
// ---------------------------------------------------------------------------------------------

function receptionOption(d, units, H, pinned) {
  const hrs = d.hours;
  const rec = d.recent || [];
  const now = hrs[0].t;
  const dl = units.label('dist');
  const rows = [
    { name: 'reception index', min: 0, max: 100, scale: false },
    { name: `range, ${dl}`, weight: 1.25 },
    { name: 'long-range chance, %', min: 0, scale: false, weight: 0.6 },
  ];
  const tMin = rec.length ? Math.max(rec[0].t, now - 48 * HOUR) : now - 2 * HOUR;
  const tMax = hrs[hrs.length - 1].t + HOUR;
  const g = stackedGrids(rows, { height: H, min: tMin, max: tMax, top: 44 });
  g.yAxis[0].interval = 25;
  const zones = [];
  let from = 0;
  for (const z of RX_ZONES) {
    zones.push([{ yAxis: from, itemStyle: { color: z.color, opacity: 0.08 }, name: labelInfo(z.name).label, label: { show: true, position: 'insideTopLeft', color: z.color, fontSize: 9.5, opacity: 0.9 } }, { yAxis: z.to }]);
    from = z.to;
  }
  const wins = bestWindows(hrs).slice(0, 4).map((w, i) => [{ xAxis: w.from, itemStyle: { color: w.ducting ? colors.violet : colors.cyan, opacity: i === 0 ? 0.22 : 0.12 } }, { xAxis: w.to }]);
  const pinArea = pinned !== null ? [[{ xAxis: pinned - HOUR / 2, itemStyle: { color: '#fff', opacity: 0.1 } }, { xAxis: pinned + HOUR / 2 }]] : [];
  const pts = (list, f) => list.map((x) => [x.t, f(x)]);
  const km = (v) => uv('dist', v, 0);
  const base = (pane, name, color, data, o = {}) => ({
    name, type: o.type || 'line', xAxisIndex: pane, yAxisIndex: yIndex(rows, pane), color, data, showSymbol: false, connectNulls: false,
    lineStyle: { width: o.width ?? 2, type: o.dashed ? 'dashed' : 'solid' }, z: o.z ?? 3, ...o.extra,
  });
  const series = [
    base(0, 'Index', colors.cyan, pts(hrs, (x) => x.index), {
      width: 2.4,
      extra: { markArea: { silent: true, data: [...zones, ...wins, ...pinArea] }, markLine: nowLine(now) },
    }),
    base(0, 'Index (observed)', colors.text2, pts(rec.filter((x) => ok(x.index)), (x) => x.index), { width: 1.4 }),
    base(1, 'p95 expected', colors.cyan, pts(hrs, (x) => km(x.p95Km?.value)), { width: 2.2, extra: { markLine: nowLine(now, '') } }),
    base(1, 'Max expected', colors.blue, pts(hrs, (x) => km(x.maxKm?.value)), { width: 1.3, dashed: true }),
    base(1, 'p95 measured', colors.text2, pts(rec, (x) => km(x.p95Km)), { width: 1.5 }),
    base(1, 'Max measured', colors.text4, pts(rec, (x) => km(x.maxKm)), { width: 1.1 }),
    ...bandSeries('p95', hrs.map((x) => [x.t, km(x.p95Km?.lo), km(x.p95Km?.hi)]), colors.cyan, { xAxisIndex: 1, yAxisIndex: yIndex(rows, 1) }),
    {
      name: 'Long-range chance', type: 'bar', xAxisIndex: 2, yAxisIndex: yIndex(rows, 2), barWidth: '60%', barMaxWidth: 8,
      data: hrs.map((x) => ({ value: [x.t + HOUR / 2, ok(x.farProbability) ? round(x.farProbability * 100, 1) : null], itemStyle: { color: labelInfo(x.label).color, opacity: 0.85 } })),
      color: colors.violet,
    },
  ];
  if (ok(d.farKm)) {
    series[3].markLine = { silent: true, symbol: 'none', lineStyle: { type: 'dashed', color: colors.violet, opacity: 0.7 }, label: { formatter: 'far-reception threshold', position: 'insideEndTop', color: colors.violet, fontSize: 9.5 }, data: [{ yAxis: km(d.farKm) }] };
  }
  const fmt = {
    Index: (v) => String(v), 'Index (observed)': (v) => String(v), 'p95 expected': dl, 'Max expected': dl, 'p95 measured': dl, 'Max measured': dl,
    'Long-range chance': (v) => units.num(v, v < 10 ? 1 : 0) + ' %',
  };
  const tip = timeTooltip(fmt);
  return {
    animation: false,
    legend: { show: true, top: 0, left: 0, type: 'scroll', data: ['Index', 'Index (observed)', 'p95 expected', 'Max expected', 'p95 measured', 'Max measured', 'Long-range chance'] },
    tooltip: {
      trigger: 'axis', confine: true, axisPointer: { type: 'line' },
      formatter: (ps) => {
        const list = Array.isArray(ps) ? ps : [ps];
        const t = list.length && Array.isArray(list[0].value) ? list[0].value[0] : null;
        const x = ok(t) ? hrs.find((q) => Math.abs(q.t - t) <= HOUR / 2) : null;
        return tip(ps) + (x && x.summary ? `<div class="wx-tt-text">${esc(labelInfo(x.label).label)} · ${esc(classInfo(x.atmosphere?.class).label)} atmosphere</div>` : '');
      },
    },
    axisPointer: { link: [{ xAxisIndex: 'all' }] },
    ...g,
    series,
  };
}

function trafficOption(d, units, H) {
  const hrs = d.hours;
  const rec = d.recent || [];
  const now = hrs[0].t;
  const rows = [{ name: 'aircraft', min: 0, scale: false }, { name: 'messages / s', min: 0, scale: false }];
  const tMin = rec.length ? Math.max(rec[0].t, now - 48 * HOUR) : now - 2 * HOUR;
  const g = stackedGrids(rows, { height: H, min: tMin, max: hrs[hrs.length - 1].t + HOUR, top: 44 });
  const L = (pane, name, color, data, o = {}) => ({
    name, type: 'line', xAxisIndex: pane, yAxisIndex: yIndex(rows, pane), color, data, showSymbol: false,
    lineStyle: { width: o.width ?? 2, type: o.dashed ? 'dashed' : 'solid' }, z: 3, ...o.extra,
  });
  const busy = (d.busiest || []).slice(0, 4).map((b, i) => [{ xAxis: b.t, itemStyle: { color: colors.amber, opacity: i === 0 ? 0.22 : 0.1 } }, { xAxis: b.t + HOUR }]);
  const series = [
    L(0, 'Aircraft expected', colors.amber, hrs.map((x) => [x.t, round(x.aircraft?.value, 1)]), { extra: { markLine: nowLine(now), markArea: { silent: true, data: busy } } }),
    L(0, 'Aircraft counted', colors.text2, rec.map((x) => [x.t, round(x.aircraft, 1)]), { width: 1.5 }),
    ...bandSeries('ac', hrs.map((x) => [x.t, round(x.aircraft?.lo, 1), round(x.aircraft?.hi, 1)]), colors.amber, { xAxisIndex: 0, yAxisIndex: yIndex(rows, 0) }),
    L(1, 'Message rate expected', colors.cyan, hrs.map((x) => [x.t, round(x.msgRate?.value, 1)]), { extra: { markLine: nowLine(now, '') } }),
    L(1, 'Message rate measured', colors.text2, rec.map((x) => [x.t, round(x.msgRate, 1)]), { width: 1.5 }),
    ...bandSeries('mr', hrs.map((x) => [x.t, round(x.msgRate?.lo, 1), round(x.msgRate?.hi, 1)]), colors.cyan, { xAxisIndex: 1, yAxisIndex: yIndex(rows, 1) }),
  ];
  const fmt = {
    'Aircraft expected': (v) => units.num(v, 0), 'Aircraft counted': (v) => units.num(v, 0),
    'Message rate expected': (v) => units.rate(v), 'Message rate measured': (v) => units.rate(v),
  };
  return {
    animation: false,
    legend: { show: true, top: 0, left: 0, type: 'scroll', data: Object.keys(fmt) },
    tooltip: { trigger: 'axis', confine: true, formatter: timeTooltip(fmt) },
    axisPointer: { link: [{ xAxisIndex: 'all' }] },
    ...g,
    series,
  };
}

function outlookOption(d, units, H) {
  const hrs = d.hours;
  const tl = units.label('temp');
  const pl = units.label('pressure');
  const wl = units.label('wind');
  const rl = units.label('precip');
  const pd = units.system === 'imperial' ? 2 : 1;
  const rows = [
    { name: tl },
    { name: pl },
    { name: wl, min: 0, scale: false },
    { name: 'cloud / precip. probability, %', min: 0, max: 100, scale: false, y2: { name: rl, min: 0, scale: false } },
    { name: 'risk, %', min: 0, max: 100, scale: false },
  ];
  const g = stackedGrids(rows, { height: H, min: hrs[0].t, max: hrs[hrs.length - 1].t, top: 44 });
  const L = (pane, name, color, f, o = {}) => ({
    name, type: o.type || 'line', xAxisIndex: pane, yAxisIndex: yIndex(rows, pane, !!o.y2), color, data: hrs.map((x) => [x.t, f(x)]), showSymbol: false,
    lineStyle: { width: o.width ?? 1.9, type: o.dashed ? 'dashed' : 'solid' }, areaStyle: o.area ? { color, opacity: 0.1 } : undefined, barMaxWidth: 7, z: 3,
  });
  const anyCorr = hrs.some((x) => x.corrected && ok(x.tempRawC) && Math.abs(x.tempRawC - x.tempC) > 0.05);
  // night shading from isDay
  const nights = [];
  let start = null;
  hrs.forEach((x, i) => {
    if (x.isDay === false && start === null) start = x.t;
    if ((x.isDay !== false || i === hrs.length - 1) && start !== null) { nights.push([{ xAxis: start, itemStyle: { color: '#000', opacity: 0.22 } }, { xAxis: x.t }]); start = null; }
  });
  const pc = (v) => (ok(v) ? round(v * 100, 0) : null);
  const series = [
    L(0, 'Temperature', colors.amber, (x) => uv('temp', x.tempC), { width: 2.2 }),
    anyCorr ? L(0, 'Temperature (raw model)', colors.grey, (x) => uv('temp', x.tempRawC), { dashed: true, width: 1.1 }) : null,
    L(0, 'Dew point', colors.teal, (x) => uv('temp', x.dewPointC), { dashed: true }),
    L(1, 'Pressure', colors.violet, (x) => uv('pressure', x.pressureMsl, pd), { width: 2.1 }),
    L(2, 'Wind', colors.green, (x) => uv('wind', x.windKmh, 0)),
    L(2, 'Gusts', colors.green, (x) => uv('wind', x.gustKmh, 0), { dashed: true, width: 1 }),
    L(3, 'Cloud cover', colors.grey, (x) => round(x.cloud, 0), { area: true, width: 1.2 }),
    L(3, 'Low cloud', colors.text4, (x) => round(x.cloudLow, 0), { width: 1, dashed: true }),
    L(3, 'Precip. probability', colors.blue, (x) => round(x.precipProb, 0), { width: 1.8 }),
    L(3, 'Precipitation', colors.cyan, (x) => (x.precipMm > 0 ? uv('precip', x.precipMm, 2) : null), { type: 'bar', y2: true }),
    L(4, 'Fog risk', colors.grey, (x) => pc(x.fogRisk), { width: 1.8 }),
    L(4, 'Inversion risk', colors.cyan, (x) => pc(x.inversionRisk), { width: 2, area: true }),
    L(4, 'Convective risk', colors.red, (x) => pc(x.convectiveRisk), { width: 1.8 }),
  ].filter(Boolean);
  series[0].markArea = { silent: true, data: nights };
  const fmt = {
    Temperature: (v) => units.num(v, 1) + ' ' + tl, 'Temperature (raw model)': (v) => units.num(v, 1) + ' ' + tl, 'Dew point': (v) => units.num(v, 1) + ' ' + tl,
    Pressure: (v) => units.num(v, pd) + ' ' + pl, Wind: wl, Gusts: wl, 'Cloud cover': '%', 'Low cloud': '%', 'Precip. probability': '%',
    Precipitation: (v) => units.num(v, 2) + ' ' + rl, 'Fog risk': '%', 'Inversion risk': '%', 'Convective risk': '%',
  };
  const tip = timeTooltip(fmt);
  return {
    animation: false,
    legend: { show: true, top: 0, left: 0, type: 'scroll' },
    tooltip: {
      trigger: 'axis', confine: true,
      formatter: (ps) => {
        const list = Array.isArray(ps) ? ps : [ps];
        const t = list.length && Array.isArray(list[0].value) ? list[0].value[0] : null;
        const x = ok(t) ? hrs.find((q) => q.t === t) : null;
        return tip(ps) + (x && x.text ? `<div class="wx-tt-text">${esc(x.text)}</div>` : '');
      },
    },
    axisPointer: { link: [{ xAxisIndex: 'all' }] },
    dataZoom: [{ type: 'inside', xAxisIndex: rows.map((_, i) => i), throttle: 50 }],
    ...g,
    series,
  };
}
