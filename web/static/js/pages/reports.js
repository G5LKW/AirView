// pages/reports.js - Reports & Replay.
//
// Views (sub-routes):
//   #/reports               report builder: time range + criteria, sorted / paged flights, CSV (reports/builder.js)
//   #/reports?from=&to=     the same, pre-filled with a time range (the daily summary links here)
//   #/reports/flight/<id>   one stored flight: facts, airports, trace map, profiles, KML / GeoJSON / CSV (reports/flight.js)
//   #/reports/daily         daily summary per day (reports/daily.js)
//   #/reports/replay        time-lapse replay of stored traffic on a full-size map (reports/replay.js)
// Each view lives in its own scope that is disposed when the view changes; the page itself only switches views.

import { makeScope } from '../reports/common.js';
import { mountBuilder } from '../reports/builder.js';
import { mountFlight } from '../reports/flight.js';
import { mountDaily } from '../reports/daily.js';
import { mountReplay } from '../reports/replay.js';

const CSS = new URL('../../css/reports.css', import.meta.url);
const VIEWS = [
  { id: 'report', label: 'Report', icon: 'report', href: '#/reports' },
  { id: 'daily', label: 'Daily summary', icon: 'chart', href: '#/reports/daily' },
  { id: 'replay', label: 'Replay', icon: 'replay', href: '#/reports/replay' },
];

function viewOf(route) {
  const a = route?.args || [];
  if (a[0] === 'replay') return { id: 'replay', key: 'replay' };
  if (a[0] === 'daily') return { id: 'daily', key: 'daily' };
  if (a[0] === 'flight' && a[1]) return { id: 'flight', key: 'flight/' + a[1], arg: a[1] };
  const q = route?.query || {};
  return { id: 'report', key: 'report?' + (q.from || '') + '-' + (q.to || '') };
}

let active = null;

export default {
  id: 'reports',
  title: 'Reports & Replay',

  async mount(container, ctx) {
    const { ui } = ctx;
    const { h } = ui;
    await ui.loadCss(CSS);
    const state = { view: null, scope: null };
    active = state;

    const nav = (current) => h('nav.rp-nav', { 'aria-label': 'Reports views' }, VIEWS.map((v) => h('a.rp-nav-item' + (v.id === current || (current === 'flight' && v.id === 'report') ? '.active' : ''),
      { href: v.href, 'aria-current': v.id === current ? 'page' : null }, ui.icon(v.icon), h('span', v.label))));

    function show(route) {
      const v = viewOf(route);
      if (state.view && state.view.key === v.key) return;
      if (state.scope) state.scope.dispose();
      state.scope = makeScope(ctx);
      state.view = v;
      const bleed = v.id === 'replay';
      container.classList.toggle('page--bleed', bleed);
      container.classList.toggle('rp-page', !bleed);
      ui.clear(container);
      container.scrollTop = 0;
      try {
        if (bleed) {
          ctx.setTitle('Replay');
          mountReplay(container, ctx, state.scope, nav('replay'));
          return;
        }
        const host = h('div.rp-view');
        const sub = { report: 'Search stored flights VirtualRadar-style, export them, open a flight', flight: 'Stored flight', daily: 'Per-day totals, farthest flight and busiest hour' }[v.id];
        container.append(h('div.page-inner.rp',
          h('div.page-head', h('div', h('h2', 'Reports & Replay'), h('div.sub', sub)), h('div.actions', nav(v.id))),
          host));
        ctx.setTitle(v.id === 'daily' ? 'Daily summary' : 'Reports & Replay');
        if (v.id === 'flight') mountFlight(host, ctx, state.scope, v.arg);
        else if (v.id === 'daily') mountDaily(host, ctx, state.scope);
        else mountBuilder(host, ctx, state.scope);
      } catch (e) {
        console.error('[reports] view failed', v.id, e);
        ui.mount(container, h('div.page-inner', ui.error(e, { title: 'This view could not be shown', retry: () => { state.view = null; show(ctx.route); } })));
      }
    }

    state.show = show;
    ctx.onCleanup(() => { if (state.scope) state.scope.dispose(); state.scope = null; if (active === state) active = null; });
    show(ctx.route);
  },

  onRoute(route) { if (active?.show) active.show(route); },

  unmount() {},
};
