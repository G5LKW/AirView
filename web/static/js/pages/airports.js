// pages/airports.js - airport boards (#/airports/{code}, docs/ROUTES-DESIGN.md §6.5): arrivals, departures and
// recently landed flights of one airport, built from the aircraft this station receives and the routes it resolved
// for them (not a schedule). Without a code: a search box (GET /api/navdata/search).
//
// API: GET /api/routes/board/{code} -> { airport, arrivals[], departures[], landed[], generated, note }.

const REFRESH_MS = 15000;

/** Formats an ETA (Unix s) with its sigma (s) as "14:32 ±3 min" in the viewer's time zone. */
export function etaText(eta, sigma) {
  if (!(eta > 0)) return '';
  const t = new Date(eta * 1000).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' });
  return sigma > 0 ? `${t} ±${Math.max(1, Math.round(sigma / 60))} min` : t;
}

/** The code shown for an airport: IATA when known, else ICAO. */
export const apCode = (icao, iata) => iata || icao || '';

export default {
  id: 'airports',
  title: 'Airports',

  async mount(container, ctx) {
    const { api, ui, units } = ctx;
    const { h } = ui;
    const code = (ctx.route.args[0] || '').toUpperCase();
    const inner = h('div.page-inner');
    container.append(inner);

    const q = ui.input({ placeholder: 'Airport code or name, e.g. LHR or Frankfurt', icon: 'search', value: code, clearable: true });
    const hits = h('div.col.gap-1');
    let searchSeq = 0;
    async function search(text) {
      const my = ++searchSeq;
      text = String(text || '').trim();
      if (text.length < 2) { ui.clear(hits); return; }
      try {
        const r = await api.get('/api/navdata/search', { params: { q: text, limit: 8 }, signal: ctx.signal, timeout: 8000 });
        if (my !== searchSeq) return;
        const list = Array.isArray(r) ? r : r?.airports || [];
        ui.mount(hits, list.length
          ? list.map((a) => h('a.row.gap-2', { href: '#/airports/' + encodeURIComponent(a.icao || a.ident || a.iata) },
            h('b.num', a.iata || a.icao || a.ident), h('span', a.name), h('span.muted', [a.municipality, a.country].filter(Boolean).join(', '))))
          : h('div.muted', 'No airport found. Import the reference data in Settings > Aviation data if this list stays empty.'));
      } catch (e) {
        if (!e.aborted && my === searchSeq) ui.mount(hits, h('div.muted', e.message));
      }
    }
    let t = null;
    q.input.addEventListener('input', () => { clearTimeout(t); t = setTimeout(() => search(q.value), 250); });
    q.input.addEventListener('keydown', (e) => { if (e.key === 'Enter' && hits.querySelector('a')) location.hash = hits.querySelector('a').getAttribute('href'); });
    ctx.onCleanup(() => clearTimeout(t));
    const searchCard = ui.card({ title: 'Find an airport', icon: 'search', body: h('div.col.gap-2', q, hits) });

    if (!code) {
      ctx.setTitle('Airports');
      inner.append(h('div.page-head', h('div', h('h2', 'Airport boards'), h('div.muted', 'Arrivals and departures from the traffic this station receives'))), searchCard);
      return;
    }

    const body = h('div.col.gap-4', ui.skeleton({ lines: 6 }));
    const head = h('div.page-head', h('div', h('h2', code), h('div.muted', 'Airport board')));
    inner.append(head, body, searchCard);

    const routeCell = (r, which) => {
      const c = which === 'from' ? apCode(r.from, r.fromIata) : apCode(r.to, r.toIata);
      const n = which === 'from' ? r.fromName : r.toName;
      return c ? h('span', { tip: n || '' }, h('b.num', c), n ? h('span.muted', ' ' + n) : null) : units.DASH;
    };
    const stateBadge = (r) => (r.state === 'doubtful' ? ui.badge('doubtful', 'amber', { tip: 'The track does not fit this route well' })
      : r.state === 'uncertain' ? ui.badge('uncertain', 'other') : null);
    const csCell = (r) => (r.icao ? h('a', { href: '#/map/' + r.icao }, h('b', r.callsign || r.icao)) : h('b', r.callsign));
    const conf = (r) => (r.conf ? `${Math.round(r.conf * 100)} %` : '');

    const tables = {
      arrivals: ui.table({ empty: 'No inbound aircraft with a resolved route right now.', columns: [
        { key: 'callsign', label: 'Flight', render: csCell },
        { key: 'from', label: 'From', render: (r) => routeCell(r, 'from') },
        { key: 'type', label: 'Type' },
        { key: 'eta', label: 'ETA', num: true, render: (r) => etaText(r.eta, r.etaSigma) || units.DASH, tip: 'Estimated from distance, speed and a standard descent profile' },
        { key: 'distKm', label: 'Distance', num: true, render: (r) => (r.distKm ? units.dist(r.distKm) : units.DASH) },
        { key: 'conf', label: 'Route', render: (r) => h('span.row.gap-1', h('span.muted', conf(r)), stateBadge(r)) },
      ] }),
      departures: ui.table({ empty: 'No departing aircraft climbing out right now.', columns: [
        { key: 'callsign', label: 'Flight', render: csCell },
        { key: 'to', label: 'To', render: (r) => routeCell(r, 'to') },
        { key: 'type', label: 'Type' },
        { key: 'altFt', label: 'Altitude', num: true, render: (r) => (typeof r.altFt === 'number' ? units.alt(r.altFt, { fl: true }) : units.DASH) },
        { key: 'distKm', label: 'Distance', num: true, render: (r) => (r.distKm ? units.dist(r.distKm) : units.DASH) },
        { key: 'conf', label: 'Route', render: (r) => h('span.row.gap-1', h('span.muted', conf(r)), stateBadge(r)) },
      ] }),
      landed: ui.table({ empty: 'No landing observed here in the last hour.', columns: [
        { key: 'callsign', label: 'Flight', render: (r) => h('b', r.callsign) },
        { key: 'from', label: 'From', render: (r) => (r.from ? h('b.num', r.from) : units.DASH) },
        { key: 't', label: 'Landed', num: true, render: (r) => (r.t ? units.ago(r.t * 1000) : units.DASH) },
        { key: 'conf', label: 'Landing evidence', num: true, render: conf, tip: 'How sure the landing at this airport is' },
      ] }),
    };
    let built = false;
    async function refresh() {
      try {
        const b = await api.get('/api/routes/board/' + encodeURIComponent(code), { signal: ctx.signal, timeout: 10000 });
        const ap = b.airport || {};
        ctx.setTitle(`${ap.iata || ap.code} board`);
        ui.mount(head, h('div', h('h2', [ap.iata, ap.code].filter(Boolean).join(' / ')), h('div.muted', ap.name || '')),
          h('div.actions', ui.button({ label: 'Show on map', icon: 'map', size: 'sm', href: `#/map?lat=${ap.lat}&lon=${ap.lon}&zoom=9` })));
        tables.arrivals.setRows(b.arrivals || []);
        tables.departures.setRows(b.departures || []);
        tables.landed.setRows(b.landed || []);
        if (!built) {
          built = true;
          ui.mount(body,
            ui.card({ title: 'Arrivals', icon: 'plane', body: tables.arrivals }),
            ui.card({ title: 'Departures', icon: 'plane', body: tables.departures }),
            ui.card({ title: 'Recently landed', icon: 'history', body: tables.landed }),
            h('p.muted', ui.icon('info'), ' ', b.note || ''));
        }
      } catch (e) {
        if (e.aborted) return;
        built = false;
        ui.mount(body, ui.empty({ icon: 'alert', title: e.status === 404 ? `Unknown airport ${code}` : 'Board unavailable',
          text: e.status === 404 ? 'Check the code, or import the reference data in Settings > Aviation data.' : e.message }));
      }
    }
    await refresh();
    ctx.every(REFRESH_MS, () => { if (!document.hidden) refresh(); });
  },
};
