// settings/sections-update.js - Settings > Updates (internal/selfupdate, WP-W3-wire): the running version, the
// last check and what is available, "Check now" and "Install now" (GET /api/update, POST /api/update/check,
// POST /api/update/install), and the update block of the configuration: automatic updates (update.enabled,
// opt-in), the release channel, the manifest URL and the release keys. Installs run only where the program
// may replace itself (a headless server with a writable program file); otherwise the page says what to run.

import { isValidUpdateKey } from './model.js';

export function renderUpdates(S, F) {
  const { ui, api, units } = S.ctx;
  const { h } = ui;
  const scope = S.scope;
  const host = h('div');
  let polling = null;

  const reload = S.panel(host, (signal) => api.get('/api/update', { signal }), (st) => statusView(st), 20000);

  function statusView(st) {
    if (!st || st.supported === false) {
      return [F.callout('info', 'info', 'This program has no self-update.')];
    }
    const last = st.last || {};
    const lastTx = last.phase ? `${last.from || '?'} → ${last.to || '?'}: ${last.phase}${last.result ? ' (' + last.result + ')' : ''}` : 'none yet';
    const avail = st.available ? ui.badge(`${st.available} available`, 'cyan') : st.lastCheck ? ui.badge('up to date', 'green') : ui.badge('not checked yet', 'other');
    const check = ui.button({ label: 'Check now', icon: 'refresh', size: 'sm', disabled: !st.configured || st.busy, onClick: async () => {
      check.setLoading(true);
      try {
        const r = await S.admin(() => api.post('/api/update/check', undefined, { signal: scope.signal, timeout: 130000 }));
        if (r?.actionError) ui.toast.error(r.actionError, { title: 'Update check' });
        else ui.toast.success(r?.available ? `Version ${r.available} is available.` : 'AirView is up to date.');
      } catch (e) { if (!e.aborted && !e.cancelled) ui.toast.error(e.message); }
      finally { check.setLoading(false); reload({ quiet: true }); }
    } });
    const install = ui.button({ label: st.available ? `Install ${st.available}` : 'Install now', icon: 'download', size: 'sm', variant: 'primary',
      disabled: !st.canInstall || !st.available || st.busy, tip: st.canInstall ? '' : st.installNote, onClick: async () => {
        if (S.isDirty()) { ui.toast.warn('Save or discard your settings first.'); return; }
        if (!(await ui.confirm({ title: `Install AirView ${st.available}?`, message: 'AirView downloads and verifies the signed release, replaces its program file and restarts. The web page is unavailable for a few seconds. If the new version does not come up healthy within the health window, the previous version is restored automatically.', confirmLabel: 'Install and restart' }))) return;
        install.setLoading(true);
        try {
          await S.admin(() => api.post('/api/update/install', undefined, { signal: scope.signal, timeout: 130000 }));
          ui.toast.info('Installing; AirView restarts when it is done.', { timeout: 15000 });
          followInstall(st.version);
        } catch (e) { if (!e.aborted && !e.cancelled) ui.toast.error(e.message); install.setLoading(false); }
      } });
    return [
      h('div.grid.cols-4.set-tiles',
        ui.stat({ label: 'Running version', icon: 'info', value: st.version || '—', sub: st.target || '' }),
        ui.stat({ label: 'Channel', icon: 'layers', value: st.channel || 'stable', sub: st.enabled ? 'automatic updates on' : 'automatic updates off' }),
        ui.stat({ label: 'Last check', icon: 'history', value: st.lastCheck ? units.ago(Date.parse(st.lastCheck)) : '—', sub: avail }),
        ui.stat({ label: 'Last update', icon: 'download', value: last.phase || '—', sub: last.phase ? `${last.from || '?'} → ${last.to || '?'}` : '' })),
      ui.kv([
        ['Release manifest', st.configured ? 'configured (signature checked with your release keys)' : 'not configured', { text: true }],
        ['Last error', st.lastError || st.actionError || '—', { text: true }],
        ['Last transaction', lastTx, { text: true }],
        st.busy ? ['Now', ui.badge('installing…', 'violet')] : null,
        st.result ? ['Last action', `${st.result.outcome}${st.result.to ? ' ' + st.result.to : ''}${st.result.detail ? ' · ' + st.result.detail : ''}`, { text: true }] : null,
      ]),
      h('div.row.gap-2', check, install),
      st.canInstall ? null : F.callout('info', 'info', st.installNote || 'This installation cannot install updates itself.'),
    ];
  }

  // After "Install": the program restarts. Wait for /healthz to answer with another version (or the same one
  // after a rollback), then reload the page.
  function followInstall(from) {
    if (polling) return;
    const t0 = Date.now();
    polling = setInterval(async () => {
      if (Date.now() - t0 > 10 * 60e3) { clearInterval(polling); polling = null; ui.toast.warn('The update takes longer than expected; check the log.'); return; }
      try {
        const r = await fetch('/healthz', { cache: 'no-store' });
        const j = await r.json();
        if (j && j.version && j.version !== from) {
          clearInterval(polling); polling = null;
          ui.toast.success(`AirView ${j.version} is running.`);
          setTimeout(() => location.reload(), 1500);
        } else {
          reload({ quiet: true });
        }
      } catch { /* restarting */ }
    }, 3000);
  }
  scope.onCleanup(() => { if (polling) clearInterval(polling); polling = null; });

  // SEC-R4: the manifest URL and the release keys are trust anchors. They are shown here but set only in the
  // operator configuration file or the AIRVIEW_UPDATE_* environment variables; the server refuses changes.
  const anchorHint = 'Set in the operator configuration file (update.manifestUrl / update.keys) or with AIRVIEW_UPDATE_MANIFEST_URL / AIRVIEW_UPDATE_KEYS. Not editable here, so a compromised web login cannot choose what gets installed.';
  const keys = h('textarea.textarea.num', { rows: 3, spellcheck: 'false', readOnly: true, 'aria-readonly': 'true' });
  keys.value = (S.get('update.keys') || []).join('\n') || '(none)';
  const keysField = ui.field({ label: 'Release keys', hint: 'Public keys that sign the release manifest. ' + anchorHint, control: keys });
  const manifest = h('input.input.num', { type: 'text', readOnly: true, 'aria-readonly': 'true', value: S.get('update.manifestUrl') || '(none)' });
  const manifestField = ui.field({ label: 'Manifest URL', hint: 'Where the signed release manifest is published ("{channel}" is replaced by the channel).', control: manifest });

  return [
    ui.card({ title: 'Version and updates', icon: 'download', body: host }),
    ui.card({ title: 'Update settings', icon: 'settings', body: [
      F.help('AirView installs only releases whose manifest is signed by one of your release keys, keeps the previous version, and restores it automatically when the new one does not come up healthy.'),
      h('div.set-fields.set-fields--2',
        F.toggle('update.enabled', { label: 'Install updates automatically', hint: 'Off by default. Checks every interval below (±25 %), installs a newer version of the channel and restarts. Needs a headless server whose program file this user may replace.' }),
        F.seg('update.channel', { label: 'Release channel', options: [{ value: 'stable', label: 'Stable' }, { value: 'beta', label: 'Beta' }], hint: 'Beta gets new versions earlier.' }),
        manifestField,
        F.number('update.checkIntervalMin', { label: 'Check interval', suffix: 'min', min: 15, max: 44640, step: 1, digits: 0, hint: '15 min to 31 days; default 1440 (daily).' }),
        F.number('update.healthWindowS', { label: 'Health window', suffix: 's', min: 10, max: 900, step: 1, digits: 0, hint: 'How long a new version must stay healthy before it is kept (10-900 s).' }),
        F.number('update.maxDownloadMB', { label: 'Largest download', suffix: 'MB', min: 1, max: 4096, step: 1, digits: 0 })),
      keysField,
      (S.get('update.keys') || []).some((k) => !isValidUpdateKey(k)) ? F.callout('warn', 'alert', 'A release key is not a base64 Ed25519 public key (44 characters ending in "=").') : null,
    ] }),
  ];
}
