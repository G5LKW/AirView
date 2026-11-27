// Aircraft silhouettes (map/aircraft-icons.js): the type designator -> shape mapping is checked
// against every designator of the embedded ICAO Doc 8643 table (internal/refdata/types_data.go),
// plus the fallbacks (Doc 8643 class / wake category, ADS-B emitter category, no data).
import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { SHAPES, shapeFor, shapeForType } from '../../static/js/map/aircraft-icons.js';

const root = fileURLToPath(new URL('../../../', import.meta.url));
const table = readFileSync(root + 'internal/refdata/types_data.go', 'utf8')
  .split('\n').map((l) => l.trim().split('|')).filter((f) => f.length === 6 && /^[A-Z0-9]{2,4}$/.test(f[0]))
  .map(([des, , , desc, wtc, role]) => ({ des, desc, wtc, role }));

/** Shapes acceptable for one Doc 8643 entry. */
function expected({ des, desc, wtc, role }) {
  if (des === 'GLID') return ['glider'];
  if (des === 'BALL' || des === 'SHIP') return ['balloon'];
  if (des === 'ULAC') return ['light'];
  if (des === 'GYRO') return ['helicopter'];
  if (des === 'ZZZZ') return [''];
  if (/^[HT]/.test(desc)) return ['helicopter'];
  const m = /^[LSA]([1-8])([JTP])$/.exec(desc);
  assert.ok(m, `${des}: unexpected description ${desc}`);
  const n = Number(m[1]);
  if (m[2] === 'J') {
    if (wtc === 'H' || wtc === 'J') return ['heavy'];
    return role === 'M' ? ['fast', 'jet'] : ['jet'];
  }
  if (m[2] === 'T') return n >= 2 ? ['turboprop'] : ['light'];
  return n >= 3 ? ['turboprop'] : ['light', 'turboprop'];
}

test('the embedded type table is read', () => {
  assert.ok(table.length > 400, `only ${table.length} types parsed`);
});

test('every Doc 8643 designator maps to a matching silhouette', () => {
  const bad = [];
  for (const t of table) {
    const got = shapeForType(t.des);
    const want = expected(t);
    if (!want.includes(got)) bad.push(`${t.des} (${t.desc} ${t.wtc}): ${got || 'none'}, want ${want.join('|')}`);
  }
  assert.deepEqual(bad, []);
});

test('common types (release checklist)', () => {
  const want = {
    jet: ['A20N', 'A21N', 'A319', 'A320', 'A321', 'B38M', 'B737', 'B738', 'B739', 'B752', 'E190', 'E195', 'E75L', 'CRJ9', 'C56X', 'GLF6'],
    heavy: ['A332', 'A333', 'A359', 'A388', 'B744', 'B748', 'B763', 'B772', 'B77W', 'B788', 'B789', 'C17', 'K35R'],
    turboprop: ['AT76', 'DH8D', 'C130', 'A400', 'B350'],
    light: ['C172', 'PA28', 'P28A', 'SR22', 'DA62', 'PC12', 'C208'],
    helicopter: ['EC35', 'EC45', 'AS50', 'H145', 'R44', 'H60', 'NH90', 'V22'],
    glider: ['GLID', 'AS21', 'DG80', 'LS8', 'DUOD'],
    fast: ['F16', 'F35', 'EUFI', 'TOR', 'HAWK', 'RFAL', 'A10'],
    balloon: ['BALL'],
  };
  for (const [shape, types] of Object.entries(want)) for (const t of types) assert.equal(shapeForType(t), shape, t);
  // historic mis-mappings: the old prefix regexes made the A320 family heavy and C130 / C160 a Cessna
  assert.equal(shapeFor({ type: 'A320', category: 'A3' }), 'jet');
  assert.equal(shapeFor({ type: 'C130' }), 'turboprop');
  assert.equal(shapeFor({ type: 'F50' }), 'turboprop'); // Fokker 50, not an F-5
  assert.equal(shapeFor({ type: 'AS32' }), 'helicopter'); // Super Puma, while AS21-AS29 are gliders
  for (const shape of new Set(Object.keys(want))) assert.ok(SHAPES[shape], shape);
});

test('the type beats the emitter category; surface vehicles stay ground', () => {
  assert.equal(shapeFor({ type: 'EC35', category: 'A1' }), 'helicopter');
  assert.equal(shapeFor({ type: 'B77W', category: 'A3' }), 'heavy');
  assert.equal(shapeFor({ type: 'a320' }), 'jet'); // case-insensitive
  assert.equal(shapeFor({ type: 'B738', category: 'C1' }), 'ground');
});

test('fallbacks when the type is unknown', () => {
  // Doc 8643 class + wake category (typeInfo of /api/aircraft/{icao})
  assert.equal(shapeFor({ type: 'XXXX', typeInfo: { description: 'L2J', wtc: 'H' } }), 'heavy');
  assert.equal(shapeFor({ typeInfo: { description: 'L2T', wtc: 'M' } }), 'turboprop');
  assert.equal(shapeFor({ typeInfo: { description: 'L1P', wtc: 'L' } }), 'light');
  assert.equal(shapeFor({ typeInfo: { description: 'H2T', wtc: 'L' } }), 'helicopter');
  assert.equal(shapeFor({ desc8643: 'L4J', wtc: 'J' }), 'heavy');
  assert.equal(shapeFor({ wtc: 'M', category: 'A1' }), 'jet'); // wake category before emitter category
  // emitter category
  const cats = { A1: 'light', A2: 'jet', A3: 'jet', A4: 'jet', A5: 'heavy', A6: 'fast', A7: 'helicopter', B1: 'glider', B2: 'balloon', B4: 'light', B7: 'fast', C2: 'ground' };
  for (const [c, s] of Object.entries(cats)) assert.equal(shapeFor({ category: c, track: 90 }), s, c);
  // nothing known
  assert.equal(shapeFor({ track: 0 }), 'unknown');
  assert.equal(shapeFor({}), 'dot');
  assert.equal(shapeFor({ category: 'A0', type: 'ZZZZ' }), 'dot');
});
