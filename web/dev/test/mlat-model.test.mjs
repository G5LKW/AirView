// MLAT views: grades and map ring styles, the station normaliser (owner privacy), history columns, the sync
// matrix ordering, CRLB rows and the viewport box / auto hysteresis of the live stream. Run: node --test web/dev/test
import test from 'node:test';
import assert from 'node:assert/strict';
import * as X from '../../static/js/mlat/model.js';
import { publicAllowed } from '../../static/js/net/model.js';

test('grades and ring styles', () => {
  assert.equal(X.gradeOf(50), 'A');
  assert.equal(X.gradeOf(51), 'B');
  assert.equal(X.gradeOf(150), 'B');
  assert.equal(X.gradeOf(500), 'C');
  assert.equal(X.gradeOf(1999), 'D');
  assert.equal(X.gradeOf(2500), '');
  assert.equal(X.gradeOf(NaN), '');
  assert.equal(X.mlatRing({ source: 'adsb', mlatGrade: 'A' }), null);
  assert.deepEqual(X.mlatRing({ source: 'mlat', mlatGrade: 'A' }), { c: X.GRADE_COLOR.A, w: 2.2 });
  assert.equal(X.mlatRing({ source: 'mlat' }).c, X.MLAT_UNGRADED);
  // better grades draw thicker rings; every grade has its own colour
  const w = X.GRADES.map((g) => g.width);
  assert.deepEqual([...w].sort((a, b) => b - a), w);
  assert.equal(new Set(X.GRADES.map((g) => g.color)).size, X.GRADES.length);
});

test('station normaliser: owner answer', () => {
  const m = X.normStationMLAT({
    running: true, known: true, positionSource: 'declared',
    clock: { clockClass: 'gps_utc', synced: true, sigmaThetaNs: 25, driftPpm: 0.001, steps: 2, syncedPeers: 2,
      events: [{ t: 1000, reason: 'step' }, { t: 3000, reason: 'promoted' }, { reason: 'no time' }],
      peers: [{ tag: 'aa11bb', band: 'near', count: 3, rmsNs: 80 }, { tag: 'cc22dd', band: 'far', count: 9, rmsNs: 120 }] },
    mlat: { contribPerHour: 321, residualBiasNs: -12, biasFlag: true },
    summary: { peers: 2, syncedPeers: 2, contribPerHour: 321, timedPerMin: 900, resultsEligible: true, resultsReceived: null },
    history: { t: [1, 2], sigmaThetaNs: [20, null] },
  });
  assert.equal(m.running, true);
  assert.deepEqual(m.peers.map((p) => p.id), ['cc22dd', 'aa11bb']); // busiest first
  assert.equal(m.peers[0].sid, '');
  assert.deepEqual(m.clock.events.map((e) => e.reason), ['promoted', 'step']); // newest first, invalid dropped
  assert.equal(m.summary.resultsReceived, undefined);
  assert.equal(m.mlat.biasFlag, true);
  assert.equal(m.history.t.length, 2);
  // engine off / empty answer
  const off = X.normStationMLAT({ running: false });
  assert.equal(off.running, false);
  assert.equal(off.clock, null);
  assert.deepEqual(off.peers, []);
});

test('history columns', () => {
  const h = { t: [10, 20, 30], trackP50M: [5, 6, 7], trackN: [2, 10, 30], solvesPerMin: [null, 12, 14] };
  assert.deepEqual(X.series(h, 'solvesPerMin'), [[10, null], [20, 12], [30, 14]]);
  assert.deepEqual(X.seriesMinN(h, 'trackP50M', 'trackN', 5), [[10, null], [20, 6], [30, 7]]);
  assert.equal(X.last(h, 'solvesPerMin'), 14);
  assert.equal(X.last({ t: [1], x: [null] }, 'x'), undefined);
  assert.deepEqual(X.series(null, 'x'), []);
});

test('sync matrix: components on the diagonal, bounded size', () => {
  const st = (sid) => ({ sid, name: sid.toUpperCase() });
  const r = {
    stations: ['a', 'b', 'c', 'd', 'e', 'z'].map(st),
    edges: [
      { a: 'a', b: 'b', count: 10, rmsNs: 50 }, { a: 'b', b: 'a', count: 10, rmsNs: 55 }, { a: 'b', b: 'c', count: 4, rmsNs: 90 },
      { a: 'd', b: 'e', count: 3, rmsNs: 400, relPpb: -7 },
    ],
  };
  const m = X.syncMatrix(r);
  assert.equal(m.components, 3); // {a,b,c}, {d,e}, {z}
  assert.deepEqual(m.ids.slice(0, 3).sort(), ['a', 'b', 'c']);
  assert.deepEqual(m.ids.slice(3, 5).sort(), ['d', 'e']);
  assert.equal(m.ids[5], 'z');
  assert.equal(m.labels[m.ids.indexOf('a')], 'A');
  assert.equal(m.cells.length, 4);
  const de = m.cells.find(([x, y]) => m.ids[x] === 'd' && m.ids[y] === 'e');
  assert.equal(de[2], 400);
  assert.equal(X.syncMatrix(r, { metric: 'relPpb' }).cells.find(([x]) => m.ids[x] === 'd')[2], 7);
  const small = X.syncMatrix(r, { max: 2 });
  assert.equal(small.ids.length, 2);
  assert.equal(small.dropped, 4);
  assert.deepEqual(small.ids.sort(), ['a', 'b']);
  assert.deepEqual(X.syncMatrix(null).ids, []);
});

test('CRLB rows', () => {
  const rows = X.crlbRows({ summary: [{ altM: 914.4, cells: 3, areaKm2A: 10, areaKm2B: 5, areaKm2C: 1, areaKm2D: 100 }, { altM: 9144 }] });
  assert.equal(rows[0].altFt, 3000);
  assert.equal(rows[1].altFt, 30000);
  assert.equal(rows[0].areaKm2Shown, 16);
});

test('viewport box', () => {
  // padded 15 % (at least 0.05°) and rounded
  assert.deepEqual(X.viewportBox(8, 50, 10, 52), [49.7, 7.7, 52.3, 10.3]);
  // panned across the antimeridian: east beyond 180 wraps, west > east marks the crossing
  const b = X.viewportBox(170, -20, 190, -10);
  assert.ok(b[1] > b[3], 'crossing box ' + b);
  assert.equal(b[1], 167);
  assert.equal(b[3], -167);
  // east edge exactly on the antimeridian stays 180, not -180
  assert.equal(X.viewportBox(170, 0, 179.95, 1, 0.001)[3], 180);
  // whole world or degenerate -> null
  assert.equal(X.viewportBox(-200, -80, 200, 80), null);
  assert.equal(X.viewportBox(-180, -95, 180, 95), null);
  assert.equal(X.viewportBox(5, 5, 5, 6), null);
  assert.equal(X.viewportBox(NaN, 0, 1, 1), null);
  assert.equal(X.bboxParam([1, 2, 3, 4]), '1,2,3,4');
});

test('viewport auto hysteresis', () => {
  assert.equal(X.wantViewport('on', 10, false), true);
  assert.equal(X.wantViewport('off', 1e6, true), false);
  assert.equal(X.wantViewport('auto', X.VIEWPORT_ON + 1, false), true);
  assert.equal(X.wantViewport('auto', X.VIEWPORT_ON - 1, false), false);
  assert.equal(X.wantViewport('auto', X.VIEWPORT_OFF + 1, true), true); // stays on between the thresholds
  assert.equal(X.wantViewport('auto', X.VIEWPORT_OFF - 1, true), false);
  assert.equal(X.wantViewport('auto', undefined, true), true);
});

test('public viewers may move their viewport', () => {
  assert.equal(publicAllowed('/api/events/view'), true);
  assert.equal(publicAllowed('/api/events/view?id=x&bbox=1,2,3,4'), true);
  assert.equal(publicAllowed('/api/admin/mlat'), false);
  assert.equal(publicAllowed('/api/me/stations/x/mlat'), false);
  assert.equal(publicAllowed('/api/public/mlat'), true);
});

test('quality layers: measured cells and CRLB squares', () => {
  const cells = [
    { lat: 50, lon: 8, band: 0, n: 40, p50M: 120, p95M: 400 },
    { lat: 50, lon: 8, band: 2, n: 80, p50M: 35, p95M: 90 },
    { lat: 51, lon: 9, band: 1, n: 25, p50M: 700, p95M: null },
    { lat: 51.5, lon: 9, band: 1, n: 25, p50M: 10 }, // not a 1-degree corner
    { lat: 52, lon: 9, band: 1, n: 25, p50M: NaN },
  ];
  const all = X.qualityCellsFc(cells, -1);
  assert.equal(all.features.length, 2);
  const c50 = all.features.find((f) => f.geometry.coordinates[0][0][1] === 50);
  assert.equal(c50.properties.p50, 35); // the best band of the cell
  assert.equal(c50.properties.c, X.GRADE_COLOR.A);
  assert.deepEqual(c50.geometry.coordinates[0], [[8, 50], [9, 50], [9, 51], [8, 51], [8, 50]]);
  const low = X.qualityCellsFc(cells, 0);
  assert.equal(low.features.length, 1);
  assert.equal(low.features[0].properties.c, X.GRADE_COLOR.B);
  const mid = X.qualityCellsFc(cells, 1);
  assert.equal(mid.features[0].properties.c, '#f87171');
  assert.equal(mid.features[0].properties.p95, null);
  assert.equal(X.qualityCellsFc(null).features.length, 0);
  assert.equal(X.qualityColor(50), X.GRADE_COLOR.A);
  assert.equal(X.qualityColor(51), X.GRADE_COLOR.B);
  assert.equal(X.qualityColor(NaN), null);

  const crlb = { stepDeg: 0.5, cells: [
    { lat: 50, lon: 8, altM: 3048, sigmaHM: 40, n: 5 },
    { lat: 50.5, lon: 8, altM: 3048, sigmaHM: 600, n: 4 },
    { lat: 50, lon: 8, altM: 914.4, sigmaHM: 90, n: 4 },
  ] };
  assert.deepEqual(X.crlbHeights(crlb), [914.4, 3048]);
  const sq = X.crlbCellsFc(crlb, 3048);
  assert.equal(sq.features.length, 2);
  assert.deepEqual(sq.features[0].geometry.coordinates[0][0], [7.75, 49.75]);
  assert.equal(sq.features[1].properties.c, '#f87171');
  assert.equal(X.heightLabel(3048), 'FL100');
  assert.equal(X.heightLabel(9144), 'FL300');
  assert.equal(X.heightLabel(914.4), '3,000 ft');
});
