// alerts/acas.js - "TCAS resolution advisories" card of the Alerts page.
// Lists the ACAS RA events the station received (GET /api/acas/events, internal/acas): when,
// which aircraft, the advisories in order, where and the threat when the aircraft reported it.
// A report is the aircraft's own statement that its ACAS issued an advisory; it says nothing about
// the crew's response, and a missing report does not mean there was none.

const REFRESH_MS = 30000;

/** @returns {{ el: HTMLElement }} */
export function createACASCard(ctx) {
  const { ui, api, units } = ctx;
  const { h } = ui;
  let days = 7;
  const body = h('div.col');
  const range = ui.segmented({ options: [{ value: 1, label: '24 h' }, { value: 7, label: '7 d' }, { value: 30, label: '30 d' }], value: days,
    onChange: (v) => { days = +v; load(); } });
  const card = ui.card({ title: 'TCAS resolution advisories', icon: 'alert',
    sub: 'Reported by the aircraft themselves (DF16, BDS 3,0, TC28) · not every RA is reported', body: h('div.col', h('div.alp-feed-tools', range), body) });

  const tbl = ui.table({
    columns: [
      { key: 'start', label: 'Time', sort: true, sortDir: 'desc', render: (e) => units.dateTime(e.start), width: 128 },
      { key: 'callsign', label: 'Aircraft', sort: true, render: (e) => h('a', { href: '#/map/' + e.icao, tip: e.icao }, e.callsign || e.icao) },
      { key: 'adv', label: 'Advisories', render: (e) => h('span', { tip: [e.corrective ? 'Corrective' : 'Preventive', e.racText ? 'Complement: ' + e.racText : '', 'Sources: ' + (e.sources || []).join(', '), e.terminated ? 'Reported as ended' : ''].filter(Boolean).join(' · ') },
        (e.open ? '● ' : '') + (e.advisories || []).join(' › ')) },
      { key: 'altFt', label: 'Altitude', num: true, align: 'right', sort: true, render: (e) => (typeof e.altFt === 'number' ? units.alt(e.altFt, { fl: true }) : '—') },
      { key: 'threat', label: 'Threat', render: threatText },
      { key: 'dur', label: 'Duration', num: true, align: 'right', render: (e) => Math.max(1, Math.round((e.end - e.start) / 1000)) + ' s' },
    ],
    rows: [], sort: { key: 'start', dir: 'desc' }, empty: 'No resolution advisories received in this period',
  });

  function threatText(e) {
    if (e.threatIcao) return h('a', { href: '#/map/' + e.threatIcao }, e.threatIcao);
    const p = [];
    if (typeof e.threatAltFt === 'number') p.push(units.alt(e.threatAltFt, { fl: true }));
    if (typeof e.threatRangeNm === 'number') p.push((e.threatRangeNm >= 12.7 ? '> 12.5' : e.threatRangeNm.toFixed(1)) + ' NM');
    if (typeof e.threatBearing === 'number') p.push(Math.round(e.threatBearing) + '° rel.');
    return p.length ? p.join(' · ') : '—';
  }

  let timer = 0;
  async function load() {
    clearTimeout(timer);
    try {
      const j = await api.get('/api/acas/events', { params: { days, limit: 1000 }, signal: ctx.signal });
      ui.mount(body, tbl);
      tbl.setRows(j?.events || []);
    } catch (e) {
      if (e.aborted) return;
      ui.mount(body, e.status === 401 || e.status === 403 ? ui.empty({ title: 'Log in to see the resolution advisories', compact: true }) : ui.error(e, { compact: true, retry: load }));
    }
    timer = setTimeout(load, REFRESH_MS);
  }
  ctx.onCleanup(() => clearTimeout(timer));
  ui.mount(body, ui.skeleton({ lines: 3 }));
  load();
  return { el: card };
}
