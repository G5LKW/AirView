// Daily briefing: the unit tokens of the server's text are formatted in the viewer's units.
import test from 'node:test';
import assert from 'node:assert/strict';
import { formatTokens } from '../../static/js/reports/briefing.js';

const units = {
  int: (v) => v.toLocaleString('en-GB'),
  dist: (km, o) => `${(km / 1.852).toFixed(o?.digits ?? 1)} nm`,
  pct: (v) => `${Math.round(v)}%`,
  alt: (ft) => `${ft} ft`,
  speed: (kt) => `${kt} kt`,
};

test('formatTokens replaces every token kind', () => {
  assert.equal(formatTokens('{int:1234} aircraft, {dist:185.2} out, {pct:12} more at {hour:7}', units), '1,234 aircraft, 100 nm out, 12% more at 07:00');
});

test('formatTokens formats altitude and speed records', () => {
  assert.equal(formatTokens('{alt:45000} / {speed:612}', units), '45000 ft / 612 kt');
});

test('formatTokens leaves other text and odd input alone', () => {
  assert.equal(formatTokens('no {x:1} tokens', units), 'no {x:1} tokens');
  assert.equal(formatTokens(null, units), '');
  assert.equal(formatTokens('{int:-5}', units), '-5');
});
