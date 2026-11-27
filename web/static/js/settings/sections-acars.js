// settings/sections-acars.js - Settings > ACARS / VDL2 (internal/acars, internal/engine/acarswire.go): the input
// state from GET /api/acars/status (listen addresses, counters, most frequent labels) and the acars block
// (enabled, UDP / TCP listen addresses, messages kept). AirView does not demodulate VHF itself: a separate decoder
// (acarsdec or dumpvdl2 on its own SDR) sends its JSON output here.

export function renderACARS(S, F) {
  const { ui, api, units } = S.ctx;
  const { h } = ui;
  const host = h('div');

  S.panel(host, (signal) => api.get('/api/acars/status', { signal }), (st) => statusView(st), 15000);

  function statusView(st) {
    if (!st || (!st.enabled && !st.demo)) {
      return [F.callout('info', 'info', 'The ACARS input is off. Switch it on below, save, and point acarsdec or dumpvdl2 at the UDP address.')];
    }
    const s = st.stats || {};
    const inp = st.input || {};
    const src = Object.entries(s.bySource || {}).map(([k, v]) => `${k} ${units.int(v)}`).join(' · ');
    const rej = [[s.duplicates, 'duplicate'], [s.parseErrors, 'unreadable'], [s.nonAcars, 'VDL2 non-ACARS'], [s.badCrc, 'CRC error'], [s.dropped, 'rate limit']]
      .filter(([v]) => v > 0).map(([v, l]) => `${units.int(v)} ${l}`).join(' · ');
    const rows = [
      st.demo ? ['Demo', ui.badge('synthetic messages', 'violet')] : null,
      st.enabled ? ['UDP input', inp.udpError ? `error: ${inp.udpError}` : inp.udp || 'off', { text: true }] : null,
      st.enabled ? ['TCP input', inp.tcpError ? `error: ${inp.tcpError}` : inp.tcp ? `${inp.tcp} · ${inp.tcpConns || 0} connected` : 'off', { text: true }] : null,
      ['Received', units.int(s.total || 0), { text: true }],
      src ? ['Decoders', src, { text: true }] : null,
      ['Kept', `${units.int(s.kept || 0)} of ${units.int(s.capacity || 0)}`, { text: true }],
      ['Aircraft', `${units.int(s.aircraft || 0)} · ${units.int(s.linked || 0)} msgs linked`, { text: true, tip: 'Messages linked to a Mode S address (reported by the decoder or matched by registration)' }],
      ['Rejected', rej || 'none', { text: true }],
      s.lastT ? ['Last message', units.ago(s.lastT), { text: true }] : null,
      s.labels?.length ? ['Top labels', s.labels.slice(0, 5).map((l) => `${l.label} ${units.int(l.n)}`).join(' · '), { text: true }] : null,
    ];
    return [ui.kv(rows)];
  }

  return [
    ui.card({ title: 'ACARS / VDL2 input', icon: 'list', body: host }),
    ui.card({ title: 'Settings', icon: 'settings', body: [
      F.help('Receives ACARS and VDL Mode 2 messages decoded by acarsdec or dumpvdl2 running on a second SDR (VHF 129-137 MHz). ' +
        'Example options: acarsdec --output json:udp:host=127.0.0.1,port=5550, or dumpvdl2 --output decoded:json:udp:address=127.0.0.1,port=5550. ' +
        'Messages are kept in memory only and appear in the aircraft detail panel of the map. Their free text can name people: they are never shown on public pages.'),
      h('div.set-fields.set-fields--2',
        F.toggle('acars.enabled', { label: 'Receive ACARS / VDL2 messages', hint: 'Off by default.' }),
        F.text('acars.udpListen', { label: 'UDP listen address', mono: true, placeholder: '127.0.0.1:5550', hint: 'Empty = 127.0.0.1:5550 (this computer only). :5550 accepts decoders on other hosts; "off" disables UDP.' }),
        F.text('acars.tcpListen', { label: 'TCP listen address', mono: true, placeholder: 'off', hint: 'For decoders in TCP client mode (newline-separated JSON). Empty = off.' }),
        F.number('acars.keep', { label: 'Messages kept', min: 0, max: 20000, step: 100, digits: 0, placeholder: '2000', hint: '100 - 20000; 0 or empty = 2000. Usually well under 1 kB each.' })),
    ] }),
  ];
}
