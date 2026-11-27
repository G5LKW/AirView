// pages/account.js - Account & security (#/me/account, network / aggregator mode): password, authenticator
// (TOTP) enrolment with a QR code, recovery codes (shown once), login sessions, data export and account
// deletion. The administrator's authenticator is mandatory and cannot be turned off here.
//
// API: /api/auth/password, /api/auth/totp/begin|confirm|disable, /api/auth/recovery-codes,
// /api/auth/sessions, /api/auth/logout-others (internal/server/auth_routes.go), GET /api/me/export,
// DELETE /api/me/account (net/endpoints.js).

import { EP } from '../net/endpoints.js';
import { normalizeCode } from '../net/model.js';
import { pageHead, loadInto, recoveryBlock, totpBlock } from '../net/view.js';
import { consentBlock, joinInfo } from '../net/consent.js';

export default {
  id: 'account',
  title: 'Account',

  async mount(container, ctx) {
    const { api, ui, units, net } = ctx;
    const { h } = ui;
    const s = net.session || {};
    const isAdmin = net.scope === 'admin';
    const page = h('div.page-inner.net-narrow',
      pageHead('Account', `${net.username} · ${isAdmin ? 'network administrator' : 'station owner'}`,
        ui.button({ label: 'Log out', icon: 'logout', onClick: async () => { const { logout } = await import('../net/auth.js'); logout(); } })));
    container.append(page);

    // ---- password ----
    const cur = ui.input({ type: 'password', autocomplete: 'current-password' });
    const nw = ui.input({ type: 'password', autocomplete: 'new-password' });
    const nw2 = ui.input({ type: 'password', autocomplete: 'new-password' });
    const fCur = ui.field({ label: 'Current password', control: cur });
    const fNew = ui.field({ label: 'New password', control: nw, hint: isAdmin ? 'At least 12 characters.' : 'At least 8 characters.' });
    const fNew2 = ui.field({ label: 'Repeat new password', control: nw2 });
    const pwBtn = ui.button({ label: 'Change password', icon: 'save', variant: 'primary', onClick: async () => {
      for (const f of [fCur, fNew, fNew2]) f.setError(null);
      if (nw.value.length < (isAdmin ? 12 : 8)) { fNew.setError(`Use at least ${isAdmin ? 12 : 8} characters.`); return; }
      if (nw.value !== nw2.value) { fNew2.setError('The passwords differ.'); return; }
      pwBtn.setLoading(true);
      try {
        await api.post(EP.password(), { current: cur.value, new: nw.value });
        cur.value = nw.value = nw2.value = '';
        ui.toast.success('Password changed. Your other sessions were logged out.');
        reloadSessions();
      } catch (e) { (e.status === 401 ? fCur : fNew).setError(e.status === 401 ? 'Wrong password.' : e.message); } finally { pwBtn.setLoading(false); }
    } });
    page.append(ui.card({ title: 'Password', icon: 'lock', body: h('div.col.gap-3', fCur, fNew, fNew2, h('div.row', pwBtn)) }));

    // ---- authenticator ----
    const totpBody = h('div.col.gap-3');
    page.append(ui.card({ title: 'Authenticator (two-step login)', icon: 'key', body: totpBody }));
    let totpOn = !!s.totp;
    const askPassword = (title, withCode) => new Promise((resolve) => {
      const pw = ui.input({ type: 'password', autocomplete: 'current-password' });
      const code = ui.input({ mono: true, autocomplete: 'one-time-code', placeholder: '123456 or a recovery code' });
      const fp = ui.field({ label: 'Password', control: pw });
      const fc = withCode ? ui.field({ label: 'Authenticator or recovery code', control: code }) : null;
      let out = null;
      const dlg = ui.modal({ title, icon: 'lock', width: 400, body: h('div.col.gap-3', fp, fc), actions: [
        { label: 'Cancel', variant: 'ghost' },
        { label: 'Continue', variant: 'primary', onClick: () => { if (!pw.value) { fp.setError('Enter your password.'); return false; } out = { password: pw.value, code: normalizeCode(code.value) }; return true; } },
      ] });
      dlg.closed.then(() => resolve(out));
    });

    const drawTotp = () => {
      ui.mount(totpBody,
        h('p', totpOn
          ? 'On. Every login asks for a code from your authenticator app (or one of your recovery codes).'
          : 'Off. Turn it on so that a stolen password alone cannot open your account. There is no e-mail reset: without the authenticator and the recovery codes the account cannot be recovered.'),
        h('div.row.gap-2', { style: 'flex-wrap:wrap' },
          totpOn
            ? [
              ui.button({ label: 'New recovery codes', icon: 'refresh', onClick: newCodes }),
              isAdmin ? null : ui.button({ label: 'Turn off', icon: 'x', variant: 'danger', onClick: disable }),
            ]
            : ui.button({ label: 'Set up authenticator', icon: 'key', variant: 'primary', onClick: enrol })),
        isAdmin ? h('p.muted', 'The administrator account always needs the authenticator. Lost it? Run "airview admin reset" on the server.') : null);
    };
    async function enrol() {
      const a = await askPassword('Set up authenticator', false);
      if (!a) return;
      let setup;
      try { setup = await api.post(EP.totpBegin(), { password: a.password }); } catch (e) { ui.toast.error(e.status === 401 ? 'Wrong password.' : e.message); return; }
      const code = ui.input({ mono: true, autocomplete: 'one-time-code', placeholder: '123456', maxlength: 8 });
      const fc = ui.field({ label: 'Code shown by the app', control: code });
      let codes = null;
      const dlg = ui.modal({ title: 'Scan with your authenticator', icon: 'key', width: 560, body: h('div.col.gap-3', totpBlock(setup), fc), actions: [
        { label: 'Cancel', variant: 'ghost' },
        { label: 'Confirm', variant: 'primary', onClick: async () => {
          fc.setError(null);
          try { codes = (await api.post(EP.totpConfirm(), { code: normalizeCode(code.value) }))?.recoveryCodes || []; return true; } catch (e) { fc.setError(e.status === 401 ? 'Wrong code - check the time on your phone and try the next code.' : e.message); return false; }
        } },
      ] });
      await dlg.closed;
      if (!codes) return;
      totpOn = true;
      drawTotp();
      showCodes(codes, 'Authenticator is on');
      net.refresh();
    }
    async function disable() {
      const a = await askPassword('Turn off the authenticator', true);
      if (!a) return;
      try { await api.post(EP.totpDisable(), { password: a.password, code: a.code }); totpOn = false; drawTotp(); ui.toast.success('Authenticator turned off'); net.refresh(); } catch (e) { ui.toast.error(e.status === 401 ? 'Wrong password or code.' : e.message); }
    }
    async function newCodes() {
      const a = await askPassword('New recovery codes', true);
      if (!a) return;
      try { const r = await api.post(EP.recoveryCodes(), { password: a.password, code: a.code }); showCodes(r?.recoveryCodes || [], 'New recovery codes'); } catch (e) { ui.toast.error(e.status === 401 ? 'Wrong password or code.' : e.message); }
    }
    function showCodes(codes, title) {
      ui.modal({ title, icon: 'key', width: 520, dismissable: false, body: recoveryBlock(codes), actions: [{ label: 'I have saved them', variant: 'primary' }] });
    }
    drawTotp();

    // ---- sessions ----
    const sessBody = h('div');
    const othersBtn = ui.button({ label: 'Log out other sessions', icon: 'logout', size: 'sm', onClick: async () => {
      try { const r = await api.post(EP.logoutOthers(), {}); ui.toast.success(`${r?.ended ?? 0} other ${r?.ended === 1 ? 'session' : 'sessions'} ended`); reloadSessions(); } catch (e) { ui.toast.error(e.message); }
    } });
    page.append(ui.card({ title: 'Login sessions', icon: 'users', actions: [othersBtn], sub: 'Addresses are not stored.', body: sessBody }));
    const reloadSessions = loadInto(sessBody, () => api.get(EP.sessions(), { signal: ctx.signal }), (r) => ui.table({
      columns: [
        { key: 'id', label: 'Session', render: (x) => h('span.row.gap-2', h('code', x.id), x.current ? ui.badge('this browser', 'blue') : null) },
        { key: 'scope', label: 'Role' },
        { key: 'created', label: 'Started', render: (x) => units.dateTime(x.created) },
        { key: 'lastSeen', label: 'Last used', render: (x) => units.ago(x.lastSeen) },
        { key: 'expires', label: 'Expires', render: (x) => units.dateTime(x.expires) },
      ],
      rows: r?.sessions || [], empty: 'No sessions',
    }), { lines: 2 });

    // ---- data ----
    let backupDays = 30;
    const c8 = h('div', consentBlock('C8', { compact: true }));
    joinInfo(api).then((j) => { if (Number.isFinite(j.backupDays)) { backupDays = j.backupDays; ui.mount(c8, consentBlock('C8', { compact: true, backupDays })); } });
    const delBtn = ui.button({ label: 'Delete my account', icon: 'trash', variant: 'danger', onClick: async () => {
      const ok = await ui.confirm({ title: 'Delete the account?', danger: true, requireText: 'DELETE', confirmLabel: 'Delete everything',
        message: `Your account, your stations' statistics, positions and settings are deleted at once (backups expire within ${backupDays} days). Feeders keep sending until you stop them; they then appear as unclaimed stations.` });
      if (!ok) return;
      try {
        await api.request('DELETE', EP.myAccount(), { body: { confirm: 'DELETE' } });
        ui.toast.success('Your account was deleted');
        await net.setSession({ ok: true, aggregator: true, scope: 'public' });
        ctx.navigate('map');
      } catch (e) { ui.toast.error(e.message); }
    } });
    page.append(ui.card({ title: 'Your data', icon: 'database', body: h('div.col.gap-3',
      c8,
      h('p', 'Export everything stored about you and your stations as JSON in a zip file, or delete the account with all its data.'),
      h('div.row.gap-2', { style: 'flex-wrap:wrap' }, ui.button({ label: 'Export my data', icon: 'download', onClick: () => { location.href = EP.myExport(); } }), isAdmin ? null : delBtn)) }));
  },
};
