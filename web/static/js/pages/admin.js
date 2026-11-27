// pages/admin.js - the network administrator's console (aggregator mode, DESIGN §11):
//   #/admin/stations   station registry: search and filter; approve, hold, deny, revoke ("ban"), rename,
//                      reveal the exact location (audited), revoke the feeder key, delete
//   #/admin/tokens     enrol tokens for the "invite" admission policy: mint (secret shown once), revoke
//   #/admin/mlat       the MLAT console (mlat/console.js): hold-out accuracy over time, solve rates, clock
//                      quality and residuals per station, sync matrix, CRLB statistics
//   #/admin/locations  location quality and the reverse-MLAT survey of every station (net/locadmin.js):
//                      start / cancel surveys, the hourly network bundle adjustment; positions only on the
//                      station's own Location tab and in the bundle result (admin)
//   #/admin/federation federation status, peers, out-feeds and the peer settings (net/fedadmin.js)
//   #/admin/shares     every owner's share links: search, state, revoke (net/shares.js)
//   #/admin/audit      the audit log (every exact-data view and every change)
//   #/admin/policy     admission, public map / statistics, default pin, island guard, survey auto-use,
//                      registration - saved through PUT /api/config (aggregator block); settings fixed by the
//                      operator file or AIRVIEW_* variables are shown locked.
//
// API: /api/admin/* (net/endpoints.js), GET/PUT /api/config.

import { EP, normStation, normStationDetail } from '../net/endpoints.js';
import { ADMISSION_LABELS, KIND_LABELS, PIN_OPTIONS, clockLabel, shortSid } from '../net/model.js';
import { pageHead, failState, admissionBadge, v, downloadText } from '../net/view.js';
import { createAdmin } from '../admin.js';
import { renderMLATConsole } from '../mlat/console.js';
import { renderLocationsAdmin } from '../net/locadmin.js';
import { renderFederationAdmin } from '../net/fedadmin.js';
import { renderAdminShares } from '../net/shares.js';
import { pagerBar } from '../net/pager.js';

const TABS = [
  { id: 'stations', label: 'Stations', icon: 'antenna' },
  { id: 'mlat', label: 'MLAT', icon: 'target' },
  { id: 'locations', label: 'Locations', icon: 'pin' },
  { id: 'federation', label: 'Federation', icon: 'globe' },
  { id: 'shares', label: 'Share links', icon: 'share' },
  { id: 'tokens', label: 'Enrol tokens', icon: 'key' },
  { id: 'audit', label: 'Audit log', icon: 'history' },
  { id: 'policy', label: 'Policy', icon: 'shield' },
];

let P = null;

export default {
  id: 'admin',
  title: 'Network admin',

  mount(container, ctx) {
    const { ui } = ctx;
    const { h } = ui;
    P = { ctx, admin: createAdmin(ctx) };
    const tabId = TABS.some((t) => t.id === ctx.route.args[0]) ? ctx.route.args[0] : 'stations';
    const tabs = ui.tabs({
      active: tabId,
      onChange: (id) => history.replaceState(null, '', '#/admin/' + id),
      tabs: TABS.map((t) => ({ ...t, render: (panel) => RENDER[t.id](panel) })),
    });
    P.tabs = tabs;
    container.append(h('div.page-inner', pageHead('Network admin', `${ctx.net.username} · every exact view is recorded in the audit log`), tabs));
  },

  onRoute(route) { if (P && TABS.some((t) => t.id === route.args[0])) P.tabs.select(route.args[0]); },
  onNet() {},
  unmount() { P = null; },
};

/** Run an admin write: re-login on 401, toast on error. Returns the result or undefined. */
async function act(fn, okText) {
  const { ui } = P.ctx;
  try {
    const r = await P.admin.run(fn, 'This needs the network administrator.');
    if (okText) ui.toast.success(okText);
    return r ?? true;
  } catch (e) {
    if (!e.cancelled) ui.toast.error(e.message || 'Failed');
    return undefined;
  }
}

// -----------------------------------------------------------------------------------------------
// stations
// -----------------------------------------------------------------------------------------------
const RENDER = {
  mlat(panel) { renderMLATConsole(panel, P.ctx); },

  locations(panel) { renderLocationsAdmin(panel, P.ctx, act); },

  federation(panel) { renderFederationAdmin(panel, P.ctx, act); },

  shares(panel) { renderAdminShares(panel, P.ctx, act); },

  stations(panel) {
    const { ui, units, api } = P.ctx;
    const { h } = ui;
    const f = { q: '', admission: '', kind: '', unclaimed: '' };
    const search = ui.input({ placeholder: 'Name, ID, owner, UUID', icon: 'search', size: 'sm', clearable: true, onInput: ui.debounce((x) => { f.q = x; refilter(); }, 300) });
    const adm = ui.select({ size: 'sm', value: '', options: [{ value: '', label: 'Any admission' }, ...Object.entries(ADMISSION_LABELS).map(([value, label]) => ({ value, label }))], onChange: (x) => { f.admission = x; refilter(); } });
    const kind = ui.select({ size: 'sm', value: '', options: [{ value: '', label: 'Any kind' }, ...Object.entries(KIND_LABELS).map(([value, label]) => ({ value, label }))], onChange: (x) => { f.kind = x; refilter(); } });
    const online = ui.select({ size: 'sm', value: '', options: [{ value: '', label: 'Claimed or not' }, { value: '1', label: 'Unclaimed' }], onChange: (x) => { f.unclaimed = x; refilter(); } });
    const count = h('span.muted');
    const host = h('div');
    // Server-side paging and sorting (DESIGN §20.2: 1,000+ stations): only one page is fetched and drawn.
    const prefs = ui.prefs('airview.admin.stations', { sort: 'online', dir: 'desc', limit: 50 });
    let sort = { key: prefs.get().sort, dir: prefs.get().dir };
    let page = { offset: 0, limit: prefs.get().limit };
    const pager = pagerBar({ pageSize: page.limit, onChange: (s) => { page = { offset: s.offset, limit: s.limit }; prefs.set({ limit: s.limit }); load(); } });
    const refilter = () => { page.offset = 0; load(); };
    panel.append(h('div.col.gap-3', h('div.row.gap-2.net-filters', search, adm, kind, online, h('span.grow'), count,
      ui.button({ icon: 'refresh', size: 'sm', variant: 'ghost', tip: 'Reload', onClick: () => load() })), host, pager));
    const sortHead = (label, key, firstDir = 'asc') => {
      const on = sort.key === key;
      const b = h('button.net-th-sort', { type: 'button', 'aria-sort': on ? (sort.dir === 'desc' ? 'descending' : 'ascending') : 'none' },
        label, h('span.net-th-ind', on ? (sort.dir === 'desc' ? ' ↓' : ' ↑') : ''));
      b.addEventListener('click', () => {
        sort = { key, dir: on ? (sort.dir === 'asc' ? 'desc' : 'asc') : firstDir };
        prefs.set({ sort: sort.key, dir: sort.dir });
        refilter();
      });
      return b;
    };
    let seq = 0;
    async function load() {
      const my = ++seq;
      if (!host.firstChild) ui.mount(host, ui.skeleton({ lines: 6 }));
      try {
        const r = await api.get(EP.adminStations(), { params: { ...f, limit: page.limit, offset: page.offset, sort: sort.key, dir: sort.dir }, signal: P.ctx.signal });
        if (my !== seq) return;
        const rows = (r?.stations || []).map(normStation).filter(Boolean);
        const total = Number.isFinite(r?.total) ? r.total : rows.length;
        const pending = r?.counts?.pending;
        ui.setText(count, `${units.int(total)} ${total === 1 ? 'station' : 'stations'}${pending ? ` · ${units.int(pending)} pending` : ''}`);
        pager.set({ total, offset: page.offset, limit: page.limit });
        ui.mount(host, ui.table({
          columns: [
            { key: 'name', label: sortHead('Station', 'name'), render: (s) => h('span.col', h('b', s.name || shortSid(s.sid)), h('code.muted', s.sid + (s.unidentified ? ' · unidentified' : '')),
              s.feederId ? h('code.muted', { title: 'Feeder ID, as shown on the feeder\'s own status page' }, `feeder ${s.feederId}`) : null) },
            { key: 'kind', label: sortHead('Kind', 'kind'), render: (s) => KIND_LABELS[s.kind] || s.kind || units.DASH },
            { key: 'owner', label: sortHead('Owner', 'owner'), render: (s) => s.owner || h('span.muted', 'unclaimed') },
            { key: 'admission', label: sortHead('Admission', 'admission'), render: (s) => admissionBadge(s.admission) || units.DASH },
            { key: 'online', label: sortHead('Online', 'online', 'desc'), render: (s) => h('span.row.gap-2', h('span.dot.' + (s.online ? 'ok' : 'off')), s.online ? 'yes' : s.lastSeen ? units.ago(s.lastSeen) : 'never') },
            { key: 'clock', label: 'Clock', render: (s) => clockLabel(s.clockClass) },
            { key: 'loc', label: 'Location', render: (s) => s.locStatus || units.DASH },
            { key: 'trust', label: sortHead('Trust', 'trust', 'desc'), align: 'right', num: true, render: (s) => v(s.trust, units.int) },
            { key: 'act', label: '', render: (s) => ui.button({ icon: 'more', size: 'sm', variant: 'ghost', tip: 'Actions', onClick: (e) => stationMenu(e.currentTarget, s, load) }) },
          ],
          rows,
          empty: 'No stations match',
        }));
      } catch (e) {
        if (my !== seq || e.aborted) return;
        ui.mount(host, failState(e, { retry: load }));
      }
    }
    load();
  },

  tokens(panel) {
    const { ui, units, api } = P.ctx;
    const { h } = ui;
    const host = h('div');
    const label = ui.input({ placeholder: 'e.g. Club member Smith', size: 'sm' });
    const uses = ui.input({ type: 'number', value: 1, min: 1, max: 1000, size: 'sm' });
    const days = ui.input({ type: 'number', value: 7, min: 1, max: 365, size: 'sm' });
    const mint = ui.button({ label: 'Create token', icon: 'plus', variant: 'primary', size: 'sm', onClick: async () => {
      const body = { note: label.value.trim(), maxUses: Math.max(1, Number(uses.value) || 1), ttlHours: Math.max(1, Number(days.value) || 7) * 24 };
      const r = await act(() => api.post(EP.adminTokenCreate(), body));
      if (!r || r === true) return;
      label.value = '';
      showToken({ ...(typeof r.token === 'object' ? r.token : r), secret: r.secret || r.token });
      load();
    } });
    panel.append(h('div.col.gap-4',
      ui.card({ title: 'New enrol token', icon: 'key', sub: 'With the "invite" policy a new feeder needs a token to join. Tokens are stored hashed: the value is shown once.', body: h('div.row.gap-3.net-filters',
        ui.field({ label: 'Label', control: label }), ui.field({ label: 'Uses', control: uses }), ui.field({ label: 'Valid (days)', control: days }), h('div.net-field-btn', mint)) }),
      ui.card({ title: 'Tokens', icon: 'list', body: host })));
    let seq = 0;
    async function load() {
      const my = ++seq;
      try {
        const r = await api.get(EP.adminTokens(), { signal: P.ctx.signal });
        if (my !== seq) return;
        const now = Date.now();
        ui.mount(host, ui.table({
          columns: [
            { key: 'label', label: 'Label', render: (t) => t.note || t.label || h('span.muted', '(no label)') },
            { key: 'uses', label: 'Used', align: 'right', render: (t) => `${units.int(t.uses || 0)} / ${units.int(t.maxUses || 1)}` },
            { key: 'created', label: 'Created', render: (t) => (t.created ? units.date(t.created) : units.DASH) },
            { key: 'expires', label: 'Expires', render: (t) => (t.expires ? units.dateTime(t.expires) : units.DASH) },
            { key: 'state', label: 'State', render: (t) => (t.revoked ? ui.badge('revoked', 'red') : t.expires && t.expires < now ? ui.badge('expired', 'other') : t.usable === false || (t.uses || 0) >= (t.maxUses || 1) ? ui.badge('used up', 'other') : ui.badge('active', 'green')) },
            { key: 'act', label: '', render: (t) => (t.revoked ? '' : ui.button({ label: 'Revoke', size: 'sm', variant: 'ghost', onClick: async () => {
              if (!(await ui.confirm({ title: 'Revoke this token?', message: 'Feeders that already joined stay; nobody else can use the token.', confirmLabel: 'Revoke', danger: true }))) return;
              if (await act(() => api.del(EP.adminTokenRevoke(t.id)), 'Token revoked')) load();
            } })) },
          ],
          rows: r?.tokens || [], empty: 'No tokens yet',
        }));
      } catch (e) { if (my === seq && !e.aborted) ui.mount(host, failState(e, { retry: load })); }
    }
    function showToken(t) {
      const host2 = location.host;
      const cmd = `airview-feeder enrol --server ${host2.split(':')[0]} --token ${t.secret}`;
      ui.modal({ title: 'Enrol token created', icon: 'key', width: 560, dismissable: false, body: h('div.col.gap-3',
        h('div.net-recovery-warn', ui.icon('alert'), h('span', 'Copy the token now. It is stored hashed and cannot be shown again.')),
        h('code.net-token', t.secret),
        ui.kv([['Label', t.note || t.label || '—'], ['Uses', String(t.maxUses || 1)], ['Expires', t.expires ? units.dateTime(t.expires) : '—']]),
        h('p.muted', 'On the feeder, for example:'), h('code.net-token', cmd),
        h('div.row.gap-2', ui.button({ label: 'Copy token', icon: 'copy', size: 'sm', onClick: async () => ((await ui.copy(t.secret)) ? ui.toast.success('Token copied') : null) }),
          ui.button({ label: 'Download', icon: 'download', size: 'sm', onClick: () => downloadText('airview-enrol-token.txt', `${t.secret}\n`) }))),
      actions: [{ label: 'Done', variant: 'primary' }] });
    }
    load();
  },

  audit(panel) {
    const { ui, units, api } = P.ctx;
    const { h } = ui;
    const host = h('div');
    const more = ui.button({ label: 'Older entries', size: 'sm', variant: 'ghost', onClick: () => load(true) });
    panel.append(h('div.col.gap-3', host, h('div.row', more)));
    let rows = [];
    async function load(older) {
      try {
        const before = older && rows.length ? rows[rows.length - 1].t : undefined;
        const r = await api.get(EP.adminAudit(), { params: { limit: 200, before }, signal: P.ctx.signal });
        const got = r?.entries || [];
        rows = older ? rows.concat(got) : got;
        more.classList.toggle('hidden', got.length < 200);
        ui.mount(host, ui.table({
          columns: [
            { key: 't', label: 'Time', render: (x) => units.dateTime(x.t) },
            { key: 'actor', label: 'Who' },
            { key: 'action', label: 'Action', render: (x) => h('code', x.action) },
            { key: 'target', label: 'Station', render: (x) => x.sid || x.target || units.DASH },
            { key: 'detail', label: 'Detail', render: (x) => x.detail || '' },
          ], rows, empty: 'The audit log is empty' }));
      } catch (e) { if (!e.aborted) ui.mount(host, failState(e, { retry: () => load(false) })); }
    }
    load(false);
  },

  policy(panel) {
    const { ui, config, api } = P.ctx;
    const { h } = ui;
    const host = h('div');
    panel.append(host);
    const draw = () => {
      const cfg = config.current;
      if (!cfg) { ui.mount(host, ui.loading('Loading the configuration…')); config.load().then(draw).catch((e) => ui.mount(host, failState(e, { retry: draw }))); return; }
      const a = cfg.aggregator || {};
      const locked = new Map((cfg.locked || []).map((x) => [x.path, x.origin]));
      const lockHint = (path, hint) => (locked.has(path) ? `Fixed by ${locked.get(path)} - change it there.` : hint);
      const dis = (path) => locked.has(path);
      const adm = ui.segmented({ block: true, value: a.admission || 'invite', options: [
        { value: 'open', label: 'Open', tip: 'Every new station joins at once' },
        { value: 'approve', label: 'Approve', tip: 'New stations wait for approval' },
        { value: 'invite', label: 'Invite', tip: 'New feeders need an enrol token (default)' }] });
      if (dis('aggregator.admission')) adm.querySelectorAll('button').forEach((b) => { b.disabled = true; });
      const pin = ui.select({ value: a.defaultPin || 'hidden', options: PIN_OPTIONS.map((o) => ({ value: o.value, label: o.label })), disabled: dis('aggregator.defaultPin') });
      const tg = (path, label, checked) => ui.toggle({ label, checked: !!checked, disabled: dis(path) });
      const pubMap = tg('aggregator.publicMap', 'Public map', a.publicMap);
      const pubStats = tg('aggregator.publicStats', 'Public network statistics', a.publicStats);
      const tar = tg('aggregator.tarJson', 'tar1090-compatible /data/*.json', a.tarJson);
      const island = tg('aggregator.islandGuard', 'Island guard (coarsen low aircraft heard only by hidden stations)', a.islandGuard);
      const autoUse = tg('aggregator.mlat.autoUseSurvey', `Use surveyed positions automatically (σ ≤ ${a.mlat?.autoUseMaxSigmaM ?? 50} m)`, a.mlat?.autoUseSurvey);
      const reg = ui.segmented({ value: a.auth?.registration || 'open', options: [{ value: 'open', label: 'Anyone may register' }, { value: 'closed', label: 'Closed' }] });
      if (dis('aggregator.auth.registration')) reg.querySelectorAll('button').forEach((b) => { b.disabled = true; });
      const name = ui.input({ value: a.name || '', disabled: dis('aggregator.name') });
      const save = ui.button({ label: 'Save policy', icon: 'save', variant: 'primary', onClick: async () => {
        save.setLoading(true);
        const ok = await act(() => config.save((c) => {
          delete c.locked;
          const g = c.aggregator || (c.aggregator = {});
          g.name = name.value.trim();
          g.admission = adm.value;
          g.defaultPin = pin.value;
          g.publicMap = pubMap.checked;
          g.publicStats = pubStats.checked;
          g.tarJson = tar.checked;
          g.islandGuard = island.checked;
          g.mlat = { ...(g.mlat || {}), autoUseSurvey: autoUse.checked };
          g.auth = { ...(g.auth || {}), registration: reg.value };
        }), 'Policy saved');
        save.setLoading(false);
        if (ok) draw();
      } });
      const field = (label, control, path, hint) => ui.field({ label, control, hint: lockHint(path, hint) });
      ui.mount(host, h('div.grid.cols-2.net-grid',
        ui.card({ title: 'Joining', icon: 'users', body: h('div.col.gap-3',
          field('Network name', name, 'aggregator.name', 'Shown on the public pages and in authenticator apps.'),
          field('Admission of new stations', adm, 'aggregator.admission', 'Compat (Beast) stations cannot present a token: under "invite" they wait for approval.'),
          field('Owner accounts', reg, 'aggregator.auth.registration', 'Closed: only accounts that already exist can log in.')) }),
        ui.card({ title: 'Public surfaces and privacy', icon: 'eye', body: h('div.col.gap-3',
          pubMap, pubStats, tar,
          field('Pin of new stations', pin, 'aggregator.defaultPin', 'Owners can change their own pin later.'),
          island, autoUse,
          locked.size ? h('p.muted', `${locked.size} setting${locked.size === 1 ? ' is' : 's are'} fixed by the operator configuration.`) : null) }),
        h('div.span-2.row', save)));
    };
    draw();
    void api;
  },
};

function stationMenu(anchor, s, reload) {
  const { ui, api } = P.ctx;
  const items = [];
  const setAdm = (a, text) => async () => { if (await act(() => api.post(EP.adminStationUpdate(s.sid), { admission: a }), text)) reload(); };
  if (s.admission !== 'approved') items.push({ label: 'Approve', icon: 'check', onClick: setAdm('approved', 'Station approved') });
  if (s.admission === 'approved') items.push({ label: 'Hold (pending)', icon: 'pause', onClick: setAdm('pending', 'Station held') });
  if (s.admission !== 'denied') items.push({ label: 'Deny', icon: 'x-circle', onClick: setAdm('denied', 'Station denied') });
  items.push({ label: 'Rename…', icon: 'edit', onClick: () => rename(s, reload) });
  // Timing quarantine overrides (internal/registry/trust): the gate applies them at once.
  const setTiming = (mode, text) => async () => { if (await act(() => api.post(EP.adminTiming(s.sid), { mode }), text)) reload(); };
  items.push({ label: 'Timing: trust now', icon: 'check', onClick: setTiming('trusted', 'Timing trusted: the station joins MLAT') });
  items.push({ label: 'Timing: judge again', icon: 'refresh', onClick: setTiming('quarantine', 'Station back in timing quarantine') });
  items.push({ label: 'Timing: exclude from MLAT', icon: 'x-circle', onClick: setTiming('excluded', 'Station excluded from MLAT') });
  items.push({ label: 'Timing: automatic', icon: 'history', onClick: setTiming('auto', 'Timing overrides cleared') });
  items.push({ label: 'Reveal exact location…', icon: 'eye', onClick: () => reveal(s) });
  items.push({ label: 'Open station map', icon: 'map', onClick: () => { P.ctx.net.setStation(s.sid); P.ctx.net.setView('station'); P.ctx.navigate('map'); } });
  items.push('sep');
  if (s.feederId) items.push({ label: 'Ban: revoke feeder key…', icon: 'shield', danger: true, onClick: async () => {
    if (!(await ui.confirm({ title: 'Revoke the feeder key?', danger: true, confirmLabel: 'Revoke', message: 'Every station of this feeder is revoked and the key can never connect again. The owner has to create a new identity and be admitted again.' }))) return;
    if (await act(() => api.post(EP.adminRevokeFeeder(s.feederId), {}), 'Feeder revoked')) reload();
  } });
  else if (s.admission !== 'revoked') items.push({ label: 'Ban: revoke station', icon: 'shield', danger: true, onClick: setAdm('revoked', 'Station revoked') });
  items.push({ label: 'Delete station…', icon: 'trash', danger: true, onClick: async () => {
    if (!(await ui.confirm({ title: `Delete ${s.name || s.sid}?`, danger: true, requireText: 'DELETE', confirmLabel: 'Delete', message: 'The station and all its statistics are deleted. A feeder that keeps sending appears again as a new station.' }))) return;
    if (await act(() => api.request('DELETE', EP.adminDelete(s.sid), { body: { reason: 'deleted in the admin console' } }), 'Deletion queued')) reload();
  } });
  ui.menu(anchor, items, { placement: 'bottom-end' });
}

async function rename(s, reload) {
  const { ui, api } = P.ctx;
  const { h } = ui;
  const name = ui.input({ value: s.name || '', maxlength: 40 });
  const dlg = ui.modal({ title: 'Rename station', icon: 'edit', width: 400, body: h('div.col.gap-3', ui.field({ label: 'Name', control: name })), actions: [
    { label: 'Cancel', variant: 'ghost' },
    { label: 'Save', variant: 'primary', value: true },
  ] });
  if ((await dlg.closed) !== true) return;
  if (await act(() => api.post(EP.adminStationUpdate(s.sid), { name: name.value.trim() }), 'Renamed')) reload();
}

async function reveal(s) {
  const { ui, api, units } = P.ctx;
  const { h } = ui;
  if (!(await ui.confirm({ title: 'Reveal the exact location?', confirmLabel: 'Reveal', message: `The exact position (and current address) of ${s.name || s.sid} is shown and the view is recorded in the audit log.` }))) return;
  const raw = await act(() => api.get(EP.adminStation(s.sid)));
  if (!raw || raw === true) return;
  const d = normStationDetail(raw);
  const r = { lat: d.lat, lon: d.lon, alt: d.alt, source: d.locSource, sigmaM: d.sigmaM, ip: (raw.liveLinks || []).map((l) => l.ip).filter(Boolean).join(', ') || raw.ip };
  ui.modal({ title: s.name || s.sid, icon: 'pin', width: 440, body: h('div.col.gap-3', ui.kv([
    ['Position', Number.isFinite(r.lat) ? units.latlon(r.lat, r.lon) : units.DASH],
    ['Height', Number.isFinite(r.alt) ? units.height(r.alt) : units.DASH],
    ['Source', r.source || units.DASH],
    ['Uncertainty', Number.isFinite(r.sigmaM) ? units.height(r.sigmaM) : units.DASH],
    ['Address', r.ip || units.DASH],
  ]), h('p.muted', 'This view was recorded in the audit log.')), actions: [{ label: 'Close', variant: 'primary' }] });
}
