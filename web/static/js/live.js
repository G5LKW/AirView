// live.js - the one shared live-data store.
//
// A single EventSource on /api/events feeds every page:
//   event "aircraft" -> model.LiveSnapshot (1 Hz)      event "status" -> model.Status (every 5 s)
//   event "delta"    -> public delta stream (opened with ?delta=1): {seq, base, now, counts, upd, del};
//                       applied to the last keyframe ("aircraft" with seq) and emitted as an "aircraft" snapshot
//   event "notice"   -> { level, text, t }               event "alert"  -> alerts.Alert (stream only, not polled)
// live.followAlerts() combines the "alert" events with /api/alerts/recent catch-up fetches (start,
// reconnect, while polling) so a page sees every alert exactly once.
// If the stream cannot be opened or goes quiet, the store falls back to polling
// /api/aircraft (1 s) and /api/status (5 s) while it keeps retrying the stream with back-off.
//
//   const off = live.on('aircraft', (snap) => render(snap.aircraft));   // called at once with the last value
//   live.on('status', (st) => ...);  live.on('state', (s) => ...);  live.on('notice', (n) => ...);
//   live.snapshot / live.status / live.aircraft (Map icao -> Aircraft) / live.get('4CA1FA')
//   live.state: 'connecting' | 'live' | 'polling' | 'offline'
//   live.serverNow(): best estimate of the server clock in Unix ms
//
// Viewport streams (server: internal/server/sse_view.go): a page that shows only part of a large network (the
// map in aggregator mode) calls live.setViewport([south, west, north, east]); the stream is reopened with
// ?bbox= and then carries only the aircraft inside that box (`counts` stay global; aircraft without a position
// are not sent). Later boxes go to POST /api/events/view?id=<stream id> without a new keyframe; on 404 (the
// stream is gone) the store reconnects with &bbox=&replace=<id>. live.setViewport(null) returns to the full
// picture. Every reconnect names the previous stream in replace= so the one-public-stream-per-address limit
// does not refuse it.

import { api } from './api.js';

const EVENTS = ['aircraft', 'status', 'notice', 'alert', 'state'];
const subs = { aircraft: new Set(), status: new Set(), notice: new Set(), alert: new Set(), state: new Set() };

const STREAM_SILENCE_MS = 8000; // snapshots arrive at 1 Hz; this long without one = dead stream
const BACKOFF_MS = [1000, 2000, 4000, 8000, 15000];
const POLL_AIRCRAFT_MS = 1000;
const POLL_AIRCRAFT_HIDDEN_MS = 5000;
const POLL_STATUS_MS = 5000;

let es = null;
let started = false;
let attempts = 0;
let reconnectTimer = 0;
let watchdogTimer = 0;
let pollAircraftTimer = 0;
let pollStatusTimer = 0;
let polling = false;
let lastStreamMsg = 0;
let pollFailures = 0;
// Public delta stream state: the seq of the picture in deltaMap (null = no keyframe yet).
let deltaSeq = null;
let deltaMap = new Map();
// Viewport subscription: the box a page wants (null = full picture), the id of the current viewport stream
// ('' = none) and the box it applies.
let vpBox = null;
let streamId = '';
let posting = false;
let postQueued = false;

function emit(type, payload) {
  for (const fn of subs[type]) {
    try { fn(payload); } catch (e) { console.error(`[live] "${type}" subscriber failed`, e); }
  }
}

function setState(s) {
  if (live.state === s) return;
  live.state = s;
  emit('state', s);
}

function onSnapshot(snap) {
  if (!snap || !Array.isArray(snap.aircraft)) return;
  live.snapshot = snap;
  const m = new Map();
  for (const a of snap.aircraft) m.set(a.icao, a);
  live.aircraft = m;
  if (typeof snap.now === 'number') {
    const off = snap.now - Date.now();
    // exponential smoothing, but jump on large steps (clock change, first sample)
    live.serverOffset = live.serverOffset === null || Math.abs(off - live.serverOffset) > 2000 ? off : live.serverOffset * 0.8 + off * 0.2;
  }
  emit('aircraft', snap);
}

// A keyframe ("aircraft" event with seq) starts a delta chain.
function onKeyframe(snap) {
  if (snap && typeof snap.seq === 'number') {
    deltaSeq = snap.seq;
    deltaMap = new Map();
    for (const a of snap.aircraft || []) deltaMap.set(a.icao, { ...a });
  }
  onSnapshot(snap);
}

// Applies a delta to the keyframe picture; a gap in the chain reopens the stream (a new keyframe).
function onDelta(d) {
  if (!d || typeof d.seq !== 'number') return;
  if (deltaSeq === null || d.seq <= deltaSeq) return; // before the keyframe: already included
  if (d.base !== deltaSeq) { dropStream(); return; }
  for (const u of d.upd || []) {
    const cur = deltaMap.get(u.icao);
    const a = cur ? cur : {};
    for (const k of Object.keys(u)) {
      if (u[k] === null) delete a[k];
      else a[k] = u[k];
    }
    if (!cur) deltaMap.set(u.icao, a);
  }
  for (const icao of d.del || []) deltaMap.delete(icao);
  deltaSeq = d.seq;
  onSnapshot({ now: d.now, counts: d.counts, aircraft: Array.from(deltaMap.values(), (a) => ({ ...a })) });
}

function onStatus(st) {
  if (!st || typeof st !== 'object') return;
  live.status = st;
  emit('status', st);
}

function parse(e) {
  try { return JSON.parse(e.data); } catch { return null; }
}

function openStream() {
  clearTimeout(reconnectTimer);
  if (!started || es) return;
  if (typeof EventSource === 'undefined') { startPolling(); return; }
  let source;
  try {
    const params = { delta: 1 };
    if (vpBox) params.bbox = vpBox.join(',');
    if (streamId) params.replace = streamId;
    streamId = '';
    source = new EventSource(api.url('/api/events', params));
    deltaSeq = null;
  } catch {
    startPolling();
    scheduleReconnect();
    return;
  }
  es = source;
  source.onopen = () => {
    if (es !== source) return;
    attempts = 0;
    lastStreamMsg = Date.now();
    stopPolling();
    setState('live');
    armWatchdog();
  };
  source.addEventListener('aircraft', (e) => {
    if (es !== source) return;
    lastStreamMsg = Date.now();
    if (live.state !== 'live') { stopPolling(); setState('live'); }
    onKeyframe(parse(e));
  });
  source.addEventListener('delta', (e) => {
    if (es !== source) return;
    lastStreamMsg = Date.now();
    if (live.state !== 'live') { stopPolling(); setState('live'); }
    onDelta(parse(e));
  });
  source.addEventListener('stream', (e) => {
    if (es !== source) return;
    const d = parse(e);
    if (d && typeof d.id === 'string') { streamId = d.id; live.viewportActive = !!vpBox; }
    // the box changed while the stream was opening
    if (vpBox && d && Array.isArray(d.bbox) && d.bbox.join(',') !== vpBox.join(',')) postViewport();
  });
  source.addEventListener('status', (e) => { if (es === source) { lastStreamMsg = Date.now(); onStatus(parse(e)); } });
  source.addEventListener('notice', (e) => { if (es === source) { const n = parse(e); if (n) emit('notice', n); } });
  source.addEventListener('alert', (e) => { if (es === source) { const a = parse(e); if (a) emit('alert', a); } });
  source.onerror = () => { if (es === source) dropStream(); };
}

/** Close the stream and open it again at once (viewport mode changed). */
function reopen() {
  if (!started) return;
  if (es) { es.onopen = es.onerror = null; es.close(); es = null; }
  clearTimeout(watchdogTimer);
  attempts = 0;
  live.viewportActive = false;
  openStream();
}

/** Send the wanted box to the open viewport stream (one request at a time; the newest box wins). */
async function postViewport() {
  if (posting) { postQueued = true; return; }
  posting = true;
  try {
    while (vpBox && streamId && es) {
      postQueued = false;
      const box = vpBox;
      const id = streamId;
      try {
        await api.post('/api/events/view', undefined, { params: { id, bbox: box.join(',') }, timeout: 8000 });
      } catch (err) {
        if (err.status === 404 || err.status === 400) { reopen(); return; }
        return; // transient: the next pan retries
      }
      if (!postQueued) return;
    }
  } finally {
    posting = false;
  }
}

function dropStream() {
  if (es) { es.onopen = es.onerror = null; es.close(); es = null; }
  clearTimeout(watchdogTimer);
  if (!started) return;
  startPolling();
  scheduleReconnect();
}

function scheduleReconnect() {
  clearTimeout(reconnectTimer);
  const wait = BACKOFF_MS[Math.min(attempts, BACKOFF_MS.length - 1)];
  attempts++;
  reconnectTimer = setTimeout(openStream, wait);
}

function armWatchdog() {
  clearTimeout(watchdogTimer);
  watchdogTimer = setTimeout(() => {
    if (!es) return;
    // Browsers throttle timers in hidden tabs but not the stream itself; only judge when visible.
    if (document.visibilityState === 'visible' && Date.now() - lastStreamMsg > STREAM_SILENCE_MS) dropStream();
    else armWatchdog();
  }, STREAM_SILENCE_MS / 2);
}

function startPolling() {
  if (polling || !started) return;
  polling = true;
  pollFailures = 0;
  if (live.state === 'live') setState('connecting');
  pollAircraft();
  pollStatus();
}

function stopPolling() {
  polling = false;
  clearTimeout(pollAircraftTimer);
  clearTimeout(pollStatusTimer);
}

async function pollAircraft() {
  clearTimeout(pollAircraftTimer);
  if (!polling) return;
  try {
    const snap = await api.aircraft({ timeout: 6000 });
    if (!polling) return;
    pollFailures = 0;
    setState('polling');
    onSnapshot(snap);
  } catch {
    if (!polling) return;
    if (++pollFailures >= 2) setState('offline');
  }
  if (polling) pollAircraftTimer = setTimeout(pollAircraft, document.visibilityState === 'visible' ? POLL_AIRCRAFT_MS : POLL_AIRCRAFT_HIDDEN_MS);
}

async function pollStatus() {
  clearTimeout(pollStatusTimer);
  if (!polling) return;
  try {
    const st = await api.status({ timeout: 6000 });
    if (polling) onStatus(st);
  } catch { /* the aircraft poll decides on/offline */ }
  if (polling) pollStatusTimer = setTimeout(pollStatus, POLL_STATUS_MS);
}

export const live = {
  /** 'connecting' | 'live' (SSE) | 'polling' (HTTP fallback) | 'offline' */
  state: 'connecting',
  /** Last model.LiveSnapshot, or null before the first one. */
  snapshot: null,
  /** Last model.Status, or null. */
  status: null,
  /** Map of ICAO hex -> model.Aircraft from the last snapshot. */
  aircraft: new Map(),
  /** Smoothed (server clock - browser clock) in ms, null until known. */
  serverOffset: null,

  /** Start the store (idempotent). Called once by app.js. */
  start() {
    if (started) return;
    started = true;
    // Prime both values over plain HTTP so the UI fills before the first stream events.
    api.status({ timeout: 8000 }).then((st) => { if (!live.status) onStatus(st); }).catch(() => {});
    api.aircraft({ timeout: 8000 }).then((s) => { if (!live.snapshot) onSnapshot(s); }).catch(() => {});
    openStream();
    document.addEventListener('visibilitychange', () => {
      if (document.visibilityState === 'visible' && started && !es) { attempts = 0; openStream(); }
    });
    window.addEventListener('online', () => { if (started && !es) { attempts = 0; openStream(); } });
  },

  /**
   * Re-open the stream and re-fetch status and aircraft at once, e.g. after a login, a logout or a change of
   * the selected station in network mode (the server projects both per viewer and station). The last values
   * stay until the new ones arrive.
   */
  restart() {
    if (!started) return;
    if (es) { es.onopen = es.onerror = null; es.close(); es = null; }
    clearTimeout(watchdogTimer);
    attempts = 0;
    api.status({ timeout: 8000 }).then(onStatus).catch(() => {});
    api.aircraft({ timeout: 8000 }).then(onSnapshot).catch(() => {});
    openStream();
  },

  /** True while the stream is a viewport stream (only aircraft inside the box arrive). */
  viewportActive: false,

  /**
   * Narrow the stream to a map viewport: box = [south, west, north, east] (degrees, west > east crosses the
   * antimeridian), or null for the full picture. Callers debounce (pans); an unchanged box is ignored.
   */
  setViewport(box) {
    const next = Array.isArray(box) && box.length === 4 && box.every((v) => Number.isFinite(v)) ? box.slice() : null;
    const same = next && vpBox ? next.join(',') === vpBox.join(',') : next === vpBox;
    if (same) return;
    const was = vpBox;
    vpBox = next;
    if (!started) return;
    if (!next || !was || !streamId || !es) { reopen(); return; }
    postViewport();
  },

  /** The box of the current viewport subscription, or null. */
  get viewport() { return vpBox ? vpBox.slice() : null; },

  /** Stop streaming and polling (used by tests / the remote shell). */
  stop() {
    started = false;
    clearTimeout(reconnectTimer);
    clearTimeout(watchdogTimer);
    stopPolling();
    if (es) { es.close(); es = null; }
    setState('offline');
  },

  /**
   * Subscribe to 'aircraft' | 'status' | 'notice' | 'alert' | 'state'.
   * By default the callback fires immediately with the current value when there is one
   * (pass { immediate: false } to wait for the next event). Returns an unsubscribe function.
   */
  on(type, fn, opts = {}) {
    if (!EVENTS.includes(type)) throw new Error(`live.on: unknown event "${type}"`);
    subs[type].add(fn);
    if (opts.immediate !== false) {
      const cur = type === 'aircraft' ? live.snapshot : type === 'status' ? live.status : type === 'state' ? live.state : null;
      if (cur !== null) { try { fn(cur); } catch (e) { console.error(`[live] "${type}" subscriber failed`, e); } }
    }
    return () => subs[type].delete(fn);
  },

  /** Current aircraft by ICAO hex (case-insensitive), or undefined. */
  get(icao) { return live.aircraft.get(String(icao || '').toUpperCase()); },

  /** Best estimate of the server's clock (Unix ms). */
  serverNow() { return Date.now() + (live.serverOffset || 0); },

  /** Fetch /api/status now (e.g. right after saving settings) and publish it. */
  async refreshStatus() {
    const st = await api.status();
    onStatus(st);
    return st;
  },

  /**
   * Every alert exactly once, pushed live from the SSE `event: alert`, with a catch-up fetch of
   * /api/alerts/recent?after= on start, on every stream (re)connect and while the stream is down
   * (polling fallback). Detects a server restart (alert ids start over) and calls onReset.
   *
   *   const f = live.followAlerts({ signal: ctx.signal, initial: 300,
   *     onAlerts(list, { initial, live }) {},   // oldest first, each id once
   *     onReset() {}, onError(err) {}, onOk() {} });
   *   await f.ready;  f.stop();
   *
   * initial: how many recent alerts the first fetch delivers (0 = none: only remember the
   * high-water mark so nothing that fired before the page opened is replayed).
   */
  followAlerts(opts = {}) { return followAlerts(opts); },
};

const ALERT_POLL_MS = 4000;      // only while the stream is down
const ALERT_CHECK_MS = 60000;    // safety catch-up / restart check while the stream is up

function followAlerts({ signal, initial = 0, onAlerts, onReset, onError, onOk } = {}) {
  let lastId = null;             // null until the first fetch returned
  let stopped = false;
  let busy = false;
  let again = null;
  let timer = 0;
  let lastCheck = Date.now();
  let prevState = live.state;
  const seen = new Set();
  const pending = [];
  let resolveReady;
  const ready = new Promise((r) => { resolveReady = r; });

  function remember(a) {
    seen.add(a.id);
    if (a.id > lastId) lastId = a.id;
  }
  function deliver(list, info) {
    const fresh = [];
    for (const a of list || []) {
      if (!a || typeof a.id !== 'number' || seen.has(a.id)) continue;
      remember(a);
      fresh.push(a);
    }
    if (seen.size > 3000) { for (const id of seen) if (id < lastId - 1500) seen.delete(id); }
    if (!fresh.length) return;
    fresh.sort((x, y) => x.id - y.id);
    try { onAlerts?.(fresh, info); } catch (e) { console.error('[live] followAlerts subscriber failed', e); }
  }
  const recent = (params) => api.get('/api/alerts/recent', { params, signal, timeout: 10000 });

  async function sync(reason) {
    if (stopped) return;
    if (busy) { again = again || reason; return; }
    busy = true;
    try {
      if (lastId === null) {
        const r = await recent({ limit: Math.max(1, initial) });
        if (stopped) return;
        const arr = Array.isArray(r) ? r : [];
        lastId = 0;
        if (initial > 0) deliver(arr, { initial: true, live: false });
        else for (const a of arr) if (a && typeof a.id === 'number') remember(a);
        deliver(pending.splice(0), { initial: false, live: true });
      } else {
        const r = await recent({ after: lastId, limit: 500 });
        if (stopped) return;
        deliver(Array.isArray(r) ? r : [], { initial: false, live: false });
        if (reason === 'reconnect' || reason === 'check') {
          lastCheck = Date.now();
          // ids restart with the server: a newest id below ours means a restart
          const top = await recent({ limit: 1 });
          if (stopped) return;
          if (lastId > 0 && (!Array.isArray(top) || !top.length || top[0].id < lastId)) {
            lastId = null;
            seen.clear();
            try { onReset?.(); } catch (e) { console.error(e); }
            again = 'restart';
            return;
          }
        }
      }
      try { onOk?.(); } catch (e) { console.error(e); }
    } catch (e) {
      if (e.aborted || stopped) return;
      if (e.status === 404) { stop(); return; }       // alerts engine missing in this build
      try { onError?.(e); } catch (x) { console.error(x); }
    } finally {
      busy = false;
      resolveReady();
      if (again && !stopped) { const r = again; again = null; sync(r); }
    }
  }

  const offAlert = live.on('alert', (a) => {
    if (stopped || !a || typeof a.id !== 'number') return;
    if (lastId === null) pending.push(a);
    else deliver([a], { initial: false, live: true });
  }, { immediate: false });
  const offState = live.on('state', (s) => {
    if (s === 'live' && prevState !== 'live') sync('reconnect');
    prevState = s;
  }, { immediate: false });

  function tick() {
    if (stopped) return;
    if (document.visibilityState === 'visible') {
      if (live.state !== 'live') sync('poll');
      else if (Date.now() - lastCheck > ALERT_CHECK_MS) sync('check');
    }
    timer = setTimeout(tick, ALERT_POLL_MS);
  }

  function stop() {
    if (stopped) return;
    stopped = true;
    clearTimeout(timer);
    offAlert();
    offState();
    resolveReady();
  }
  if (signal) {
    if (signal.aborted) stop();
    else signal.addEventListener('abort', stop, { once: true });
  }
  if (!stopped) { sync('initial'); timer = setTimeout(tick, ALERT_POLL_MS); }

  return { ready, stop, sync: () => sync('check'), get lastId() { return lastId; } };
}

export default live;
