// airports-page.test.mjs - helpers of the airport board page (pages/airports.js).
import test from 'node:test';
import assert from 'node:assert/strict';
import { etaText, apCode } from '../../static/js/pages/airports.js';

test('etaText rounds sigma to whole minutes and hides missing ETAs', () => {
  assert.equal(etaText(0, 120), '');
  assert.match(etaText(1_800_000_000, 170), /±3 min$/);
  assert.doesNotMatch(etaText(1_800_000_000, 0), /±/);
});

test('apCode prefers IATA', () => {
  assert.equal(apCode('EGLL', 'LHR'), 'LHR');
  assert.equal(apCode('EGLL', ''), 'EGLL');
});
