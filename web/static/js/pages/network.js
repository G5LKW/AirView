// pages/network.js - the public network page (network / aggregator mode): stations online, traffic, the
// published MLAT quality and the opt-in public station list (DESIGN §8.2: non-locating fields only).
//
// Everything rendered here goes through scrubPublic(): even a server that sends more than the public DTO
// shows nothing receiver-related on this page. Pins are listed only as "on the map"; their (deliberately
// inexact) points are drawn by the Live Map's Network view.
//
// API (net/endpoints.js): GET /api/public/stats, GET /api/public/stations, GET /api/public/mlat (the 1° accuracy
// cells: only cells with at least 5 contributing stations, no identities; drawn by the map's MLAT quality layer).

import { EP, normPublicSummary, normPublicStations } from '../net/endpoints.js';
import { scrubPublic, publicPin } from '../net/model.js';
import { pageHead, failState, PRIVACY_NOTE, v } from '../net/view.js';

export default {
  id: 'network',
  title: 'Network',

  async mount(container, ctx) {
    const { api, ui, units } = ctx;
    const { h } = ui;
    const head = pageHead('Network', 'Loading…',
      ui.button({ label: 'Network map', icon: 'map', variant: 'primary', onClick: () => { ctx.net.setView('network'); ctx.navigate('map'); } }),
      ui.button({ label: 'Feed Check', icon: 'signal', href: '#/me/check' }));
    const tiles = {
      online: ui.stat({ label: 'Stations online', icon: 'antenna', accent: 'green' }),
      aircraft: ui.stat({ label: 'Aircraft', icon: 'plane', accent: 'cyan' }),
      mlat: ui.stat({ label: 'MLAT aircraft', icon: 'target', accent: 'amber' }),
      rate: ui.stat({ label: 'Messages', icon: 'signal', accent: 'blue', unit: '/s' }),
    };
    const quality = h('div');
    const coverage = h('div');
    const list = h('div');
    container.append(h('div.page-inner',
      head,
      h('div.grid.cols-4.net-tiles', Object.values(tiles)),
      h('div.grid.cols-3.net-network-grid',
        ui.card({ title: 'MLAT accuracy', icon: 'target', sub: 'Measured on held-out ADS-B aircraft', body: h('div.col.gap-3', quality, coverage) }),
        ui.card({ title: 'Public stations', icon: 'antenna', sub: 'Stations whose owners publish statistics', body: list, class: 'span-2' })),
      h('p.muted.net-privacy-note', ui.icon('shield'), PRIVACY_NOTE)));

    const renderSummary = (raw) => {
      const s = scrubPublic(normPublicSummary(raw));
      head.setSub([s.name, s.stationCounts ? `${units.int(s.stationCounts.online)} of ${units.int(s.stationCounts.total)} stations online` : null].filter(Boolean).join(' · '));
      tiles.online.set(v(s.stationCounts?.online, units.int));
      tiles.online.setSub(s.stationCounts ? `${units.int(s.stationCounts.total)} registered` : '');
      tiles.aircraft.set(v(s.aircraft, units.int));
      tiles.mlat.set(v(s.mlatAircraft, units.int));
      tiles.rate.set(v(s.messagesPerSec, (x) => units.num(x, 0)), '/s');
      const q = s.mlat;
      if (!q || !Number.isFinite(q.p50M)) {
        ui.mount(quality, ui.empty({ icon: 'target', title: 'No MLAT figures yet', text: 'They appear once the network has solved enough positions of aircraft that also send ADS-B.', compact: true }));
      } else {
        // Headline: the error estimated from the cross-track errors, free of
        // the ADS-B reference's own position latency. The raw comparison with
        // the ADS-B positions follows, labelled as such.
        const est = Number.isFinite(q.estP50M) && q.estP50M > 0;
        const rows = est
          ? [['Median (P50)', units.height(q.estP50M)], ['P95', units.height(q.estP95M)],
            ['P50 / P95 vs ADS-B', `${units.height(q.p50M)} / ${units.height(q.p95M)}`]]
          : [['Median (P50)', units.height(q.p50M)], ['P95', units.height(q.p95M)]];
        rows.push(['P99 vs ADS-B', units.height(q.p99M)], ['Fixes per minute', v(q.fixesPerMin, units.int)]);
        ui.mount(quality, h('div.col.gap-2', ui.kv(rows),
          est ? h('p.muted.small', 'Median and P95 are estimated from the cross-track error of held-out aircraft. The comparison with their ADS-B positions also contains the aircraft’s own position delay, so it reads higher.') : null));
      }
    };
    const renderStations = (raw) => {
      const rows = scrubPublic(normPublicStations(raw)).map((x) => ({ ...x, hasPin: !!publicPin(x) }));
      if (!rows.length) {
        ui.mount(list, ui.empty({ icon: 'antenna', title: 'No public stations', text: 'Owners choose whether their station is listed. None has opted in yet.', compact: true }));
        return;
      }
      ui.mount(list, ui.table({
        columns: [
          { key: 'name', label: 'Station', sort: true, render: (r) => h('span.row.gap-2', h('span.dot.' + (r.online ? 'ok' : 'off')), r.name || 'Unnamed station') },
          { key: 'rate', label: 'msg/s', align: 'right', num: true, sort: (r) => r.stats?.msgRate ?? null, render: (r) => v(r.stats?.msgRate, (x) => units.num(x, 0)) },
          { key: 'up', label: 'Uptime', align: 'right', num: true, sort: (r) => r.stats?.uptimePct ?? null, render: (r) => v(r.stats?.uptimePct, (x) => units.num(x, 1) + ' %') },
          { key: 'ac', label: 'Aircraft', align: 'right', num: true, tip: 'Aircraft now (or per day where the server reports it)', sort: (r) => r.stats?.aircraftDay ?? r.stats?.aircraft ?? null, render: (r) => v(r.stats?.aircraftDay ?? r.stats?.aircraft, units.int) },
          { key: 'pos', label: 'Positions', align: 'right', num: true, sort: (r) => r.stats?.positions ?? null, render: (r) => v(r.stats?.positions, units.si) },
          { key: 'mlat', label: 'MLAT', align: 'right', num: true, tip: 'Contributions to MLAT fixes (24 h)', sort: (r) => r.stats?.mlatContrib ?? null, render: (r) => v(r.stats?.mlatContrib, units.si) },
          { key: 'pin', label: 'Pin', render: (r) => (r.hasPin ? 'on map' : '—') },
        ],
        rows,
        sort: { key: 'rate', dir: 'desc' },
      }));
    };

    const renderCoverage = (r) => {
      const cells = Array.isArray(r?.cells) ? r.cells : [];
      const areas = new Set(cells.filter((c) => Number.isInteger(c?.lat) && Number.isInteger(c?.lon)).map((c) => c.lat + ':' + c.lon)).size;
      if (!areas) { ui.clear(coverage); return; }
      const showOnMap = () => {
        ui.prefs('airview.map.net', {}).set({ mlatq: 'measured', mlatqBand: -1 });
        ctx.net.setView('network');
        ctx.navigate('map');
      };
      ui.mount(coverage, h('p.muted', 'Measured in ' + units.int(areas) + ' one-degree ' + (areas === 1 ? 'area' : 'areas') + ' (only areas served by at least 5 stations are published).'),
        ui.button({ label: 'Accuracy map', icon: 'map', size: 'sm', onClick: showOnMap }));
    };

    let failed = false;
    const refresh = async () => {
      const [a, b, c] = await Promise.allSettled([
        api.get(EP.publicSummary(), { signal: ctx.signal }),
        api.get(EP.publicStations(), { signal: ctx.signal }),
        api.get(EP.publicMLAT(), { signal: ctx.signal }),
      ]);
      if (ctx.signal.aborted) return;
      if (c.status === 'fulfilled') renderCoverage(scrubPublic(c.value)); else ui.clear(coverage);
      // The accuracy figures come from /api/public/mlat (the summary carries none).
      const mq = c.status === 'fulfilled' ? c.value?.quality : null;
      if (a.status === 'fulfilled') renderSummary(mq && !a.value?.mlat ? { ...a.value, mlat: mq } : a.value);
      else if (!failed) { head.setSub(''); ui.mount(quality, failState(a.reason)); }
      if (b.status === 'fulfilled') renderStations(b.value);
      else if (!failed) ui.mount(list, failState(b.reason));
      failed = a.status === 'rejected' && b.status === 'rejected';
    };
    await refresh();
    ctx.every(30000, () => { if (!document.hidden) refresh(); });
  },

  onNet() { /* nothing station-specific on this page */ },
};
