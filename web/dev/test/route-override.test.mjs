// route-override.test.mjs - the "Correct route…" dialog's request building (map/route-override.js).
import test from 'node:test';
import assert from 'node:assert/strict';
import { normAirports, overrideBody, prefillAirports } from '../../static/js/map/route-override.js';

test('normAirports accepts 2..8 codes in any separator and upper-cases them', () => {
  assert.equal(normAirports('egll kjfk'), 'EGLL-KJFK');
  assert.equal(normAirports('LHR, FRA > FCO'), 'LHR-FRA-FCO');
  assert.equal(normAirports('EGLL'), null);
  assert.equal(normAirports('EGLL-K!FK'), null);
  assert.equal(normAirports(Array(9).fill('EGLL').join('-')), null);
});

test('overrideBody validates per mode and sets the expiry', () => {
  assert.deepEqual(overrideBody({ mode: 'fixed', airports: 'egll-kjfk', note: ' seasonal ' }).body, { mode: 'fixed', airports: 'EGLL-KJFK', note: 'seasonal', expires: 0 });
  assert.ok(overrideBody({ mode: 'verify', airports: 'EGLL' }).error);
  const b = overrideBody({ mode: 'block', airports: '' }).body;
  assert.equal(b.mode, 'block');
  assert.equal(b.airports, undefined);
  const now = Math.floor(Date.now() / 1000);
  const e = overrideBody({ mode: 'fixed', airports: 'EGLL KJFK', days: 2 }).body.expires;
  assert.ok(e >= now + 2 * 86400 - 2 && e <= now + 2 * 86400 + 2);
  assert.equal(overrideBody({ mode: 'bogus', airports: 'EGLL KJFK' }).body.mode, 'fixed');
});

test('prefillAirports uses the whole stop list of multi-stop routes', () => {
  assert.equal(prefillAirports({ from: 'EGLL', to: 'KJFK' }), 'EGLL-KJFK');
  assert.equal(prefillAirports({ from: 'EDDF', to: 'LIRF', stops: ['EGLL', 'EDDF', 'LIRF'] }), 'EGLL-EDDF-LIRF');
  assert.equal(prefillAirports(null), '');
});
