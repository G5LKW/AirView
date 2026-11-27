// pages/timing.js - NTP reference and receiver timing analysis.
//
// Endpoints: /api/timing/ntp (10 s), /api/timing/history (60 s), /api/timing/latency (5 s),
// /api/timing/receiver (15 s), POST /api/timing/ntp/sample (admin, "Measure now"), /api/login on 401.
// The "Time difference of arrival" section (latency chain, waterfall, NTP/GPS check) lives in timing/tdoa.js.
// Offsets are "true UTC minus host clock" in ms: positive = the station's clock is behind (slow).

import { createAdmin } from '../admin.js';
import { createTdoa } from '../timing/tdoa.js';
import { feedRef, chainRef, REF_TEXT, frameEndCorrUs, clockFindings } from '../timing/ref.js';

const WARN_MS = 100; // system clock error that earns a warning
const BAD_MS = 1000; // ... and an alert
const RANGES = [{ value: '1', label: '1 h' }, { value: '6', label: '6 h' }, { value: '24', label: '24 h' }, { value: '144', label: '6 d' }];
const LEAP = ['none announced', 'the last minute of today has 61 s', 'the last minute of today has 59 s', 'alarm: the server itself is not synchronised'];

const ok = (v) => typeof v === 'number' && Number.isFinite(v);

function stratumText(s) {
  if (!ok(s) || s <= 0) return 'unknown';
  if (s === 1) return 'primary server (GPS / atomic clock attached)';
  if (s >= 16) return 'unsynchronised';
  return `${s - 1} hop${s === 2 ? '' : 's'} from a reference clock`;
}

/** GPS lock state out of the receiver's status text (see feed.BeastStatus.String on the server). */
function gpsState(feed) {
  const info = String(feed.info || '').toLowerCase();
  if (!feed.gpsTime) return { label: feed.connected ? 'no GPS timestamps' : '—', kind: 'other', tip: 'The feed delivers no absolute timestamps (12 MHz counter or none). Latency cannot be measured.' };
  if (feed.type === 'demo') return { label: 'simulated', kind: 'blue', tip: 'The built-in simulator stamps its frames with the host clock.' };
  if (info.includes('gps locked')) return { label: 'GPS locked', kind: 'green', tip: 'Antenna fine, at least three satellites, timestamps valid.' };
  if (info.includes('antenna fault')) return { label: 'antenna fault', kind: 'red', tip: 'The receiver reports a GPS antenna problem: check the cable and the antenna.' };
  if (info.includes('not valid')) return { label: 'time not valid', kind: 'red', tip: 'The receiver flags its timestamps as not usable.' };
  if (info.includes('acquiring')) return { label: 'acquiring', kind: 'amber', tip: 'Fewer than three satellites tracked: the GPS antenna needs a better view of the sky.' };
  if (info.includes('no gps satellites')) return { label: 'no satellites', kind: 'red', tip: 'No satellite tracked: check the GPS antenna.' };
  if (info.includes('not reported')) return { label: 'not reported', kind: 'other', tip: 'Old firmware: the status frame carries no GPS detail.' };
  return { label: 'GPS timestamps', kind: 'cyan', tip: 'GPS timestamp mode detected; no status frame seen yet.' };
}

/** Radarcape status frame bytes [settings, ppsOffset, gps] -> fields (mirrors feed.ParseBeastStatus). */
function decodeStatus(st) {
  let bytes = null;
  if (Array.isArray(st) && st.length >= 3 && st.every((b) => Number.isInteger(b))) bytes = st;
  else if (st && typeof st === 'object' && Array.isArray(st.data)) bytes = st.data;
  else if (st && typeof st === 'object' && Number.isInteger(st.Settings) && Number.isInteger(st.GPS)) bytes = [st.Settings, (st.PPSOffset || 0) & 0xff, st.GPS]; // feed.BeastStatus as JSON
  else if (typeof st === 'string' && /^[0-9a-f]{2}( ?[0-9a-f]{2}){2,}$/i.test(st.trim())) bytes = st.trim().replace(/ /g, '').match(/../g).map((x) => parseInt(x, 16)); // hex payload
  if (!bytes) return null;
  const s = bytes[0] & 0xff;
  const g = bytes[2] & 0xff;
  const pps = bytes[1] > 127 ? bytes[1] - 256 : bytes[1];
  return {
    gpsTimestamps: !!(s & 0x10), detail: g !== 0, antennaOk: !!(g & 0x01), tracking: !!(g & 0x02), sats3: !!(g & 0x04), utc: !!(g & 0x08),
    bad: !!(g & 0x20), ppsNs: pps * 15.625, hex: bytes.slice(0, 3).map((b) => (b & 0xff).toString(16).padStart(2, '0')).join(' '),
  };
}

export default {
  id: 'timing',
  title: 'Timing',

  async mount(container, ctx) {
    const { api, live, units, ui, charts } = ctx;
    const { h } = ui;
    await ui.loadCss(new URL('../../css/timing.css', import.meta.url));

    const admin = createAdmin(ctx);
    const DASH = units.DASH;
    const prefs = ui.prefs('airview.timing', { hours: '24' });
    let alive = true;
    ctx.onCleanup(() => { alive = false; });

    // ---- formatting: milliseconds have no unit system; numbers still go through units.num ----
    const fmtMs = (v, o = {}) => {
      if (!ok(v)) return DASH;
      const a = Math.abs(v);
      const sign = o.sign && v > 0 ? '+' : '';
      if (a >= 10000) return sign + units.num(v / 1000, 2) + ' s';
      if (a >= 100) return sign + units.num(v, 0) + ' ms';
      if (a >= 10) return sign + units.num(v, 1) + ' ms';
      if (a >= 0.1 || a === 0) return sign + units.num(v, 2) + ' ms';
      return sign + units.num(v * 1000, 1) + ' µs';
    };
    const msParts = (v, o) => { const s = fmtMs(v, o); const i = s.lastIndexOf(' '); return i < 0 ? [s, ''] : [s.slice(0, i), s.slice(i + 1)]; };
    const clockTenths = (t) => (ok(t) ? units.clock(t, { z: false }) + '.' + Math.floor((((t % 1000) + 1000) % 1000) / 100) : DASH);
    const axisMs = (v) => units.num(v, Math.abs(v) < 10 ? 1 : 0);

    // ---- state ----
    let ntp = null;        // /api/timing/ntp
    let hist = null;       // /api/timing/history
    let lat = null;        // /api/timing/latency
    let rcv = null;        // /api/timing/receiver
    let hours = RANGES.some((r) => r.value === prefs.get().hours) ? prefs.get().hours : '24';

    // ------------------------------------------------------------------------------------------
    // banners
    // ------------------------------------------------------------------------------------------
    const banners = h('div.tm-banners');
    const banner = (kind, icon, title, text, action) => h('div.tm-banner.tm-banner--' + kind, { role: kind === 'bad' ? 'alert' : 'status' }, ui.icon(icon),
      h('div.grow', h('div.tm-banner-title', title), h('div.tm-banner-text', text)), action || null);
    const settingsBtn = () => ui.button({ label: 'Open settings', icon: 'settings', size: 'sm', href: '#/settings' });
    const timeSettingsBtn = () => ui.button({ label: 'Settings → Time (NTP)', icon: 'settings', size: 'sm', href: '#/settings/time' });
    let bannerSig = '';

    function paintBanners() {
      const list = [];
      const sig = [];
      const add = (key, ...args) => { sig.push(key); list.push(() => banner(...args)); };
      // the latency analysis' own verdict on the host clock (no NTP, NTP unsynchronised, frames "arriving before
      // they were sent"): shown first, as it changes how every latency number on this page reads
      const clk = clockFindings(lat && lat.chain);
      const ntpOff = ntp && (!(ntp.config || {}).enabled || !(ntp.config || {}).server);
      if (clk.length) {
        const worst = clk[0].level === 'bad' ? 'bad' : 'warn';
        add('clk:' + clk.map((f) => f.key + f.level + f.text).join('/'), worst, 'alert',
          ntpOff ? 'Host clock not checked against NTP: latency includes the clock error' : 'The host clock limits the latency figures',
          h('span', clk.map((f) => f.text).join(' '), ' ',
            ntpOff ? 'Enter a time server - ideally a GPS-locked one on your network, such as the Radarcape\'s own NTP service - under ' : 'Check the time server under ',
            h('a', { href: '#/settings/time' }, 'Settings → Time (NTP)'), '.'),
          timeSettingsBtn());
      }
      if (ntp) {
        const cfg = ntp.config || {};
        const st = ntp.status || {};
        if ((!cfg.enabled || !cfg.server) && !clk.length) {
          add('nontp', 'warn', 'alert', 'NTP is not configured',
            'Without an NTP reference AirView cannot tell how far this computer\'s clock is from true UTC, and the feed latency below includes the full clock error. Enter a time server (for example pool.ntp.org, or your router) under Settings → Time.', timeSettingsBtn());
        } else if (!st.synced) {
          if (st.lastError) add('err:' + st.lastError, 'bad', 'x-circle', 'The NTP server does not answer', `${cfg.server}: ${st.lastError}. Check the server name, the network and that outgoing UDP port 123 is allowed.`, timeSettingsBtn());
          else add('wait', 'info', 'timer', 'Waiting for the first NTP measurement', `Asking ${cfg.server} every ${units.int(cfg.intervalSec)} s; the first answer usually arrives within a few seconds.`);
        } else {
          const off = st.offsetMs;
          const a = Math.abs(off);
          if (a >= WARN_MS) {
            add('off:' + (a >= BAD_MS ? 'bad' : 'warn') + ':' + fmtMs(off), a >= BAD_MS ? 'bad' : 'warn', 'alert', `The system clock is ${fmtMs(a)} ${off > 0 ? 'slow' : 'fast'}`,
              `True UTC is ${fmtMs(a)} ${off > 0 ? 'ahead of' : 'behind'} this computer's clock (warning above ${fmtMs(WARN_MS)}). AirView corrects its own latency analysis with this offset but does not set the clock: time stamps in the database, in logs and sent to other programs are off by this amount. Turn on the operating system's time synchronisation (Windows Time service, chrony or systemd-timesyncd).`);
          }
          const age = live.serverNow() - st.lastOk;
          if (st.lastOk > 0 && age > Math.max(5 * (cfg.intervalSec || 64) * 1000, 120000)) {
            add('stale', 'warn', 'timer', 'The NTP reference is stale', `Last successful measurement ${units.ago(st.lastOk, live.serverNow())}${st.lastError ? ' - ' + st.lastError : ''}. The offset shown may no longer be right.`);
          }
        }
      }
      if (rcv) {
        for (const f of rcv.feeds || []) {
          const g = gpsState(f);
          if (f.connected && f.gpsTime && g.kind === 'red') add('gps:' + f.id + g.label, 'bad', 'alert', `Receiver GPS problem on "${f.name}"`, `${g.tip} Status: ${f.info || 'n/a'}. GPS timestamps (and MLAT) are not reliable until this is fixed.`);
          else if (f.connected && f.gpsTime && g.kind === 'amber') add('gps:' + f.id + g.label, 'warn', 'alert', `Receiver GPS is still acquiring on "${f.name}"`, `${g.tip} Status: ${f.info || 'n/a'}.`);
        }
      }
      const s = sig.join('|');
      if (s === bannerSig) return;
      bannerSig = s;
      ui.mount(banners, list.map((fn) => fn()));
    }

    // ------------------------------------------------------------------------------------------
    // clock comparison tiles (ticked locally, 10 Hz, text only)
    // ------------------------------------------------------------------------------------------
    const cSys = ui.stat({ label: 'System clock (UTC)', icon: 'timer', tip: 'The clock of the computer AirView runs on, as the server reports it' });
    const cNtp = ui.stat({ label: 'NTP-corrected UTC', icon: 'globe', accent: 'cyan', tip: 'System clock plus the measured NTP offset: the best estimate of true UTC' });
    const cGps = ui.stat({ label: 'Receiver GPS time', icon: 'radar', accent: 'green', tip: 'Time stamp the receiver puts on frames arriving right now (NTP-corrected arrival time minus the median feed latency)' });
    const cBrowser = ui.stat({ label: 'This browser (UTC)', icon: 'eye', tip: 'The clock of the device you are looking at, for comparison' });
    for (const t of [cSys, cNtp, cGps, cBrowser]) t.classList.add('tm-clock');

    function correction() {
      if (lat && lat.clockSynced) return lat.clockOffsetMs;
      const st = (ntp && ntp.status) || (live.status && live.status.ntp);
      return st && st.synced ? st.offsetMs : null;
    }
    function gpsFeed() { return lat && (lat.feeds || []).find((f) => f.samples > 0) || null; }
    function tickClocks() {
      const sys = live.serverNow();
      const corr = correction();
      const gf = gpsFeed();
      cSys.set(clockTenths(sys), 'Z');
      cNtp.set(corr === null ? DASH : clockTenths(sys + corr), corr === null ? '' : 'Z');
      cGps.set(gf ? clockTenths(sys + (corr || 0) - gf.p50Ms) : DASH, gf ? 'Z' : '');
      cBrowser.set(clockTenths(Date.now()), 'Z');
    }
    function paintClockSubs() {
      const corr = correction();
      const gf = gpsFeed();
      cSys.setSub(units.date(live.serverNow()) + ' · host of the station');
      cNtp.setSub(corr === null ? 'no NTP reference - cannot be computed' : `system clock ${fmtMs(corr, { sign: true })}`);
      cGps.setSub(gf ? `feed ${gf.feedId} · median latency ${fmtMs(gf.p50Ms)}` : 'no feed delivers GPS time stamps');
      const b = live.serverOffset;
      cBrowser.setSub(ok(b) ? (Math.abs(b) < 20 ? 'in step with the station (±20 ms)' : `${fmtMs(Math.abs(b))} ${b > 0 ? 'behind' : 'ahead of'} the station (incl. network delay)`) : 'comparing…');
    }

    // ------------------------------------------------------------------------------------------
    // NTP status
    // ------------------------------------------------------------------------------------------
    const tOffset = ui.stat({ label: 'Clock offset', icon: 'timer', tip: 'True UTC minus the system clock (smoothed). Positive: the system clock is slow. Good: within ±50 ms.' });
    const tDelay = ui.stat({ label: 'Round-trip delay', icon: 'route', tip: 'Time for a query to reach the NTP server and come back. Good: below 50 ms and steady.' });
    const tJitter = ui.stat({ label: 'Jitter', icon: 'signal', tip: 'RMS scatter of the recent offsets. Good: below 5 ms on a wired network.' });
    const tStratum = ui.stat({ label: 'Stratum', icon: 'layers', tip: 'Distance of the server from a reference clock: 1 = GPS / atomic clock attached, 2 = synchronised to a stratum 1 server, ...' });
    const tLast = ui.stat({ label: 'Last sync', icon: 'check-circle', tip: 'Time of the last successful measurement' });
    const tDrift = ui.stat({ label: 'Clock drift (24 h)', icon: 'chart', tip: 'How fast the system clock gains or loses against the server. 1 ppm = 86 ms per day. Positive: the clock runs slow.' });
    const ntpKvHost = h('div');
    const ntpStatsHost = h('div');
    const measureBtn = ui.button({ label: 'Measure now', icon: 'refresh', size: 'sm', tip: 'Take an NTP measurement immediately (admin)', onClick: measureNow });

    function accentFor(el, level) {
      for (const c of ['stat--green', 'stat--amber', 'stat--red']) el.classList.remove(c);
      if (level) el.classList.add('stat--' + level);
    }

    function paintNtp() {
      if (!ntp) return;
      const cfg = ntp.config || {};
      const st = ntp.status || {};
      const s = ntp.latest;
      const on = !!(cfg.enabled && cfg.server);
      measureBtn.disabled = !on;
      if (!on || !st.synced) {
        const why = !on ? 'NTP not configured' : 'no answer yet';
        tOffset.set(DASH, ''); tOffset.setSub(why); accentFor(tOffset, null);
        tDelay.set(DASH, ''); tDelay.setSub('');
        tJitter.set(DASH, ''); tJitter.setSub('');
        tStratum.set(DASH, ''); tStratum.setSub('');
        tLast.set(st.lastOk > 0 ? units.ago(st.lastOk, live.serverNow()) : DASH, ''); tLast.setSub(st.lastOk > 0 ? units.dateTime(st.lastOk) : why);
        tDrift.set(DASH, ''); tDrift.setSub('');
      } else {
        const a = Math.abs(st.offsetMs);
        tOffset.set(...msParts(st.offsetMs, { sign: true }));
        tOffset.setSub(a >= BAD_MS ? 'system clock badly off' : a >= WARN_MS ? `system clock ${st.offsetMs > 0 ? 'slow' : 'fast'}` : a < 50 ? 'good: within ±50 ms' : 'acceptable');
        accentFor(tOffset, a >= BAD_MS ? 'red' : a >= WARN_MS ? 'amber' : 'green');
        tDelay.set(...msParts(st.delayMs));
        tDelay.setSub(st.delayMs < 50 ? 'good: short path' : st.delayMs < 150 ? 'acceptable' : 'long path - try a closer server');
        tJitter.set(...msParts(st.jitterMs));
        tJitter.setSub(st.jitterMs < 5 ? 'good: steady' : st.jitterMs < 20 ? 'acceptable' : 'noisy link');
        accentFor(tJitter, st.jitterMs >= 20 ? 'amber' : null);
        tStratum.set(units.int(st.stratum), '');
        tStratum.setSub(stratumText(st.stratum));
        tLast.set(units.ago(st.lastOk, live.serverNow()), '');
        tLast.setSub(units.dateTime(st.lastOk) + ` · every ${units.int(cfg.intervalSec)} s`);
        const d = ntp.day && ntp.day.driftPpm;
        tDrift.set(ok(d) ? (d > 0 ? '+' : '') + units.num(d, 2) : DASH, ok(d) ? 'ppm' : '');
        tDrift.setSub(ok(d) ? `${fmtMs(Math.abs(d) * 86.4)} per day ${d > 0 ? 'slow' : 'fast'}` : 'needs 4 samples over 2 min');
      }

      if (!on) {
        ui.mount(ntpKvHost, ui.empty({ icon: 'timer', compact: true, title: 'NTP is not configured', text: 'Enter a time server under Settings → Time to compare this computer\'s clock with true UTC. AirView only measures; it never changes the system clock.', action: timeSettingsBtn() }));
      } else {
        ui.mount(ntpKvHost, ui.kv([
          ['Server', cfg.server, { text: true }],
          ['Answered by', (s && s.addr) || DASH],
          ['State', st.synced ? ui.badge('synchronised', 'green') : ui.badge(st.lastError ? 'failing' : 'waiting', st.lastError ? 'red' : 'amber'), { text: true }],
          st.lastError ? ['Last error', st.lastError, { text: true, class: 'tm-wrap' }] : null,
          ['Stratum', st.synced ? `${units.int(st.stratum)} - ${stratumText(st.stratum)}` : DASH, { text: true }],
          ['Reference ID', st.refId || (s && s.refId) || DASH, { tip: 'What the server itself is synchronised to: a four-letter clock name (GPS, PPS, ...) for stratum 1, otherwise the address of its own time source' }],
          ['Offset (smoothed)', st.synced ? fmtMs(st.offsetMs, { sign: true }) : DASH],
          ['Offset (last sample)', s ? fmtMs(s.offsetMs, { sign: true }) : DASH],
          ['Round-trip delay', st.synced ? fmtMs(st.delayMs) : DASH],
          ['Jitter (RMS)', st.synced ? fmtMs(st.jitterMs) : DASH],
          ['Server root delay / dispersion', s ? `${fmtMs(s.rootDelayMs)} / ${fmtMs(s.rootDispMs)}` : DASH, { tip: 'The server\'s own distance and accumulated error bound to its reference clock' }],
          ['Leap second', s ? LEAP[s.leap] || String(s.leap) : DASH, { text: true }],
          ['Last burst', s ? `${units.int(s.replies)} of ${units.int(s.sent)} queries answered` : DASH, { text: true }],
          ['Last sync', st.lastOk > 0 ? `${units.dateTime(st.lastOk)} (${units.ago(st.lastOk, live.serverNow())})` : DASH],
          ['Polling interval', `${units.int(cfg.intervalSec)} s`],
        ]));
      }

      const windows = [['Last hour', ntp.hour], ['Last 24 h', ntp.day], ['All in memory', ntp.all]];
      if (!windows.some(([, w]) => w && w.count > 0)) {
        ui.mount(ntpStatsHost, ui.empty({ icon: 'chart', compact: true, title: 'No NTP statistics yet', text: on ? 'Statistics appear after the first measurements; drift needs at least 4 samples spanning 2 minutes.' : 'Needs a configured NTP server.' }));
      } else {
        ui.mount(ntpStatsHost, ui.table({
          columns: [
            { key: 'name', label: 'Window' },
            { key: 'count', label: 'Samples', align: 'right', num: true, render: (r) => units.int(r.w.count) },
            { key: 'span', label: 'Span', align: 'right', num: true, render: (r) => (r.w.count ? units.age(r.w.spanSec) : DASH) },
            { key: 'mean', label: 'Offset mean', align: 'right', num: true, render: (r) => (r.w.count ? fmtMs(r.w.offsetMeanMs, { sign: true }) : DASH) },
            { key: 'range', label: 'Offset min … max', align: 'right', num: true, render: (r) => (r.w.count ? `${fmtMs(r.w.offsetMinMs, { sign: true })} … ${fmtMs(r.w.offsetMaxMs, { sign: true })}` : DASH) },
            { key: 'std', label: 'Std dev', align: 'right', num: true, render: (r) => (r.w.count ? fmtMs(r.w.offsetStdMs) : DASH), tip: 'Standard deviation of the offset: how much the clock wanders' },
            { key: 'delay', label: 'Delay mean', align: 'right', num: true, render: (r) => (r.w.count ? fmtMs(r.w.delayMeanMs) : DASH) },
            { key: 'drift', label: 'Drift', align: 'right', num: true, render: (r) => (ok(r.w.driftPpm) ? (r.w.driftPpm > 0 ? '+' : '') + units.num(r.w.driftPpm, 2) + ' ppm' : DASH), tip: 'Slope of the offset: positive = the system clock runs slow' },
          ],
          rows: windows.filter(([, w]) => w).map(([name, w]) => ({ name, w })),
        }), h('div.tm-cap', `Since AirView started: ${units.int(ntp.all.polls)} polls, ${units.int(ntp.all.failures)} without an answer, ${units.int(ntp.all.steps)} clock step${ntp.all.steps === 1 ? '' : 's'} detected (the operating system set the clock).`));
      }
    }

    async function measureNow() {
      measureBtn.setLoading(true);
      try {
        const s = await admin.run(() => api.post('/api/timing/ntp/sample', undefined, { timeout: 30000 }), 'Taking an NTP measurement on demand needs the admin password.');
        if (!alive) return;
        ui.toast.success(`Offset ${fmtMs(s.offsetMs, { sign: true })}, delay ${fmtMs(s.delayMs)}, stratum ${units.int(s.stratum)}`, { title: 'NTP measurement' });
        loadNtp();
        loadHistory();
      } catch (e) {
        if (!(e && (e.cancelled || e.aborted))) ui.toast.error(e, { title: 'NTP measurement failed' });
      } finally {
        measureBtn.setLoading(false);
        if (ntp) measureBtn.disabled = !(ntp.config && ntp.config.enabled && ntp.config.server);
      }
    }

    // ------------------------------------------------------------------------------------------
    // NTP history charts
    // ------------------------------------------------------------------------------------------
    function chartBox(title, caption, cls) {
      const el = h('div.chart' + (cls || ''));
      const note = h('div.tm-chart-note.hidden');
      const chart = charts.create(el);
      return {
        el: h('div.tm-chartbox', h('div.label', title), h('div.tm-chart', el, note), h('div.tm-cap', caption)),
        chart,
        note(node) { note.classList.toggle('hidden', !node); el.classList.toggle('tm-faded', !!node); if (node) ui.mount(note, node); },
        dispose() { chart.dispose(); },
      };
    }
    const tdoa = createTdoa(ctx, { fmtMs, chartBox });
    const cOffset = chartBox('Clock offset', 'True UTC minus system clock. Good: a flat line within a few ms of zero. A steady slope is clock drift; a sudden jump is a clock step or a change of network path.');
    const cDelay = chartBox('Round-trip delay', 'Travel time to the NTP server and back. Good: low and steady. Spikes mean a congested link and make the offset of that moment less trustworthy.');
    const cJitter = chartBox('Jitter', 'Scatter (RMS) of the recent offsets. Good: below about 5 ms; a rising curve points at Wi-Fi or a busy network.');
    const rangeSeg = ui.segmented({ options: RANGES, value: hours, onChange: (v) => { hours = v; prefs.set({ hours }); loadHistory({ show: true }); } });
    const histInfo = h('span.muted.tm-info');

    function paintHistory() {
      if (!hist) return;
      const samples = hist.samples || [];
      const on = ntp && ntp.config && ntp.config.enabled && ntp.config.server;
      const label = RANGES.find((r) => r.value === hours).label;
      if (samples.length < 2) {
        const node = h('div', h('div.tm-note-title', 'Not enough data yet'),
          h('div', on ? `A curve needs at least two NTP measurements in the last ${label} (one is taken every ${units.int(ntp.config.intervalSec)} s); ${units.int(samples.length)} so far.` : 'Needs a configured NTP server: enter one under Settings.'));
        for (const c of [cOffset, cDelay, cJitter]) { c.note(node.cloneNode(true)); c.chart.set({ series: [] }, { replace: true }); }
        ui.setText(histInfo, '');
        return;
      }
      const pts = (key) => samples.filter((s) => ok(s[key])).map((s) => [s.t, s[key]]);
      const hasSmooth = hist.source === 'memory';
      const v = (x) => (ok(x) ? fmtMs(x, { sign: true }) : DASH);
      const vAbs = (x) => (ok(x) ? fmtMs(x) : DASH);
      cOffset.note(null);
      cOffset.chart.set(charts.line({
        series: [
          { name: 'Offset (each sample)', data: pts('offsetMs'), color: charts.colors.blue, width: 1.1 },
          ...(hasSmooth ? [{ name: 'Smoothed', data: pts('smoothedMs'), color: charts.colors.cyan, width: 2 }] : []),
        ],
        yName: 'ms', yScale: true, yFormat: axisMs, valueFormat: v, zoom: samples.length > 200,
      }), { replace: true });
      cDelay.note(null);
      cDelay.chart.set(charts.area({ series: [{ name: 'Round-trip delay', data: pts('delayMs'), color: charts.colors.amber }], yName: 'ms', yMin: 0, yFormat: axisMs, valueFormat: vAbs, legend: false }), { replace: true });
      if (hasSmooth) {
        cJitter.note(null);
        cJitter.chart.set(charts.area({ series: [{ name: 'Jitter (RMS)', data: pts('jitterMs'), color: charts.colors.violet || '#a78bfa' }], yName: 'ms', yMin: 0, yFormat: axisMs, valueFormat: vAbs, legend: false }), { replace: true });
      } else {
        cJitter.chart.set({ series: [] }, { replace: true });
        cJitter.note(h('div', h('div.tm-note-title', 'Not stored for this range'), h('div', 'Jitter is kept in memory only (about 6 days, lost on restart); the database keeps offset, delay and stratum.')));
      }
      ui.setText(histInfo, `${units.int(samples.length)}${hist.total > samples.length ? ' of ' + units.int(hist.total) : ''} samples · ${hist.source === 'memory' ? 'from memory' : 'from the database'}`);
    }

    // ------------------------------------------------------------------------------------------
    // receiver timing
    // ------------------------------------------------------------------------------------------
    const rcvFeedsHost = h('div');
    const rcvGpsHost = h('div');
    const rcvFramesHost = h('div');
    const rcvCard = ui.card({ title: 'Receiver timing', icon: 'radar', sub: 'GPS time-stamp mode and GPS lock per feed' });

    function paintReceiver() {
      if (!rcv) return;
      rcvCard.setSub(`${rcv.model || 'Receiver'} · GPS time-stamp mode and GPS lock per feed`);
      const errs = new Map(((live.status && live.status.feeds) || []).map((f) => [f.id, f.lastError]));
      const feeds = rcv.feeds || [];
      if (!feeds.length) {
        ui.mount(rcvFeedsHost, ui.empty({ icon: 'radar', compact: true, title: 'No feeds configured', text: 'Add the receiver (for example a Radarcape on port 10003) under Settings.', action: settingsBtn() }));
      } else {
        ui.mount(rcvFeedsHost, ui.table({
          columns: [
            { key: 'name', label: 'Feed', render: (f) => h('span', f.name, ' ', h('span.muted.num', f.id)) },
            { key: 'type', label: 'Type', num: true },
            { key: 'conn', label: 'Link', render: (f) => (f.connected ? ui.badge('connected', 'green') : ui.badge('offline', errs.get(f.id) ? 'red' : 'other', { tip: errs.get(f.id) || 'Not connected (disabled or unreachable)' })) },
            { key: 'mode', label: 'Time stamps', render: (f) => (f.gpsTime ? ui.badge('GPS time', 'cyan', { tip: 'Each frame carries the GPS second of the day and nanoseconds: absolute time, usable for latency analysis and MLAT' }) : f.connected ? ui.badge('12 MHz counter', 'other', { tip: 'Free-running counter or no time stamp: relative time only' }) : DASH) },
            { key: 'gps', label: 'GPS status', render: (f) => { const g = gpsState(f); return g.label === '—' ? DASH : ui.badge(g.label, g.kind, { tip: g.tip }); } },
            { key: 'info', label: 'Receiver says', render: (f) => (f.info ? h('span.tm-info-text', f.info) : DASH) },
            { key: 'last', label: 'Last frame', align: 'right', num: true, render: (f) => (f.lastFrame > 0 ? units.ago(f.lastFrame, rcv.now) : DASH) },
          ],
          rows: feeds,
        }));
      }

      const g = rcv.receiverGps;
      if (!g) {
        ui.mount(rcvGpsHost, h('div.tm-cap', 'Receiver GPS position: none reported. A Radarcape sends its own GPS position from time to time; other receivers never do.'));
      } else {
        const rx = ctx.config.current && ctx.config.current.receiver;
        const dKm = rx && rx.located ? ctx.geo.distanceKm(rx.lat, rx.lon, g.lat, g.lon) : null;
        ui.mount(rcvGpsHost, ui.section('Position reported by the receiver\'s GPS', 'pin'), ui.kv([
          ['Position', units.latlon(g.lat, g.lon, 5)],
          ['Altitude', units.height(g.altM)],
          ['Reported', `${units.ago(g.t, rcv.now)} via feed ${g.feedId}`, { text: true }],
          ['Distance from the configured location', dKm === null ? 'receiver location not set' : dKm < 1 ? units.height(dKm * 1000) : units.dist(dKm), { tip: 'More than a few hundred metres means the location in Settings is wrong' }],
        ], { columns: 2 }));
      }

      const rows = (rcv.history || []).map((r) => ({ ...r, d: decodeStatus(r.status) }));
      const yes = (b, good = true) => ui.badge(b ? 'yes' : 'no', b === good ? 'green' : 'red');
      if (!rows.length) {
        ui.mount(rcvFramesHost, h('div.tm-cap', `Status frames: ${units.int(rcv.statusFrames)} received since AirView started, none stored yet. Only a Radarcape in GPS mode sends them (one per second, with the antenna, satellite and UTC flags).`));
      } else {
        ui.mount(rcvFramesHost, ui.section(`Stored receiver status frames (${units.int(rcv.statusFrames)} received since start)`, 'history'), ui.table({
          class: 'tm-frames',
          columns: [
            { key: 't', label: 'Time', num: true, render: (r) => units.dateTime(r.t) },
            { key: 'feed', label: 'Feed', num: true },
            { key: 'ts', label: 'GPS stamps', render: (r) => (r.d ? yes(r.d.gpsTimestamps) : DASH) },
            { key: 'ant', label: 'Antenna OK', render: (r) => (r.d && r.d.detail ? yes(r.d.antennaOk) : DASH) },
            { key: 'sat', label: 'Satellites', render: (r) => (r.d && r.d.detail ? ui.badge(r.d.sats3 ? '3 or more' : r.d.tracking ? '1-2' : 'none', r.d.sats3 ? 'green' : r.d.tracking ? 'amber' : 'red') : DASH) },
            { key: 'utc', label: 'Time base', render: (r) => (r.d && r.d.detail ? (r.d.utc ? 'UTC' : 'GPS time') : DASH) },
            { key: 'valid', label: 'Stamps valid', render: (r) => (r.d && r.d.detail ? yes(!r.d.bad) : DASH) },
            { key: 'pps', label: '1PPS offset', align: 'right', num: true, render: (r) => (r.d ? units.num(r.d.ppsNs, 1) + ' ns' : DASH), tip: 'Offset of the receiver\'s nanosecond counter at the GPS pulse-per-second edge. Good: a few tens of ns, steady.' },
            { key: 'raw', label: 'Raw', num: true, render: (r) => (r.d ? r.d.hex : typeof r.status === 'string' ? r.status : JSON.stringify(r.status)) },
          ],
          rows,
        }));
      }
    }

    // ------------------------------------------------------------------------------------------
    // feed latency
    // ------------------------------------------------------------------------------------------
    const latHost = h('div.col.gap-4');
    const latPanels = new Map(); // feedId -> panel
    let latState = '';

    function makeLatPanel(feedId) {
      const p50 = ui.stat({ label: 'Median (p50)', accent: 'cyan', tip: 'Half of the frames arrive faster than this' });
      const p95 = ui.stat({ label: 'p95', tip: '95 % of the frames arrive faster than this' });
      const p99 = ui.stat({ label: 'p99', tip: '99 % of the frames arrive faster than this: the slow tail' });
      const spread = ui.stat({ label: 'Mean ± std dev', tip: 'Average latency and its scatter' });
      const histBox = chartBox('Distribution (rolling window)', 'How many frames arrived with which delay after their GPS time stamp. Good: one narrow peak a few ms wide. A long tail to the right means the network or the computer stalls now and then.');
      const timeBox = chartBox('Latency over time (per minute)', 'Fastest, median and 95th percentile of each minute. Good: flat, close together. A slow ramp is a drifting clock; saw-teeth are buffering in the receiver or the network.');
      const kvHost = h('div');
      const note = h('div.tm-inline-note.hidden');
      const refNote = h('div.tm-cap.tm-ref-note');
      const card = ui.card({
        title: 'Feed latency: ' + feedId, icon: 'timer', sub: 'Receiver GPS time stamp, end of frame, against NTP-corrected arrival time',
        body: h('div.col.gap-4', note, h('div.grid.cols-4', p50, p95, p99, spread), h('div.grid.cols-2', histBox.el, timeBox.el), kvHost, refNote),
      });
      let refSig = '';
      return {
        el: card,
        dispose() { histBox.dispose(); timeBox.dispose(); },
        paint(f) {
          const has = f.samples > 0;
          // which instant of the frame this latency starts from, against the latency chain's receiver stage
          const fr = feedRef(lat, f);
          const cr = chainRef(lat && lat.chain);
          const corr = frameEndCorrUs(lat && lat.chain);
          const sig = fr + '|' + cr + '|' + corr;
          if (sig !== refSig) {
            refSig = sig;
            card.setSub(`Receiver GPS time stamp, ${REF_TEXT[fr]}, against NTP-corrected arrival time`);
            ui.setText(refNote, !cr ? `Measured from the ${REF_TEXT[fr]}.`
              : cr === fr ? `Measured from the ${REF_TEXT[fr]}: the same quantity as the "Receiver → host" stage of the latency chain below (that one is filtered per position message).`
                : `Measured from the ${REF_TEXT[fr]} (the receiver's raw time stamp). The "Receiver → host" stage of the latency chain below starts at the ${REF_TEXT[cr]} instead, so it reads ${corr ? `${units.int(corr)} µs` : 'one frame length (64–120 µs)'} longer for the same frame.`);
          }
          p50.set(...(has ? msParts(f.p50Ms) : [DASH, '']));
          p95.set(...(has ? msParts(f.p95Ms) : [DASH, '']));
          p99.set(...(has ? msParts(f.p99Ms) : [DASH, '']));
          spread.set(has ? units.num(f.meanMs, Math.abs(f.meanMs) < 10 ? 2 : 1) : DASH, has ? 'ms' : '');
          spread.setSub(has ? `± ${fmtMs(f.stdMs)}` : '');
          p50.setSub(has ? (f.p50Ms < -2 ? 'negative: see note' : f.p50Ms < 50 ? 'good: local network' : f.p50Ms < 250 ? 'acceptable' : 'slow path') : 'waiting for frames');
          p99.setSub(has ? `tail ${fmtMs(f.p99Ms - f.p50Ms)} above median` : '');

          const notes = [];
          if (!f.clockSynced) notes.push('No NTP reference: these numbers include the whole error of the system clock, so only their spread is meaningful, not the absolute value.');
          const isDemo = !!(rcv && (rcv.feeds || []).some((x) => x.id === f.feedId && x.type === 'demo'));
          if (has && f.p50Ms < -2 && isDemo) {
            notes.push('The simulator stamps its frames with the uncorrected system clock, so its "latency" is simply the NTP offset. A real receiver with GPS time stamps shows the true delay here.');
          } else if (has && f.p50Ms < -2) {
            notes.push(Math.abs(f.p50Ms + 18000) < 1500
              ? 'Frames appear to arrive about 18 s before they were stamped: the receiver counts GPS time, not UTC (GPS time is 18 s ahead). Switch the receiver to UTC.'
              : 'Frames appear to arrive before they were stamped: either the clock correction is wrong (check the NTP offset above) or the receiver\'s time base is not UTC.');
          }
          note.classList.toggle('hidden', !notes.length);
          ui.mount(note, notes.map((t) => h('div', t)));

          ui.mount(kvHost, ui.kv([
            ['Fastest … slowest in window', has ? `${fmtMs(f.minMs)} … ${fmtMs(f.maxMs)}` : DASH],
            ['Last frame', has ? fmtMs(f.lastMs) : DASH],
            ['Samples in window', `${units.int(f.samples)} over ${units.age(f.windowS)}`],
            ['GPS-stamped frames since start', units.int(f.total)],
            ['p01 / p05 / p25 / p75', has ? [f.p01Ms, f.p05Ms, f.p25Ms, f.p75Ms].map((x) => units.num(x, 2)).join(' / ') + ' ms' : DASH],
            ['Clock correction applied', f.clockSynced ? fmtMs(f.clockOffsetMs, { sign: true }) : 'none (NTP not synchronised)'],
            ['Measured from', REF_TEXT[fr], { tip: 'The instant of the frame the latency starts at: the Radarcape stamps the end of a Mode S frame' }],
          ], { columns: 2 }));

          const bins = f.hist || [];
          const totalCount = bins.reduce((n, b) => n + b.count, 0);
          if (!has || !totalCount) {
            histBox.chart.set({ series: [] }, { replace: true });
            histBox.note(h('div', h('div.tm-note-title', 'Not enough data yet'), h('div', 'The histogram fills as soon as GPS-stamped frames arrive on this feed.')));
          } else {
            const width = bins.length ? bins[0].hiMs - bins[0].loMs : 1;
            const digits = width >= 1 ? 0 : width >= 0.1 ? 1 : width >= 0.01 ? 2 : 3;
            const opt = charts.bar({
              x: bins.map((b) => units.num((b.loMs + b.hiMs) / 2, digits)),
              series: [{ name: 'Frames', data: bins.map((b) => b.count), color: charts.colors.blue }],
              xName: 'latency, ms', yName: 'frames', legend: false, valueFormat: (v) => units.int(v),
            });
            opt.grid.bottom = 26;
            const idx = (ms) => { let i = bins.findIndex((b) => ms >= b.loMs && ms < b.hiMs); if (i < 0) i = ms < bins[0].loMs ? 0 : bins.length - 1; return i; };
            opt.series[0].barCategoryGap = '8%';
            opt.series[0].markLine = {
              symbol: 'none', silent: true, animation: false,
              label: { formatter: '{b}', color: '#adbfdf', fontSize: 10 },
              data: [
                { name: 'p50', xAxis: idx(f.p50Ms), lineStyle: { color: charts.colors.cyan } },
                { name: 'p95', xAxis: idx(f.p95Ms), lineStyle: { color: charts.colors.amber } },
                { name: 'p99', xAxis: idx(f.p99Ms), lineStyle: { color: '#ff4d5e' } },
              ],
            };
            histBox.note(null);
            histBox.chart.set(opt, { replace: true });
          }

          const hi = f.history || [];
          if (hi.length < 2) {
            timeBox.chart.set({ series: [] }, { replace: true });
            timeBox.note(h('div', h('div.tm-note-title', 'Not enough data yet'), h('div', `One point is added per minute; a curve needs two (${units.int(hi.length)} so far).`)));
          } else {
            timeBox.note(null);
            timeBox.chart.set(charts.line({
              series: [
                { name: 'p95', data: hi.map((x) => [x.t, x.p95Ms]), color: charts.colors.amber, width: 1.2 },
                { name: 'median', data: hi.map((x) => [x.t, x.p50Ms]), color: charts.colors.cyan, width: 2 },
                { name: 'p05', data: hi.map((x) => [x.t, x.p05Ms]), color: charts.colors.blue, width: 1.1 },
                { name: 'fastest', data: hi.map((x) => [x.t, x.minMs]), color: '#7388ad', width: 1, dashed: true },
              ],
              yName: 'ms', yScale: true, yFormat: axisMs, valueFormat: (v) => (ok(v) ? fmtMs(v) : DASH), zoom: hi.length > 240,
            }), { replace: true });
          }
        },
      };
    }

    function paintLatency() {
      if (!lat) return;
      const feeds = lat.feeds || [];
      if (!feeds.length) {
        for (const p of latPanels.values()) p.dispose();
        latPanels.clear();
        if (latState !== 'empty') {
          latState = 'empty';
          ui.mount(latHost, ui.card({ title: 'Feed latency', icon: 'timer', sub: 'Receiver GPS time stamp against NTP-corrected arrival time',
            body: ui.empty({ icon: 'timer', title: 'No feed delivers GPS time stamps', text: 'Latency can only be measured when the receiver stamps every frame with absolute time: a Radarcape with GPS time stamps switched on (or the demo simulator). Receivers with a 12 MHz counter give relative time only.' }) }));
        }
        return;
      }
      if (latState !== 'feeds') { latState = 'feeds'; latHost.replaceChildren(); }
      const seen = new Set();
      for (const f of feeds) {
        seen.add(f.feedId);
        let p = latPanels.get(f.feedId);
        if (!p) { p = makeLatPanel(f.feedId); latPanels.set(f.feedId, p); latHost.appendChild(p.el); }
        p.paint(f);
      }
      for (const [id, p] of latPanels) if (!seen.has(id)) { p.dispose(); p.el.remove(); latPanels.delete(id); }
    }

    // ------------------------------------------------------------------------------------------
    // loaders
    // ------------------------------------------------------------------------------------------
    const errorIn = (host, e, title, retry) => ui.mount(host, ui.error(e, { title, retry, compact: true }));

    async function loadNtp() {
      try {
        const d = await api.get('/api/timing/ntp', { signal: ctx.signal });
        if (!alive) return;
        ntp = d;
        paintNtp(); paintBanners(); paintClockSubs();
        if (hist && (hist.samples || []).length < 2) paintHistory();
      } catch (e) {
        if (!alive || (e && e.aborted) || ntp) return;
        errorIn(ntpKvHost, e, 'Could not load the NTP status', loadNtp);
        ntpStatsHost.replaceChildren();
      }
    }
    let histSeq = 0;
    async function loadHistory(o = {}) {
      const my = ++histSeq;
      if (o.show) for (const c of [cOffset, cDelay, cJitter]) c.chart.loading(true);
      try {
        const d = await api.get('/api/timing/history', { params: { hours, limit: 2000 }, signal: ctx.signal });
        if (!alive || my !== histSeq) return;
        hist = d;
        paintHistory();
      } catch (e) {
        if (!alive || my !== histSeq || (e && e.aborted)) return;
        if (!hist || o.show) {
          hist = null;
          const node = () => ui.error(e, { title: 'Could not load the NTP history', retry: () => loadHistory({ show: true }), compact: true });
          for (const c of [cOffset, cDelay, cJitter]) { c.chart.set({ series: [] }, { replace: true }); c.note(node()); }
        }
      } finally {
        if (my === histSeq) for (const c of [cOffset, cDelay, cJitter]) c.chart.loading(false);
      }
    }
    async function loadLatency() {
      try {
        const d = await api.get('/api/timing/latency', { signal: ctx.signal });
        if (!alive) return;
        lat = d;
        paintLatency(); paintClockSubs(); paintBanners();
        tdoa.setLatency(d);
      } catch (e) {
        if (!alive || (e && e.aborted) || lat) return;
        latState = 'error';
        tdoa.setLatency(null);
        ui.mount(latHost, ui.card({ title: 'Feed latency', icon: 'timer', body: ui.error(e, { title: 'Could not load the feed latency', retry: loadLatency, compact: true }) }));
      }
    }
    async function loadReceiver() {
      try {
        const d = await api.get('/api/timing/receiver', { params: { limit: 30 }, signal: ctx.signal });
        if (!alive) return;
        rcv = d;
        paintReceiver(); paintBanners();
      } catch (e) {
        if (!alive || (e && e.aborted) || rcv) return;
        errorIn(rcvFeedsHost, e, 'Could not load the receiver timing state', loadReceiver);
      }
    }

    // ------------------------------------------------------------------------------------------
    // layout
    // ------------------------------------------------------------------------------------------
    ui.mount(ntpKvHost, ui.skeleton({ lines: 8 }));
    ui.mount(ntpStatsHost, ui.skeleton({ lines: 3 }));
    ui.mount(rcvFeedsHost, ui.skeleton({ lines: 3 }));
    ui.mount(latHost, ui.card({ title: 'Feed latency', icon: 'timer', body: ui.skeleton({ lines: 4 }) }));
    ui.mount(rcvCard.body, h('div.col.gap-4', rcvFeedsHost, rcvGpsHost, rcvFramesHost));

    const explain = (term, text) => h('div.tm-explain-item', h('dt', term), h('dd', text));
    const explainCard = ui.card({
      title: 'What the numbers mean for an ADS-B station', icon: 'help',
      body: h('dl.tm-explain',
        explain('Offset', 'How far this computer\'s clock is from true UTC as measured over NTP. Every flight, statistic and log line is stamped with the system clock, and aggregators discard data whose time stamps are seconds off. Good: within ±50 ms and flat. AirView measures only - it never sets the clock.'),
        explain('Delay', 'Round trip to the time server. The offset cannot be more accurate than about half of the delay\'s asymmetry, so a short, steady path (a server on your own network or at your provider) gives the best reference.'),
        explain('Jitter', 'How much successive offsets scatter. Below 5 ms is typical for a wired connection; tens of ms point at Wi-Fi, a busy uplink or an overloaded computer.'),
        explain('Stratum and reference ID', 'Stratum 1 servers have a GPS or atomic clock attached; each hop adds one. Anything from 1 to 4 is fine. Stratum 16 or leap "alarm" means the server itself has lost its time.'),
        explain('Drift', 'Rate at which the system clock gains or loses, in parts per million: 1 ppm is 86 ms per day. A few ppm is normal for a quartz oscillator and is removed by the operating system\'s time service.'),
        explain('Receiver GPS time stamps', 'A Radarcape in GPS mode stamps every frame with GPS-disciplined time to the nanosecond. Multilateration (MLAT) relies on these stamps, not on the computer\'s clock - which is why a GPS lock matters far more for MLAT than NTP does.'),
        explain('Feed latency', 'GPS time stamp of a frame compared with the NTP-corrected moment it reached AirView: receiver processing plus network transit plus whatever clock error is left. Good: a few ms to a few tens of ms with a narrow distribution. Position decoding pairs messages within 10 s, so only latencies of seconds do harm, but a wide or growing tail is an early sign of network trouble.')),
    });

    container.append(h('div.page-inner.tm-page',
      h('div.page-head', h('div', h('h2', 'Timing'), h('div.sub', 'NTP reference, system clock and receiver time stamps')), h('div.actions', measureBtn)),
      banners,
      h('div.grid.cols-4.tm-gap', cSys, cNtp, cGps, cBrowser),
      h('div.tm-section', ui.section('NTP reference', 'globe')),
      h('div.grid-auto.tm-gap', { style: { '--min': '170px' } }, tOffset, tDelay, tJitter, tStratum, tLast, tDrift),
      ui.card({ title: 'NTP history', icon: 'chart', sub: 'Offset, delay and jitter over time', actions: [histInfo, rangeSeg], class: 'tm-gap', body: h('div.grid.cols-3', cOffset.el, cDelay.el, cJitter.el) }),
      h('div.grid.cols-2.tm-gap',
        h('div.col.gap-4',
          ui.card({ title: 'NTP server', icon: 'globe', sub: 'Newest measurement', body: ntpKvHost }),
          ui.card({ title: 'Offset statistics', icon: 'chart', sub: 'How stable the system clock is', body: ntpStatsHost })),
        explainCard),
      h('div.tm-section', ui.section('Receiver', 'radar')),
      h('div.tm-gap', rcvCard),
      latHost,
      tdoa.el));

    // ---- go ----
    tickClocks();
    paintClockSubs();
    ctx.every(100, () => { if (!document.hidden) tickClocks(); });
    ctx.every(1000, () => { if (!document.hidden) paintClockSubs(); });
    loadNtp(); loadHistory(); loadLatency(); loadReceiver();
    ctx.every(10000, () => { if (!document.hidden) loadNtp(); });
    ctx.every(60000, () => { if (!document.hidden) loadHistory(); });
    ctx.every(5000, () => { if (!document.hidden) loadLatency(); });
    ctx.every(15000, () => { if (!document.hidden) loadReceiver(); });
    ctx.subscribe('status', () => { if (rcv) paintReceiver(); }, { immediate: false });
    const onVis = () => { if (!document.hidden) { loadNtp(); loadLatency(); loadReceiver(); loadHistory(); tdoa.refresh(); } };
    document.addEventListener('visibilitychange', onVis);
    ctx.onCleanup(() => document.removeEventListener('visibilitychange', onVis));
    ctx.onCleanup(units.onChange(() => { paintNtp(); paintHistory(); paintReceiver(); paintLatency(); bannerSig = ''; paintBanners(); }));
  },
};
