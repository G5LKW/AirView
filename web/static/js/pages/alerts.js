// pages/alerts.js - the Alerts page.
//
// Layout: engine tiles (rules, fired / suppressed, evaluation, highlights, webhooks) above two columns:
//   left   rules (enable toggles, plain-language summary, actions, last fired; the editor opens in a drawer with
//          a condition builder, geofence map, actions, cooldowns and a dry run) and the watch-lists card
//   right  the recent-alerts feed (live SSE alerts + /api/alerts/recent catch-up via live.followAlerts, links to #/map/<ICAO>, optional chime)
// Every admin call (POST / PUT / DELETE) offers the admin login on 401 and retries once (js/admin.js).

import { createAdmin } from '../admin.js';
import { SEVERITIES, severityBadge, describe } from '../alerts/model.js';
import { openEditor } from '../alerts/editor.js';
import { createWatchlists } from '../alerts/watchlists.js';
import { play as playSound, unlock as unlockSound, isMuted, setMuted, onMuteChange } from '../alertsound.js';

const CSS = new URL('../../css/alerts.css', import.meta.url);
const FEED_MAX = 300;
const STATS_POLL_MS = 10000;
const SEV_RANK = { info: 0, warning: 1, critical: 2 };

export default {
  id: 'alerts',
  title: 'Alerts',

  async mount(container, ctx) {
    const { api, ui, units, config, live } = ctx;
    const { h } = ui;
    await ui.loadCss(CSS);
    container.classList.add('alp-page');

    const admin = createAdmin(ctx);
    const prefs = ui.prefs('airview.alerts', { sound: true, feedKind: 'all', feedSev: '' });
    // The chime is played app-wide by the shell (js/alertsound.js); this switch is its per-browser mute.
    // Migrate the old page-only switch once.
    if (prefs.get().sound === false) { setMuted(true); prefs.set({ sound: true }); }
    const st = {
      rules: null, rulesErr: null, stats: null, statsErr: null,
      feed: [], feedErr: null, feedLoaded: false, lastId: 0, lastFired: new Map(),
      watchNames: [], placeholders: null, editor: null,
    };

    // ---- layout -------------------------------------------------------------------------------
    const lockBadge = h('div.alp-lock');
    const soundToggle = ui.toggle({ label: 'Sound', checked: !isMuted(), tip: 'Play a chime in this browser when an alert with sound fires, on every page (Settings > Alerts switches sounds off for all browsers)',
      onChange: (v) => { setMuted(!v); if (v) { unlockSound(); playSound('info'); } } });
    ctx.onCleanup(onMuteChange((m) => { if (soundToggle.checked !== !m) soundToggle.checked = !m; }));
    const newBtn = ui.button({ label: 'New rule', icon: 'plus', variant: 'primary', onClick: () => edit(null) });
    const banner = h('div.alp-banners');

    const tiles = {
      rules: ui.stat({ label: 'Active rules', icon: 'bell', accent: 'cyan' }),
      fired: ui.stat({ label: 'Alerts fired', icon: 'bolt', accent: 'amber', tip: 'Since AirView started' }),
      hl: ui.stat({ label: 'Highlighted now', icon: 'eye', accent: 'violet', tip: 'Aircraft in the live picture that a rule highlights on the map' }),
      eval: ui.stat({ label: 'Evaluation', icon: 'timer', tip: 'Rules are evaluated once a second against the live picture' }),
      hook: ui.stat({ label: 'Webhooks', icon: 'link', accent: 'teal' }),
    };
    for (const t of Object.values(tiles)) t.set(units.DASH);

    const rulesBody = h('div.alp-rules');
    const moreBtn = ui.iconButton('more', 'Import, export, reset', () => ui.menu(moreBtn, [
      { label: 'Import rules (JSON)…', icon: 'upload', onClick: importRules },
      { label: 'Export all rules', icon: 'download', onClick: () => { location.href = '/api/alerts/export'; } },
      'sep',
      { label: 'Reset built-in rules…', icon: 'history', onClick: resetBuiltins },
    ]), { variant: 'ghost', size: 'sm' });
    const rulesCard = ui.card({ title: 'Rules', icon: 'filter', sub: 'Evaluated once a second against every aircraft and system event', actions: [ui.button({ label: 'New', icon: 'plus', size: 'sm', onClick: () => edit(null) }), moreBtn], body: rulesBody });

    const watch = createWatchlists(ctx, { admin, onLists: (names) => { st.watchNames = names; } });

    const feedBody = h('div.alp-feed');
    const feedKind = ui.segmented({ options: [{ value: 'all', label: 'All' }, { value: 'aircraft', label: 'Aircraft' }, { value: 'system', label: 'System' }], value: prefs.get().feedKind,
      onChange: (v) => { prefs.set({ feedKind: v }); renderFeed(); } });
    const feedSev = ui.select({ options: [{ value: '', label: 'Any severity' }, ...SEVERITIES.map((s) => ({ value: s.value, label: s.label + ' +' }))], value: prefs.get().feedSev, size: 'sm',
      onChange: (v) => { prefs.set({ feedSev: v }); renderFeed(); } });
    const feedCard = ui.card({ title: 'Recent alerts', icon: 'history', sub: 'Newest first · kept in memory (last 500)', body: h('div.col', h('div.alp-feed-tools', feedKind, feedSev), feedBody) });
    feedCard.classList.add('alp-feed-card');

    container.append(h('div.page-inner.alp',
      h('div.page-head', h('div', h('h2', 'Alerts'), h('div.sub', 'Rules, watch-lists and the alerts they fire')), h('div.actions', lockBadge, soundToggle, newBtn)),
      banner,
      h('div.alp-tiles', Object.values(tiles)),
      h('div.alp-cols', h('div.alp-main', rulesCard, watch.el), h('div.alp-side', feedCard))));

    // ---- banners / lock ------------------------------------------------------------------------
    function renderBanners() {
      const cfg = config.current;
      const out = [];
      if (cfg?.alerts && cfg.alerts.enabled === false) {
        out.push(h('div.alx-callout.alx-callout--warn', ui.icon('alert'), h('div.grow', h('b', 'The alert engine is switched off. '), 'Rules are not evaluated until it is enabled under Settings.'),
          ui.button({ label: 'Settings', size: 'sm', href: '#/settings' })));
      }
      if (cfg?.alerts && cfg.alerts.sound === false) {
        out.push(h('div.alx-callout', ui.icon('info'), h('div.grow', 'Alert sounds are switched off for this station in Settings; rules with "Play a sound" stay silent.')));
      }
      ui.mount(banner, out);
      const locked = !!(live.status || ctx.status)?.adminLocked;
      ui.mount(lockBadge, locked ? [ui.badge('Read-only', 'amber', { large: true, tip: 'Changes need the admin password' }),
        ui.button({ label: 'Log in', icon: 'lock', size: 'sm', onClick: async () => { if (await admin.prompt('Log in to change alert rules and watch-lists.')) { await live.refreshStatus().catch(() => {}); renderBanners(); } } })] : null);
    }
    ctx.subscribe('status', renderBanners, { immediate: false });
    ctx.onCleanup(config.subscribe(renderBanners));
    renderBanners();

    // ---- rules ---------------------------------------------------------------------------------
    async function loadRules() {
      if (!st.rules) ui.mount(rulesBody, ui.skeleton({ lines: 6 }));
      try {
        st.rules = await api.get('/api/alerts/rules', { signal: ctx.signal });
        st.rulesErr = null;
      } catch (e) {
        if (e.aborted) return;
        st.rulesErr = e;
      }
      renderRules();
      renderTiles();
    }

    function renderRules() {
      if (st.rulesErr && !st.rules) { ui.mount(rulesBody, ui.error(st.rulesErr, { title: 'The rules could not be loaded', retry: loadRules })); return; }
      if (!st.rules) return;
      if (!st.rules.length) {
        ui.mount(rulesBody, ui.empty({ icon: 'bell', title: 'No rules yet', text: 'Create a rule, or restore the built-in ones (emergency, military, low overhead, records …).',
          action: h('div.row.gap-2', ui.button({ label: 'New rule', icon: 'plus', variant: 'primary', size: 'sm', onClick: () => edit(null) }), ui.button({ label: 'Restore built-ins', icon: 'history', size: 'sm', onClick: resetBuiltins })) }));
        return;
      }
      ui.mount(rulesBody, st.rules.map(ruleRow));
    }

    function ruleRow(r) {
      const tog = ui.toggle({ checked: r.enabled, tip: r.enabled ? 'Enabled - click to switch off' : 'Disabled - click to switch on', onChange: (v) => setEnabled(r, v, tog) });
      const a = r.actions || {};
      const acts = [
        a.notify ? h('span.alr-act', { tip: 'Shows a notice' }, ui.icon('bell')) : null,
        a.sound ? h('span.alr-act', { tip: 'Plays a sound' }, ui.icon('signal')) : null,
        a.log ? h('span.alr-act', { tip: 'Written to the event log' }, ui.icon('database')) : null,
        a.webhook ? h('span.alr-act', { tip: 'Webhook: ' + a.webhook }, ui.icon('link')) : null,
        a.highlight ? h('span.alr-swatch', { style: { '--c': a.highlight }, tip: 'Map highlight ' + a.highlight }) : null,
      ];
      const last = st.lastFired.get(r.id);
      return h('div.alr' + (r.enabled ? '' : '.is-off'),
        tog,
        h('button.alr-main', { type: 'button', onclick: () => edit(r), 'aria-label': 'Edit ' + r.name },
          h('div.alr-title', h('span.alr-name', r.name), ui.badge(cap(r.severity), severityBadge(r.severity)), r.builtin ? ui.badge('Built-in', 'other') : null, r.includePredicted ? ui.badge('Incl. predicted', 'cyan', { tip: 'Position conditions also match aircraft at a predicted (coasting) position' }) : null),
          h('div.alr-sum', describe(r.match, units)),
          h('div.alr-meta', h('span', acts), h('span.muted', `cooldown ${units.age(r.cooldownSec)}`), last ? h('span.alr-last', 'fired ' + units.ago(last, live.serverNow())) : null)),
        ui.iconButton('edit', 'Edit', () => edit(r), { variant: 'ghost', size: 'sm', class: 'alr-edit' }));
    }

    async function setEnabled(r, on, tog) {
      try {
        const stored = await admin.run(() => api.post(`/api/alerts/rules/${encodeURIComponent(r.id)}/enabled`, { enabled: on }), 'Switching rules needs the admin password.');
        Object.assign(r, stored || { enabled: on });
        renderRules();
        loadStats();
      } catch (e) {
        tog.checked = !on;
        if (!e.cancelled) ui.toast.error(e.message, { title: 'Not changed' });
      }
    }

    async function edit(rule) {
      if (st.editor) st.editor.close();
      if (!st.placeholders) {
        try {
          const ph = await api.get('/api/alerts/placeholders', { signal: ctx.signal, timeout: 5000 });
          st.placeholders = String(ph?.text || '').split(/\s+/).filter((x) => /^\{\w+\}$/.test(x));
        } catch { st.placeholders = []; }
      }
      if (ctx.signal.aborted) return;
      st.editor = openEditor(ctx, {
        rule, isNew: !rule, admin, watchlists: st.watchNames, placeholders: st.placeholders,
        onSaved: () => { loadRules(); loadStats(); },
        onDeleted: () => { loadRules(); loadStats(); },
      });
    }

    async function importRules() {
      const fileIn = h('input', { type: 'file', accept: '.json,application/json' });
      fileIn.addEventListener('change', async () => {
        const f = fileIn.files?.[0];
        if (!f) return;
        let arr;
        try {
          if (f.size > 4 * 1024 * 1024) throw new Error('The file is larger than 4 MB.');
          arr = JSON.parse(await f.text());
          if (!Array.isArray(arr)) arr = [arr];
          if (!arr.length || !arr.every((x) => x && typeof x === 'object' && x.name && x.match)) throw new Error('Expected a JSON array of rules (as written by "Export all rules").');
        } catch (e) { ui.toast.error(e.message, { title: 'Not imported' }); return; }
        const replace = arr.filter((x) => x.id && st.rules?.some((r) => r.id === x.id)).length;
        const ok = await ui.confirm({ title: 'Import rules?', message: `${arr.length} ${arr.length === 1 ? 'rule' : 'rules'} from ${f.name}.${replace ? ` ${replace} replace existing rules with the same id.` : ''} All are checked first; nothing is imported if one is invalid.`, confirmLabel: 'Import' });
        if (!ok) return;
        try {
          const res = await admin.run(() => api.post('/api/alerts/import', arr, { timeout: 30000 }), 'Importing rules needs the admin password.');
          ui.toast.success(`${units.int(res?.imported ?? arr.length)} rules imported.`);
          loadRules(); loadStats();
        } catch (e) { if (!e.cancelled) ui.toast.error(e.message, { title: 'Not imported' }); }
      });
      fileIn.click();
    }

    async function resetBuiltins() {
      const ok = await ui.confirm({ title: 'Reset built-in rules?', message: 'The built-in rules return to their shipped settings (including enabled / disabled). Your own rules are kept.', confirmLabel: 'Reset built-ins' });
      if (!ok) return;
      try {
        await admin.run(() => api.post('/api/alerts/rules/reset-defaults', {}), 'Resetting rules needs the admin password.');
        ui.toast.success('Built-in rules restored.');
        loadRules(); loadStats();
      } catch (e) { if (!e.cancelled) ui.toast.error(e.message, { title: 'Not reset' }); }
    }

    // ---- engine stats / tiles ------------------------------------------------------------------
    async function loadStats() {
      try {
        st.stats = await api.get('/api/alerts/stats', { signal: ctx.signal });
        st.statsErr = null;
      } catch (e) {
        if (e.aborted) return;
        st.statsErr = e;
      }
      renderTiles();
    }

    function renderTiles() {
      const s = st.stats;
      if (!s) {
        if (st.statsErr) for (const k of ['fired', 'eval', 'hook']) tiles[k].setSub('not available');
        if (st.rules) { tiles.rules.set(units.int(st.rules.filter((r) => r.enabled).length)); tiles.rules.setSub(`of ${units.int(st.rules.length)} rules`); }
        return;
      }
      tiles.rules.set(units.int(s.enabledRules), '');
      tiles.rules.setSub(`of ${units.int(s.rules)} rules`);
      tiles.fired.set(units.int(s.fired));
      tiles.fired.setSub(`${units.int(s.suppressed)} held back by cooldowns`);
      const ev = evalTime(s.lastEvalNanos ?? (typeof s.lastEvalMicros === 'number' ? s.lastEvalMicros * 1000 : null));
      tiles.eval.set(ev[0], ev[1]);
      tiles.eval.setSub(`${units.int(s.lastEvalAircraft)} aircraft · ${units.si(s.evaluations)} runs`);
      tiles.hook.set(units.int(s.webhookSent), 'sent');
      const fails = (s.webhookFailed || 0) + (s.webhookDropped || 0);
      tiles.hook.setSub(fails ? h('span.c-red', { tip: s.lastWebhookError || '' }, `${units.int(s.webhookFailed)} failed · ${units.int(s.webhookDropped)} dropped`) : 'no failures');
    }

    ctx.subscribe('aircraft', ui.throttle((snap) => {
      let n = 0;
      for (const a of snap?.aircraft || []) if (a.highlight) n++;
      tiles.hl.set(units.int(n));
      tiles.hl.setSub(n ? 'colour-marked on the Live Map' : 'none at the moment');
    }, 2000));

    // ---- recent alerts feed --------------------------------------------------------------------
    // Pushed live by the SSE `event: alert`; live.followAlerts() adds the catch-up fetches
    // (/api/alerts/recent?after=) on mount, on every stream reconnect and while the stream is down.
    let feedRender = 0;
    const scheduleFeed = () => { if (!feedRender) feedRender = requestAnimationFrame(() => { feedRender = 0; renderFeed(); }); };
    ctx.onCleanup(() => cancelAnimationFrame(feedRender));
    const follow = live.followAlerts({
      signal: ctx.signal,
      initial: FEED_MAX,
      onAlerts(fresh, info) {
        st.feedErr = null;
        st.feedLoaded = true;
        st.lastId = Math.max(st.lastId, fresh[fresh.length - 1].id);
        st.feed = [...fresh.slice().reverse(), ...st.feed].slice(0, FEED_MAX);
        let changedRules = false;
        for (const a of fresh) if (!st.lastFired.has(a.ruleId) || st.lastFired.get(a.ruleId) < a.t) { st.lastFired.set(a.ruleId, a.t); changedRules = true; }
        if (changedRules) renderRules();
        if (info.initial) renderFeed(); else scheduleFeed();
      },
      onReset() { st.feed = []; st.lastId = 0; st.feedLoaded = false; st.lastFired.clear(); renderRules(); },
      onOk() { const was = st.feedErr || !st.feedLoaded; st.feedErr = null; st.feedLoaded = true; if (was) renderFeed(); },
      onError(e) { st.feedErr = e; renderFeed(); },
    });
    const pollFeed = () => follow.sync();

    function renderFeed() {
      if (!st.feedLoaded) {
        ui.mount(feedBody, st.feedErr ? ui.error(st.feedErr, { title: 'Alerts could not be loaded', retry: pollFeed, compact: true }) : ui.skeleton({ lines: 6 }));
        return;
      }
      const kind = prefs.get().feedKind;
      const minSev = SEV_RANK[prefs.get().feedSev] ?? -1;
      const rows = st.feed.filter((a) => (kind === 'all' || a.kind === kind) && (SEV_RANK[a.severity] ?? 0) >= minSev);
      const errNote = st.feedErr ? h('div.alx-callout.alx-callout--warn', ui.icon('alert'), 'Live updates paused: ' + st.feedErr.message) : null;
      if (!rows.length) {
        ui.mount(feedBody, errNote, ui.empty({ icon: 'bell', compact: true, title: st.feed.length ? 'Nothing matches the filter' : 'No alerts yet',
          text: st.feed.length ? 'Change the filter to see the other alerts.' : 'Not enough data yet - alerts appear here as soon as an enabled rule fires.' }));
        return;
      }
      const now = live.serverNow();
      ui.mount(feedBody, errNote, rows.map((a) => feedItem(a, now)));
    }

    function feedItem(a, now) {
      const ident = a.callsign || a.reg || a.icao;
      const meta = [];
      if (typeof a.altFt === 'number') meta.push(units.alt(a.altFt, { fl: true }));
      if (typeof a.distKm === 'number') meta.push(units.dist(a.distKm));
      if (a.squawk) meta.push('sq ' + a.squawk);
      if (a.type) meta.push(a.type);
      if (a.operator) meta.push(a.operator);
      return h('article.alf' + '.alf--' + (a.severity || 'info'), { style: a.highlight ? { '--hl': a.highlight } : null },
        h('div.alf-head',
          h('span.alf-time.num', { tip: units.dateTime(a.t) }, units.clock(a.t)),
          ui.badge(cap(a.severity || 'info'), severityBadge(a.severity)),
          h('span.alf-rule', a.ruleName || a.ruleId),
          h('span.spacer'),
          h('span.alf-ago.muted', units.ago(a.t, now))),
        a.kind === 'aircraft' && a.icao
          ? h('div.alf-ac', h('a.alf-ident', { href: '#/map/' + a.icao, tip: 'Show on the Live Map' }, ui.icon('map'), ident), ident !== a.icao ? h('span.num.muted', a.icao) : null, meta.length ? h('span.alf-meta.num', meta.join(' · ')) : null)
          : a.icao ? h('div.alf-ac', h('a.alf-ident', { href: '#/map/' + a.icao }, ui.icon('map'), ident)) : null,
        h('div.alf-text', units.alertText(a)),
        a.webhook || a.logged ? h('div.alf-flags', a.webhook ? h('span', ui.icon('link'), 'webhook') : null, a.logged ? h('span', ui.icon('database'), 'logged') : null) : null);
    }

    // ---- timers / lifecycle ---------------------------------------------------------------------
    ctx.every(STATS_POLL_MS, () => { if (!document.hidden) loadStats(); });
    // refresh the "x min ago" labels
    ctx.every(30000, () => {
      if (document.hidden) return;
      renderFeed();
      if (st.lastFired.size) renderRules();
    });
    ctx.onCleanup(units.onChange(() => { renderRules(); renderFeed(); }));
    ctx.onCleanup(() => { if (st.editor) st.editor.close(); });
    const firstGesture = () => unlockSound();
    container.addEventListener('pointerdown', firstGesture, { once: true });

    renderFeed();
    await Promise.all([loadRules(), loadStats(), follow.ready]);
  },
};

/**
 * Evaluation time [value, unit] with 2 significant digits: 850 ns, 440 ns, 4.2 µs, 37 µs, 1.3 ms, 2.0 s.
 * A zero means the run was faster than the server's resolution: '<1 µs' when only micros are known.
 */
export function evalTime(ns) {
  if (typeof ns !== 'number' || !Number.isFinite(ns) || ns < 0) return ['—', ''];
  if (ns === 0) return ['<1', 'µs'];
  // round to 2 significant digits first, then pick the unit: 999.6 µs -> 1.0 ms, 9960 ns -> 10 µs
  const r = Number(ns.toPrecision(2));
  const [div, unit] = r >= 1e9 ? [1e9, 's'] : r >= 1e6 ? [1e6, 'ms'] : r >= 1e3 ? [1e3, 'µs'] : [1, 'ns'];
  const v = Number((r / div).toPrecision(2));
  return [v >= 10 ? String(Math.round(v)) : v.toFixed(1), unit];
}

function cap(s) { return s ? s[0].toUpperCase() + s.slice(1) : ''; }
