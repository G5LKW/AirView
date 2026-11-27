// net/fedadmin.js - the Federation tab of the network admin console (pages/admin.js; DESIGN §14 phase 5
// "federation between aggregators", internal/federation WIRING.md):
//
//   status    this node's ID (to give to partner operators), TLS pin, listener, received aircraft and the loop /
//             hop / spoof guards (GET /api/admin/federation, refreshed every 10 s)
//   peers     state, direction, RTT, aircraft / MLAT in and out, rejected and dropped records, last error
//   out-feeds federation-safe Beast / JSON feeds to other networks
//   settings  switch federation on, network name, listener, hide remote aircraft, and the peer list with the
//             per-peer sharing policy - saved through PUT /api/config (aggregator.federation). Settings fixed by
//             the operator file or AIRVIEW_* variables are shown locked. Changes rebuild the federation node.
//
// Privacy (shown next to the policy): peers receive at most the public map. Island aircraft are coarsened or
// dropped; feeder identities, station positions, receiver lists, signal levels and IP addresses are never
// shared.

import { ui, h } from '../ui.js';
import { units } from '../units.js';
import { EP } from './endpoints.js';
import { failState, v } from './view.js';
import { pagedTable } from './pager.js';

const STATE_KIND = { connected: 'green', connecting: 'amber', backoff: 'amber', disabled: 'other', idle: 'other', denied: 'red', error: 'red' };
const FED_PATH = 'aggregator.federation';

export function renderFederationAdmin(panel, ctx, act) {
  const { api, config } = ctx;
  const statusHost = h('div');
  const peersHost = h('div');
  const feedsHost = h('div');
  const settingsHost = h('div');
  panel.append(h('div.col.gap-4',
    statusHost,
    ui.card({ title: 'Peers', icon: 'link', sub: 'Other AirView networks this network exchanges its public picture with', body: peersHost }),
    ui.card({ title: 'Out-feeds', icon: 'upload', sub: 'Federation-safe feeds to other networks (Beast with zeroed timestamps and signal, or JSON)', body: feedsHost }),
    settingsHost));
  let peersTable = null;
  let timer = null;

  async function loadStatus() {
    let r;
    try {
      r = await api.get(EP.adminFederation(), { signal: ctx.signal, timeout: 15000 });
    } catch (e) {
      if (!e.aborted) ui.mount(statusHost, failState(e, { retry: loadStatus }));
      return;
    }
    const st = r?.status || null;
    const head = !r?.enabled
      ? h('div.net-note', ui.icon('info'), h('span', 'Federation is off. Switch it on below to exchange the public picture with partner networks.'))
      : r.error ? h('div.net-warn', ui.icon('alert'), h('span', 'Federation could not start: ' + r.error))
        : !r.running ? h('div.net-note', ui.icon('info'), h('span', 'Federation is switched on but not running yet.')) : null;
    const rcv = st?.received || {};
    ui.mount(statusHost, ui.card({ title: 'This node', icon: 'globe', actions: [r?.running ? ui.badge('running', 'green') : ui.badge(r?.enabled ? 'not running' : 'off', 'other')],
      body: h('div.col.gap-3', head,
        st ? ui.kv([
          ['Node ID', h('span.row.gap-2', h('code.net-secret', st.nodeId || '—'), st.nodeId ? ui.button({ icon: 'copy', size: 'sm', variant: 'ghost', tip: 'Copy the node ID', onClick: async () => ((await ui.copy(st.nodeId)) ? ui.toast.success('Node ID copied') : null) }) : null), { text: true }],
          ['Name sent to peers', st.name || '—'],
          ['Listener', st.listen || 'none (outbound only)'],
          ['Running since', st.startedMs ? `${units.dateTime(st.startedMs)} (${units.ago(st.startedMs)})` : '—'],
          ['Transit / max hops', `${st.transit ? 'on' : 'off'} / ${v(st.maxHops)}`],
          ['Remote aircraft held', `${units.int(rcv.aircraft || 0)} (${units.int(rcv.fixes || 0)} MLAT)`],
          ['Exports', units.si(rcv.exports || 0)],
          ['Guards: loops / hop limit / path spoof', `${units.si(rcv.loopsDropped || 0)} / ${units.si(rcv.hopLimitDropped || 0)} / ${units.si(rcv.pathSpoofDropped || 0)}`],
          ['Protocol errors / denied handshakes', `${units.si(rcv.protocolErrors || 0)} / ${units.si(rcv.deniedHandshakes || 0)}`],
        ], { columns: 2 }) : null,
        h('p.muted', 'Give the node ID to partner operators; they add it to their peer list, and you add theirs. ', h('code', 'airview federation id'), ' prints it on the server too.')) }));
    const peers = st?.peers || [];
    if (!peersTable) {
      peersTable = pagedTable({ rows: peers, pageSize: 25, search: (p) => `${p.name} ${p.id} ${p.addr || ''} ${p.peerName || ''}`, placeholder: 'Name, node ID or address',
        empty: r?.enabled ? 'No peers configured.' : 'Federation is off.', sort: { key: 'name', dir: 'asc' },
        columns: [
          { key: 'name', label: 'Peer', sort: true, render: (p) => h('span.col', h('b', p.name || p.peerName || '—'), h('code.muted', (p.id || '').slice(0, 16) + ((p.id || '').length > 16 ? '…' : ''))) },
          { key: 'state', label: 'State', sort: true, render: (p) => h('span.col', ui.badge(p.state || '—', STATE_KIND[p.state] || 'other'), p.direction ? h('span.muted.small', p.direction === 'in' ? 'inbound' : 'outbound') : null) },
          { key: 'addr', label: 'Address', render: (p) => p.addr || h('span.muted', 'accept only') },
          { key: 'rttMs', label: 'RTT', align: 'right', num: true, sort: true, render: (p) => v(p.rttMs, (x) => units.num(x, 0) + ' ms') },
          { key: 'in', label: 'Aircraft in / out', align: 'right', num: true, sort: (p) => p.aircraftIn, render: (p) => `${units.si(p.aircraftIn || 0)} / ${units.si(p.aircraftOut || 0)}` },
          { key: 'mlat', label: 'MLAT in / out', align: 'right', num: true, render: (p) => `${units.si(p.mlatIn || 0)} / ${units.si(p.mlatOut || 0)}` },
          { key: 'remoteAircraft', label: 'Held', align: 'right', num: true, sort: true, render: (p) => units.int(p.remoteAircraft || 0) },
          { key: 'bad', label: 'Rejected / dropped', align: 'right', num: true, sort: (p) => (p.rejected || 0) + (p.dropped || 0), render: (p) => `${units.si(p.rejected || 0)} / ${units.si(p.dropped || 0)}` },
          { key: 'err', label: 'Last error', render: (p) => (p.lastError ? h('span.col', h('span.net-err-text', p.lastError), p.lastErrorMs ? h('span.muted.small', units.ago(p.lastErrorMs)) : null) : '') },
        ] });
      ui.mount(peersHost, peersTable);
    } else peersTable.setRows(peers);
    const feeds = st?.outFeeds || [];
    ui.mount(feedsHost, feeds.length ? ui.table({ rows: feeds, columns: [
      { key: 'name', label: 'Feed', render: (f) => h('b', f.name) },
      { key: 'format', label: 'Format', render: (f) => `${f.format} · ${f.mode}` },
      { key: 'addr', label: 'Address', render: (f) => h('code', f.addr || '—') },
      { key: 'state', label: 'State', render: (f) => (f.mode === 'listen' ? `${units.int(f.clients || 0)} clients` : ui.badge(f.connected ? 'connected' : 'not connected', f.connected ? 'green' : 'amber')) },
      { key: 'aircraft', label: 'Aircraft', align: 'right', num: true, render: (f) => units.int(f.aircraft || 0) },
      { key: 'out', label: 'Sent', align: 'right', num: true, render: (f) => units.bytes ? units.bytes(f.bytesOut || 0) : units.si(f.bytesOut || 0) },
      { key: 'dropped', label: 'Dropped', align: 'right', num: true, render: (f) => units.si(f.dropped || 0) },
      { key: 'err', label: 'Last error', render: (f) => f.lastError || '' },
    ] }) : ui.empty({ icon: 'upload', title: 'No out-feeds', text: 'Out-feeds are configured in aggregator.federation.feeds of the operator file.', compact: true }));
  }

  function drawSettings() {
    const cfg = config.current;
    if (!cfg) { ui.mount(settingsHost, ui.loading('Loading the configuration…')); config.load().then(drawSettings).catch((e) => ui.mount(settingsHost, failState(e, { retry: drawSettings }))); return; }
    const fed = cfg.aggregator?.federation || {};
    const lockedPaths = (cfg.locked || []).filter((x) => x.path === FED_PATH || x.path.startsWith(FED_PATH + '.'));
    const locked = (path) => lockedPaths.some((x) => x.path === path || path.startsWith(x.path + '.') || x.path.startsWith(path + '.'));
    const peersLocked = locked(FED_PATH + '.peers');
    const enabled = ui.toggle({ label: 'Federation on', checked: !!fed.enabled, disabled: locked(FED_PATH + '.enabled') });
    const hide = ui.toggle({ label: 'Keep received aircraft off the map', checked: !!fed.hideRemote, disabled: locked(FED_PATH + '.hideRemote') });
    const name = ui.input({ value: fed.name || '', placeholder: 'e.g. North Valley Network', disabled: locked(FED_PATH + '.name') });
    const listen = ui.input({ value: fed.listen || '', placeholder: ':30170 (empty = no inbound peers)', disabled: locked(FED_PATH + '.listen') });
    let peers = (fed.peers || []).map((p) => structuredClone(p));
    const peerHost = h('div');
    const drawPeers = () => ui.mount(peerHost, peers.length ? ui.table({ rows: peers.map((p, i) => ({ ...p, i })), columns: [
      { key: 'name', label: 'Peer', render: (p) => h('span.col', h('b', p.name || '—'), h('code.muted', (p.id || '').slice(0, 20))) },
      { key: 'addr', label: 'Dial', render: (p) => p.addr || h('span.muted', 'accept only') },
      { key: 'send', label: 'We share', render: (p) => policyText(p.send) },
      { key: 'recv', label: 'We accept', render: (p) => `aircraft ${p.receive?.aircraft || 'all'} · MLAT ${p.receive?.mlat || 'all'}` },
      { key: 'on', label: '', render: (p) => (p.disabled ? ui.badge('disabled', 'other') : '') },
      { key: 'act', label: '', render: (p) => (peersLocked ? '' : h('span.row.gap-1',
        ui.button({ icon: 'edit', size: 'sm', variant: 'ghost', tip: 'Edit', onClick: async () => { const x = await peerDialog(p); if (x) { peers[p.i] = x; drawPeers(); } } }),
        ui.button({ icon: 'trash', size: 'sm', variant: 'ghost', tip: 'Remove', onClick: () => { peers.splice(p.i, 1); drawPeers(); } }))) },
    ] }) : ui.empty({ icon: 'link', title: 'No peers', text: 'Add the node ID and address a partner operator gave you.', compact: true }));
    drawPeers();
    const add = ui.button({ label: 'Add peer', icon: 'plus', size: 'sm', disabled: peersLocked, onClick: async () => { const x = await peerDialog({ send: {}, receive: {} }); if (x) { peers.push(x); drawPeers(); } } });
    const save = ui.button({ label: 'Save federation settings', icon: 'save', variant: 'primary', onClick: async () => {
      save.setLoading(true);
      const ok = await act(() => config.save((c) => {
        delete c.locked;
        const g = c.aggregator || (c.aggregator = {});
        const f = g.federation || (g.federation = {});
        if (!locked(FED_PATH + '.enabled')) f.enabled = enabled.checked;
        if (!locked(FED_PATH + '.hideRemote')) f.hideRemote = hide.checked;
        if (!locked(FED_PATH + '.name')) f.name = name.value.trim();
        if (!locked(FED_PATH + '.listen')) f.listen = listen.value.trim();
        if (!peersLocked) f.peers = peers;
      }), 'Federation settings saved: the node restarts with them');
      save.setLoading(false);
      if (ok) { drawSettings(); setTimeout(loadStatus, 1500); }
    } });
    const fld = (label, control, path, hint) => ui.field({ label, control, hint: locked(path) ? `Fixed by ${lockedPaths.find((x) => path.startsWith(x.path) || x.path.startsWith(path))?.origin || 'the operator configuration'} - change it there.` : hint });
    ui.mount(settingsHost, ui.card({ title: 'Settings', icon: 'settings', sub: 'aggregator.federation · every setting is also available in the operator file and as AIRVIEW_AGGREGATOR_FEDERATION_* variables',
      body: h('div.col.gap-3',
        h('div.grid.cols-2.net-grid',
          h('div.col.gap-3', enabled, hide),
          h('div.col.gap-3', fld('Network name for peers', name, FED_PATH + '.name', 'Shown to partner operators.'),
            fld('Listener', listen, FED_PATH + '.listen', 'host:port for inbound peers (TCP 30170 is the convention); open it in the firewall only when set.'))),
        h('div.row', h('b.grow', 'Peers'), add), peerHost,
        h('div.net-note', ui.icon('shield'), h('span', 'Peers receive at most the public map. Aircraft the island guard protects are coarsened or dropped; feeder identities, station positions, receiver lists, signal levels and IP addresses are never shared.')),
        lockedPaths.length ? h('p.muted', `${lockedPaths.length} federation setting${lockedPaths.length === 1 ? ' is' : 's are'} fixed by the operator configuration.`) : null,
        h('div.row', save)) }));
  }

  function policyText(s = {}) {
    const parts = [`aircraft ${s.aircraft || 'all'}`];
    if (s.mlat) parts.push('MLAT');
    parts.push(`island ${s.island || 'coarsen'}`);
    if (s.minAltFt) parts.push(`≥ ${s.minAltFt} ft`);
    if (s.noGround) parts.push('no ground');
    if (s.noMilitary) parts.push('no military');
    if (s.delayS) parts.push(`${s.delayS} s delay`);
    if (s.boxes?.length) parts.push(`${s.boxes.length} area${s.boxes.length === 1 ? '' : 's'}`);
    return parts.join(' · ');
  }

  async function peerDialog(p) {
    const s = p.send || {};
    const r = p.receive || {};
    const name = ui.input({ value: p.name || '', placeholder: 'Local label' });
    const id = ui.input({ value: p.id || '', placeholder: 'Node ID from the partner operator' });
    const addr = ui.input({ value: p.addr || '', placeholder: 'host:30170 (empty = only accept this peer)' });
    const pin = ui.input({ value: p.pin || '', placeholder: 'sha256/… or webpki (optional)' });
    const disabled = ui.toggle({ label: 'Disabled (keep the entry, no connection)', checked: !!p.disabled });
    const sAir = ui.select({ value: s.aircraft || 'all', options: [{ value: 'all', label: 'All aircraft' }, { value: 'nomlat', label: 'Without MLAT positions' }, { value: 'none', label: 'Nothing' }] });
    const sMlat = ui.toggle({ label: 'Share MLAT results', checked: !!s.mlat });
    const sIsland = ui.select({ value: s.island || 'coarsen', options: [{ value: 'coarsen', label: 'Coarsen island aircraft' }, { value: 'drop', label: 'Drop island aircraft' }] });
    const sMin = ui.input({ type: 'number', value: s.minAltFt || 0, min: 0, max: 60000, suffix: 'ft' });
    const sGround = ui.toggle({ label: 'Not on the ground', checked: !!s.noGround });
    const sMil = ui.toggle({ label: 'No military aircraft', checked: !!s.noMilitary });
    const sDelay = ui.input({ type: 'number', value: s.delayS || 0, min: 0, max: 600, suffix: 's' });
    const rAir = ui.select({ value: r.aircraft || 'all', options: [{ value: 'all', label: 'Accept aircraft' }, { value: 'none', label: 'Accept no aircraft' }] });
    const rMlat = ui.select({ value: r.mlat || 'all', options: [{ value: 'all', label: 'Accept MLAT' }, { value: 'none', label: 'Accept no MLAT' }] });
    const fId = ui.field({ label: 'Node ID', control: id });
    const dlg = ui.modal({ title: p.id ? 'Edit peer' : 'Add peer', icon: 'link', width: 620, body: h('div.col.gap-3',
      h('div.grid.cols-2.net-grid', ui.field({ label: 'Label', control: name }), fId, ui.field({ label: 'Address to dial', control: addr }), ui.field({ label: 'TLS pin', control: pin })),
      disabled,
      h('b', 'What we share with this peer'),
      h('div.grid.cols-2.net-grid', ui.field({ label: 'Aircraft', control: sAir }), ui.field({ label: 'Island aircraft', control: sIsland }),
        ui.field({ label: 'Lowest altitude', control: sMin }), ui.field({ label: 'Delay', control: sDelay })),
      h('div.row.gap-3', { style: 'flex-wrap:wrap' }, sMlat, sGround, sMil),
      h('b', 'What we accept'),
      h('div.grid.cols-2.net-grid', rAir, rMlat),
      h('p.muted', 'Area boxes, caps and transit are set in the operator file (aggregator.federation.peers[].send / receive); they are kept as they are.')),
    actions: [{ label: 'Cancel', variant: 'ghost' }, { label: p.id ? 'Apply' : 'Add', variant: 'primary', value: true, onClick: () => {
      fId.setError(null);
      if (!/^[a-z2-7]{16}$/.test(id.value.trim().toLowerCase())) { fId.setError('A node ID has 16 characters (a-z, 2-7), as "airview federation id" prints it.'); return false; }
      return undefined;
    } }] });
    if ((await dlg.closed) !== true) return null;
    const num = (x, lo, hi) => Math.min(hi, Math.max(lo, Number(x.value) || 0));
    const out = { ...structuredClone(p), name: name.value.trim(), id: id.value.trim().toLowerCase(), addr: addr.value.trim(), pin: pin.value.trim(), disabled: disabled.checked };
    out.send = { ...(p.send || {}), aircraft: sAir.value, mlat: sMlat.checked, island: sIsland.value, minAltFt: num(sMin, 0, 60000), noGround: sGround.checked, noMilitary: sMil.checked, delayS: num(sDelay, 0, 600) };
    out.receive = { ...(p.receive || {}), aircraft: rAir.value, mlat: rMlat.value };
    for (const k of ['addr', 'pin']) if (!out[k]) delete out[k];
    if (!out.disabled) delete out.disabled;
    delete out.i;
    return out;
  }

  loadStatus();
  drawSettings();
  timer = setInterval(() => { if (!panel.isConnected) { clearInterval(timer); return; } if (!document.hidden && panel.offsetParent !== null) loadStatus(); }, 10000);
}
