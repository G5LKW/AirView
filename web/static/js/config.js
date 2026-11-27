// config.js - shared store for model.Config (GET/PUT /api/config). Pages receive it as ctx.config.
//
//   ctx.config.current                         -> the last loaded config object (treat as read-only)
//   ctx.config.get('ui.trailMinutes', 15)      -> dotted-path read with a fallback
//   await ctx.config.save((c) => { c.receiver.lat = 51.5; c.receiver.located = true; })
//   ctx.config.subscribe((cfg) => ...)         -> called after every load/save; returns unsubscribe
//
// save() always sends the whole config. Blank/"set" secret placeholders returned by the server
// (hub.password, remote.password, http.adminPasswordHash) must be passed back unchanged unless
// the user typed a new value.

import { api } from './api.js';

const subs = new Set();
let loading = null;

function emit() {
  for (const fn of subs) {
    try { fn(config.current); } catch (e) { console.error('[config] subscriber failed', e); }
  }
}

const clone = (o) => (typeof structuredClone === 'function' ? structuredClone(o) : JSON.parse(JSON.stringify(o)));

export const config = {
  /** Last loaded model.Config, or null before the first load. */
  current: null,

  /** Fetch the config from the server (concurrent calls share one request). */
  load(opts) {
    if (!loading) {
      loading = api.config(opts).then((cfg) => {
        config.current = cfg;
        emit();
        return cfg;
      }).finally(() => { loading = null; });
    }
    return loading;
  },

  /**
   * Save the configuration.
   * @param {object|function(object): (object|void)} next a full config object, or a mutator that receives a deep
   *        copy of the current config and edits it in place (or returns a replacement).
   * @returns {Promise<object>} the config as stored by the server
   */
  async save(next) {
    let body = next;
    if (typeof next === 'function') {
      if (!config.current) await config.load();
      const draft = clone(config.current);
      body = next(draft) || draft;
    }
    const stored = await api.saveConfig(body);
    config.current = stored;
    emit();
    return stored;
  },

  /** Read a value by dotted path, e.g. get('receiver.lat'). */
  get(path, fallback) {
    let o = config.current;
    for (const k of String(path).split('.')) {
      if (o === null || o === undefined) return fallback;
      o = o[k];
    }
    return o === undefined || o === null ? fallback : o;
  },

  /** Subscribe to config changes (fires after each load/save). Returns an unsubscribe function. */
  subscribe(fn, opts = {}) {
    subs.add(fn);
    if (opts.immediate && config.current) fn(config.current);
    return () => subs.delete(fn);
  },

  /** Forget the loaded config (network mode: after the administrator logged out). */
  clear() {
    if (config.current === null) return;
    config.current = null;
    emit();
  },

  /** A deep copy of the current config for editing in forms. */
  draft() { return config.current ? clone(config.current) : null; },
};

export default config;
