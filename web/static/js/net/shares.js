// net/shares.js - share links (WP-H2; DESIGN §8.2 "My map … share links in phase 4", §14 phase 4: "revocable and
// scoped, with a location warning"):
//
//   renderMyShares(container, ctx)        #/me/shares: the owner's links (state, stations, expiry, last use),
//                                         "New share link" and "Revoke"
//   openCreateShare(ctx, { sids, done })  the create dialog: stations, label, what is shared, validity, low-
//                                         aircraft rounding and the location warning the owner must confirm;
//                                         afterwards the link (shown once) with Copy and a QR code
//   renderAdminShares(panel, ctx, act)    network admin tab "Share links": every link with owner, search, state
//                                         filter, server paging and revoke
//
// What a link shows (internal/server/api_share.go): the aircraft the chosen stations heard in the last 60 s in the
// public shape (no receiver data), and for "map + statistics" the stations' non-locating statistics. Never the
// station position, pin, SID, range or coverage. The secret is shown once; the server keeps only its hash.

import { ui, h } from '../ui.js';
import { units } from '../units.js';
import { EP } from './endpoints.js';
import { pageHead, failState, loadInto } from './view.js';
import { pagedTable, pagerBar } from './pager.js';
import { qrSvg } from './qr.js';
import { shortSid } from './model.js';

export const SHARE_SCOPES = [
  { value: 'map', label: 'Live aircraft', hint: 'The aircraft your stations heard in the last minute, on a map and in a list.' },
  { value: 'map+stats', label: 'Live aircraft + statistics', hint: 'Also message rate, aircraft count and uptime of the stations (no position, no range).' },
];
const DAYS = [1, 7, 30, 90, 365];
const STATE_KIND = { active: 'green', revoked: 'red', expired: 'other' };
const FALLBACK_WARNING = 'Anyone who has this link sees the aircraft your station receives, live. Where those aircraft are, and especially the low ones, shows roughly where your antenna is. Share it only with people you would tell your location to. You can revoke the link at any time.';

/** The full link for a share path ("/#/share/<token>"). */
export function shareURL(path) {
  return location.origin + (path || '');
}

const stationNames = (l) => (l.stations || []).map((s) => (s.name || shortSid(s.sid)) + (s.owned === false ? ' (no longer yours)' : '')).join(', ');

export function renderMyShares(container, ctx) {
  const { api } = ctx;
  const host = h('div');
  const head = pageHead('Share links', 'Read-only live views of your stations for people without an account',
    ui.button({ label: 'New share link', icon: 'plus', variant: 'primary', onClick: () => openCreateShare(ctx, { done: () => reload(true) }) }),
    ui.button({ label: 'My stations', icon: 'antenna', href: '#/me/stations' }));
  container.append(h('div.page-inner', head, host));
  let table = null;
  const reload = loadInto(host, () => api.get(EP.myShares(), { signal: ctx.signal }), (r) => {
    const links = r?.links || [];
    const active = links.filter((l) => l.state === 'active').length;
    head.setSub(`${units.int(active)} of ${units.int(r?.max || 20)} active links · revoked and expired links stay listed for 30 days`);
    const warn = h('div.net-warn', ui.icon('alert'), h('span', r?.warning || FALLBACK_WARNING));
    table = pagedTable({ rows: links, pageSize: 25, search: (l) => `${l.label} ${stationNames(l)} ${l.id}`, placeholder: 'Label or station',
      empty: 'No share links yet. A link lets someone without an account follow what your station receives.',
      sort: { key: 'created', dir: 'desc' },
      columns: [
        { key: 'label', label: 'Link', sort: (l) => l.label || '', render: (l) => h('span.col', h('b', l.label || 'Untitled link'), h('span.muted.small', stationNames(l))) },
        { key: 'scope', label: 'Shows', render: (l) => (SHARE_SCOPES.find((s) => s.value === l.scope) || {}).label || l.scope },
        { key: 'state', label: 'State', sort: true, render: (l) => ui.badge(l.state, STATE_KIND[l.state] || 'other') },
        { key: 'created', label: 'Created', sort: true, sortDir: 'desc', render: (l) => units.date(l.created) },
        { key: 'expires', label: 'Valid until', sort: true, render: (l) => (l.state === 'revoked' ? `revoked ${units.ago(l.revoked)}` : units.dateTime(l.expires)) },
        { key: 'lastUsed', label: 'Last opened', sort: true, sortDir: 'desc', render: (l) => (l.lastUsed ? `${units.ago(l.lastUsed)} · ${units.int(l.uses)}×` : 'never') },
        { key: 'act', label: '', render: (l) => (l.state === 'active' ? ui.button({ label: 'Revoke', size: 'sm', variant: 'ghost', onClick: async () => {
          if (!(await ui.confirm({ title: 'Revoke this link?', message: 'Everyone who has it loses access at once. You can create a new link later.', confirmLabel: 'Revoke', danger: true }))) return;
          try { await api.del(EP.myShare(l.id)); ui.toast.success('Link revoked'); reload(true); } catch (e) { ui.toast.error(e.message); }
        } }) : '') },
      ] });
    return h('div.col.gap-3', warn, ui.card({ title: 'Your links', icon: 'share', body: table }),
      h('p.muted', 'The full link is shown only once, when you create it. If you lost it, revoke the link and create a new one.'));
  });
}

/**
 * The create dialog. opts: { sids (preselected), done() }.
 */
export async function openCreateShare(ctx, opts = {}) {
  const { api, net } = ctx;
  const own = (net.stations || []).filter((s) => s.own);
  if (!own.length) { ui.alert('No stations', 'Claim a station first: share links show what your own stations receive.'); return; }
  const pre = new Set(opts.sids && opts.sids.length ? opts.sids : own.length === 1 ? [own[0].sid] : []);
  const checks = own.map((s) => ({ s, c: ui.checkbox({ label: s.name || shortSid(s.sid), checked: pre.has(s.sid) }) }));
  const label = ui.input({ placeholder: 'e.g. For the club evening', maxlength: 60 });
  const scope = ui.segmented({ block: true, value: 'map', options: SHARE_SCOPES.map((o) => ({ value: o.value, label: o.label, tip: o.hint })) });
  const scopeHint = h('div.field-hint', SHARE_SCOPES[0].hint);
  scope.addEventListener('click', () => ui.setText(scopeHint, (SHARE_SCOPES.find((o) => o.value === scope.value) || SHARE_SCOPES[0]).hint));
  // The network's limits (aggregator.shareLinks): longest validity, default validity, low-aircraft default.
  let lim = null;
  try { lim = await api.get(EP.myShares()); } catch (e) {
    if (e && e.status === 404) { ui.alert('Share links are off', 'The operator of this network has switched share links off.'); return; }
  }
  const maxDays = Number(lim?.maxDays) || 365;
  const defDays = Math.min(Number(lim?.defaultDays) || 30, maxDays);
  const dayOpts = [...new Set([...DAYS.filter((d) => d <= maxDays), defDays, maxDays])].sort((a, b) => a - b);
  const days = ui.select({ value: String(defDays), options: dayOpts.map((d) => ({ value: String(d), label: d === 1 ? '1 day' : d === 365 ? '1 year' : `${d} days` })) });
  const coarse = ui.toggle({ label: 'Round low aircraft and aircraft on the ground to about 5 km (recommended)', checked: lim?.coarseLowDefault !== false });
  const ack = ui.checkbox({ label: 'I understand that the link reveals roughly where my station is.' });
  const warning = lim?.warning || FALLBACK_WARNING;
  const warnText = h('span', warning);
  const fSt = ui.field({ label: 'Stations', control: h('div.col.gap-1', checks.map((x) => x.c)) });
  const fAck = ui.field({ control: ack });
  const dlg = ui.modal({ title: 'New share link', icon: 'share', width: 560, body: h('div.col.gap-3',
    fSt,
    ui.field({ label: 'Label (only you see it)', control: label }),
    ui.field({ label: 'What the link shows', control: h('div.col.gap-1', scope, scopeHint) }),
    ui.field({ label: 'Valid for', control: days, hint: 'You can revoke the link earlier at any time.' }),
    coarse,
    h('div.net-warn', ui.icon('alert'), warnText),
    fAck),
  actions: [{ label: 'Cancel', variant: 'ghost' }, { label: 'Create link', variant: 'primary', icon: 'share', onClick: async () => {
    fSt.setError(null);
    fAck.setError(null);
    const sids = checks.filter((x) => x.c.checked).map((x) => x.s.sid);
    if (!sids.length) { fSt.setError('Choose at least one station.'); return false; }
    if (!ack.checked) { fAck.setError('Please confirm that you read the warning.'); return false; }
    try {
      const r = await api.post(EP.myShares(), { stations: sids, label: label.value.trim(), scope: scope.value, days: Number(days.value), coarseLow: coarse.checked, acknowledge: true });
      dlg.close(true);
      showCreated(r);
      opts.done?.();
    } catch (e) {
      fAck.setError(e.message || 'The link could not be created.');
    }
    return false;
  } }] });
  return dlg.closed;
}

function showCreated(r) {
  const url = shareURL(r?.path);
  const qr = h('div.net-qr');
  try { qr.innerHTML = qrSvg(url, { ecl: 'M', scale: 4, margin: 4, title: 'QR code of the share link' }); } catch { qr.append(h('p.muted', 'No QR code for this link.')); }
  ui.modal({ title: 'Share link created', icon: 'share', width: 560, dismissable: false, body: h('div.col.gap-3',
    h('div.net-recovery-warn', ui.icon('alert'), h('span', 'Copy the link now. It is shown only this once.')),
    h('code.net-token.net-share-url', url),
    h('div.row.gap-2', { style: 'flex-wrap:wrap' },
      ui.button({ label: 'Copy link', icon: 'copy', size: 'sm', variant: 'primary', onClick: async () => ((await ui.copy(url)) ? ui.toast.success('Link copied') : ui.toast.warn('Copy failed - select the link and copy it by hand')) }),
      ui.button({ label: 'Open', icon: 'external', size: 'sm', onClick: () => window.open(url, '_blank', 'noopener') })),
    h('div.net-share-qr', qr, h('p.muted', 'Scan to open the link on a phone.'))),
  actions: [{ label: 'Done', variant: 'primary' }] });
}

export function renderAdminShares(panel, ctx, act) {
  const { api } = ctx;
  const f = { q: '', state: '' };
  let page = { offset: 0, limit: 50 };
  const search = ui.input({ placeholder: 'Label, owner, station ID', icon: 'search', size: 'sm', clearable: true, onInput: ui.debounce((x) => { f.q = x; page.offset = 0; load(); }, 300) });
  const state = ui.select({ size: 'sm', value: '', options: [{ value: '', label: 'Any state' }, { value: 'active', label: 'Active' }, { value: 'revoked', label: 'Revoked' }, { value: 'expired', label: 'Expired' }],
    onChange: (x) => { f.state = x; page.offset = 0; load(); } });
  const count = h('span.muted');
  const host = h('div');
  const pager = pagerBar({ pageSize: page.limit, onChange: (s) => { page = { offset: s.offset, limit: s.limit }; load(); } });
  panel.append(h('div.col.gap-3',
    h('p.muted', 'Owners hand out read-only live views of their stations. Revoke a link that is misused; the owner sees who revoked it.'),
    h('div.row.gap-2.net-filters', search, state, h('span.grow'), count, ui.button({ icon: 'refresh', size: 'sm', variant: 'ghost', tip: 'Reload', onClick: () => load() })),
    host, pager));
  let seq = 0;
  async function load() {
    const my = ++seq;
    if (!host.firstChild) ui.mount(host, ui.skeleton({ lines: 5 }));
    try {
      const r = await api.get(EP.adminShares(), { params: { q: f.q || undefined, state: f.state || undefined, offset: page.offset, limit: page.limit }, signal: ctx.signal });
      if (my !== seq) return;
      const c = r?.counts || {};
      ui.setText(count, `${units.int(c.active || 0)} active · ${units.int(c.revoked || 0)} revoked · ${units.int(c.expired || 0)} expired`);
      pager.set({ total: r?.total || 0, offset: page.offset, limit: page.limit });
      ui.mount(host, ui.table({ rows: r?.links || [], empty: 'No share links match', columns: [
        { key: 'label', label: 'Link', render: (l) => h('span.col', h('b', l.label || 'Untitled link'), h('code.muted', l.id)) },
        { key: 'owner', label: 'Owner', render: (l) => l.owner || `#${l.ownerId}` },
        { key: 'stations', label: 'Stations', render: (l) => h('span.col', (l.stations || []).map((s) => h('span', s.name || shortSid(s.sid), h('code.muted', ' ' + s.sid), s.owned === false ? h('span.muted', ' (not owned)') : null))) },
        { key: 'scope', label: 'Shows', render: (l) => (SHARE_SCOPES.find((s) => s.value === l.scope) || {}).label || l.scope },
        { key: 'state', label: 'State', render: (l) => h('span.col', ui.badge(l.state, STATE_KIND[l.state] || 'other'), l.revokedBy ? h('span.muted.small', 'by ' + l.revokedBy) : null) },
        { key: 'expires', label: 'Valid until', render: (l) => units.dateTime(l.expires) },
        { key: 'used', label: 'Opened', render: (l) => (l.lastUsed ? `${units.int(l.uses)}× · ${units.ago(l.lastUsed)}` : 'never') },
        { key: 'act', label: '', render: (l) => (l.state === 'active' ? ui.button({ label: 'Revoke', size: 'sm', variant: 'ghost', onClick: async () => {
          if (!(await ui.confirm({ title: 'Revoke this link?', message: `The link of ${l.owner || 'this owner'} stops working at once. The action is recorded in the audit log.`, confirmLabel: 'Revoke', danger: true }))) return;
          if (await act(() => api.del(EP.adminShare(l.id)), 'Link revoked')) load();
        } }) : '') },
      ] }));
    } catch (e) {
      if (my !== seq || e.aborted) return;
      ui.mount(host, failState(e, { retry: load }));
    }
  }
  load();
}
