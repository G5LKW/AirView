// api.js - fetch wrapper for the AirView HTTP API.
//
//   import { api, ApiError } from './api.js';
//   const st = await api.get('/api/status');
//   const res = await api.get('/api/stats/range', { params: { hours: 24 }, signal: ctx.signal });
//   await api.put('/api/config', cfg);
//
// Every call resolves with the parsed JSON body (or null for an empty body) and rejects with an
// ApiError carrying the HTTP status (0 = network failure / timeout / aborted) and the server's
// {"error":"message"} text.
//
// Network (aggregator) mode: `api.station` (set by net/store.js from ctx.station) adds `station=<sid>` to
// station-scoped API paths, and `api.guard` refuses, in the browser, requests a public viewer may not make
// (ApiError status 403, `.blocked = true`, nothing goes on the wire). Both are empty in standalone mode, so
// every request is exactly what it was before.

import { stationParam } from './net/model.js';

export class ApiError extends Error {
  /**
   * @param {string} message
   * @param {{status?: number, body?: any, url?: string, aborted?: boolean, timeout?: boolean}} [info]
   */
  constructor(message, info = {}) {
    super(message);
    this.name = 'ApiError';
    this.status = info.status || 0;
    this.body = info.body;
    this.url = info.url || '';
    this.aborted = !!info.aborted;
    this.timeout = !!info.timeout;
  }
  /** True when the request never reached the server (offline, DNS, refused, timeout). */
  get network() { return this.status === 0 && !this.aborted; }
}

const listeners = { unauthorized: new Set(), error: new Set() };

function emit(type, payload) {
  for (const fn of listeners[type]) {
    try { fn(payload); } catch (e) { console.error('[api] listener failed', e); }
  }
}

function buildURL(path, params) {
  let url = api.base + path;
  const sid = params && params.station !== undefined ? '' : stationParam(path, api.station);
  if (sid) params = { ...(params || {}), station: sid };
  if (params) {
    const q = new URLSearchParams();
    for (const [k, v] of Object.entries(params)) {
      if (v === undefined || v === null || v === '') continue;
      q.append(k, Array.isArray(v) ? v.join(',') : String(v));
    }
    const s = q.toString();
    if (s) url += (url.includes('?') ? '&' : '?') + s;
  }
  return url;
}

async function request(method, path, opts = {}) {
  const { params, body, signal, headers, timeout = api.timeout, raw = false } = opts;
  const refused = typeof api.guard === 'function' ? api.guard(path, method) : null;
  if (refused) {
    const err = new ApiError(refused, { status: 403, url: api.base + path });
    err.blocked = true;
    throw err;
  }
  if (method !== 'GET' && method !== 'HEAD' && !api.csrfToken && !SESSION_PATHS.has(path)) await loadSessionOnce();
  const url = buildURL(path, params);
  const ctl = new AbortController();
  let timedOut = false;
  const timer = timeout > 0 ? setTimeout(() => { timedOut = true; ctl.abort(); }, timeout) : 0;
  const onAbort = () => ctl.abort();
  if (signal) {
    if (signal.aborted) ctl.abort();
    else signal.addEventListener('abort', onAbort, { once: true });
  }
  const init = {
    method,
    signal: ctl.signal,
    headers: { Accept: 'application/json', ...api.headers, ...csrfHeader(method), ...headers },
    cache: 'no-store',
    credentials: 'same-origin',
  };
  if (body !== undefined) {
    if (body instanceof FormData || body instanceof Blob || typeof body === 'string') {
      init.body = body;
    } else {
      init.body = JSON.stringify(body);
      init.headers['Content-Type'] = 'application/json';
    }
  }
  let res;
  try {
    res = await fetch(url, init);
  } catch (e) {
    const aborted = !timedOut && (signal?.aborted || e?.name === 'AbortError');
    const err = new ApiError(timedOut ? `Request timed out after ${Math.round(timeout / 1000)} s` : aborted ? 'Request cancelled' : 'Cannot reach the AirView server', { url, aborted, timeout: timedOut });
    if (!aborted) emit('error', err);
    throw err;
  } finally {
    clearTimeout(timer);
    if (signal) signal.removeEventListener('abort', onAbort);
  }
  if (raw && res.ok) return res;
  let data = null;
  let text = '';
  try {
    text = await res.text();
    if (text) data = JSON.parse(text);
  } catch {
    data = null;
  }
  if (!res.ok) {
    const msg = (data && typeof data.error === 'string' && data.error) || (text && text.length < 200 && !text.startsWith('<') ? text.trim() : '') || `HTTP ${res.status} ${res.statusText}`.trim();
    const err = new ApiError(msg, { status: res.status, body: data, url });
    if (res.status === 401 || res.status === 403) emit('unauthorized', err);
    emit('error', err);
    throw err;
  }
  if (data === null && text) throw new ApiError('Server returned a non-JSON response', { status: res.status, url, body: text.slice(0, 200) });
  if (SESSION_PATHS.has(path) && data && typeof data === 'object') api.csrfToken = typeof data.csrfToken === 'string' ? data.csrfToken : '';
  return data;
}

// The session's CSRF token (sessionInfo.csrfToken) is sent as X-CSRF-Token on every non-GET
// request: required in aggregator mode, checked when present in standalone mode. It is taken
// from the replies of the login, logout and session endpoints (and the account endpoints that
// return sessionInfo).
const SESSION_PATHS = new Set(['/api/session', '/api/login', '/api/logout', '/api/auth/register']);

// A session cookie may outlive the page: learn its CSRF token once before the first write.
let sessionLoaded = null;
function loadSessionOnce() {
  if (!sessionLoaded) sessionLoaded = request('GET', '/api/session', { timeout: 5000 }).catch(() => null);
  return sessionLoaded;
}

function csrfHeader(method) {
  return method !== 'GET' && method !== 'HEAD' && api.csrfToken ? { 'X-CSRF-Token': api.csrfToken } : {};
}

export const api = {
  /** URL prefix for every request ('' = same origin). */
  base: '',
  /** Default timeout in ms (0 disables). */
  timeout: 15000,
  /** Extra headers sent with every request (e.g. an admin token set by the settings page). */
  headers: {},
  /** CSRF token of the current login session ('' = none); maintained automatically. */
  csrfToken: '',
  /** Network mode: SID added as station= to station-scoped paths ('' = none, always in standalone mode). */
  station: '',
  /** Network mode: fn(path, method) -> refusal message or null; requests it refuses never leave the browser. */
  guard: null,

  /** Full URL of a GET path with params and the station= parameter (for EventSource, downloads, links). */
  url: (path, params) => buildURL(path, params),

  request,
  /** GET path -> JSON. opts: { params, signal, timeout, headers } */
  get: (path, opts) => request('GET', path, opts),
  /** POST JSON body -> JSON. */
  post: (path, body, opts) => request('POST', path, { ...opts, body }),
  /** PUT JSON body -> JSON. */
  put: (path, body, opts) => request('PUT', path, { ...opts, body }),
  /** DELETE -> JSON. */
  del: (path, opts) => request('DELETE', path, opts),

  /** Subscribe to 'unauthorized' (401/403) or 'error' (any failed request). Returns an unsubscribe fn. */
  on(type, fn) {
    listeners[type].add(fn);
    return () => listeners[type].delete(fn);
  },

  // ---- core v1 endpoints (docs/ARCHITECTURE.md) ----
  /** @returns {Promise<object>} model.Status */
  status: (opts) => request('GET', '/api/status', opts),
  /** @returns {Promise<object>} model.Config (secrets blanked) */
  config: (opts) => request('GET', '/api/config', opts),
  /** Save the whole config; resolves with the stored config. */
  saveConfig: (cfg, opts) => request('PUT', '/api/config', { ...opts, body: cfg }),
  /** @returns {Promise<object>} model.LiveSnapshot */
  aircraft: (opts) => request('GET', '/api/aircraft', opts),
  /** @returns {Promise<{aircraft: object|null, trail: Array, info: object, flights: Array}>} */
  aircraftDetail: (icao, opts) => request('GET', '/api/aircraft/' + encodeURIComponent(String(icao).toUpperCase()), opts),
  /** @returns {Promise<{now: number, trails: Object<string, Array>}>} live in-memory trails */
  trails: (minutes, opts) => request('GET', '/api/trails', { ...opts, params: { minutes } }),
  /** Wipes the database and configuration. */
  factoryReset: (opts) => request('POST', '/api/factory-reset', { ...opts, body: { confirm: 'RESET' } }),
};

export default api;
