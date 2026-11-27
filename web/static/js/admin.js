// admin.js - run an admin-only API call; when the server answers 401 ask for the admin password
// (POST /api/login sets the session cookie) and retry the call once. Every page that writes
// (Settings, Alerts, Aircraft, Timing ...) goes through this instead of handling 401 itself.
//
//   import { createAdmin } from '../admin.js';
//   const admin = createAdmin(ctx, { onLogin(session) {}, onLocked() {} });
//   await admin.run(() => api.post('/api/alerts/import', body), 'Importing rules needs the admin password.');
//   if (await admin.prompt('Log in to change the rules.')) { ... }
//
// run() rejects with the original error for anything but 401, and with an Error carrying
// `.cancelled = true` when the user dismisses the login dialog. Only one login dialog is open at a time:
// concurrent 401s share it. While a run() is in flight adminCallActive() is true, so the shell does not
// show its generic "needs the admin password" toast for a 401 that the dialog already handles.

import { api as defaultApi } from './api.js';
import { ui as defaultUi } from './ui.js';
import { live as defaultLive } from './live.js';
import { net } from './net/store.js';
import { loginDialog } from './net/auth.js';

let pending = null;      // the open login dialog (shared by every createAdmin instance)
let active = 0;          // run() calls in flight

/** True while an admin.run() call is in flight (the shell suppresses its generic 401 toast). */
export function adminCallActive() { return active > 0; }

/**
 * @param {object} [ctx] page context (api, ui, live); the shared modules are used when omitted
 * @param {{ onLogin?(session: object): void, onLocked?(): void }} [hooks]
 *   onLocked: a call was refused with 401 (before the dialog opens); onLogin: the login succeeded
 * @returns {{ run(fn: () => Promise<any>, what?: string): Promise<any>, prompt(what?: string): Promise<boolean> }}
 */
export function createAdmin(ctx = {}, hooks = {}) {
  const api = ctx.api || defaultApi;
  const ui = ctx.ui || defaultUi;
  const live = ctx.live || defaultLive;
  const { h } = ui;

  function openDialog(what) {
    return new Promise((resolve) => {
      let dialog = null;
      let session = null;
      const pw = ui.input({
        type: 'password', placeholder: 'Admin password', autocomplete: 'current-password',
        onEnter: () => { if (dialog) dialog.actionButtons[1].click(); },
      });
      const field = ui.field({ label: 'Admin password', control: pw, hint: 'The password set under Settings > Web server. Requests from the AirView computer itself never need it.' });
      dialog = ui.modal({
        title: 'Admin login required',
        icon: 'lock',
        width: 420,
        body: h('div.col.gap-3', h('p', what || 'This action changes the station and needs the admin password.'), field),
        actions: [
          { label: 'Cancel', variant: 'ghost', value: false },
          {
            label: 'Log in', variant: 'primary', value: true,
            onClick: async () => {
              field.setError(null);
              try {
                const s = await api.post('/api/login', { password: pw.value });
                if (s && s.admin) { session = s; return true; }
                field.setError('The server did not grant admin access.');
              } catch (e) {
                field.setError(e && e.status === 401 ? 'Wrong password.'
                  : e && e.status === 429 ? 'Too many failed attempts - wait a moment and try again.'
                    : (e && e.message) || 'Login failed');
              }
              try { pw.input?.select(); } catch { /* ignore */ }
              return false;
            },
          },
        ],
      });
      dialog.closed.then((v) => resolve(v === true ? session || {} : null));
    });
  }

  /** Show the login dialog. Resolves true when this client is admin afterwards. */
  async function prompt(what) {
    if (!pending) {
      pending = (net.aggregator ? loginDialog({ what: what || 'This action needs the network administrator.', needAdmin: true }) : openDialog(what))
        .finally(() => { pending = null; });
    }
    const session = await pending;
    if (!session) return false;
    live.refreshStatus?.().catch(() => {});
    try { hooks.onLogin?.(session); } catch (e) { console.error('[admin] onLogin failed', e); }
    return true;
  }

  /** Run fn(); on 401 offer the login and retry once. */
  async function run(fn, what) {
    active++;
    try {
      return await fn();
    } catch (e) {
      if (!e || e.status !== 401) throw e;
      try { hooks.onLocked?.(); } catch (x) { console.error('[admin] onLocked failed', x); }
      const ok = await prompt(what);
      if (!ok) throw Object.assign(new Error('Admin login cancelled'), { cancelled: true });
      return await fn();
    } finally {
      active--;
    }
  }

  return { run, prompt };
}

export default createAdmin;
