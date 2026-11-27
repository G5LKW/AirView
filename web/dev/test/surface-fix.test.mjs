// fix:surface regressions in net/endpoints.js (UX-01, PRIV-A8). Run: node --test web/dev/test
import test from 'node:test';
import assert from 'node:assert/strict';
import { uptimeSegments, normStationDetail, normCheck } from '../../static/js/net/endpoints.js';

test('uptime window starts at the station creation (UX-01)', () => {
  const now = 1_800_000_000_000;
  const created = now - 6 * 60e3; // 6 min old
  const segs = uptimeSegments([{ on: created + 1000 }], now, 7, created);
  const total = segs.reduce((a, s) => a + (s.to - s.from), 0);
  const up = segs.filter((s) => s.online).reduce((a, s) => a + (s.to - s.from), 0);
  assert.equal(segs[0].from, created);
  assert.ok(up / total > 0.99, `uptime ${up / total}`);
  // without notBefore: the 7-day window as before
  assert.equal(uptimeSegments([], now, 7)[0].from, now - 7 * 86400e3);
});

test('station detail maps the server connection shape (UX-01)', () => {
  const d = normStationDetail({ sid: 's1', connection: { proto: 'avf', software: 'airview-feeder 1.2', kbps: 3.4, rttMs: 21, links: [{ proto: 'avf', msgRate: 340.5, software: 'airview-feeder 1.2' }] } });
  assert.equal(d.connection.version, 'airview-feeder 1.2');
  assert.equal(d.connection.kBps, 3.4);
  assert.equal(d.connection.rttP50Ms, 21);
  assert.equal(d.connection.links[0].msgRate, 340.5);
});

test('feed check: details flag and claim method (PRIV-A8)', () => {
  const [a, b] = normCheck({ links: [{ proto: 'avf', details: false, claimable: true, claimMethod: 'code' }, { proto: 'beast', claimMethod: 'admin' }] });
  assert.equal(a.details, false);
  assert.equal(a.claimKind, 'code');
  assert.equal(b.details, true);
  assert.equal(b.claimKind, 'admin');
});
