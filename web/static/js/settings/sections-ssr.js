// settings/sections-ssr.js - Settings > SSR self-location (EXPERIMENTAL; internal/engine/ssrwire.go,
// docs/aggregator/SSR-RESEARCH.md): the ssr.enabled switch with its warning, the estimator state from
// GET /api/ssr (replies, pair matches, the last solve with its interrogators) and "Solve now"
// (POST /api/ssr/solve). Admin only. The result is advisory: there is deliberately no button that applies it to
// the station position.

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
    ];
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
