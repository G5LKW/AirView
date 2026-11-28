// reports/briefing.js - "Daily briefing" card: a short plain-language account of one station-local day,
// written on the station from its own statistics (GET /api/briefing/daily). The server's text carries unit
// tokens ({int:N} {dist:KM} {pct:P} {hour:H} {alt:FT} {speed:KT}) that are formatted here in the viewer's units.

const DAYS = [{ value: 'yesterday', label: 'Yesterday' }, { value: 'today', label: 'Today so far' }];
const MOOD = { busy: 'amber', quiet: 'blue', outage: 'red', normal: 'green', partial: 'cyan', nodata: 'other' };
const MOOD_LABEL = { busy: 'Busy', quiet: 'Quiet', outage: 'Partial data', normal: 'Normal', partial: 'Running', nodata: 'No data' };
const LEVEL = { high: 'amber', veryHigh: 'amber', low: 'blue', veryLow: 'blue', normal: 'green' };
const KIND = { emergency: ['Emergency', 'red'], military: ['Military', 'violet'], interesting: ['Watch list', 'teal'], record: ['Record', 'amber'], feed_down: ['Feed', 'other'] };

/** Replace the briefing's unit tokens with text in the viewer's units. Exported for tests. */
export function formatTokens(s, units) {
  return String(s ?? '').replace(/\{(int|dist|pct|hour|alt|speed):(-?[0-9.]+)\}/g, (_, k, v) => {
    const x = Number(v);
    if (!Number.isFinite(x)) return v;
    switch (k) {
      case 'int': return units.int(x);
      case 'dist': return units.dist(x, { digits: 0 });
      case 'pct': return units.pct(x);
      case 'hour': return String(Math.trunc(x)).padStart(2, '0') + ':00';
      case 'alt': return units.alt(x);
      case 'speed': return units.speed(x);
    }
    return v;
  });
}

export function mountBriefing(ctx, scope) {
  const { ui, units, api } = ctx;
  const { h } = ui;
  const prefs = ui.prefs('airview.reports.briefing', { day: 'yesterday' });
  let day = DAYS.some((d) => d.value === prefs.get().day) ? prefs.get().day : 'yesterday';
  let model = null;
  let error = null;
  let seq = 0;

  const body = h('div.rpbr');
  const seg = ui.segmented({ options: DAYS, value: day, onChange: (v) => { day = v; prefs.set({ day: v }); load(); } });
  const card = ui.card({ title: 'Daily briefing', icon: 'report', sub: 'Written on this station from its own statistics', actions: seg, body });

  async function load() {
    const my = ++seq;
    model = null; error = null;
    render();
    try {
      const res = await api.get('/api/briefing/daily', { params: { date: day }, signal: scope.signal, timeout: 30000 });
      if (my !== seq) return;
      model = res;
    } catch (e) {
      if (e.aborted || my !== seq) return;
      error = e;
    }
    render();
  }

  function factChip(f) {
    if (!f || f.value === null || f.value === undefined) return null;
    const val = f.unit === 'km' ? units.dist(f.value, { digits: 0 }) : f.unit === 'min' ? units.pct(Math.min(100, (f.value / 1440) * 100)) : units.int(f.value);
    let tip = null;
    if (f.typical !== null && f.typical !== undefined) {
      const typ = f.unit === 'km' ? units.dist(f.typical, { digits: 0 }) : units.int(f.typical);
      tip = `Typical ${typ}` + (f.diffPct !== null && f.diffPct !== undefined ? ` · ${f.diffPct >= 0 ? '+' : '−'}${units.pct(Math.abs(f.diffPct))}` : '') + (f.explained ? ' · explained by missing data' : '');
    } else if (f.key === 'uptime') tip = 'Share of the day with data';
    const kind = f.explained ? 'other' : (LEVEL[f.level] || 'other');
    const arrow = f.level === 'high' || f.level === 'veryHigh' ? ' ▲' : f.level === 'low' || f.level === 'veryLow' ? ' ▼' : '';
    return h('span.rpbr-fact', { tip }, h('span.muted', f.label), ' ', ui.badge(val + arrow, kind));
  }

  function notableRow(n) {
    const [label, kind] = KIND[n.kind] || [n.kind, 'other'];
    const time = units.clock(n.t + (model.utcOffsetSec || 0) * 1000, { seconds: false, z: false });
    const href = n.flightId ? '#/reports/flight/' + n.flightId : n.icao ? `#/reports?icao=${n.icao}&from=${model.from}&to=${model.to}` : null;
    return h('li.rpbr-note', ui.badge(label, kind), h('span.muted.num', time), href ? h('a', { href }, formatTokens(n.text, units)) : h('span', formatTokens(n.text, units)));
  }

  function render() {
    if (error) { ui.mount(body, ui.error(error, { title: 'The briefing could not be loaded', retry: load })); return; }
    if (!model) { ui.mount(body, ui.skeleton({ lines: 4 })); return; }
    card.setSub(`${model.date}${model.partial ? ' · so far' : ''} · written on this station`);
    const text = (model.paragraphs || []).map((p) => h('p.rpbr-p', formatTokens(p, units)));
    const side = [];
    if (model.mood !== 'nodata') {
      const facts = (model.facts || []).filter((f) => !(model.partial && f.key === 'uptime')).map(factChip).filter(Boolean);
      if (facts.length) side.push(h('div.rpbr-facts', facts));
    }
    if (model.notable?.length) {
      const shown = model.notable.slice(0, 12);
      side.push(h('ul.rpbr-notes', shown.map(notableRow)));
      if (model.notable.length > shown.length) side.push(h('p.muted.rpbr-small', `and ${units.int(model.notable.length - shown.length)} more in the event log`));
    }
    ui.mount(body, [
      h('div.rpbr-head', ui.badge(MOOD_LABEL[model.mood] || model.mood, MOOD[model.mood] || 'other'), h('p.rpbr-headline', formatTokens(model.headline, units))),
      h('div.rpbr-cols' + (side.length && text.length ? '' : '.is-single'), text.length ? h('div.rpbr-text', text) : null, side.length ? h('div.rpbr-side', side) : null),
      h('p.muted.rpbr-small', model.baseline?.days ? model.baseline.text + ' ' : '', model.method || ''),
    ]);
  }

  scope.onCleanup(units.onChange(() => { if (model) render(); }));
  load();
  return card;
}
