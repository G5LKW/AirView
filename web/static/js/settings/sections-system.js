// settings/sections-system.js - Weather, Web server, Connection / remote, Display and Demo sections (Time (NTP) lives in pages/settings.js).

import { BASEMAPS } from '../map/basemaps.js';
import { MAX_RINGS } from './model.js';

const isNum = (v) => typeof v === 'number' && Number.isFinite(v);

// =============================================================================================
// Weather
// =============================================================================================

export function renderWeather(S, F) {
  const { ui, api, units } = S.ctx;
  const { h } = ui;
  const statusHost = h('div');
  const reload = S.panel(statusHost, (signal) => api.get('/api/weather/current', { signal }), (r) => {
    const st = r.status || {};
    if (!r.enabled) return ui.empty({ icon: 'cloud', compact: true, title: 'Weather is switched off', text: 'No weather is fetched. The weather page, the propagation analysis and the reception forecast need it.' });
    if (!r.located) return ui.empty({ icon: 'pin', compact: true, title: 'Waiting for the station position', text: 'Weather is fetched for the station location - set it under Station location first.' });
    const err = st.lastError || st.error;
    return [
      ui.kv([
        ['State', st.lastOk ? (st.failures ? ui.badge(`${st.failures} failed since the last success`, 'amber') : ui.badge('OK', 'green')) : ui.badge('No data yet', 'amber')],
        ['Last successful fetch', st.lastOk ? units.ago(st.lastOk) : 'never'],
        ['Next fetch', st.nextPoll ? units.clock(st.nextPoll) : '—'],
        ['Now', r.current ? `${r.text || ''} ${units.temp(r.current.tempC)}`.trim() : '—', { text: true }],
        ['Radar overlay frames', st.radarEnabled ? (st.radarLastOk ? units.ago(st.radarLastOk) : 'not fetched yet') : 'off'],
        st.usage ? ['Requests today', `${units.int(st.usage.day)}${st.usage.budget?.PerDay ? ' of ' + units.int(st.usage.budget.PerDay) + ' allowed' : ''}`] : null,
        err ? ['Last error', String(err), { text: true }] : null,
      ]),
      h('div.set-help', r.attribution || 'Weather data by Open-Meteo.com', r.attributionUrl ? [' - ', h('a', { href: r.attributionUrl, target: '_blank', rel: 'noopener noreferrer' }, r.attributionUrl)] : null),
    ];
  }, 60000);

  const out = h('span.muted');
  const btn = ui.button({ label: 'Fetch now', icon: 'refresh', onClick: async () => {
    btn.setLoading(true);
    ui.setText(out, '');
    try {
      await S.admin(() => api.post('/api/weather/refresh', undefined, { signal: S.scope.signal, timeout: 45000 }));
      ui.setText(out, 'Weather refreshed.');
      reload({ quiet: true });
    } catch (e) {
      if (!e.aborted && !e.cancelled) ui.setText(out, 'Refresh failed: ' + e.message);
    } finally {
      btn.setLoading(false);
    }
  } });

  return [
    ui.card({ title: 'Weather data', icon: 'cloud', body: [
      F.help('Current conditions, the hourly forecast and the upper-air profile come from Open-Meteo: free, no account, no API key. They drive the weather page, the ducting / propagation analysis and the reception forecast. Needs the internet and the station position.'),
      h('div.set-fields.set-fields--2',
        F.toggle('weather.enabled', { label: 'Fetch weather', hint: 'Off: AirView makes no weather requests at all.' }),
        F.number('weather.intervalMin', { label: 'Refresh interval', suffix: 'min', min: 5, max: 1440, step: 1, hint: 'Minutes between fetches (5 - 1440). The models update hourly at best, so 15 is plenty.' }),
        F.toggle('weather.radarOverlay', { label: 'Precipitation radar on the map', hint: 'Offers the RainViewer radar layer on the Live Map (personal / educational use, attribution shown on the map).' })),
    ] }),
    ui.card({ title: 'Live status', icon: 'signal', actions: btn, body: [statusHost, out] }),
  ];
}

// =============================================================================================
// Web server
// =============================================================================================

export function renderWeb(S, F) {
  const { ui, api, units, live } = S.ctx;
  const { h } = ui;
  const scope = S.scope;

  // ---- port / bind ----
  const listenKv = ui.kv([['Listening on', '—'], ['Port in use now', '—'], ['Restart needed', '—']]);
  function paintListen() {
    const sys = S.system;
    listenKv.set(0, sys?.httpAddr || '—');
    listenKv.set(1, String(sys?.httpPort ?? S.status?.httpPort ?? '—'));
    listenKv.set(2, sys ? (sys.restartRequired ? ui.badge('Yes - restart AirView', 'amber') : ui.badge('No', 'green')) : '—');
  }
  paintListen();
  S.refreshSystem().then(() => { if (!scope.signal.aborted) paintListen(); });
  scope.every(15000, () => S.refreshSystem().then(() => { if (!scope.signal.aborted) paintListen(); }));

  const bindMode = (v) => { const s = String(v ?? '').trim().toLowerCase(); return s === '' || s === '*' ? 'all' : s === '127.0.0.1' || s === 'localhost' || s === '::1' ? 'local' : 'custom'; };
  const bindText = F.text('http.bindAddr', { label: 'Bind address', mono: true, placeholder: 'empty = all interfaces', hint: 'IP address of one network interface of this computer.', onChange: () => paintBind() });
  const bindSeg = ui.segmented({ value: bindMode(S.get('http.bindAddr')), options: [
    { value: 'all', label: 'All interfaces', tip: 'Reachable from every device on your network' },
    { value: 'local', label: 'This computer only', tip: '127.0.0.1: only a browser on the AirView computer itself' },
    { value: 'custom', label: 'One address' },
  ], onChange: (v) => {
    if (v === 'all') { S.set('http.bindAddr', ''); bindText.control.value = ''; }
    else if (v === 'local') { S.set('http.bindAddr', '127.0.0.1'); bindText.control.value = '127.0.0.1'; }
    paintBind(v);
  } });
  const bindWarn = h('div.set-help');
  function paintBind(force) {
    const m = force || bindMode(S.get('http.bindAddr'));
    if (!force) bindSeg.value = m;
    bindText.classList.toggle('hidden', m !== 'custom');
    ui.setText(bindWarn, m === 'local' ? 'After the restart the web interface is no longer reachable from other devices (phone, tablet, remote AirView clients).'
      : m === 'all' ? 'Every device that can reach this computer can open the web interface. Set an admin password below if others share your network.' : '');
  }
  paintBind();

  // ---- tile cache ----
  const tileHost = h('div');
  const reloadTiles = S.panel(tileHost, (signal) => api.get('/api/tilecache', { signal }), (t) => {
    if (!t.enabled) return ui.empty({ icon: 'layers', compact: true, title: 'Tile cache is off', text: 'Map tiles are loaded straight from the map servers by every browser.' });
    const pct = t.maxBytes ? (t.bytes / t.maxBytes) * 100 : 0;
    const total = (t.hits || 0) + (t.misses || 0);
    return [
      h('div.set-meter-row', ui.meter(pct), h('span.num', `${units.bytes(t.bytes)} of ${units.bytes(t.maxBytes)}`)),
      ui.kv([
        ['Cached files', units.int(t.files)],
        ['Served from cache', total ? `${units.pct((t.hits / total) * 100)} (${units.int(t.hits)} of ${units.int(total)})` : 'no requests yet'],
        ['Revalidated / stale / errors', `${units.int(t.revalidated)} / ${units.int(t.stale)} / ${units.int(t.errors)}`],
        ['Evicted (cache full)', units.int(t.evictions)],
        ['Folder', t.dir || '—', { text: true, tip: t.dir }],
      ]),
      h('div.set-help', `Hosts: ${(t.hosts || []).join(', ')}. A high cache share means fast maps and little load on the free map servers.`),
    ];
  }, 30000);
  const clearBtn = ui.button({ label: 'Clear tile cache', icon: 'trash', size: 'sm', onClick: async () => {
    if (!(await ui.confirm({ title: 'Clear the tile cache?', message: 'All cached map tiles are deleted and fetched again when needed. Nothing else is touched.', confirmLabel: 'Clear cache' }))) return;
    clearBtn.setLoading(true);
    try {
      await S.admin(() => api.post('/api/tilecache/clear', undefined, { signal: scope.signal, timeout: 60000 }));
      ui.toast.success('Tile cache cleared.');
      reloadTiles({ quiet: true });
    } catch (e) {
      if (!e.aborted && !e.cancelled) ui.toast.error('Could not clear the cache: ' + e.message);
    } finally {
      clearBtn.setLoading(false);
    }
  } });

  // ---- session ----
  const sesHost = h('div');
  async function paintSession(refresh) {
    if (refresh) await S.refreshSession();
    if (scope.signal.aborted) return;
    const s = S.session;
    if (!s) { ui.mount(sesHost, ui.empty({ compact: true, icon: 'lock', title: 'Session state unknown', text: 'The server did not answer.' })); return; }
    ui.mount(sesHost,
      ui.kv([
        ['This browser', s.loopback ? 'on the AirView computer (always admin)' : 'another device on the network', { text: true }],
        ['Admin password', s.passwordSet ? ui.badge('set', 'green') : ui.badge('not set - everyone on the network can change settings', 'amber')],
        ['May change settings', s.admin ? ui.badge('yes', 'green') : ui.badge('no - log in', 'amber')],
        ['Logged in', s.loggedIn ? 'yes (session cookie, 30 days or until AirView restarts)' : 'no', { text: true }],
      ]),
      h('div.row-wrap.set-actions',
        !s.admin ? ui.button({ label: 'Log in', icon: 'lock', variant: 'primary', onClick: async () => { if (await S.login()) paintSession(true); } }) : null,
        s.loggedIn ? ui.button({ label: 'Log out', icon: 'power', onClick: async () => {
          try {
            S.session = await api.post('/api/logout', undefined, { signal: scope.signal });
            ui.toast.info('Logged out.');
            live.refreshStatus().catch(() => {});
          } catch (e) { if (!e.aborted && !e.cancelled) ui.toast.error('Logout failed: ' + e.message); }
          paintSession(false);
        } }) : null));
  }
  paintSession(true);

  return [
    ui.card({ title: 'HTTP port and address', icon: 'globe', body: [
      h('div.set-fields.set-fields--2',
        F.number('http.port', { label: 'HTTP port', min: 1, max: 65535, step: 1, hint: 'Default 80, so the address is simply http://<this computer>/. If the port is taken (another web server, missing permission on Linux) AirView falls back to 8080 … 8089 and then to a free port, and shows the real one here.' }),
        ui.field({ label: 'Reachable from', control: bindSeg, hint: 'Which network interfaces the web server listens on.' }),
        h('div', listenKv), h('div', bindText, bindWarn)),
      F.callout('warn', 'power', h('b', 'Restart required. '), 'A changed port or bind address is saved at once but only used after AirView is restarted. Until then the server keeps listening where it does now; the "Restart needed" flag above shows when that is the case.'),
    ] }),
    ui.card({ title: 'Map tile cache', icon: 'layers', actions: clearBtn, body: [
      F.toggle('http.tileCache', { label: 'Cache map tiles on disk', hint: 'Browsers fetch map tiles through AirView (/tilecache/), which keeps them on disk: faster maps, works for clients without internet access, and far fewer requests to the free tile servers.' }),
      tileHost,
    ] }),
    ui.card({ title: 'Admin password', icon: 'shield', body: [
      F.help('Without a password every device that can reach the web interface can change settings, prune the database or factory-reset the station. With one, other devices must log in first; a browser on the AirView computer itself (localhost) is always admin, so you cannot lock yourself out. Reading - maps, statistics, charts - stays open to everyone.'),
      F.secret('http.adminPasswordHash', { label: 'Password', what: 'admin password', confirm: true, hint: 'Only a salted hash is stored. Changing or removing the password logs every other device out.' }),
    ] }),
    ui.card({ title: 'This session', icon: 'lock', body: sesHost }),
  ];
}

// =============================================================================================
// Connection / remote
// =============================================================================================

const MODES = [
  { value: 'standalone', label: 'Standalone', icon: 'radar',
    text: 'This AirView connects to the receiver itself, keeps its own database and serves its own web interface. The normal mode for the computer (or Raspberry Pi) next to the receiver.' },
  { value: 'remote-data', label: 'Remote data', icon: 'route',
    text: 'This AirView logs in to the data port of another AirView (the host) and receives its live frame stream. Decoding, statistics and the database run here, on this computer - full functionality without a receiver of your own, at the cost of some bandwidth.' },
  { value: 'remote-web', label: 'Remote web', icon: 'globe',
    text: 'The desktop window simply shows the web interface of another AirView. Nothing is computed or stored here; all history lives on the host. Lightest option, same as opening the host in a browser.' },
];

export function renderConnection(S, F) {
  const { ui, api, units } = S.ctx;
  const { h } = ui;
  const scope = S.scope;

  const modeCards = h('div.set-modes');
  const remoteCard = h('div');
  function paintModes() {
    const cur = S.get('mode');
    ui.mount(modeCards, MODES.map((m) => h('button.set-mode' + (m.value === cur ? '.active' : ''), { type: 'button', 'aria-pressed': String(m.value === cur), onclick: () => { if (S.get('mode') !== m.value) { S.set('mode', m.value); paintModes(); } } },
      h('div.set-mode-head', ui.icon(m.icon), h('b', m.label), m.value === cur ? ui.badge('selected', 'blue') : null), h('p', m.text))));
    remoteCard.classList.toggle('set-dim', cur === 'standalone');
    paintLink();
  }
  const modeF = F.reg('mode', ui.field({ control: modeCards }));
  const eff = S.status?.mode;
  const link = h('div.set-help');
  function paintLink() {
    const host = String(S.get('remote.host') || '').trim();
    const port = S.get('remote.httpPort');
    if (!host) { ui.mount(link, S.get('mode') === 'standalone' ? 'Only used in the two remote modes.' : ''); return; }
    const url = `http://${host.includes(':') ? `[${host}]` : host}${port && port !== 80 ? ':' + port : ''}/`;
    ui.mount(link, 'Host web interface: ', h('a', { href: url, target: '_blank', rel: 'noopener noreferrer' }, url));
  }

  const hubHost = h('div');
  S.panel(hubHost, (signal) => api.get('/api/feeds', { signal }), (r) => {
    const hub = r.hub || {};
    if (!hub.enabled) return ui.empty({ icon: 'link', compact: true, title: 'The data port is off', text: 'Switch it on (and save) to let AirView desktop clients in remote-data mode connect to this station.' });
    const clients = hub.clientList || [];
    return [
      ui.kv([
        ['State', hub.error ? ui.badge('failed to start', 'red') : ui.badge('listening', 'green')],
        ['Port', String(hub.port)],
        ['Connected clients', units.int(hub.clients)],
        ['Clients dropped for being too slow', units.int(hub.droppedClients)],
        hub.error ? ['Error', hub.error, { text: true }] : null,
      ]),
      clients.length
        ? ui.table({ columns: [
          { key: 'addr', label: 'Client', num: true },
          { key: 'since', label: 'Connected', render: (c) => units.ago(c.since) },
          { key: 'bytesSent', label: 'Sent', align: 'right', num: true, render: (c) => units.bytes(c.bytesSent) },
          { key: 'backlog', label: 'Backlog', align: 'right', num: true, render: (c) => units.bytes(c.backlog), tip: 'Bytes waiting for a slow client' },
        ], rows: clients })
        : h('div.set-help', 'No client is connected right now.'),
    ];
  }, 10000);

  ui.mount(remoteCard,
    h('div.set-fields.set-fields--2',
      F.text('remote.host', { label: 'Host', mono: true, placeholder: 'airview-pi.local  or  192.168.1.20', hint: 'Host name or IP address of the AirView station to connect to (e.g. the Raspberry Pi).', onChange: paintLink }),
      F.toggle('remote.useHostReceiver', { label: 'Use the host\'s station settings', hint: 'Adopt the host\'s station position and antenna definitions instead of the ones stored here (remote-data mode).' }),
      F.number('remote.dataPort', { label: 'Data port', min: 1, max: 65535, step: 1, hint: 'The host\'s data port (hub) for remote-data mode. Default 30154.' }),
      F.number('remote.httpPort', { label: 'Website port', min: 1, max: 65535, step: 1, hint: 'The host\'s HTTP port for remote-web mode. Default 80.', onChange: paintLink }),
      F.secret('remote.password', { label: 'Data port password', hint: 'The password configured for the data port on the host. It never crosses the network (challenge-response login).' })),
    link);
  paintModes();

  return [
    ui.card({ title: 'Run mode', icon: 'route', body: [
      modeF,
      eff && eff !== S.saved.mode ? F.callout('info', 'info', `Effective mode right now: ${eff}. A headless / Raspberry Pi build always runs standalone, whatever is stored here.`) : null,
      F.help('The mode matters for the Windows desktop app. A Raspberry Pi station always runs standalone and can offer its data port to desktop clients (below). Changing the mode restarts the engine when you save.'),
    ] }),
    ui.card({ title: 'Remote AirView host', icon: 'link', body: remoteCard }),
    ui.card({ title: 'Data port (hub) of this station', icon: 'signal', body: [
      F.help('The data port lets other AirView instances (desktop app in remote-data mode) log in and receive this station\'s live frame stream together with its position and antennas. Slow clients are dropped rather than slowing the station down.'),
      h('div.set-fields.set-fields--2',
        F.toggle('hub.enabled', { label: 'Offer the data port', hint: 'TCP server for remote AirView clients.' }),
        F.number('hub.port', { label: 'Port', min: 1, max: 65535, step: 1, hint: 'Default 30154. Open it in the firewall of this computer if clients cannot connect.' }),
        F.secret('hub.password', { label: 'Password', hint: 'Shared secret for the login. Without one anybody who can reach the port can read your data stream.' })),
      ui.section('Live status', 'signal'),
      hubHost,
    ] }),
  ];
}

// =============================================================================================
// Display
// =============================================================================================

export function renderDisplay(S, F) {
  const { ui, units } = S.ctx;
  const { h } = ui;

  // ---- range rings (stored in km, edited in the display unit) ----
  const chips = h('div.set-chips');
  const ringsF = F.reg('ui.rangeRingsKm', ui.field({ label: 'Range rings', control: chips, hint: `Circles around the station on the Live Map, up to ${MAX_RINGS}. Stored in km; shown here in your distance unit (${units.label('dist')}).` }));
  const rings = () => (Array.isArray(S.draft.ui.rangeRingsKm) ? S.draft.ui.rangeRingsKm : (S.draft.ui.rangeRingsKm = []));
  const addInp = ui.input({ type: 'number', size: 'sm', placeholder: 'radius', suffix: units.label('dist'), min: 1, step: 'any', onEnter: addRing });
  function setRings(list) {
    S.set('ui.rangeRingsKm', [...new Set(list)].sort((a, b) => a - b));
    paintRings();
  }
  function addRing() {
    const v = addInp.value;
    if (!isNum(v) || v <= 0) return;
    const km = Math.round(units.toBase('dist', v));
    if (km >= 1) setRings([...rings(), km]);
    addInp.value = '';
  }
  function paintRings() {
    ui.mount(chips,
      rings().map((km) => h('span.set-chip', h('span.num', units.dist(km, { digits: 0 })),
        h('button', { type: 'button', 'aria-label': `Remove ${units.dist(km)}`, onclick: () => setRings(rings().filter((x) => x !== km)) }, ui.icon('x')))),
      rings().length ? null : h('span.muted', 'No rings'),
      h('span.set-chip-add', addInp, ui.button({ label: 'Add', icon: 'plus', size: 'sm', onClick: addRing })),
      ui.button({ label: units.system === 'metric' ? '100 / 200 / 300 / 400 km' : units.system === 'imperial' ? '50 / 100 / 150 / 200 / 250 mi' : '50 / 100 / 150 / 200 / 250 nm', size: 'sm', variant: 'ghost', tip: 'Replace with a typical set', onClick: () => {
        const base = units.system === 'metric' ? [100, 200, 300, 400] : [50, 100, 150, 200, 250].map((v) => Math.round(units.toBase('dist', v)));
        setRings(base);
      } }));
  }
  paintRings();

  const applyBtn = ui.button({ label: 'Use in this browser now', icon: 'check', size: 'sm', onClick: () => { units.set(S.get('ui.units')); ui.toast.success(`This browser now shows ${units.systemLabels?.[units.system] || units.system} units.`); } });

  return [
    ui.card({ title: 'Units', icon: 'ruler', body: [
      F.seg('ui.units', { label: 'Default unit system', options: [
        { value: 'aviation', label: 'Aviation', tip: 'ft · kt · nm' }, { value: 'metric', label: 'Metric', tip: 'm · km/h · km' }, { value: 'imperial', label: 'Imperial', tip: 'ft · mph · mi' },
      ], hint: 'Aviation: feet, knots, nautical miles. Metric: metres, km/h, kilometres. Imperial: feet, mph, statute miles. This is the default for browsers that have not chosen themselves; every browser can switch in the top bar.' }),
      h('div.row-wrap', applyBtn, h('span.muted', `This browser currently uses: ${units.systemLabels?.[units.system] || units.system}.`)),
    ] }),
    ui.card({ title: 'Live map', icon: 'map', body: h('div.set-fields.set-fields--2',
      F.number('ui.trailMinutes', { label: 'Trail length', suffix: 'min', min: 1, max: 1440, step: 1, hint: 'How many minutes of track are drawn behind each aircraft (1 - 1440). Longer trails cost drawing time on slow devices.' }),
      F.number('ui.aircraftTtlS', { label: 'Aircraft timeout', suffix: 's', min: 5, max: 3600, step: 1, hint: 'Seconds without any message before an aircraft leaves the live list and the map (5 - 3600). 60 is usual.' }),
      F.select('ui.mapStyle', { label: 'Base map', options: BASEMAPS.map((b) => ({ value: b.id, label: b.label })), hint: 'Default base map for new browsers. All of them are free services without an API key.' }),
      F.seg('ui.colorTrailsBy', { label: 'Colour trails by', options: [
        { value: 'altitude', label: 'Altitude' }, { value: 'speed', label: 'Speed' }, { value: 'signal', label: 'Signal' }, { value: 'source', label: 'Source' },
      ], hint: 'What the colour of aircraft and trails encodes by default.' }),
      ringsF) }),
    F.callout('info', 'info', 'These are station-wide defaults. Layers, filters and the view of the Live Map are remembered per browser and are not part of the configuration.'),
  ];
}

// =============================================================================================
// Demo mode
// =============================================================================================

export function renderDemo(S, F) {
  const { ui, units } = S.ctx;
  const { h } = ui;
  const st = S.status;
  const forced = st?.demo && !S.saved.demo?.enabled;
  const sim = (st?.feeds || []).find((f) => f.type === 'demo');
  return [
    ui.card({ title: 'Built-in traffic simulator', icon: 'play', body: [
      F.help('Demo mode replaces the receiver with a simulator that produces realistic Mode S / ADS-B traffic around the station, so every page can be tried without any hardware. Real feeds are paused while it runs, and the simulated traffic is recorded in the database like real traffic.'),
      h('div.set-fields.set-fields--2',
        F.toggle('demo.enabled', { label: 'Demo mode', hint: 'Applies as soon as you save: the engine switches between simulator and real feeds.' }),
        F.number('demo.aircraft', { label: 'Simulated aircraft', min: 1, max: 1000, step: 1, hint: 'How many aircraft are in the air at once (1 - 1000). 60 looks like a busy afternoon; several hundred is a load test.' })),
      ui.kv([
        ['Running right now', st ? (st.demo ? ui.badge('yes', 'violet') : ui.badge('no', 'other')) : '—'],
        ['Simulator', sim ? `${sim.connected ? units.rate(sim.msgRate) : 'stopped'} · ${sim.info || ''}` : 'not running', { text: true }],
      ]),
      forced ? F.callout('info', 'info', 'Demo mode is forced by the -demo command-line option of this run. The switch above only controls what happens on a normal start.') : null,
      F.callout('warn', 'alert', h('b', 'Mind your statistics. '), 'Simulated flights end up in the same database as real ones (records, coverage, heat map). After trying the demo on a real station, use Danger zone > Factory reset - or a fresh data folder - to start clean.'),
    ] }),
  ];
}
