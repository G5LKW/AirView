// settings/sections-ssr.js - Settings > SSR self-location (EXPERIMENTAL; internal/engine/ssrwire.go,
// docs/aggregator/SSR-RESEARCH.md): the ssr.enabled switch with its warning, the estimator state from
// GET /api/ssr (replies, pair matches, the last solve with its interrogators) and "Solve now"
// (POST /api/ssr/solve), and the interrogators located from their beam sweeps (status.radars; ssr.LocateBySweep).
// Admin only. The result is advisory: there is deliberately no button that applies it to the station position.

export function renderSSR(S, F) {
  const { ui, api, units } = S.ctx;
  const { h } = ui;
  const scope = S.scope;
  const host = h('div');
  const num = (v, d = 0) => (typeof v === 'number' && Number.isFinite(v) ? v.toFixed(d) : '-');

  const reload = S.panel(host, (signal) => api.get('/api/ssr', { signal }), (st) => statusView(st), 15000);

  function statusView(st) {
    if (!st || !st.enabled) {
      return [F.callout('info', 'info', 'SSR self-location is off. Switch it on below and save; the estimator then listens to the replies this receiver decodes.')];
    }
    const est = st.estimator || {};
    const last = st.last || null;
    const solve = ui.button({ label: 'Solve now', icon: 'refresh', size: 'sm', disabled: !!st.solving, onClick: async () => {
      solve.setLoading(true);
      try {
        await S.admin(() => api.post('/api/ssr/solve', undefined, { signal: scope.signal }));
        ui.toast.info('Solve queued.');
        setTimeout(() => reload({ quiet: true }), 1500);
      } catch (e) { if (!e.aborted && !e.cancelled) ui.toast.error(e.message); }
      finally { solve.setLoading(false); }
    } });
    const rows = [
      ['Feed', st.feedId || 'all feeds', { text: true }],
      ['Replies / positions', `${units.int(st.replies || 0)} / ${units.int(st.positions || 0)} from ${units.int(st.aircraft || 0)} aircraft`, { text: true }],
      ['Pair matches', units.int(est.pairMatches || 0), { text: true }],
      st.solving ? ['Now', ui.badge('solving', 'violet')] : null,
    ];
    if (last) {
      const where = last.ok
        ? `${num(last.lat, 4)}, ${num(last.lon, 4)} (±${num(last.sigmaHM, 0)} m, ${last.nUsed}/${last.nObs} observations)`
        : `no result${last.reason ? ': ' + last.reason : ''}`;
      rows.push(['Last solve', where, { text: true }]);
      if (st.lastAt) rows.push(['Solved', units.ago(st.lastAt), { text: true }]);
      const radars = last.radars || [];
      if (radars.length) {
        rows.push(['Interrogators', radars.map((i) => (i.name || 'unnamed') + (i.periodS ? ` (rotation ${num(i.periodS, 2)} s)` : '')).join(', '), { text: true }]);
      }
    }
    return [
      F.callout('warn', 'alert', 'Experimental and advisory. The estimate is shown here only; AirView never uses it to set, confirm or replace the station position.'),
      ui.kv(rows),
      h('div.row.gap-2', solve),
      radarsView(st),
    ];
  }

  // Interrogators located from the times their beam swept the aircraft that answered their all-calls.
  function radarsView(st) {
    const list = st.radars || [];
    const bursts = (st.estimator || {}).sweepBursts || 0;
    const intro = F.help('Radars located from their beam sweeps: a rotating Mode S radar makes aircraft answer its all-calls (DF11, tagged with its II/SI code) only while its beam points at them, so the burst times and the aircraft positions give its site and rotation. '
      + 'Errors are formal 1σ ellipses from the fit; they do not include beam squint, lock-out or position latency, and the accuracy on real radars has not been measured. '
      + `${units.int(bursts)} bursts kept (last hour). Codes are reused, so one code can show several radars.`);
    if (!list.length) {
      return h('div.ssr-radars', ui.section('Radars located from beam sweeps', 'antenna'), intro,
        h('div.set-help', st.last ? 'No interrogator codes with all-call bursts yet.' : 'Located at each solve (Solve now, or every solve interval).'));
    }
    const coord = (r) => (r.lat == null ? units.DASH : h('span.num', `${num(r.lat, 4)}, ${num(r.lon, 4)}`));
    // A reference more than 25 km away is a different radar, not a check of this one.
    const refCell = (r) => {
      if (r.refDistM == null) return units.DASH;
      if (r.refDistM > 25000) return h('span.muted', `none within 25 km`);
      return `${r.ref || 'unnamed'} · ${num(r.refDistM / 1000, 1)} km`;
    };
    const rot = (r) => (r.periodS ? `${num(Math.abs(r.periodS), 2)} s ${r.periodS < 0 ? '↺' : '↻'}` : units.DASH);
    const t = ui.table({
      columns: [
        { key: 'code', label: 'Code', sort: (r) => (r.code.startsWith('SI') ? 100 : 0) + Number(r.code.slice(3)), render: (r) => h('span.num', r.code) },
        { key: 'ok', label: 'Status', render: (r) => (r.ok ? ui.badge('located', 'green') : ui.badge('no fix', 'other', { tip: r.reason || '' })) },
        { key: 'pos', label: 'Position', render: coord },
        { key: 'semiMajorM', label: 'Formal error', align: 'right', num: true, sort: true, tip: 'Formal 1σ error ellipse: semi-major / semi-minor axis', render: (r) => (r.lat == null ? units.DASH : `${units.int(r.semiMajorM)} / ${units.int(r.semiMinorM)} m`) },
        { key: 'periodS', label: 'Rotation', align: 'right', tip: '↻ clockwise, ↺ anticlockwise', render: rot },
        { key: 'nUsed', label: 'Bursts', align: 'right', num: true, sort: true, tip: 'Bursts used / all bursts of this code, and the number of aircraft', render: (r) => `${units.int(r.nUsed)}/${units.int(r.nObs)} · ${units.int(r.nAircraft)} ac` },
        { key: 'coherence', label: 'Fit', align: 'right', num: true, sort: true, tip: 'How well the burst times follow one rotating beam (1 = perfectly)', render: (r) => num(r.coherence, 2) },
        { key: 'ref', label: 'Reference site', tip: 'Nearest site in ssr.sites (for example copied from a published radar list)', render: refCell },
      ],
      rows: list, sort: { key: 'code', dir: 'asc' }, empty: 'No radars',
    });
    const why = list.filter((r) => !r.ok && r.reason).map((r) => h('li', h('span.num', r.code), ': ', r.reason));
    return h('div.ssr-radars', ui.section('Radars located from beam sweeps', 'antenna'), intro, t, why.length ? h('ul.set-help', why) : null);
  }

  return [
    ui.card({ title: 'SSR self-location (experimental)', icon: 'wave', body: host }),
    ui.card({ title: 'Settings', icon: 'settings', body: [
      F.help('Estimates where this receiver is from the timing of secondary-radar replies of aircraft with known positions. Research feature, off by default; the detailed tunables are in the ssr block of the configuration file.'),
      h('div.set-fields.set-fields--2',
        F.toggle('ssr.enabled', { label: 'Run the SSR self-location', hint: 'Off by default. Uses some CPU; the result is advisory only.' })),
    ] }),
  ];
}
