#!/usr/bin/env node
// Public-view privacy crawl: fetches every request the public network UI makes (no session cookie) plus a few
// seconds of the SSE stream and fails when a payload carries receiver data (DESIGN §8.4): the forbidden keys of
// net/model.js, IP addresses, or - with --coords=lat,lon;lat,lon - any station coordinate to 3 decimals.
//
//   node web/dev/privacy-crawl.mjs http://127.0.0.1:28526 [--coords=52.123,0.456] [--sse-seconds=4] [--bbox=s,w,n,e]
//                                  [--share=<token>[,<token>]] [--sids=<sid>,<sid>]
//
// --share also crawls what anyone with a share link sees (/api/share/<token>, /aircraft, /stats); --sids fails
// on any of these station IDs in any payload (a share link must never name its stations).
//
// Exit status 0 = clean, 1 = findings (printed), 2 = the server could not be reached.

import { PUBLIC_FORBIDDEN_KEYS } from '../static/js/net/model.js';
import { EP, PUBLIC_UI_REQUESTS } from '../static/js/net/endpoints.js';

const args = process.argv.slice(2);
const base = (args.find((a) => !a.startsWith('--')) || 'http://127.0.0.1:8099').replace(/\/$/, '');
const opt = Object.fromEntries(args.filter((a) => a.startsWith('--')).map((a) => { const [k, v = ''] = a.slice(2).split('='); return [k, v]; }));
const coords = (opt.coords || '').split(';').filter(Boolean).map((s) => s.split(',').map(Number));
const sseSeconds = Number(opt['sse-seconds'] || 4);
// station status keys of model.Status that must not reach a public viewer
const STATUS_KEYS = new Set(['station', 'feeds', 'ntp', 'dataDir', 'maxRangeKm']);
const IPV4 = /\b(?:\d{1,3}\.){3}\d{1,3}\b/;
const sids = (opt.sids || '').split(',').filter(Boolean);
const shares = (opt.share || '').split(',').filter(Boolean);

const findings = [];
function scan(where, v, path = '') {
  if (v === null || v === undefined) return;
  if (Array.isArray(v)) { v.forEach((x, i) => scan(where, x, `${path}[${i}]`)); return; }
  if (typeof v === 'object') {
    // a station position: an object whose lat AND lon both match one station to 3 decimals (one axis alone
    // matches by chance: 250 aircraft against 15 stations give a few hits per snapshot)
    const la = v.lat ?? v.latitude;
    const lo = v.lon ?? v.longitude;
    if (typeof la === 'number' && typeof lo === 'number') {
      for (const [lat, lon] of coords) {
        if (Math.abs(la - lat) < 0.0005 && Math.abs(lo - lon) < 0.0005) findings.push(`${where}: station position at ${path}: ${la}, ${lo}`);
      }
    }
    for (const [k, x] of Object.entries(v)) {
      if (PUBLIC_FORBIDDEN_KEYS.has(k)) findings.push(`${where}: forbidden key ${path}.${k}`);
      if (where.includes('/api/status') && STATUS_KEYS.has(k) && x !== null && !(Array.isArray(x) && !x.length)) findings.push(`${where}: station status field ${path}.${k}`);
      scan(where, x, `${path}.${k}`);
    }
    return;
  }
  if (typeof v === 'string' && IPV4.test(v) && !/^\d+\.\d+\.\d+$/.test(v)) findings.push(`${where}: IP-like value at ${path}: ${v}`);
  if (typeof v === 'string' && sids.some((s) => v.includes(s))) findings.push(`${where}: station ID at ${path}: ${v}`);
}

async function getJSON(p) {
  const r = await fetch(base + p, { headers: { Accept: 'application/json' } });
  const text = await r.text();
  let data = null;
  try { data = text ? JSON.parse(text) : null; } catch { /* not JSON */ }
  return { status: r.status, data };
}

async function sse(path = '/api/events') {
  const ctl = new AbortController();
  const timer = setTimeout(() => ctl.abort(), sseSeconds * 1000);
  try {
    const r = await fetch(base + path, { signal: ctl.signal, headers: { Accept: 'text/event-stream' } });
    const reader = r.body.getReader();
    const dec = new TextDecoder();
    let buf = '';
    for (;;) {
      const { value, done } = await reader.read();
      if (done) break;
      buf += dec.decode(value, { stream: true });
      let i;
      while ((i = buf.indexOf('\n\n')) >= 0) {
        const block = buf.slice(0, i);
        buf = buf.slice(i + 2);
        const ev = /^event: (.*)$/m.exec(block)?.[1] || 'message';
        const data = block.split('\n').filter((l) => l.startsWith('data: ')).map((l) => l.slice(6)).join('\n');
        try { scan(`SSE ${ev === 'status' ? '/api/status' : ev}`, JSON.parse(data)); } catch { /* keep-alive */ }
      }
    }
  } catch (e) {
    if (e.name !== 'AbortError') throw e;
  } finally { clearTimeout(timer); }
}

try {
  for (const p of PUBLIC_UI_REQUESTS) {
    const { status, data } = await getJSON(p);
    console.log(`${String(status).padEnd(4)} ${p}`);
    // the station list's own `stations` / Feed Check's `links` wrappers are the DTO containers, not receiver lists
    if (data) scan(p, p.endsWith('/public/stations') ? data.stations : data);
  }
  for (const tok of shares) {
    for (const p of [EP.share(tok), EP.shareAircraft(tok), EP.shareStats(tok)]) {
      await new Promise((r) => setTimeout(r, 1100)); // the public rate limit (1/s, burst 10)
      const { status, data } = await getJSON(p);
      console.log(`${String(status).padEnd(4)} /api/share/…${p.slice(p.indexOf(tok) + tok.length)}`);
      if (data) scan('share' + p.slice(p.indexOf(tok) + tok.length), data);
    }
  }
  await sse();
  // a viewport stream (map in aggregator mode): same rules
  if (opt.bbox) await sse('/api/events?delta=1&bbox=' + encodeURIComponent(opt.bbox));
} catch (e) {
  console.error(`cannot reach ${base}: ${e.message}`);
  process.exit(2);
}
if (findings.length) {
  console.log(`\n${findings.length} finding(s):`);
  for (const f of [...new Set(findings)].slice(0, 200)) console.log('  ' + f);
  process.exit(1);
}
console.log('\nclean: no receiver data in the public payloads');
