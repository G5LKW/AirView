// Phase 4-5 UI helpers (WP-W45-web): whole-list sorting of the paged admin tables, the coverage-improvement map
// layer, share-link endpoints and the share page's access rule.
import test from 'node:test';
import assert from 'node:assert/strict';
import { sortBy } from '../../static/js/net/sort.js';
import { improveCellsFc, IMPROVE_SCALE } from '../../static/js/mlat/model.js';
import { EP } from '../../static/js/net/endpoints.js';
import * as M from '../../static/js/net/model.js';

test('sortBy sorts the whole list, nulls last, stable, case-insensitive', () => {
  const rows = [{ n: 'b', v: 2 }, { n: 'A', v: null }, { n: 'c', v: 1 }, { n: 'a', v: 2 }, { n: 'D', v: undefined }, { n: 'e', v: NaN }];
  const byV = sortBy(rows, { key: 'v', sort: true }, 'asc').map((r) => r.n);
  assert.deepEqual(byV, ['c', 'b', 'a', 'A', 'D', 'e']);
  const byVd = sortBy(rows, { key: 'v', sort: true }, 'desc').map((r) => r.n);
  assert.deepEqual(byVd, ['b', 'a', 'c', 'A', 'D', 'e']);
  const byN = sortBy(rows, { key: 'n', sort: (r) => r.n }, 'asc').map((r) => r.n);
  assert.deepEqual(byN, ['A', 'a', 'b', 'c', 'D', 'e']);
  // 1,200 stations: page 2 of 50 continues page 1 of the same order
  const many = Array.from({ length: 1200 }, (_, i) => ({ id: i, rate: (i * 7919) % 1000 }));
  const s = sortBy(many, { key: 'rate', sort: true }, 'desc');
  assert.equal(s.length, 1200);
  for (let i = 1; i < s.length; i++) assert.ok(s[i - 1].rate >= s[i].rate);
});

test('improveCellsFc colours by benefit, labels the ranked sites and drops < 5 %', () => {
  const r = { stepDeg: 0.5, sites: [{ lat: 50.25, lon: 8.25 }], cells: [
    { lat: 50.25, lon: 8.25, score: 1 }, { lat: 50.75, lon: 8.25, score: 0.3 }, { lat: 51.25, lon: 8.25, score: 0.01 }, { lat: 'x', lon: 1, score: 1 }] };
  const fc = improveCellsFc(r);
  assert.equal(fc.features.length, 2);
  assert.equal(fc.features[0].properties.g, '#1');
  assert.equal(fc.features[0].properties.c, IMPROVE_SCALE[0].color);
  assert.equal(fc.features[1].properties.g, '30 %');
  const ring = fc.features[0].geometry.coordinates[0];
  assert.deepEqual(ring[0], [8, 50]);
  assert.deepEqual(ring[2], [8.5, 50.5]);
  assert.equal(improveCellsFc(null).features.length, 0);
});

test('share endpoints and the share page are public; the token is path-escaped', () => {
  assert.equal(EP.share('ab_c-1'), '/api/share/ab_c-1');
  assert.equal(EP.shareAircraft('a/b'), '/api/share/a%2Fb/aircraft');
  assert.equal(EP.shareStats('t'), '/api/share/t/stats');
  assert.equal(EP.myShare('x y'), '/api/me/shares/x%20y');
  assert.equal(M.PAGE_ACCESS.share, 'public');
  assert.equal(M.pageAccess('share', { mode: 'aggregator', scope: 'public' }).ok, true);
  // the share page is never in a sidebar
  for (const s of ['public', 'owner', 'admin']) assert.ok(!M.navFor(s).includes('share'));
});
