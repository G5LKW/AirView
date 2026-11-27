// rf/signal.js - RF Lab "Signal": RSSI distributions, noise / decode floor, overload and gain advice, dynamic
// range, RSSI per distance ring / elevation band, RSSI versus aspect angle, fading and multipath.

import { ok, histPoints, histTotal, anyNum, statusOf } from './common.js';

const STRONG_WARN = 0.01; // share of frames within 3 dB of full scale (server: rfstat strongWarn / strongBad)
const STRONG_BAD = 0.05;
const MARGIN_LOW_DB = 6;

export default {
  id: 'signal',
  label: 'Signal',
  icon: 'signal',
  endpoints: {
    signal: { path: '/api/rf/signal', every: 10000 },
    health: { path: '/api/rf/health', every: 10000 },
  },

  build(env) {
    const { h, ui, units, charts, C, f, panel, show, notEnough } = env;
    const prefs = ui.prefs('airview.rf.signal', { bands: 'ring', fade: 'k' });
    let bandsMode = prefs.get().bands === 'elev' ? 'elev' : 'ring';
    let fadeMode = prefs.get().fade === 'std' ? 'std' : 'k';
    let last = null;

    const P = {};
    P.hist = panel({
      title: 'Signal level distribution', icon: 'signal', class: 'span-2', height: 300,
      caption: 'How strong the received frames are (share of frames per 0.5 dB). The left edge of the hump is the weakest signal the receiver still decodes (decode floor); the noise floor is an estimate 10 dB below it. Frames piling up near 0 dBFS (right) are clipping the receiver. A healthy station shows a broad hump that ends well before the overload zone.',
    });

    const ladder = h('div.rf-ladder');
    const levelsKv = h('div');
    const gainEl = h('div.rf-findings');
    P.levels = panel({
      title: 'Levels, headroom & gain', icon: 'ruler', content: h('div.rf-levels', ladder, levelsKv, gainEl),
      caption: 'The ladder shows where your signals sit between the (estimated) noise floor and full scale. Dynamic range = spread between the weakest and strongest 0.1 % of frames; SNR margin = median minus decode floor. Little margin means the gain is too low, many frames within 3 dB of full scale means it is too high.',
    });

    const bandsSeg = ui.segmented({
      options: [{ value: 'ring', label: 'By distance', tip: 'Signal level per 25 km distance ring' }, { value: 'elev', label: 'By elevation', tip: 'Signal level per elevation-angle band' }],
      value: bandsMode, onChange: (v) => { bandsMode = v; prefs.set({ bands: v }); paintBands(); },
    });
    P.bands = panel({
      title: 'Signal level by distance', icon: 'radar', actions: bandsSeg, height: 280,
      caption: 'Box = the middle 90 % of frames (5th to 95th percentile), dot = median. With distance the level should fall smoothly (about 6 dB per doubling); a sudden drop points at the radio horizon or an obstruction. By elevation: aircraft just above the horizon are the hardest to hear.',
    });

    const aspectKv = h('div.rf-kvrow');
    P.aspect = panel({
      title: 'Signal versus aspect angle', icon: 'plane', height: 300, top: null,
      content: h('div.chart', { style: { '--h': '300px' } }),
      caption: 'How the aircraft\'s own body shadows its transponder antenna: signal level relative to each aircraft\'s average, by the angle between its heading and the line to your station (top = flying towards you, bottom = flying away, drawn mirrored). A round shape means no airframe effect; a dent at the tail is typical for bottom-mounted antennas.',
    });
    P.aspect.extra.append(aspectKv);

    const fadeSeg = ui.segmented({
      options: [{ value: 'k', label: 'K factor', tip: 'Rician K: steady (line of sight) versus scattered power' }, { value: 'std', label: 'Spread', tip: 'Standard deviation of the level within short windows' }],
      value: fadeMode, onChange: (v) => { fadeMode = v; prefs.set({ fade: v }); paintFading(); },
    });
    const fadeKv = h('div.rf-kvrow');
    const worstHost = h('div');
    P.fading = panel({
      title: 'Fading', icon: 'wave', actions: fadeSeg, height: 220,
      caption: 'Fading = how much one aircraft\'s signal flickers over a few seconds. A high K factor (above ~10 dB) means a clean direct path; values near 0 dB mean the signal is a mix of reflections (multipath) and will drop out now and then. Low aircraft and those behind obstacles fade most.',
    });
    P.fading.extra.append(fadeKv, worstHost);

    const mpKv = h('div.rf-kvrow');
    P.multipath = panel({
      title: 'Multipath (ground reflection)', icon: 'mountain', height: 220, class: 'span-2',
      caption: 'Signal deviation by elevation angle after removing the distance trend. A regular ripple is the two-ray effect: the direct wave and a wave reflected off the ground (or a roof) add and cancel alternately. The ripple spacing tells the antenna\'s height above that reflecting surface.',
    });
    P.multipath.extra.append(mpKv);

    // ------------------------------------------------------------------------------------------
    function paintHist(sg) {
      const all = sg.all || {};
      const tot = histTotal(all.hist);
      if (!tot) return { empty: notEnough('The distribution appears after the first frames with a signal level have arrived.') };
      const series = [{ type: 'bar', name: 'All frames', data: histPoints(all.hist, 100 / tot), color: C.blue, barWidth: '90%', barCategoryGap: '8%', itemStyle: { borderRadius: [2, 2, 0, 0] }, z: 1 }];
      const ants = (sg.perAntenna || []).filter((a) => histTotal(a.hist) > 0);
      const antColors = [C.cyan, C.amber, C.violet, C.green];
      if (ants.length > 1) {
        ants.forEach((a, i) => {
          const t = histTotal(a.hist);
          series.push({ type: 'line', name: a.label, data: histPoints(a.hist, 100 / t), color: antColors[i % antColors.length], showSymbol: false, smooth: 0.2, lineStyle: { width: 1.8 }, z: 3 });
        });
      }
      const marks = [];
      if (ok(sg.decodeFloorDbfs)) marks.push({ xAxis: sg.decodeFloorDbfs, name: 'decode floor', lineStyle: { color: C.cyan } });
      if (ok(sg.noiseFloorEstDbfs)) marks.push({ xAxis: sg.noiseFloorEstDbfs, name: 'noise floor (est.)', lineStyle: { color: C.text3 } });
      if (ok(sg.p50Dbfs)) marks.push({ xAxis: sg.p50Dbfs, name: 'median', lineStyle: { color: C.blueHi, type: 'solid' } });
      series[0].markLine = { symbol: 'none', silent: true, label: { formatter: '{b}', color: C.text2, fontSize: 10 }, data: marks };
      series[0].markArea = { silent: true, itemStyle: { color: 'rgba(255,77,94,0.10)' }, label: { color: C.red, fontSize: 10, position: 'insideTop' }, data: [[{ xAxis: -3, name: 'overload' }, { xAxis: 0 }]] };
      const lo = Math.floor(Math.min(all.hist.lo, ok(sg.noiseFloorEstDbfs) ? sg.noiseFloorEstDbfs - 2 : all.hist.lo));
      P.hist.chart.set({
        animation: false,
        grid: { left: 8, right: 16, top: ants.length > 1 ? 34 : 18, bottom: 8, containLabel: true },
        legend: ants.length > 1 ? { show: true, top: 0, left: 0 } : { show: false },
        tooltip: { trigger: 'axis', confine: true, axisPointer: { type: 'line' }, formatter: (ps) => {
          const x = ps[0] && ps[0].value[0];
          return `${f.dbfs(x - all.hist.step / 2)} … ${f.dbfs(x + all.hist.step / 2)}<br>` + ps.map((p) => `${p.marker}${p.seriesName}: <b>${f.pct(p.value[1], 1)}</b>`).join('<br>');
        } },
        xAxis: { type: 'value', min: lo, max: 0, name: 'dBFS', nameLocation: 'middle', nameGap: 24, axisLabel: { formatter: (v) => units.num(v, 0) } },
        yAxis: { type: 'value', name: '% of frames', axisLabel: { formatter: (v) => units.num(v, 1) } },
        series,
      }, { replace: true });
      P.hist.card.setSub(`${units.int(sg.frames)} frames · ${ants.map((a) => `${a.label} ${units.int(a.n)}`).join(' · ')}`);
      return null;
    }

    function paintLevels(sg, hl) {
      if (!sg.frames) return { empty: notEnough('Levels are measured from the first frames with a signal level.') };
      // ladder: noise floor .. 0 dBFS
      const lo = Math.min(ok(sg.noiseFloorEstDbfs) ? sg.noiseFloorEstDbfs : -50, -40) - 2;
      const pos = (v) => `${(((v - lo) / (0 - lo)) * 100).toFixed(2)}%`;
      const mark = (v, label, cls) => (ok(v) ? h('div.rf-ladder-mark.' + cls, { style: { left: pos(v) }, tip: `${label}: ${f.dbfs(v)}` }, h('span', label)) : null);
      const rng = ok(sg.all?.p05) && ok(sg.all?.p95) ? h('div.rf-ladder-range', { style: { left: pos(sg.all.p05), width: `calc(${pos(sg.all.p95)} - ${pos(sg.all.p05)})` }, tip: `Middle 90 % of frames: ${f.dbfs(sg.all.p05)} … ${f.dbfs(sg.all.p95)}` }) : null;
      ui.mount(ladder,
        h('div.rf-ladder-track', h('div.rf-ladder-noise', { style: { width: pos(ok(sg.decodeFloorDbfs) ? sg.decodeFloorDbfs : lo) } }), h('div.rf-ladder-over', { style: { left: pos(-3) } }), rng,
          mark(sg.noiseFloorEstDbfs, 'noise (est.)', 'noise'), mark(sg.decodeFloorDbfs, 'decode floor', 'floor'), mark(sg.p50Dbfs, 'median', 'median'), mark(sg.p999Dbfs, 'p99.9', 'peak')),
        h('div.rf-ladder-scale', h('span', f.dbfs(lo, 0)), h('span', 'full scale ' + f.dbfs(0, 0))));
      ui.mount(levelsKv, ui.kv([
        ['Noise floor (estimate)', f.dbfs(sg.noiseFloorEstDbfs), { tip: `Decode floor minus an assumed decoder SNR of ${f.db(sg.assumedDecodeSnrDb, 0)} - not a measurement` }],
        ['Decode floor', f.dbfs(sg.decodeFloorDbfs), { tip: '1st percentile of the frame levels: the weakest signal that is still reliably decoded' }],
        ['Median level', f.dbfs(sg.p50Dbfs)],
        ['Strongest (p99.9)', f.dbfs(sg.p999Dbfs)],
        ['Dynamic range', f.db(sg.dynamicRangeDb), { tip: 'Spread between the 0.1 % weakest and the 0.1 % strongest frames' }],
        ['SNR margin', f.db(sg.snrMarginDb), { tip: 'Median minus decode floor' }],
        ['SNR (estimate)', f.db(sg.snrEstDb), { tip: 'Median minus the estimated noise floor' }],
        ['Within 3 dB of full scale', f.frac(sg.strongFrac, 2), { class: (sg.strongFrac ?? 0) > STRONG_BAD ? 'c-red' : '' }],
        ['Within 1 dB (clipping)', f.frac(sg.nearClipFrac, 2)],
      ], { columns: 2 }));
      // overload / gain verdict: server text when available, else the same thresholds locally
      const gain = (hl?.items || []).find((i) => i.key === 'gain');
      let level = 'ok';
      let text = 'Gain looks right: signals use the range well without clipping.';
      const st = sg.strongFrac ?? 0;
      if (st > STRONG_BAD) { level = 'bad'; text = `Overload: ${f.frac(st, 1)} of frames are within 3 dB of full scale. Reduce the gain (or add attenuation) by 3–6 dB.`; } else if (st > STRONG_WARN) { level = 'warn'; text = `Gain on the high side: ${f.frac(st, 1)} of frames are near full scale. Consider 2–3 dB less.`; } else if (ok(sg.snrMarginDb) && sg.snrMarginDb < MARGIN_LOW_DB && st < 0.001) { level = 'warn'; text = `Gain may be too low: the median is only ${f.db(sg.snrMarginDb)} above the decode floor and nothing approaches full scale. Try 2–3 dB more, or check the LNA and cable losses.`; }
      if (gain && gain.score >= 0) level = gain.status === 'ok' ? 'ok' : gain.status === 'bad' ? 'bad' : 'warn';
      ui.mount(gainEl, env.finding(level, h('span', h('b', level === 'ok' ? 'Gain OK. ' : level === 'bad' ? 'Overload. ' : 'Check gain. '), text)));
      P.levels.card.setSub(statusOf(level).label);
      return null;
    }

    function paintBands() {
      if (!last) return;
      P.bands.card.setTitle(bandsMode === 'elev' ? 'Signal level by elevation' : 'Signal level by distance');
      show(P.bands, last.signal, (sg) => {
        const list = (bandsMode === 'elev' ? sg.perElev : sg.perRing) || [];
        const rows = list.filter((b) => b.n > 0 && ok(b.p50));
        if (!rows.length) return { empty: notEnough(bandsMode === 'elev' ? 'Needs positions to work out each aircraft\'s elevation angle.' : 'Needs aircraft positions and a receiver location.') };
        const lab = (b) => {
          if (bandsMode === 'ring') return f.band(b.lo, b.hi);
          if (!ok(b.lo)) return `< ${units.deg(b.hi, 1)}`;
          if (!ok(b.hi)) return `> ${units.deg(b.lo, 1)}`;
          return `${units.num(b.lo, 1)}–${units.deg(b.hi, 1)}`;
        };
        const x = rows.map(lab);
        P.bands.chart.set({
          animation: false,
          grid: { left: 8, right: 14, top: 30, bottom: 8, containLabel: true },
          legend: { show: true, top: 0, left: 0 },
          tooltip: { trigger: 'axis', confine: true, axisPointer: { type: 'shadow' }, formatter: (ps) => {
            const b = rows[ps[0].dataIndex];
            return `${x[ps[0].dataIndex]} · ${units.int(b.n)} frames<br>5–95 %: <b>${f.dbfs(b.p05)} … ${f.dbfs(b.p95)}</b><br>median: <b>${f.dbfs(b.p50)}</b> · mean ${f.dbfs(b.mean)}${b.strongFrac > 0 ? `<br>near full scale: ${f.frac(b.strongFrac, 2)}` : ''}`;
          } },
          xAxis: { type: 'category', data: x, axisLabel: { rotate: x.length > 7 ? 35 : 0 } },
          yAxis: { type: 'value', name: 'dBFS', scale: true, max: (v) => Math.min(0, Math.ceil((v.max + 3) / 5) * 5), axisLabel: { formatter: (v) => units.num(v, 0) } },
          series: [
            { type: 'candlestick', name: '5–95 %', data: rows.map((b) => [b.p05, b.p95, b.p05, b.p95]), barMaxWidth: 28, itemStyle: { color: 'rgba(47,125,255,0.35)', borderColor: C.blueHi, color0: 'rgba(47,125,255,0.35)', borderColor0: C.blueHi } },
            { type: 'line', name: 'median', data: rows.map((b) => b.p50), color: C.cyan, symbol: 'circle', symbolSize: 7, showSymbol: true, lineStyle: { width: 1.2, type: 'dashed' } },
          ],
        }, { replace: true });
        return null;
      });
    }

    function paintAspect(sg) {
      const a = sg.aspect || {};
      const from = a.fromDeg || [];
      const mean = a.meanDb || [];
      if (!anyNum(mean)) return { empty: notEnough('Needs aircraft seen from several angles (approaching, passing, departing).') };
      const step = from.length > 1 ? from[1] - from[0] : 10;
      const right = [];
      const left = [];
      from.forEach((d, i) => { if (ok(mean[i])) { const c = d + step / 2; right.push([mean[i], c]); left.push([mean[i], 360 - c]); } });
      const pts = [...right, ...left.reverse()];
      const maxAbs = Math.max(3, Math.ceil(Math.max(...pts.map((p) => Math.abs(p[0]))) + 1));
      const ref = [];
      for (let b = 0; b <= 360; b += 10) ref.push([0, b]);
      const opt = charts.polar({
        series: [
          { name: 'Average (0 dB)', data: ref, color: C.text4, closed: false, width: 1 },
          { name: 'Relative level', data: pts, color: C.cyan, area: false, width: 2 },
        ],
        minR: -maxAbs, maxR: maxAbs, rFormat: (v) => f.dbs(v, 0), legend: false,
        tooltip: (p) => (p.seriesIndex === 0 ? '' : `aspect ${units.deg(p.value[1] > 180 ? 360 - p.value[1] : p.value[1])}<br><b>${f.dbs(p.value[0])}</b>`),
      });
      opt.angleAxis.axisLabel = { formatter: (v) => (v === 0 ? 'nose' : v === 180 ? 'tail' : v === 90 || v === 270 ? 'beam' : '') };
      opt.series[1].showSymbol = true;
      opt.series[1].symbolSize = 5;
      P.aspect.chart.set(opt, { replace: true });
      const nRot = (a.n || []).reduce((s, v) => s + (ok(v) ? v : 0), 0);
      ui.mount(aspectKv, ui.kv([
        ['Nose', f.dbs(a.noseDb), { tip: 'Aspect 0–30°: aircraft flying towards the station' }],
        ['Beam', f.dbs(a.beamDb), { tip: 'Aspect 60–120°: passing sideways' }],
        ['Tail', f.dbs(a.tailDb), { tip: 'Aspect 150–180°: flying away' }],
        ['Nose − tail', f.db(a.noseTailDb)],
        ['Samples', units.int(nRot)],
      ], { columns: 2 }));
      return null;
    }

    function paintFading() {
      if (!last) return;
      show(P.fading, last.signal, (sg) => {
        const fd = sg.fading || {};
        const hist = fadeMode === 'std' ? fd.stdHist : fd.kHist;
        const tot = histTotal(hist);
        if (!fd.windows || !tot) return { empty: notEnough('Fading is measured over short windows of each aircraft; it appears after a few minutes of traffic.') };
        const med = fadeMode === 'std' ? fd.stdMedianDb : fd.kMedianDb;
        P.fading.chart.set({
          animation: false,
          grid: { left: 8, right: 14, top: 18, bottom: 8, containLabel: true },
          tooltip: { trigger: 'axis', confine: true, axisPointer: { type: 'shadow' }, formatter: (ps) => `${f.db(ps[0].value[0] - hist.step / 2)} … ${f.db(ps[0].value[0] + hist.step / 2)}<br><b>${units.int(ps[0].value[1])}</b> windows` },
          xAxis: { type: 'value', name: fadeMode === 'std' ? 'spread (dB)' : 'K (dB)', nameLocation: 'middle', nameGap: 24, scale: true, axisLabel: { formatter: (v) => units.num(v, 0) } },
          yAxis: { type: 'value', name: 'windows', minInterval: 1, axisLabel: { formatter: (v) => units.int(v) } },
          series: [{
            type: 'bar', data: histPoints(hist), color: fadeMode === 'std' ? C.violet : C.teal, barWidth: '90%',
            markLine: ok(med) ? { symbol: 'none', silent: true, label: { formatter: 'median', color: C.text2, fontSize: 10 }, data: [{ xAxis: med }] } : undefined,
          }],
        }, { replace: true });
        ui.mount(fadeKv, ui.kv([
          ['Median K', f.db(fd.kMedianDb), { tip: 'Rician K factor: ratio of the steady (direct) to the scattered power' }],
          ['Median spread', f.db(fd.stdMedianDb)],
          ['Windows', units.int(fd.windows)],
          ...(fd.kByElev || []).filter((e) => e.n > 0).map((e) => [`K at ${e.label}`, f.db(e.kDb), { tip: `${units.int(e.n)} windows` }]),
        ], { columns: 2 }));
        const worst = (fd.worst || []).filter((w) => ok(w.kDb));
        ui.mount(worstHost, worst.length ? [ui.section('Strongest fading now', 'plane'), ui.table({
          columns: [
            { key: 'icao', label: 'ICAO', render: (r) => h('span.num', r.icao) },
            { key: 'kDb', label: 'K', align: 'right', num: true, sort: true, render: (r) => f.db(r.kDb) },
            { key: 'stdDb', label: 'Spread', align: 'right', num: true, sort: true, render: (r) => f.db(r.stdDb) },
            { key: 'distKm', label: 'Distance', align: 'right', num: true, sort: true, render: (r) => units.dist(r.distKm) },
            { key: 'elevDeg', label: 'Elev.', align: 'right', num: true, sort: true, render: (r) => units.deg(r.elevDeg, 1) },
          ],
          rows: worst.slice(0, 8), sort: { key: 'kDb', dir: 'asc' }, onRowClick: (r) => env.ctx.navigate('map/' + r.icao),
        })] : null);
        return null;
      });
    }

    function paintMultipath(sg) {
      const m = sg.multipath || {};
      const el = m.elevDeg || [];
      const dev = m.devDb || [];
      if (!anyNum(el) || !anyNum(dev)) return { empty: notEnough('Needs many positions at low elevation angles (below about 3°) to see a reflection pattern.') };
      const data = el.map((e, i) => [e, dev[i]]).filter((p) => ok(p[0]) && ok(p[1]));
      P.multipath.chart.set(charts.line({
        xType: 'value', xName: 'elevation (°)', xFormat: (v) => units.num(v, 1),
        series: [{ name: 'deviation', data, color: m.detected ? C.amber : C.blueHi, smooth: true, area: 0.15, markLine: { symbol: 'none', silent: true, label: { show: false }, data: [{ yAxis: 0 }] } }],
        yName: 'dB', yFormat: (v) => units.num(v, 1), valueFormat: (v) => f.dbs(v), legend: false,
      }), { replace: true });
      ui.mount(mpKv, h('div.rf-mp', ui.badge(m.detected ? 'ripple detected' : 'no clear ripple', m.detected ? 'amber' : 'green'), ui.kv([
        ['Ripple (RMS)', f.db(m.rippleDb)],
        ['Lobe spacing', ok(m.periodDeg) ? units.deg(m.periodDeg, 2) : units.DASH],
        ['Reflector height', ok(m.reflectorHeightM) ? units.height(m.reflectorHeightM) : units.DASH, { tip: 'Height of the antenna above the dominant reflecting surface, from the lobe spacing' }],
        ['Pattern strength', f.frac(m.strength, 0), { tip: '100 % = a pure periodic ripple' }],
      ], { columns: 2 })));
      return null;
    }

    return {
      el: h('div.grid.cols-2.rf-grid', P.hist.card, P.levels.card, P.aspect.card, P.bands.card, P.fading.card, P.multipath.card),
      render(state) {
        last = state;
        show(P.hist, state.signal, paintHist);
        show(P.levels, state.signal, (sg) => paintLevels(sg, state.health && state.health.data));
        paintBands();
        show(P.aspect, state.signal, paintAspect);
        paintFading();
        show(P.multipath, state.signal, paintMultipath);
      },
    };
  },
};
