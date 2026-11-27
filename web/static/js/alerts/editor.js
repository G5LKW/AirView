// alerts/editor.js - the rule editor drawer: name / severity, a condition builder (nested AND / OR groups,
// NOT on any node, every leaf type the API supports, geofences drawn on a small map), actions (notice,
// sound, map highlight colour, webhook), cooldowns, message template, and a dry run against the live picture.

import {
  CONDS, COND_GROUPS, SEVERITIES, SYSTEM_EVENTS, condDef, isGroup, newLeaf, toDraft, fromDraft, prepareTree,
  validate, describe, isSystemRule, splitValues, clone, round, usesPosition,
} from './model.js';
import { createGeofenceMap } from './geofence.js';
import { play as playSound, unlock as unlockSound } from '../alertsound.js';

const UNIT_KINDS = new Set(['alt', 'speed', 'vrate', 'dist']);
const DEFAULT_HIGHLIGHT = '#ffd60a';

/**
 * Open the editor.
 * @param ctx page context
 * @param {{ rule: object|null, isNew: boolean, admin, watchlists: string[], placeholders: string[],
 *           onSaved(rule), onDeleted(id) }} o
 */
export function openEditor(ctx, o) {
  const { ui, units, api, config } = ctx;
  const { h } = ui;
  const draft = toDraft(o.rule || { name: '', enabled: true, severity: 'info', cooldownSec: 900, match: { op: 'and', conds: [] }, actions: { notify: true, sound: false, log: true } });
  prepareTree(draft.match);
  const original = JSON.stringify(fromDraft(draft));
  const rx = config.current?.receiver;
  const ids = new WeakMap();
  let nextId = 1;
  const idOf = (c) => { let id = ids.get(c); if (!id) { id = 'c' + nextId++; ids.set(c, id); } return id; };
  const errEls = new Map();              // cond id -> error element
  const geoBlocks = new Map();           // cond id -> { el, map, cond }
  let showErrors = false;
  let busy = false;

  // ---------------------------------------------------------------------------------------------
  // header fields
  // ---------------------------------------------------------------------------------------------
  const serverErr = h('div.alx-callout.alx-callout--bad.hidden', { role: 'alert' });
  const nameIn = ui.input({ value: draft.name, placeholder: 'e.g. Heavies over the house', maxlength: 100, onInput: (v) => { draft.name = v; changed(); } });
  const nameF = ui.field({ label: 'Name', control: nameIn });
  const descIn = ui.input({ value: draft.description || '', placeholder: 'Optional note for yourself', maxlength: 500, onInput: (v) => { draft.description = v; } });
  const sev = ui.segmented({ options: SEVERITIES.map((s) => ({ value: s.value, label: s.label })), value: draft.severity, onChange: (v) => { draft.severity = v; } });
  const enabled = ui.toggle({ label: 'Enabled', checked: draft.enabled, onChange: (v) => { draft.enabled = v; } });

  // ---------------------------------------------------------------------------------------------
  // condition tree
  // ---------------------------------------------------------------------------------------------
  const treeHost = h('div.alx-tree');
  const summaryEl = h('div.alx-summary');
  const rootErr = h('div.field-error.hidden');

  // Position conditions skip aircraft shown at a predicted (coasting) position unless this is on.
  const predT = ui.toggle({ label: 'Include predicted positions', checked: !!draft.includePredicted,
    tip: 'Coasting aircraft are drawn at the filter\'s predicted position when no real position arrived for more than 4 s',
    onChange: (v) => { draft.includePredicted = v; syncPred(); } });
  const predNote = h('div.alx-small.muted');
  const predRow = h('div.alx-pred.hidden', predT, predNote);
  function syncPred() {
    const pos = usesPosition(draft.match);
    predRow.classList.toggle('hidden', !pos && !draft.includePredicted);
    ui.setText(predNote, !pos ? 'Only used by altitude, distance, bearing and geofence conditions - this rule has none.'
      : draft.includePredicted ? 'Aircraft at a predicted (coasting) position can fire this rule too - it may fire on a position that is only estimated.'
        : 'Off (default): altitude, distance, bearing and geofence conditions ignore aircraft whose position is only predicted (coasting), so the rule fires on real positions only.');
  }

  function changed() {
    ui.setText(summaryEl, 'Fires when ' + describe(fromDraft(draft).match, units) + '.');
    if (showErrors) paintErrors();
    syncTestEvent();
    syncPred();
  }

  function paintErrors() {
    const probs = validate(draft, idOf);
    for (const [id, el] of errEls) {
      const p = probs.find((x) => x.path === 'cond:' + id);
      el.classList.toggle('hidden', !p);
      ui.setText(el, p ? p.msg : '');
    }
    const byPath = (p) => probs.find((x) => x.path === p)?.msg || null;
    nameF.setError(byPath('name'));
    msgF.setError(byPath('message'));
    cdF.setError(byPath('cooldown'));
    whF.setError(byPath('webhook'));
    const m = byPath('match');
    rootErr.classList.toggle('hidden', !m);
    ui.setText(rootErr, m || '');
    return probs;
  }

  function typeMenu(anchor, onPick) {
    const body = h('div.alx-typemenu', COND_GROUPS.map((g) => h('div.alx-typegroup',
      h('div.alx-typegroup-title', g),
      CONDS.filter((c) => c.group === g).map((c) => h('button.menu-item', { type: 'button', onclick: () => { pop.close(); onPick(c.type); } }, c.label)))));
    const pop = ui.popover(anchor, body, { placement: 'bottom-start', class: 'alx-typepop' });
  }

  function renderTree() {
    errEls.clear();
    const live = new Set();
    ui.mount(treeHost, renderGroup(draft.match, null, 0, live));
    for (const [id, b] of geoBlocks) if (!live.has(id)) { b.map?.remove(); geoBlocks.delete(id); }
    changed();
  }

  function renderGroup(g, parent, depth, live) {
    const id = idOf(g);
    const err = h('div.field-error.hidden');
    errEls.set(id, err);
    const opSeg = ui.segmented({ options: [{ value: 'and', label: 'ALL of' }, { value: 'or', label: 'ANY of' }], value: g.op === 'or' ? 'or' : 'and', onChange: (v) => { g.op = v; renderTree(); } });
    const not = ui.checkbox({ label: 'NOT', checked: !!g.negate, tip: 'Invert this group', onChange: (v) => { g.negate = v; changed(); } });
    const addBtn = ui.button({ label: 'Condition', icon: 'plus', size: 'sm', variant: 'ghost', onClick: () => typeMenu(addBtn, (type) => { g.conds.push(prepareTree(newLeaf(type, rx))); renderTree(); }) });
    const addGrp = depth < 6 ? ui.button({ label: 'Group', icon: 'plus', size: 'sm', variant: 'ghost', tip: 'Nested group (switch it to ANY for an OR)', onClick: () => { g.conds.push({ op: g.op === 'or' ? 'and' : 'or', conds: [] }); renderTree(); } }) : null;
    const rm = parent ? ui.iconButton('trash', 'Remove group', () => { parent.conds.splice(parent.conds.indexOf(g), 1); renderTree(); }, { variant: 'ghost', size: 'sm' }) : null;
    const kids = g.conds.map((c) => (isGroup(c) ? renderGroup(c, g, depth + 1, live) : renderLeaf(c, g, live)));
    return h('div.alx-group' + (depth ? '' : '.alx-group--root') + (g.op === 'or' ? '.is-or' : ''),
      h('div.alx-group-head', h('span.alx-group-label', depth ? 'Group' : 'Match'), opSeg, not, h('span.spacer'), addBtn, addGrp, rm),
      kids.length ? h('div.alx-group-body', kids) : h('div.alx-group-empty', 'No conditions yet - add one.'),
      err);
  }

  function renderLeaf(c, parent, live) {
    const id = idOf(c);
    live.add(id);
    const d = condDef(c.type);
    const err = h('div.field-error.hidden');
    errEls.set(id, err);
    const not = ui.checkbox({ label: 'NOT', checked: !!c.negate, tip: 'Invert this condition', onChange: (v) => { c.negate = v; changed(); } });
    const rm = ui.iconButton('trash', 'Remove condition', () => { parent.conds.splice(parent.conds.indexOf(c), 1); renderTree(); }, { variant: 'ghost', size: 'sm' });
    const body = d ? leafBody(c, d, id) : h('div.muted', `Unknown condition type "${c.type}" - kept as it is.`);
    return h('div.alx-leaf',
      h('div.alx-leaf-head', h('span.alx-leaf-type', d ? d.label : c.type), d?.hint ? h('span.alx-leaf-hint', d.hint) : null, h('span.spacer'), not, rm),
      body, err);
  }

  function chips(choices, selected, onToggle) {
    return h('div.alx-chips', choices.map((ch) => {
      const on = selected.includes(ch.value);
      const b = h('button.alx-chip', { type: 'button', 'aria-pressed': String(on) }, ch.label);
      b.addEventListener('click', () => { const now = b.getAttribute('aria-pressed') !== 'true'; b.setAttribute('aria-pressed', String(now)); onToggle(ch.value, now); });
      return b;
    }));
  }

  function leafBody(c, d, id) {
    switch (d.kind) {
      case 'values': {
        c.values = c.values || [];
        const inp = ui.input({ value: c.values.join(', '), placeholder: d.ph, mono: true, onInput: (v) => { c.values = splitValues(v); changed(); } });
        if (!d.choices) return inp;
        const ch = chips(d.choices, c.values, (v, on) => {
          c.values = on ? [...new Set([...c.values, v])] : c.values.filter((x) => x !== v);
          inp.value = c.values.join(', ');
          changed();
        });
        return h('div.col.gap-1', ch, inp);
      }
      case 'choice': {
        c.values = c.values || [];
        const choices = c.type === 'watchlist' ? (o.watchlists || []).map((n) => ({ value: n, label: n })) : d.choices || [];
        const known = new Set(choices.map((x) => x.value));
        const extra = () => c.values.filter((v) => !known.has(v));
        const free = d.free ? ui.input({ value: extra().join(', '), mono: true, size: 'sm', placeholder: c.type === 'watchlist' ? 'other list names' : 'record:max_dist*, custom:*', onInput: (v) => {
          c.values = [...c.values.filter((x) => known.has(x)), ...splitValues(v)];
          changed();
        } }) : null;
        const ch = choices.length ? chips(choices, c.values, (v, on) => {
          c.values = on ? [...new Set([...c.values, v])] : c.values.filter((x) => x !== v);
          changed();
        }) : h('div.muted.alx-small', c.type === 'watchlist' ? 'No watch-lists yet - create one under Watch-lists, or type a name.' : '');
        return h('div.col.gap-1', ch, free);
      }
      case 'bool': {
        return ui.segmented({ options: [{ value: true, label: cap(d.yes) }, { value: false, label: cap(d.no) }], value: c.value !== false, onChange: (v) => { c.value = v; changed(); } });
      }
      case 'range': {
        const conv = UNIT_KINDS.has(d.unit);
        const lab = conv ? units.label(d.unit) : d.unit;
        const show = (v) => (typeof v === 'number' ? (conv ? round(units.value(d.unit, v), d.unit === 'dist' ? 2 : 0) : v) : '');
        const back = (v) => (v === null || v === '' || !Number.isFinite(v) ? undefined : conv ? round(units.toBase(d.unit, v), d.unit === 'dist' ? 3 : 1) : v);
        const step = d.step ?? (conv ? 'any' : 1);
        const mn = ui.input({ type: 'number', value: show(c.min), placeholder: 'min', step, suffix: lab, size: 'sm', onInput: (v) => { c.min = back(v); changed(); } });
        const mx = ui.input({ type: 'number', value: show(c.max), placeholder: 'max', step, suffix: lab, size: 'sm', onInput: (v) => { c.max = back(v); changed(); } });
        return h('div.alx-range', h('span.alx-small.muted', 'from'), mn, h('span.alx-small.muted', 'to'), mx);
      }
      case 'bearing': {
        const f = ui.input({ type: 'number', value: c.from ?? '', min: 0, max: 360, step: 1, suffix: '°', size: 'sm', onInput: (v) => { c.from = v ?? undefined; changed(); } });
        const t = ui.input({ type: 'number', value: c.to ?? '', min: 0, max: 360, step: 1, suffix: '°', size: 'sm', onInput: (v) => { c.to = v ?? undefined; changed(); } });
        return h('div.col.gap-1', h('div.alx-range', h('span.alx-small.muted', 'from'), f, h('span.alx-small.muted', 'clockwise to'), t),
          h('div.alx-small.muted', 'Degrees true from the receiver; 300 → 60 wraps through north.'));
      }
      case 'geofence': return geofenceBlock(c, id);
      default: return null;
    }
  }

  function geofenceBlock(c, id) {
    const old = geoBlocks.get(id);
    if (old && old.cond === c) { setTimeout(() => old.map?.resize(), 0); return old.el; }
    const mapHost = h('div.alx-geomap');
    const mapState = h('div.alx-geomap-state', ui.loading('Loading map…', { compact: true }));
    const block = { el: null, map: null, cond: c };
    const radius = ui.input({ type: 'number', value: typeof c.radiusKm === 'number' ? round(units.value('dist', c.radiusKm), 2) : '', min: 0, step: 'any', size: 'sm', suffix: units.label('dist'),
      onInput: (v) => { c.radiusKm = v === null ? undefined : round(units.toBase('dist', v), 3); block.map?.refresh(); changed(); } });
    const centre = h('span.alx-small.num.muted');
    const pts = h('span.alx-small.num.muted');
    const paintInfo = () => {
      ui.setText(centre, typeof c.lat === 'number' ? 'centre ' + units.latlon(c.lat, c.lon, 4) : 'click the map to set the centre');
      ui.setText(pts, `${(c.polygon || []).length} points`);
    };
    const circleRow = h('div.alx-range', h('span.alx-small.muted', 'radius'), radius, centre,
      rx?.located ? ui.button({ label: 'Station', icon: 'locate', size: 'sm', variant: 'ghost', tip: 'Centre on the receiver', onClick: () => { c.lat = round(rx.lat, 5); c.lon = round(rx.lon, 5); block.map?.refresh(true); paintInfo(); changed(); } }) : null);
    const polyRow = h('div.alx-range', pts,
      ui.button({ label: 'Undo point', icon: 'history', size: 'sm', variant: 'ghost', onClick: () => { (c.polygon || []).pop(); block.map?.refresh(); paintInfo(); changed(); } }),
      ui.button({ label: 'Clear', icon: 'x', size: 'sm', variant: 'ghost', onClick: () => { c.polygon = []; block.map?.refresh(); paintInfo(); changed(); } }));
    const showRows = () => { circleRow.classList.toggle('hidden', c.shape === 'polygon'); polyRow.classList.toggle('hidden', c.shape !== 'polygon'); };
    const shape = ui.segmented({ options: [{ value: 'circle', label: 'Circle' }, { value: 'polygon', label: 'Polygon' }], value: c.shape || 'circle', onChange: (v) => {
      c.shape = v;
      if (v === 'circle' && typeof c.lat !== 'number') { const p = c.polygon?.[0]; c.lat = p ? p[1] : rx?.lat ?? 50; c.lon = p ? p[0] : rx?.lon ?? 8; c.radiusKm = c.radiusKm || 25; radius.value = round(units.value('dist', c.radiusKm), 2); }
      if (v === 'polygon' && !Array.isArray(c.polygon)) c.polygon = [];
      showRows(); paintInfo(); block.map?.refresh(true); changed();
    } });
    const inside = ui.segmented({ options: [{ value: true, label: 'Inside' }, { value: false, label: 'Outside' }], value: c.inside !== false, onChange: (v) => { c.inside = v; block.map?.refresh(); changed(); } });
    const hint = h('div.alx-small.muted', 'Click the map: a circle moves its centre there, a polygon gets a new point.');
    block.el = h('div.col.gap-2', h('div.row.row-wrap.gap-2', shape, inside), circleRow, polyRow, h('div.alx-geowrap', mapHost, mapState), hint);
    showRows();
    paintInfo();
    geoBlocks.set(id, block);
    createGeofenceMap(mapHost, ctx, c, () => { paintInfo(); changed(); }).then((m) => {
      if (!geoBlocks.has(id) || closed) { m.remove(); return; }
      block.map = m;
      mapState.remove();
    }).catch((e) => {
      console.error('[alerts] geofence map failed', e);
      ui.mount(mapState, ui.error(new Error('The map could not be started (' + (e?.message || 'WebGL unavailable') + '). Coordinates can still be typed via import/export.'), { compact: true }));
    });
    return block.el;
  }

  // ---------------------------------------------------------------------------------------------
  // actions, timing, message
  // ---------------------------------------------------------------------------------------------
  const A = draft.actions;
  const notify = ui.toggle({ label: 'Show a notice', checked: !!A.notify, onChange: (v) => { A.notify = v; } });
  const soundT = ui.toggle({ label: 'Play a sound', checked: !!A.sound, onChange: (v) => { A.sound = v; if (v) { unlockSound(); playSound(draft.severity); } } });
  const logT = ui.toggle({ label: 'Write to the event log', checked: A.log !== false, onChange: (v) => { A.log = v; } });
  const colorIn = h('input.alx-color', { type: 'color', value: /^#[0-9a-f]{6}$/i.test(A.highlight || '') ? A.highlight : DEFAULT_HIGHLIGHT, 'aria-label': 'Highlight colour' });
  const hlT = ui.toggle({ label: 'Highlight on the map', checked: !!A.highlight, onChange: (v) => { A.highlight = v ? colorIn.value : ''; colorIn.disabled = !v; } });
  colorIn.disabled = !A.highlight;
  colorIn.addEventListener('input', () => { if (hlT.checked) A.highlight = colorIn.value; });
  const whIn = ui.input({ value: A.webhook || '', placeholder: 'https://example.org/hook (optional)', mono: true, onInput: (v) => { A.webhook = v.trim(); if (showErrors) paintErrors(); } });
  const whTest = ui.button({ label: 'Send test', icon: 'bolt', size: 'sm', onClick: testWebhook });
  const whF = ui.field({ label: 'Webhook', control: h('div.alx-inline', whIn, whTest), hint: config.current?.alerts?.webhooks === false
    ? 'Webhooks are switched off in Settings - nothing is sent until they are enabled.'
    : 'AirView POSTs the alert as JSON (5 s timeout, no retries).' });

  async function testWebhook() {
    const url = (A.webhook || '').trim();
    if (!url) { whF.setError('Enter a URL first.'); return; }
    whF.setError(null);
    whTest.setLoading(true);
    try {
      await o.admin.run(() => api.post('/api/alerts/webhook-test', { url }, { timeout: 12000 }), 'Sending a test webhook needs the admin password.');
      ui.toast.success('The test POST was accepted by ' + url);
    } catch (e) {
      if (!e.cancelled) whF.setError(e.status === 409 ? 'Webhooks are switched off in Settings.' : 'Test failed: ' + e.message);
    } finally { whTest.setLoading(false); }
  }

  const cdIn = ui.input({ type: 'number', value: round(draft.cooldownSec / 60, 2), min: 0, step: 'any', size: 'sm', suffix: 'min', onInput: (v) => { draft.cooldownSec = v === null ? 0 : Math.round(v * 60); if (showErrors) paintErrors(); } });
  const cdF = ui.field({ label: 'Cooldown per aircraft', control: cdIn, hint: 'The same aircraft (or system event) fires again only after this pause.' });
  const rcdIn = ui.input({ type: 'number', value: draft.ruleCooldownSec ? round(draft.ruleCooldownSec / 60, 2) : '', min: 0, step: 'any', size: 'sm', suffix: 'min', placeholder: 'none', onInput: (v) => { draft.ruleCooldownSec = v ? Math.round(v * 60) : 0; } });
  const rcdF = ui.field({ label: 'Minimum gap for the whole rule', control: rcdIn, hint: 'Optional: at most one alert of this rule per period, whatever the aircraft.' });
  const msgIn = h('textarea.textarea.alx-msg', { rows: 2, maxlength: 500, placeholder: '{rule}: {ident} at {alt} ft, {dist} km' });
  msgIn.value = draft.message || '';
  msgIn.addEventListener('input', () => { draft.message = msgIn.value; if (showErrors) paintErrors(); });
  const insert = (ph) => {
    const s = msgIn.selectionStart ?? msgIn.value.length;
    const e = msgIn.selectionEnd ?? s;
    msgIn.value = msgIn.value.slice(0, s) + ph + msgIn.value.slice(e);
    msgIn.focus();
    msgIn.selectionStart = msgIn.selectionEnd = s + ph.length;
    draft.message = msgIn.value;
  };
  const phChips = h('div.alx-chips.alx-chips--sm', (o.placeholders || []).map((p) => h('button.alx-chip', { type: 'button', onclick: () => insert(p), tip: 'Insert' }, p)));
  const msgF = ui.field({ label: 'Message', control: h('div.col.gap-1', msgIn, phChips), hint: 'Empty = the default text. Numbers in the message use the server units (ft, kt, km).' });

  // ---------------------------------------------------------------------------------------------
  // dry run
  // ---------------------------------------------------------------------------------------------
  const testOut = h('div.alx-test-out');
  const evKind = ui.select({ options: SYSTEM_EVENTS.map((e) => ({ value: e.value, label: e.label })), value: 'record', size: 'sm' });
  const evKey = ui.input({ placeholder: 'key, e.g. max_dist_km', size: 'sm', mono: true });
  const evRow = h('div.alx-inline.hidden', h('span.alx-small.muted', 'Simulated event'), evKind, evKey);
  const testBtn = ui.button({ label: 'Test against the live picture', icon: 'play', onClick: runTest });
  function syncTestEvent() { evRow.classList.toggle('hidden', !isSystemRule(draft.match)); }

  async function runTest() {
    showErrors = true;
    const probs = paintErrors();
    if (probs.length) { ui.mount(testOut, h('div.alx-callout.alx-callout--warn', ui.icon('alert'), 'Fix the marked conditions first.')); return; }
    const body = { rule: { ...fromDraft(draft), enabled: true } };
    delete body.rule.id;
    if (isSystemRule(draft.match)) body.event = { kind: evKind.value, key: evKey.value.trim(), text: 'Test event' };
    testBtn.setLoading(true);
    ui.mount(testOut, ui.loading('Testing…', { compact: true }));
    try {
      const res = await o.admin.run(() => api.post('/api/alerts/test', body), 'Testing a rule needs the admin password.');
      const m = res?.matches || [];
      const head = h('div.alx-test-head', h('b', m.length ? `${units.int(m.length)} ${m.length === 1 ? 'match' : 'matches'}` : 'No match'),
        h('span.muted', ` among ${units.int(res?.aircraft ?? 0)} aircraft in the live picture`), m.length >= 500 ? h('span.muted', ' (first 500)') : null);
      ui.mount(testOut, head, m.length ? h('ul.alx-test-list', m.slice(0, 200).map((x) => h('li',
        x.icao ? h('a.alx-ident', { href: '#/map/' + x.icao, onclick: () => dr.close() }, x.callsign || x.reg || x.icao) : h('span.alx-ident', 'event'),
        x.icao ? h('span.num.muted', x.icao) : null, x.type ? h('span.muted', x.type) : null,
        h('span.alx-test-text', x.text || '')))) : h('div.alx-small.muted', 'Nothing in the current picture matches. The rule still fires as soon as an aircraft does.'));
    } catch (e) {
      ui.mount(testOut, e.cancelled ? h('div.muted.alx-small', 'Test cancelled - log in as admin to run it.') : ui.error(e, { compact: true, title: 'Test failed' }));
    } finally { testBtn.setLoading(false); }
  }

  // ---------------------------------------------------------------------------------------------
  // drawer
  // ---------------------------------------------------------------------------------------------
  const sec = (title, iconName, ...kids) => h('section.alx-sec', ui.section(title, iconName), ...kids);
  const body = h('div.alx-editor',
    serverErr,
    o.rule?.builtin ? h('div.alx-callout', ui.icon('info'), h('div', 'A built-in rule. You may change it; ', h('b', 'Reset built-ins'), ' on the Alerts page restores the shipped version.')) : null,
    sec('Rule', 'bell', nameF, ui.field({ label: 'Description', control: descIn }), h('div.row.row-wrap.gap-3', ui.field({ label: 'Severity', control: sev }), h('div.alx-enable', enabled))),
    sec('Conditions', 'filter', summaryEl, treeHost, rootErr, predRow),
    sec('Actions', 'bolt', h('div.alx-toggles', notify, h('div.alx-inline', soundT, ui.iconButton('play', 'Preview the sound', () => { unlockSound(); playSound(draft.severity); }, { variant: 'ghost', size: 'sm' })), logT, h('div.alx-inline', hlT, colorIn)), whF),
    sec('Timing & message', 'timer', h('div.grid.cols-2.alx-grid', cdF, rcdF), msgF),
    sec('Dry run', 'play', h('div.alx-small.muted', 'Evaluates the rule against every aircraft now in the live picture. Nothing fires and no cooldown starts.'), h('div.row.row-wrap.gap-2', testBtn, evRow), testOut));

  const delBtn = !o.isNew && !o.rule?.builtin ? ui.button({ label: 'Delete', icon: 'trash', variant: 'danger', onClick: del }) : null;
  const dupBtn = !o.isNew ? ui.button({ label: 'Duplicate', icon: 'copy', variant: 'ghost', onClick: () => {
    const copy = fromDraft(draft);
    delete copy.id;
    copy.name = (copy.name || 'Rule') + ' (copy)';
    dr.close();
    openEditor(ctx, { ...o, rule: copy, isNew: true });
  } }) : null;
  const saveBtn = ui.button({ label: o.isNew ? 'Create rule' : 'Save', icon: 'save', variant: 'primary', onClick: save });
  let closed = false;
  const dr = ui.drawer({
    title: o.isNew ? 'New alert rule' : 'Edit rule', icon: 'bell', width: 780, body,
    foot: [delBtn, dupBtn, h('span.spacer'), ui.button({ label: 'Cancel', variant: 'ghost', onClick: () => dr.close() }), saveBtn],
    onClose: () => { closed = true; for (const b of geoBlocks.values()) b.map?.remove(); geoBlocks.clear(); },
  });
  dr.el.classList.add('alx-drawer');
  renderTree();
  if (o.isNew && !draft.match.conds.length) setTimeout(() => nameIn.focus(), 60);

  function fail(e) {
    serverErr.classList.remove('hidden');
    ui.mount(serverErr, ui.icon('x-circle'), h('div', h('b', 'Not saved. '), e.status === 401 ? 'The admin password is required.' : e.network ? 'The AirView server cannot be reached.' : e.message));
    serverErr.scrollIntoView({ block: 'nearest', behavior: 'smooth' });
  }

  async function save() {
    if (busy) return;
    showErrors = true;
    const probs = paintErrors();
    if (probs.length) {
      ui.toast.warn(`${probs.length} ${probs.length === 1 ? 'item needs' : 'items need'} attention.`, { title: 'Not saved' });
      const first = probs[0].path.startsWith('cond:') ? errEls.get(probs[0].path.slice(5)) : null;
      (first || nameF).scrollIntoView({ block: 'center', behavior: 'smooth' });
      return;
    }
    const rule = fromDraft(draft);
    serverErr.classList.add('hidden');
    busy = true;
    saveBtn.setLoading(true);
    try {
      let stored;
      if (o.isNew) { delete rule.id; stored = await o.admin.run(() => api.post('/api/alerts/rules', rule), 'Saving alert rules needs the admin password.'); }
      else stored = await o.admin.run(() => api.put('/api/alerts/rules/' + encodeURIComponent(o.rule.id), rule), 'Saving alert rules needs the admin password.');
      ui.toast.success(o.isNew ? `Rule "${stored.name}" created.` : `Rule "${stored.name}" saved.`);
      o.onSaved?.(stored);
      dr.close();
    } catch (e) {
      if (!e.cancelled) fail(e);
    } finally { busy = false; saveBtn.setLoading(false); }
  }

  async function del() {
    const ok = await ui.confirm({ title: 'Delete rule?', message: `"${o.rule.name}" is removed for good. Alerts it already fired stay in the history.`, confirmLabel: 'Delete', danger: true });
    if (!ok) return;
    try {
      await o.admin.run(() => api.del('/api/alerts/rules/' + encodeURIComponent(o.rule.id)), 'Deleting alert rules needs the admin password.');
      ui.toast.success(`Rule "${o.rule.name}" deleted.`);
      o.onDeleted?.(o.rule.id);
      dr.close();
    } catch (e) { if (!e.cancelled) fail(e); }
  }

  return { close: () => dr.close(), isDirty: () => JSON.stringify(fromDraft(draft)) !== original, draft: () => clone(draft) };
}

function cap(s) { return s ? s[0].toUpperCase() + s.slice(1) : s; }
