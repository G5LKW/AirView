// pages/aircraft.js - the aircraft database browser.
//
//   #/aircraft                 airframe table (search, sort, paging, filters)
//   #/aircraft/<ICAO>          the same with the detail drawer of one airframe open (?flight=<id> selects a flight)
//   #/aircraft/flights         recent flights            #/aircraft/events   event log + all-time records
//   #/aircraft/database        aircraft-database (registration / type lookup data) status and import
//
// Endpoints: /api/history/aircraft, /api/history/aircraft/{icao}, /api/aircraft/{icao} (fallback),
// /api/history/flights, /api/history/flights/{id}, /api/stats/events, /api/stats/records, /api/stats/top,
// /api/astro/days (effective receiver position), /api/db/acdb/{sources,progress,import,upload,cancel,lookup}, /api/login.

import { createAdmin } from '../admin.js';
import { createTrailMap } from '../aircraft/trailmap.js';
import { altColor, signalColor } from '../map/palette.js';

const TABS = ['airframes', 'flights', 'events', 'database'];
const HEX = /^~?[0-9A-Fa-f]{6}$/;
const SRC_BITS = [[1, 'adsb'], [2, 'mlat'], [4, 'tisb'], [8, 'other'], [16, 'modes']];
const FLAG_BITS = [[1, 'Military', 'amber', 'Military airframe (aircraft database flag or military address block)'], [2, 'Interesting', 'violet', 'Flagged as interesting in the aircraft database'], [4, 'PIA', 'teal', 'Privacy ICAO Address: a temporary address that hides the real airframe'], [8, 'LADD', 'teal', 'Limiting Aircraft Data Displayed: the owner asked for the data not to be shown publicly']];
const EVENT_KINDS = {
  emergency: ['Emergency', 'red'], military: ['Military', 'amber'], interesting: ['Interesting', 'violet'],
  record: ['Record', 'cyan'], feed_up: ['Feed up', 'green'], feed_down: ['Feed down', 'red'], alert: ['Alert', 'blue'],
};
const COUNTRY_ALIASES = {
  turkey: 'TR', congo: 'CG', 'dr congo': 'CD', 'hong kong': 'HK', macau: 'MO', macao: 'MO', myanmar: 'MM', 'cabo verde': 'CV',
  palestine: 'PS', 'sao tome and principe': 'ST', 'ivory coast': 'CI', 'cote d ivoire': 'CI', 'czech republic': 'CZ', 'russian federation': 'RU',
};

let page = null; // { onRoute(route) } of the mounted instance

function normCountry(s) {
  return String(s).normalize('NFD').replace(/[̀-ͯ]/g, '').toLowerCase().replace(/&/g, 'and').replace(/\bst\.?\s/g, 'saint ').replace(/[^a-z ]+/g, ' ').replace(/\s+/g, ' ').trim();
}

/** Country name -> ISO 3166-1 alpha-2 (history rows carry only the name). */
function makeCountryCodes(live) {
  const map = new Map(Object.entries(COUNTRY_ALIASES));
  try {
    const dn = new Intl.DisplayNames(['en'], { type: 'region' });
    for (let a = 65; a <= 90; a++) {
      for (let b = 65; b <= 90; b++) {
        const cc = String.fromCharCode(a, b);
        let name;
        try { name = new Intl.Locale('und-' + cc).region === cc ? dn.of(cc) : null; } catch { name = null; } // skips retired codes (DD -> DE)
        if (name && name !== cc && !/unknown/i.test(name)) { const k = normCountry(name); if (!map.has(k)) map.set(k, cc); }
      }
    }
  } catch { /* Intl.DisplayNames missing: aliases and live data only */ }
  return (name) => {
    if (!name) return '';
    const k = normCountry(name);
    let cc = map.get(k);
    if (!cc) {
      for (const a of live.aircraft.values()) if (a.country === name && a.countryCode) { cc = a.countryCode; map.set(k, cc); break; }
    }
    return cc || '';
  };
}

/** Plain table with server-side sorting (click on a header calls onSort) and a state row for loading / empty / error. */
function makeTable(ui, opts) {
  const { h } = ui;
  const cols = opts.columns;
  let sort = opts.sort ? { ...opts.sort } : null;
  const thead = h('thead');
  const tbody = h('tbody');
  const el = h('div.table-wrap.ac-table', { class: opts.class }, h('table.table', thead, tbody));
  const ths = cols.map((c) => {
    const th = h('th' + (c.align === 'right' ? '.right' : '') + (c.sortKey ? '.sortable' : ''), { tip: c.tip }, c.label || '');
    if (c.sortKey) {
      th.addEventListener('click', () => {
        sort = { key: c.sortKey, dir: sort && sort.key === c.sortKey ? (sort.dir === 'asc' ? 'desc' : 'asc') : c.sortDir || 'asc' };
        paintSort();
        if (opts.onSort) opts.onSort(sort);
      });
    }
    return th;
  });
  thead.appendChild(h('tr', ths));
  function paintSort() {
    cols.forEach((c, i) => {
      if (sort && c.sortKey && sort.key === c.sortKey) ths[i].setAttribute('aria-sort', sort.dir === 'desc' ? 'descending' : 'ascending');
      else ths[i].removeAttribute('aria-sort');
    });
  }
  paintSort();
  el.setRows = (rows) => {
    el.classList.remove('is-busy');
    ui.mount(tbody, rows.map((r) => {
      const tr = h('tr', { class: opts.rowClass ? opts.rowClass(r) : null }, cols.map((c) => {
        const v = c.render(r);
        return h('td' + (c.align === 'right' ? '.right' : '') + (c.num ? '.num' : '') + (c.cls ? '.' + c.cls : ''), v instanceof Node ? v : v ?? '—');
      }));
      if (opts.rowKey) tr.dataset.key = opts.rowKey(r);
      if (opts.onRowClick) { tr.classList.add('ac-click'); tr.addEventListener('click', () => opts.onRowClick(r)); }
      return tr;
    }));
  };
  el.setState = (node) => { el.classList.remove('is-busy'); ui.mount(tbody, h('tr.ac-state-row', h('td', { colspan: cols.length }, node))); };
  el.setBusy = (on) => el.classList.toggle('is-busy', !!on);
  el.setSelected = (key) => { for (const tr of tbody.children) tr.classList.toggle('selected', key !== null && key !== undefined && tr.dataset.key === String(key)); };
  el.rowsEl = tbody;
  return el;
}

export default {
  id: 'aircraft',
  title: 'Aircraft',

  async mount(container, ctx) {
    const { api, live, units, ui, charts } = ctx;
    const { h } = ui;
    await ui.loadCss(new URL('../../css/aircraft.css', import.meta.url));

    const admin = createAdmin(ctx);
    const countryCode = makeCountryCodes(live);
    const prefs = ui.prefs('airview.aircraft', { size: 50 });
    const now = () => live.serverNow();
    const DASH = units.DASH;
    let alive = true;
    ctx.onCleanup(() => { alive = false; });

    // ------------------------------------------------------------------------------------------
    // small formatters
    // ------------------------------------------------------------------------------------------
    const when = (ms) => (ms ? h('span', { tip: units.ago(ms, now()) }, units.dateTime(ms)) : DASH);
    const ago = (ms) => (ms ? h('span', { tip: units.dateTime(ms) }, units.ago(ms, now())) : DASH);
    const flagCell = (name, cc) => (name ? h('span.ac-country', ui.flag(cc || countryCode(name), name), h('span.ellipsis', name)) : DASH);
    const srcBadges = (mask) => {
      const list = SRC_BITS.filter(([bit]) => mask & bit).map(([, src]) => ui.sourceBadge(src));
      return list.length ? h('span.ac-badges', list) : DASH;
    };
    const flagBadges = (flags, military) => {
      const f = (flags || 0) | (military ? 1 : 0);
      const list = FLAG_BITS.filter(([bit]) => f & bit).map(([, label, kind, tip]) => ui.badge(label, kind, { tip }));
      return list.length ? h('span.ac-badges', list) : null;
    };
    const range = (lo, hi, fmt) => {
      if (lo === undefined && hi === undefined) return DASH;
      if (lo === undefined || hi === undefined || lo === hi) return fmt(lo ?? hi);
      return fmt(lo, { unit: false }) + ' – ' + fmt(hi);
    };
    const icaoCell = (icao) => h('span.ac-icao', h('i.ac-live-dot', { tip: 'Tracked right now' }), icao);
    const isMil = (r) => !!((r.flags & 1) || r.military);

    const RECORDS = {
      max_dist_km: ['Farthest position', 'ruler', (v) => units.dist(v)],
      max_alt_ft: ['Highest altitude', 'arrow-up', (v) => units.alt(v)],
      max_gs_kt: ['Fastest ground speed', 'bolt', (v) => units.speed(v)],
      min_rssi_pos_dbfs: ['Weakest decoded position', 'signal', (v) => units.dbfs(v)],
      max_aircraft: ['Most aircraft at once', 'plane', (v) => units.int(v)],
      max_msg_rate: ['Highest message rate', 'chart', (v) => units.rate(v)],
      max_flight_s: ['Longest contact', 'timer', (v) => units.age(v)],
    };
    const recordValue = (key, v) => (RECORDS[key] ? RECORDS[key][2](v) : units.num(v, 1));

    function eventText(e) {
      const d = e.detail || {};
      const who = [d.callsign, d.reg, d.type].filter(Boolean).join(' · ');
      switch (e.kind) {
        case 'emergency': return `${who || e.icao || 'Aircraft'} squawks ${d.squawk || 'an emergency code'}${d.emergency ? ' (' + d.emergency + ')' : ''}`;
        case 'military': return ['Military aircraft', who, d.operator, d.country].filter(Boolean).join(' · ');
        case 'interesting': return ['Notable airframe', who, d.operator, d.country].filter(Boolean).join(' · ');
        case 'record': {
          const name = RECORDS[d.key] ? RECORDS[d.key][0] : d.key || 'Record';
          let cur = recordValue(d.key, d.value);
          let was = d.previous !== undefined && d.previous !== null ? recordValue(d.key, d.previous) : null;
          if (was !== null && was === cur && d.key !== 'max_flight_s' && typeof d.value === 'number' && typeof d.previous === 'number') { // e.g. 395.1/s vs 394.7/s
            const unit = cur.replace(/^[−\-+\d.,\s]+/, ''); // '/s', ' km', ...
            const kind = { max_dist_km: 'dist', max_alt_ft: 'alt', max_gs_kt: 'speed' }[d.key];
            const conv = (v) => (kind ? units.value(kind, v) : v);
            cur = units.num(conv(d.value), 1) + unit; was = units.num(conv(d.previous), 1) + unit;
          }
          return `${name}: ${cur}${was !== null ? ` (was ${was})` : ''}${who ? ' · ' + who : ''}`;
        }
        case 'feed_up': return `Feed "${d.name || d.feed || '?'}" connected`;
        case 'feed_down': return `Feed "${d.name || d.feed || '?'}" lost${d.error ? ': ' + d.error : ''}`;
        // alert events carry the alerts.Alert: rebuild the text in the display units when it is the default one
        case 'alert': return units.alertText(d) || d.ruleName || 'Alert';
        default: return typeof d.text === 'string' && d.text ? d.text : e.kind;
      }
    }
    function eventWhere(e) {
      const d = e.detail || {};
      const parts = [];
      if (typeof d.distKm === 'number') parts.push(units.dist(d.distKm) + (typeof d.bearing === 'number' ? ' ' + units.cardinal(d.bearing) : ''));
      if (typeof d.altFt === 'number') parts.push(units.alt(d.altFt, { fl: true }));
      if (typeof d.gsKt === 'number') parts.push(units.speed(d.gsKt));
      return parts.join(' · ') || DASH;
    }
    const kindBadge = (kind) => { const k = EVENT_KINDS[kind] || [kind, 'other']; return ui.badge(k[0], k[1]); };

    // effective receiver position (config says "not located" in demo mode; the astro endpoint knows the effective one)
    let receiver = null;
    const cfgRx = ctx.config.current?.receiver;
    if (cfgRx?.located) receiver = { lat: cfgRx.lat, lon: cfgRx.lon };
    api.get('/api/astro/days', { params: { days: 1 }, signal: ctx.signal }).then((r) => { if (r && r.located && r.receiver) receiver = r.receiver; }).catch(() => {});

    // ------------------------------------------------------------------------------------------
    // tab 1: airframes
    // ------------------------------------------------------------------------------------------
    const S = { q: '', seen: 'all', mil: false, reg: false, country: '', type: '', sort: 'last_seen', desc: true, page: 0, size: prefs.get().size || 50 };
    let afSeq = 0;
    let afLast = null; // { rows, total, scanned, truncated }
    let afCache = null; // { key, t, rows, truncated } full result set for client-side filters
    const FULL_PAGE = 1000;
    const FULL_MAX = 20;

    const afTable = makeTable(ui, {
      sort: { key: 'last_seen', dir: 'desc' },
      rowKey: (r) => r.icao,
      onRowClick: (r) => openAircraft(r.icao),
      onSort: (s) => { S.sort = s.key; S.desc = s.dir === 'desc'; S.page = 0; loadAirframes(); },
      columns: [
        { key: 'icao', label: 'ICAO', sortKey: 'icao', render: (r) => icaoCell(r.icao), tip: '24-bit Mode S address (hex)' },
        { key: 'reg', label: 'Registration', sortKey: 'reg', num: true, render: (r) => r.reg || null },
        { key: 'type', label: 'Type', sortKey: 'type', render: (r) => (r.type ? h('span', { tip: r.typeDesc || null }, h('span.num', r.type), r.typeDesc ? h('span.muted.ac-typedesc', ' ' + r.typeDesc) : null) : null) },
        { key: 'operator', label: 'Operator', sortKey: 'operator', cls: 'ac-wide', render: (r) => (r.operator ? h('span.ellipsis', r.operator) : null) },
        { key: 'country', label: 'Country', sortKey: 'country', render: (r) => flagCell(r.country) },
        { key: 'callsign', label: 'Last callsign', sortKey: 'callsign', num: true, render: (r) => r.callsign || null },
        { key: 'first', label: 'First seen', sortKey: 'first_seen', sortDir: 'desc', num: true, render: (r) => when(r.firstSeen) },
        { key: 'last', label: 'Last seen', sortKey: 'last_seen', sortDir: 'desc', num: true, render: (r) => ago(r.lastSeen) },
        { key: 'flights', label: 'Flights', sortKey: 'flights', sortDir: 'desc', align: 'right', num: true, render: (r) => units.int(r.flights) },
        { key: 'msgs', label: 'Messages', sortKey: 'msgs', sortDir: 'desc', align: 'right', num: true, render: (r) => units.int(r.msgs) },
        { key: 'dist', label: 'Max dist', sortKey: 'max_dist_km', sortDir: 'desc', align: 'right', num: true, render: (r) => units.dist(r.maxDistKm) },
        { key: 'alt', label: 'Max alt', sortKey: 'max_alt', sortDir: 'desc', align: 'right', num: true, render: (r) => units.alt(r.maxAlt) },
        { key: 'flags', label: '', render: (r) => flagBadges(r.flags, r.military) || '' },
      ],
    });

    const afInfo = h('div.muted.ac-pager-info');
    const afNote = h('div.ac-note.hidden');
    const afPrev = ui.button({ icon: 'chevron-left', size: 'sm', variant: 'ghost', tip: 'Previous page', onClick: () => { if (S.page > 0) { S.page--; loadAirframes(); } } });
    const afNext = ui.button({ icon: 'chevron-right', size: 'sm', variant: 'ghost', tip: 'Next page', onClick: () => { S.page++; loadAirframes(); } });
    const afSize = ui.select({ size: 'sm', options: [25, 50, 100, 250].map((n) => ({ value: String(n), label: n + ' / page' })), value: String(S.size), onChange: (v) => { S.size = Number(v); prefs.set({ size: S.size }); S.page = 0; loadAirframes(); } });
    const reset = () => { S.page = 0; loadAirframes(); };
    const afSearch = ui.input({ icon: 'search', placeholder: 'Search ICAO, registration, callsign, type, operator', clearable: true, class: 'ac-search', onInput: ui.debounce((v) => { S.q = String(v || '').trim(); reset(); }, 300) });
    const afSeen = ui.segmented({ options: [{ value: 'all', label: 'All time' }, { value: 'today', label: 'Today' }, { value: 'week', label: '7 days' }], value: 'all', onChange: (v) => { S.seen = v; reset(); } });
    const afMil = ui.toggle({ label: 'Military', checked: false, onChange: (v) => { S.mil = v; reset(); } });
    const afReg = ui.toggle({ label: 'With registration', checked: false, onChange: (v) => { S.reg = v; reset(); } });
    const afCountry = ui.select({ size: 'sm', class: 'ac-select', options: [{ value: '', label: 'All countries' }], onChange: (v) => { S.country = v; reset(); } });
    const afType = ui.select({ size: 'sm', class: 'ac-select', options: [{ value: '', label: 'All types' }], onChange: (v) => { S.type = v; reset(); } });
    const afClear = ui.button({ label: 'Clear filters', icon: 'x', size: 'sm', variant: 'ghost', onClick: () => {
      Object.assign(S, { q: '', seen: 'all', mil: false, reg: false, country: '', type: '', page: 0 });
      afSearch.value = ''; afSeen.value = 'all'; afMil.checked = false; afReg.checked = false; afCountry.value = ''; afType.value = '';
      loadAirframes();
    } });
    const pageSub = h('div.sub', 'Every airframe this station has ever received');

    const filtersActive = () => !!(S.q || S.seen !== 'all' || S.mil || S.reg || S.country || S.type);
    const clientFiltered = () => S.mil || S.reg || !!S.country || !!S.type;

    async function fetchAirframes() {
      let since;
      if (S.seen === 'today') { const d = new Date(now()); d.setHours(0, 0, 0, 0); since = d.getTime(); }
      else if (S.seen === 'week') since = Math.round(now() - 7 * 86400e3);
      const base = { q: S.q, sort: S.sort, desc: S.desc ? 1 : 0, since };
      if (!clientFiltered()) {
        const r = await api.get('/api/history/aircraft', { params: { ...base, limit: S.size, offset: S.page * S.size }, signal: ctx.signal });
        return { rows: r.rows || [], total: r.total || 0, scanned: null, truncated: false };
      }
      // The API has no military / registration / country / type filter: page through the matches (already
      // sorted by the server) and filter here. Kept for 30 s so paging through the result is instant.
      const key = JSON.stringify({ ...base, since: since ? Math.round(since / 60000) : 0 });
      if (!afCache || afCache.key !== key || Date.now() - afCache.t > 30000) {
        const all = [];
        let total = Infinity;
        let pages = 0;
        while (all.length < total && pages < FULL_MAX) {
          const r = await api.get('/api/history/aircraft', { params: { ...base, limit: FULL_PAGE, offset: pages * FULL_PAGE }, signal: ctx.signal });
          total = r.total || 0;
          all.push(...(r.rows || []));
          pages++;
          if (!r.rows || r.rows.length < FULL_PAGE) break;
        }
        afCache = { key, t: Date.now(), rows: all, truncated: all.length < total };
      }
      const rows = afCache.rows.filter((r) => (!S.mil || isMil(r)) && (!S.reg || !!r.reg) && (!S.country || r.country === S.country) && (!S.type || r.type === S.type));
      return { rows: rows.slice(S.page * S.size, (S.page + 1) * S.size), total: rows.length, scanned: afCache.rows.length, truncated: afCache.truncated };
    }

    function paintAirframes() {
      const r = afLast;
      if (!r) return;
      afClear.classList.toggle('hidden', !filtersActive());
      if (!r.rows.length) {
        afTable.setState(filtersActive()
          ? ui.empty({ icon: 'filter', title: 'No airframe matches', text: 'Nothing in the database fits this search and these filters.', action: ui.button({ label: 'Clear filters', size: 'sm', onClick: () => afClear.click() }) })
          : ui.empty({ icon: 'plane', title: 'No aircraft recorded yet', text: 'Airframes appear here a few seconds after a feed delivers its first Mode S frames. Check the feed connection in Settings if this stays empty.', action: ui.button({ label: 'Open settings', size: 'sm', href: '#/settings' }) }));
      } else {
        afTable.setRows(r.rows);
        paintLiveDots();
      }
      const from = r.total ? S.page * S.size + 1 : 0;
      const to = Math.min(r.total, (S.page + 1) * S.size);
      ui.setText(afInfo, `${units.int(from)}–${units.int(to)} of ${units.int(r.total)}`);
      afPrev.disabled = S.page <= 0;
      afNext.disabled = to >= r.total;
      afNote.classList.toggle('hidden', !r.truncated);
      if (r.truncated) ui.setText(afNote, `Filtered within the first ${units.int(r.scanned)} matches only - narrow the search to cover everything.`);
      if (!filtersActive()) ui.setText(pageSub, r.total ? `${units.int(r.total)} airframes this station has ever received` : 'Every airframe this station has ever received');
    }

    async function loadAirframes(o = {}) {
      const my = ++afSeq;
      if (!afLast) afTable.setState(ui.skeleton({ lines: 6 })); else if (!o.quiet) afTable.setBusy(true);
      try {
        let r = await fetchAirframes();
        if (my !== afSeq || !alive) return;
        if (!r.rows.length && r.total > 0 && S.page > 0) { S.page = Math.max(0, Math.ceil(r.total / S.size) - 1); r = await fetchAirframes(); if (my !== afSeq || !alive) return; }
        afLast = r;
        paintAirframes();
      } catch (e) {
        if (my !== afSeq || !alive || (e && e.aborted)) return;
        if (o.quiet && afLast) return; // keep showing the last good page on a failed background refresh
        afLast = null;
        afTable.setState(ui.error(e, { title: 'Could not load the aircraft list', retry: () => loadAirframes(), compact: true }));
        ui.setText(afInfo, '');
      }
    }

    function paintLiveDots() {
      for (const tr of afTable.rowsEl.children) if (tr.dataset.key) tr.classList.toggle('is-live', live.aircraft.has(tr.dataset.key));
    }

    async function loadFilterOptions() {
      try {
        const top = await api.get('/api/stats/top', { params: { days: 3650, limit: 200 }, signal: ctx.signal });
        const names = (list) => (list || []).map((x) => x.key).filter(Boolean).sort((a, b) => a.localeCompare(b));
        afCountry.setOptions([{ value: '', label: 'All countries' }, ...names(top.countries).map((c) => ({ value: c, label: c }))]);
        afCountry.value = S.country;
        const types = (top.types || []).filter((x) => x.key).sort((a, b) => a.key.localeCompare(b.key));
        afType.setOptions([{ value: '', label: types.length ? 'All types' : 'All types (needs aircraft database)' }, ...types.map((t) => ({ value: t.key, label: t.label && t.label !== t.key ? `${t.key} - ${t.label}` : t.key }))]);
        afType.value = S.type;
      } catch { /* the selects simply stay at "All" */ }
    }

    const airframesPanel = h('div.col.gap-3',
      h('div.ac-toolbar', afSearch, afSeen, afCountry, afType, afMil, afReg, afClear),
      afNote,
      afTable,
      h('div.ac-pager', afInfo, h('div.spacer'), afSize, afPrev, afNext));

    // ------------------------------------------------------------------------------------------
    // tab 2: recent flights
    // ------------------------------------------------------------------------------------------
    const F = { span: 'all', icao: '', page: 0, size: 100 };
    let flSeq = 0;
    let flLast = null;
    let flStarted = false;
    const flightColumns = (withAircraft) => [
      { key: 'start', label: 'Start', num: true, render: (f) => when(f.start) },
      { key: 'dur', label: 'Duration', num: true, align: 'right', render: (f) => units.duration(f.end - f.start), tip: 'A flight is one contact session; a gap of more than 30 min starts a new one' },
      ...(withAircraft ? [
        { key: 'icao', label: 'ICAO', render: (f) => icaoCell(f.icao) },
        { key: 'reg', label: 'Reg', num: true, render: (f) => f.reg || null },
        { key: 'type', label: 'Type', num: true, render: (f) => f.type || null },
      ] : []),
      { key: 'callsign', label: 'Callsign', num: true, render: (f) => (f.callsign ? h('span', f.callsign, f.emergency ? h('span', ' ', ui.badge(f.emergency, 'solid-red')) : null) : (f.emergency ? ui.badge(f.emergency, 'solid-red') : null)) },
      { key: 'squawk', label: 'Squawk', num: true, render: (f) => f.squawk || null },
      ...(withAircraft ? [{ key: 'operator', label: 'Operator', cls: 'ac-wide', render: (f) => (f.operator ? h('span.ellipsis', f.operator) : null) }] : []),
      { key: 'dist', label: 'Max dist', num: true, align: 'right', render: (f) => (typeof f.maxDistKm === 'number' ? units.dist(f.maxDistKm) + (typeof f.maxDistBrg === 'number' ? ' ' + units.bearing(f.maxDistBrg) : '') : DASH), tip: 'Farthest position of the flight and its bearing from the receiver' },
      { key: 'alt', label: 'Altitude', num: true, align: 'right', render: (f) => range(f.minAlt, f.maxAlt, (v, o) => units.alt(v, o)) },
      { key: 'rssi', label: 'RSSI', num: true, align: 'right', render: (f) => range(f.rssiMin, f.rssiMax, (v, o) => units.dbfs(v, o)), tip: 'Weakest to strongest signal level of the flight' },
      { key: 'msgs', label: 'Msgs', num: true, align: 'right', render: (f) => units.int(f.msgs) },
      { key: 'src', label: 'Sources', render: (f) => srcBadges(f.srcMask) },
    ];
    const flTable = makeTable(ui, { columns: flightColumns(true), rowKey: (f) => f.icao, onRowClick: (f) => openAircraft(f.icao, f.id) });
    const flInfo = h('div.muted.ac-pager-info');
    const flPrev = ui.button({ icon: 'chevron-left', size: 'sm', variant: 'ghost', tip: 'Newer', onClick: () => { if (F.page > 0) { F.page--; loadFlights(); } } });
    const flNext = ui.button({ icon: 'chevron-right', size: 'sm', variant: 'ghost', tip: 'Older', onClick: () => { F.page++; loadFlights(); } });
    const flSpan = ui.segmented({ options: [{ value: 'all', label: 'All' }, { value: '24', label: '24 h' }, { value: '168', label: '7 days' }], value: 'all', onChange: (v) => { F.span = v; F.page = 0; loadFlights(); } });
    const flIcao = ui.input({ icon: 'search', placeholder: 'ICAO address', mono: true, clearable: true, size: 'sm', class: 'ac-search-sm', maxlength: 7, onInput: ui.debounce((v) => {
      const s = String(v || '').trim().toUpperCase();
      if (s && !HEX.test(s)) return;
      F.icao = s; F.page = 0; loadFlights();
    }, 300) });

    function paintFlights() {
      if (!flLast) return;
      const rows = flLast;
      if (!rows.length) {
        flTable.setState(F.page > 0
          ? ui.empty({ icon: 'route', title: 'No older flights', compact: true })
          : ui.empty({ icon: 'route', title: F.span === 'all' && !F.icao ? 'No flights recorded yet' : 'No flight matches', text: F.span === 'all' && !F.icao ? 'A flight (one contact session with an aircraft) is stored as soon as the first aircraft is received.' : 'Try a longer time span or another address.' }));
      } else {
        flTable.setRows(rows);
        for (const tr of flTable.rowsEl.children) if (tr.dataset.key) tr.classList.toggle('is-live', live.aircraft.has(tr.dataset.key));
      }
      ui.setText(flInfo, rows.length ? `Flights ${units.int(F.page * F.size + 1)}–${units.int(F.page * F.size + rows.length)}, newest first` : '');
      flPrev.disabled = F.page <= 0;
      flNext.disabled = rows.length < F.size;
    }
    async function loadFlights(o = {}) {
      const my = ++flSeq;
      if (!flLast) flTable.setState(ui.skeleton({ lines: 6 })); else if (!o.quiet) flTable.setBusy(true);
      try {
        const r = await api.get('/api/history/flights', { params: { limit: F.size, offset: F.page * F.size, icao: F.icao, hours: F.span === 'all' ? undefined : F.span }, signal: ctx.signal });
        if (my !== flSeq || !alive) return;
        flLast = r.flights || [];
        paintFlights();
      } catch (e) {
        if (my !== flSeq || !alive || (e && e.aborted)) return;
        if (o.quiet && flLast) return;
        flLast = null;
        flTable.setState(ui.error(e, { title: 'Could not load the flights', retry: () => loadFlights(), compact: true }));
      }
    }
    const flightsPanel = h('div.col.gap-3',
      h('div.ac-toolbar', flSpan, flIcao, h('div.spacer'), h('span.muted.ac-hint', 'Click a flight to see its trail')),
      flTable,
      h('div.ac-pager', flInfo, h('div.spacer'), flPrev, flNext));

    // ------------------------------------------------------------------------------------------
    // tab 3: events + records
    // ------------------------------------------------------------------------------------------
    const E = { kind: '', rows: null, more: false };
    let evSeq = 0;
    let evStarted = false;
    let recLast = null;
    const recHost = h('div.grid-auto.ac-records', { style: { '--min': '210px' } });
    const evTable = makeTable(ui, {
      rowKey: (e) => e.id,
      onRowClick: (e) => { if (e.icao) openAircraft(e.icao, e.detail && e.detail.flightId); },
      rowClass: (e) => (e.icao ? '' : 'ac-noclick'),
      columns: [
        { key: 't', label: 'Time', num: true, render: (e) => when(e.t) },
        { key: 'kind', label: 'Kind', render: (e) => kindBadge(e.kind) },
        { key: 'icao', label: 'Aircraft', render: (e) => (e.icao ? icaoCell(e.icao) : null) },
        { key: 'text', label: 'What happened', cls: 'ac-text', render: (e) => eventText(e) },
        { key: 'where', label: 'Where', num: true, align: 'right', render: (e) => eventWhere(e) },
      ],
    });
    const evMore = ui.button({ label: 'Load older events', icon: 'chevron-down', size: 'sm', variant: 'ghost', onClick: () => loadEvents({ append: true }) });
    const evKind = ui.segmented({
      options: [{ value: '', label: 'All' }, { value: 'emergency', label: 'Emergencies' }, { value: 'military', label: 'Military' }, { value: 'interesting', label: 'Notable' }, { value: 'record', label: 'Records' }, { value: 'feed', label: 'Feeds' }],
      value: '', onChange: (v) => { E.kind = v; E.rows = null; loadEvents(); },
    });

    function paintEvents() {
      if (!E.rows) return;
      if (!E.rows.length) {
        evTable.setState(ui.empty({ icon: 'history', title: E.kind ? 'No such events yet' : 'No events yet', text: 'Emergency squawks, military and notable airframes, new station records and feed drop-outs are logged here as they happen.' }));
      } else {
        evTable.setRows(E.rows);
        for (const tr of evTable.rowsEl.children) { const ev = E.rows.find((x) => String(x.id) === tr.dataset.key); tr.classList.toggle('is-live', !!(ev && ev.icao && live.aircraft.has(ev.icao))); }
      }
      evMore.classList.toggle('hidden', !E.more);
    }
    async function fetchEvents(params) {
      if (E.kind !== 'feed') return (await api.get('/api/stats/events', { params: { ...params, kind: E.kind }, signal: ctx.signal })).events || [];
      const [up, down] = await Promise.all(['feed_up', 'feed_down'].map((kind) => api.get('/api/stats/events', { params: { ...params, kind }, signal: ctx.signal })));
      return [...(up.events || []), ...(down.events || [])].sort((a, b) => b.id - a.id).slice(0, params.limit);
    }
    async function loadEvents(o = {}) {
      const my = ++evSeq;
      const LIMIT = 100;
      if (!E.rows) evTable.setState(ui.skeleton({ lines: 5 })); else if (!o.quiet) evTable.setBusy(true);
      if (o.append) evMore.setLoading(true);
      try {
        const before = o.append && E.rows && E.rows.length ? E.rows[E.rows.length - 1].id : undefined;
        // a quiet refresh re-reads as many rows as are shown so "load older" results do not vanish
        const limit = o.quiet && E.rows ? Math.min(1000, Math.max(LIMIT, E.rows.length)) : LIMIT;
        const rows = await fetchEvents({ limit, before });
        if (my !== evSeq || !alive) return;
        E.rows = o.append ? [...E.rows, ...rows] : rows;
        E.more = rows.length >= limit;
        paintEvents();
      } catch (e) {
        if (my !== evSeq || !alive || (e && e.aborted)) return;
        if ((o.quiet || o.append) && E.rows) { if (o.append) ui.toast.error(e); evTable.setBusy(false); return; }
        E.rows = null;
        evTable.setState(ui.error(e, { title: 'Could not load the event log', retry: () => loadEvents(), compact: true }));
      } finally {
        evMore.setLoading(false);
      }
    }
    function paintRecords() {
      if (!recLast) return;
      if (!recLast.length) {
        ui.mount(recHost, h('div.muted.ac-hint', 'No station records yet - they are set by the first aircraft with a position.'));
        return;
      }
      const order = Object.keys(RECORDS);
      const list = [...recLast].sort((a, b) => (order.indexOf(a.key) + 99) % 99 - (order.indexOf(b.key) + 99) % 99);
      ui.mount(recHost, list.map((r) => {
        const def = RECORDS[r.key] || [r.key, 'target', (v) => units.num(v, 1)];
        const d = r.detail || {};
        const who = [d.callsign, d.reg, d.type].filter(Boolean).join(' · ') || r.icao || '';
        const tile = ui.stat({ label: def[0], icon: def[1], value: def[2](r.value), sub: [who, units.ago(r.t, now())].filter(Boolean).join(' · '), tip: r.icao ? 'Open ' + r.icao : units.dateTime(r.t) });
        if (r.icao) { tile.classList.add('ac-click'); tile.addEventListener('click', () => openAircraft(r.icao)); }
        return tile;
      }));
    }
    async function loadRecords() {
      try {
        const r = await api.get('/api/stats/records', { signal: ctx.signal });
        if (!alive) return;
        recLast = r.records || [];
        paintRecords();
      } catch (e) {
        if (!alive || (e && e.aborted) || recLast) return;
        ui.mount(recHost, ui.error(e, { title: 'Could not load the records', retry: loadRecords, compact: true }));
      }
    }
    const eventsPanel = h('div.col.gap-3',
      ui.section('All-time station records', 'target'),
      recHost,
      ui.section('Event log', 'history'),
      h('div.ac-toolbar', evKind),
      evTable,
      h('div.ac-pager', h('div.spacer'), evMore));

    // ------------------------------------------------------------------------------------------
    // tab 4: aircraft database (reference data import)
    // ------------------------------------------------------------------------------------------
    let dbStarted = false;
    let dbDoc = null;
    let dbPollTimer = 0;
    const dbInstalled = h('div');
    const dbProgress = h('div.ac-progress.hidden');
    const dbSources = h('div.grid.cols-2');
    const fileInput = h('input', { type: 'file', accept: '.csv,.gz,.csv.gz,text/csv,application/gzip', style: 'display:none' });
    const uploadBtn = ui.button({ label: 'Import a file…', icon: 'upload', size: 'sm', variant: 'ghost', tip: 'tar1090-db or OpenSky CSV, optionally gzip-compressed (at most 600 MB)', onClick: () => fileInput.click() });
    const lookupOut = h('div.ac-lookup-out');
    const lookupIn = ui.input({ placeholder: 'e.g. 3C6444', mono: true, size: 'sm', maxlength: 6, icon: 'search', class: 'ac-search-sm', onEnter: (v) => doLookup(v) });
    const importBtns = new Map();

    function stageText(st) {
      const p = st.progress || {};
      switch (st.stage) {
        case 'download': return p.stage === 'connect' ? 'Connecting…' : 'Downloading and parsing';
        case 'store': return 'Writing to the database';
        case 'done': return 'Import finished';
        case 'error': return 'Import failed';
        case 'cancelled': return 'Import cancelled';
        default: return '';
      }
    }
    function paintProgress(st) {
      const running = !!st.running;
      const show = running || st.stage === 'error' || st.stage === 'cancelled';
      dbProgress.classList.toggle('hidden', !show);
      for (const b of importBtns.values()) b.disabled = running;
      uploadBtn.disabled = running;
      if (!show) return;
      const p = st.progress || {};
      const src = (dbDoc && dbDoc.sources.find((s) => s.id === st.source)) || null;
      let pct = null;
      if (st.stage === 'store' && p.records > 0) pct = (st.stored / p.records) * 100;
      else if (p.bytesTotal > 0) pct = (p.bytesRead / p.bytesTotal) * 100;
      else if (src && src.approxBytes > 0 && p.bytesRead > 0) pct = Math.min(99, (p.bytesRead / src.approxBytes) * 100);
      const meter = ui.meter(pct ?? 0);
      meter.classList.toggle('ac-indeterminate', running && pct === null);
      const bits = [];
      if (p.bytesRead) bits.push(units.bytes(p.bytesRead) + (p.bytesTotal > 0 ? ' of ' + units.bytes(p.bytesTotal) : ''));
      if (p.records) bits.push(units.int(p.records) + ' records parsed');
      if (st.stored) bits.push(units.int(st.stored) + ' stored');
      if (p.skipped) bits.push(units.int(p.skipped) + ' malformed lines skipped');
      ui.mount(dbProgress,
        h('div.row', h('strong', stageText(st)), st.source ? h('span.muted', (src ? src.name : st.source)) : null, h('div.spacer'),
          pct !== null && running ? h('span.num', units.pct(pct)) : null,
          running ? ui.button({ label: 'Cancel', icon: 'x', size: 'sm', variant: 'ghost', onClick: cancelImport }) : null),
        running ? meter : null,
        h('div.muted.num.ac-progress-detail', bits.join(' · ')),
        st.error ? h('div.ac-error-text', st.error) : null);
    }
    function paintInstalled() {
      const inst = dbDoc.installed || {};
      const info = inst.info;
      if (!inst.rows && !info) {
        ui.mount(dbInstalled, ui.empty({ icon: 'database', compact: true, title: 'No aircraft database installed', text: 'Without one, registration, aircraft type and operator are known only for airframes whose registration can be derived from the address. Import one of the sources below (admin only) - it takes about a minute.' }));
        return;
      }
      ui.mount(dbInstalled, ui.kv([
        ['Records', units.int(inst.rows)],
        ['Source', info ? info.source : 'unknown', { text: true }],
        ['Imported', info ? units.dateTime(info.t) + ' (' + units.ago(info.t, now()) + ')' : DASH],
        info && info.lastModified ? ['Source file dated', units.dateTime(info.lastModified)] : null,
        info && info.licence ? ['Licence', info.licence, { text: true }] : null,
        info && info.attribution ? ['Attribution', info.attribution, { text: true, class: 'ac-wrap' }] : null,
      ]));
    }
    function paintSources() {
      importBtns.clear();
      const installedSrc = dbDoc.installed && dbDoc.installed.info ? dbDoc.installed.info.source : '';
      ui.mount(dbSources, dbDoc.sources.map((s) => {
        const btn = ui.button({ label: installedSrc === s.id ? 'Update' : 'Import', icon: 'download', size: 'sm', variant: 'primary', onClick: () => startImport(s) });
        importBtns.set(s.id, btn);
        return ui.card({
          title: s.name, icon: 'database',
          sub: `about ${units.int(s.approxRecords)} airframes · ${units.bytes(s.approxBytes)} download`,
          actions: [installedSrc === s.id ? ui.badge('installed', 'green') : null, btn],
          body: h('div.col.gap-3.ac-source',
            h('p', s.notes),
            ui.kv([
              ['Licence', s.licenceUrl ? h('a', { href: s.licenceUrl, target: '_blank', rel: 'noopener noreferrer' }, s.licence) : s.licence, { text: true, class: 'ac-wrap' }],
              ['Attribution', s.attribution, { text: true, class: 'ac-wrap' }],
              ['Project', h('a', { href: s.homepage, target: '_blank', rel: 'noopener noreferrer' }, s.homepage.replace(/^https?:\/\//, '')), { text: true, class: 'ac-wrap' }],
            ])),
        });
      }));
    }
    function pollProgress() {
      clearTimeout(dbPollTimer);
      if (!alive) return;
      dbPollTimer = setTimeout(async () => {
        try {
          const st = await api.get('/api/db/acdb/progress', { signal: ctx.signal });
          if (!alive) return;
          paintProgress(st);
          if (st.running) pollProgress();
          else { await loadDb(); if (st.stage === 'done') { ui.toast.success(`Aircraft database imported: ${units.int(st.stored)} records`); afCache = null; loadFilterOptions(); loadAirframes({ quiet: true }); } }
        } catch (e) {
          if (alive && !(e && e.aborted)) pollProgress();
        }
      }, 1000);
    }
    ctx.onCleanup(() => clearTimeout(dbPollTimer));

    async function loadDb() {
      try {
        const d = await api.get('/api/db/acdb/sources', { signal: ctx.signal });
        if (!alive) return;
        dbDoc = d;
        paintInstalled();
        paintSources();
        paintProgress(d.status || {});
        if (d.status && d.status.running) pollProgress();
      } catch (e) {
        if (!alive || (e && e.aborted) || dbDoc) return;
        ui.mount(dbInstalled, ui.error(e, { title: 'Could not load the aircraft database status', retry: loadDb, compact: true }));
      }
    }
    const adminFail = (e, title) => { if (e && e.cancelled) return; if (!(e && e.aborted)) ui.toast.error(e, { title }); };
    async function startImport(s) {
      const host = (() => { try { return new URL(s.url).hostname; } catch { return 'the internet'; } })();
      const ok = await ui.confirm({
        title: 'Import aircraft database', confirmLabel: 'Download and import',
        message: h('div.col.gap-3', h('p', `Download "${s.name}" (about ${units.bytes(s.approxBytes)}) from ${host} and replace the installed aircraft database?`), h('p.muted', s.attribution)),
      });
      if (!ok) return;
      const btn = importBtns.get(s.id);
      if (btn) btn.setLoading(true);
      try {
        const st = await admin.run(() => api.post('/api/db/acdb/import', { source: s.id }), 'Importing an aircraft database needs the admin password.');
        paintProgress(st || { running: true, stage: 'download', source: s.id });
        pollProgress();
      } catch (e) {
        if (e && e.status === 409) { ui.toast.warn('An import is already running.'); pollProgress(); } else adminFail(e, 'Import not started');
      } finally {
        if (btn) btn.setLoading(false);
      }
    }
    async function cancelImport() {
      try { paintProgress(await admin.run(() => api.post('/api/db/acdb/cancel'), 'Cancelling the import needs the admin password.')); pollProgress(); } catch (e) { adminFail(e, 'Could not cancel'); }
    }
    fileInput.addEventListener('change', async () => {
      const file = fileInput.files && fileInput.files[0];
      fileInput.value = '';
      if (!file) return;
      if (file.size > 600 * 1024 * 1024) { ui.toast.error('The file is larger than the 600 MB limit.'); return; }
      uploadBtn.setLoading(true);
      clearTimeout(dbPollTimer);
      paintProgress({ running: true, stage: 'download', source: file.name, stored: 0, progress: { stage: 'connect' } });
      try {
        // the request blocks until the import has finished
        const st = await admin.run(() => api.post('/api/db/acdb/upload', file, { params: { name: file.name }, timeout: 0, headers: { 'Content-Type': 'application/octet-stream' } }), 'Importing an aircraft database needs the admin password.');
        if (!alive) return;
        if (st && st.stage === 'done') {
          ui.toast.success(`Aircraft database imported: ${units.int(st.stored)} records`, { title: file.name });
          afCache = null; loadFilterOptions(); loadAirframes({ quiet: true });
        } else if (st && st.error) ui.toast.error(st.error, { title: file.name });
      } catch (e) {
        adminFail(e, 'Upload failed');
      } finally {
        uploadBtn.setLoading(false);
        if (alive) loadDb();
      }
    });
    async function doLookup(v) {
      const icao = String(v || '').trim().toUpperCase();
      if (!/^[0-9A-F]{6}$/.test(icao)) { ui.mount(lookupOut, h('div.ac-error-text', 'Enter the six hex digits of an ICAO address.')); return; }
      ui.mount(lookupOut, ui.loading('Looking up ' + icao, { compact: true }));
      try {
        const r = await api.get('/api/db/acdb/lookup', { params: { icao }, signal: ctx.signal });
        const i = r.info || {};
        const rec = r.record || {};
        ui.mount(lookupOut, ui.kv([
          ['In aircraft database', r.found ? ui.badge('yes', 'green') : ui.badge('no', 'other')],
          ['Registration', rec.reg || i.reg || DASH],
          ['Type', [rec.type || i.type, rec.typeDesc || i.typeDesc].filter(Boolean).join(' - ') || DASH, { text: true }],
          ['Operator', rec.operator || i.operator || DASH, { text: true }],
          ['Country', i.country ? flagCell(i.country, i.countryCode) : DASH, { text: true }],
          ['Year built', rec.year || i.year ? String(rec.year || i.year) : DASH],
          ['Flags', flagBadges(rec.flags, rec.military || i.military) || DASH, { text: true }],
        ]), h('div.row.mt-2', ui.button({ label: 'Open in browser', icon: 'arrow-right', size: 'sm', variant: 'ghost', onClick: () => openAircraft(icao) })));
      } catch (e) {
        if (!(e && e.aborted)) ui.mount(lookupOut, ui.error(e, { compact: true, title: 'Lookup failed' }));
      }
    }
    const databasePanel = h('div.col.gap-4',
      h('div.grid.cols-2',
        ui.card({ title: 'Installed aircraft database', icon: 'database', sub: 'Maps the 24-bit address to registration, type and operator', actions: [uploadBtn, fileInput], body: h('div.col.gap-3', dbInstalled, dbProgress) }),
        ui.card({ title: 'Look up an address', icon: 'search', sub: 'What the database and the built-in tables know about one ICAO address', body: h('div.col.gap-3', h('div.row', lookupIn, ui.button({ label: 'Look up', size: 'sm', onClick: () => doLookup(lookupIn.value) })), lookupOut) })),
      ui.section('Sources', 'download'),
      dbSources,
      h('p.muted.ac-hint', 'Imports run on the AirView machine, in the background; received traffic is not interrupted. Importing and cancelling are admin actions: from another computer you are asked for the admin password when one is set.'));

    // ------------------------------------------------------------------------------------------
    // detail drawer
    // ------------------------------------------------------------------------------------------
    let drawer = null; // { icao, close(), setFlight(id), rerender(), onLive() }

    function openAircraft(icao, flightId) {
      icao = String(icao).toUpperCase();
      const path = 'aircraft/' + icao + (flightId ? '?flight=' + flightId : '');
      if (drawer && drawer.icao === icao) { if (flightId) drawer.setFlight(flightId); return; }
      ctx.navigate(path);
    }

    function showDrawer(icao, flightId) {
      if (drawer) { if (drawer.icao === icao) { if (flightId) drawer.setFlight(flightId); return; } drawer.close(true); }
      const body = h('div.ac-drawer');
      const myCharts = [];
      let map = null;
      let mapP = null;
      let doc = null;
      let closed = false;
      let selFlight = flightId ? Number(flightId) : null;
      let flightSeq = 0;
      let flightDoc = null;
      let colorMode = 'altitude';
      let liveKv = null;
      const liveBtn = ui.button({ label: 'Show live', icon: 'follow', size: 'sm', variant: 'primary', class: 'hidden', tip: 'Open the Live Map on this aircraft', onClick: () => { self.close(true); ctx.navigate('map/' + icao); } });
      const liveStrip = h('div.ac-live.hidden');

      const dr = ui.drawer({
        title: 'Aircraft ' + icao, icon: 'plane', width: 880, body,
        onClose: () => {
          closed = true;
          for (const c of myCharts) c.dispose();
          if (map) map.remove();
          if (drawer === self) drawer = null;
          if (!self.silent && alive && curRoute.args[0] && HEX.test(curRoute.args[0])) ctx.navigate('aircraft' + (tabs.active !== 'airframes' ? '/' + tabs.active : ''));
        },
      });
      dr.el.classList.add('ac-drawer-el');
      const self = {
        icao, silent: false,
        close(silent) { self.silent = !!silent; dr.close(); },
        setFlight(id) { selFlight = Number(id); if (doc) selectFlight(selFlight, true); },
        rerender() { if (doc) render(); },
        refresh() { if (flightDoc && flightDoc.live && selFlight === flightDoc.flight.id) selectFlight(selFlight, false); }, // a flight in progress keeps growing
        onLive() { paintLive(); },
      };
      drawer = self;

      function paintLive() {
        const a = live.get(icao);
        liveBtn.classList.toggle('hidden', !a);
        liveStrip.classList.toggle('hidden', !a);
        if (!a) return;
        if (!liveKv) {
          liveKv = ui.kv([['Callsign', ''], ['Altitude', ''], ['Speed', ''], ['Distance', ''], ['Signal', ''], ['Source', '']], { columns: 2 });
          ui.mount(liveStrip, h('div.row', h('i.ac-live-dot.on'), h('strong', 'Tracked right now')), liveKv);
        }
        liveKv.set(0, a.callsign || DASH);
        liveKv.set(1, a.onGround ? 'ground' : units.alt(a.altBaro ?? a.altGeom, { fl: true }));
        liveKv.set(2, units.speed(a.gs));
        liveKv.set(3, typeof a.distKm === 'number' ? units.dist(a.distKm) + ' ' + units.cardinal(a.bearing) : DASH);
        liveKv.set(4, units.dbfs(a.rssiAvg ?? a.rssi));
        liveKv.set(5, ui.sourceLabel(a.source));
      }

      // ---- flight panel (map + profiles) ----
      const flightHead = h('div.ac-flight-head');
      const mapHost = h('div.ac-map');
      const mapWrap = h('div.ac-map-wrap', mapHost);
      const mapLegend = h('div.ac-map-legend');
      const altChartEl = h('div.chart.chart--sm');
      const sigChartEl = h('div.chart.chart--sm');
      const profiles = h('div.grid.cols-2.ac-profiles',
        h('div', h('div.label', 'Altitude and ground speed'), altChartEl, h('div.ac-cap', 'Height and speed along the flight. Gaps are stretches without reception; a smooth, unbroken line means the aircraft was followed all the way.')),
        h('div', h('div.label', 'Signal level and distance'), sigChartEl, h('div.ac-cap', 'RSSI of the stored positions (0 dBFS = receiver full scale) against distance. Good: the signal falls gently with distance and stays above about −30 dBFS.')));
      const flightBody = h('div.ac-flight-body');
      const flightPanel = h('div.ac-flight.hidden', flightHead, flightBody);
      const modeSeg = ui.segmented({ options: [{ value: 'altitude', label: 'Altitude' }, { value: 'signal', label: 'Signal' }], value: colorMode, onChange: (v) => { colorMode = v; paintLegend(); pushMap(); } });
      let altChart = null;
      let sigChart = null;

      function paintLegend() {
        const stops = colorMode === 'signal' ? [-40, -30, -20, -10, 0] : [0, 5000, 10000, 20000, 30000, 40000];
        const col = colorMode === 'signal' ? signalColor : altColor;
        const lo = stops[0]; const hi = stops[stops.length - 1];
        const grad = [];
        for (let i = 0; i <= 20; i++) grad.push(col(lo + ((hi - lo) * i) / 20));
        ui.mount(mapLegend,
          h('span.muted', 'Trail colour'), modeSeg,
          h('span.ac-ramp', { style: { background: `linear-gradient(90deg, ${grad.join(',')})` } }),
          h('span.num.muted', colorMode === 'signal' ? `${units.dbfs(lo, { unit: false, digits: 0 })} … ${units.dbfs(hi, { digits: 0 })}` : `${units.alt(lo, { unit: false })} … ${units.alt(hi)}`),
          h('span.ac-legend-marks', h('i.m-start'), 'first', h('i.m-end'), 'last', h('i.m-far'), 'farthest', h('i.m-rx'), 'receiver'));
      }
      function describePoint(p) {
        return [units.clock(p[0]), units.alt(p[3], { fl: true }), units.speed(p[4]), units.dbfs(p[5])].filter((s) => s !== DASH).join('  ·  ');
      }
      function farPoint(trail) {
        if (!receiver) return null;
        let best = null; let bestD = -1;
        for (const p of trail) { const d = ctx.geo.distanceKm(receiver.lat, receiver.lon, p[1], p[2]); if (d > bestD) { bestD = d; best = p; } }
        return best ? { lat: best[1], lon: best[2] } : null;
      }
      function pushMap() {
        if (!map || !flightDoc) return;
        const trail = flightDoc.trail || [];
        map.set({ key: flightDoc.flight.id, trail, mode: colorMode, receiver, far: farPoint(trail) });
      }
      function paintFlight() {
        if (!flightDoc || closed) return;
        const f = flightDoc.flight;
        const trail = (flightDoc.trail || []).filter((p) => typeof p[1] === 'number' && typeof p[2] === 'number');
        ui.mount(flightHead,
          h('div.row-wrap',
            h('strong.num', f.callsign || 'No callsign'), flightDoc.live ? ui.badge('in progress', 'green') : null, f.emergency ? ui.badge(f.emergency, 'solid-red') : null,
            h('span.muted.num', `${units.dateTime(f.start)} – ${units.clock(f.end, { seconds: false })} · ${units.duration(f.end - f.start)}`)),
          ui.kv([
            ['Max distance', typeof f.maxDistKm === 'number' ? units.dist(f.maxDistKm) + (typeof f.maxDistBrg === 'number' ? ' at ' + units.bearing(f.maxDistBrg) : '') : DASH],
            ['Closest', units.dist(f.minDistKm)],
            ['Altitude', range(f.minAlt, f.maxAlt, (v, o) => units.alt(v, o))],
            ['Max ground speed', units.speed(f.maxGs)],
            ['RSSI min / avg / max', [f.rssiMin, f.rssiAvg, f.rssiMax].every((v) => typeof v === 'number') ? `${units.dbfs(f.rssiMin, { unit: false })} / ${units.dbfs(f.rssiAvg, { unit: false })} / ${units.dbfs(f.rssiMax)}` : DASH],
            ['Squawk', f.squawk || DASH],
            ['Messages / positions', `${units.int(f.msgs)} / ${units.int(f.positions)}`],
            ['Sources', srcBadges(f.srcMask), { text: true }],
          ], { columns: 2 }));
        if (!trail.length) {
          ui.mount(flightBody, ui.empty({ icon: 'route', compact: true, title: 'No stored trail for this flight', text: f.positions ? 'The trail of this flight is older than the trail retention period and has been pruned.' : 'The aircraft sent no position during this contact (Mode S only, or too far for a position fix).' }));
          return;
        }
        if (!mapWrap.isConnected || flightBody.firstChild !== mapWrap) ui.mount(flightBody, mapWrap, mapLegend, profiles);
        paintLegend();
        if (!mapP) {
          mapP = createTrailMap(mapHost, ctx, { describe: describePoint }).then((m) => { if (closed) { m.remove(); return null; } map = m; pushMap(); return m; });
          mapP.catch((e) => { console.error('[aircraft] map failed', e); if (!closed) ui.mount(mapHost, ui.error(e, { compact: true, title: 'The map could not be loaded' })); });
        } else if (map) { map.resize(); pushMap(); }

        const t0 = trail;
        const altData = t0.filter((p) => typeof p[3] === 'number').map((p) => [p[0], units.value('alt', p[3])]);
        const gsData = t0.filter((p) => typeof p[4] === 'number').map((p) => [p[0], units.value('speed', p[4])]);
        const rssiData = t0.filter((p) => typeof p[5] === 'number').map((p) => [p[0], p[5]]);
        const distData = receiver ? t0.map((p) => [p[0], units.value('dist', ctx.geo.distanceKm(receiver.lat, receiver.lon, p[1], p[2]))]) : [];
        const r1 = (v) => (typeof v === 'number' ? units.num(v, Math.abs(v) < 100 ? 1 : 0) : DASH);
        const altOpt = charts.line({
          series: [{ name: `Altitude (${units.label('alt')})`, data: altData, color: charts.colors.blue, area: 0.25 }, { name: `Ground speed (${units.label('speed')})`, data: gsData, color: charts.colors.amber, y2: true }],
          yName: units.label('alt'), y2Name: units.label('speed'), valueFormat: r1,
        });
        const sigOpt = charts.line({
          series: [{ name: 'RSSI (dBFS)', data: rssiData, color: charts.colors.cyan }, ...(distData.length ? [{ name: `Distance (${units.label('dist')})`, data: distData, color: charts.colors.violet || '#a78bfa', y2: true }] : [])],
          yName: 'dBFS', yScale: true, y2Name: distData.length ? units.label('dist') : undefined, valueFormat: r1,
        });
        if (!altChart) { altChart = charts.create(altChartEl); sigChart = charts.create(sigChartEl); myCharts.push(altChart, sigChart); }
        altChart.set(altOpt, { replace: true });
        sigChart.set(sigOpt, { replace: true });
      }
      async function selectFlight(id, scroll) {
        selFlight = id;
        flightsTable.setSelected(id);
        flightPanel.classList.remove('hidden');
        const my = ++flightSeq;
        if (!flightDoc || flightDoc.flight.id !== id) {
          ui.mount(flightHead, ui.loading('Loading flight…', { compact: true }));
          flightBody.replaceChildren();
        }
        if (scroll) flightPanel.scrollIntoView({ block: 'nearest', behavior: 'smooth' });
        try {
          const d = await api.get('/api/history/flights/' + encodeURIComponent(id), { signal: ctx.signal });
          if (my !== flightSeq || closed) return;
          flightDoc = d;
          paintFlight();
        } catch (e) {
          if (my !== flightSeq || closed || (e && e.aborted)) return;
          flightDoc = null;
          ui.mount(flightHead, ui.error(e, { compact: true, title: e.status === 404 ? 'This flight is no longer stored' : 'Could not load the flight', retry: () => selectFlight(id) }));
          flightBody.replaceChildren();
        }
      }
      const flightsTable = makeTable(ui, { class: 'ac-flights-table', columns: flightColumns(false), rowKey: (f) => f.id, onRowClick: (f) => selectFlight(f.id, true) });
      const eventsTable = makeTable(ui, {
        rowKey: (e) => e.id, rowClass: () => 'ac-noclick',
        columns: [
          { key: 't', label: 'Time', num: true, render: (e) => when(e.t) },
          { key: 'kind', label: 'Kind', render: (e) => kindBadge(e.kind) },
          { key: 'text', label: 'What happened', cls: 'ac-text', render: (e) => eventText(e) },
          { key: 'where', label: 'Where', num: true, align: 'right', render: (e) => eventWhere(e) },
        ],
      });
      let innerTabs = null;

      function render() {
        const info = doc.info || {};
        const seen = doc.seen;
        const acdb = doc.acdb;
        const reg = (acdb && acdb.reg) || (seen && seen.reg) || info.reg;
        const type = (acdb && acdb.type) || (seen && seen.type) || info.type;
        const typeDesc = (info.typeInfo && info.typeInfo.name) || (acdb && acdb.typeDesc) || info.typeDesc || (seen && seen.typeDesc);
        const operator = (acdb && acdb.operator) || info.operator || (seen && seen.operator);
        const flags = ((acdb && acdb.flags) || 0) | ((seen && seen.flags) || 0) | (info.interesting ? 2 : 0) | (info.pia ? 4 : 0) | (info.ladd ? 8 : 0);
        const mil = info.military || (acdb && acdb.military) || (seen && seen.military);
        const cs = info.callsign;
        const sq = info.squawk;
        const country = info.country || (seen && seen.country);

        const hero = h('div.ac-hero',
          h('div.ac-hero-main',
            h('div.ac-hero-icao.num', icao),
            h('div.ac-hero-sub', [reg, type, operator].filter(Boolean).join(' · ') || 'No reference data for this address')),
          h('div.ac-hero-side', flagBadges(flags, mil), country ? flagCell(country, info.countryCode) : null,
            h('div.row', liveBtn, ui.button({ icon: 'copy', size: 'sm', variant: 'ghost', tip: 'Copy the ICAO address', onClick: () => ui.copy(icao).then(() => ui.toast.success('Copied ' + icao)) }))));

        const refRows = [
          ['Registration', reg ? reg + (!acdb && info.reg ? ' (derived)' : '') : DASH, { tip: !acdb && info.reg ? 'Calculated from the address; an imported aircraft database takes precedence' : null }],
          ['Type', [type, typeDesc].filter(Boolean).join(' - ') || DASH, { text: true }],
          info.typeClass ? ['Class', info.typeClass, { text: true }] : null,
          info.wtc ? ['Wake category', info.wtc, { text: true }] : null,
          ['Operator', operator || DASH, { text: true }],
          (acdb && acdb.year) || info.year ? ['Year built', String((acdb && acdb.year) || info.year)] : null,
          ['Country', country ? flagCell(country, info.countryCode) : DASH, { text: true }],
          info.state && info.state !== info.country ? ['State of registry', info.state, { text: true }] : null,
          ['Address block', info.alloc === 'state' || info.alloc === 'territory' ? 'allocated to ' + (info.country || info.state || 'a state') : [info.alloc, info.region].filter(Boolean).join(' - ') || DASH, { text: true }],
          (seen && seen.category) || info.categoryDesc ? ['Emitter category', [seen && seen.category, info.categoryDesc].filter(Boolean).join(' - '), { text: true }] : null,
          cs ? ['Last callsign', [cs.callsign, cs.operator, info.spoken ? '"' + info.spoken + '"' : null].filter(Boolean).join(' · '), { text: true }] : null,
          sq ? ['Squawk meaning', `${sq.code}: ${sq.meaning} (${sq.region})`, { text: true }] : null,
          ['Aircraft database', acdb ? ui.badge('record found', 'green') : ui.badge('no record', 'other', { tip: 'Import an aircraft database on the Database tab to get registration, type and operator' }), { text: true }],
        ];
        const totalRows = seen ? [
          ['First seen', units.dateTime(seen.firstSeen)],
          ['Last seen', units.dateTime(seen.lastSeen) + ' (' + units.ago(seen.lastSeen, now()) + ')'],
          ['Flights', units.int(seen.flights)],
          ['Messages', units.int(seen.msgs)],
          ['Positions', units.int(seen.positions)],
          ['Max distance', units.dist(seen.maxDistKm)],
          ['Max altitude', units.alt(seen.maxAlt)],
          ['Sources', srcBadges(seen.srcMask), { text: true }],
        ] : null;

        const flights = doc.flights || [];
        const events = doc.events || [];
        if (flights.length) flightsTable.setRows(flights);
        else flightsTable.setState(ui.empty({ icon: 'route', compact: true, title: 'No flights stored', text: 'Flights are kept for the flight retention period set in Settings.' }));
        if (events.length) eventsTable.setRows(events);
        else eventsTable.setState(ui.empty({ icon: 'history', compact: true, title: 'No events for this airframe', text: 'Emergencies, records it set and military / notable sightings would be listed here.' }));
        const activeTab = innerTabs ? innerTabs.active : 'flights';
        innerTabs = ui.tabs({
          active: activeTab,
          tabs: [
            { id: 'flights', label: 'Flights', icon: 'route', badge: flights.length ? String(flights.length) : null, content: h('div.col.gap-3', flightsTable, h('div.muted.ac-hint', flights.length ? 'Click a flight to draw its stored trail and profiles.' : ''), flightPanel) },
            { id: 'events', label: 'Events', icon: 'history', badge: events.length ? String(events.length) : null, content: eventsTable },
          ],
        });

        ui.mount(body, hero, liveStrip,
          h('div.grid.cols-2.ac-detail-grid',
            h('div', ui.section('Reference data', 'info'), ui.kv(refRows)),
            h('div', ui.section('Seen by this station', 'radar'), totalRows ? ui.kv(totalRows) : ui.empty({ icon: 'database', compact: true, title: 'Not stored yet', text: 'This aircraft is being tracked but has not been written to the database yet (rows are flushed every few seconds).' }))),
          innerTabs);
        paintLive();
        if (flightDoc) { flightsTable.setSelected(flightDoc.flight.id); paintFlight(); }
      }

      async function load() {
        ui.mount(body, ui.skeleton({ title: true, lines: 8 }));
        try {
          let d;
          try {
            d = await api.get('/api/history/aircraft/' + encodeURIComponent(icao), { params: { limit: 200 }, signal: ctx.signal });
          } catch (e) {
            if (!e || e.status !== 404) throw e;
            d = await api.aircraftDetail(icao, { signal: ctx.signal }); // tracked or merely looked up, never stored
            d.events = [];
            if (!d.aircraft && !d.seen) d.neverSeen = true;
          }
          if (closed) return;
          doc = d;
          render();
          if (d.neverSeen) body.insertBefore(h('div.ac-note', 'This address has never been received by this station; only the reference data is shown.'), body.children[1] || null);
          const flights = d.flights || [];
          const pick = (selFlight && flights.find((f) => f.id === selFlight)) || flights.find((f) => f.positions > 0) || null;
          if (pick) selectFlight(pick.id, !!selFlight);
          else if (selFlight) selectFlight(selFlight, true); // older than the 200 newest: load it directly
        } catch (e) {
          if (closed || (e && e.aborted)) return;
          ui.mount(body, ui.error(e, { title: 'Could not load ' + icao, retry: load }));
        }
      }
      load();
    }

    // ------------------------------------------------------------------------------------------
    // page shell, routing, refresh
    // ------------------------------------------------------------------------------------------
    let curRoute = ctx.route || { args: [], query: {} };
    const tabs = ui.tabs({
      active: 'airframes',
      onChange: (id) => ctx.navigate('aircraft' + (id !== 'airframes' ? '/' + id : '')),
      tabs: [
        { id: 'airframes', label: 'Airframes', icon: 'plane', content: airframesPanel },
        { id: 'flights', label: 'Recent flights', icon: 'route', content: flightsPanel, onShow: () => { if (!flStarted) { flStarted = true; loadFlights(); } } },
        { id: 'events', label: 'Events', icon: 'history', content: eventsPanel, onShow: () => { if (!evStarted) { evStarted = true; loadRecords(); loadEvents(); } } },
        { id: 'database', label: 'Aircraft database', icon: 'database', content: databasePanel, onShow: () => { if (!dbStarted) { dbStarted = true; loadDb(); } } },
      ],
    });

    container.append(h('div.page-inner.ac-page',
      h('div.page-head', h('div', h('h2', 'Aircraft database'), pageSub)),
      ui.card({ flush: true, body: tabs, class: 'ac-card' })));

    // #/aircraft?q=<search>&country=<name>&type=<ICAO type> (links from the Statistics page) preset
    // the airframe filters. Returns true when the filters changed.
    function routeFilters(route) {
      const q = (route && route.query) || {};
      if (q.q === undefined && q.country === undefined && q.type === undefined) return false;
      const nq = String(q.q || '').trim(), nc = String(q.country || ''), nt = String(q.type || '');
      if (nq === S.q && nc === S.country && nt === S.type) return false;
      Object.assign(S, { q: nq, country: nc, type: nt, page: 0 });
      afSearch.value = nq; afCountry.value = nc; afType.value = nt;
      return true;
    }

    function applyRoute(route) {
      curRoute = route || { args: [], query: {} };
      const a0 = curRoute.args && curRoute.args[0];
      if (a0 && HEX.test(a0)) {
        showDrawer(a0.toUpperCase(), curRoute.query && curRoute.query.flight);
        return;
      }
      if (drawer) drawer.close(true);
      const tab = TABS.includes(a0) ? a0 : 'airframes';
      if (tabs.active !== tab) tabs.select(tab);
      if (tab === 'airframes' && routeFilters(curRoute)) reset();
    }
    page = { onRoute: applyRoute };
    ctx.onCleanup(() => { if (drawer) drawer.close(true); page = null; });

    if (!(ctx.route && ctx.route.args && ctx.route.args[0])) routeFilters(ctx.route);
    loadAirframes();
    loadFilterOptions();
    applyRoute(ctx.route);

    // gentle refresh: only the visible tab, never while a request of the same kind is the user's own
    ctx.every(30000, () => {
      if (document.hidden) return;
      if (drawer) drawer.refresh();
      if (tabs.active === 'airframes') loadAirframes({ quiet: true });
      else if (tabs.active === 'flights' && F.page === 0) loadFlights({ quiet: true });
      else if (tabs.active === 'events') { loadRecords(); loadEvents({ quiet: true }); }
      else if (tabs.active === 'database' && !(dbDoc && dbDoc.status && dbDoc.status.running)) loadDb();
    });
    ctx.every(120000, () => { if (!document.hidden) loadFilterOptions(); });
    let liveTick = 0;
    ctx.subscribe('aircraft', () => {
      if (drawer) drawer.onLive();
      if (++liveTick % 5 === 0 && tabs.active === 'airframes') paintLiveDots();
    });
    ctx.onCleanup(units.onChange(() => {
      paintAirframes(); paintFlights(); paintEvents(); paintRecords();
      if (dbDoc) { paintInstalled(); paintSources(); paintProgress(dbDoc.status || {}); }
      if (drawer) drawer.rerender();
    }));
  },

  onRoute(route) { if (page) page.onRoute(route); },
};
