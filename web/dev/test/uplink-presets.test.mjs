// uplink-presets.test.mjs - the "Feed community networks" presets (settings/sections-uplinks.js): the
// addresses and Beast formats checked against each network's own feed client on 27 Nov 2026.
import test from 'node:test';
import assert from 'node:assert/strict';
import { NETWORK_PRESETS, presetBeastUplink, newMLATUplink } from '../../static/js/settings/sections-uplinks.js';

const WANT = {
  'adsb.lol': ['feed.adsb.lol', 'beast_reduce_plus_out', 'feed.adsb.lol'],
  'adsb.fi': ['feed.adsb.fi', 'beast_reduce_plus_out', 'feed.adsb.fi'],
  'airplanes.live': ['feed.airplanes.live', 'beast_reduce_plus_out', 'feed.airplanes.live'],
  'ADS-B Exchange': ['feed1.adsbexchange.com', 'beast_reduce_out', 'feed.adsbexchange.com'],
  OARC: ['adsb.oarc.uk', 'beast_reduce_plus_out', 'adsb.oarc.uk'],
  'The Air Traffic': ['feed.theairtraffic.com', 'beast_reduce_out', 'feed.theairtraffic.com'],
};

test('presets match the networks\' published feeder settings', () => {
  assert.deepEqual(NETWORK_PRESETS.map((n) => n.name).sort(), Object.keys(WANT).sort());
  for (const n of NETWORK_PRESETS) {
    const [bh, fmt, mh] = WANT[n.name];
    assert.deepEqual(n.beast, [bh, 30004], n.name);
    assert.equal(n.format, fmt, n.name);
    assert.deepEqual(n.mlat, [mh, 31090], n.name);
  }
  assert.match(NETWORK_PRESETS.find((x) => x.name === 'OARC').note, /members/);
});

test('a preset adds a Beast output with the network\'s format and an MLAT output sharing the UUID', () => {
  const list = [];
  const n = NETWORK_PRESETS.find((x) => x.name === 'ADS-B Exchange');
  const b = presetBeastUplink(list, n, 'u-1');
  list.push(b);
  const m = newMLATUplink(list, n.mlat[0], n.mlat[1], 'u-1');
  assert.equal(b.type, 'beast');
  assert.equal(b.host, 'feed1.adsbexchange.com');
  assert.equal(b.port, 30004);
  assert.equal(b.format, 'beast_reduce_out');
  assert.equal(m.type, 'mlat');
  assert.equal(m.uuid, b.uuid);
  assert.notEqual(m.id, b.id);
});
