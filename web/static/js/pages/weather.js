// pages/weather.js - Weather & Propagation.
//
// Top to bottom: current conditions (Open-Meteo), sun card, radio propagation (refractivity, k-factor,
// class gauge, radio horizon, M-profile duct diagram, refractivity time line), vertical profiles with
// the aircraft-derived (Mode S EHS / MRAR) profile overlaid on the model, winds aloft, time-height
// history, weather history aligned with range metrics and the weather-impact analysis.
//
// Endpoints: /api/weather/current, /hourly, /profile, /ducting, /radar, /impact, /refresh (POST),
// /api/met/profile, /compare, /history, /api/astro/now, /api/stats/range, /api/forecast/reception
// (antenna height only). The profile and analysis sections live in ../weather/profile.js and
// ../weather/impact.js; shared helpers in ../weather/common.js.

import { createAdmin } from '../admin.js';
import {
  HOUR, FT_PER_M, ok, clamp, esc, hm, dayTime, fmtDelta, signed, uv, round, horizonKm, classInfo, INDEX_ZONES,
  caption, panel, source, soft, attribution, compassSvg, trendArrow, gaugeOption, nowLine, timeTooltip, colors,
} from '../weather/common.js';
import { mountProfiles } from '../weather/profile.js';
import { mountImpact } from '../weather/impact.js';

const HORIZON_ALTS_FT = [1000, 5000, 10000, 20000, 30000, 35000, 40000, 45000];

export default {
  id: 'weather',
  title: 'Weather & Propagation',

  async mount(container, ctx) {
    const { api, units, ui, charts } = ctx;
    const { h } = ui;
    const admin = createAdmin(ctx);
    await ui.loadCss(new URL('../../css/weather.css', import.meta.url));

    const panels = [];
    const reg = (p) => { panels.push(p); return p; };
    const refreshers = [];

    // ------------------------------------------------------------------ page head
    const statusEl = h('div.sub', 'Open-Meteo model · aircraft-derived profile · 1090 MHz refractivity');
    const refreshBtn = ui.button({
      label: 'Fetch now', icon: 'refresh', size: 'sm', variant: 'ghost', tip: 'Download the weather now instead of waiting for the next poll',
      onClick: async () => {
        refreshBtn.setLoading(true);
        try {
          await admin.run(() => api.post('/api/weather/refresh', null, { signal: ctx.signal, timeout: 40000 }), 'Fetching the weather now needs the admin password.');
          ui.toast.success('Weather updated');
        } catch (e) {
          if (!e.aborted && !e.cancelled) ui.toast.error(e.message || 'Weather refresh failed');
        } finally {
          refreshBtn.setLoading(false);
          for (const r of refreshers) r();
        }
      },
    });
    const banner = h('div.wx-banner.hidden');
    const attribEl = h('div');

    // ------------------------------------------------------------------ conditions tiles
    const T = {
      temp: ui.stat({ label: 'Temperature', icon: 'thermo', accent: 'cyan' }),
      dew: ui.stat({ label: 'Dew point', icon: 'rain' }),
      rh: ui.stat({ label: 'Humidity', icon: 'rain' }),
      pres: ui.stat({ label: 'Pressure (MSL)', icon: 'timer' }),
      wind: ui.stat({ label: 'Wind', icon: 'wind', class: 'wx-stat-wind' }),
      cloud: ui.stat({ label: 'Cloud cover', icon: 'cloud' }),
      precip: ui.stat({ label: 'Precipitation', icon: 'rain' }),
      vis: ui.stat({ label: 'Visibility', icon: 'eye' }),
      cape: ui.stat({ label: 'CAPE', icon: 'bolt', tip: 'Convective available potential energy: fuel for showers and thunderstorms' }),
      sun: ui.stat({ label: 'Day / night', icon: 'sun' }),
    };
    const compassHost = h('div.wx-compass-host');
    T.wind.append(compassHost);
    const cloudBars = h('div.wx-cloudbars');
    const tilesEl = h('div.wx-tiles', Object.values(T));
    let tendency = null; // { d3h, base } from the hourly record
    let lastCurrent = null;
    let lastAstro = null;

    function renderTiles() {
      const d = lastCurrent;
      if (!d) return;
      const c = d.current || (d.hour ? { ...d.hour, dewPointC: d.hour.dewpointC } : null);
      const fromHour = !d.current && !!d.hour;
      for (const t of Object.values(T)) t.classList.toggle('wx-stale', !c);
      if (!c) {
        for (const [k, t] of Object.entries(T)) if (k !== 'sun') { t.set(null, ''); t.setSub(''); }
        ui.clear(compassHost);
        return;
      }
      T.temp.set(units.format('temp', c.tempC, { unit: false }), units.label('temp'));
      T.temp.setSub([d.text, ok(c.apparentC) ? 'feels ' + units.temp(c.apparentC) : null].filter(Boolean).join(' · ') || (fromHour ? 'hourly value' : ''));
      T.dew.set(units.format('temp', c.dewPointC, { unit: false }), units.label('temp'));
      const spread = ok(c.tempC) && ok(c.dewPointC) ? c.tempC - c.dewPointC : null;
      T.dew.setSub(ok(spread) ? `spread ${fmtDelta('temp', spread).replace('+', '')}${spread < 2 ? ' · fog / low cloud possible' : ''}` : '');
      T.rh.set(units.num(c.rh, 0), '%');
      T.rh.setSub(!ok(c.rh) ? '' : c.rh >= 90 ? 'saturated air' : c.rh >= 70 ? 'moist' : c.rh >= 40 ? 'moderate' : 'dry');
      T.pres.set(units.format('pressure', c.pressureMsl, { unit: false }), units.label('pressure'));
      if (tendency && ok(tendency.d3h)) {
        const a = Math.abs(tendency.d3h);
        const word = a < 0.5 ? 'steady' : (tendency.d3h > 0 ? 'rising' : 'falling') + (a >= 3.5 ? ' rapidly' : a >= 1.6 ? '' : ' slowly');
        T.pres.setSub(h('span', h('span.wx-arrow' + (tendency.d3h > 0.5 ? '.up' : tendency.d3h < -0.5 ? '.down' : ''), trendArrow(tendency.d3h, 0.5)), ` ${fmtDelta('pressure', tendency.d3h, units.system === 'imperial' ? 2 : 1)} / 3 h · ${word}`));
      } else {
        T.pres.setSub(ok(c.surfacePressure) ? 'station ' + units.pressure(c.surfacePressure) : '3 h tendency needs 3 h of data');
      }
      T.wind.set(units.format('wind', c.windKmh, { unit: false }), units.label('wind'));
      T.wind.setSub([ok(c.windDir) ? 'from ' + units.bearing(c.windDir) : null, ok(c.gustKmh) ? 'gusts ' + units.wind(c.gustKmh) : null].filter(Boolean).join(' · '));
      ui.mount(compassHost, compassSvg(ok(c.windKmh) && c.windKmh >= 0.5 ? c.windDir : null));
      T.cloud.set(units.num(c.cloud, 0), '%');
      ui.mount(cloudBars, [['high', c.cloudHigh], ['mid', c.cloudMid], ['low', c.cloudLow]].map(([n, v]) =>
        h('div.wx-cloudbar', { tip: `${n} cloud ${units.pct(v)}` }, h('span', n), h('i', { style: { '--pct': clamp(v || 0, 0, 100) + '%' } }), h('b.num', units.pct(v)))));
      T.cloud.setSub(cloudBars);
      T.precip.set(units.format('precip', c.precipMm, { unit: false }), units.label('precip'));
      const parts = [];
      if (c.rainMm > 0) parts.push('rain ' + units.precip(c.rainMm));
      if (c.showersMm > 0) parts.push('showers ' + units.precip(c.showersMm));
      if (c.snowCm > 0) parts.push('snow ' + units.precip(c.snowCm * 10));
      T.precip.setSub(parts.length ? parts.join(' · ') : ok(c.precipMm) ? 'dry in the last hour' : '');
      T.vis.set(units.format('visibility', c.visibilityM, { unit: false }), units.label('visibility'));
      T.vis.setSub(!ok(c.visibilityM) ? '' : c.visibilityM < 1000 ? 'fog' : c.visibilityM < 5000 ? 'mist / haze' : c.visibilityM < 10000 ? 'moderate' : 'clear');
      T.cape.set(units.num(c.cape, 0), 'J/kg');
      T.cape.setSub(!ok(c.cape) ? '' : c.cape < 100 ? 'stable air' : c.cape < 1000 ? 'weak instability' : c.cape < 2500 ? 'moderate: showers possible' : 'strong: thunderstorms possible');
      renderSunTile();
    }

    function renderSunTile() {
      const a = lastAstro;
      const c = lastCurrent && (lastCurrent.current || lastCurrent.hour);
      if (a && a.located && a.position) {
        const ph = ctx.geo.dayPhase(a.position.elevation);
        T.sun.set(ph.label, '');
        const e = a.events || {};
        const e2 = a.tomorrow || {};
        const next = [['sunrise', e.sunrise], ['sunset', e.sunset], ['civil dawn', e.civilDawn], ['civil dusk', e.civilDusk],
          ['civil dawn', e2.civilDawn], ['sunrise', e2.sunrise]]
          .filter(([, t]) => t > a.t).sort((x, y) => x[1] - y[1])[0];
        T.sun.setSub(`sun ${signed(a.position.elevation, 1)}°` + (next ? ` · ${next[0]} ${hm(next[1])}` : ''));
      } else if (c && ok(c.isDay)) {
        T.sun.set(c.isDay ? 'Day' : 'Night', '');
        T.sun.setSub('');
      } else {
        T.sun.set(null, '');
        T.sun.setSub(a && !a.located ? 'receiver not located' : '');
      }
    }

    refreshers.push(source(ctx, {
      every: 60000,
      load: (signal) => api.get('/api/weather/current', { signal }),
      onData(d) {
        lastCurrent = d;
        renderTiles();
        const st = d.status || {};
        const bits = [];
        if (d.model) bits.push('model ' + d.model);
        if (d.fetched) bits.push('updated ' + units.ago(d.fetched, ctx.live.serverNow()));
        if (st.nextPoll) bits.push('next poll ' + hm(st.nextPoll));
        if (ok(d.elevationM)) bits.push('grid elevation ' + units.height(d.elevationM, { digits: 0 }));
        ui.setText(statusEl, bits.length ? bits.join(' · ') : 'Open-Meteo model · aircraft-derived profile · 1090 MHz refractivity');
        let msg = null;
        if (!d.enabled) msg = { title: 'Weather download is switched off', text: 'Enable it in Settings → Weather to see conditions, radio propagation and the weather-impact analysis. Aircraft-derived winds and temperatures below still work without it.' };
        else if (!d.located) msg = { title: 'Receiver position unknown', text: 'Weather is fetched for the receiver location. Set the position in Settings (or let the location guesser find it) first.' };
        else if (!d.current && !d.hour) msg = { title: 'Waiting for the first weather download', text: st.lastError ? 'Last attempt failed: ' + st.lastError : 'Open-Meteo is polled every ' + (st.intervalMin || 15) + ' minutes; the first values appear shortly after start.' };
        else if (st.failures > 0 && st.lastError) msg = { title: 'Weather download is failing', text: `${st.failures} failed attempt${st.failures > 1 ? 's' : ''}: ${st.lastError}. Showing the last values received.` };
        banner.classList.toggle('hidden', !msg);
        if (msg) ui.mount(banner, ui.icon('alert'), h('div', h('b', msg.title), h('div.muted', msg.text)), (!d.enabled || !d.located) ? ui.button({ label: 'Settings', size: 'sm', icon: 'settings', onClick: () => ctx.navigate('settings') }) : null);
        refreshBtn.disabled = !d.enabled || !d.located;
        renderAttribution();
      },
      onError(e) {
        if (!lastCurrent) {
          banner.classList.remove('hidden');
          ui.mount(banner, ui.icon('alert'), h('div', h('b', 'Current conditions could not be loaded'), h('div.muted', e.message || String(e))));
        }
      },
    }));

    // pressure tendency from the station's hourly record
    refreshers.push(source(ctx, {
      every: 300000,
      load: (signal) => api.get('/api/weather/hourly', { params: { hours: 7, ahead: 0 }, signal }),
      onData(d) {
        const obs = (d.obs || []).filter((r) => ok(r.pressureMsl));
        tendency = null;
        if (obs.length >= 2) {
          const last = obs[obs.length - 1];
          let ref = null;
          for (const r of obs) if (Math.abs(last.t - r.t - 3 * HOUR) <= 0.5 * HOUR) ref = r;
          if (ref) tendency = { d3h: last.pressureMsl - ref.pressureMsl };
        }
        renderTiles();
      },
    }));

    // ------------------------------------------------------------------ sun card
    const sunCard = ui.card({ title: 'Sun', icon: 'sun', sub: 'Elevation through today (browser local time)' });
    const sunPanel = reg(panel(sunCard.body, {
      skeleton: 120,
      empty: (a) => (!a.located || !a.position ? { icon: 'pin', title: 'Receiver position unknown', text: 'Sunrise, sunset and twilight times need the receiver location.' } : null),
      build(el) {
        const arc = h('div.wx-sunarc');
        const kvHost = h('div.wx-sun-times');
        el.append(arc, kvHost, caption('The curve is the sun’s height above the horizon today; the shaded band below the horizon line is twilight (civil, nautical, astronomical). Night-time cooling of the ground is what builds the inversions that stretch ADS-B range.'));
        return (a) => {
          arc.innerHTML = sunArcSvg(ctx.geo, a);
          const e = a.events || {};
          const t = (v) => (v > 0 ? hm(v) : units.DASH);
          const rows = [
            ['Astro dawn', t(e.astroDawn)], ['Astro dusk', t(e.astroDusk)],
            ['Nautical dawn', t(e.nauticalDawn)], ['Nautical dusk', t(e.nauticalDusk)],
            ['Civil dawn', t(e.civilDawn)], ['Civil dusk', t(e.civilDusk)],
            ['Sunrise', t(e.sunrise)], ['Sunset', t(e.sunset)],
            ['Solar noon', t(e.solarNoon)], ['Day length', e.polarDay ? '24 h' : e.polarNight ? '0 h' : ok(e.dayLengthSec) ? units.age(e.dayLengthSec) : units.DASH],
            ['Sun now', `${signed(a.position.elevation, 1)}° · ${units.bearing(a.position.azimuth)}`], ['Noon elevation', ok(e.noonElevation) ? units.num(e.noonElevation, 1) + '°' : units.DASH],
            a.moon ? ['Moon', a.moon.name, { text: true }] : null, a.moon ? ['Illuminated', units.pct(a.moon.illumination * 100)] : null,
          ];
          ui.mount(kvHost, ui.kv(rows, { columns: 2 }));
        };
      },
    }));
    refreshers.push(source(ctx, {
      every: 60000, panels: [sunPanel],
      load: async (signal) => {
        const [a, days] = await Promise.all([api.get('/api/astro/now', { signal }), soft(api.get('/api/astro/days', { params: { days: 2 }, signal }))]);
        if (a && days && (days.days || []).length > 1) a.tomorrow = days.days[1];
        return a;
      },
      onData(a) { lastAstro = a; renderSunTile(); },
    }));

    // ------------------------------------------------------------------ propagation
    const propCard = ui.card({ title: 'Radio propagation now', icon: 'signal', accent: true, sub: 'How the lower atmosphere bends 1090 MHz' });
    const propPanel = reg(panel(propCard.body, {
      skeleton: 220,
      empty: (d) => (noAtmos(d) ? { icon: 'cloud', title: 'No refractivity data yet', text: 'The propagation state is computed from the Open-Meteo surface values and pressure-level profile. It appears after the first weather download.' } : null),
      build(el) {
        const gaugeEl = h('div.chart.wx-gauge');
        const classEl = h('div.wx-class');
        const kvHost = h('div');
        const explainEl = h('div.wx-explain');
        const horizonEl = h('div.chart', { style: { '--h': '250px' } });
        el.append(h('div.wx-prop',
          h('div.wx-prop-gauge', gaugeEl, classEl, ui.legend(INDEX_ZONES.map((z) => ({ label: classInfo(z.name).label, color: z.color })))),
          h('div.wx-prop-kv', ui.section('Refractivity', 'layers'), kvHost),
          h('div.wx-prop-text', ui.section('What it means for ADS-B range', 'radar'), explainEl)),
        h('div.wx-prop-horizon', ui.section('Radio horizon: now versus standard atmosphere', 'ruler'), horizonEl,
          caption('Line-of-sight distance to an aircraft at each altitude over a smooth earth (antenna height above sea level included). “Now” applies today’s k-factor; bars longer than “standard” mean the atmosphere is carrying signals further than geometry alone. Terrain and obstructions can only shorten these figures.')));
        const gauge = charts.create(gaugeEl);
        const horizon = charts.create(horizonEl);
        return (d) => {
          const a = d.duct.current || {};
          const ci = classInfo(a.class);
          gauge.set(gaugeOption(a.index, INDEX_ZONES, 'propagation index'), { replace: true });
          ui.mount(classEl, ui.badge(ci.label, ci.badge, { }), h('span.muted', ` ${ci.text}`));
          const pct = ok(a.horizonFactor) ? (a.horizonFactor - 1) * 100 : null;
          const ductTxt = a.ducts > 0
            ? `${a.ducts} · ${units.height(a.ductBaseM, { digits: 0 })} – ${units.height(a.ductTopM, { digits: 0 })}`
            : 'none';
          ui.mount(kvHost, ui.kv([
            ['Surface refractivity N', units.num(a.nSurface, 1) + ' N', { tip: 'Refractivity at the ground: (n − 1) × 10⁶. Around 315 N at sea level in mid-latitudes; humid, warm air raises it.' }],
            ['Gradient dN/dh (lowest km)', signed(a.dNdh, 1) + ' N/km', { tip: 'Standard atmosphere: about −40 N/km. Below −79 = super-refraction, below −157 = ducting, above 0 = sub-refraction.' }],
            ['Steepest layer', ok(a.minGradient) ? `${signed(a.minGradient, 0)} N/km at ${units.height(a.minGradientM, { digits: 0 })}` : units.DASH, { tip: 'Most negative layer gradient below 3 km and the height (above sea level) of its base' }],
            ['k-factor', ok(a.k) ? (a.k > 50 ? '∞ (trapping)' : units.num(a.k, 2, 2)) : units.DASH, { tip: 'Effective earth-radius factor. 4/3 (1.33) is the standard atmosphere; larger = waves follow the earth’s curve further.' }],
            ['Radio horizon vs standard', ok(pct) ? signed(pct, 1) + ' %' : units.DASH],
            ['Ducts', ductTxt],
            a.ducts > 0 ? ['Strongest duct', `${units.num(a.ductStrengthM, 1)} M-units${a.ductKind ? ' · ' + a.ductKind : ''}${a.ducting1090 ? ' · traps 1090 MHz' : ' · too thin for 1090 MHz'}`, { text: true }] : null,
            ['Low-level inversion', ok(a.inversionC) ? (a.inversionC > 0 ? fmtDelta('temp', a.inversionC) + ' warmer aloft' : 'none') : units.DASH, { tip: 'Temperature excess of the warmest level in the lowest 1.5 km over the 2 m temperature' }],
            ['Basis', a.hasProfile ? 'pressure-level profile' : a.estimated ? 'surface estimate only' : 'stored surface columns', { text: true }],
          ]));
          const antM = antennaAmsl(d);
          ui.mount(explainEl, explain(a, antM, units).map((t) => h('p', t)), a.summary ? h('p.muted', a.summary) : null);
          const k = ok(a.horizonFactor) ? a.horizonFactor : 1;
          const std = HORIZON_ALTS_FT.map((ft) => horizonKm(antM, ft / FT_PER_M));
          const opt = charts.bar({
            x: HORIZON_ALTS_FT.map((ft) => units.alt(ft, { fl: true })), horizontal: true, legend: true,
            series: [
              { name: 'Standard atmosphere (k = 4/3)', data: std.map((v) => uv('dist', v, 0)), color: 'rgba(132,148,179,0.55)' },
              { name: 'Now', data: std.map((v) => uv('dist', v * k, 0)), color: ci.color },
            ],
            valueFormat: (v) => units.num(v, 0) + ' ' + units.label('dist'),
          });
          opt.xAxis.name = units.label('dist');
          opt.xAxis.nameLocation = 'end';
          opt.xAxis.nameGap = 6;
          opt.grid.right = 36;
          horizon.set(opt, { replace: true });
        };
      },
    }));

    let ductTop = 'low';
    const ductSeg = ui.segmented({ options: [{ value: 'low', label: 'Lowest 3 km' }, { value: 'all', label: 'Full profile' }], value: ductTop, onChange: (v) => { ductTop = v; ductPanel.rerender(); } });
    const ductCard = ui.card({ title: 'Duct diagram', icon: 'layers', sub: 'Modified refractivity M against height (model profile)', actions: ductSeg });
    const ductPanel = reg(panel(ductCard.body, {
      skeleton: 300,
      empty: (d) => (!(d.profile?.points || []).length ? { icon: 'layers', title: 'No vertical profile yet', text: 'The M-profile needs the pressure-level data of the weather model (temperature, humidity and height per level). It arrives with the first weather download.' } : null),
      build(el) {
        const chartEl = h('div.chart', { style: { '--h': '340px' } });
        const listEl = h('div.wx-ductlist');
        el.append(chartEl, caption('M normally increases with height (line leaning right). Wherever it leans back to the left, radio waves are bent down faster than the earth curves away: a trapping layer. The duct reaches from the top of that layer down to where M has the same value again. No shaded band = no duct, which is the usual state.'), listEl);
        const chart = charts.create(chartEl);
        return (d) => {
          chart.set(ductOption(d, ductTop, units), { replace: true });
          ui.mount(listEl, ductList(d, ctx));
        };
      },
    }));

    const lineCard = ui.card({ title: 'Refractivity time line', icon: 'history', sub: 'Last 48 h observed, next 72 h from the forecast' });
    const linePanel = reg(panel(lineCard.body, {
      skeleton: 260,
      empty: (d) => (!(d.duct.hours || []).length ? { icon: 'history', title: 'No hourly refractivity yet', text: 'One point per hour is stored with every weather download; the time line fills as the station runs.' } : null),
      build(el) {
        const chartEl = h('div.chart', { style: { '--h': '280px' } });
        const sumEl = h('div.wx-inline-stats');
        el.append(chartEl, sumEl, caption('Propagation index per hour: 50 is a standard atmosphere, above 65 the horizon is noticeably extended (super-refraction), 85 and more means ducting is likely. The dashed line is the refractivity gradient that drives it; diamonds mark hours with a duct in the profile.'));
        const chart = charts.create(chartEl);
        return (d) => {
          chart.set(timelineOption(d.duct, units), { replace: true });
          const hrs = d.duct.hours || [];
          const fut = hrs.filter((x) => x.t >= d.duct.now - HOUR);
          const peak = fut.reduce((m, x) => (!m || x.index > m.index ? x : m), null);
          ui.mount(sumEl,
            h('span', 'Hours with a duct in this window: ', h('b.num', units.int(d.duct.ductHours ?? hrs.filter((x) => x.ducts > 0).length))),
            peak ? h('span', 'Highest index ahead: ', h('b.num', String(peak.index)), ` (${classInfo(peak.class).label.toLowerCase()}, ${dayTime(peak.t)})`) : null);
        };
      },
    }));

    refreshers.push(source(ctx, {
      every: 120000, panels: [propPanel, ductPanel, linePanel],
      load: async (signal) => {
        const [duct, profile, reception] = await Promise.all([
          api.get('/api/weather/ducting', { signal }),
          soft(api.get('/api/weather/profile', { signal })),
          soft(api.get('/api/forecast/reception', { signal })),
        ]);
        return { duct, profile, reception };
      },
    }));

    // ------------------------------------------------------------------ attribution (Open-Meteo, RainViewer)
    let radar = null;
    function renderAttribution() {
      const d = lastCurrent || {};
      ui.mount(attribEl, attribution([
        { text: d.attribution || 'Weather data by Open-Meteo.com (CC BY 4.0)', url: d.attributionUrl || 'https://open-meteo.com/' },
        {
          text: radar?.attribution || 'Radar data by RainViewer', url: radar?.attributionUrl || 'https://www.rainviewer.com/',
          note: radar ? (radar.available ? `(precipitation radar overlay on the Live Map, newest frame ${hm(radar.generated || radar.fetched)})` : radar.enabled ? '(radar overlay enabled, waiting for the first frame index)' : '(precipitation radar overlay for the Live Map, currently switched off)') : '',
        },
        { text: 'Winds and temperatures aloft derived from Mode S EHS / MRAR replies of aircraft in range' },
      ]));
    }
    renderAttribution();
    refreshers.push(source(ctx, {
      every: 600000,
      load: (signal) => api.get('/api/weather/radar', { signal }),
      onData(r) { radar = r; renderAttribution(); },
    }));

    // ------------------------------------------------------------------ layout
    const profileHost = h('div.wx-section');
    const impactHost = h('div.wx-section');
    container.append(h('div.page-inner.wx',
      h('div.page-head', h('div', h('h2', 'Weather & Propagation'), statusEl), h('div.actions', refreshBtn)),
      banner,
      tilesEl,
      h('div.wx-grid.wx-grid--prop', propCard, sunCard),
      h('div.wx-grid.wx-grid--2', ductCard, lineCard),
      profileHost,
      impactHost,
      attribEl));

    mountProfiles(profileHost, ctx, { reg, refreshers });
    mountImpact(impactHost, ctx, { reg, refreshers });

    ctx.onCleanup(units.onChange(() => {
      renderTiles();
      for (const p of panels) p.rerender();
    }));
  },
};

// -------------------------------------------------------------------------------------------------
// helpers
// -------------------------------------------------------------------------------------------------

function noAtmos(d) {
  const a = d.duct && d.duct.current;
  return !a || (!(d.duct.hours || []).length && !a.hasProfile && !ok(a.nSurface)) || (!a.class && !ok(a.index));
}

function antennaAmsl(d) {
  if (d.reception && ok(d.reception.antennaAmslM) && d.reception.antennaAmslM > 0) return d.reception.antennaAmslM;
  if (ok(d.duct.stationElevM)) return d.duct.stationElevM + 8;
  return 10;
}

/** Plain-language reading of the propagation state for a hobbyist. */
function explain(a, antM, units) {
  const out = [];
  const pct = ok(a.horizonFactor) ? (a.horizonFactor - 1) * 100 : 0;
  const std350 = horizonKm(antM, 35000 / FT_PER_M);
  const now350 = std350 * (ok(a.horizonFactor) ? a.horizonFactor : 1);
  const duct = a.ducts > 0;
  if (duct && a.ducting1090) {
    out.push(`A duct strong enough to trap 1090 MHz lies between ${units.height(a.ductBaseM, { digits: 0 })} and ${units.height(a.ductTopM, { digits: 0 })} above sea level. Signals from aircraft inside or skimming that layer can arrive from far beyond the normal radio horizon: expect occasional very distant, low-altitude contacts and an unusually long maximum range, often only in some directions.`);
    out.push('Ducted signals fade in and out quickly, so far-away aircraft may appear with few positions. High-altitude traffic is hardly affected: it is already limited by geometry, not by the atmosphere.');
  } else if (duct) {
    out.push(`The profile contains a duct (${units.height(a.ductBaseM, { digits: 0 })} – ${units.height(a.ductTopM, { digits: 0 })}), but it is too shallow to trap the 27.5 cm wavelength of ADS-B efficiently. Some extra range at low elevation angles is possible, spectacular long-distance reception is unlikely.`);
  } else if (a.class === 'super-refraction') {
    out.push(`Super-refraction: radio waves are bent towards the ground more strongly than usual, so the radio horizon is about ${units.num(Math.abs(pct), 0)} % further away. Low-flying aircraft near the horizon show up a little earlier and the maximum range grows slightly.`);
    out.push('This is typical for calm, clear nights and mornings (temperature inversion) or for moist air under a dry layer.');
  } else if (a.class === 'sub-refraction') {
    out.push(`Sub-refraction: radio waves bend away from the ground, the radio horizon is about ${units.num(Math.abs(pct), 0)} % closer than in a standard atmosphere. The furthest low-altitude contacts are lost a little earlier; nearby and high traffic is unaffected.`);
  } else if (a.class === 'ducting') {
    out.push('The gradient in the lowest kilometre is steep enough for ducting: signals can follow the earth’s curvature and arrive from well beyond the usual horizon, mainly from low-flying aircraft.');
  } else {
    out.push(`Nothing unusual: the atmosphere bends 1090 MHz about as much as the textbook standard (${pct >= 0 ? '+' : '−'}${units.num(Math.abs(pct), 1)} % on the radio horizon). Range is set by geometry right now - aircraft altitude, antenna height, terrain and obstructions - not by the weather.`);
  }
  out.push(`For an airliner at ${units.alt(35000, { fl: true })} the line-of-sight limit is about ${units.dist(now350, { digits: 0 })} now, against ${units.dist(std350, { digits: 0 })} in a standard atmosphere.`);
  if (!a.hasProfile) out.push('Note: no pressure-level profile was available for this hour, so the gradient is estimated from surface values and elevated ducts cannot be detected.');
  return out;
}

/** SVG of today's sun elevation curve with horizon, twilight band and the current position. */
function sunArcSvg(geo, a) {
  const W = 360;
  const H = 132;
  const lat = a.receiver.lat;
  const lon = a.receiver.lon;
  const d0 = new Date(a.t);
  d0.setHours(0, 0, 0, 0);
  const t0 = +d0;
  const DAY = 24 * HOUR;
  const pts = [];
  let maxEl = -90;
  let minEl = 90;
  for (let i = 0; i <= 144; i++) {
    const t = t0 + (i * DAY) / 144;
    const el = geo.sunElevation(lat, lon, t);
    pts.push([t, el]);
    if (el > maxEl) maxEl = el;
    if (el < minEl) minEl = el;
  }
  const yMax = Math.max(12, maxEl + 8);
  const yMin = Math.max(-34, Math.min(-22, minEl - 4));
  const X = (t) => ((t - t0) / DAY) * W;
  const Y = (el) => 6 + (H - 22) * (1 - (clamp(el, yMin, yMax) - yMin) / (yMax - yMin));
  const y0 = Y(-0.833);
  const path = pts.map(([t, el], i) => `${i ? 'L' : 'M'}${X(t).toFixed(1)} ${Y(el).toFixed(1)}`).join(' ');
  const e = a.events || {};
  const vline = (t, cls) => (t > t0 && t < t0 + DAY ? `<line x1="${X(t).toFixed(1)}" x2="${X(t).toFixed(1)}" y1="${(y0 - 5).toFixed(1)}" y2="${(y0 + 5).toFixed(1)}" class="${cls}"/>` : '');
  const hours = [0, 6, 12, 18, 24].map((hr) => `<text x="${clamp((hr / 24) * W, 8, W - 8).toFixed(0)}" y="${H - 3}" class="wx-sun-tick">${hr < 10 ? '0' + hr : hr}</text>`).join('');
  const el = a.position.elevation;
  const sx = X(a.t);
  const sy = Y(el);
  const up = el > -0.833;
  return `<svg viewBox="0 0 ${W} ${H}" preserveAspectRatio="none" role="img" aria-label="Sun elevation today">
    <defs>
      <linearGradient id="wxSunDay" x1="0" y1="0" x2="0" y2="1"><stop offset="0" stop-color="#fbbf24" stop-opacity="0.34"/><stop offset="1" stop-color="#fbbf24" stop-opacity="0.02"/></linearGradient>
      <clipPath id="wxSunAbove"><rect x="0" y="0" width="${W}" height="${y0.toFixed(1)}"/></clipPath>
    </defs>
    <rect x="0" y="${y0.toFixed(1)}" width="${W}" height="${(Y(-18) - y0).toFixed(1)}" class="wx-sun-twilight"/>
    <rect x="0" y="${Y(-18).toFixed(1)}" width="${W}" height="${Math.max(0, H - 16 - Y(-18)).toFixed(1)}" class="wx-sun-night"/>
    <path d="${path} L${W} ${H} L0 ${H} Z" fill="url(#wxSunDay)" clip-path="url(#wxSunAbove)"/>
    <path d="${path}" class="wx-sun-curve"/>
    <line x1="0" x2="${W}" y1="${y0.toFixed(1)}" y2="${y0.toFixed(1)}" class="wx-sun-horizon"/>
    ${vline(e.sunrise, 'wx-sun-mark')}${vline(e.sunset, 'wx-sun-mark')}${vline(e.solarNoon, 'wx-sun-mark wx-sun-mark--noon')}
    <circle cx="${sx.toFixed(1)}" cy="${sy.toFixed(1)}" r="9" class="wx-sun-glow${up ? '' : ' down'}"/>
    <circle cx="${sx.toFixed(1)}" cy="${sy.toFixed(1)}" r="4.2" class="wx-sun-dot${up ? '' : ' down'}"/>
    ${hours}
  </svg>`;
}

/** M (and N) against height with shaded non-standard layers and ducts. */
function ductOption(d, mode, units) {
  const prof = d.profile;
  const elev = ok(d.duct.stationElevM) ? d.duct.stationElevM : null;
  const topM = mode === 'low' ? 3000 + (elev || 0) : Infinity;
  const all = (prof.points || []).filter((p) => ok(p.heightM) && ok(p.m));
  let pts = all.filter((p) => p.heightM <= topM);
  if (pts.length < all.length && pts.length) pts = all.slice(0, pts.length + 1); // one level above the cut so the line reaches the edge
  if (pts.length < 2) pts = all;
  const hv = (m) => uv('height', m, 0);
  const yMax = mode === 'low' ? hv(topM) : undefined;
  const base = pts[0];
  const top = pts[pts.length - 1];
  const ref = [[round(base.m, 1), hv(base.heightM)], [round(base.m + 0.117 * (Math.min(top.heightM, topM) - base.heightM), 1), hv(Math.min(top.heightM, topM))]];
  const areas = [];
  for (const l of prof.layers || []) {
    if (!l.class || l.class === 'standard' || l.baseM > topM) continue;
    const ci = classInfo(l.class);
    areas.push([{ yAxis: hv(l.baseM), itemStyle: { color: ci.color, opacity: 0.13 }, name: ci.label, label: { show: true, position: 'insideRight', color: ci.color, fontSize: 10 } }, { yAxis: hv(Math.min(l.topM, topM)) }]);
  }
  for (const k of prof.ducts || []) {
    if (k.baseM > topM) continue;
    areas.push([{ yAxis: hv(k.baseM), itemStyle: { color: colors.violet, opacity: 0.3 }, name: (k.kind ? k.kind + ' ' : '') + 'duct' + (k.traps1090 ? ' · traps 1090 MHz' : ''), label: { show: true, position: 'insideLeft', color: '#e9defc', fontSize: 10.5, fontWeight: 600 } }, { yAxis: hv(Math.min(k.topM, topM)) }]);
  }
  const hl = units.label('height');
  return {
    animationDuration: 250,
    grid: { left: 10, right: 18, top: 58, bottom: 28, containLabel: true },
    legend: { show: true, top: 0, left: 0, type: 'scroll' },
    tooltip: {
      trigger: 'item', confine: true,
      formatter: (p) => `${esc(p.seriesName)}<br><b>${units.num(p.value[0], 1)}</b> ${p.seriesName.startsWith('N') ? 'N' : 'M'}-units at <b>${units.num(p.value[1], 0)} ${hl}</b>`,
    },
    xAxis: { type: 'value', scale: true, name: 'N / M units', nameLocation: 'middle', nameGap: 24, splitLine: { show: true, lineStyle: { color: colors.line, type: [2, 4] } } },
    yAxis: { type: 'value', name: `height AMSL, ${hl}`, nameTextStyle: { align: 'left' }, nameGap: 10, min: hv(Math.min(base.heightM, elev ?? base.heightM)) < 0 ? undefined : 0, max: yMax, axisLabel: { formatter: (v) => units.num(v, 0) } },
    series: [
      {
        name: 'M (modified refractivity)', type: 'line', color: colors.cyan, showSymbol: true, symbolSize: 5, lineStyle: { width: 2.2 },
        data: pts.map((p) => [round(p.m, 1), hv(p.heightM)]),
        markArea: { silent: true, data: areas },
        markLine: elev !== null ? { silent: true, symbol: 'none', lineStyle: { color: colors.text3, type: 'dashed' }, label: { formatter: 'station', position: 'insideEndTop', color: colors.text3, fontSize: 10 }, data: [{ yAxis: hv(elev) }] } : undefined,
      },
      { name: 'Standard slope (+117 M/km)', type: 'line', color: colors.grey, showSymbol: false, silent: true, lineStyle: { width: 1, type: 'dashed' }, data: ref },
      { name: 'N (refractivity)', type: 'line', color: colors.blue, showSymbol: true, symbolSize: 3, lineStyle: { width: 1.2, opacity: 0.8 }, data: pts.filter((p) => ok(p.n)).map((p) => [round(p.n, 1), hv(p.heightM)]) },
    ],
  };
}

function ductList(d, ctx) {
  const { ui, units } = ctx;
  const { h } = ui;
  const out = [];
  const ducts = d.profile.ducts || [];
  if (ducts.length) {
    out.push(ui.section('Ducts in the model profile', 'layers'), ui.table({
      columns: [
        { key: 'kind', label: 'Kind' },
        { key: 'baseM', label: 'Base', align: 'right', num: true, render: (r) => units.height(r.baseM, { digits: 0 }) },
        { key: 'topM', label: 'Top', align: 'right', num: true, render: (r) => units.height(r.topM, { digits: 0 }) },
        { key: 'thicknessM', label: 'Thickness', align: 'right', num: true, render: (r) => units.height(r.thicknessM, { digits: 0 }) },
        { key: 'strengthM', label: 'ΔM', align: 'right', num: true, render: (r) => units.num(r.strengthM, 1), tip: 'M deficit across the trapping layer: the larger, the stronger the duct' },
        { key: 'cutoffMHz', label: 'Cut-off', align: 'right', num: true, render: (r) => (ok(r.cutoffMHz) ? units.num(r.cutoffMHz, 0) + ' MHz' : units.DASH), tip: 'Lowest frequency the duct can trap; it must be below 1090 MHz to guide ADS-B' },
        { key: 'traps1090', label: '1090 MHz', render: (r) => ui.badge(r.traps1090 ? 'trapped' : 'leaks', r.traps1090 ? 'violet' : 'other') },
      ],
      rows: ducts,
    }));
  } else {
    out.push(h('p.wx-note', ui.icon('check'), ` No duct in the model profile for ${dayTime(d.profile.t)} (${d.profile.source || 'model'}).`));
  }
  const ac = d.duct.aircraft;
  if (ac) {
    if ((ac.layers || []).length) {
      out.push(ui.section('Inversions measured by aircraft right now', 'plane'), ui.table({
        columns: [
          { key: 'baseFt', label: 'Base', align: 'right', num: true, render: (r) => units.alt(r.baseFt) },
          { key: 'topFt', label: 'Top', align: 'right', num: true, render: (r) => units.alt(r.topFt) },
          { key: 'strengthC', label: 'Warming', align: 'right', num: true, render: (r) => fmtDelta('temp', r.strengthC) },
          { key: 'dndh', label: 'dN/dh', align: 'right', num: true, render: (r) => (ok(r.dndh) ? signed(r.dndh, 0) + ' N/km' : units.DASH) },
          { key: 'class', label: 'Class', render: (r) => (r.class ? ui.badge(classInfo(r.class).label, classInfo(r.class).badge) : units.DASH) },
          { key: 'humidity', label: 'Humidity', render: (r) => (r.humidity === 'model' ? 'model RH' : 'dry-air bound') },
        ],
        rows: ac.layers,
      }));
    }
    if (ac.note) out.push(h('p.wx-note', ui.icon('plane'), ' ' + ac.note));
  }
  return out;
}

function timelineOption(duct, units) {
  const hrs = (duct.hours || []).filter((x) => ok(x.index));
  const now = duct.now;
  const past = hrs.filter((x) => x.t <= now);
  const fut = hrs.filter((x) => x.t >= now - HOUR);
  const zones = [];
  let from = 0;
  for (const z of INDEX_ZONES) {
    zones.push([{ yAxis: from, itemStyle: { color: z.color, opacity: 0.07 }, name: classInfo(z.name).label, label: { show: true, position: 'insideTopLeft', color: z.color, fontSize: 9.5, opacity: 0.85 } }, { yAxis: z.to }]);
    from = z.to;
  }
  const fmt = { 'dN/dh': (v) => signed(v, 0) + ' N/km', 'Index (observed)': (v) => String(v), 'Index (forecast)': (v) => String(v), Duct: (v, p) => 'in profile' };
  return {
    animationDuration: 250,
    grid: { left: 10, right: 12, top: 34, bottom: 8, containLabel: true },
    legend: { show: true, top: 0, left: 0, type: 'scroll' },
    tooltip: { trigger: 'axis', confine: true, formatter: timeTooltip(fmt) },
    xAxis: { type: 'time', axisPointer: { label: { formatter: (p) => dayTime(p.value) } } },
    yAxis: [
      { type: 'value', min: 0, max: 100, name: 'index', interval: 25 },
      { type: 'value', name: 'dN/dh, N/km', scale: true, splitLine: { show: false } },
    ],
    series: [
      { name: 'Index (observed)', type: 'line', color: colors.cyan, showSymbol: false, lineStyle: { width: 2.2 }, data: past.map((x) => [x.t, x.index]), markArea: { silent: true, data: zones }, markLine: nowLine(now) },
      { name: 'Index (forecast)', type: 'line', color: colors.cyan, showSymbol: false, lineStyle: { width: 2, type: 'dashed' }, data: fut.map((x) => [x.t, x.index]) },
      { name: 'dN/dh', type: 'line', yAxisIndex: 1, color: colors.grey, showSymbol: false, lineStyle: { width: 1.1, type: [3, 3] }, data: hrs.map((x) => [x.t, round(x.dNdh, 1)]) },
      { name: 'Duct', type: 'scatter', color: colors.violet, symbol: 'diamond', symbolSize: 9, data: hrs.filter((x) => x.ducts > 0).map((x) => [x.t, x.index]) },
    ],
  };
}
