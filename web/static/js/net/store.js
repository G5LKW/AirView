// net/store.js - network (aggregator) state shared by the shell and the pages: mode, session scope, the
// stations of the station picker, ctx.station and the map view.
//
//   import { net } from './net/store.js';
//   await net.init();                 // once, by app.js: GET /api/session decides the mode
//   net.mode                          // 'standalone' | 'aggregator'
//   net.scope                         // 'local' (standalone) | 'public' | 'owner' | 'admin'
//   net.station                       // ctx.station: { sid, name, scope, located, lat?, lon?, antennas, local }
//   net.stations                      // picker entries (owner: own stations; admin: every station)
//   net.view                          // map view: 'network' | 'mine' | 'station' | 'admin'
//   net.setStation(sid) / net.setView(id) / net.setSession(sessionInfo) / await net.refresh()
//   net.on((reason) => {})            // reason: 'session' | 'station' | 'stations' | 'view'; returns off()
//
// Standalone mode: net.station follows config.receiver / config.antennas (sid '' = no station= parameter),
// api.guard and api.station stay empty, nothing else changes.

import { api } from '../api.js';
import { config } from '../config.js';
import { EP, normStation } from './endpoints.js';
import * as M from './model.js';

const STATION_KEY = 'airview.net.station';
const VIEW_KEY = 'airview.net.view';
const subs = new Set();

function store(key, v) { try { if (v === null || v === undefined || v === '') localStorage.removeItem(key); else localStorage.setItem(key, v); } catch { /* private mode */ } }
function stored(key) { try { return localStorage.getItem(key) || ''; } catch { return ''; } }

function emit(reason) {
  for (const fn of subs) { try { fn(reason); } catch (e) { console.error('[net] subscriber failed', e); } }
}

/** The api.js guard: a public viewer in network mode only reaches the public routes. */
function guard(path) {
  if (net.mode !== M.MODE_AGGREGATOR || net.scope !== 'public') return null;
  return M.publicAllowed(path) ? null : 'Not available in the public view - log in first.';
}

let initDone = null;

export const net = {
  mode: M.MODE_STANDALONE,
  /** Last sessionInfo from the server (null before init). */
  session: null,
  scope: 'local',
  username: '',
  /** No admin with an authenticator exists yet (aggregator first-run setup pending). */
  setupRequired: false,
  stations: [],
  stationsError: null,
  station: null,
  view: 'network',

  get aggregator() { return this.mode === M.MODE_AGGREGATOR; },
  get loggedIn() { return this.aggregator && this.scope !== 'public'; },
  get isAdmin() { return this.aggregator ? this.scope === 'admin' : true; },
  /** Views this viewer may pick on the map. */
  get views() { return this.aggregator ? M.viewsFor(this.scope) : []; },

  /** Decide the mode and load the session (idempotent; never rejects). */
  init() {
    if (!initDone) {
      initDone = (async () => {
        let session = null;
        try { session = await api.get('/api/session', { timeout: 6000 }); } catch { /* old server or offline: standalone */ }
        this.mode = M.detectMode(session, null);
        if (this.mode === M.MODE_AGGREGATOR) {
          api.guard = guard;
          await this.applySession(session, { quiet: true });
        } else {
          this.scope = 'local';
          this.station = M.localStation(config.current);
          // standalone: ctx.station simply follows the configuration (no event: nothing reloads)
          config.subscribe((cfg) => { if (this.mode === M.MODE_STANDALONE) this.station = M.localStation(cfg); });
        }
      })();
    }
    return initDone;
  },

  /** Take a sessionInfo reply (login, logout, register, GET /api/session) and reload what depends on it. */
  async setSession(session) { await this.applySession(session, {}); },

  async applySession(session, { quiet }) {
    const before = `${this.scope}|${this.username}`;
    this.session = session || null;
    this.scope = M.scopeOf(session);
    this.username = (session && session.username) || '';
    this.setupRequired = !!(session && session.setupRequired);
    const changed = before !== `${this.scope}|${this.username}`;
    if (changed || quiet) await this.loadStations();
    this.view = M.normalizeView(stored(VIEW_KEY) || (this.scope === 'public' ? 'network' : 'mine'), this.scope, !!this.station);
    if (!quiet) emit('session');
  },

  /** Re-read the session from the server (e.g. after an account page changed it). */
  async refresh() {
    if (!this.aggregator) return;
    try { await this.setSession(await api.get('/api/session', { timeout: 6000 })); } catch { /* keep the old state */ }
  },

  /** Load the picker's stations for the current scope and re-select the remembered one. */
  async loadStations() {
    this.stationsError = null;
    let list = [];
    if (this.scope === 'owner' || this.scope === 'admin') {
      try {
        const own = await api.get(EP.myStations(), { timeout: 10000 });
        list = (own?.stations || []).map(normStation).filter(Boolean).map((s) => ({ ...s, own: true }));
      } catch (e) { this.stationsError = e; }
      if (this.scope === 'admin') {
        try {
          const all = await api.get(EP.adminStations(), { params: { limit: 500 }, timeout: 15000 });
          const seen = new Set(list.map((s) => s.sid));
          for (const s of (all?.stations || []).map(normStation)) if (s && !seen.has(s.sid)) list.push({ ...s, own: false });
        } catch (e) { if (!this.stationsError) this.stationsError = e; }
      }
    }
    this.stations = list.filter((s) => s && s.sid);
    const want = stored(STATION_KEY);
    const pick = this.stations.find((s) => s.sid === want) || this.stations.find((s) => s.own) || null;
    this.selectStation(pick, false);
    emit('stations');
  },

  selectStation(dto, notify = true) {
    this.station = dto ? M.networkStation(dto, this.scope) : null;
    api.station = this.station ? this.station.sid : '';
    if (this.view === 'station' && !this.station) this.view = M.normalizeView(this.view, this.scope, false);
    if (notify) emit('station');
  },

  /** Choose the station of the station-scoped pages and of the map's Station view ('' = none). */
  setStation(sid) {
    if (!this.aggregator) return;
    const dto = this.stations.find((s) => s.sid === sid) || null;
    if ((dto?.sid || '') === (this.station?.sid || '')) return;
    store(STATION_KEY, dto ? dto.sid : '');
    this.selectStation(dto, true);
  },

  /** Merge fresh fields of one station (after an edit on its page) into the picker list. */
  updateStation(dto) {
    dto = normStation(dto);
    if (!dto) return;
    const i = this.stations.findIndex((s) => s.sid === dto.sid);
    if (i >= 0) this.stations[i] = { ...this.stations[i], ...dto };
    else this.stations.push({ ...dto, own: true });
    if (this.station && this.station.sid === dto.sid) this.selectStation(this.stations.find((s) => s.sid === dto.sid), false);
    emit('stations');
  },

  /** Choose the map view (normalised to what the viewer may use). */
  setView(view) {
    const v = M.normalizeView(view, this.scope, !!this.station);
    store(VIEW_KEY, v);
    if (v === this.view) return;
    this.view = v;
    emit('view');
  },

  /** Subscribe to changes; returns an unsubscribe function. */
  on(fn) { subs.add(fn); return () => subs.delete(fn); },
};

export default net;
