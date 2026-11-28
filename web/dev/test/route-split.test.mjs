// route-split.test.mjs - the selected aircraft's route line, split at its position (map/route-split.js).
import test from 'node:test';
import assert from 'node:assert/strict';
import { splitPath, routeLineFeatures, routeColor, ROUTE_STATE_COLOR } from '../../static/js/map/route-split.js';

const path = [[0, 50], [1, 50], [2, 50], [3, 50]];

test('splits at the nearest point of the nearest segment', () => {
  const s = splitPath(path, [1.5, 50.05]);
  assert.deepEqual(s.flown, [[0, 50], [1, 50], [1.5, 50]]);
  assert.deepEqual(s.remaining, [[1.5, 50], [2, 50], [3, 50]]);
});

test('at the ends one part is a single point (null)', () => {
  const a = splitPath(path, [-0.5, 50]);
  assert.equal(a.flown, null);
  assert.deepEqual(a.remaining, path);
  const b = splitPath(path, [3.5, 50]);
  assert.equal(b.remaining, null);
  assert.equal(b.flown.length, 4);
  assert.equal(routeLineFeatures(path, [3.5, 50], '#fff').map((f) => f.properties.part).join(), 'flown');
});

test('far from the path: no split, the whole path is remaining', () => {
  assert.equal(splitPath(path, [1, 40]), null);
  const f = routeLineFeatures(path, [1, 40], '#fff');
  assert.equal(f.length, 1);
  assert.equal(f[0].properties.part, 'remaining');
});

test('works across the antimeridian (continuous longitudes)', () => {
  const p = [[178, 10], [180, 10], [182, 10]];
  const s = splitPath(p, [-179, 10]); // = 181
  assert.ok(Math.abs(s.flown[s.flown.length - 1][0] - 181) < 1e-9);
});

test('colour by state', () => {
  assert.equal(routeColor('doubtful'), ROUTE_STATE_COLOR.doubtful);
  assert.equal(routeColor('uncertain'), ROUTE_STATE_COLOR.uncertain);
  assert.equal(routeColor(undefined, { doubtful: true }), ROUTE_STATE_COLOR.doubtful);
  assert.equal(routeColor(undefined, null), ROUTE_STATE_COLOR.fits);
});
