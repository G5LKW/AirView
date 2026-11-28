// weather-hazards.test.mjs - wording of BDS 4,5 hazard reports on the Weather page.
import test from 'node:test';
import assert from 'node:assert/strict';
import { hazardText } from '../../static/js/pages/weather.js';

test('hazardText names levels 1..3 and leaves out nil', () => {
  assert.equal(hazardText({ turbulence: 2, icing: 1, windShear: 0 }), 'moderate turbulence, light icing');
  assert.equal(hazardText({ microburst: 3, wakeVortex: 1 }), 'severe microburst, light wake vortex');
  assert.equal(hazardText({ turbulence: 0 }), '');
  assert.equal(hazardText({ turbulence: 7 }), '');
});
