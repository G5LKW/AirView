// pages/settings.js - configuration UI over GET/PUT /api/config (model.Config).
//
// One draft of the whole configuration is edited across all sections (#/settings/<section>); nothing is sent
// until "Save". The draft survives section changes, failed saves and leaving the page (it is parked in
// ctx.shared and restored on return). Client-side validation mirrors internal/config/validate.go and blocks
// saving; whatever the server still corrects is found by comparing the sent and the stored configuration and
// shown next to the field. When this client is locked out (status.adminLocked) the page is a read-only view
// with a login prompt; every admin call retries once after a successful login on 401.
//
// Section list, validation and the import-shape check of every block (including prediction, terrain, navData,
// alerts and rfStat) live in settings/model.js. This file renders the Time (NTP) section and the sections of
// those newer blocks itself.

import { SECTIONS, clone, getPath, setPath, validate, diffConfig, changedKeys, splitHost } from '../settings/model.js';
import { makeForm } from '../settings/form.js';
import { createAdmin } from '../admin.js';
import { renderStation, renderAntennas, renderFeeds } from '../settings/sections-station.js';
import { renderWeather, renderWeb, renderConnection, renderDisplay, renderDemo } from '../settings/sections-system.js';
import { renderStorage, renderDanger, renderAbout } from '../settings/sections-data.js';
import { renderUplinks } from '../settings/sections-uplinks.js';
import { renderUpdates } from '../settings/sections-update.js';
import { renderSSR } from '../settings/sections-ssr.js';
import { hasRadarcape } from '../settings/links.js';

const RENDER = {
  station: renderStation, antennas: renderAntennas, feeds: renderFeeds, time: renderTimeRef, weather: renderWeather,
  web: renderWeb, connection: renderConnection, display: renderDisplay, storage: renderStorage, demo: renderDemo,
  danger: renderDanger, about: renderAbout, uplinks: renderUplinks,
  prediction: renderPrediction, terrain: renderTerrain, navdata: renderNavData, alerts: renderAlerts, rf: renderRF,
  updates: renderUpdates, ssr: renderSSR,
};

const isNum = (v) => typeof v === 'number' && Number.isFinite(v);

const STASH_KEY = 'settings.stash';
const mem = { open: {} };          // UI memory that survives navigation (which list item is expanded ...)
let S = null;

function makeScope(ctx) {
  const ac = new AbortController();
  const cleanups = [];
  return {
    signal: ac.signal,
    onCleanup: (fn) => { cleanups.push(fn); },
    every(ms, fn) { const t = setInterval(() => { if (!document.hidden) fn(); }, ms); cleanups.push(() => clearInterval(t)); },
    subscribe(type, fn, opts) { cleanups.push(ctx.live.on(type, fn, opts)); },
    dispose() { ac.abort(); for (const fn of cleanups.splice(0)) { try { fn(); } catch (e) { console.error('[settings] cleanup failed', e); } } },
  };
}

const fmtVal = (v) => (v === '' || v === null || v === undefined ? 'empty' : Array.isArray(v) ? v.join(', ') : String(v));

export default {
  id: 'settings',
  title: 'Settings',

  async mount(container, ctx) {
    const { api, ui, units, config, live } = ctx;
    const { h } = ui;
    await ui.loadCss(new URL('../../css/settings.css', import.meta.url));

    const navEl = h('nav.set-nav', { 'aria-label': 'Settings sections' });
    const bannerEl = h('div.set-banners');
    const sectionEl = h('fieldset.set-section');
    const barEl = h('div.set-savebar.hidden', { role: 'region', 'aria-label': 'Unsaved changes' });
    const headBadges = h('div.actions');
    container.append(h('div.page-inner.set',
      h('div.page-head', h('div', h('h2', 'Settings'), h('div.sub', 'Station, antennas, feeds and everything else AirView stores in config.json')), headBadges),
      bannerEl,
      h('div.set-layout', navEl, h('div.set-main', sectionEl)),
      barEl));

    // ---- load -------------------------------------------------------------------------------
    ui.mount(sectionEl, ui.skeleton({ lines: 8, title: true }));
    let cfg;
    try {
      cfg = await config.load({ signal: ctx.signal });
    } catch (e) {
      if (e.aborted || e.cancelled) return;
      ui.mount(sectionEl, ui.error(e, { title: 'The configuration could not be loaded', retry: () => location.reload() }));
      return;
    }

    const store = {
      ctx, mem,
      saved: cfg, draft: clone(cfg),
      status: live.status || ctx.status || null,
      session: null, system: null,
      locked: !!(live.status || ctx.status)?.adminLocked,
      fields: new Map(), notes: new Map(), local: new Map(), problems: [],
      serverMsgs: [], saveError: null, saving: false, savedAt: 0,
      section: null, scope: null,

      get: (path) => getPath(store.draft, path),
      savedValue: (path) => getPath(store.saved, path),
      /** Write one draft value; repaints validation and the save bar. */
      set(path, value) {
        setPath(store.draft, path, value);
        store.notes.delete(path);
        store.touch();
      },
      /** Re-validate after any change of the draft. */
      touch() {
        store.problems = validate(store.draft);
        for (const [path, msg] of store.local) store.problems.push({ path, msg, section: sectionOfPath(path) });
        for (const path of store.fields.keys()) store.paint(path);
        renderBar();
        renderNav();
      },
      /** Paint the problem / server note of one registered field. */
      paint(path) {
        const f = store.fields.get(path);
        if (!f) return;
        const p = store.problems.find((x) => x.path === path);
        f.setError(p ? p.msg : null);
        const note = store.notes.get(path);
        if (f.__note) { f.__note.classList.toggle('hidden', !note); ui.setText(f.__note, note || ''); }
      },
      problem: (path) => store.problems.find((x) => x.path === path)?.msg || null,
      isDirty: () => JSON.stringify(store.draft) !== JSON.stringify(store.saved),
      rerender: () => showSection(store.section, { keepScroll: true }),
      save, discard, login, admin, saveOnly,
      /** Has any of these top-level config keys unsaved edits? */
      dirtyKeys: (keys) => keys.some((k) => JSON.stringify(store.draft[k]) !== JSON.stringify(store.saved[k])),
      /** ui.load with a gentle refresh tied to the lifetime of the current section. */
      panel(host, loader, render, periodMs) {
        const scope = store.scope;
        // ui.load only mounts a returned Node; section renderers return arrays, so mount here (this also
        // replaces the old content on a quiet refresh instead of appending to it).
        const draw = async (data, el) => { const out = await render(data, el); if (out !== undefined) ui.mount(el, out); };
        const reload = ui.load(host, () => loader(scope.signal), draw, { compact: true, skeleton: ui.skeleton({ lines: 3 }) });
        if (periodMs) scope.every(periodMs, () => reload({ quiet: true }));
        return reload;
      },
      refreshSystem, refreshSession,
    };
    S = store;
    const F = makeForm(store);
    store.F = F;

    const sectionOfPath = (path) => {
      const top = String(path).split('.')[0];
      return SECTIONS.find((s) => s.keys.includes(top))?.id || 'station';
    };

    // ---- restore a draft parked by an earlier visit --------------------------------------------
    const stash = ctx.shared.get(STASH_KEY);
    if (stash && stash.draft) {
      store.draft = stash.draft;
      ctx.shared.set(STASH_KEY, null);
      if (store.isDirty()) ui.toast.info('Your unsaved settings were restored.', { title: 'Settings' });
    }
    store.problems = validate(store.draft);

    // ---- admin: session, login, 401 handling --------------------------------------------------
    async function refreshSession() {
      try { store.session = await api.get('/api/session', { signal: ctx.signal }); } catch (e) { if (!e.aborted && !e.cancelled) store.session = null; }
      return store.session;
    }
    async function refreshSystem() {
      try { store.system = await api.get('/api/system', { signal: ctx.signal }); } catch (e) { if (e.aborted) return null; }
      renderBanners();
      return store.system;
    }
    function setLocked(locked) {
      if (locked === store.locked) return;
      store.locked = locked;
      renderBanners();
      renderBar();
      if (store.section) store.rerender();
    }

    // Login dialog and 401-retry come from js/admin.js; the hooks keep the page's lock state in step.
    const adminHelper = createAdmin(ctx, {
      onLocked() { setLocked(true); },
      onLogin(session) {
        store.session = session;
        ui.toast.success('Logged in as admin.');
        live.refreshStatus().catch(() => { /* the next status event corrects it */ });
        setLocked(false);
        renderBanners();
      },
    });

    /** Login dialog -> true when this client is admin afterwards. */
    function login(what) { return adminHelper.prompt(typeof what === 'string' ? what : 'Log in to change the settings of this station.'); }

    /** Run an admin call; on 401 offer the login dialog and retry once (rejects with .cancelled when dismissed). */
    function admin(fn, what) { return adminHelper.run(fn, what || 'Changing the settings needs the admin password.'); }

    // ---- save / discard -----------------------------------------------------------------------
    async function save() {
      if (store.saving) return;
      store.touch();
      if (store.problems.length) {
        const first = store.problems[0];
        ui.toast.warn(`${store.problems.length} ${store.problems.length === 1 ? 'value needs' : 'values need'} attention before saving.`, { title: 'Not saved' });
        gotoProblem(first);
        return;
      }
      const body = clone(store.draft);
      store.saving = true;
      store.saveError = null;
      store.serverMsgs = [];
      store.savedAt = Date.now();
      renderBar();
      try {
        const stored = await admin(() => config.save(body));
        const corrections = diffConfig(body, stored);
        store.saved = stored;
        store.draft = clone(stored);
        store.local.clear();
        store.notes.clear();
        for (const c of corrections) store.notes.set(c.path, `Adjusted by the server: you entered ${fmtVal(c.sent)}, stored ${fmtVal(c.stored)}.`);
        store.corrections = corrections;
        ui.toast.success(corrections.length ? `Saved - the server adjusted ${corrections.length} ${corrections.length === 1 ? 'value' : 'values'}.` : 'Settings saved and applied.');
        store.saving = false;
        store.problems = validate(store.draft);
        store.rerender();
        renderBar();
        renderNav();
        refreshSystem();
        live.refreshStatus().catch(() => {});
      } catch (e) {
        store.saving = false;
        // The draft is untouched: nothing the user typed is lost.
        store.saveError = e.cancelled || e.status === 401 ? 'Not saved: the admin password is required. Log in and press Save again - your changes are still here.'
          : e.network ? 'Not saved: the AirView server cannot be reached. Your changes are still here - try again.'
            : `Not saved: ${e.message}`;
        renderBar();
      }
    }

    async function discard() {
      if (!store.isDirty()) return;
      const n = changedKeys(store.draft, store.saved).length;
      const ok = await ui.confirm({ title: 'Discard changes?', message: `All unsaved changes (${n} ${n === 1 ? 'section' : 'sections'}) are thrown away and the stored settings are shown again.`, confirmLabel: 'Discard', danger: true });
      if (!ok) return;
      store.draft = clone(store.saved);
      store.local.clear();
      store.notes.clear();
      store.saveError = null;
      store.problems = validate(store.draft);
      store.rerender();
      renderBar();
      renderNav();
    }

    /**
     * Save only some top-level sections (e.g. "Save & test" of the NTP server); other unsaved edits stay in the
     * draft. Returns the stored config, or null when validation blocked it.
     */
    async function saveOnly(keys) {
      if (store.saving) return null;
      store.touch();
      const probs = store.problems.filter((p) => keys.includes(String(p.path).split('.')[0]));
      if (probs.length) {
        ui.toast.warn(probs[0].msg, { title: 'Not saved' });
        gotoProblem(probs[0]);
        return null;
      }
      const sent = {};
      for (const k of keys) sent[k] = clone(store.draft[k]);
      store.saving = true;
      renderBar();
      try {
        const stored = await admin(() => config.save((c) => { for (const k of keys) c[k] = clone(sent[k]); }));
        store.saved = stored;
        for (const k of keys) {
          store.draft[k] = clone(stored[k]);
          for (const c of diffConfig({ [k]: sent[k] }, { [k]: stored[k] })) store.notes.set(c.path, `Adjusted by the server: you entered ${fmtVal(c.sent)}, stored ${fmtVal(c.stored)}.`);
        }
        return stored;
      } finally {
        store.saving = false;
        store.touch();
        live.refreshStatus().catch(() => {});
      }
    }

    function gotoProblem(p) {
      if (p.section !== store.section) ctx.navigate('settings/' + p.section);
      setTimeout(() => {
        const f = store.fields.get(p.path);
        if (f) { f.scrollIntoView({ block: 'center', behavior: 'smooth' }); const inp = f.querySelector('input,select,textarea'); if (inp) inp.focus({ preventScroll: true }); }
      }, 60);
    }

    // ---- chrome: nav, banners, save bar ---------------------------------------------------------
    const navItems = new Map();
    for (const s of SECTIONS) {
      const dot = h('i.set-nav-dot');
      const sync = s.id === 'time' ? h('i.set-nav-sync') : null;
      const a = h('a.set-nav-item' + (s.id === 'danger' ? '.set-nav-item--danger' : '') + (s.id === 'time' ? '.set-nav-item--key' : ''), { href: '#/settings/' + s.id }, ui.icon(s.icon), h('span', s.label), sync, dot);
      navItems.set(s.id, { a, dot, sync });
      navEl.appendChild(a);
    }
    /** The Time entry carries a small sync lamp: the timing analysis depends on it. */
    function paintNavSync(st) {
      const el = navItems.get('time')?.sync;
      if (!el) return;
      const n = st?.ntp;
      const cls = !n?.enabled ? 'off' : n.synced ? 'ok' : 'warn';
      el.className = 'set-nav-sync ' + cls;
      el.dataset.tip = cls === 'off' ? 'NTP sampling is off' : cls === 'ok' ? `Synchronised (stratum ${n.stratum || '?'})` : 'NTP: no good sample yet';
    }
    paintNavSync(store.status);
    function renderNav() {
      const dirty = new Set(changedKeys(store.draft, store.saved));
      for (const s of SECTIONS) {
        const it = navItems.get(s.id);
        const on = s.id === store.section;
        it.a.classList.toggle('active', on);
        if (on) it.a.setAttribute('aria-current', 'page'); else it.a.removeAttribute('aria-current');
        const bad = store.problems.some((p) => p.section === s.id);
        const changed = s.keys.some((k) => dirty.has(k));
        it.dot.className = 'set-nav-dot' + (bad ? ' bad' : changed ? ' changed' : '');
        it.dot.dataset.tip = bad ? 'Needs attention' : changed ? 'Unsaved changes' : '';
        if (!bad && !changed) delete it.dot.dataset.tip;
      }
    }

    function renderBanners() {
      const out = [];
      const ses = store.session;
      ui.mount(headBadges,
        store.locked ? ui.badge('Read-only', 'amber', { large: true, tip: 'An admin password protects the settings' })
          : ses?.loopback ? ui.badge('Local admin', 'green', { large: true, tip: 'Requests from this computer are always admin' })
            : ses?.loggedIn ? ui.badge('Logged in', 'green', { large: true }) : null,
        store.locked ? ui.button({ label: 'Log in', icon: 'lock', variant: 'primary', size: 'sm', onClick: login }) : null);
      if (store.locked) {
        out.push(h('div.set-callout.set-callout--warn', ui.icon('lock'),
          h('div.grow', h('b', 'Settings are locked. '), 'An admin password protects this station. You can look at everything, but changes need a login.'),
          ui.button({ label: 'Log in', icon: 'lock', variant: 'primary', size: 'sm', onClick: login })));
      }
      if (store.system?.restartRequired) {
        out.push(h('div.set-callout.set-callout--warn', ui.icon('power'),
          h('div.grow', h('b', 'Restart required. '), `The web server still listens on port ${store.system.httpPort}; the saved port / bind address take effect when AirView is restarted.`)));
      }
      ui.mount(bannerEl, out);
    }

    function renderBar() {
      const dirty = store.isDirty();
      const corrections = store.corrections || [];
      const show = !store.locked && (dirty || store.saving || store.saveError || corrections.length || store.serverMsgs.length);
      barEl.classList.toggle('hidden', !show);
      if (!show) return;
      const keys = changedKeys(store.draft, store.saved);
      const names = SECTIONS.filter((s) => s.keys.some((k) => keys.includes(k))).map((s) => s.label);
      const nProb = store.problems.length;
      const info = h('div.set-savebar-text');
      if (dirty) {
        info.append(h('div.set-savebar-title', h('i.set-pulse'), 'Unsaved changes'),
          h('div.set-savebar-sub', names.length ? 'In: ' + names.join(', ') : ''));
      } else if (corrections.length || store.serverMsgs.length) {
        info.append(h('div.set-savebar-title', 'Saved'), h('div.set-savebar-sub', 'The server adjusted some values:'));
      }
      const extra = [];
      if (nProb && dirty) {
        extra.push(h('button.set-savebar-problems', { type: 'button', onclick: () => gotoProblem(store.problems[0]) },
          ui.icon('alert'), `${nProb} ${nProb === 1 ? 'problem' : 'problems'} - show`));
      }
      if (store.saveError) extra.push(h('div.set-savebar-error', { role: 'alert' }, ui.icon('x-circle'), store.saveError));
      if (!dirty && (corrections.length || store.serverMsgs.length)) {
        extra.push(h('ul.set-savebar-list',
          corrections.slice(0, 8).map((c) => h('li', h('code', c.path), ` ${fmtVal(c.sent)} → ${fmtVal(c.stored)}`)),
          corrections.length > 8 ? h('li', `… and ${corrections.length - 8} more`) : null,
          store.serverMsgs.slice(0, 8).map((m) => h('li', m))));
      }
      const buttons = dirty || store.saving
        ? [ui.button({ label: 'Discard', icon: 'history', variant: 'ghost', onClick: discard, disabled: store.saving }),
          (() => { const b = ui.button({ label: 'Save', icon: 'save', variant: 'primary', onClick: save }); if (store.saving) b.setLoading(true); return b; })()]
        : [ui.button({ label: 'Dismiss', variant: 'ghost', size: 'sm', onClick: () => { store.corrections = []; store.serverMsgs = []; store.saveError = null; renderBar(); } })];
      ui.mount(barEl, h('div.set-savebar-inner', info, h('div.set-savebar-extra', extra), h('div.set-savebar-actions', buttons)));
    }

    // ---- section rendering ------------------------------------------------------------------------
    function showSection(id, opts = {}) {
      const page = container;
      const top = page.scrollTop;
      if (store.scope) store.scope.dispose();
      store.scope = makeScope(ctx);
      store.section = id;
      store.fields.clear();
      const meta = SECTIONS.find((s) => s.id === id);
      ctx.setTitle('Settings - ' + meta.label);
      sectionEl.disabled = store.locked;
      sectionEl.classList.toggle('is-locked', store.locked);
      let content;
      try {
        content = RENDER[id](store, F);
      } catch (e) {
        console.error('[settings] section failed', id, e);
        content = ui.error(e, { title: 'This section could not be shown' });
      }
      ui.mount(sectionEl, h('div.set-section-head', ui.icon(meta.icon), h('h3', meta.label)), content);
      renderNav();
      page.scrollTop = opts.keepScroll ? top : 0;
      const active = navItems.get(id)?.a;
      if (active && !opts.keepScroll && active.scrollIntoView) active.scrollIntoView({ block: 'nearest', inline: 'nearest' });
    }
    store.show = showSection;

    const sectionFromRoute = (route) => {
      const id = route?.args?.[0];
      return RENDER[id] ? id : null;
    };
    this._onRoute = (route) => {
      const id = sectionFromRoute(route);
      if (!id) { ctx.navigate('settings/' + (store.section || 'station'), { replace: true }); return; }
      if (id !== store.section) showSection(id);
    };

    // ---- live wiring ----------------------------------------------------------------------------------
    ctx.subscribe('status', (st) => { store.status = st; paintNavSync(st); setLocked(!!st?.adminLocked); }, { immediate: false });
    ctx.subscribe('notice', (n) => {
      // Corrections made by config.Validate arrive as warn notices right after a save.
      if (!n || n.level !== 'warn' || Date.now() - store.savedAt > 6000) return;
      if (!store.serverMsgs.includes(n.text)) { store.serverMsgs.push(n.text); renderBar(); }
    }, { immediate: false });
    ctx.onCleanup(config.subscribe((next) => {
      if (store.saving || next === store.saved) return;
      const wasDirty = store.isDirty();
      store.saved = next;                      // changed elsewhere (another tab saved, factory reset ...)
      if (!wasDirty) { store.draft = clone(next); if (store.section) store.rerender(); }
      store.touch();
    }));
    ctx.onCleanup(units.onChange(() => { if (store.section) store.rerender(); }));
    const beforeUnload = (e) => { if (store.isDirty()) { e.preventDefault(); e.returnValue = ''; } };
    window.addEventListener('beforeunload', beforeUnload);
    ctx.onCleanup(() => window.removeEventListener('beforeunload', beforeUnload));
    ctx.onCleanup(() => {
      if (store.scope) store.scope.dispose();
      // Park unsaved edits so that a detour to another page does not lose them.
      ctx.shared.set(STASH_KEY, store.isDirty() ? { draft: store.draft } : null);
      if (S === store) S = null;
    });

    renderBanners();
    renderBar();
    const first = sectionFromRoute(ctx.route);
    // Not ctx.navigate: a replace-navigation re-enters the router while this page is still mounting.
    if (!first) history.replaceState(null, '', '#/settings/station');
    showSection(first || 'station');
    refreshSession().then(() => { if (store.session) { setLocked(!store.session.admin); renderBanners(); } });
    refreshSystem();
  },

  onRoute(route) { if (this._onRoute) this._onRoute(route); },

  unmount() { this._onRoute = null; },
};

// =============================================================================================
// shared bits of the sections below
// =============================================================================================

const extLink = (h, href, text) => h('a', { href, target: '_blank', rel: 'noopener noreferrer' }, text);

/** GET that turns "409 switched off" into { disabled: true } so the panel can show an empty state instead of an error. */
const getOrOff = (api, path, signal, params) => api.get(path, { signal, params }).catch((e) => {
  if (e && e.status === 409) return { disabled: true, message: e.message };
  throw e;
});

/** Slider bound to an integer config path. o: { label, hint, min, max, step, format, onInput } */
function sliderField(S, F, path, o) {
  const { ui } = S.ctx;
  const cur = S.get(path);
  const sl = ui.slider({
    min: o.min, max: o.max, step: o.step || 1, value: isNum(cur) ? Math.min(o.max, Math.max(o.min, cur)) : o.min, format: o.format,
    onInput: (v) => { S.set(path, v); if (o.onInput) o.onInput(v); },
  });
  const f = F.reg(path, ui.field({ label: o.label, hint: o.hint, control: sl }));
  f.control = sl;
  return f;
}

/**
 * S.panel with an adaptive refresh: every `fast` ms while busy() says so, otherwise every `slow` ms.
 * Returns reload().
 */
function adaptivePanel(S, host, loader, render, busy, fast = 1000, slow = 15000) {
  const reload = S.panel(host, loader, render);
  let last = Date.now();
  S.scope.every(fast, () => {
    const now = Date.now();
    if (busy() || now - last >= slow) { last = now; reload({ quiet: true }); }
  });
  return reload;
}

/** "Changes are only used after saving" note for actions that run on the stored configuration. */
function savedOnlyNote(S, keys, text) {
  const { ui } = S.ctx;
  const el = ui.h('div.set-help.set-help--warn');
  const paint = () => ui.setText(el, S.dirtyKeys(keys) ? text : '');
  paint();
  S.scope.every(1000, paint);       // cheap: one JSON compare of a small section, text written only on change
  return { el, paint };
}

// =============================================================================================
// Time (NTP) - the reference behind every timing statistic
// =============================================================================================

const GPS_REFIDS = /^(GPS|GPSD|PPS|GNSS|GAL|GLO|BDS|NMEA|SHM|SOCK|PZF|DCF|MSF|WWVB?|JJY|ATOM|CDMA|IRIG|TDF|CHU)$/i;

/** Quality verdict of an NTP reference: { kind: 'green'|'blue'|'amber', text } */
function ntpGrade(stratum, refId, delayMs) {
  if (!(stratum > 0)) return null;
  const ref = String(refId || '').trim();
  let g;
  if (stratum === 1 && GPS_REFIDS.test(ref)) g = { kind: 'green', text: `Excellent: a stratum-1 server locked to ${ref}. This is the best reference for the timing analysis.` };
  else if (stratum === 1) g = { kind: 'green', text: `Stratum 1 (reference clock "${ref || '?'}").` };
  else if (stratum <= 3) g = { kind: 'blue', text: `Good: stratum ${stratum}. A GPS-locked stratum-1 server on your network would make the timing analysis more precise.` };
  else g = { kind: 'amber', text: `Coarse: stratum ${stratum} is far from a reference clock. Use a stratum-1 or stratum-2 server.` };
  if (isNum(delayMs) && delayMs > 40) g = { kind: g.kind === 'green' ? 'blue' : 'amber', text: g.text + ` The round trip of ${Math.round(delayMs)} ms limits the accuracy: a server on the local network is better.` };
  return g;
}

function renderTimeRef(S, F) {
  const { ui, api, units } = S.ctx;
  const { h } = ui;
  const scope = S.scope;
  const msTxt = (v, d = 3) => (isNum(v) ? `${units.num(v, d)} ms` : '—');

  // ---- headline tiles ----
  const tOffset = ui.stat({ label: 'Clock offset', icon: 'timer', unit: 'ms', accent: 'cyan', tip: 'True time minus the host clock' });
  const tDelay = ui.stat({ label: 'Round-trip delay', icon: 'signal', unit: 'ms' });
  const tStratum = ui.stat({ label: 'Stratum', icon: 'layers', tip: '1 = directly attached to a reference clock (GPS, PPS ...)' });
  const tRef = ui.stat({ label: 'Reference ID', icon: 'target', tip: 'What the server is synchronised to: "GPS"/"PPS" for a GPS-locked stratum 1, the upstream server for stratum 2+' });
  const stateBadge = h('span');
  const gradeEl = h('div');
  function paintTiles(st, latest) {
    const have = st && st.enabled && st.lastOk > 0;
    tOffset.set(have ? units.num(st.offsetMs, 3) : '—', 'ms');
    tDelay.set(have ? units.num(st.delayMs, 2) : '—', 'ms');
    tStratum.set(have && st.stratum ? String(st.stratum) : '—');
    tRef.set(have ? (st.refId || latest?.refId || '—') : '—');
    tOffset.setSub(have && isNum(st.jitterMs) ? `jitter ${msTxt(st.jitterMs, 3)}` : '');
    tDelay.setSub(have && latest?.addr ? latest.addr : '');
    tStratum.setSub(have ? (st.stratum === 1 ? 'reference clock' : st.stratum ? `${st.stratum - 1} hop${st.stratum > 2 ? 's' : ''} from a reference` : '') : '');
    tRef.setSub(have ? (GPS_REFIDS.test(st.refId || '') ? 'GPS locked' : '') : '');
    ui.mount(stateBadge, !st ? null : !st.enabled ? ui.badge('Sampling off', 'other', { large: true })
      : st.synced ? ui.badge('Synchronised', 'green', { large: true }) : ui.badge(st.lastOk ? 'Not synchronised' : 'No answer yet', 'amber', { large: true }));
    const g = have ? ntpGrade(st.stratum, st.refId || latest?.refId, st.delayMs) : null;
    ui.mount(gradeEl, g ? h('div.set-grade.set-grade--' + g.kind, ui.icon(g.kind === 'amber' ? 'alert' : 'check-circle'), h('span', g.text)) : null);
  }
  paintTiles(null);

  // ---- server entry ----
  // only a real Radarcape (not the factory placeholder feed) is offered as an NTP source
  const radarcape = hasRadarcape(S.draft, S.status) ? (S.draft.feeds || []).find((f) => /radarcape/i.test(`${f.name} ${f.host}`) && splitHost(f.host).host) : null;
  const serverF = F.text('ntp.server', {
    label: 'NTP server', mono: true, placeholder: '192.168.1.10   or   ntp.example.lan:123',
    hint: 'Host name or IP address, optionally with ":port" (default 123; IPv6: [fd00::1]:123). The only time reference AirView uses. A GPS-locked stratum-1 server on your own network gives the best timing.', onChange: paintTest,
  });
  serverF.classList.add('set-ntp-server');
  const pick = (value, label, tip) => ui.button({ label, size: 'sm', variant: 'ghost', tip, onClick: () => { serverF.control.value = value; S.set('ntp.server', value); if (!S.get('ntp.enabled')) { S.set('ntp.enabled', true); enabledF.control.checked = true; } paintTest(); } });
  const picks = h('div.set-chips',
    h('span.muted', 'Quick fill:'),
    radarcape ? pick(splitHost(radarcape.host).host, `Radarcape (${splitHost(radarcape.host).host})`, 'The Radarcape can serve its GPS time over NTP') : null,
    pick('pool.ntp.org', 'pool.ntp.org', 'Public pool: works anywhere, but only stratum 2-3 over the internet'));
  const enabledF = F.toggle('ntp.enabled', { label: 'Sample the NTP server', hint: 'Off: the host clock is trusted as it is.', onChange: paintTest });
  const intervalF = F.number('ntp.intervalSec', { label: 'Sampling interval', suffix: 's', min: 2, max: 86400, step: 1, hint: '2 - 86400 s. 64 s is the classic NTP poll interval; a server on your own network can take 16 s.', onChange: paintTest });

  // ---- test (the typed address, nothing saved) + save ----
  const testOut = h('div.set-ntp-out', { 'aria-live': 'polite' });
  const testNote = h('span.muted');
  const testBtn = ui.button({ label: 'Test now', icon: 'play', variant: 'primary', tip: 'One query to the address in the box - nothing is saved', onClick: runTest });
  const saveBtn = ui.button({ label: 'Save', icon: 'save', tip: 'Save the NTP settings only; other unsaved changes stay in the draft', onClick: saveNtp });
  function paintTest() {
    const dirty = S.dirtyKeys(['ntp']);
    const hp = splitHost(S.get('ntp.server'));
    testBtn.disabled = S.locked || !hp.ok || !hp.host;
    saveBtn.disabled = S.locked || !dirty;
    saveBtn.setLabel(dirty ? 'Save' : 'Saved');
    ui.setText(testNote, S.locked ? 'Log in to test or change the server.' : !hp.ok ? 'The address is not valid: use a host name, IPv4 or [IPv6] address, optionally with :port.'
      : !hp.host ? 'Enter a server address to test it.'
        : dirty ? '"Test now" queries the address as typed without saving it. Save to use it - it takes effect at once.'
          : 'Saved. "Test now" takes one extra measurement; the sampler keeps running on its interval.');
  }
  function verdictOf(s) {
    const gps = s.stratum === 1 && GPS_REFIDS.test(String(s.refId || '').trim());
    if (gps) return ui.badge('GPS-locked stratum 1', 'green', { large: true });
    if (s.stratum === 1) return ui.badge('Stratum 1 - reference "' + (s.refId || '?') + '"', 'cyan', { large: true });
    if (s.stratum > 1) return ui.badge(`Not GPS-locked · stratum ${s.stratum}`, s.stratum <= 3 ? 'blue' : 'amber', { large: true });
    return null;
  }
  async function runTest() {
    const typed = String(S.get('ntp.server') || '').trim();
    testBtn.setLoading(true);
    ui.mount(testOut, ui.loading(`Asking ${typed}…`, { compact: true }));
    try {
      const s = await S.admin(() => api.post('/api/timing/ntp/sample', { server: typed }, { signal: scope.signal, timeout: 20000 }));
      if (!s || s.ok === false) {
        ui.mount(testOut, h('div.set-result.bad', ui.icon('x-circle'), h('div',
          h('b', `${s?.server || typed} did not answer. `), s?.error || '',
          h('div.set-help', 'Check the address, that UDP port 123 of the server is reachable from the AirView computer (firewall), and that the server is actually serving time. Nothing was saved.'))));
        return;
      }
      const g = ntpGrade(s.stratum, s.refId, s.delayMs);
      ui.mount(testOut, h('div.set-result.ok', ui.icon('check-circle'), h('div.grow',
        h('div.set-ntp-res-head', h('b', `${s.server || typed} answered`), s.addr ? h('span.num.muted', ` from ${s.addr}`) : null, verdictOf(s)),
        h('div.grid.cols-3.set-ntp-res',
          resTile('Stratum', s.stratum ? String(s.stratum) : '—'),
          resTile('Reference ID', s.refId || '—'),
          resTile('Offset', msTxt(s.offsetMs), 'true time − host clock'),
          resTile('Round-trip delay', msTxt(s.delayMs, 2)),
          resTile('Root dispersion', msTxt(s.rootDispMs, 2), `root delay ${msTxt(s.rootDelayMs, 2)}`),
          resTile('Leap indicator', s.leap === 3 ? 'unsynchronised' : s.leap ? 'leap second' : 'none', units.clock(s.t, { seconds: true }))),
        g ? h('div.set-grade.set-grade--' + g.kind, ui.icon(g.kind === 'amber' ? 'alert' : 'check-circle'), h('span', g.text)) : null,
        h('div.set-help', S.dirtyKeys(['ntp']) ? 'Test only - nothing was saved. Press Save to use this server.' : 'Test only - the saved settings are unchanged.'))));
    } catch (e) {
      if (e.aborted || e.cancelled) return;
      ui.mount(testOut, h('div.set-result.bad', ui.icon('x-circle'), h('div', h('b', e.status === 400 ? 'Not a valid address. ' : 'Test failed. '), e.message)));
    } finally {
      testBtn.setLoading(false);
      paintTest();
    }
  }
  function resTile(label, value, sub) {
    return h('div.set-ntp-res-tile', h('div.label', label), h('div.num.set-ntp-res-val', value), sub ? h('div.muted.set-ntp-res-sub', sub) : null);
  }
  async function saveNtp() {
    saveBtn.setLoading(true);
    try {
      const stored = await S.saveOnly(['ntp']);
      if (stored) { ui.toast.success(stored.ntp?.server ? `NTP server ${stored.ntp.server} saved and in use.` : 'NTP settings saved.'); reload({ quiet: true }); setTimeout(() => reload({ quiet: true }), 3000); }
    } catch (e) {
      if (!e.aborted && !e.cancelled) ui.toast.error(e.message, { title: 'Not saved' });
    } finally {
      saveBtn.setLoading(false);
      paintTest();
    }
  }
  S.scope.every(1000, paintTest);   // follows Save / Discard in the save bar
  paintTest();

  // ---- live status + statistics ----
  const statusHost = h('div');
  const reload = S.panel(statusHost, (signal) => api.get('/api/timing/ntp', { signal }), (r) => {
    const st = r.status || {};
    paintTiles(st, r.latest);
    if (!st.enabled) {
      return ui.empty({ icon: 'timer', compact: true, title: 'NTP sampling is off',
        text: 'Enter your NTP server above, switch sampling on and press Save ("Test now" tries an address without saving it). AirView then measures how far the host clock is from true time and uses that to judge the receiver\'s GPS timestamps and the feed latency.' });
    }
    const hr = r.hour || {};
    const rows = [
      ['Server', st.server || '—', { text: true }],
      ['Last good sample', st.lastOk ? units.ago(st.lastOk) : 'never'],
      st.lastError ? ['Last error', String(st.lastError), { text: true }] : null,
    ];
    if (!hr.count) {
      return [ui.kv(rows), ui.empty({ icon: 'chart', compact: true, title: 'Not enough data yet', text: 'Statistics appear after the first successful samples.' })];
    }
    rows.push(
      ['Samples in the last hour', `${units.int(hr.count)}${hr.failures ? ` · ${units.int(hr.failures)} failed` : ''}`],
      ['Offset mean ± std (1 h)', `${msTxt(hr.offsetMeanMs)} ± ${msTxt(hr.offsetStdMs)}`],
      ['Offset range (1 h)', `${msTxt(hr.offsetMinMs)} … ${msTxt(hr.offsetMaxMs)}`],
      ['Delay min / mean (1 h)', `${msTxt(hr.delayMinMs, 2)} / ${msTxt(hr.delayMeanMs, 2)}`],
      ['Host clock drift', isNum(hr.driftPpm) ? `${units.num(hr.driftPpm, 2)} ppm` : 'needs 4 samples'],
      hr.steps ? ['Host clock steps detected', units.int(hr.steps)] : null);
    return ui.kv(rows);
  }, 10000);

  return [
    ui.card({ title: 'Time reference (NTP)', sub: 'The clock behind feed latency, GPS-timestamp checks and every timing statistic', icon: 'timer', accent: true, class: 'set-time-hero', actions: stateBadge, body: [
      h('div.grid.cols-4.set-tiles.set-time-tiles', tOffset, tDelay, tStratum, tRef),
      gradeEl,
      h('div.set-ntp-entry', serverF, h('div.set-ntp-test', testBtn, saveBtn)),
      picks,
      h('div.row-wrap', testNote),
      testOut,
      F.callout('info', 'target', h('b', 'Best timing: a local GPS-locked stratum-1 server. '),
        'A server whose clock is disciplined by GPS (stratum 1, reference "GPS" or "PPS") on your own network keeps the offset within a fraction of a millisecond - the yardstick AirView needs to judge the receiver\'s frame timestamps and the feed latency. ' + (radarcape ? 'The Radarcape itself can serve its GPS time over NTP; ' : '') + 'A Raspberry Pi with a GPS/PPS hat (gpsd + chrony) works as well. Public internet servers (stratum 2-3) are only good to a few milliseconds.'),
      h('div.set-fields.set-fields--2', enabledF, intervalF),
      F.help('AirView never sets the computer clock. It only measures the difference to the NTP server and corrects its own timing measurements with it.'),
    ] }),
    ui.card({ title: 'Live status', icon: 'signal', actions: h('a.set-link', { href: '#/timing' }, ui.icon('chart'), 'Timing page'), body: statusHost }),
  ];
}

// =============================================================================================
// Prediction
// =============================================================================================

function renderPrediction(S, F) {
  const { ui, api, units } = S.ctx;
  const { h } = ui;
  const SCALE = 130;              // seconds shown on the coasting timeline
  const START = 4;                // prediction starts 4 s after the last real position (server constant)

  // ---- coasting timeline ----
  const segLive = h('div.set-coast-seg.set-coast-seg--live');
  const segPred = h('div.set-coast-seg.set-coast-seg--pred');
  const segGone = h('div.set-coast-seg.set-coast-seg--gone');
  const markEnd = h('div.set-coast-mark.set-coast-mark--end');
  const endLabel = h('span.num');
  const timeline = h('div.set-coast', { 'aria-hidden': 'true' },
    h('div.set-coast-bar', segLive, segPred, segGone),
    h('div.set-coast-marks',
      h('div.set-coast-mark', { style: { left: '0%' } }, h('span', 'last position')),
      h('div.set-coast-mark.set-coast-mark--start', { style: { left: (START / SCALE) * 100 + '%' } }, h('span.num', `${START} s`)),
      markEnd));
  markEnd.append(endLabel);
  function paintTimeline() {
    const c = Math.max(START, Math.min(120, S.get('prediction.coastSec') || 0));
    segLive.style.width = (START / SCALE) * 100 + '%';
    segPred.style.width = ((c - START) / SCALE) * 100 + '%';
    segGone.style.width = ((SCALE - c) / SCALE) * 100 + '%';
    markEnd.style.left = (c / SCALE) * 100 + '%';
    ui.setText(endLabel, `${c} s`);
    body.classList.toggle('set-dim', !S.get('prediction.enabled'));
  }

  const coastF = sliderField(S, F, 'prediction.coastSec', {
    label: 'Keep coasting aircraft for', min: 5, max: 120, step: 1, format: (v) => `${v} s`, onInput: paintTimeline,
    hint: 'How long an aircraft without new positions stays on the map at its predicted position (5 - 120 s). After that it is drawn no more until a position arrives again.',
  });
  const pathF = sliderField(S, F, 'prediction.pathSec', {
    label: 'Predicted path length', min: 10, max: 900, step: 10, format: (v) => units.age(v),
    hint: 'How far ahead the predicted path of the selected aircraft reaches (10 s - 15 min). Uncertainty grows quickly beyond a few minutes.',
  });
  const body = h('div.set-group',
    h('div.set-glow-demo',
      h('span.set-glow-plane', ui.icon('plane')),
      h('div', h('b', 'Predicted aircraft glow on the map. '),
        `When no position has arrived for ${START} s, AirView keeps drawing the aircraft where its tracking filter expects it to be: with a soft glow, an uncertainty circle and fading opacity. Positions normally arrive twice a second, so short gaps are ordinary reception jitter. Predicted positions are only drawn - they never enter trails, the database, statistics or records.`)),
    timeline,
    h('div.set-fields.set-fields--2', coastF, pathF),
    F.toggle('prediction.showPaths', { label: 'Show predicted paths', hint: 'The Live Map draws the predicted path of the selected aircraft, with where it is expected to drop below or rise above the terrain horizon.' }));
  paintTimeline();

  // ---- status ----
  const statusHost = h('div');
  S.panel(statusHost, (signal) => getOrOff(api, '/api/prediction/stats', signal), (r) => {
    if (r.disabled) return ui.empty({ icon: 'target', compact: true, title: 'Prediction is off', text: 'Switch it on and save to see the tracking filter here.' });
    if (!r.tracks) return ui.empty({ icon: 'plane', compact: true, title: 'Not enough data yet', text: 'No aircraft is being tracked right now.' });
    const row = (hz) => (r.horizons || []).find((x) => x.horizon === hz && x.predictor === 'imm' && x.phase === 'all' && x.n > 0);
    const skill = (hz) => (r.skill || []).find((x) => x.horizon === hz && x.phase === 'all' && x.n > 0);
    const err = (hz) => { const x = row(hz); return x ? `${units.height(x.medianKm * 1000)} median · ${units.height(x.p95Km * 1000)} p95 (${units.int(x.n)} checks)` : 'not enough data yet'; };
    const sk = skill(60) || skill(30);
    return [
      ui.kv([
        ['Tracked aircraft', `${units.int(r.tracks)}${isNum(r.ready) ? ` · ${units.int(r.ready)} ready` : ''}`],
        ['Coasting right now', units.int(r.coasting)],
        ['Prediction error after 15 s', err(15)],
        ['Prediction error after 60 s', err(60)],
        ['Better than dead reckoning', sk ? `${sk.skillPct >= 0 ? '+' : ''}${units.pct(sk.skillPct)} at ${sk.horizon} s` : 'not enough data yet'],
      ]),
      h('div.set-help', 'The filter scores itself continuously against the positions that arrive later.'),
    ];
  }, 15000);

  return [
    ui.card({ title: 'Trajectory prediction', icon: 'target', body: [
      F.toggle('prediction.enabled', { label: 'Track and predict aircraft', hint: 'Runs a tracking filter on every position (a fraction of a microsecond per frame, fine on a Raspberry Pi). Off: no coasting, no predicted paths, no closest-approach figures.', onChange: paintTimeline }),
      body,
    ] }),
    ui.card({ title: 'Live status', icon: 'signal', body: statusHost }),
  ];
}

// =============================================================================================
// Terrain
// =============================================================================================

const TILE_MB_450 = 36;            // measured: about 36 MB of elevation tiles for 450 km at automatic zoom

function renderTerrain(S, F) {
  const { ui, api, units } = S.ctx;
  const { h } = ui;
  const scope = S.scope;

  const sizeEl = h('span.num');
  const paintSize = () => {
    const r = S.get('terrain.radiusKm') || 450;
    ui.setText(sizeEl, `≈ ${units.bytes(TILE_MB_450 * 1e6 * (r / 450) ** 2)} of elevation tiles at automatic zoom, downloaded once`);
  };
  const radiusF = sliderField(S, F, 'terrain.radiusKm', {
    label: 'Analysis radius', min: 50, max: 1000, step: 10, format: (v) => units.dist(v, { digits: 0 }), onInput: paintSize,
    hint: 'How far around the station terrain is analysed. Radio line of sight rarely reaches beyond 450 km.',
  });
  radiusF.append(h('div.set-help', sizeEl));
  paintSize();

  const lat = S.get('receiver.lat') || 0;
  const res = (z) => units.height((156543.03 * Math.cos((lat * Math.PI) / 180)) / 2 ** z, { digits: 0 });
  const zoomF = F.select('terrain.zoom', {
    label: 'Elevation tile zoom', numeric: true,
    options: [{ value: 0, label: 'Automatic (recommended)' }, ...[6, 7, 8, 9, 10, 11, 12].map((z) => ({ value: z, label: `Zoom ${z} · ≈ ${res(z)} per pixel` }))],
    hint: 'Automatic picks the detail from the radius. Higher zoom = finer terrain, many more tiles (each step quadruples the download).',
  });

  const kF = F.number('terrain.kFactor', { label: 'Refraction k-factor', min: 0.5, max: 10, step: 0.01, digits: 3, hint: 'Effective earth radius factor for radio line of sight (0.5 - 10). 4/3 is the standard atmosphere.' });
  const kPick = (v, label, tip) => ui.button({ label, size: 'sm', variant: 'ghost', tip, onClick: () => { S.set('terrain.kFactor', v); kF.show(v); } });
  const kPicks = h('div.set-chips', h('span.muted', 'Presets:'),
    kPick(4 / 3, '4/3 standard', 'Standard atmosphere: radio waves bend slightly towards the ground'),
    kPick(1, '1.0 geometric', 'No refraction: the purely geometric horizon'),
    kPick(2 / 3, '2/3 conservative', 'Sub-refraction: a pessimistic planning value'));

  // ---- status / prefetch ----
  const prefNote = savedOnlyNote(S, ['terrain'], 'Prefetch uses the saved settings - save your changes first.');
  let running = false;
  const PHASE = { tiles: 'Downloading elevation tiles', terrain: 'Sampling the terrain', coverage: 'Computing the coverage' };
  const statusHost = h('div');
  const reload = adaptivePanel(S, statusHost, (signal) => api.get('/api/terrain/status', { signal }), (st) => {
    const job = st.job || {};
    running = job.state === 'running';
    paintButtons(st);
    const out = [];
    if (!st.enabled) out.push(ui.empty({ icon: 'mountain', compact: true, title: 'Terrain analysis is off', text: 'No tiles are downloaded and no terrain line of sight is computed.' }));
    else if (!st.located) out.push(ui.empty({ icon: 'pin', compact: true, title: 'Waiting for the station position', text: 'Terrain is analysed around the station - set the location under Station location first.' }));
    if (running) {
      const t = job.tiles || {};
      out.push(h('div.set-progress',
        h('div.row', h('span.spinner'), h('b', PHASE[job.phase] || job.message || 'Working…'), h('span.spacer'), h('span.num.muted', units.pct((job.progress || 0) * 100))),
        ui.meter((job.progress || 0) * 100),
        h('div.set-help.num', job.phase === 'tiles' && t.total
          ? `${units.int(t.done)} of ${units.int(t.total)} tiles · ${units.int(t.downloaded)} downloaded (${units.bytes(t.bytes)}) · ${units.int(t.cached)} cached${t.failed ? ` · ${units.int(t.failed)} failed` : ''}${t.offline ? ' · offline, retrying later' : ''}`
          : `${job.message || ''} · ${units.pct((job.phaseProgress || 0) * 100)} of this step`)));
    } else if (job.state === 'failed' || job.error) {
      out.push(h('div.set-result.bad', ui.icon('x-circle'), h('div', h('b', 'The last terrain analysis failed. '), job.error || job.message || '')));
    } else if (job.state === 'cancelled') {
      out.push(h('div.set-result.bad', ui.icon('x-circle'), h('div', h('b', 'The last run was cancelled. '), st.available ? 'The previous result is still used.' : '')));
    }
    if (st.available && !running) {
      out.push(h('div.set-result.ok', ui.icon('check-circle'), h('div', h('b', 'Terrain analysis available'), ` · computed ${units.ago(st.computedMs)}`,
        st.dataComplete ? '' : h('div.set-help', 'Some elevation tiles were missing and were read as sea level - press "Re-run" when the internet connection is back.'))));
    } else if (st.enabled && st.located && !running && !st.available && job.state !== 'failed') {
      out.push(ui.empty({ icon: 'mountain', compact: true, title: 'Not computed yet', text: st.autoPrefetch ? 'It starts automatically in a moment.' : 'Automatic prefetch is off - press "Prefetch now".' }));
    }
    const p = st.provider;
    if (p) {
      const pct = p.diskCapBytes ? (p.diskBytes / p.diskCapBytes) * 100 : 0;
      out.push(ui.section('Tile cache', 'database'),
        h('div.set-meter-row', ui.meter(pct), h('span.num', `${units.bytes(p.diskBytes)} of ${units.bytes(p.diskCapBytes)}`)),
        ui.kv([
          ['Tiles on disk', `${units.int(p.diskTiles)}${p.zoom ? ` · zoom ${p.zoom}` : ''}`],
          ['In memory', `${units.int(p.memTiles)} of ${units.int(p.memCapacity)} tiles · ${units.bytes(p.memBytes)}`],
          ['Downloaded since start', `${units.int(p.downloads)} tiles · ${units.bytes(p.downloadBytes)}${p.failures ? ` · ${units.int(p.failures)} failures` : ''}`],
          ['Tile server', p.offline ? ui.badge(`unreachable - retry ${p.offlineUntilMs ? units.clock(p.offlineUntilMs, { utc: false }) : 'later'}`, 'amber') : ui.badge('reachable', 'green')],
          ['Folder', p.dir || '—', { text: true, tip: p.dir }],
        ]));
    }
    if (st.attribution) out.push(h('p.set-licence', h('span.label', 'Data'), ' ', st.attributionUrl ? extLink(h, st.attributionUrl, st.attribution) : st.attribution));
    return out;
  }, () => running, 1000, 15000);

  const prefetchBtn = ui.button({ label: 'Prefetch now', icon: 'download', variant: 'primary', size: 'sm', onClick: () => terrainPost('/api/terrain/prefetch', prefetchBtn) });
  const rerunBtn = ui.button({ label: 'Re-run', icon: 'refresh', size: 'sm', tip: 'Run the analysis again ignoring the result cache (also retries missing tiles)', onClick: () => terrainPost('/api/terrain/prefetch', rerunBtn, { force: 1 }) });
  const cancelBtn = ui.button({ label: 'Cancel', icon: 'x', size: 'sm', variant: 'ghost', onClick: () => terrainPost('/api/terrain/cancel', cancelBtn) });
  function paintButtons(st) {
    const can = st.enabled && st.located;
    prefetchBtn.classList.toggle('hidden', running);
    rerunBtn.classList.toggle('hidden', running || !st.available);
    cancelBtn.classList.toggle('hidden', !running);
    prefetchBtn.disabled = !can;
    rerunBtn.disabled = !can;
  }
  async function terrainPost(path, btn, params) {
    btn.setLoading(true);
    try {
      const r = await S.admin(() => api.post(path, undefined, { params, signal: scope.signal, timeout: 30000 }));
      if (path.endsWith('/cancel')) ui.toast.info('Terrain job cancelled.');
      else if (r && r.started === false && r.job?.state !== 'running') ui.toast.info('The terrain analysis is already up to date.');
      running = true;
    } catch (e) {
      if (e.aborted || e.cancelled) return;
      if (e.status === 409) ui.toast.warn(e.message, { title: 'Terrain' }); else ui.toast.error('Terrain request failed: ' + e.message);
    } finally {
      btn.setLoading(false);
      reload({ quiet: true });
    }
  }

  return [
    ui.card({ title: 'Terrain line of sight', icon: 'mountain', body: [
      F.help('AirView downloads elevation tiles around the station and computes, per bearing, how low an aircraft can be and still be seen over the hills. That drives the theoretical coverage and coverage-floor layers of the Live Map, the terrain horizon of the Antenna Lab and the "visible / behind terrain" figure of each aircraft.'),
      h('div.set-fields.set-fields--2',
        F.toggle('terrain.enabled', { label: 'Terrain analysis', hint: 'Off: no tiles are downloaded, terrain layers and figures disappear.' }),
        F.toggle('terrain.autoPrefetch', { label: 'Download automatically', hint: 'Fetch the tiles and run the analysis on its own once the station is located, and again after the position, antenna heights, k-factor or radius change. Off: only when you press "Prefetch now".' }),
        radiusF, zoomF,
        h('div.set-group', kF, kPicks)),
    ] }),
    ui.card({ title: 'Status and tile cache', icon: 'download', actions: h('div.row', cancelBtn, rerunBtn, prefetchBtn), body: [prefNote.el, statusHost] }),
  ];
}

// =============================================================================================
// Aviation data (navdata)
// =============================================================================================

const OURAIRPORTS = ['countries', 'airports', 'runways', 'navaids'];
const DS_LABEL = { countries: 'Countries', airports: 'Airports', runways: 'Runways', navaids: 'Navaids', routes: 'Routes (standing data)' };
const STAGE_LABEL = { download: 'Downloading', import: 'Importing', unchanged: 'Unchanged on the server', done: 'Done', error: 'Failed' };

function renderNavData(S, F) {
  const { ui, api, units } = S.ctx;
  const { h } = ui;
  const scope = S.scope;
  let running = false;
  let lastStatus = null;

  const progHost = h('div');
  const dataHost = h('div');
  const force = ui.checkbox({ label: 'Download again even when unchanged' });

  const reload = adaptivePanel(S, dataHost, (signal) => api.get('/api/navdata/status', { signal }), (st) => {
    lastStatus = st;
    running = !!st.import?.running;
    paintProgress(st.import);
    const byDs = new Map((st.datasets || []).map((d) => [d.dataset, d]));
    const sources = st.sources || [];
    const src = (ds) => sources.find((s) => s.dataset === ds) || byDs.get(ds)?.source || null;
    const group = (title, list, blurb) => {
      const rows = list.map((ds) => byDs.get(ds) || { dataset: ds, rows: 0 });
      const have = rows.some((d) => d.rows > 0);
      const s0 = src(list[0]) || {};
      const bytes = list.reduce((a, ds) => a + (src(ds)?.approxBytes || 0), 0);
      return h('div.set-source',
        h('div.set-source-head', h('b', title), ui.badge(`~${units.bytes(bytes)}`, 'blue'),
          have ? ui.badge('imported', 'green') : ui.badge('not imported', 'other')),
        h('p', blurb),
        ui.table({ columns: [
          { key: 'dataset', label: 'Dataset', render: (d) => DS_LABEL[d.dataset] || d.dataset },
          { key: 'rows', label: 'Rows', align: 'right', num: true, render: (d) => (d.rows ? units.int(d.rows) : '—') },
          { key: 'imported', label: 'Imported', render: (d) => (d.imported ? units.ago(d.imported) : 'never') },
        ], rows }),
        h('p.set-licence', h('span.label', 'Licence'), ' ', s0.licenceUrl ? extLink(h, s0.licenceUrl, s0.licence) : (s0.licence || '—')),
        h('p.set-licence', h('span.label', 'Attribution'), ' ', s0.attribution || '—'),
        h('div.row-wrap',
          ui.button({ label: have ? 'Update' : 'Download and import', icon: 'download', variant: have ? undefined : 'primary', size: 'sm', disabled: running, onClick: () => startImport(title, list, bytes, s0) }),
          s0.homepage ? h('a.set-link', { href: s0.homepage, target: '_blank', rel: 'noopener noreferrer' }, ui.icon('external'), 'Project page') : null));
    };
    return [
      h('div.set-sources',
        group('OurAirports', OURAIRPORTS, 'Airports, runways, navaids and countries for the airports overlay, airport details and departure / arrival guesses.'),
        group('Flight routes', ['routes'], 'Virtual Radar Server standing data: callsign → route table, used offline to show where a flight comes from and goes to.')),
      ui.kv([
        ['Airports near the station', st.regionAirports ? `${units.int(st.regionAirports)} loaded for the live picture` : 'none (import OurAirports first)'],
      ]),
      (st.attribution || []).length ? h('p.set-licence', h('span.label', 'Shown on the map'), ' ', st.attribution.join(' · ')) : null,
    ];
  }, () => running, 1000, 30000);

  function paintProgress(imp) {
    if (!imp) { ui.mount(progHost); return; }
    const p = imp.progress;
    if (imp.running) {
      const pct = isNum(imp.fraction) ? imp.fraction * 100 : null;
      ui.mount(progHost, h('div.set-progress',
        h('div.row', h('span.spinner'),
          h('b', p ? `${STAGE_LABEL[p.stage] || p.stage} ${DS_LABEL[p.dataset] || p.dataset || ''}` : 'Starting…'),
          p && p.count ? h('span.muted.num', ` · ${p.index} of ${p.count}`) : null,
          h('span.spacer'),
          ui.button({ label: 'Cancel', icon: 'x', size: 'sm', variant: 'ghost', onClick: cancelImport })),
        pct === null ? h('div.meter.set-meter-busy', h('i')) : ui.meter(pct),
        p ? h('div.set-help.num', `${units.bytes(p.bytesRead)}${p.bytesTotal > 0 ? ' of ' + units.bytes(p.bytesTotal) : ''} · ${units.int(p.records)} records${p.skipped ? ` · ${units.int(p.skipped)} skipped` : ''}`) : null));
      return;
    }
    const res = imp.result;
    if (!res) { ui.mount(progHost); return; }
    const failed = (res.datasets || []).filter((d) => d.error);
    const ok = (res.datasets || []).filter((d) => !d.error);
    ui.mount(progHost, h('div.set-result' + (failed.length || res.cancelled ? '.bad' : '.ok'), ui.icon(failed.length || res.cancelled ? 'x-circle' : 'check-circle'), h('div',
      h('b', res.cancelled ? 'The last import was cancelled. ' : failed.length ? 'The last import finished with errors. ' : 'The last import finished. '),
      res.finished ? units.ago(res.finished) : '',
      h('ul.set-problem-list',
        ok.map((d) => h('li', `${DS_LABEL[d.dataset] || d.dataset}: ${units.int(d.records)} records${d.skipped ? `, ${units.int(d.skipped)} skipped` : ''} in ${units.duration(d.elapsedMs)}`)),
        failed.map((d) => h('li', `${DS_LABEL[d.dataset] || d.dataset}: ${d.error}`))),
      failed.length ? h('div.set-help', 'A failed or cancelled dataset keeps its previous contents.') : null)));
  }

  async function startImport(title, datasets, bytes, s0) {
    const ok = await ui.confirm({ title: `Import ${title}?`, confirmLabel: 'Download and import',
      message: h('div.col.gap-2',
        h('p', `AirView downloads about ${units.bytes(bytes)} from ${s0.url ? new URL(s0.url).hostname : 'the source'} and imports it in the background. This needs the internet; the live picture keeps running.`),
        h('p.set-licence', h('span.label', 'Licence'), ' ', s0.licence || '—'),
        h('p.set-licence', h('span.label', 'Attribution'), ' ', s0.attribution || '—')) });
    if (!ok) return;
    try {
      const r = await S.admin(() => api.post('/api/navdata/import', { datasets, force: !!force.checked }, { signal: scope.signal }));
      running = true;
      paintProgress(r.import);
    } catch (e) {
      if (e.aborted || e.cancelled) return;
      if (e.status === 409) { ui.toast.warn('An import is already running.'); running = true; } else ui.toast.error('Import could not be started: ' + e.message);
    }
    reload({ quiet: true });
  }
  async function cancelImport() {
    try {
      const r = await S.admin(() => api.post('/api/navdata/cancel', undefined, { signal: scope.signal }));
      paintProgress(r.import);
    } catch (e) { if (!e.aborted && !e.cancelled) ui.toast.error('Cancel failed: ' + e.message); }
    reload({ quiet: true });
  }

  // ---- online services ----
  const contactF = F.text('navData.photoContact', { label: 'Contact for planespotters.net', placeholder: 'you@example.org  or  https://your-site.example', maxLength: 200,
    hint: 'planespotters.net requires every client to identify itself; AirView sends this in the User-Agent of photo requests. Note: everyone who can open this AirView\'s web interface can read it in the configuration.' });
  const paintContact = () => contactF.classList.toggle('set-dim', !S.get('navData.photos'));
  paintContact();
  const clearBtn = ui.button({ label: 'Forget looked-up routes', icon: 'trash', size: 'sm', variant: 'ghost', tip: 'Clear the cache of online route look-ups', onClick: async () => {
    clearBtn.setLoading(true);
    try { await S.admin(() => api.post('/api/navdata/route-cache/clear', undefined, { signal: scope.signal })); ui.toast.success('Route cache cleared.'); }
    catch (e) { if (!e.aborted && !e.cancelled) ui.toast.error('Could not clear the cache: ' + e.message); }
    finally { clearBtn.setLoading(false); }
  } });

  return [
    ui.card({ title: 'Offline reference data', icon: 'database', body: [
      F.help('Airports, runways, navaids and the Virtual Radar Server route table (CC0 / public domain, about 25 MB; no data about your station is sent) are downloaded at first start and checked once a day; an unchanged file is not downloaded again. Switch the automatic update off for an air-gapped station and import by hand below; afterwards airports and routes work without the internet.'),
      F.toggle('navData.autoRefresh', { label: 'Download and update reference data automatically', hint: 'Daily check at a random time of day for this station (conditional download).' }),
      progHost,
      dataHost,
      force,
    ] }),
    ui.card({ title: 'Map', icon: 'map', body: [
      F.toggle('navData.airportsOverlay', { label: 'Airports layer on the Live Map', hint: 'Offers an airports / runways overlay (needs the OurAirports import).' }),
    ] }),
    ui.card({ title: 'Routes', icon: 'route', body: [
      h('div.set-fields.set-fields--2',
        F.toggle('navData.routes.enabled', { label: 'Show routes', hint: 'Origin and destination from your own overrides, routes AirView learned from take-offs and landings it saw, and the standing data, each checked against the live track.' }),
        F.toggle('navData.routes.learn', { label: 'Learn routes', hint: 'Learns routes from take-offs and landings this station observes (kept locally; exportable as CC0).' }),
        F.toggle('navData.routes.eta', { label: 'Estimate arrival times', hint: 'Shows progress and an ETA with its uncertainty for routes that fit the track.' })),
      h('p.set-licence', h('span.label', 'Overrides'), ' Corrections per callsign: API ', h('code', '/api/routes/overrides'), ' or the file ', h('code', 'routes.overrides.csv'), ' (callsign,airports,mode,note) in the data folder.'),
      h('p.set-licence', h('span.label', 'Learned routes'), ' ', h('a', { href: '/api/routes/export/learned.csv' }, 'Export (VRS format, CC0)'), ' - submit confident corrections to the ', extLink(h, 'https://sdm.virtualradarserver.co.uk/', 'VRS standing data'), '.'),
    ] }),
    ui.card({ title: 'Online look-ups', icon: 'globe', actions: clearBtn, body: [
      F.callout('warn', 'shield', h('b', 'Privacy: these options send data to third parties. '),
        'When online route look-up is on (the default), the callsign of each airline aircraft this station receives that has no offline route is sent to adsbdb.com, at most once a day per callsign and at most 1000 per day. The receiver location is never sent. Answers are cached and shown only to this station\'s own users (adsbdb\'s terms forbid republishing them). Switch it off below to keep route look-ups local. Aircraft photos send the ICAO hex address (and registration) of each aircraft you open to planespotters.net, together with your contact. Both requests come from the AirView computer, so these services also see its public IP address and can infer roughly where your station is and what it receives. Leave both off to keep everything local.'),
      h('div.set-fields.set-fields--2',
        F.toggle('navData.routes.online', { label: 'Look up unknown routes online', hint: 'Sends unknown airline callsigns to adsbdb.com (on by default). Results are cached for a day.' }),
        F.toggle('navData.photos', { label: 'Aircraft photos', hint: 'Shows a thumbnail from planespotters.net in the aircraft details, with the photographer credit and a link to the photo.', onChange: paintContact }),
        h('div'), contactF),
      h('p.set-licence', h('span.label', 'Routes'), ' ', extLink(h, 'https://www.adsbdb.com/', 'adsbdb.com'), ' - free aircraft and callsign API, used as-is.'),
      h('p.set-licence', h('span.label', 'Photos'), ' ', extLink(h, 'https://www.planespotters.net/', 'planespotters.net'), ' - every photo remains © its photographer and is shown with credit and link as their API terms require.'),
    ] }),
  ];
}

// =============================================================================================
// Alerts
// =============================================================================================

function renderAlerts(S, F) {
  const { ui, api, units } = S.ctx;
  const { h } = ui;
  const statsHost = h('div');
  S.panel(statsHost, (signal) => api.get('/api/alerts/stats', { signal }), (r) => {
    if (!S.saved.alerts?.enabled) return ui.empty({ icon: 'bell', compact: true, title: 'Alerts are off', text: 'No rule is evaluated. Switch alerts on and save.' });
    if (!r.evaluations) return ui.empty({ icon: 'bell', compact: true, title: 'Not enough data yet', text: 'The rules have not been evaluated yet.' });
    return ui.kv([
      ['Rules', `${units.int(r.enabledRules)} enabled of ${units.int(r.rules)}`],
      ['Alerts fired', `${units.int(r.fired)}${r.suppressed ? ` · ${units.int(r.suppressed)} repeats suppressed` : ''}`],
      ['Webhooks', `${units.int(r.webhookSent)} sent${r.webhookFailed ? ` · ${units.int(r.webhookFailed)} failed` : ''}${r.webhookDropped ? ` · ${units.int(r.webhookDropped)} dropped` : ''}`],
      ['Last evaluation', isNum(r.lastEvalMicros) && r.lastEvalAircraft ? `${units.int(r.lastEvalAircraft)} aircraft in ${r.lastEvalMicros < 1 ? '< 1 µs' : r.lastEvalMicros < 1000 ? `${units.int(r.lastEvalMicros)} µs` : `${units.num(r.lastEvalMicros / 1000, 1)} ms`}` : '—'],
    ]);
  }, 15000);
  return [
    ui.card({ title: 'Alert engine', icon: 'bell', actions: ui.button({ label: 'Rules and alerts', icon: 'chevron-right', size: 'sm', href: '#/alerts' }), body: [
      F.help('Alert rules watch the live picture once a second: squawks, military or watch-listed aircraft, geofences, distance, records, feed or NTP problems. Rules, watch-lists and the alert history are managed on the Alerts page.'),
      h('div.set-fields.set-fields--2',
        F.toggle('alerts.enabled', { label: 'Evaluate alert rules', hint: 'Off: no rule is checked, nothing is highlighted, no notification is sent.' }),
        F.toggle('alerts.sound', { label: 'Sound', hint: 'Open AirView pages may play a short tone for alerts whose rule asks for it.' }),
        F.toggle('alerts.webhooks', { label: 'Allow webhooks', hint: 'Rules may POST to their webhook URL (Discord, ntfy, Home Assistant …). Off: no webhook leaves this computer, whatever the rules say.' })),
      h('a.set-link', { href: '#/alerts' }, ui.icon('bell'), 'Open the Alerts page to edit rules, watch-lists and test webhooks'),
    ] }),
    ui.card({ title: 'Engine status', icon: 'signal', body: statsHost }),
  ];
}

// =============================================================================================
// RF statistics
// =============================================================================================

function renderRF(S, F) {
  const { ui, api, units } = S.ctx;
  const { h } = ui;
  const host = h('div');
  S.panel(host, (signal) => getOrOff(api, '/api/rf/summary', signal), (r) => {
    if (r.disabled) return ui.empty({ icon: 'wave', compact: true, title: 'RF statistics are off', text: 'Switch them on and save to fill the RF Lab.' });
    if (!r.windowSec || !isNum(r.fps)) return ui.empty({ icon: 'wave', compact: true, title: 'Not enough data yet', text: 'The statistics window is still filling.' });
    const kind = r.healthStatus === 'ok' ? 'green' : r.healthStatus === 'bad' ? 'red' : 'amber';
    return [
      ui.kv([
        ['RF health', isNum(r.health) ? ui.badge(`${units.int(r.health)} / 100 · ${r.healthStatus || ''}`, kind) : '—'],
        ['Frame rate', units.rate(r.fps)],
        ['Channel occupancy', isNum(r.occupancyPct) ? `${units.pct(r.occupancyPct, 2)} (peak ${units.pct(r.peakOccupancyPct, 2)})` : '—'],
        ['Decode floor / noise estimate', `${units.dbfs(r.decodeFloorDbfs)} / ${units.dbfs(r.noiseFloorEstDbfs)}`],
        ['Statistics window', units.age(r.windowSec)],
      ]),
      (r.advice || []).length ? h('ul.set-problem-list', r.advice.map((a) => h('li', a))) : null,
    ];
  }, 15000);
  return [
    ui.card({ title: 'RF and protocol statistics', icon: 'wave', actions: ui.button({ label: 'RF Lab', icon: 'chevron-right', size: 'sm', href: '#/rf' }), body: [
      F.help('Analyses every received frame: signal levels, decode floor, channel occupancy, frame intervals, interrogating radars, timing and GNSS-integrity figures. It shares the frame path with the tracking filter and costs a fraction of a microsecond per frame.'),
      F.toggle('rfStat.enabled', { label: 'Collect RF statistics', hint: 'Off: the RF Lab page and the RF health figures are empty.' }),
    ] }),
    ui.card({ title: 'Live summary', icon: 'signal', body: host }),
  ];
}
