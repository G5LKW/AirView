// map/list.js - the aircraft list side panel of the Live Map: search, quick filters, configurable
// sortable virtualised table (column chooser, persisted), hover / selection / multi-selection wiring.

import { ui, h, icon, setText } from '../ui.js';
import { units } from '../units.js';
import { parseQuery, matchTerms } from './route-query.js';

const ROUTE_QUERY_TIP = 'Route terms: from:EGLL to:JFK via:FRA airport:LHR route:LHR-JFK (route:LHR~JFK either way) airline:BAW state:doubtful src:learned kind:positioning eta<30m noroute; * and ? wildcards, a leading - negates.';

const SOURCE_BADGE = { adsb: ['ADS-B', 'adsb'], mlat: ['MLAT', 'mlat'], tisb: ['TIS-B', 'tisb'], adsr: ['ADS-R', 'adsr'], modes: ['Mode S', 'modes'], other: ['Other', 'other'] };
const EMERGENCY_SQUAWKS = new Set(['7500', '7600', '7700']);

export const isEmergency = (a) => !!a.emergency || EMERGENCY_SQUAWKS.has(a.squawk);
export const hasPosition = (a) => typeof a.lat === 'number' && typeof a.lon === 'number';
/**
 * A position is stale when it has not been refreshed for a while (MLAT updates are naturally slower).
 * Aircraft shown at a predicted position are never "stale": the map draws them as coasting instead.
 */
export const isStale = (a) => !a.predicted && typeof a.posAge === 'number' && a.posAge > (a.source === 'mlat' ? 30 : 15);

const FILTERS = [
  { id: 'pos', label: 'Position', tip: 'Only aircraft with a position', test: hasPosition },
  { id: 'adsb', label: 'ADS-B', group: 'src', tip: 'ADS-B positions', test: (a) => a.source === 'adsb' || a.source === 'adsr' },
  { id: 'mlat', label: 'MLAT', group: 'src', tip: 'Multilateration positions', test: (a) => a.source === 'mlat' },
  { id: 'mil', label: 'Military', tip: 'Military aircraft', test: (a) => !!a.military },
  { id: 'emg', label: 'Emergency', tip: 'Squawk 7500 / 7600 / 7700 or ADS-B emergency status', test: isEmergency, alert: true },
];

const DEFAULT_COLS = ['country', 'callsign', 'icao', 'type', 'alt', 'gs', 'dist', 'brg', 'rssi', 'source', 'msgs', 'age'];
const n = (v) => typeof v === 'number' && Number.isFinite(v);

/** Minimum pixel width of a CSS grid track spec ('72px', 'minmax(84px,1fr)'). */
const minPx = (w) => { const m = /(\d+)px/.exec(w || ''); return m ? Number(m[1]) : 60; };

/**
 * @param {object} ctx page context
 * @param {{ onSelect(icao: string, e?: Event), onHover(icao: string|null), colorOf(a): string, onClose(): void,
 *           filter?: () => ((a) => boolean)|null, isGrouped?: (icao) => boolean }} hooks
 */
export function createList(ctx, hooks) {
  const prefs = ui.prefs('airview.map.list', { sortKey: null, sortDir: 'asc', filters: [], wide: false, q: '', cols: null });
  let all = [];
  let now = Date.now();
  let query = prefs.get().q || '';
  const active = new Set(prefs.get().filters || []);

  const countEl = h('span.num', '0');
  const shownEl = h('span.side-shown.muted', '');

  const search = ui.input({
    icon: 'search', placeholder: 'Filter: callsign, ICAO, type… or to:LHR, route:LHR-JFK  ( / )', tip: ROUTE_QUERY_TIP, size: 'sm', clearable: true, value: query,
    spellcheck: 'false', autocomplete: 'off', 'aria-label': 'Filter aircraft',
    onInput: ui.debounce((v) => { query = v.trim(); prefs.set({ q: query }); apply(); }, 120),
  });
  if (query) search.querySelector('.input-clear')?.classList.remove('hidden');

  const filterBtns = new Map();
  const filterBar = h('div.side-filters', FILTERS.map((f) => {
    const cnt = h('span.num.side-filter-n', '0');
    const b = h('button.side-filter' + (f.alert ? '.side-filter--alert' : ''), { type: 'button', tip: f.tip, 'aria-pressed': String(active.has(f.id)) }, f.label, cnt);
    b.addEventListener('click', () => {
      if (active.has(f.id)) active.delete(f.id); else active.add(f.id);
      b.setAttribute('aria-pressed', String(active.has(f.id)));
      prefs.set({ filters: [...active] });
      apply();
    });
    filterBtns.set(f.id, { b, n: cnt });
    return b;
  }));

  const colsBtn = ui.button({ icon: 'settings', variant: 'ghost', size: 'sm', tip: 'Choose columns', onClick: () => openChooser() });
  const wideBtn = ui.button({ icon: 'panel', variant: 'ghost', size: 'sm', class: 'side-wide', tip: 'Wider list', pressed: prefs.get().wide, onClick: () => setWide(!prefs.get().wide) });
  const closeBtn = ui.button({ icon: 'x', variant: 'ghost', size: 'sm', tip: 'Hide list', class: 'side-close', onClick: () => hooks.onClose && hooks.onClose() });

  const fmtAlt = (a) => {
    if (a.onGround) return 'GND';
    const v = a.altBaro ?? a.altGeom;
    return n(v) ? units.format('alt', v, { unit: false }) : '';
  };
  const distTxt = (km) => (n(km) ? units.format('dist', km, { unit: false, digits: units.value('dist', km) >= 100 ? 0 : 1 }) : '');

  /** Every column the chooser offers, in display order. `fixed` columns cannot be hidden. */
  const COLUMNS = [
    {
      key: 'country', label: '', name: 'Flag', width: '40px', align: 'center', tip: 'Country of registration', sort: (a) => a.country || a.countryCode,
      update(td, a) {
        const cc = a.countryCode || '';
        if (td.__cc === cc) return;
        td.__cc = cc;
        ui.mount(td, ui.flag(cc, a.country));
      },
    },
    {
      key: 'callsign', label: 'Callsign', fixed: true, width: 'minmax(84px,1fr)', sort: (a) => a.callsign || null,
      update(td, a) {
        if (!td.__dot) { td.__dot = h('i.ac-dot'); td.__txt = h('span'); td.append(td.__dot, td.__txt); }
        const c = hooks.colorOf(a);
        if (td.__c !== c) { td.__c = c; td.__dot.style.background = c; td.__dot.style.color = c; }
        const p = a.predicted ? 'ac-dot pred' : 'ac-dot';
        if (td.__p !== p) { td.__p = p; td.__dot.className = p; }
        const cs = a.callsign || '';
        if (setText(td.__txt, cs || '····')) td.__txt.className = cs ? 'cs' : 'cs dim';
      },
    },
    { key: 'icao', label: 'ICAO', width: '60px', num: true, cls: () => 'muted', sort: true },
    { key: 'reg', label: 'Reg', name: 'Registration', width: '72px', num: true, sort: (a) => a.reg || null, render: (a) => a.reg || '' },
    { key: 'type', label: 'Type', width: '52px', num: true, sort: (a) => a.type || null, render: (a) => a.type || '' },
    { key: 'operator', label: 'Operator', width: 'minmax(96px,1.2fr)', sort: (a) => a.operator || null, render: (a) => a.operator || '', cls: () => 'ellipsis' },
    {
      key: 'route', label: 'Route', width: '92px', tip: 'Route from the reference data (amber = does not fit the position)', sort: (a) => (a.route ? a.route.from + a.route.to : null),
      render: (a) => (a.route ? `${a.route.fromIata || a.route.from}–${a.route.toIata || a.route.to}` : ''),
      cls: (a) => (a.route && (a.route.state ? a.route.state === 'doubtful' || a.route.state === 'reversed' : a.route.checked && !a.route.plausible) ? 'route-doubt' : ''),
    },
    // optional route columns (Columns menu; ROUTES-DESIGN §6.4)
    { key: 'rfrom', label: 'From', name: 'Route from', width: '52px', num: true, sort: (a) => (a.route ? a.route.fromIata || a.route.from : null), render: (a) => (a.route ? a.route.fromIata || a.route.from : '') },
    { key: 'rto', label: 'To', name: 'Route to', width: '52px', num: true, sort: (a) => (a.route ? a.route.toIata || a.route.to : null), render: (a) => (a.route ? a.route.toIata || a.route.to : '') },
    { key: 'rprog', label: 'Prog', name: 'Route progress', width: '48px', align: 'right', num: true, tip: 'Progress along the active leg', sort: (a) => (n(a.route?.along) ? a.route.along : null), render: (a) => (n(a.route?.along) ? Math.round(a.route.along) + '%' : '') },
    { key: 'reta', label: 'ETA', name: 'Estimated arrival', width: '52px', num: true, tip: 'Estimated arrival (your time zone)', sort: (a) => (a.route?.eta > 0 ? a.route.eta : null),
      render: (a) => (a.route?.eta > 0 ? new Date(a.route.eta * 1000).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' }) : '') },
    { key: 'rconf', label: 'Conf', name: 'Route confidence', width: '46px', align: 'right', num: true, sortDir: 'desc', sort: (a) => (n(a.route?.conf) ? a.route.conf : null), render: (a) => (n(a.route?.conf) ? Math.round(a.route.conf * 100) + '%' : '') },
    { key: 'rsrc', label: 'Src', name: 'Route source', width: '72px', sort: (a) => a.route?.source || null, render: (a) => a.route?.source || '', cls: () => 'ellipsis muted' },
    { key: 'squawk', label: 'Sqk', name: 'Squawk', width: '46px', num: true, sort: (a) => a.squawk || null, render: (a) => a.squawk || '', cls: (a) => (EMERGENCY_SQUAWKS.has(a.squawk) ? 'sq-emerg' : '') },
    {
      key: 'alt', label: 'Alt', name: 'Altitude', width: '72px', align: 'right', num: true, sortDir: 'desc', tip: 'Altitude',
      sort: (a) => (a.onGround ? 0 : a.altBaro ?? a.altGeom ?? null),
      update(td, a) {
        if (!td.__v) { td.__v = h('span'); td.__a = h('span.vs'); td.append(td.__v, td.__a); }
        setText(td.__v, fmtAlt(a));
        const vr = a.vertRate;
        const dir = n(vr) ? (vr > 150 ? 'up' : vr < -150 ? 'down' : '') : '';
        if (td.__d !== dir) { td.__d = dir; td.__a.className = 'vs ' + dir; td.__a.textContent = dir === 'up' ? '↑' : dir === 'down' ? '↓' : ''; }
      },
    },
    { key: 'vrate', label: 'V/S', name: 'Vertical rate', width: '64px', align: 'right', num: true, sortDir: 'desc', tip: 'Vertical rate', sort: (a) => a.vertRate ?? null, render: (a) => (n(a.vertRate) ? units.format('vrate', a.vertRate, { unit: false, sign: true }) : '') },
    { key: 'gs', label: 'Spd', name: 'Ground speed', width: '46px', align: 'right', num: true, sortDir: 'desc', tip: 'Ground speed', sort: (a) => a.gs ?? null, render: (a) => (n(a.gs) ? units.format('speed', a.gs, { unit: false }) : '') },
    { key: 'dist', label: 'Dist', name: 'Distance', width: '52px', align: 'right', num: true, tip: 'Distance from the receiver', sort: (a) => a.distKm ?? null, render: (a) => distTxt(a.distKm) },
    { key: 'brg', label: 'Brg', name: 'Bearing', width: '42px', align: 'right', num: true, tip: 'Bearing from the receiver', sort: (a) => a.bearing ?? null, render: (a) => (n(a.bearing) ? String(Math.round(a.bearing) % 360).padStart(3, '0') + '°' : '') },
    { key: 'elev', label: 'Elev', name: 'Elevation angle', width: '46px', align: 'right', num: true, sortDir: 'desc', tip: 'Elevation above the receiver horizon', sort: (a) => a.elevation ?? null, render: (a) => (n(a.elevation) ? units.num(a.elevation, 1, 1) + '°' : '') },
    {
      key: 'rssi', label: 'Sig', name: 'Signal (RSSI)', width: '36px', align: 'center', sortDir: 'desc', tip: 'Signal strength (smoothed RSSI)', sort: (a) => a.rssiAvg ?? a.rssi ?? null,
      update(td, a) {
        if (!td.__bar) { td.__bar = ui.signalBar(null); td.appendChild(td.__bar); }
        const v = a.rssiAvg ?? a.rssi;
        td.__bar.set(v);
        const t = n(v) ? units.dbfs(v) : '';
        if (td.__tip !== t) { td.__tip = t; if (t) td.dataset.tip = t; else delete td.dataset.tip; }
      },
    },
    { key: 'rssiv', label: 'dBFS', name: 'RSSI value', width: '50px', align: 'right', num: true, sortDir: 'desc', tip: 'Smoothed RSSI, dBFS', sort: (a) => a.rssiAvg ?? a.rssi ?? null, render: (a) => units.dbfs(a.rssiAvg ?? a.rssi, { unit: false }).replace(units.DASH, '') },
    {
      key: 'source', label: 'Src', name: 'Source', width: '62px', sort: (a) => a.source,
      update(td, a) {
        const s = a.predicted ? 'pred' : SOURCE_BADGE[a.source] ? a.source : 'other';
        if (td.__s === s) return;
        td.__s = s;
        ui.mount(td, s === 'pred' ? ui.badge('PRED', 'violet', { tip: 'Shown at a predicted position' }) : ui.badge(SOURCE_BADGE[s][0], SOURCE_BADGE[s][1]));
      },
    },
    {
      key: 'los', label: 'LOS', name: 'LOS margin', width: '52px', align: 'right', num: true, sortDir: 'desc', tip: 'Elevation above the terrain skyline (negative = hidden by terrain)',
      sort: (a) => a.losMarginDeg ?? null, render: (a) => (n(a.losMarginDeg) ? (a.losMarginDeg > 0 ? '+' : '') + units.num(a.losMarginDeg, 1, 1) + '°' : ''),
      cls: (a) => (n(a.losMarginDeg) && a.losMarginDeg < 0 ? 'los-neg' : ''),
    },
    {
      key: 'cpa', label: 'CPA', name: 'Closest approach', width: '76px', align: 'right', num: true, tip: 'Predicted closest approach to the station (distance · time to go)',
      sort: (a) => a.cpaKm ?? null,
      render: (a) => (n(a.cpaKm) ? distTxt(a.cpaKm) + (n(a.cpaInS) && a.cpaInS > 0 ? ' · ' + units.age(a.cpaInS).split(' ')[0] : '') : ''),
    },
    { key: 'msgs', label: 'Msgs', name: 'Messages', width: '58px', align: 'right', num: true, sortDir: 'desc', tip: 'Messages received this session', sort: (a) => a.msgs, render: (a) => units.si(a.msgs), cls: () => 'muted' },
    { key: 'age', label: 'Age', width: '42px', align: 'right', num: true, tip: 'Time since the last message', sort: (a) => -a.lastSeen, render: (a) => units.age(Math.max(0, (now - a.lastSeen) / 1000)), cls: () => 'muted' },
  ];
  const BY_KEY = new Map(COLUMNS.map((c) => [c.key, c]));

  const located = () => !!ctx.status?.located;
  const tableHost = h('div.side-table');
  let table = null;

  function visibleCols() {
    const want = Array.isArray(prefs.get().cols) ? prefs.get().cols : DEFAULT_COLS;
    const set = new Set(want.filter((k) => BY_KEY.has(k)));
    set.add('callsign');
    return COLUMNS.filter((c) => set.has(c.key));
  }

  function buildTable() {
    const cols = visibleCols();
    const prevRows = table ? table.rows() : null;
    const sel = table ? table.__sel : null;
    if (table) table.destroy();
    const sortKey = prefs.get().sortKey;
    const sort = sortKey && cols.some((c) => c.key === sortKey) ? { key: sortKey, dir: prefs.get().sortDir } : { key: located() ? 'dist' : 'callsign', dir: 'asc' };
    table = ui.dataTable({
      columns: cols,
      rowKey: (a) => a.icao,
      rowHeight: 28,
      minWidth: cols.reduce((s, c) => s + minPx(c.width), 0),
      sort,
      onSort: (s) => prefs.set({ sortKey: s.key, sortDir: s.dir }),
      onRowClick: (a, e) => hooks.onSelect(a.icao, e),
      onRowHover: (a) => hooks.onHover(a ? a.icao : null),
      rowClass: (a) => {
        let c = isEmergency(a) ? 'alert' : hasPosition(a) ? (isStale(a) ? 'dim' : a.predicted ? 'pred' : '') : 'nopos';
        if (a.highlight) c += ' hl';
        if (hooks.isGrouped && hooks.isGrouped(a.icao)) c += ' grp';
        return c;
      },
      empty: ui.empty({ icon: 'plane', title: 'No aircraft', text: 'Nothing matches the current filters, or no traffic is being received.', compact: true }),
    });
    table.__sel = sel;
    ui.mount(tableHost, table.el);
    if (sel) table.setSelected(sel);
    if (prevRows) apply();
  }

  function openChooser() {
    const cur = new Set(visibleCols().map((c) => c.key));
    const save = () => { prefs.set({ cols: COLUMNS.filter((c) => cur.has(c.key)).map((c) => c.key) }); buildTable(); };
    const body = h('div.mapv-panel.mapv-cols',
      ui.section('List columns', 'list'),
      h('div.mapv-cols-grid', COLUMNS.map((c) => ui.checkbox({
        label: c.name || c.label, checked: cur.has(c.key), disabled: c.fixed,
        onChange: (v) => { if (v) cur.add(c.key); else cur.delete(c.key); save(); },
      }))),
      h('div.mapv-panel-foot', ui.button({ label: 'Default columns', size: 'sm', variant: 'ghost', icon: 'refresh', onClick: () => { prefs.set({ cols: null }); buildTable(); pop.close(); } })));
    const pop = ui.popover(colsBtn, body, { placement: 'bottom-end' });
  }

  const el = h('aside.mapv-side', { 'aria-label': 'Aircraft list' },
    h('div.side-head', h('h3', icon('list'), 'Aircraft', countEl), shownEl, h('div.spacer'), colsBtn, wideBtn, closeBtn),
    h('div.side-tools', search, filterBar),
    tableHost);
  buildTable();

  function setWide(on) {
    prefs.set({ wide: on });
    el.classList.toggle('wide', on);
    wideBtn.setPressed(on);
    ui.tip(wideBtn, on ? 'Narrower list' : 'Wider list');
  }
  setWide(!!prefs.get().wide);

  function matches(a, q) {
    if (q.terms.length && !matchTerms(a, q.terms, Math.floor(now / 1000))) return false;
    if (!q.plain.length) return true;
    const r = a.route;
    const hay = `${a.callsign || ''} ${a.icao} ${a.reg || ''} ${a.type || ''} ${a.typeDesc || ''} ${a.operator || ''} ${a.country || ''} ${a.squawk || ''} ${r ? `${r.from} ${r.to} ${r.fromIata || ''} ${r.toIata || ''}` : ''}`.toLowerCase();
    for (const t of q.plain) if (!hay.includes(t)) return false;
    return true;
  }

  function apply() {
    const tokens = parseQuery(query);
    const single = FILTERS.filter((f) => !f.group && active.has(f.id));
    const srcGroup = FILTERS.filter((f) => f.group === 'src' && active.has(f.id));
    const adv = hooks.filter ? hooks.filter() : null;
    const counts = Object.fromEntries(FILTERS.map((f) => [f.id, 0]));
    const out = [];
    for (const a of all) {
      for (const f of FILTERS) if (f.test(a)) counts[f.id]++;
      if (adv && !adv(a)) continue;
      if (!matches(a, tokens)) continue;
      if (single.some((f) => !f.test(a))) continue;
      if (srcGroup.length && !srcGroup.some((f) => f.test(a))) continue;
      out.push(a);
    }
    for (const f of FILTERS) {
      const { b, n: cnt } = filterBtns.get(f.id);
      setText(cnt, counts[f.id]);
      if (f.alert) b.classList.toggle('has', counts[f.id] > 0);
    }
    setText(countEl, all.length);
    setText(shownEl, out.length === all.length ? '' : `${out.length} shown`);
    table.setRows(out);
    return out;
  }

  return {
    el,
    /** Feed a new snapshot. */
    update(snap) {
      all = snap.aircraft;
      now = snap.now || Date.now();
      return apply();
    },
    /** Re-run the filters (after the map's advanced filter changed). */
    reapply() { apply(); },
    setSelected(icao) { table.__sel = icao; table.setSelected(icao); },
    setHover(icao) { table.setHover(icao); },
    reveal(icao) { table.scrollToKey(icao); },
    /** Put the keyboard focus into the search box. */
    focusSearch() { search.input.focus(); search.input.select(); },
    refresh() { table.refresh(); apply(); },
    destroy() { table.destroy(); },
  };
}
