// AirView Feeder status page. Plain script, no dependencies. Talks to the
// local feeder over /api/* (see internal/feederui/server.go).
'use strict';

(function () {
  const $ = (id) => document.getElementById(id);
  const HDR = { 'Content-Type': 'application/json', 'X-AirView-Feeder': '1' };
  let snap = null;
  let settings = null;
  let settingsDirty = false;

  const esc = (s) => String(s == null ? '' : s).replace(/[&<>"']/g, (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));
  // Consent texts C1-C4 (docs/aggregator/CONSENT.md, word for word; checked by
  // internal/feederui TestConsentTextsMatchDoc), shown next to the timestamps,
  // pin, name and statistics settings.
  const CONSENT_C1 = `If your receiver sends timestamps, the network operator can determine its location precisely, whatever you enter. Other users and the public cannot. Choose 'No timestamps (no MLAT)' if you do not want this.`;
  const CONSENT_C2 = `Show my station on the public map?
  - Hidden (default): no pin.
  - Region: a point placed at random inside a 0.5-degree area around your station, about 55 by 35 km in the UK.
  - Area: the same inside a 0.1-degree area, about 11 by 7 km. Where fewer than 3 stations are nearby it is shown as Region.
The point is chosen once and stays put, so it cannot be averaged out. Your exact position is never public. You can change this at any time.`;
  const CONSENT_C3 = `Show my station name publicly? Others will see the name next to the station's public statistics and, if you chose a pin, next to the pin. Do not use your callsign or real name if you want to stay anonymous: a name together with even a rough area can identify you.`;
  const CONSENT_C4 = `Publish my station's statistics? Only figures that do not reveal where your station is: message rate, uptime, aircraft per day, positions per day and MLAT contribution. Range, coverage, polar plots and records stay private to you.`;
  const CONSENT_C9 = `Beast connections are not encrypted. Your receiver data and this UUID go to that aggregator's operator under their terms. AirView never forwards MLAT results to them.`;
  const consentHTML = (t) => `<p class="muted consent">${esc(t).replace(/\n/g, '<br>')}</p>`;
  const num = (v, d = 0) => (v == null || isNaN(v) ? '-' : Number(v).toLocaleString(undefined, { maximumFractionDigits: d, minimumFractionDigits: d }));
  const bytes = (b) => {
    if (!b) return '0 B';
    const u = ['B', 'KiB', 'MiB', 'GiB'];
    let i = 0;
    while (b >= 1024 && i < u.length - 1) { b /= 1024; i++; }
    return b.toFixed(i ? 1 : 0) + ' ' + u[i];
  };
  const ago = (ms, now) => {
    if (!ms) return '-';
    let s = Math.max(0, Math.round(((now || Date.now()) - ms) / 1000));
    if (s < 60) return s + ' s';
    if (s < 3600) return Math.floor(s / 60) + ' min';
    if (s < 86400) return Math.floor(s / 3600) + ' h ' + Math.floor((s % 3600) / 60) + ' min';
    return Math.floor(s / 86400) + ' d ' + Math.floor((s % 86400) / 3600) + ' h';
  };
  const kv = (label, value) => `<div><span>${esc(label)}</span><b>${value}</b></div>`;

  async function api(method, path, body) {
    const opt = { method, headers: method === 'GET' ? {} : HDR, cache: 'no-store' };
    if (body !== undefined) opt.body = JSON.stringify(body);
    const r = await fetch(path, opt);
    let j = null;
    try { j = await r.json(); } catch (_) { /* empty */ }
    if (!r.ok) throw new Error((j && j.error) || r.status + ' ' + r.statusText);
    return j;
  }

  // ---- explanation of AVF error codes that need the user ----
  const CODE_HELP = {
    enrol_required: 'The aggregator only accepts invited feeders. Ask its operator for an enrol token and enter it under Settings.',
    enrol_invalid: 'The enrol token was not accepted (wrong, used up or expired). Ask the operator for a new one.',
    revoked: 'The aggregator operator revoked this feeder.',
    cloned_feeder: 'Another computer is using the same feeder identity. Run "airview-feeder new-identity" on one of them.',
    proto_version: 'This feeder is too old or too new for the aggregator. Update the feeder.',
    denied: 'The aggregator refused this feeder.',
  };
  const STATE_CLASS = { connected: 'ok', handshake: 'warn', connecting: 'warn', backoff: 'warn', idle: 'warn', stopped: 'bad', disabled: '' };

  function renderUplinks(s) {
    const box = $('uplinks');
    if (!s.uplinks || !s.uplinks.length) {
      box.innerHTML = '<p class="muted">No aggregator configured yet. Add one under Settings, or run <span class="mono">airview-feeder setup</span>.</p>';
      return;
    }
    box.innerHTML = s.uplinks.map((u) => {
      const cls = STATE_CLASS[u.state] || '';
      let state = u.state;
      if (u.state === 'connected' && u.approval === 'pending') state = 'connected, waiting for approval';
      if (u.state === 'backoff' && u.retryAtMs) state = 'reconnecting in ' + Math.max(0, Math.round((u.retryAtMs - s.nowMs) / 1000)) + ' s';
      const c = u.counters || {};
      const b = u.buffer || {};
      let err = '';
      if (u.lastError && u.state !== 'connected') {
        err = `<div class="err">${esc(u.lastError)}${u.errorCode ? ' (' + esc(u.errorCode) + ')' : ''}</div>`;
        if (CODE_HELP[u.errorCode]) err += `<div class="note">${esc(CODE_HELP[u.errorCode])}</div>`;
      }
      const notices = (u.notices || []).slice(-3).map((n) => `<div>${esc(new Date(n.atMs).toLocaleString())}: ${esc(n.text || n.code)}</div>`).join('');
      const rx = (u.receivers || []).map((r) => {
        const fb = r.feedback || null;
        let t = `rx ${r.rxNo}: ${esc(r.clockClass || 'untimed')}`;
        if (fb && fb.sync) t += ', sync ' + esc(fb.sync);
        return t;
      }).join(' &middot; ');
      return `<div class="row ${cls}">
        <div class="row-head"><span class="dot"></span><span class="name">${esc(u.serverName || u.host)}</span>
          <span class="mono muted">${esc(u.host)}:${esc(u.port)}</span><span class="pill">${esc(state)}</span>
          ${u.mode ? `<span class="pill">${esc(u.mode)}</span>` : ''}
          ${u.state === 'stopped' || u.state === 'backoff' ? `<button class="btn small" data-retry="${esc(u.id)}">Retry now</button>` : ''}</div>
        ${err}
        ${u.motd ? `<div class="muted">${esc(u.motd)}</div>` : ''}
        <div class="kv">
          ${kv('Connected for', u.state === 'connected' ? ago(u.connectedMs, s.nowMs) : '-')}
          ${kv('Frames sent', num(c.framesSent))}
          ${kv('Late (backfill)', num(c.framesLate))}
          ${kv('Sent this session', bytes(c.bytesWire))}
          ${kv('Buffered', num(b.ringRecords) + ' rec, ' + num(b.ringSpanS) + ' s')}
          ${kv('Disk spool', b.spoolMb ? bytes(b.spoolBytes) + ' / ' + b.spoolMb + ' MiB' : 'off')}
          ${kv('MLAT results in', num(c.resultsIn))}
          ${kv('Lost (buffers full)', num(c.droppedRecords))}
        </div>
        ${rx ? `<div class="muted">${rx}</div>` : ''}
        ${notices ? `<div class="notices">${notices}</div>` : ''}
      </div>`;
    }).join('');
  }

  // Beast outputs to other aggregators ("also feed others").
  function renderBeast(s) {
    const box = $('beast-outputs');
    if (!box) return;
    const list = s.beast || [];
    box.classList.toggle('hidden', !list.length);
    box.innerHTML = list.length ? '<h3>Other aggregators (Beast)</h3>' + list.map((b) => {
      const cls = STATE_CLASS[b.state] || '';
      return `<div class="row ${cls}">
        <div class="row-head"><span class="dot"></span><span class="name">${esc(b.host)}</span>
          <span class="mono muted">${esc(b.host)}:${esc(b.port)}</span><span class="pill">${esc(b.state)}</span><span class="pill">${esc(b.format)}</span>
          ${b.state === 'backoff' ? `<button class="btn small" data-retry="${esc(b.id)}">Retry now</button>` : ''}</div>
        ${b.lastError && b.state !== 'connected' ? `<div class="err">${esc(b.lastError)}</div>` : ''}
        <div class="kv">
          ${kv('Frames sent', num(b.framesSent))}
          ${kv('Reduced', num(b.reduced))}
          ${kv('Dropped while down', num(b.dropped))}
          ${kv('MLAT results refused', num(b.mlatRefused))}
          ${kv('Ping echoes', num(b.pingsEchoed))}
          ${kv('Slow-down requests', num(b.slowDowns))}
        </div>
        ${b.uuid ? `<div class="muted">UUID <span class="mono">${esc(b.uuid)}</span> <button class="btn small ghost" type="button" data-copy="${esc(b.uuid)}">Copy</button>
          <br>Use the same UUID for an mlat-client to this aggregator, so it joins both links into one station.</div>` : ''}
      </div>`;
    }).join('') : '';
  }

  function renderReceivers(s) {
    const box = $('receivers');
    if (!s.receivers || !s.receivers.length) {
      box.innerHTML = '<p class="muted">No receiver configured.</p>';
      return;
    }
    box.innerHTML = s.receivers.map((r) => {
      const st = r.stats || {};
      const fresh = st.lastFrameMs && s.nowMs - st.lastFrameMs < 30000;
      const cls = !r.enabled ? '' : r.connected ? (fresh ? 'ok' : 'warn') : 'bad';
      let state = !r.enabled ? 'disabled' : r.connected ? (fresh ? 'receiving' : 'connected, no data') : 'not connected';
      if (r.inputType === 'auto' && !r.detected) state = 'searching for a receiver';
      const pos = r.located ? `${num(r.lat, 5)}, ${num(r.lon, 5)}, ${num(r.altM, 0)} m ${esc((r.altRef || '').toUpperCase())}` : 'not declared';
      return `<div class="row ${cls}">
        <div class="row-head"><span class="dot"></span><span class="name">${esc(r.name || 'receiver')}</span>
          <span class="muted">rx ${esc(r.rxNo)}</span><span class="pill">${esc(state)}</span>
          <span class="mono muted">${esc(r.detected || r.inputType)}${r.addr ? ' @ ' + esc(r.addr) : ''}</span></div>
        ${r.lastError && !r.connected ? `<div class="err">${esc(r.lastError)}</div>` : ''}
        <div class="kv">
          ${kv('Messages / s', num(st.fps, 1))}
          ${kv('Aircraft (1 min)', num(st.aircraft) + (st.aircraftAdsb != null ? ' (' + num(st.aircraftAdsb) + ' ADS-B)' : ''))}
          ${kv('Messages total', num(st.frames + (st.decoded || 0)))}
          ${kv('Positions', num(st.positions))}
          ${kv('Signal (mean)', st.hasSignal ? num(st.signalDbfs, 1) + ' dBFS' : '-')}
          ${kv('Strong (> -3 dBFS)', st.hasSignal ? num(st.strongPct, 1) + ' %' : '-')}
          ${kv('Clock', esc(r.clockClass || 'not known yet') + (r.epoch ? ' / epoch ' + r.epoch : ''))}
          ${kv('Position', pos)}
        </div>
        ${r.info ? `<div class="muted">${esc(r.info)}</div>` : ''}
      </div>`;
    }).join('');
  }

  function renderResults(s) {
    const r = s.results || {};
    let out = kv('Beast output', r.beastPort ? (r.listening ? '127.0.0.1:' + r.beastPort : 'not listening' + (r.listenError ? ': ' + esc(r.listenError) : '')) : 'off');
    out += kv('Clients', num(r.clients));
    out += kv('Push to readsb', r.readsbAddr ? esc(r.readsbAddr) + (r.readsbConnected ? ' (connected)' : r.readsbError ? ' (' + esc(r.readsbError) + ')' : ' (not connected)') : 'off');
    out += kv('Results received', num(r.received));
    out += kv('Frames delivered', num(r.frames));
    $('results').innerHTML = out;
  }

  function renderOverall(s) {
    const ups = s.uplinks || [];
    const conn = ups.filter((u) => u.state === 'connected').length;
    const recv = (s.receivers || []).filter((r) => r.connected).length;
    let cls = 'ok';
    let text = `Feeding ${conn} of ${ups.length} aggregator${ups.length === 1 ? '' : 's'}`;
    if (!ups.length) { cls = 'warn'; text = 'No aggregator configured'; } else if (conn < ups.length) cls = conn ? 'warn' : 'bad';
    if (!recv) { cls = 'bad'; text += ', no receiver connected'; }
    $('overall').className = 'overall ' + cls;
    $('overall').innerHTML = `<span class="dot"></span><span>${esc(text)}</span>`;
    $('ident').textContent = `feeder ${s.feederId}  ·  ${s.software} ${s.version}  ·  up ${ago(s.startedMs, s.nowMs)}`;
    const banner = $('banner');
    const notes = s.notes || [];
    if (notes.length) {
      banner.className = 'banner';
      banner.innerHTML = '<b>Configuration notes</b><br>' + notes.map(esc).join('<br>');
    } else banner.className = 'banner hidden';
    $('foot').textContent = `Config: ${s.configFile || '-'}  ·  data: ${s.dataDir || '-'}  ·  ${s.os}/${s.arch}`;
  }

  function fillClaimSelects(s) {
    const us = $('claim-uplink');
    const rs = $('claim-rx');
    const keepU = us.value;
    const keepR = rs.value;
    us.innerHTML = (s.uplinks || []).map((u) => `<option value="${esc(u.id)}">${esc(u.serverName || u.host)}</option>`).join('');
    rs.innerHTML = (s.receivers || []).map((r) => `<option value="${esc(r.rxNo)}">${esc(r.rxNo + ': ' + (r.name || 'receiver'))}</option>`).join('');
    if (keepU) us.value = keepU;
    if (keepR) rs.value = keepR;
    $('claim-btn').disabled = !(s.uplinks || []).some((u) => u.state === 'connected');
  }

  async function refreshUpdate() {
    const el = $('update-state');
    if (!el) return;
    try {
      const u = await api('GET', 'api/update');
      const parts = [];
      if (u.available) parts.push('version ' + u.available + ' available');
      if (u.last && u.last.phase) parts.push('last update: ' + u.last.phase);
      el.textContent = parts.join(' · ');
      const v = $('update-version');
      if (v) v.textContent = snap && snap.version ? '(running ' + snap.version + ')' : '';
    } catch (_) { el.textContent = ''; }
  }

  async function refresh() {
    try {
      snap = await api('GET', 'api/status');
      renderOverall(snap);
      renderUplinks(snap);
      renderBeast(snap);
      renderReceivers(snap);
      renderResults(snap);
      fillClaimSelects(snap);
    } catch (e) {
      $('overall').className = 'overall bad';
      $('overall').innerHTML = `<span class="dot"></span><span>Feeder not reachable: ${esc(e.message)}</span>`;
    }
  }

  document.addEventListener('click', (ev) => {
    const t = ev.target && ev.target.getAttribute && ev.target.getAttribute('data-copy');
    if (t && navigator.clipboard) navigator.clipboard.writeText(t).catch(() => {});
  });

  document.addEventListener('click', async (ev) => {
    const id = ev.target && ev.target.getAttribute && ev.target.getAttribute('data-retry');
    if (id == null) return;
    ev.target.disabled = true;
    try { await api('POST', 'api/retry', { id }); } catch (e) { alert(e.message); }
    setTimeout(refresh, 500);
  });

  $('claim-btn').addEventListener('click', async () => {
    const out = $('claim-out');
    out.className = 'claim';
    out.innerHTML = 'Asking the aggregator...';
    try {
      const r = await api('POST', 'api/claim', { id: $('claim-uplink').value, rxNo: Number($('claim-rx').value) || 0 });
      out.innerHTML = `<div class="code">${esc(r.code)}</div><div class="muted">valid until ${esc(new Date(r.expiresMs).toLocaleString())}</div>`;
    } catch (e) {
      out.innerHTML = `<span class="err">${esc(e.message)}</span>`;
    }
  });

  // ---- settings ----

  function receiverForm(r, i) {
    const inp = r.input || {};
    return `<div class="sub-card" data-rx="${i}">
      <div class="grid">
        <label>Name<input type="text" maxlength="32" data-f="name" value="${esc(r.name)}"></label>
        <label>Input<select data-f="input.type">
          ${['auto', 'beast', 'avr', 'sbs', 'rtltcp'].map((t) => `<option value="${t}"${inp.type === t ? ' selected' : ''}>${t === 'auto' ? 'auto-detect' : t === 'rtltcp' ? 'RTL-SDR (rtl_tcp)' : t}</option>`).join('')}
        </select></label>
        <label>Host<input type="text" data-f="input.host" placeholder="127.0.0.1" value="${esc(inp.host)}"></label>
        <label>Port<input type="number" min="0" max="65535" data-f="input.port" value="${esc(inp.port || '')}"></label>
        <label>Clock class<select data-f="clockClass">
          ${['', 'gps_utc', 'gps_gpst', 'gps_emul', 'hw12', 'sdr12', 'sdr12_hires', 'sbs20', 'none'].map((c) => `<option value="${c}"${(r.clockClass || '') === c ? ' selected' : ''}>${c || 'detect'}</option>`).join('')}
        </select></label>
      </div>
      <div class="grid mt">
        <label class="check"><input type="checkbox" data-f="located"${r.located ? ' checked' : ''}> Declare position</label>
        <label>Latitude<input type="number" step="any" min="-90" max="90" data-f="lat" value="${esc(r.lat)}"></label>
        <label>Longitude<input type="number" step="any" min="-180" max="180" data-f="lon" value="${esc(r.lon)}"></label>
        <label>Antenna altitude (m)<input type="number" step="any" data-f="altM" value="${esc(r.altM)}"></label>
        <label>Altitude reference<select data-f="altRef">
          <option value="msl"${r.altRef !== 'hae' ? ' selected' : ''}>above mean sea level</option>
          <option value="hae"${r.altRef === 'hae' ? ' selected' : ''}>above the WGS-84 ellipsoid</option></select></label>
        <label>Antenna above ground (m)<input type="number" step="any" min="0" max="1000" data-f="aglM" value="${esc(r.aglM)}"></label>
      </div>
      ${inp.type === 'rtltcp' ? `<div class="grid mt">
        <label>Gain (max, auto or dB)<input type="text" data-f="input.sdr.gain" placeholder="max" value="${esc((inp.sdr || {}).gain || '')}"></label>
        <label>Frequency correction (ppm)<input type="number" min="-500" max="500" data-f="input.sdr.ppm" value="${esc((inp.sdr || {}).ppm || 0)}"></label>
        <label class="check"><input type="checkbox" data-f="input.sdr.biasTee"${(inp.sdr || {}).biasTee ? ' checked' : ''}> Bias tee (antenna power)</label>
        <label class="check"><input type="checkbox" data-f="input.sdr.spawn"${(inp.sdr || {}).spawn ? ' checked' : ''}> Start my rtl_tcp</label>
      </div><p class="muted">Uses your own rtl_tcp (from the rtl-sdr package; AirView does not ship it). Keep it on 127.0.0.1:1234.</p>` : ''}</div>`;
  }

  function typeSelect(u) {
    return `<label>Type<select data-f="type">
          <option value=""${u.type !== 'beast' ? ' selected' : ''}>AirView network (encrypted)</option>
          <option value="beast"${u.type === 'beast' ? ' selected' : ''}>Other aggregator (Beast, e.g. readsb on port 30004)</option></select></label>`;
  }

  function beastForm(u, i) {
    return `<div class="sub-card" data-up="${i}">
      <div class="grid">
        ${typeSelect(u)}
        <label>Host<input type="text" data-f="host" value="${esc(u.host)}" placeholder="feed.example.org"></label>
        <label>Port<input type="number" min="1" max="65535" data-f="port" value="${esc(u.port || 30004)}"></label>
        <label>Format<select data-f="format">
          ${['beast_reduce_plus_out', 'beast_reduce_out', 'beast_out'].map((f) => `<option value="${f}"${(u.format || 'beast_reduce_plus_out') === f ? ' selected' : ''}>${f}</option>`).join('')}
        </select></label>
        <label>UUID<input type="text" data-f="uuid" value="${esc(u.uuid)}" placeholder="leave empty to generate one"></label>
        <label>Reduce interval (ms)<input type="number" min="0" max="5000" data-f="reduceMs" value="${esc(u.reduceMs || '')}" placeholder="250"></label>
      </div>
      <div class="grid mt">
        <label class="check"><input type="checkbox" data-f="timestamps"${u.timestamps ? ' checked' : ''}> Send timestamps</label>
        <label class="check"><input type="checkbox" data-f="disabled"${u.disabled ? ' checked' : ''}> Paused</label>
      </div>
      ${consentHTML(CONSENT_C9)}
      <div class="form-row end"><button class="btn small ghost" type="button" data-remove-up="${i}">Remove</button></div></div>`;
  }

  function uplinkForm(u, i) {
    if (u.type === 'beast') return beastForm(u, i);
    return `<div class="sub-card" data-up="${i}">
      <div class="grid">
        ${typeSelect(u)}
        <label>Aggregator host<input type="text" data-f="host" value="${esc(u.host)}" placeholder="feed.example.org"></label>
        <label>Port<input type="number" min="1" max="65535" data-f="port" value="${esc(u.port || 30160)}"></label>
        <label>Server pin<input type="text" data-f="serverPin" value="${esc(u.serverPin)}" placeholder="sha256/..."></label>
        <label>Enrol token<input type="password" data-f="enrolToken" value="${esc(u.enrolToken)}" placeholder="from the operator"></label>
        <label>Public map pin<select data-f="pin">
          <option value="hidden"${u.pin === 'hidden' || !u.pin ? ' selected' : ''}>hidden</option>
          <option value="region"${u.pin === 'region' ? ' selected' : ''}>region (0.5-degree area)</option>
          <option value="cell01"${u.pin === 'cell01' ? ' selected' : ''}>area (0.1-degree area)</option></select></label>
      </div>
      <div class="grid mt">
        <label class="check"><input type="checkbox" data-f="sendLocation"${u.sendLocation ? ' checked' : ''}> Send my declared position</label>
        <label class="check"><input type="checkbox" data-f="timestamps"${u.timestamps ? ' checked' : ''}> Send timestamps (MLAT)</label>
        <label class="check"><input type="checkbox" data-f="returnMlat"${u.returnMlat ? ' checked' : ''}> Receive MLAT results</label>
        <label class="check"><input type="checkbox" data-f="publicStats"${u.publicStats ? ' checked' : ''}> Public statistics</label>
        <label class="check"><input type="checkbox" data-f="showName"${u.showName ? ' checked' : ''}> Show name publicly</label>
        <label class="check"><input type="checkbox" data-f="disabled"${u.disabled ? ' checked' : ''}> Paused</label>
      </div>
      ${consentHTML(CONSENT_C1)}
      <details class="mt"><summary>What the public pin, name and statistics show</summary>
        ${consentHTML(CONSENT_C2)}${consentHTML(CONSENT_C3)}${consentHTML(CONSENT_C4)}</details>
      <div class="form-row end"><button class="btn small ghost" type="button" data-remove-up="${i}">Remove</button></div></div>`;
  }

  function renderSettings() {
    const c = settings.config;
    $('settings-file').textContent = settings.readOnly ? 'Read only: ' + (settings.file || 'no configuration file') : 'Saved to ' + settings.file;
    const lk = $('settings-locked');
    if (settings.locked && settings.locked.length) {
      lk.className = 'note';
      lk.textContent = 'Set by environment variables or command-line options (they win over this page): ' + settings.locked.join(', ');
    } else lk.className = 'note hidden';
    $('set-receivers').innerHTML = (c.receivers || []).map(receiverForm).join('') || '<p class="muted">No receiver.</p>';
    $('set-uplinks').innerHTML = (c.uplinks || []).map(uplinkForm).join('') || '<p class="muted">No aggregator.</p>';
    document.querySelectorAll('#settings [data-k]').forEach((el) => {
      const [a, b] = el.getAttribute('data-k').split('.');
      el.value = (c[a] || {})[b] == null ? '' : c[a][b];
    });
    // Update source and release keys: read only here (SEC-R1); the
    // installer or an administrator sets them in the configuration file.
    const up = c.update || {};
    const keys = (up.keys || []).length;
    $('set-update').innerHTML = `<div><span>Automatic updates</span><b>${c.autoUpdate ? 'on' : 'off'}</b></div>
      <div><span>Release manifest</span><b>${esc(up.manifestUrl || 'derived from the aggregator')}</b></div>
      <div><span>Release keys</span><b>${keys ? keys + (keys === 1 ? ' key' : ' keys') : 'none'}</b></div>`;
    $('set-update-note').textContent = 'Set by the installer or an administrator in the configuration file; not editable on this page.';
  }

  function setPath(obj, path, val) {
    const parts = path.split('.');
    let o = obj;
    for (let i = 0; i < parts.length - 1; i++) {
      if (o[parts[i]] == null || typeof o[parts[i]] !== 'object') o[parts[i]] = {};
      o = o[parts[i]];
    }
    o[parts[parts.length - 1]] = val;
  }

  // Number fields that must not be saved as 0 when cleared (UX-04): an
  // empty one is a validation error while "Declare position" is on.
  const REQUIRED_NUM = { lat: 'Latitude', lon: 'Longitude', altM: 'Antenna altitude' };

  function readField(el) {
    if (el.type === 'checkbox') return el.checked;
    if (el.type === 'number') {
      if (el.value === '') return REQUIRED_NUM[el.getAttribute('data-f')] ? null : 0;
      return Number(el.value);
    }
    return el.value.trim();
  }

  function collect() {
    const c = JSON.parse(JSON.stringify(settings.config));
    document.querySelectorAll('[data-rx]').forEach((card) => {
      const r = c.receivers[Number(card.getAttribute('data-rx'))];
      const prev = JSON.parse(JSON.stringify(r));
      card.querySelectorAll('[data-f]').forEach((el) => setPath(r, el.getAttribute('data-f'), readField(el)));
      const empty = Object.keys(REQUIRED_NUM).filter((k) => r[k] === null);
      if (empty.length && r.located) {
        throw new Error(empty.map((k) => REQUIRED_NUM[k]).join(', ') + ' cannot be empty while "Declare position" is on');
      }
      empty.forEach((k) => { r[k] = prev[k] == null ? 0 : prev[k]; }); // not declared: keep what was there
    });
    document.querySelectorAll('[data-up]').forEach((card) => {
      const u = c.uplinks[Number(card.getAttribute('data-up'))];
      card.querySelectorAll('[data-f]').forEach((el) => setPath(u, el.getAttribute('data-f'), readField(el)));
    });
    document.querySelectorAll('#settings [data-k]').forEach((el) => setPath(c, el.getAttribute('data-k'), readField(el)));
    return c;
  }

  async function loadSettings() {
    try {
      settings = await api('GET', 'api/settings');
      settingsDirty = false;
      renderSettings();
    } catch (e) {
      $('save-msg').textContent = 'Cannot load settings: ' + e.message;
    }
  }

  $('settings-box').addEventListener('toggle', () => { if ($('settings-box').open && !settingsDirty) loadSettings(); });
  $('settings').addEventListener('input', () => { settingsDirty = true; });
  $('settings').addEventListener('change', (ev) => {
    // switching an uplink's type or a receiver's input re-renders its form
    const f = ev.target && ev.target.getAttribute && ev.target.getAttribute('data-f');
    if (f !== 'type' && f !== 'input.type') return;
    settings.config = collect();
    for (const u of settings.config.uplinks || []) {
      if (u.type === 'beast' && (!u.port || u.port === 30160)) u.port = 30004;
      if (u.type !== 'beast' && u.port === 30004) u.port = 30160;
    }
    for (const r of settings.config.receivers || []) {
      if (r.input && r.input.type === 'rtltcp' && !r.input.port) r.input.port = 1234;
    }
    settingsDirty = true;
    renderSettings();
  });
  $('add-uplink').addEventListener('click', () => {
    settings.config = collect();
    settings.config.uplinks = settings.config.uplinks || [];
    settings.config.uplinks.push({ id: 'uplink-' + (settings.config.uplinks.length + 1), host: '', port: 30160, serverPin: '', enrolToken: '', sendLocation: true, timestamps: true, returnMlat: true, spoolMb: -1, pin: 'hidden', publicStats: false, showName: false, disabled: false });
    settingsDirty = true;
    renderSettings();
  });
  $('settings').addEventListener('click', (ev) => {
    const i = ev.target.getAttribute && ev.target.getAttribute('data-remove-up');
    if (i == null) return;
    if (!confirm('Remove this aggregator? The feeder stops sending to it.')) return;
    settings.config = collect();
    settings.config.uplinks.splice(Number(i), 1);
    settingsDirty = true;
    renderSettings();
  });
  $('settings').addEventListener('submit', async (ev) => {
    ev.preventDefault();
    const msg = $('save-msg');
    msg.textContent = 'Saving...';
    try {
      const r = await api('PUT', 'api/settings', collect()); // collect() throws on an empty required field
      msg.textContent = r.notes && r.notes.length ? 'Saved with corrections: ' + r.notes.join('; ') : 'Saved and applied.';
      settingsDirty = false;
      await loadSettings();
      refresh();
    } catch (e) {
      msg.textContent = 'Not saved: ' + e.message;
    }
  });

  // <status page>#settings (the Windows tray's first-run link) opens the
  // settings: Chrome and Edge expand a <details> on fragment navigation,
  // Firefox does not.
  const openFromHash = () => {
    if (location.hash !== '#settings' && location.hash !== '#settings-box') return;
    const box = $('settings-box');
    if (!box.open) box.open = true; // the toggle handler loads the settings
    box.scrollIntoView({ block: 'start' });
  };
  window.addEventListener('hashchange', openFromHash);
  openFromHash();

  refresh();
  setInterval(refresh, 2000);
  refreshUpdate();
  setInterval(refreshUpdate, 60000);
})();
