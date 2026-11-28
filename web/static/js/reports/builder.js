// reports/builder.js - the VirtualRadar-style report builder: a time range and criteria form, a server-sorted
// and paged results table (/api/reports), CSV export (/api/reports.csv) and row -> flight detail.
// Departure / arrival airports: filters dep= / arr= (exact ICAO or IATA code); the Dep / Arr columns are shown
// only when the server's rows carry `dep` / `arr` (they are omitted while unknown, and by older servers).
// The query, page and sort survive a detour to a flight (ctx.shared 'reports.builder').

import { toInputUTC, fromInputUTC, HOUR, DAY, fin, srcMaskLabels, urlWith } from './common.js';

const SHARED_KEY = 'reports.builder';
const PRESETS = [
  { value: '1h', label: '1 h', ms: HOUR }, { value: '6h', label: '6 h', ms: 6 * HOUR }, { value: '24h', label: '24 h', ms: DAY },
  { value: '7d', label: '7 d', ms: 7 * DAY }, { value: '30d', label: '30 d', ms: 30 * DAY }, { value: 'custom', label: 'Custom' },
];
const PAGE_SIZES = [25, 50, 100, 250];
const TEXT_SORTS = new Set(['callsign', 'icao', 'reg', 'type', 'operator', 'country', 'dep', 'arr']);

const blankQuery = () => ({
  preset: '24h', from: null, to: null, callsign: '', reg: '', icao: '', type: '', operator: '', country: '', squawk: '', dep: '', arr: '',
  military: '', emergency: false, minDistKm: null, maxDistKm: null, minAlt: null, maxAlt: null, minPositions: null,
});

/**
 * @param {HTMLElement} host
 * @param ctx page context
 * @param scope view scope (reports/common.js)
 */
export function mountBuilder(host, ctx, scope) {
  const { ui, units, api, live } = ctx;
  const { h } = ui;
  const saved = ctx.shared.get(SHARED_KEY);
  const S = saved ? JSON.parse(JSON.stringify(saved)) : { q: blankQuery(), at: 0, offset: 0, limit: 50, sort: 'start', desc: true, ran: false };
  S.q = { ...blankQuery(), ...S.q };
  const rq = ctx.route.query || {};
  if (rq.from && Number.isFinite(+rq.from)) {
    S.q = { ...blankQuery(), preset: 'custom', from: +rq.from, to: Number.isFinite(+rq.to) ? +rq.to : +rq.from + DAY };
    S.offset = 0; S.ran = true; S.at = 0;
    if (rq.sort) { S.sort = String(rq.sort); S.desc = rq.desc !== '0'; }
    if (typeof rq.icao === 'string' && /^[0-9A-Fa-f]{6}$/.test(rq.icao)) S.q.icao = rq.icao.toUpperCase();
  }
  const persist = () => ctx.shared.set(SHARED_KEY, JSON.parse(JSON.stringify(S)));
  scope.onCleanup(persist);

  // ---- form -----------------------------------------------------------------------------------
  const formHost = h('div.rpb-form');
  const resultsHost = h('div.rpb-results');
  const csvBtn = h('a.btn.btn--sm', { href: '#', download: '', tip: 'Download every matching flight as CSV (up to 100,000 rows)' }, ui.icon('download'), h('span', 'CSV'));
  const resCard = ui.card({ title: 'Flights', icon: 'list', sub: 'Run a search to list flights', actions: csvBtn, body: resultsHost, flush: true });
  resCard.classList.add('rpb-card');
  host.append(h('div.rpb', ui.card({ title: 'Criteria', icon: 'filter', body: formHost }), resCard));

  let fields = {};
  function renderForm() {
    const q = S.q;
    const txt = (key, label, ph, o = {}) => {
      const inp = ui.input({ value: q[key] || '', placeholder: ph, mono: !!o.mono, size: 'sm', clearable: true, maxlength: 64, onInput: (v) => { q[key] = v.trim(); }, onEnter: () => search() });
      return ui.field({ label, control: inp, tip: o.tip });
    };
    const num = (key, label, kind, tip) => {
      const conv = !!kind;
      const inp = ui.input({ type: 'number', value: fin(q[key]) ? (conv ? Math.round(units.value(kind, q[key]) * 10) / 10 : q[key]) : '', min: 0, step: 'any', size: 'sm', suffix: conv ? units.label(kind) : '',
        onInput: (v) => { q[key] = v === null ? null : conv ? units.toBase(kind, v) : v; }, onEnter: () => search() });
      return ui.field({ label, control: inp, tip });
    };
    const fromIn = ui.input({ type: 'datetime-local', size: 'sm', value: toInputUTC(q.from ?? live.serverNow() - DAY), onChange: (v) => { q.from = fromInputUTC(v); } });
    const toIn = ui.input({ type: 'datetime-local', size: 'sm', value: toInputUTC(q.to ?? live.serverNow()), onChange: (v) => { q.to = fromInputUTC(v); } });
    const custom = h('div.rpb-custom' + (q.preset === 'custom' ? '' : '.hidden'), ui.field({ label: 'From (UTC)', control: fromIn }), ui.field({ label: 'To (UTC)', control: toIn }));
    const preset = ui.segmented({ options: PRESETS.map((p) => ({ value: p.value, label: p.label })), value: q.preset, onChange: (v) => {
      q.preset = v;
      if (v === 'custom' && !fin(q.from)) { q.to = live.serverNow(); q.from = q.to - DAY; fromIn.value = toInputUTC(q.from); toIn.value = toInputUTC(q.to); }
      custom.classList.toggle('hidden', v !== 'custom');
    } });
    const mil = ui.select({ size: 'sm', options: [{ value: '', label: 'Any' }, { value: '1', label: 'Only military' }, { value: '0', label: 'No military' }], value: q.military, onChange: (v) => { q.military = v; } });
    const emer = ui.checkbox({ label: 'Declared an emergency', checked: !!q.emergency, onChange: (v) => { q.emergency = v; } });
    const errEl = h('div.field-error.hidden');
    fields = { errEl };
    ui.mount(formHost,
      h('div.rpb-range', ui.field({ label: 'Time range (flights overlapping it)', control: preset }), custom),
      h('div.rpb-grid',
        txt('callsign', 'Callsign', 'DLH*', { mono: true }), txt('reg', 'Registration', 'D-A*', { mono: true }), txt('icao', 'ICAO address', '3C*', { mono: true }),
        txt('type', 'Type', 'A32?', { mono: true }), txt('operator', 'Operator', 'Lufthansa'), txt('country', 'Country', 'Germany'),
        txt('squawk', 'Squawk', '77*', { mono: true }), ui.field({ label: 'Military', control: mil }),
        txt('dep', 'Departure', 'EDDF / FRA', { mono: true, tip: 'Departure airport: exact ICAO or IATA code (known once the flight has ended near an airport)' }),
        txt('arr', 'Arrival', 'EGLL / LHR', { mono: true, tip: 'Arrival airport: exact ICAO or IATA code' }),
        num('minDistKm', 'Farthest at least', 'dist', 'The flight got at least this far from the station'),
        num('maxDistKm', 'Closest at most', 'dist', 'The flight came at least this close to the station'),
        num('minAlt', 'Highest at least', 'alt', 'The highest altitude of the flight was at least this'),
        num('maxAlt', 'Lowest at most', 'alt', 'The lowest altitude of the flight was at most this'),
        num('minPositions', 'Positions at least', null, 'Flights with at least this many stored positions'),
        h('div.field.rpb-check', emer)),
      errEl,
      h('div.rpb-actions',
        ui.button({ label: 'Search', icon: 'search', variant: 'primary', onClick: () => search() }),
        ui.button({ label: 'Reset', icon: 'x', variant: 'ghost', onClick: () => { S.q = blankQuery(); S.offset = 0; renderForm(); search(); } }),
        h('span.rpb-hint.muted', 'Text fields take * and ? wildcards. Times are UTC.')));
  }

  /** Query parameters for the current criteria, or a string with the problem. */
  function params() {
    const q = S.q;
    let from; let to;
    if (q.preset === 'custom') {
      from = q.from; to = q.to;
      if (!fin(from) || !fin(to)) return 'Enter both a start and an end.';
      if (to <= from) return 'The end must be after the start.';
    } else {
      const p = PRESETS.find((x) => x.value === q.preset) || PRESETS[2];
      to = S.at || live.serverNow();
      from = to - p.ms;
    }
    if (q.icao && !/^[0-9A-Fa-f*?]{1,6}$/.test(q.icao)) return 'ICAO: up to six hex digits with * / ? wildcards.';
    for (const [k, l] of [['dep', 'Departure'], ['arr', 'Arrival']]) if (q[k] && !/^[A-Za-z0-9]{3,4}$/.test(q[k])) return `${l}: a 3-letter IATA or 4-letter ICAO airport code.`;
    const r = (v, d) => (fin(v) ? Math.round(v * 10 ** d) / 10 ** d : undefined);
    return {
      from: Math.round(from), to: Math.round(to),
      callsign: q.callsign, reg: q.reg, icao: q.icao, type: q.type, operator: q.operator, country: q.country, squawk: q.squawk,
      military: q.military, emergency: q.emergency ? 1 : undefined, dep: q.dep ? q.dep.toUpperCase() : undefined, arr: q.arr ? q.arr.toUpperCase() : undefined,
      minDistKm: r(q.minDistKm, 3), maxDistKm: r(q.maxDistKm, 3), minAlt: r(q.minAlt, 0), maxAlt: r(q.maxAlt, 0), minPositions: r(q.minPositions, 0),
    };
  }

  // ---- results ----------------------------------------------------------------------------------
  let data = null;
  let error = null;
  let loading = false;
  let seq = 0;

  function search(o = {}) {
    if (!o.keepPage) S.offset = 0;
    if (!o.keepTime) S.at = live.serverNow();
    S.ran = true;
    persist();
    run();
  }

  async function run() {
    const p = params();
    if (typeof p === 'string') { fields.errEl.classList.remove('hidden'); ui.setText(fields.errEl, p); return; }
    fields.errEl.classList.add('hidden');
    const my = ++seq;
    loading = true;
    const full = { ...p, sort: S.sort, desc: S.desc ? 1 : 0, limit: S.limit, offset: S.offset };
    csvBtn.href = urlWith('/api/reports.csv', { ...p, sort: S.sort, desc: S.desc ? 1 : 0, limit: 100000 });
    renderResults();
    try {
      const res = await api.get('/api/reports', { params: full, signal: scope.signal, timeout: 30000 });
      if (my !== seq) return;
      data = res; error = null;
    } catch (e) {
      if (e.aborted || my !== seq) return;
      error = e;
    }
    loading = false;
    renderResults();
  }

  const COLS = [
    { key: 'start', label: 'First seen', num: true, render: (r) => units.dateTime(r.start) },
    { key: 'duration', label: 'Duration', num: true, align: 'right', render: (r) => units.age(r.durationS) },
    { key: 'callsign', label: 'Callsign', mono: true, render: (r) => r.callsign || units.DASH },
    { key: 'icao', label: 'ICAO', mono: true, render: (r) => r.icao },
    { key: 'reg', label: 'Reg', mono: true, render: (r) => r.reg || units.DASH },
    { key: 'type', label: 'Type', mono: true, render: (r) => r.type || units.DASH, tip: (r) => r.typeDesc || '' },
    { key: 'operator', label: 'Operator', render: (r) => r.operator || units.DASH, cls: 'rpb-wide' },
    { key: 'country', label: 'Country', render: (r) => r.country || units.DASH },
    { key: 'dep', label: 'Dep', mono: true, airport: true, render: (r) => r.dep || units.DASH, tip: () => 'Departure airport' },
    { key: 'arr', label: 'Arr', mono: true, airport: true, render: (r) => r.arr || units.DASH, tip: () => 'Arrival airport' },
    { key: 'maxDistKm', label: 'Farthest', num: true, align: 'right', render: (r) => units.dist(r.maxDistKm) },
    { key: 'minDistKm', label: 'Closest', num: true, align: 'right', render: (r) => units.dist(r.minDistKm) },
    { key: 'maxAlt', label: 'Alt max', num: true, align: 'right', render: (r) => units.alt(r.maxAlt) },
    { key: 'minAlt', label: 'Alt min', num: true, align: 'right', render: (r) => units.alt(r.minAlt) },
    { key: 'maxGs', label: 'Speed max', num: true, align: 'right', render: (r) => units.speed(r.maxGs) },
    { key: 'positions', label: 'Pos.', num: true, align: 'right', render: (r) => units.int(r.positions) },
    { key: 'msgs', label: 'Msgs', num: true, align: 'right', render: (r) => units.int(r.msgs) },
    { key: 'rssiMax', label: 'RSSI max', num: true, align: 'right', render: (r) => units.dbfs(r.rssiMax, { unit: false }) },
    { key: '', label: '', render: (r) => flags(r) },
  ];

  function flags(r) {
    const out = [];
    if (r.emergency) out.push(ui.badge(r.emergency === 'true' || r.emergency === '1' ? 'Emergency' : 'Emerg. ' + r.emergency, 'solid-red'));
    if (r.military) out.push(ui.badge('Mil', 'amber'));
    if (r.interesting) out.push(ui.badge('★', 'violet'));
    for (const [k, l] of srcMaskLabels(r.srcMask || 0)) out.push(ui.badge(l, k));
    return h('span.rpb-flags', out);
  }

  function sortBy(key) {
    if (!key) return;
    if (S.sort === key) S.desc = !S.desc;
    else { S.sort = key; S.desc = !TEXT_SORTS.has(key); }
    S.offset = 0;
    persist();
    run();
  }

  function renderResults() {
    if (!S.ran) {
      ui.mount(resultsHost, ui.empty({ icon: 'report', title: 'Choose criteria and press Search', text: 'Every stored flight that overlaps the time range and matches all criteria is listed.' }));
      return;
    }
    if (error && !data) { ui.mount(resultsHost, ui.error(error, { title: 'The report could not be run', retry: run })); resCard.setSub('Error'); return; }
    if (!data) { ui.mount(resultsHost, h('div.rpb-pad', ui.skeleton({ lines: 8 }))); resCard.setSub('Searching…'); return; }
    const total = data.total || 0;
    resCard.setSub(`${units.int(total)} ${total === 1 ? 'flight' : 'flights'} · ${units.dateTime(data.from)} – ${units.dateTime(data.to)}`);
    if (!data.rows?.length) {
      ui.mount(resultsHost, error ? h('div.rpb-pad', ui.error(error, { compact: true, retry: run })) : null,
        ui.empty({ icon: 'search', title: total ? 'No rows on this page' : 'No flights match', text: total ? 'Go back a page.' : 'Not enough data yet for this range, or the criteria are too narrow. Widen the time range or clear some fields.' }));
      return;
    }
    // Dep / Arr only when the server knows airports (keys omitted while unknown and by older servers)
    const hasAirports = data.rows.some((r) => 'dep' in r || 'arr' in r) || S.sort === 'dep' || S.sort === 'arr';
    const cols = COLS.filter((c) => !c.airport || hasAirports);
    const thead = h('thead', h('tr', cols.map((c) => {
      const active = c.key && S.sort === c.key;
      const th = h('th' + (c.align ? '.' + c.align : '') + (c.key ? '.sortable' : ''), { 'aria-sort': active ? (S.desc ? 'descending' : 'ascending') : null, scope: 'col' }, c.label);
      if (c.key) { th.tabIndex = 0; th.addEventListener('click', () => sortBy(c.key)); th.addEventListener('keydown', (e) => { if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); sortBy(c.key); } }); }
      return th;
    })));
    const tbody = h('tbody', data.rows.map((r) => {
      const tr = h('tr.rpb-row', { tabIndex: 0, 'aria-label': `Flight ${r.callsign || r.icao}` }, cols.map((c) => {
        const v = c.render(r);
        return h('td' + (c.align ? '.' + c.align : '') + (c.num || c.mono ? '.num' : '') + (c.cls ? '.' + c.cls : ''), { tip: c.tip ? c.tip(r) || null : null }, v);
      }));
      const open = () => ctx.navigate('reports/flight/' + r.flightId);
      tr.addEventListener('click', open);
      tr.addEventListener('keydown', (e) => { if (e.key === 'Enter') open(); });
      return tr;
    }));
    const from = S.offset + 1;
    const to = Math.min(total, S.offset + data.rows.length);
    const size = ui.select({ size: 'sm', options: PAGE_SIZES.map((n) => ({ value: String(n), label: `${n} / page` })), value: String(S.limit), onChange: (v) => { S.limit = +v; S.offset = 0; persist(); run(); } });
    const prev = ui.button({ icon: 'minus', label: 'Prev', size: 'sm', variant: 'ghost', disabled: S.offset <= 0 || loading, onClick: () => { S.offset = Math.max(0, S.offset - S.limit); persist(); run(); } });
    const next = ui.button({ icon: 'plus', label: 'Next', size: 'sm', variant: 'ghost', disabled: to >= total || loading, onClick: () => { S.offset += S.limit; persist(); run(); } });
    ui.mount(resultsHost,
      error ? h('div.rpb-pad', ui.error(error, { compact: true, retry: run })) : null,
      h('div.table-wrap.rpb-table' + (loading ? '.is-loading' : ''), h('table.table', thead, tbody)),
      h('div.rpb-pager', h('span.num.muted', `${units.int(from)}–${units.int(to)} of ${units.int(total)}`), h('span.spacer'), size, prev, next));
  }

  renderForm();
  renderResults();
  if (S.ran) run();
  scope.onCleanup(units.onChange(() => { renderForm(); renderResults(); }));
}
