// pages/claim.js - claim a feeder (#/me/claim, DESIGN §7):
//   native AirView feeder: the 8-character code from `airview-feeder claim`, its status page or the installer
//                          (40 bits, valid 24 h, single use);
//   readsb / mlat-client:  the full UUID, and the browser must come from the station's current address.
// Needs an account: a public visitor gets the login / create-account buttons first. Rate limits (5 per 10
// minutes per address, 20 per day per account) come back as 429 with Retry-After.
//
// API: POST /api/me/claim { code } | { uuid } -> { sid, name }.

import { EP } from '../net/endpoints.js';
import { normalizeClaimCode, validClaimCode, normalizeUUID, validUUID } from '../net/model.js';
import { loginDialog, registerDialog } from '../net/auth.js';
import { pageHead } from '../net/view.js';
import { consentBlock } from '../net/consent.js';

export default {
  id: 'claim',
  title: 'Claim a feeder',

  async mount(container, ctx) {
    const { api, ui, net } = ctx;
    const { h } = ui;
    const q = ctx.route.query || {};
    const compat = q.kind === 'uuid' || ((q.proto === 'beast' || q.proto === 'mlatc') && q.kind !== 'code');

    const inner = h('div.page-inner.net-narrow', pageHead('Claim a feeder', 'Link a receiver to your account'));
    container.append(inner);

    if (!net.loggedIn) {
      inner.append(ui.card({ title: 'Log in first', icon: 'lock', body: h('div.col.gap-3',
        h('p', 'A claimed station belongs to your account: you see its statistics, timing and exact location, and you decide what the public map shows about it.'),
        h('div.row.gap-2', { style: 'flex-wrap:wrap' },
          ui.button({ label: 'Log in', icon: 'lock', variant: 'primary', onClick: () => loginDialog({ what: 'Log in to claim your feeder.' }) }),
          ui.button({ label: 'Create an account', icon: 'user', onClick: () => registerDialog() }))) }));
      return;
    }

    const consent = ui.checkbox({ label: 'I have read this' });
    // C1 (timestamps reveal the location) with the acknowledgement, and C6 (automatic use of a surveyed position)
    const consentBox = h('div.col.gap-2', consentBlock('C1', { extra: consent }), consentBlock('C6', { compact: true }));

    // ---- native code ----
    const code = ui.input({ placeholder: 'ABCD2345', mono: true, maxlength: 12, autocomplete: 'off', autocapitalize: 'characters', spellcheck: 'false' });
    const fCode = ui.field({ label: 'Claim code', control: code, hint: 'Run "airview-feeder claim" on the feeder, open its status page, or take the code shown by the installer. Codes are valid for 24 hours and work once.' });
    const codeBtn = ui.button({ label: 'Claim', icon: 'claim', variant: 'primary', onClick: () => submit('code') });
    code.input.addEventListener('keydown', (e) => { if (e.key === 'Enter') submit('code'); });

    // ---- compat UUID ----
    const uuid = ui.input({ placeholder: '0123abcd-…', mono: true, autocomplete: 'off', spellcheck: 'false' });
    const fUuid = ui.field({ label: 'Receiver UUID', control: uuid, hint: 'The full UUID your readsb / mlat-client sends (e.g. /boot/adsb-config.txt or /etc/default/readsb). Open this page from the same internet connection as the feeder: the claim only works from the station\'s current address.' });
    const uuidBtn = ui.button({ label: 'Claim', icon: 'claim', variant: 'primary', onClick: () => submit('uuid') });
    uuid.input.addEventListener('keydown', (e) => { if (e.key === 'Enter') submit('uuid'); });

    const tabs = ui.tabs({
      active: compat ? 'uuid' : 'code',
      tabs: [
        { id: 'code', label: 'AirView feeder', icon: 'antenna', content: h('div.col.gap-3', fCode, h('div.row', codeBtn)) },
        { id: 'uuid', label: 'readsb / mlat-client', icon: 'target', content: h('div.col.gap-3', consentBlock('C5', { compact: true }), fUuid, h('div.row', uuidBtn), h('p.muted', 'Feeders without a UUID cannot be claimed this way; the network administrator can assign them to your account.')) },
      ],
    });
    inner.append(ui.card({ body: h('div.col.gap-4', consentBox, tabs) }));

    async function submit(kind) {
      fCode.setError(null);
      fUuid.setError(null);
      const field = kind === 'code' ? fCode : fUuid;
      const btn = kind === 'code' ? codeBtn : uuidBtn;
      if (!consent.checked) { field.setError('Please read the note on timestamps above and tick the box.'); return; }
      let body;
      if (kind === 'code') {
        const c = normalizeClaimCode(code.value);
        if (!validClaimCode(c)) { fCode.setError('A claim code has 8 characters: letters A-Z and digits 2-7.'); return; }
        body = { code: c };
      } else {
        if (!validUUID(uuid.value)) { fUuid.setError('Enter the whole UUID (32 hexadecimal digits, dashes optional).'); return; }
        body = { uuid: normalizeUUID(uuid.value) };
      }
      btn.setLoading(true);
      try {
        const r = await api.post(EP.claim(), body, { signal: ctx.signal });
        ui.toast.success(`${r?.name || 'The station'} is now yours.`, { title: 'Claimed' });
        await net.loadStations();
        if (r?.sid) { net.setStation(r.sid); ctx.navigate('me/stations/' + encodeURIComponent(r.sid)); }
        else ctx.navigate('me/stations');
      } catch (e) {
        if (e.aborted) return;
        if (e.status === 429) field.setError(e.message && !/^HTTP/.test(e.message) ? e.message : 'Too many claim attempts. Wait a few minutes and try again.');
        else if (e.status === 404 && !e.body?.error) field.setError('Claiming is not available on this server yet.');
        else field.setError(e.message || 'The claim failed.');
      } finally { btn.setLoading(false); }
    }
  },
};
