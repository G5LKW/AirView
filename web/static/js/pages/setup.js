// pages/setup.js - first-run setup of the network administrator (#/setup, aggregator mode, DESIGN §7):
//   1. the one-time token from the setup-token file in the data directory, a user name and a password (12+ characters);
//   2. scan the authenticator QR code and confirm with a code;
//   3. save the recovery codes (shown once), then log in.
// The same can be done on the server with `airview admin init`.
//
// API: GET /api/auth/setup, POST /api/auth/setup/begin, POST /api/auth/setup/finish.

import { EP } from '../net/endpoints.js';
import { normalizeCode, validUsername } from '../net/model.js';
import { pageHead, failState, recoveryBlock, totpBlock } from '../net/view.js';
import { loginDialog } from '../net/auth.js';

export default {
  id: 'setup',
  title: 'First-run setup',

  async mount(container, ctx) {
    const { api, ui, net } = ctx;
    const { h } = ui;
    const body = h('div');
    container.append(h('div.page-inner.net-narrow', pageHead('First-run setup', 'Create the network administrator'), body));

    let state;
    try { state = await api.get(EP.setupState(), { signal: ctx.signal }); } catch (e) { if (!e.aborted) ui.mount(body, failState(e)); return; }
    if (!state?.required) {
      ui.mount(body, ui.empty({ icon: 'check-circle', title: 'Setup is complete', text: 'The network has an administrator. Log in with that account.',
        action: net.loggedIn ? null : ui.button({ label: 'Log in', icon: 'lock', variant: 'primary', onClick: () => loginDialog({ needAdmin: true }) }) }));
      return;
    }

    // step 1
    const token = ui.input({ mono: true, autocomplete: 'off', spellcheck: 'false', placeholder: 'from the setup-token file' });
    const user = ui.input({ autocomplete: 'username', value: 'admin' });
    const pw = ui.input({ type: 'password', autocomplete: 'new-password' });
    const pw2 = ui.input({ type: 'password', autocomplete: 'new-password' });
    const win = state.os === 'windows';
    const where = state.tokenFile ? `the file ${state.tokenFile}` : 'the file "setup-token" in the AirView data directory';
    const readCmd = win ? (state.tokenFile ? `type "${state.tokenFile}"` : 'type setup-token (in an administrator command prompt)') : (state.tokenFile ? `sudo cat ${state.tokenFile}` : 'sudo cat <data directory>/setup-token');
    const fTok = ui.field({ label: 'Setup token', control: token, hint: state.tokenPending
      ? `The one-time token is in ${where}, readable only by ${win ? 'administrators' : 'root'} and the service account (${readCmd}). It is printed on the console only when AirView runs in a terminal, and never written to the log files.`
      : `No token is pending: restart AirView to write a new one, or run "airview admin init" on the server${win ? ' (as administrator)' : ''}.` });
    const fUser = ui.field({ label: 'Administrator user name', control: user });
    const fPw = ui.field({ label: 'Password', control: pw, hint: 'At least 12 characters.' });
    const fPw2 = ui.field({ label: 'Repeat password', control: pw2 });
    const next = ui.button({ label: 'Continue', icon: 'chevron-right', variant: 'primary', onClick: begin });
    ui.mount(body, ui.card({ title: '1 · Administrator account', icon: 'user', body: h('div.col.gap-3', fTok, fUser, fPw, fPw2, h('div.row', next)) }));

    async function begin() {
      for (const f of [fTok, fUser, fPw, fPw2]) f.setError(null);
      const name = user.value.trim().toLowerCase();
      if (!token.value.trim()) { fTok.setError('Enter the token from the console.'); return; }
      if (!validUsername(name)) { fUser.setError('Use 3-32 of a-z, 0-9, dot, dash or underscore.'); return; }
      if (pw.value.length < 12) { fPw.setError('Use at least 12 characters.'); return; }
      if (pw.value !== pw2.value) { fPw2.setError('The passwords differ.'); return; }
      next.setLoading(true);
      try {
        const setup = await api.post(EP.setupBegin(), { token: token.value.trim(), username: name, password: pw.value });
        step2(setup, name);
      } catch (e) {
        (e.status === 401 || e.status === 403 ? fTok : fPw).setError(e.message || 'Setup failed');
      } finally { next.setLoading(false); }
    }

    function step2(setup, name) {
      const code = ui.input({ mono: true, autocomplete: 'one-time-code', placeholder: '123456', maxlength: 8 });
      const fCode = ui.field({ label: 'Code shown by the app', control: code });
      const done = ui.button({ label: 'Confirm', icon: 'check', variant: 'primary', onClick: async () => {
        fCode.setError(null);
        done.setLoading(true);
        try {
          const r = await api.post(EP.setupFinish(), { token: token.value.trim(), username: name, code: normalizeCode(code.value) });
          step3(r?.recoveryCodes || [], name);
        } catch (e) { fCode.setError(e.status === 401 ? 'Wrong code - check the time on your phone and try the next code.' : e.message); } finally { done.setLoading(false); }
      } });
      ui.mount(body, ui.card({ title: '2 · Authenticator', icon: 'key', body: h('div.col.gap-3', totpBlock(setup), fCode, h('div.row', done)) }));
    }

    function step3(codes, name) {
      ui.mount(body, ui.card({ title: '3 · Recovery codes', icon: 'key', body: h('div.col.gap-3',
        recoveryBlock(codes, 'airview-admin-recovery-codes.txt'),
        h('div.row', ui.button({ label: 'Log in now', icon: 'lock', variant: 'primary', onClick: async () => {
          await net.refresh();
          const s = await loginDialog({ what: `Log in as ${name} with the password and a code from the authenticator.`, needAdmin: true });
          if (s) ctx.navigate('admin');
        } }))) }));
    }
  },
};
