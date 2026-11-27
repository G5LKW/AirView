// Station Location helpers (net/location.js, WP-W3-wire). Run: node --test web/dev/test
import test from 'node:test';
import assert from 'node:assert/strict';
import * as L from '../../static/js/net/location.js';

const near = (a, b, tol, msg) => assert.ok(Math.abs(a - b) <= tol, `${msg || ''} ${a} vs ${b} (±${tol})`);

test('distance, bearing and local offsets', () => {
  const a = { lat: 50, lon: 9 };
  const b = { lat: 50.001, lon: 9 }; // ~111 m north
  near(L.distanceM(a, b), 111.2, 0.5, 'north distance');
  near(L.bearingDeg(a, b), 0, 0.01, 'north bearing');
  const o = L.enuOffset(a, { lat: 50, lon: 9.001 });
  near(o.e, 71.5, 0.5, 'east offset at 50N');
  near(o.n, 0, 1e-9, 'no north offset');
  assert.equal(L.compass(44), 'NE');
  assert.equal(L.compass(359), 'N');
  assert.equal(L.compass(-90), 'W');
  // across the antimeridian
  near(L.enuOffset({ lat: 0, lon: 179.9995 }, { lat: 0, lon: -179.9995 }).e, 111.2, 0.5, 'antimeridian');
});

test('fmtDistance', () => {
  assert.equal(L.fmtDistance(12.4), '12 m');
  assert.equal(L.fmtDistance(1340), '1.3 km');
  assert.equal(L.fmtDistance(24400), '24 km');
  assert.equal(L.fmtDistance(NaN), '—');
});

test('compareDeclared: verdict against max(100 m, 3σ) or the survey limit', () => {
  const s = { lat: 50, lon: 9, sigmaHM: 5 };
  const close = L.compareDeclared({ lat: 50.0005, lon: 9 }, s); // 56 m north
  assert.ok(close.agrees);
  assert.equal(close.limitM, 100);
  assert.equal(close.dir, 'N');
  const far = L.compareDeclared({ lat: 50.003, lon: 9 }, s); // 334 m
  assert.ok(!far.agrees);
  const wide = L.compareDeclared({ lat: 50.003, lon: 9 }, { ...s, sigmaHM: 150 }); // limit 450 m
  assert.ok(wide.agrees);
  const lim = L.compareDeclared({ lat: 50.0005, lon: 9 }, { ...s, declaredLimitM: 30 });
  assert.ok(!lim.agrees);
  assert.equal(L.compareDeclared(null, s), null);
  assert.equal(L.compareDeclared({ lat: 95, lon: 0 }, s), null);
});

test('plotLayout: centre, scale, north up, ellipse rotation', () => {
  const lay = L.plotLayout([
    { id: 'survey', pos: { lat: 50, lon: 9 }, ellipse: { a: 20, b: 10, azDeg: 0 } },
    { id: 'declared', pos: { lat: 50.001, lon: 9 } },
  ], 240, 20);
  const sv = lay.items.find((i) => i.id === 'survey');
  const dc = lay.items.find((i) => i.id === 'declared');
  assert.equal(sv.x, 120);
  assert.equal(sv.y, 120);
  near(dc.x, 120, 1e-6, 'declared straight north: same x');
  assert.ok(dc.y < sv.y, 'north is up');
  assert.ok(dc.y >= 20 - 1e-6, 'inside the margin');
  near(sv.ellipse.rotDeg, -90, 1e-9, 'a north-pointing major axis is vertical');
  near(sv.ellipse.rx / sv.ellipse.ry, 2, 1e-9, 'axis ratio kept');
  // a lone point: at least ±25 m across
  const one = L.plotLayout([{ id: 'x', pos: { lat: 1, lon: 1 } }], 200, 0);
  near(one.spanM, 50, 1e-6, 'minimum span');
  assert.equal(L.plotLayout([{ id: 'x', pos: null }]), null);
  // a circle widens the extent
  const c = L.plotLayout([{ id: 'v2', pos: { lat: 1, lon: 1 }, r: 5000 }], 200, 0);
  near(c.items[0].r, 100, 1e-6, 'circle fills the half-size');
});

test('niceLength', () => {
  assert.equal(L.niceLength(73), 50);
  assert.equal(L.niceLength(199), 100);
  assert.equal(L.niceLength(2500), 2000);
  assert.equal(L.niceLength(0), 0);
});

test('survey wording', () => {
  assert.match(L.surveyStatusText({ enabled: false }), /not running/);
  assert.match(L.surveyStatusText({ enabled: true, running: true, observed: 12, reason: 'owner' }), /12 observations.*you asked/);
  assert.match(L.surveyStatusText({ enabled: true }), /Not surveyed yet/);
  assert.match(L.surveyStatusText({ enabled: true, last: { valid: true, stable: true, sigmaHM: 4.2 } }), /Measured \(±4 m\)/);
  assert.match(L.lastResultText({ valid: true, suspect: true, sigmaHM: 4 }), /consistency/);
  assert.match(L.lastResultText({ valid: false, state: 'nodata' }), /without enough data/);
  assert.match(L.lastResultText({ valid: true, stable: false, sigmaHM: 300 }), /no stable position/);
  assert.equal(L.reasonText('check'), 'the regular check of the declared position');
});

test('confirmableSurvey picks the newest stable survey estimate', () => {
  const es = [
    { id: '1', method: 'survey', status: 'stable', t: 10, lat: 1, lon: 1 },
    { id: '2', method: 'survey', status: 'unstable', t: 30, lat: 1, lon: 1 },
    { id: '3', method: 'survey', status: 'stable', t: 20, lat: 1, lon: 1 },
    { id: '4', method: 'locateme', status: 'stable', t: 40, lat: 1, lon: 1 },
  ];
  assert.equal(L.confirmableSurvey(es).id, '3');
  assert.equal(L.confirmableSurvey([]), null);
  assert.equal(L.locBadge('estimated').kind, 'cyan');
  assert.equal(L.locBadge('').label, 'Not located');
});
