// net/view.js - small DOM helpers shared by the network pages (network, check, claim, me, account, admin,
// setup): page heads, "not available on this server yet" states, status dots, recovery-code and QR blocks.

import { ui, h } from '../ui.js';
import { units } from '../units.js';
import { qrSvg } from './qr.js';
import { groupSecret, parseOtpauth } from './model.js';

/** Page head: title, sub line and actions (like the standalone pages). */
export function pageHead(title, sub, ...actions) {
  const subEl = h('div.sub', sub || '');
  const el = h('div.page-head', h('div', h('h2', title), subEl), actions.length ? h('div.actions', actions) : null);
  el.setSub = (t) => ui.setText(subEl, t || '');
  return el;
}

/**
 * Error state for a failed network call. A 404 from a route that the server does not have yet says so
 * plainly instead of showing "HTTP 404".
 */
export function failState(err, opts = {}) {
  if (err && err.status === 404 && !(err.body && err.body.error && !/not found|no route|404/i.test(err.body.error))) {
    return ui.empty({ icon: 'info', title: opts.title404 || 'Not available on this server', text: opts.text404 || 'This AirView server does not offer this page (yet). Update the server, or ask its operator.' });
  }
  if (err && err.status === 401) return ui.empty({ icon: 'lock', title: 'Login required', text: 'Your session has ended. Log in again.' });
  if (err && err.status === 403) return ui.empty({ icon: 'shield', title: 'Not allowed', text: err.message || 'Your account may not see this.' });
  return ui.error(err, { title: opts.title || 'Could not be loaded', retry: opts.retry });
}

/**
 * Load into a container with a skeleton, an error state with retry, and a render callback.
 * Returns reload(). render(data) returns a Node (or nothing).
 */
export function loadInto(container, loader, render, opts = {}) {
  let seq = 0;
  async function reload(quiet) {
    const my = ++seq;
    if (!quiet || !container.firstChild) ui.mount(container, ui.skeleton({ lines: opts.lines || 4, title: true }));
    try {
      const data = await loader();
      if (my !== seq) return;
      const node = render(data);
      if (node) ui.mount(container, node);
    } catch (e) {
      if (my !== seq || e?.aborted) return;
      if (quiet && container.firstChild && !opts.replaceOnError) return; // keep the last good view on a background refresh
      ui.mount(container, failState(e, { ...opts, retry: () => reload(false) }));
    }
  }
  reload(false);
  return reload;
}

/** Online dot with a label. */
export function onlineBadge(online, lastSeen) {
  if (online) return ui.badge('online', 'green');
  return ui.badge(lastSeen ? `offline · ${units.ago(lastSeen)}` : 'offline', 'other');
}

const ADM_KIND = { approved: 'green', pending: 'amber', denied: 'red', revoked: 'red' };
export function admissionBadge(a) { return a ? ui.badge(a, ADM_KIND[a] || 'other') : null; }

/** A value or the dash. */
export const v = (x, fmt) => (x === null || x === undefined || (typeof x === 'number' && !Number.isFinite(x)) ? units.DASH : fmt ? fmt(x) : String(x));

/** Recovery codes block: grid, copy and download buttons, the "shown once" warning. */
export function recoveryBlock(codes, label = 'airview-recovery-codes.txt') {
  const text = (codes || []).join('\n');
  return h('div.net-recovery',
    h('div.net-recovery-warn', ui.icon('alert'), h('span', 'Save these recovery codes now. Each one logs you in once if you lose your authenticator. They are shown only this time.')),
    h('div.net-recovery-grid', (codes || []).map((c) => h('code', c))),
    h('div.row.gap-2',
      ui.button({ label: 'Copy', icon: 'copy', size: 'sm', onClick: async () => ((await ui.copy(text)) ? ui.toast.success('Codes copied') : ui.toast.warn('Copy failed - write them down')) }),
      ui.button({ label: 'Download', icon: 'download', size: 'sm', onClick: () => downloadText(label, text + '\n') })));
}

/** Authenticator enrolment block: QR of the otpauth URI, the secret for manual entry. */
export function totpBlock(setup) {
  const info = parseOtpauth(setup?.uri) || {};
  const qr = h('div.net-qr');
  try { qr.innerHTML = qrSvg(setup.uri, { ecl: 'M', scale: 4, margin: 4, title: 'Authenticator QR code' }); } catch { qr.append(h('p.muted', 'QR code unavailable - enter the key by hand.')); }
  return h('div.net-totp', qr,
    h('div.col.gap-2',
      h('p', 'Scan the code with an authenticator app (Aegis, Google Authenticator, 1Password, Microsoft Authenticator …) or enter the key by hand.'),
      ui.kv([['Account', info.label || '—'], ['Issuer', info.issuer || '—'], ['Key', h('code.net-secret', groupSecret(setup?.secret || info.secret)), { text: true }], ['Type', `time-based, ${info.digits || 6} digits, ${info.period || 30} s`]]),
      ui.button({ label: 'Copy key', icon: 'copy', size: 'sm', onClick: async () => ((await ui.copy(setup?.secret || info.secret || '')) ? ui.toast.success('Key copied') : null) })));
}

/** Save text as a file in the browser. */
export function downloadText(name, text, type = 'text/plain') {
  const url = URL.createObjectURL(new Blob([text], { type }));
  const a = h('a', { href: url, download: name });
  document.body.append(a);
  a.click();
  a.remove();
  setTimeout(() => URL.revokeObjectURL(url), 2000);
}

/** The consent text of DESIGN §10.5 (installers, claim page, privacy notice). */
export const CONSENT_TEXT = "If your receiver sends timestamps, the network operator can determine its location precisely, whatever you enter. Other users and the public cannot. Choose 'No timestamps (no MLAT)' if you do not want this.";

/** A short reminder of what the public sees (DESIGN §8). */
export const PRIVACY_NOTE = 'The public map never shows receiver data: no station positions (only an optional, deliberately inexact pin), no receivers per aircraft, no coverage or range. Owners see their own stations; the operator sees everything and every exact look is logged.';
