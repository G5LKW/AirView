// settings/sections-data.js - Storage & database, Danger zone and About / diagnostics sections.

import { clone, validate, checkImportShape, SECTIONS } from './model.js';

const isNum = (v) => typeof v === 'number' && Number.isFinite(v);

// =============================================================================================
// Storage & database
// =============================================================================================

export function renderStorage(S, F) {
  const { ui, api, units } = S.ctx;
  const { h } = ui;
  const scope = S.scope;

  // ---- database statistics ----
  const dbHost = h('div');
  const reloadDb = S.panel(dbHost, (signal) => api.get('/api/db/stats', { signal }), (r) => {
    const st = r.stats || {};
    const tables = Object.entries(r.tables || {}).map(([table, rows]) => ({ table, rows })).filter((t) => t.table !== 'schema_migrations');
    const total = tables.reduce((n, t) => n + (t.rows || 0), 0);
    const dropped = (st.droppedLow || 0) + (st.droppedMid || 0) + (st.droppedHigh || 0);
    return [
      h('div.grid.cols-4.set-tiles',
        ui.stat({ label: 'Database file', icon: 'database', value: units.bytes(st.dbBytes), sub: `+ ${units.bytes(st.walBytes)} write-ahead log` }),
        ui.stat({ label: 'Rows', icon: 'list', value: units.si(total), sub: `${tables.length} tables` }),
        ui.stat({ label: 'Reclaimable', icon: 'box', value: units.bytes((st.freelistPages || 0) * (st.pageSize || 0)), sub: 'free pages - "Compact" returns them' }),
        ui.stat({ label: 'Writer', icon: 'save', accent: st.flushErrors || dropped ? 'red' : undefined, value: st.flushErrors || dropped ? 'Check' : 'OK', sub: st.lastFlush ? `last write ${units.ago(st.lastFlush)}` : 'no write yet' })),
      ui.kv([
        ['File', st.path || '—', { text: true, tip: st.path }],
        ['Rows waiting to be written', units.int(st.queuedRows)],
        ['Write errors / dropped rows', `${units.int(st.flushErrors)} / ${units.int(dropped)}`],
      ]),
      total === 0
        ? ui.empty({ icon: 'database', compact: true, title: 'The database is still empty', text: 'Rows appear within a minute of the first received aircraft; hourly tables fill after the first full hour.' })
        : ui.table({ columns: [
          { key: 'table', label: 'Table', sort: true },
          { key: 'rows', label: 'Rows', align: 'right', num: true, sort: true, sortDir: 'desc', render: (t) => units.int(t.rows) },
        ], rows: tables, sort: { key: 'rows', dir: 'desc' }, class: 'set-table-scroll' }),
    ];
  }, 30000);

  // ---- maintenance ----
  const out = h('div');
  const result = (ok, ...content) => ui.mount(out, h('div.set-result.' + (ok ? 'ok' : 'bad'), ui.icon(ok ? 'check-circle' : 'x-circle'), h('div', content)));
  const sizes = (r) => `${units.bytes(r.beforeBytes)} → ${units.bytes(r.afterBytes)} in ${units.num(r.elapsedMs / 1000, 1)} s`;
  function maint(label, icon, path, opts) {
    const btn = ui.button({ label, icon, tip: opts.tip, onClick: async () => {
      if (opts.confirm && !(await ui.confirm(opts.confirm()))) return;
      btn.setLoading(true);
      ui.mount(out, ui.loading(opts.busy, { compact: true }));
      try {
        const r = await S.admin(() => api.post(path, undefined, { signal: scope.signal, timeout: 900000 }));
        result(true, h('b', opts.done), ' ', opts.describe(r));
        reloadDb({ quiet: true });
      } catch (e) {
        if (e.aborted || e.cancelled) return;
        result(false, h('b', `${label} failed. `), e.message);
      } finally {
        btn.setLoading(false);
      }
    } });
    return btn;
  }
  const storageDirty = () => JSON.stringify(S.draft.storage) !== JSON.stringify(S.saved.storage);
  const pruneBtn = maint('Prune now', 'trash', '/api/db/prune', {
    tip: 'Delete everything older than the retention periods',
    busy: 'Deleting old rows…', done: 'Pruned.',
    confirm: () => ({ title: 'Prune the database now?', confirmLabel: 'Prune', danger: true,
      message: `Rows older than the saved retention periods are deleted for good: trails older than ${S.saved.storage.trailRetentionDays} days, flights older than ${S.saved.storage.flightRetentionDays || '∞'} days, per-minute statistics older than ${S.saved.storage.minuteRetentionDays} days.${storageDirty() ? ' Your unsaved retention changes are NOT used - save first if you want them to apply.' : ''} AirView does the same automatically once a day.` }),
    describe: (r) => { const p = r.pruned || {}; return `${units.int(p.flights)} flights, ${units.int(p.positions)} trail points, ${units.int(p.minuteStats)} minute rows, ${units.int((p.metObs || 0) + (p.ntpSamples || 0) + (p.receiverStatus || 0))} other rows removed. ${sizes(r)}.`; },
  });
  const vacuumBtn = maint('Compact (vacuum)', 'minimize', '/api/db/vacuum', {
    tip: 'Rewrite the database file to return free space to the disk',
    busy: 'Compacting the database - this can take minutes…', done: 'Compacted.',
    confirm: () => ({ title: 'Compact the database?', confirmLabel: 'Compact',
      message: 'The whole file is rewritten. Recording pauses while it runs: seconds on a PC, possibly minutes on a Raspberry Pi with a large database, and it needs free disk space of about the database size. Only worthwhile after pruning a lot.' }),
    describe: sizes,
  });
  const checkpointBtn = maint('Flush to disk', 'save', '/api/db/checkpoint', { tip: 'Write the in-memory statistics and fold the write-ahead log into the database file', busy: 'Flushing…', done: 'Flushed.', describe: sizes });
  const rollupBtn = maint('Roll up hours', 'history', '/api/db/rollup', { tip: 'Aggregate minute statistics into hourly rows now (normally 3 minutes past each hour)', busy: 'Rolling up…', done: 'Rolled up.', describe: (r) => `${units.int(r.hours)} hours aggregated. ${sizes(r)}.` });

  // ---- CSV export ----
  const exportHost = h('div');
  S.panel(exportHost, (signal) => api.get('/api/db/export', { signal }), (r) => {
    const rows = r.tables || [];
    if (!rows.length) return ui.empty({ compact: true, icon: 'download', title: 'Nothing to export' });
    return ui.table({ class: 'set-table-scroll', columns: [
      { key: 'table', label: 'Table', sort: true },
      { key: 'rows', label: 'Rows', align: 'right', num: true, sort: true, sortDir: 'desc', render: (t) => units.int(t.rows) },
      { key: 'dl', label: 'Download', render: (t) => h('span.row',
        h('a.set-link', { href: t.url, download: `${t.table}.csv` }, ui.icon('download'), 'all'),
        t.timeColumn ? h('a.set-link', { href: `${t.url}?hours=24`, download: `${t.table}-24h.csv` }, 'last 24 h') : null,
        t.timeColumn ? h('a.set-link', { href: `${t.url}?days=7`, download: `${t.table}-7d.csv` }, '7 days') : null) },
    ], rows });
  });

  // ---- aircraft database ----
  const acdbHost = h('div');
  const progHost = h('div');
  let polling = false;
  const reloadAcdb = S.panel(acdbHost, (signal) => api.get('/api/db/acdb/sources', { signal }), (r) => {
    const inst = r.installed || {};
    paintProgress(r.status);
    if (r.status?.running) pollProgress();
    return [
      inst.installed || inst.rows
        ? h('div.set-result.ok', ui.icon('check-circle'), h('div', h('b', `${units.int(inst.rows)} aircraft installed`),
          inst.info ? ` from ${inst.info.source}, imported ${units.dateTime(inst.info.t)}.` : '.',
          inst.info?.attribution ? h('div.set-help', inst.info.attribution, inst.info.licence ? ` Licence: ${inst.info.licence}.` : '') : null))
        : ui.empty({ icon: 'plane', compact: true, title: 'No aircraft database installed', text: 'Without one AirView still derives country and (for many states) the registration from the ICAO address. An imported database adds registration, type and operator for most airframes.' }),
      h('div.set-sources', (r.sources || []).map((src) => h('div.set-source',
        h('div.set-source-head', h('b', src.name), ui.badge(`~${units.bytes(src.approxBytes)}`, 'blue'), ui.badge(`~${units.si(src.approxRecords)} aircraft`, 'other')),
        h('p', src.notes),
        h('p.set-licence', h('span.label', 'Licence'), ' ', src.licenceUrl ? h('a', { href: src.licenceUrl, target: '_blank', rel: 'noopener noreferrer' }, src.licence) : src.licence),
        h('p.set-licence', h('span.label', 'Attribution'), ' ', src.attribution),
        h('div.row-wrap',
          ui.button({ label: 'Download and import', icon: 'download', variant: 'primary', size: 'sm', onClick: () => startImport(src) }),
          src.homepage ? h('a.set-link', { href: src.homepage, target: '_blank', rel: 'noopener noreferrer' }, ui.icon('external'), 'Project page') : null)))),
    ];
  });

  function paintProgress(st) {
    if (!st || !st.stage) { ui.mount(progHost); return; }
    const p = st.progress || {};
    const pct = p.bytesTotal > 0 ? Math.min(100, (p.bytesRead / p.bytesTotal) * 100) : null;
    if (st.running) {
      ui.mount(progHost, h('div.set-progress',
        h('div.row', h('span.spinner'), h('b', st.stage === 'store' ? 'Writing to the database…' : 'Downloading and reading…'), h('span.spacer'),
          ui.button({ label: 'Cancel', icon: 'x', size: 'sm', variant: 'ghost', onClick: async () => {
            try { paintProgress(await S.admin(() => api.post('/api/db/acdb/cancel', undefined, { signal: scope.signal }))); } catch (e) { if (!e.aborted && !e.cancelled) ui.toast.error('Cancel failed: ' + e.message); }
          } })),
        pct === null ? h('div.meter.set-meter-busy', h('i')) : ui.meter(pct),
        h('div.set-help.num', `${st.source || ''} · ${units.bytes(p.bytesRead)}${p.bytesTotal > 0 ? ' of ' + units.bytes(p.bytesTotal) : ''} · ${units.int(p.records)} records read · ${units.int(st.stored)} stored`)));
    } else if (st.stage === 'done') {
      ui.mount(progHost, h('div.set-result.ok', ui.icon('check-circle'), h('div', h('b', 'Last import finished. '), `${units.int(st.stored)} aircraft stored from ${st.source || 'the file'}${st.finished ? ', ' + units.ago(st.finished) : ''}.${p.skipped ? ` ${units.int(p.skipped)} malformed lines skipped.` : ''}`)));
    } else {
      ui.mount(progHost, h('div.set-result.bad', ui.icon('x-circle'), h('div', h('b', st.stage === 'cancelled' ? 'Last import was cancelled. ' : 'Last import failed. '), st.error || '', ' The installed database was left untouched.')));
    }
  }
  function pollProgress() {
    if (polling) return;
    polling = true;
    const tick = async () => {
      if (scope.signal.aborted) return;
      try {
        const st = await api.get('/api/db/acdb/progress', { signal: scope.signal });
        paintProgress(st);
        if (st.running) { setTimeout(tick, 1000); return; }
        polling = false;
        reloadAcdb({ quiet: true });
        reloadDb({ quiet: true });
      } catch (e) {
        if (e.aborted || e.cancelled) return;
        setTimeout(tick, 3000);
      }
    };
    setTimeout(tick, 600);
  }
  async function startImport(src) {
    const ok = await ui.confirm({ title: `Import ${src.name}?`, confirmLabel: 'Download and import',
      message: h('div.col.gap-2', h('p', `AirView downloads about ${units.bytes(src.approxBytes)} from ${new URL(src.url).hostname} and replaces the installed aircraft database. This needs the internet and runs in the background.`),
        h('p.set-licence', h('span.label', 'Licence'), ' ', src.licence), h('p.set-licence', h('span.label', 'Attribution'), ' ', src.attribution)) });
    if (!ok) return;
    try {
      paintProgress(await S.admin(() => api.post('/api/db/acdb/import', { source: src.id }, { signal: scope.signal })));
      pollProgress();
    } catch (e) {
      if (e.aborted || e.cancelled) return;
      if (e.status === 409) { ui.toast.warn('An import is already running.'); pollProgress(); } else ui.toast.error('Import could not be started: ' + e.message);
    }
  }
  const fileInp = h('input.sr-only', { type: 'file', accept: '.csv,.gz,.csv.gz,text/csv,application/gzip', tabindex: -1 });
  const uploadBtn = ui.button({ label: 'Import a file…', icon: 'upload', size: 'sm', tip: 'tar1090-db aircraft.csv(.gz) or an OpenSky aircraft database CSV, up to 600 MB', onClick: () => fileInp.click() });
  fileInp.addEventListener('change', async () => {
    const file = fileInp.files && fileInp.files[0];
    fileInp.value = '';
    if (!file) return;
    if (file.size > 600 * 1024 * 1024) { ui.toast.error('The file is larger than 600 MB.'); return; }
    if (!(await ui.confirm({ title: 'Import this file?', confirmLabel: 'Import', message: `"${file.name}" (${units.bytes(file.size)}) replaces the installed aircraft database. Make sure you are allowed to use the data and keep its attribution.` }))) return;
    uploadBtn.setLoading(true);
    pollProgress();
    try {
      const st = await S.admin(() => api.post('/api/db/acdb/upload', file, { params: { name: file.name }, headers: { 'Content-Type': 'application/octet-stream' }, signal: scope.signal, timeout: 0 }));
      paintProgress(st);
      reloadAcdb({ quiet: true });
    } catch (e) {
      if (!e.aborted && !e.cancelled) ui.mount(progHost, h('div.set-result.bad', ui.icon('x-circle'), h('div', h('b', 'Import failed. '), e.message)));
    } finally {
      uploadBtn.setLoading(false);
    }
  });

  return [
    ui.card({ title: 'Retention', icon: 'history', body: [
      F.help('AirView prunes old detail once a day. Hourly and daily statistics, coverage, records and the list of every airframe ever seen are small and kept forever.'),
      h('div.set-fields.set-fields--2',
        F.number('storage.trailRetentionDays', { label: 'Keep position trails', suffix: 'days', min: 1, max: 3650, step: 1, hint: 'Stored track points of past flights (1 - 3650). By far the largest table: roughly 10-40 MB per day on a busy station.' }),
        F.number('storage.trailIntervalSec', { label: 'Trail point spacing', suffix: 's', min: 1, max: 3600, step: 1, hint: 'Minimum seconds between two stored track points of one aircraft (1 - 3600). 5 s is smooth; 15 s cuts the size to a third.' }),
        F.number('storage.flightRetentionDays', { label: 'Keep flights', suffix: 'days', min: 0, max: 36500, step: 1, hint: 'One row per contact with an aircraft (0 = keep forever). 730 days = two years of history.' }),
        F.number('storage.minuteRetentionDays', { label: 'Keep per-minute statistics', suffix: 'days', min: 1, max: 3650, step: 1, hint: 'Minute-resolution message rates and counts (1 - 3650); the hourly roll-ups stay forever.' })),
    ] }),
    ui.card({ title: 'Database', icon: 'database', body: dbHost }),
    ui.card({ title: 'Maintenance', icon: 'settings', body: [
      h('div.row-wrap.set-actions', pruneBtn, vacuumBtn, checkpointBtn, rollupBtn),
      out,
      F.help('None of this is needed in normal operation. Prune applies the saved retention periods immediately; Compact shrinks the file afterwards.'),
    ] }),
    ui.card({ title: 'CSV export', icon: 'download', body: [
      F.help('Every table can be downloaded as CSV with a header row, exactly as stored (times in Unix seconds). Large tables are streamed - "positions" can be hundreds of megabytes.'),
      exportHost,
    ] }),
    ui.card({ title: 'Aircraft database', icon: 'plane', actions: [uploadBtn, fileInp], body: [progHost, acdbHost] }),
  ];
}

// =============================================================================================
// Danger zone
// =============================================================================================

export function renderDanger(S, F) {
  const { ui, api, units, config } = S.ctx;
  const { h } = ui;
  const scope = S.scope;

  // ---- export ----
  const exportBtn = ui.button({ label: 'Download config.json', icon: 'download', onClick: async () => {
    exportBtn.setLoading(true);
    try {
      const cfg = await api.get('/api/config', { signal: scope.signal });   // secrets arrive as "" / "set"
      const blob = new Blob([JSON.stringify(cfg, null, 2) + '\n'], { type: 'application/json' });
      const url = URL.createObjectURL(blob);
      const a = h('a', { href: url, download: `airview-config-${units.date(Date.now())}.json` });
      document.body.appendChild(a);
      a.click();
      a.remove();
      setTimeout(() => URL.revokeObjectURL(url), 5000);
      if (S.isDirty()) ui.toast.info('The file contains the saved settings - your unsaved changes are not in it.');
    } catch (e) {
      if (!e.aborted && !e.cancelled) ui.toast.error('Export failed: ' + e.message);
    } finally {
      exportBtn.setLoading(false);
    }
  } });

  // ---- import ----
  const importOut = h('div');
  const fileInp = h('input.sr-only', { type: 'file', accept: '.json,application/json', tabindex: -1 });
  const importBtn = ui.button({ label: 'Import config.json…', icon: 'upload', onClick: () => fileInp.click() });
  const labelOf = (id) => SECTIONS.find((s) => s.id === id)?.label || id;
  fileInp.addEventListener('change', async () => {
    const file = fileInp.files && fileInp.files[0];
    fileInp.value = '';
    if (!file) return;
    const fail = (title, list) => ui.mount(importOut, h('div.set-result.bad', ui.icon('x-circle'), h('div', h('b', title), h('ul.set-problem-list', list.map((t) => h('li', t))), h('div.set-help', 'Nothing was changed.'))));
    if (file.size > 2 * 1024 * 1024) { fail('This is not an AirView configuration file.', ['The file is larger than 2 MB.']); return; }
    let obj;
    try { obj = JSON.parse(await file.text()); } catch (e) { fail('The file is not valid JSON.', [e.message]); return; }
    const shape = checkImportShape(obj);
    if (shape.length) { fail('This is not an AirView configuration file.', shape); return; }
    // Sections missing from the file keep their stored value, exactly as the server treats them.
    const full = { ...clone(S.saved), ...obj, version: S.saved.version };
    for (const k of ['http', 'receiver', 'ntp', 'weather', 'hub', 'remote', 'storage', 'ui', 'demo']) full[k] = { ...clone(S.saved[k]), ...(obj[k] || {}) };
    let problems;
    try { problems = validate(full); } catch (e) { fail('The file could not be checked.', [e.message]); return; }
    if (problems.length) {
      fail(`The file contains ${problems.length} invalid ${problems.length === 1 ? 'value' : 'values'}.`, problems.slice(0, 12).map((p) => `${labelOf(p.section)} - ${p.path}: ${p.msg}`).concat(problems.length > 12 ? [`… and ${problems.length - 12} more`] : []));
      return;
    }
    const sections = Object.keys(obj).filter((k) => k !== 'version');
    const ok = await ui.confirm({ title: 'Replace the configuration?', confirmLabel: 'Import and save', danger: true,
      message: h('div.col.gap-2',
        h('p', `"${file.name}" passed validation. It replaces: ${sections.join(', ')}. The engine applies the new settings at once (feeds reconnect).`),
        h('p', 'Passwords written as "set" or left empty keep the ones stored now.'),
        S.isDirty() ? h('p', h('b', 'Your unsaved changes on this page are replaced by the file.')) : null) });
    if (!ok) return;
    S.draft = full;
    S.local.clear();
    S.notes.clear();
    S.touch();
    ui.mount(importOut);
    await S.save();
    if (S.saveError) ui.mount(importOut, h('div.set-result.bad', ui.icon('x-circle'), h('div', h('b', 'The imported configuration was not saved. '), 'It is loaded as unsaved changes - use Save to try again or Discard to drop it.')));
    else ui.mount(importOut, h('div.set-result.ok', ui.icon('check-circle'), h('div', h('b', 'Configuration imported and saved.'))));
  });

  // ---- factory reset ----
  const resetBtn = ui.button({ label: 'FACTORY RESET', icon: 'alert', variant: 'danger', size: 'lg', onClick: async () => {
    const ok = await ui.confirm({
      title: 'Factory reset', danger: true, requireText: 'RESET', confirmLabel: 'Erase everything', width: 520,
      message: h('div.col.gap-2',
        h('p', h('b', 'This cannot be undone.'), ' The following is erased from this AirView station:'),
        h('ul.set-problem-list',
          h('li', h('b', 'The database'), ' - every aircraft, flight and stored trail ever recorded.'),
          h('li', h('b', 'All statistics'), ' - message rates, coverage, records, the heat map, weather and timing history, forecast models, the imported aircraft database.'),
          h('li', h('b', 'The configuration'), ' - station position, antennas, feeds, passwords and every other setting return to the defaults.')),
        h('p', 'Kept: the map tile cache and the display preferences stored in your browsers. All admin logins end. Export config.json first if you want your settings back afterwards.')),
    });
    if (!ok) return;
    resetBtn.setLoading(true);
    const busy = ui.modal({ title: 'Factory reset running…', icon: 'alert', danger: true, dismissable: false, actions: [], body: ui.loading('Wiping the database and restarting the engine. This takes a few seconds.') });
    try {
      await S.admin(() => api.post('/api/factory-reset', { confirm: 'RESET' }, { timeout: 120000 }));
      S.draft = clone(S.saved);            // nothing left to protect: do not trigger the unsaved-changes guard
      S.ctx.shared.set('settings.stash', null);
      try { await config.load(); } catch { /* the reload below fetches it again */ }
      busy.close();
      ui.toast.success('Factory reset done - reloading.');
      setTimeout(() => { location.hash = '#/settings/station'; location.reload(); }, 900);
    } catch (e) {
      busy.close();
      resetBtn.setLoading(false);
      if (e.cancelled) return;
      ui.alert('Factory reset failed', `${e.message}. Nothing may have been erased - check the status bar and try again.`);
    }
  } });

  return [
    ui.card({ title: 'Configuration file', icon: 'save', body: [
      F.help('config.json holds everything on these settings pages. Download it as a backup or to copy a setup to another station. Passwords are never exported: the file only says "set", and importing "set" keeps whatever password the station has.'),
      h('div.row-wrap.set-actions', exportBtn, importBtn, fileInp),
      importOut,
      F.help('An imported file is checked with the same rules as the forms above and refused as a whole if anything is invalid; then it is saved and applied like a normal Save.'),
    ] }),
    h('section.card.set-danger',
      h('div.card-head', ui.icon('alert'), h('div.grow', h('div.card-title', 'Factory reset'))),
      h('div.card-body',
        h('p', 'Returns this station to the state of a fresh installation.'),
        h('ul.set-problem-list',
          h('li', 'Deletes the database with all aircraft, flights, trails and every statistic.'),
          h('li', 'Deletes the configuration: location, antennas, feeds, passwords - back to defaults.'),
          h('li', 'Restarts the engine with the factory default feed; you set up your receiver feed again.')),
        h('div.row-wrap.set-actions', resetBtn, h('span.muted', 'You will be asked to type RESET.')))),
  ];
}

// =============================================================================================
// About & diagnostics
// =============================================================================================

const LIBS = [
  ['MapLibre GL JS 6.18', 'BSD-3-Clause', 'vendor/maplibre/LICENSE.txt', 'Map rendering'],
  ['Apache ECharts 6.1', 'Apache-2.0', 'vendor/echarts/LICENSE', 'Charts (bundles zrender, BSD-3-Clause)'],
  ['Inter', 'SIL Open Font License 1.1', 'vendor/fonts/LICENSE-Inter.txt', 'Interface typeface'],
  ['JetBrains Mono', 'SIL Open Font License 1.1', 'vendor/fonts/LICENSE-JetBrainsMono.txt', 'Numeric typeface'],
  ['modernc.org/sqlite', 'BSD-3-Clause', 'https://gitlab.com/cznic/sqlite', 'Embedded database (pure Go SQLite)'],
  ['go-webview2', 'MIT', 'https://github.com/jchv/go-webview2', 'Desktop window on Windows'],
];
const DATA = [
  ['OpenFreeMap', 'https://openfreemap.org/', 'Vector map tiles, fonts and sprites. © OpenMapTiles, data from OpenStreetMap.'],
  ['OpenStreetMap', 'https://www.openstreetmap.org/copyright', 'Map data and the optional raster base map. © OpenStreetMap contributors, ODbL.'],
  ['Open-Meteo', 'https://open-meteo.com/', 'Weather, upper-air profiles and terrain elevation (Copernicus DEM). CC BY 4.0.'],
  ['RainViewer', 'https://www.rainviewer.com/', 'Optional precipitation radar overlay. Personal / educational use with attribution.'],
  ['tar1090-db / Mictronics', 'https://github.com/wiedehopf/tar1090-db', 'Optional aircraft database. ODC-By 1.0.'],
  ['OpenSky Network', 'https://opensky-network.org/data/aircraft', 'Optional aircraft database, provided as is.'],
  ['World Magnetic Model 2025', 'https://www.ncei.noaa.gov/products/world-magnetic-model', 'Magnetic declination. NOAA / NCEI and BGS, public domain.'],
];

export function renderAbout(S, F) {
  const { ui, api, units } = S.ctx;
  const { h } = ui;
  const host = h('div');
  let last = null;
  S.panel(host, (signal) => api.get('/api/system', { signal }), (s) => {
    last = s;
    S.system = s;
    const mem = s.memory || {};
    const off = isNum(s.utcOffsetSec) ? `UTC${s.utcOffsetSec >= 0 ? '+' : '−'}${String(Math.floor(Math.abs(s.utcOffsetSec) / 3600)).padStart(2, '0')}:${String(Math.floor((Math.abs(s.utcOffsetSec) % 3600) / 60)).padStart(2, '0')}` : '';
    return [
      h('div.grid.cols-4.set-tiles',
        ui.stat({ label: 'Version', icon: 'info', value: s.version || '—', sub: `${s.app || 'AirView'} · ${s.goVersion || ''}` }),
        ui.stat({ label: 'Uptime', icon: 'timer', value: units.age(s.uptimeS), sub: `started ${units.dateTime(s.started)}` }),
        ui.stat({ label: 'Memory', icon: 'box', value: units.bytes(mem.heapInuseBytes), sub: `${units.bytes(mem.sysBytes)} reserved` }),
        ui.stat({ label: 'Database', icon: 'database', value: units.bytes(s.dbBytes), sub: `${units.int(s.tracker?.tracked)} aircraft tracked` })),
      ui.kv([
        ['Platform', `${s.platform || '—'} · ${units.int(s.cpus)} CPU cores`, { text: true }],
        ['Host name', s.hostname || '—', { text: true }],
        ['Window', s.gui ? 'desktop app (WebView2)' : 'headless server', { text: true }],
        ['Run mode', `${s.mode}${s.demo ? ' · demo mode' : ''}${s.located ? '' : ' · station not located'}`, { text: true }],
        ['Web server', `${s.httpAddr || '—'} (port ${s.httpPort})${s.restartRequired ? ' · restart required' : ''}`, { text: true }],
        ['Data directory', s.dataDir || '—', { text: true, tip: s.dataDir }],
        ['Server time zone', `${s.timeZone || '—'} ${off}`, { text: true }],
        ['Live browser connections', units.int(s.liveClients)],
        ['Admin sessions', units.int(s.sessions)],
        ['Message rate', units.rate(s.tracker?.msgRate)],
        ['Frames since start', units.int(s.tracker?.frames)],
        ['Goroutines / GC runs', `${units.int(s.goroutines)} / ${units.int(mem.numGc)}`],
        ['Forecast models', s.forecaster?.fitted ? `fitted ${units.ago(s.forecaster.fitted)} on ${units.int(s.forecaster.dataHours)} h of data` : 'not fitted yet', { text: true }],
        ['Process ID', String(s.pid ?? '—')],
      ], { columns: 2 }),
    ];
  }, 10000);

  const copyBtn = ui.button({ label: 'Copy diagnostics', icon: 'copy', size: 'sm', tip: 'Copy this information as JSON, e.g. for a bug report', onClick: async () => {
    if (!last) return;
    const ok = await ui.copy(JSON.stringify({ system: last, feeds: S.status?.feeds, browser: navigator.userAgent }, null, 2));
    if (ok === false) ui.toast.warn('Copying is not available here.'); else ui.toast.success('Diagnostics copied to the clipboard.');
  } });

  const link = (href, text) => h('a', { href, target: '_blank', rel: 'noopener noreferrer' }, text);
  return [
    ui.card({ title: 'This station', icon: 'radar', actions: copyBtn, body: host }),
    ui.card({ title: 'Bundled software', icon: 'box', body: [
      F.help('Everything is served from the AirView program itself - nothing is loaded from a CDN.'),
      h('div.table-wrap', h('table.table', h('thead', h('tr', h('th', 'Library'), h('th', 'Licence'), h('th', 'Used for'))),
        h('tbody', LIBS.map(([name, lic, href, use]) => h('tr', h('td', name), h('td', link(href, lic)), h('td.set-wrap', use)))))),
    ] }),
    ui.card({ title: 'Data sources and attribution', icon: 'globe', body: [
      h('dl.set-presets', DATA.map(([name, href, text]) => [h('dt', link(href, name)), h('dd', text)])),
      F.help('All of these are free services that need no account or API key. Be a good citizen: keep the tile cache on and the weather interval moderate.'),
    ] }),
  ];
}
