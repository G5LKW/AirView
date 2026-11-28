// route-query.test.mjs - the list's route filter grammar (map/route-query.js).
import test from 'node:test';
import assert from 'node:assert/strict';
import { parseQuery, matchTerms } from '../../static/js/map/route-query.js';

const now = 1_800_000_000;
const ac = { callsign: 'BAW117 ', route: { from: 'EGLL', to: 'KJFK', fromIata: 'LHR', toIata: 'JFK', state: 'fits', source: 'learned', kind: 'charter', eta: now + 20 * 60, stops: ['EGLL', 'EINN', 'KJFK'] } };
const bare = { callsign: 'DLH4AB' };
const m = (q, a = ac) => { const p = parseQuery(q); return matchTerms(a, p.terms, now); };

test('plain text stays plain', () => {
  assert.deepEqual(parseQuery('baw 3c6444').plain, ['baw', '3c6444']);
  assert.deepEqual(parseQuery('foo:bar').plain, ['foo:bar']);
  assert.equal(parseQuery('to:').terms.length, 0);
});

test('airport terms match ICAO and IATA, with wildcards and negation', () => {
  assert.ok(m('to:JFK'));
  assert.ok(m('to:kjfk'));
  assert.ok(m('from:EG*'));
  assert.ok(!m('from:K*'));
  assert.ok(m('-to:EGLL'));
  assert.ok(!m('-to:JFK'));
  assert.ok(m('via:EINN'));
  assert.ok(!m('via:EGLL'));
  assert.ok(m('airport:EINN') && m('airport:LHR'));
  assert.ok(!m('to:JFK', bare));
});

test('route, airline, state, source, kind, eta and noroute', () => {
  assert.ok(m('route:LHR-JFK'));
  assert.ok(!m('route:JFK-LHR'));
  assert.ok(m('route:JFK~LHR'));
  assert.ok(m('airline:BAW'));
  assert.ok(m('airline:BAW', { callsign: 'BAW117', route: null }));
  assert.ok(m('state:fits') && !m('state:doubtful'));
  assert.ok(m('src:learned') && m('kind:charter'));
  assert.ok(m('eta<30m') && !m('eta<10') && !m('eta<30', { ...ac, route: { ...ac.route, eta: 0 } }));
  assert.ok(m('noroute', bare) && !m('noroute'));
  assert.ok(m('-noroute'));
  assert.ok(m('to:JFK state:fits eta<25'));
});
