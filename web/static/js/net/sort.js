// net/sort.js - whole-list sorting for the paged admin tables (net/pager.js). Pure: no DOM, tested by
// web/dev/test/pager.test.mjs.

/** Comparable value of a row for a column (sort: true | fn). */
function sortValue(col, row) {
  const v = typeof col.sort === 'function' ? col.sort(row) : row[col.key];
  if (v === null || v === undefined || (typeof v === 'number' && !Number.isFinite(v))) return null;
  return typeof v === 'string' ? v.toLowerCase() : v;
}

/** Sort rows by a column (nulls last in both directions; stable). */
export function sortBy(rows, col, dir) {
  const k = dir === 'desc' ? -1 : 1;
  return rows.map((r, i) => ({ r, i, v: sortValue(col, r) })).sort((a, b) => {
    if (a.v === null && b.v === null) return a.i - b.i;
    if (a.v === null) return 1;
    if (b.v === null) return -1;
    if (a.v < b.v) return -k;
    if (a.v > b.v) return k;
    return a.i - b.i;
  }).map((x) => x.r);
}

