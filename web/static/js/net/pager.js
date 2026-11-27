// net/pager.js - scale-friendly tables for the admin console (DESIGN §20.2: 1,000+ stations):
//
//   pagerBar(opts)    "1-50 of 1,234" with first / previous / next / last and a page-size choice; used by
//                     server-paged lists (GET /api/admin/stations?offset=&limit=&sort=&dir=) and by pagedTable
//   pagedTable(opts)  client-side search, sort and paging over a list that is already loaded (MLAT station
//                     table, survey console, share links, live links): only one page of rows is in the DOM
//
// Sorting covers the whole list (not just the visible page). Both keep their state while the data is
// refreshed (setRows), so a 10-second refresh does not jump back to page 1.

import { ui, h } from '../ui.js';
import { units } from '../units.js';
import { sortBy } from './sort.js';

export { sortBy };

export const PAGE_SIZES = [25, 50, 100, 250];

/**
 * Pager bar. opts: { pageSize = 50, onChange({ offset, limit }) }.
 * Returns el with .set({ total, offset, limit }) and .state().
 */
export function pagerBar(opts = {}) {
  let total = 0;
  let offset = 0;
  let limit = opts.pageSize || 50;
  const info = h('span.net-pager-info.muted', { 'aria-live': 'polite' });
  const go = (o) => {
    const max = Math.max(0, Math.floor((Math.max(total, 1) - 1) / limit) * limit);
    offset = Math.min(Math.max(0, o), max);
    draw();
    opts.onChange?.({ offset, limit });
  };
  const first = ui.button({ icon: 'chevrons-left', size: 'sm', variant: 'ghost', tip: 'First page', onClick: () => go(0) });
  const prev = ui.button({ icon: 'chevron-left', size: 'sm', variant: 'ghost', tip: 'Previous page', onClick: () => go(offset - limit) });
  const next = ui.button({ icon: 'chevron-right', size: 'sm', variant: 'ghost', tip: 'Next page', onClick: () => go(offset + limit) });
  const last = ui.button({ icon: 'chevrons-right', size: 'sm', variant: 'ghost', tip: 'Last page', onClick: () => go(Number.MAX_SAFE_INTEGER) });
  const size = ui.select({ size: 'sm', value: String(limit), options: PAGE_SIZES.map((n) => ({ value: String(n), label: `${n} per page` })),
    onChange: (x) => { limit = Number(x) || 50; offset = Math.floor(offset / limit) * limit; draw(); opts.onChange?.({ offset, limit }); } });
  const el = h('div.net-pager', info, h('span.grow'), size, h('div.row.gap-1', first, prev, next, last));
  function draw() {
    const from = total ? offset + 1 : 0;
    const to = Math.min(total, offset + limit);
    ui.setText(info, total ? `${units.int(from)}–${units.int(to)} of ${units.int(total)}` : 'Nothing to show');
    const atStart = offset <= 0;
    const atEnd = offset + limit >= total;
    first.disabled = prev.disabled = atStart;
    next.disabled = last.disabled = atEnd;
    el.classList.toggle('net-pager--single', total <= PAGE_SIZES[0] && limit >= total);
  }
  el.set = (s = {}) => {
    if (Number.isFinite(s.total)) total = Math.max(0, s.total);
    if (Number.isFinite(s.limit)) limit = s.limit;
    if (Number.isFinite(s.offset)) offset = s.offset;
    if (offset >= total && total > 0) offset = Math.floor((total - 1) / limit) * limit;
    draw();
  };
  el.state = () => ({ total, offset, limit });
  draw();
  return el;
}

/**
 * Client-side paged table.
 * opts: { columns (as ui.table; `sort: true | fn` sorts the whole list), rows, sort: { key, dir }, pageSize,
 *         search(row) -> string (enables the search box), placeholder, empty, rowClass, onRowClick, filters: [nodes] }
 * Returns el with .setRows(rows) (keeps query, sort and page), .count().
 */
export function pagedTable(opts = {}) {
  let rows = opts.rows || [];
  let sort = opts.sort ? { ...opts.sort } : null;
  let query = '';
  let offset = 0;
  let limit = opts.pageSize || 50;
  const host = h('div');
  const pager = pagerBar({ pageSize: limit, onChange: (s) => { offset = s.offset; limit = s.limit; draw(); } });
  const search = opts.search ? ui.input({ placeholder: opts.placeholder || 'Search', icon: 'search', size: 'sm', clearable: true,
    onInput: ui.debounce((x) => { query = String(x || '').trim().toLowerCase(); offset = 0; draw(); }, 200) }) : null;
  const count = h('span.muted');
  const bar = search || opts.filters ? h('div.row.gap-2.net-filters', search, ...(opts.filters || []), h('span.grow'), count) : null;
  const cols = (opts.columns || []).map((c) => {
    if (!c.sort) return c;
    const btn = h('button.net-th-sort', { type: 'button', tip: c.tip }, c.label ?? '', h('span.net-th-ind'));
    btn.addEventListener('click', () => {
      sort = { key: c.key, dir: sort && sort.key === c.key ? (sort.dir === 'asc' ? 'desc' : 'asc') : c.sortDir || 'asc' };
      offset = 0;
      draw();
    });
    return { ...c, label: btn, __btn: btn, sort: undefined, tip: undefined };
  });
  const el = h('div.col.gap-2.net-paged', bar, host, pager);
  function filtered() {
    if (!query || !opts.search) return rows;
    return rows.filter((r) => String(opts.search(r) || '').toLowerCase().includes(query));
  }
  function draw() {
    let list = filtered();
    const col = sort && (opts.columns || []).find((c) => c.key === sort.key && c.sort);
    if (col) list = sortBy(list, col, sort.dir);
    for (const c of cols) {
      if (!c.__btn) continue;
      const on = sort && sort.key === c.key;
      c.__btn.setAttribute('aria-sort', on ? (sort.dir === 'desc' ? 'descending' : 'ascending') : 'none');
      ui.setText(c.__btn.querySelector('.net-th-ind'), on ? (sort.dir === 'desc' ? ' ↓' : ' ↑') : '');
    }
    if (offset >= list.length && list.length) offset = Math.floor((list.length - 1) / limit) * limit;
    pager.set({ total: list.length, offset, limit });
    ui.setText(count, query ? `${units.int(list.length)} of ${units.int(rows.length)}` : '');
    ui.mount(host, ui.table({ columns: cols, rows: list.slice(offset, offset + limit), empty: query ? 'Nothing matches' : opts.empty,
      rowClass: opts.rowClass, onRowClick: opts.onRowClick, fixed: opts.fixed }));
  }
  el.setRows = (r) => { rows = r || []; draw(); };
  el.count = () => rows.length;
  draw();
  return el;
}
