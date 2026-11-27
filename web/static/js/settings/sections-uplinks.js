// settings/sections-uplinks.js - Settings > Network uplinks: this station feeding one or more AirView
// aggregators over AVF/1 (DESIGN §12.4, internal/uplink). Edits `uplinks[]` of model.Config (host, port,
// server pin, enrol token as a write-only secret, inputs, sendLocation, shareGnss, timestamps, returnMlat,
// spoolMb, disabled, pin / publicStats / showName) and shows a live status card per uplink from
// GET /api/uplinks (state, error, retry, approval, MOTD, notices, buffer, counters, feedback per receiver)
// with "Claim code" and "Retry now" (POST /api/uplinks). Before timestamps are turned on for the first time
// the consent text of DESIGN §10.5 is shown.

import { CONSENT_TEXT } from '../net/view.js';
import { PIN_OPTIONS, clockLabel } from '../net/model.js';

const SPOOL_AUTO = -1;
const consented = new Set(); // uplink ids whose consent text was accepted in this browser session (never saved in the config)
const STATE_KIND = { connected: 'ok', handshake: 'new', connecting: 'new', backoff: 'bad', stopped: 'bad', disabled: 'off', idle: 'off' };
const ERROR_TEXT = {
  enrol_required: 'The aggregator needs an enrol token: ask its operator for one and enter it below.',
  enrol_invalid: 'The enrol token was refused (wrong, used up or expired).',
  revoked: 'The aggregator revoked this feeder.',
  cloned_feeder: 'Another computer uses the same feeder identity. Create a new identity on one of them (airview-feeder new-identity).',
  proto_version: 'The aggregator speaks another protocol version: update AirView.',
};

/** Consent text C9 (docs/aggregator/CONSENT.md): shown next to a Beast output. */
const CONSENT_C9 = 'Beast connections are not encrypted. Your receiver data and this UUID go to that aggregator\'s operator under their terms. AirView never forwards MLAT results to them.';
const BEAST_FORMATS = [
  { value: 'beast_reduce_plus_out', label: 'beast_reduce_plus_out (reduced, with UUID)' },
  { value: 'beast_reduce_out', label: 'beast_reduce_out (reduced)' },
  { value: 'beast_out', label: 'beast_out (every frame)' },
];

/** Consent text of an MLAT output (shown next to it and before a preset is added). */
const CONSENT_MLAT = 'MLAT needs your receiver position and timestamps: both, this UUID and the display name go to that network\'s mlat server (unencrypted) under their terms. "Hide on the public map" asks the network not to show the station. MLAT positions come back to this map; AirView never forwards them anywhere.';

/**
 * Community networks that take Beast data and run an mlat server (feeder settings as the networks published
 * them; check the network's own feeder page if a connection fails). Each preset adds a Beast output and an MLAT
 * output sharing one UUID, so the network joins both links into one station.
 */
export const NETWORK_PRESETS = [
  { name: 'adsb.lol', beast: ['feed.adsb.lol', 30004], mlat: ['feed.adsb.lol', 31090], url: 'https://adsb.lol/' },
  { name: 'adsb.fi', beast: ['feed.adsb.fi', 30004], mlat: ['feed.adsb.fi', 31090], url: 'https://adsb.fi/' },
  { name: 'airplanes.live', beast: ['feed.airplanes.live', 30004], mlat: ['feed.airplanes.live', 31090], url: 'https://airplanes.live/' },
  { name: 'ADS-B Exchange', beast: ['feed1.adsbexchange.com', 30004], mlat: ['feed.adsbexchange.com', 31090], url: 'https://www.adsbexchange.com/' },
  { name: 'The Air Traffic', beast: ['feed.theairtraffic.com', 30004], mlat: ['feed.theairtraffic.com', 31090], url: 'https://theairtraffic.com/' },
];

function nextId(existing, base) {
  const ids = new Set((existing || []).map((u) => u.id));
  let n = 1;
  while (ids.has(`${base}${n}`)) n++;
  return `${base}${n}`;
}

/** A new MLAT output (uplinks[] with type "mlat": the mlat-client protocol). */
export function newMLATUplink(existing, host = '', port = 31090, uuid = '') {
  return { id: nextId(existing, 'mlat'), type: 'mlat', host, port, user: '', uuid, inputs: [], timestamps: true, sendLocation: true, returnMlat: true, spoolMb: SPOOL_AUTO, pin: 'hidden', disabled: false };
}

/** A new Beast output ("also feed others", uplinks[] with type "beast"). */
export function newBeastUplink(existing) {
  let n = 1;
  const ids = new Set((existing || []).map((u) => u.id));
  while (ids.has(`beast${n}`)) n++;
  return { id: `beast${n}`, type: 'beast', host: '', port: 30004, format: 'beast_reduce_plus_out', uuid: '', reduceMs: 0, timestamps: true, sendLocation: true, returnMlat: true, spoolMb: SPOOL_AUTO, pin: 'hidden', disabled: false };
}

/** A new uplink with the defaults of model.DefaultUplink. */
export function newUplink(existing) {
  let n = 1;
  const ids = new Set((existing || []).map((u) => u.id));
  while (ids.has(`net${n}`)) n++;
  return { id: `net${n}`, host: '', port: 30160, serverPin: '', enrolToken: '', sendLocation: true, shareGnss: false, timestamps: true, returnMlat: true, spoolMb: SPOOL_AUTO, pin: 'hidden', publicStats: false, showName: false, disabled: false };
}

export function renderUplinks(S, F) {
  const { ui, units, api } = S.ctx;
  const { h } = ui;
  if (!Array.isArray(S.draft.uplinks)) S.draft.uplinks = [];
  const list = () => S.draft.uplinks;
  // a new uplink starts with timestamps off until the owner has read the consent text (DESIGN §10.5)
  const askConsent = (u) => !consented.has(u.id) && !savedIds.has(u.id);
  const structural = () => { S.touch(); S.rerender(); };
  const savedIds = new Set((S.saved.uplinks || []).map((u) => u.id));
  const feeds = (S.draft.feeds || []).filter((f) => f.type !== 'demo' && f.type !== 'hub');

  // ---- live status ----
  const statusHost = new Map(); // uplink id -> element
  let status = null;
  async function loadStatus() {
    try {
      status = await api.get('/api/uplinks', { signal: S.scope.signal, timeout: 8000 });
    } catch (e) {
      if (e.aborted) return;
      status = { error: e };
    }
    paintStatus();
  }
  function paintStatus() {
    for (const [id, el] of statusHost) {
      if (!savedIds.has(id)) { ui.mount(el, h('div.set-help', 'New - connects after you save.')); continue; }
      if (!status) { ui.mount(el, ui.skeleton({ lines: 2 })); continue; }
      if (status.error) {
        ui.mount(el, h('div.set-help', status.error.status === 503 ? 'The uplink service is not running in this build.' : status.error.status === 404 ? 'This AirView does not report uplink status yet.' : `Status unavailable: ${status.error.message}`));
        continue;
      }
      const u = (status.uplinks || []).find((x) => x.id === id);
      const b = u ? null : (status.beast || []).find((x) => x.id === id);
      const ml = u || b ? null : (status.mlat || []).find((x) => x.id === id);
      ui.mount(el, u ? statusCard(u) : b ? beastCard(b) : ml ? mlatCard(ml) : h('div.set-help', 'No status yet.'));
    }
  }
  function statusCard(u) {
    const c = u.counters || {};
    const b = u.buffer || {};
    const kind = STATE_KIND[u.state] || 'off';
    const now = S.ctx.live.serverNow();
    const err = u.errorCode ? ERROR_TEXT[u.errorCode] || u.lastError : u.lastError;
    return h('div.col.gap-3',
      h('div.row.gap-2', h('i.set-dot.' + kind), h('b', u.state || 'unknown'), u.sinceMs ? h('span.muted', `since ${units.ago(u.sinceMs)}`) : null,
        u.approval === 'pending' ? ui.badge('waiting for approval', 'amber') : u.approval === 'approved' ? ui.badge('approved', 'green') : null,
        u.mode ? ui.badge(u.mode, 'blue') : null, u.mlat ? ui.badge('MLAT', 'mlat') : null),
      err ? F.callout(u.state === 'stopped' ? 'bad' : 'warn', 'alert', err, u.retryAtMs && u.retryAtMs > now ? ` Next attempt in ${units.age((u.retryAtMs - now) / 1000)}.` : '') : null,
      u.motd ? F.callout('info', 'info', h('b', u.serverName || 'Aggregator'), ': ', u.motd) : null,
      ...(u.notices || []).slice(-3).map((n) => F.callout('info', 'info', n.text || n.message || JSON.stringify(n))),
      ui.kv([
        ['Aggregator', u.serverName || `${u.host}:${u.port}`],
        ['Feeder ID', u.feederId ? h('code', u.feederId) : units.DASH],
        ['Frames sent', `${units.int(c.framesSent)}${c.framesLate ? ` (${units.int(c.framesLate)} late)` : ''}`],
        ['MLAT frames refused', units.int(c.mlatRefused), { tip: 'Frames that already carry MLAT positions are never forwarded' }],
        ['MLAT results', `${units.int(c.resultsIn)} received · ${units.int(c.resultsInjected)} on the map`],
        ['Buffer', `${units.bytes(b.ringBytes || 0)} RAM (${units.age(b.ringSpanS || 0)})${b.spoolMb ? ` · ${units.bytes(b.spoolBytes || 0)} of ${b.spoolMb} MB spool` : ''}`],
        ['Lost (buffers full)', units.int(c.droppedRecords)],
      ], { columns: 2 }),
      (u.receivers || []).length ? ui.table({ columns: [
        { key: 'rxNo', label: 'Receiver' },
        { key: 'feedId', label: 'Feed' },
        { key: 'clock', label: 'Clock', render: (r) => clockLabel(r.clockClass) },
        { key: 'mode', label: 'Mode' },
        { key: 'fb', label: 'Feedback', render: (r) => (r.feedback ? Object.entries(r.feedback).map(([k, v]) => `${k} ${typeof v === 'number' ? units.num(v, 1) : v}`).join(' · ') : units.DASH) },
      ], rows: u.receivers }) : null,
      h('div.row-wrap',
        ui.button({ label: 'Claim code', icon: 'claim', size: 'sm', tip: 'Get a single-use code to claim this receiver in your account on the aggregator', onClick: () => claim(u) }),
        u.state === 'stopped' || u.state === 'backoff' ? ui.button({ label: 'Retry now', icon: 'refresh', size: 'sm', onClick: () => retry(u) }) : null));
  }
  function beastCard(b) {
    const kind = STATE_KIND[b.state] || 'off';
    return h('div.col.gap-3',
      h('div.row.gap-2', h('i.set-dot.' + kind), h('b', b.state || 'unknown'), ui.badge(b.format || 'beast', 'blue')),
      b.lastError && b.state !== 'connected' ? F.callout('warn', 'alert', b.lastError) : null,
      ui.kv([
        ['Frames sent', units.int(b.framesSent)],
        ['Reduced', units.int(b.reduced)],
        ['Dropped while down', units.int(b.dropped)],
        ['MLAT results refused', units.int(b.mlatRefused), { tip: 'MLAT results are never forwarded to other aggregators' }],
        ['Ping echoes', units.int(b.pingsEchoed)],
        ['Slow-down requests', units.int(b.slowDowns)],
      ], { columns: 2 }),
      b.uuid ? h('div.row-wrap', h('span.muted', 'UUID'), h('code', b.uuid),
        ui.button({ label: 'Copy', icon: 'copy', size: 'sm', onClick: async () => ((await ui.copy(b.uuid)) ? ui.toast.success('UUID copied') : null) }),
        h('span.set-help', 'Use the same UUID for an mlat-client to this aggregator, so it joins both links into one station.')) : null,
      b.state === 'backoff' ? h('div.row-wrap', ui.button({ label: 'Retry now', icon: 'refresh', size: 'sm', onClick: () => retry(b) })) : null);
  }
  function mlatCard(m) {
    const kind = STATE_KIND[m.state] || 'off';
    return h('div.col.gap-3',
      h('div.row.gap-2', h('i.set-dot.' + kind), h('b', m.state || 'unknown'), ui.badge('MLAT', 'mlat'), m.compress ? ui.badge(m.compress, 'blue') : null),
      (m.denied || []).length ? F.callout('bad', 'alert', 'The server refused the connection: ', m.denied.join('; ')) : m.lastError && m.state !== 'connected' ? F.callout('warn', 'alert', m.lastError) : null,
      m.motd ? F.callout('info', 'info', m.motd) : null,
      ui.kv([
        ['Aircraft announced', units.int(m.seen)],
        ['Requested by the server', units.int(m.requested)],
        ['Sync messages', units.int(m.syncSent)],
        ['MLAT messages', units.int(m.mlatSent)],
        ['Results on the map', units.int(m.results)],
        ['Peers', m.peerCount ? units.int(m.peerCount) : units.DASH, { tip: 'Stations this one is synchronised with (from the server\'s statistics)' }],
        ['Frames without 12 MHz clock', units.int(m.noClock), { tip: 'Skipped: MLAT output needs a 12 MHz receiver timestamp (RTL-SDR with readsb / dump1090, Beast, Radarcape in 12 MHz mode)' }],
      ], { columns: 2 }),
      m.state === 'backoff' ? h('div.row-wrap', ui.button({ label: 'Retry now', icon: 'refresh', size: 'sm', onClick: () => retry(m) })) : null);
  }
  async function claim(u) {
    const rxNo = (u.receivers || [])[0]?.rxNo ?? 0;
    try {
      const r = await S.admin(() => api.post('/api/uplinks', { action: 'claim', id: u.id, rxNo }), 'Getting a claim code needs the admin password.');
      const code = r?.code || r?.claimCode || '';
      ui.modal({ title: 'Claim code', icon: 'claim', width: 440, body: h('div.col.gap-3',
        h('p', `Open the aggregator in your browser, log in, choose "Claim a feeder" and enter this code. It works once${r?.expiresMs ? ` until ${units.dateTime(r.expiresMs)}` : ' within 24 hours'}.`),
        h('code.net-token', code || '—'),
        ui.button({ label: 'Copy', icon: 'copy', size: 'sm', onClick: async () => ((await ui.copy(code)) ? ui.toast.success('Code copied') : null) })),
      actions: [{ label: 'Done', variant: 'primary' }] });
    } catch (e) { if (!e.cancelled) ui.toast.error(e.message); }
  }
  async function retry(u) {
    try { await S.admin(() => api.post('/api/uplinks', { action: 'retry', id: u.id }), 'Retrying needs the admin password.'); ui.toast('Reconnecting…'); setTimeout(loadStatus, 1500); } catch (e) { if (!e.cancelled) ui.toast.error(e.message); }
  }

  // ---- editor ----
  function beastItem(i) {
    const u = list()[i];
    const p = `uplinks.${i}.`;
    const stHost = h('div');
    statusHost.set(u.id, stHost);
    return ui.card({
      title: u.host ? `${u.host}:${u.port || 30004}` : 'New Beast output',
      sub: `ID ${u.id} · Beast${u.disabled ? ' · disabled' : ''}`,
      icon: 'globe',
      actions: [ui.button({ icon: 'trash', variant: 'ghost', size: 'sm', tip: 'Remove this output', onClick: async () => {
        if (savedIds.has(u.id) && !(await ui.confirm({ title: 'Remove this output?', message: 'This station stops feeding that aggregator after you save.', confirmLabel: 'Remove', danger: true }))) return;
        list().splice(i, 1);
        structural();
      } })],
      body: h('div.col.gap-4',
        stHost,
        F.callout('info', 'info', CONSENT_C9),
        h('div.set-fields.set-fields--2',
          F.text(p + 'host', { label: 'Aggregator host', mono: true, placeholder: 'feed.example.org', hint: 'Beast ingest of the other aggregator (readsb-style).' }),
          F.number(p + 'port', { label: 'Port', min: 1, max: 65535, step: 1, hint: 'Usually 30004.' }),
          F.select(p + 'format', { label: 'Format', options: BEAST_FORMATS }),
          F.text(p + 'uuid', { label: 'UUID (optional)', mono: true, placeholder: 'leave empty to generate one', hint: 'Set it to your mlat-client\'s --uuid so that aggregator joins both links.' }),
          F.number(p + 'reduceMs', { label: 'Reduce interval (ms)', min: 0, max: 5000, step: 10, hint: '0 = 250 ms. Ignored for beast_out.' }),
          F.toggle(p + 'timestamps', { label: 'Send timestamps', hint: 'Off sends all-zero timestamps.' }),
          F.toggle(p + 'disabled', { label: 'Disabled', hint: 'Keep the settings but do not connect.' }))),
    });
  }

  function mlatItem(i) {
    const u = list()[i];
    const p = `uplinks.${i}.`;
    const stHost = h('div');
    statusHost.set(u.id, stHost);
    return ui.card({
      title: u.host ? `${u.host}:${u.port || 31090}` : 'New MLAT output',
      sub: `ID ${u.id} · MLAT${u.disabled ? ' · disabled' : ''}`,
      icon: 'radar',
      actions: [ui.button({ icon: 'trash', variant: 'ghost', size: 'sm', tip: 'Remove this output', onClick: async () => {
        if (savedIds.has(u.id) && !(await ui.confirm({ title: 'Remove this output?', message: 'This station stops taking part in that network\'s MLAT after you save.', confirmLabel: 'Remove', danger: true }))) return;
        list().splice(i, 1);
        structural();
      } })],
      body: h('div.col.gap-4',
        stHost,
        F.callout('info', 'info', CONSENT_MLAT),
        h('div.set-fields.set-fields--2',
          F.text(p + 'host', { label: 'MLAT server host', mono: true, placeholder: 'feed.example.org', hint: 'The network\'s mlat server (mlat-client protocol).' }),
          F.number(p + 'port', { label: 'Port', min: 1, max: 65535, step: 1, hint: 'Usually 31090.' }),
          F.text(p + 'user', { label: 'Display name', placeholder: 'airview', hint: 'Shown by the network (letters, digits, space, _ . -).' }),
          F.text(p + 'uuid', { label: 'UUID (optional)', mono: true, placeholder: 'leave empty to generate one', hint: 'Use the UUID of your Beast output to the same network.' }),
          F.seg(p + 'pin', { label: 'Public map', options: [{ value: 'hidden', label: 'Hide the station' }, { value: 'region', label: 'May be shown' }], block: true }),
          F.toggle(p + 'disabled', { label: 'Disabled', hint: 'Keep the settings but do not connect.' }))),
    });
  }

  async function addPreset(n) {
    const ok = await ui.confirm({ title: `Feed ${n.name}?`, message: `${CONSENT_C9} ${CONSENT_MLAT}`, confirmLabel: 'Add both outputs' });
    if (!ok) return;
    const uuid = globalThis.crypto?.randomUUID ? globalThis.crypto.randomUUID() : '';
    const b = newBeastUplink(list());
    b.host = n.beast[0]; b.port = n.beast[1]; b.uuid = uuid;
    list().push(b);
    list().push(newMLATUplink(list(), n.mlat[0], n.mlat[1], uuid));
    structural();
    ui.toast(`Added the ${n.name} outputs - save to connect.`);
  }

  function item(i) {
    const u = list()[i];
    if (u.type === 'beast') return beastItem(i);
    if (u.type === 'mlat') return mlatItem(i);
    const p = `uplinks.${i}.`;
    const stHost = h('div');
    statusHost.set(u.id, stHost);
    const inputsOpts = feeds.map((f) => ({ id: f.id, label: `${f.name || f.id} (${f.id})` }));
    const inputsBox = h('div.col.gap-1', inputsOpts.length
      ? inputsOpts.map((o) => ui.checkbox({ label: o.label, checked: !(u.inputs || []).length || (u.inputs || []).includes(o.id), onChange: () => {
        const checked = [...inputsBox.querySelectorAll('input')].map((x, k) => (x.checked ? inputsOpts[k].id : null)).filter(Boolean);
        S.set(p + 'inputs', checked.length === inputsOpts.length ? [] : checked);
      } }))
      : h('div.set-help', 'No receiver feed is configured.'));
    const tsToggle = F.toggle(p + 'timestamps', { label: 'Send timestamps (MLAT)', hint: 'Needed for multilateration and for the network to find the exact position of your antenna.', onChange: async (v) => {
      if (!v || consented.has(u.id) || (savedIds.has(u.id) && (S.saved.uplinks || []).find((x) => x.id === u.id)?.timestamps)) return;
      const ok = await ui.confirm({ title: 'Send timestamps?', message: CONSENT_TEXT, confirmLabel: 'Send timestamps' });
      if (ok) { consented.add(u.id); return; }
      S.set(p + 'timestamps', false);
      tsToggle.control.checked = false;
    } });
    return ui.card({
      title: u.host ? `${u.host}:${u.port || 30160}` : 'New uplink',
      sub: `ID ${u.id}${u.disabled ? ' · disabled' : ''}`,
      icon: 'globe',
      actions: [ui.button({ icon: 'trash', variant: 'ghost', size: 'sm', tip: 'Remove this uplink', onClick: async () => {
        if (savedIds.has(u.id) && !(await ui.confirm({ title: 'Remove this uplink?', message: 'This station stops feeding that aggregator after you save.', confirmLabel: 'Remove', danger: true }))) return;
        list().splice(i, 1);
        structural();
      } })],
      body: h('div.col.gap-4',
        stHost,
        h('div.set-fields.set-fields--2',
          F.text(p + 'host', { label: 'Aggregator host', mono: true, placeholder: 'adsb.example.org', hint: 'Host name or address of the AirView aggregator.' }),
          F.number(p + 'port', { label: 'Port', min: 1, max: 65535, step: 1, hint: 'AVF/1 port, default 30160.' }),
          F.secret(p + 'enrolToken', { label: 'Enrol token', what: 'token', hint: 'Only needed the first time, when the aggregator admits feeders by invitation.' }),
          F.text(p + 'serverPin', { label: 'Server pin (optional)', mono: true, placeholder: 'sha256/…', hint: 'Pins the aggregator\'s TLS key. Empty = trust the system certificates; a self-signed aggregator shows its pin on its admin page.' }),
          F.toggle(p + 'disabled', { label: 'Disabled', hint: 'Keep the settings but do not connect.' })),
        ui.section('What is sent', 'signal'),
        h('div.set-fields.set-fields--2',
          tsToggle,
          F.toggle(p + 'sendLocation', { label: 'Send the station position', hint: 'Lets the network use this station for MLAT right away. Never shown publicly.' }),
          F.toggle(p + 'shareGnss', { label: 'Share the receiver GNSS position', hint: 'The receiver\'s own GPS position (e.g. a Radarcape); only when the receiver reports one.' }),
          F.toggle(p + 'returnMlat', { label: 'Receive MLAT results', hint: 'Show the network\'s MLAT positions on this station\'s map.' }),
          F.select(p + 'spoolMb', { label: 'Outage buffer on disk', numeric: true, options: [{ value: SPOOL_AUTO, label: 'Automatic' }, { value: 0, label: 'RAM only' }, { value: 256, label: '256 MB' }, { value: 1024, label: '1 GB' }, { value: 4096, label: '4 GB' }], hint: 'Frames kept while the aggregator is unreachable and sent afterwards.' }),
          ui.field({ label: 'Receiver feeds', hint: 'All ticked = every timed feed (default).', control: inputsBox })),
        ui.section('On the public pages of the aggregator', 'eye'),
        h('div.set-fields.set-fields--2',
          F.seg(p + 'pin', { label: 'Map pin', options: PIN_OPTIONS.map((o) => ({ value: o.value, label: o.label, tip: o.hint })), block: true }),
          F.toggle(p + 'publicStats', { label: 'Public statistics' }),
          F.toggle(p + 'showName', { label: 'Show the station name' }))),
    });
  }

  loadStatus();
  S.scope.every(5000, loadStatus);
  return [
    ui.card({ title: 'Feed AirView networks', icon: 'globe', body: [
      F.help('An uplink sends this station\'s receiver data to an AirView aggregator over the encrypted AVF/1 protocol. The station keeps working on its own; frames are buffered while the aggregator is unreachable. Frames that already carry MLAT positions are never forwarded.'),
      h('div.row', ui.button({ label: 'Add uplink', icon: 'plus', variant: 'primary', onClick: async () => {
        const u = newUplink(list());
        u.timestamps = askConsent(u) ? await ui.confirm({ title: 'Send timestamps (MLAT)?', message: CONSENT_TEXT, confirmLabel: 'Send timestamps', cancelLabel: 'No timestamps' }) : true;
        if (u.timestamps) consented.add(u.id);
        list().push(u);
        structural();
      } }),
      ui.button({ label: 'Add Beast output', icon: 'plus', tip: 'Also feed another aggregator (Beast, e.g. readsb on port 30004)', onClick: () => {
        list().push(newBeastUplink(list()));
        structural();
      } }),
      ui.button({ label: 'Add MLAT output', icon: 'plus', tip: 'Take part in another network\'s multilateration (mlat-client protocol, e.g. port 31090)', onClick: () => {
        list().push(newMLATUplink(list()));
        structural();
      } })),
    ] }),
    ui.card({ title: 'Feed community networks', icon: 'users', body: [
      F.help('Adds a Beast output and an MLAT output with one shared UUID for a network. Feeding a network shares this station\'s receiver data with it under its terms; MLAT positions it sends back are shown here and never forwarded. Check the network\'s own feeder page if a connection fails: their addresses can change.'),
      h('div.row-wrap', NETWORK_PRESETS.map((n) => ui.button({ label: n.name, icon: 'plus', size: 'sm', tip: `Beast ${n.beast.join(':')} + MLAT ${n.mlat.join(':')}`, onClick: () => addPreset(n) }))),
    ] }),
    ...list().map((_, i) => item(i)),
  ];
}
