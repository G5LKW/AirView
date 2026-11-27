// net/auth.js - account dialogs of network (aggregator) mode: log in (user name, password, then the
// authenticator or a recovery code when the server asks for it), create an account, log out.
//
//   import { loginDialog, registerDialog, logout } from '../net/auth.js';
//   const session = await loginDialog({ what: 'Log in to see your stations.' });  // null = cancelled
//   if (await registerDialog()) ...
//   await logout();
//
// Every success hands the sessionInfo to net.setSession(), which reloads the stations and tells the shell to
// redraw. POST /api/login (aggregator): { username, password, code? }; 401 with codeRequired: true asks for the
// code. The CSRF token of the reply is kept by api.js. A login that used a recovery code warns how many are
// left (sessionInfo.recoveryLeft).

import { api } from '../api.js';
import { ui } from '../ui.js';
import { net } from './store.js';
import { EP } from './endpoints.js';
import { normalizeCode, validUsername } from './model.js';
import { consentBlock, joinInfo, privacyHref } from './consent.js';

const { h } = ui;
let pending = null;

function errText(e, fallback) {
  if (!e) return fallback;
  if (e.status === 429) return e.message && !/^HTTP/.test(e.message) ? e.message : 'Too many attempts - wait a moment and try again.';
  return e.message || fallback;
}

/** Warn after a login that used a recovery code. */
export function recoveryWarning(session) {
  if (!session || typeof session.recoveryLeft !== 'number') return;
  const n = session.recoveryLeft;
  ui.toast(n > 0
    ? `You logged in with a recovery code. ${n} ${n === 1 ? 'code is' : 'codes are'} left - create new ones under Account.`
    : 'You used your last recovery code. Create new recovery codes under Account now.', { type: n > 2 ? 'warn' : 'error', title: 'Recovery code used', timeout: 15000 });
}

/**
 * The login dialog. Resolves with the new sessionInfo, or null when cancelled.
 * @param {{what?: string, needAdmin?: boolean}} [opts]
 */
export function loginDialog(opts = {}) {
  if (pending) return pending;
  pending = new Promise((resolve) => {
    let session = null;
    let codeStep = false;
    let switching = false;
    const user = ui.input({ placeholder: 'User name', autocomplete: 'username', value: net.username || '' });
    const pw = ui.input({ type: 'password', placeholder: 'Password', autocomplete: 'current-password' });
    const code = ui.input({ placeholder: '123456 or a recovery code', autocomplete: 'one-time-code', inputmode: 'text', mono: true });
    const fUser = ui.field({ label: 'User name', control: user });
    const fPw = ui.field({ label: 'Password', control: pw });
    const fCode = ui.field({ label: 'Authenticator code', control: code, hint: 'The 6-digit code from your authenticator app, or one of your recovery codes.' });
    fCode.classList.add('hidden');
    const note = h('p.muted', opts.what || 'Log in with your AirView Network account.');
    const links = h('div.net-login-links',
      h('a', { href: '#/me/claim', onclick: () => dialog.close(null) }, 'New feeder? Claim it after creating an account'),
      net.setupRequired ? h('a', { href: '#/setup', onclick: () => dialog.close(null) }, 'First-run setup of the administrator') : null);
    const submit = () => dialog.actionButtons[2].click();
    for (const c of [user, pw, code]) c.input.addEventListener('keydown', (e) => { if (e.key === 'Enter') { e.preventDefault(); submit(); } });
    const dialog = ui.modal({
      title: 'Log in', icon: 'lock', width: 420,
      body: h('form.col.gap-3', { onsubmit: (e) => { e.preventDefault(); submit(); } }, note, fUser, fPw, fCode, links),
      actions: [
        { label: 'Create account', variant: 'ghost', onClick: (close) => { switching = true; close(null); registerDialog().then(resolve); return false; } },
        { label: 'Cancel', variant: 'ghost', value: null },
        {
          label: 'Log in', variant: 'primary',
          onClick: async () => {
            for (const f of [fUser, fPw, fCode]) f.setError(null);
            const body = { username: user.value.trim(), password: pw.value };
            if (!body.username) { fUser.setError('Enter your user name.'); return false; }
            if (!body.password) { fPw.setError('Enter your password.'); return false; }
            if (codeStep) {
              body.code = normalizeCode(code.value);
              if (!body.code) { fCode.setError('Enter the code.'); return false; }
            }
            try {
              const s = await api.post('/api/login', body);
              if (opts.needAdmin && !(s && s.admin)) {
                fUser.setError('This account is not the network administrator.');
                await net.setSession(s);
                return false;
              }
              session = s;
              return true;
            } catch (e) {
              if (e.status === 401 && e.body && e.body.codeRequired) {
                codeStep = true;
                fCode.classList.remove('hidden');
                if (body.code) fCode.setError('Wrong or expired code.');
                setTimeout(() => code.input.focus(), 0);
                return false;
              }
              const msg = errText(e, 'Login failed');
              if (codeStep && e.status === 401) fCode.setError(/code/i.test(msg) ? msg : 'Wrong user name, password or code.');
              else fPw.setError(e.status === 401 ? 'Wrong user name or password.' : msg);
              return false;
            }
          },
        },
      ],
    });
    dialog.closed.then(async () => {
      if (switching) return;
      if (session) {
        await net.setSession(session);
        recoveryWarning(session);
        ui.toast.success(`Logged in as ${session.username || 'user'}`);
      }
      resolve(session);
    });
  }).finally(() => { pending = null; });
  return pending;
}

/** Create an owner account (POST /api/auth/register logs in at once). Resolves with the sessionInfo or null. */
export function registerDialog() {
  return new Promise((resolve) => {
    let session = null;
    const user = ui.input({ placeholder: 'e.g. alice-radio', autocomplete: 'username' });
    const pw = ui.input({ type: 'password', placeholder: 'At least 8 characters', autocomplete: 'new-password' });
    const pw2 = ui.input({ type: 'password', placeholder: 'Repeat the password', autocomplete: 'new-password' });
    const fUser = ui.field({ label: 'User name', control: user, hint: '3-32 characters: a-z, 0-9, dot, dash, underscore. No e-mail address is needed or stored.' });
    const fPw = ui.field({ label: 'Password', control: pw });
    const fPw2 = ui.field({ label: 'Repeat password', control: pw2 });
    // C7 (notice, not consent) under the form; the privacy-notice link comes from aggregator.privacyNoticeUrl
    const c7 = h('div', consentBlock('C7', { compact: true }));
    joinInfo(api).then((j) => { const href = privacyHref(j); if (href) ui.mount(c7, consentBlock('C7', { compact: true, privacyHref: href })); });
    const dialog = ui.modal({
      title: 'Create an account', icon: 'user', width: 440,
      body: h('div.col.gap-3',
        h('p.muted', 'An account lets you claim your feeders, see their statistics and choose what the public map shows about them. There is no password reset by e-mail: turn on an authenticator and keep the recovery codes.'),
        fUser, fPw, fPw2, c7),
      actions: [
        { label: 'Cancel', variant: 'ghost', value: null },
        {
          label: 'Create account', variant: 'primary',
          onClick: async () => {
            for (const f of [fUser, fPw, fPw2]) f.setError(null);
            const name = user.value.trim().toLowerCase();
            if (!validUsername(name)) { fUser.setError('Use 3-32 of a-z, 0-9, dot, dash or underscore.'); return false; }
            if (pw.value.length < 8) { fPw.setError('Use at least 8 characters.'); return false; }
            if (pw.value !== pw2.value) { fPw2.setError('The passwords differ.'); return false; }
            try {
              session = await api.post(EP.register(), { username: name, password: pw.value });
              return true;
            } catch (e) {
              if (e.status === 409) fUser.setError('This user name is taken.');
              else if (e.status === 403 || e.status === 404) fUser.setError(e.message || 'New accounts are not accepted on this network.');
              else fPw.setError(errText(e, 'The account could not be created.'));
              return false;
            }
          },
        },
      ],
    });
    dialog.closed.then(async () => {
      if (session) {
        await net.setSession(session);
        ui.toast.success(`Welcome, ${session.username}. Claim your feeder next.`, { action: { label: 'Claim', onClick: () => { location.hash = '#/me/claim'; } } });
      }
      resolve(session);
    });
  });
}

/** Log out (POST /api/logout) and drop back to the public view. */
export async function logout() {
  try {
    const s = await api.post('/api/logout', {});
    await net.setSession(s || { ok: true, aggregator: true, scope: 'public' });
  } catch {
    await net.setSession({ ok: true, aggregator: true, scope: 'public' });
  }
  ui.toast('Logged out');
}

/** Make sure someone is logged in; opens the login dialog when not. Resolves true when logged in. */
export async function requireLogin(what) {
  if (net.loggedIn) return true;
  return !!(await loginDialog({ what }));
}
