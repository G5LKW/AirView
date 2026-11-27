// rf/interrogation.js - RF Lab "Interrogators": radar census from DF11 interrogator codes (II / SI), reply rates
// over time, surveillance reply rates, Comm-B register mix and ACAS activity.

import { ok } from './common.js';

const BDS = {
  '1,0': 'Data-link capability', '1,7': 'GICB capability', '1,8': 'Mode S services', '2,0': 'Aircraft identification',
  '2,1': 'Registration', '3,0': 'ACAS resolution advisory', '4,0': 'Selected vertical intention', '4,4': 'Meteorological routine',
  '4,5': 'Meteorological hazard', '5,0': 'Track and turn', '5,3': 'Air-referenced state', '6,0': 'Heading and speed',
};
/**
 * /api/rf/interrogators answered the bare census (rfstat.InterrogationStats) on older servers; newer ones wrap
 * it with a timestamp: { t, interrogators: [...codes] } (the other census fields beside it) or
 * { t, interrogators: { codes, ... } }. Returns the census with `t` (ms, or null).
 */
export function normInterrogators(d) {
  if (!d || typeof d !== 'object') return d;
  const w = d.interrogators;
  if (Array.isArray(w)) return { ...d, codes: w, t: typeof d.t === 'number' ? d.t : null };
  if (w && typeof w === 'object') return { ...d, ...w, t: typeof d.t === 'number' ? d.t : typeof w.t === 'number' ? w.t : null };
  return { ...d, t: typeof d.t === 'number' ? d.t : null };
}

const CODE_COLORS = ['#3b8bff', '#22d3ee', '#fbbf24', '#a78bfa', '#34d399', '#f472b6', '#fb923c', '#2dd4bf', '#94a3b8'];

export default {
  id: 'interrogators',
  label: 'Interrogators',
  icon: 'antenna',
  endpoints: {
    interrogators: { path: '/api/rf/interrogators', every: 10000 },
    history: { path: '/api/rf/history', every: 60000, params: { days: 7 } },
  },

  build(env) {
    const { h, ui, units, charts, C, f, panel, show, notEnough } = env;
    const prefs = ui.prefs('airview.rf.interrogators', { span: 'hour' });
    let span = prefs.get().span === 'week' ? 'week' : 'hour';
    let last = null;
    let codesT = null;

    const P = {};
    const codesHost = h('div.rf-tablewrap');
    P.codes = panel({
      title: 'Interrogator codes', icon: 'antenna', class: 'span-2', content: codesHost,
      caption: 'Every Mode S radar uses an interrogator code (II 1–15, or SI 1–63) that aircraft echo in their all-call replies (DF11), so you can count the radars that reach the aircraft you hear. II 0 also contains the aircraft\'s spontaneous acquisition squitters. "Lock-out likely": aircraft stop answering this radar\'s all-calls because it has already acquired them - normal for a working radar. The sparkline is replies per minute over the last hour.',
    });
    const spanSeg = ui.segmented({
      options: [{ value: 'hour', label: '60 min', tip: 'Replies per minute, last hour' }, { value: 'week', label: '7 days', tip: 'Replies per hour from the hourly history' }],
      value: span, onChange: (v) => { span = v; prefs.set({ span: v }); paintTimeline(); },
    });
    P.timeline = panel({
      title: 'All-call replies over time', icon: 'history', class: 'span-2', actions: spanSeg, height: 260,
      caption: 'Stacked replies per interrogator code. Rotating radars appear as a steady band; a code that comes and goes is a radar at the edge of reach or one that only sweeps part of the sky.',
    });
    const survKv = h('div');
    const topHost = h('div');
    P.surv = panel({
      title: 'Surveillance replies', icon: 'radar', content: h('div', survKv, topHost),
      caption: 'Replies to targeted radar interrogations (DF4/5/20/21) per aircraft and second. One radar typically asks 1–5 times per sweep; high rates mean several radars or a busy terminal area. The table lists the most interrogated aircraft.',
    });
    P.commB = panel({
      title: 'Comm-B register mix', icon: 'database', height: 240,
      caption: 'What the radars ask aircraft for (Enhanced Surveillance, DF20/21). BDS 4,0 / 5,0 / 6,0 are the selected altitude, track and speed registers used by air traffic control; 2,0 is the callsign.',
    });
    const acasKv = h('div');
    P.acas = panel({
      title: 'ACAS / TCAS activity', icon: 'shield', content: acasKv,
      caption: 'Collision-avoidance systems interrogate nearby aircraft on their own (short DF0 replies, long DF16 coordination). A resolution advisory (RA) means two aircraft were told to climb or descend - rare and worth a look.',
    });
    const mixHost = h('div.chart', { style: { '--h': '200px' } });
    P.mix = panel({
      title: 'Who is under radar coverage', icon: 'layers', content: mixHost,
      caption: 'Active aircraft (heard within 2 minutes) by what you receive from them: ADS-B only, Mode S only (no ADS-B), or both. "Radar share" = ADS-B aircraft that are also being interrogated by a Mode S radar.',
    });
    let mixChart = null;

    // ------------------------------------------------------------------------------------------
    function codeRows(d) {
      return (d.codes || []).map((c, i) => ({ ...c, color: CODE_COLORS[i % CODE_COLORS.length] }));
    }

    function paintCodes(d) {
      const rows = codeRows(d);
      if (!rows.length) return { empty: ['No interrogator codes yet', 'Codes are read from DF11 all-call replies. Without any Mode S radar in reach you will only ever see II 0.', 'antenna'] };
      P.codes.card.setSub(`${units.int(d.activeRadars)} active radar${d.activeRadars === 1 ? '' : 's'} (≥ 10 replies in 10 min, II 0 excluded)${ok(d.t) && d.t > 0 ? ' · as of ' + units.clock(d.t) : ''}`);
      if (!codesT) {
        codesT = ui.table({
          columns: [
            { key: 'code', label: 'Code', sort: (r) => r.iid + (r.code.startsWith('SI') ? 100 : 0), render: (r) => h('span.rf-code', h('i', { style: { background: r.color } }), h('span.num', r.code), r.acqSquitter ? ui.badge('+ squitters', 'other', { tip: 'II 0 also counts spontaneous DF11 acquisition squitters' }) : null) },
            { key: 'replies', label: 'Replies', align: 'right', num: true, sort: true, render: (r) => units.int(r.replies) },
            { key: 'rate', label: 'Rate', align: 'right', num: true, sort: true, sortDir: 'desc', render: (r) => f.rate(r.rate, 2) },
            { key: 'last10m', label: '10 min', align: 'right', num: true, sort: true, render: (r) => units.int(r.last10m) },
            { key: 'aircraft', label: 'Aircraft', align: 'right', num: true, sort: true, render: (r) => units.int(r.aircraft) },
            { key: 'replyingFrac', label: 'Replying', align: 'right', num: true, sort: true, tip: 'Aircraft answering this code ÷ aircraft under Mode S surveillance', render: (r) => f.frac(r.replyingFrac, 0) },
            { key: 'lockoutLikely', label: 'Lock-out', align: 'center', render: (r) => (r.lockoutLikely ? ui.badge('likely', 'green') : h('span.muted', units.DASH)) },
            { key: 'timeline', label: 'Last 60 min', render: (r) => env.spark(r.timeline, { width: 120, height: 20 }) },
          ],
          rows, sort: { key: 'rate', dir: 'desc' }, empty: 'No codes',
        });
        ui.mount(codesHost, codesT);
      } else {
        codesT.setRows(rows);
      }
      return null;
    }

    function paintTimeline() {
      if (!last) return;
      if (span === 'week') {
        show(P.timeline, last.history, (d) => {
          const hours = (d && d.hours) || [];
          const codes = new Set();
          for (const r of hours) for (const k of Object.keys(r.df11ByIid || {})) codes.add(k);
          if (!hours.length || !codes.size) return { empty: ['Not enough data yet', 'Hourly reply counts appear after the first full clock hour.', 'history'] };
          const list = [...codes].sort((a, b) => a.localeCompare(b, undefined, { numeric: true }));
          const opt = charts.area({
            series: list.map((k, i) => ({ name: k, data: hours.map((r) => [r.t, (r.df11ByIid || {})[k] || 0]), color: CODE_COLORS[i % CODE_COLORS.length], stack: 'all', area: 0.5 })),
            yName: 'replies/h', yFormat: (v) => units.si(v), valueFormat: (v) => units.int(v), zoom: true,
          });
          P.timeline.chart.set(opt, { replace: true });
          return null;
        });
        return;
      }
      show(P.timeline, last.interrogators, (d) => {
        const rows = codeRows(d).filter((c) => (c.timeline || []).some((v) => v > 0));
        if (!rows.length) return { empty: notEnough('Replies per minute are collected over the last hour.') };
        // the timeline ends at the census time when the server says when that was
        const now = Math.floor((ok(d.t) && d.t > 0 ? d.t : env.ctx.live.serverNow()) / 60000) * 60000;
        const n = rows[0].timeline.length;
        const t = (i) => now - (n - 1 - i) * 60000;
        const opt = charts.area({
          series: rows.map((c) => ({ name: c.code, data: c.timeline.map((v, i) => [t(i), v]), color: c.color, stack: 'all', area: 0.5, step: 'end' })),
          yName: 'replies/min', yFormat: (v) => units.si(v), valueFormat: (v) => units.int(v),
        });
        opt.animation = false;
        P.timeline.chart.set(opt, { replace: true });
        return null;
      });
    }

    function paintSurv(d) {
      ui.mount(survKv, ui.kv([
        ['Median per aircraft', ok(d.survRateP50) ? f.rate(d.survRateP50, 2) : units.DASH],
        ['90th percentile', ok(d.survRateP90) ? f.rate(d.survRateP90, 2) : units.DASH],
        ['Highest', ok(d.survRateMax) ? f.rate(d.survRateMax, 2) : units.DASH],
        ['Mode S radar share', f.frac(d.radarShare, 0), { tip: 'ADS-B aircraft that are also under Mode S surveillance' }],
      ], { columns: 2 }));
      const top = d.topSurv || [];
      ui.mount(topHost, top.length ? [ui.section('Most interrogated', 'plane'), ui.table({
        columns: [
          { key: 'icao', label: 'ICAO', render: (r) => h('span.num', r.icao) },
          { key: 'rate', label: 'Rate', align: 'right', num: true, render: (r) => f.rate(r.rate, 2) },
          { key: 'replies', label: 'Replies', align: 'right', num: true, render: (r) => units.int(r.replies) },
        ],
        rows: top.slice(0, 8), onRowClick: (r) => env.ctx.navigate('map/' + r.icao),
      })] : env.finding('info', 'No targeted surveillance replies seen: no Mode S radar seems to be interrogating the aircraft you hear (common in areas covered by ADS-B / WAM only, and in the demo).'));
      return null;
    }

    function paintCommB(d) {
      const list = (d.commB || []).filter((b) => b.count > 0);
      if (!list.length) return { empty: ['No Comm-B replies yet', 'Comm-B replies (DF20/21) only appear where an Enhanced Surveillance radar interrogates the aircraft.', 'database'] };
      const total = list.reduce((s, b) => s + b.count, 0);
      const opt = charts.donut({
        data: list.map((b) => ({ name: `BDS ${b.bds}`, value: b.count })),
        valueFormat: (v) => units.int(v), centerText: units.si(total),
      });
      opt.tooltip.formatter = (p) => { const b = list[p.dataIndex]; return `BDS ${b.bds} · ${BDS[b.bds] || 'register'}<br><b>${units.int(b.count)}</b> · ${f.frac(b.frac, 1)}`; };
      opt.legend.formatter = (name) => { const b = list.find((x) => `BDS ${x.bds}` === name); return `${name}  ${BDS[b?.bds] ? BDS[b.bds] + ' ' : ''}${f.frac(b?.frac, 0)}`; };
      opt.legend.textStyle = { fontSize: 11 };
      P.commB.chart.set(opt, { replace: true });
      return null;
    }

    function paintAcas(d) {
      ui.mount(acasKv, h('div',
        ui.kv([
          ['DF0 short replies', f.rate(d.df0Rate, 1)],
          ['DF16 coordination', f.rate(d.df16Rate, 2)],
          ['Aircraft with ACAS', `${units.int(d.acasAircraft)} of ${units.int(d.activeAircraft)}`],
          ['Resolution advisories', units.int(d.raReports), { class: d.raReports > 0 ? 'c-red' : '' }],
        ], { columns: 2 }),
        d.raReports > 0 ? env.finding('warn', `${units.int(d.raReports)} resolution advisory report(s) were decoded in the window: two aircraft were instructed to separate vertically.`) : null));
      return null;
    }

    function paintMix(d) {
      const total = (d.adsbAndModeS || 0) + (d.adsbOnly || 0) + (d.modeSOnly || 0);
      if (!total) return { empty: notEnough('No active aircraft right now.') };
      if (!mixChart) mixChart = charts.create(mixHost);
      const opt = charts.donut({
        data: [
          { name: 'ADS-B and Mode S', value: d.adsbAndModeS || 0, color: C.cyan },
          { name: 'ADS-B only', value: d.adsbOnly || 0, color: C.teal },
          { name: 'Mode S only', value: d.modeSOnly || 0, color: C.violet },
        ],
        valueFormat: (v) => units.int(v), centerText: units.int(total),
      });
      mixChart.set(opt, { replace: true });
      return null;
    }

    return {
      el: h('div.grid.cols-2.rf-grid', P.codes.card, P.timeline.card, P.surv.card, P.commB.card, P.acas.card, P.mix.card),
      render(state) {
        const raw = state.interrogators;
        const s = raw && raw.data ? { ...raw, data: normInterrogators(raw.data) } : raw;
        last = { ...state, interrogators: s };
        show(P.codes, s, paintCodes);
        paintTimeline();
        show(P.surv, s, paintSurv);
        show(P.commB, s, paintCommB);
        show(P.acas, s, paintAcas);
        show(P.mix, s, paintMix);
      },
    };
  },
};
