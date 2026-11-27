// map/route-override.js - "Correct route…" dialog of the detail panel (docs/ROUTES-DESIGN.md §4, §6.3).
// PUT /api/routes/overrides/{callsign} {airports, mode, note, expires} and DELETE for "Remove"; the server
// validates again (internal/routes normOverride) and answers 409 for rows locked by routes.overrides.csv.
// Admin only (on a station: whoever may change the settings).

import { ui, h } from '../ui.js';

export const OVERRIDE_MODES = [
  { value: 'fixed', label: 'Fixed: always show this route' },
  { value: 'verify', label: 'Verify: show it only while the track fits' },
  { value: 'block', label: 'Block: never show a route for this callsign' },
];

/** Normalises an airport list ("egll kjfk", "EGLL-KJFK", "LHR,JFK") to "EGLL-KJFK"; null when invalid (2..8 codes of 3-4 letters/digits). */
export function normAirports(text) {
  const codes = String(text || '').toUpperCase().split(/[\s,;>\-–]+/).filter(Boolean);
  if (codes.length < 2 || codes.length > 8 || !codes.every((c) => /^[A-Z0-9]{3,4}$/.test(c))) return null;
  return codes.join('-');
}

/** Builds the request body; returns { body } or { error } (the text shown under the field). */
export function overrideBody({ mode, airports, note, days }) {
  const m = OVERRIDE_MODES.some((x) => x.value === mode) ? mode : 'fixed';
  const body = { mode: m, note: String(note || '').trim().slice(0, 200), expires: 0 };
  if (m !== 'block') {
    const a = normAirports(airports);
    if (!a) return { error: 'Enter 2 to 8 airport codes in flight order, e.g. EGLL-KJFK or LHR JFK.' };
    body.airports = a;
  }
  const d = Number(days);
  if (Number.isFinite(d) && d > 0) body.expires = Math.floor(Date.now() / 1000) + Math.round(d * 86400);
  return { body };
}

/** Prefill from the live route short (active leg, or the full stop list of a multi-stop route). */
export function prefillAirports(route) {
  if (!route) return '';
  if (Array.isArray(route.stops) && route.stops.length > 1) return route.stops.join('-');
  return route.from && route.to ? `${route.from}-${route.to}` : '';
}

/**
 * Opens the dialog. ctx: { api }. Resolves to 'saved' | 'removed' | undefined.
 */
export async function openRouteOverride(ctx, callsign, route) {
  const cs = String(callsign || '').trim().toUpperCase();
  if (!cs) return undefined;
  let existing = null;
  try {
    const list = await ctx.api.get('/api/routes/overrides', { timeout: 8000 });
    existing = (list || []).find((o) => o.callsign === cs) || null;
  } catch { /* the PUT will report a missing permission */ }
  const mode = ui.select({ options: OVERRIDE_MODES, value: existing?.mode || 'fixed' });
  const airports = ui.input({ value: existing?.airports || prefillAirports(route), mono: true, placeholder: 'EGLL-KJFK' });
  const note = ui.input({ value: existing?.note || '', placeholder: 'Why (seasonal change, charter, ...)', maxlength: 200 });
  const days = ui.input({ type: 'number', value: '', min: 0, step: 1, placeholder: 'never' });
  const err = h('div.field-error.hidden', { role: 'alert' });
  const apField = ui.field({ label: 'Airports in flight order', control: airports, hint: 'ICAO or IATA codes; 2 to 8 airports.' });
  const sync = () => apField.classList.toggle('hidden', mode.value === 'block');
  mode.addEventListener('change', sync);
  sync();
  const locked = !!existing?.locked;
  const body = h('div.col.gap-3',
    h('p.muted', `Overrides change what AirView shows for ${cs} on this station: they come first in the route resolution and are kept in an audit log.`),
    locked ? h('p.set-help', 'This override comes from routes.overrides.csv in the data directory and can only be changed there.') : null,
    ui.field({ label: 'Mode', control: mode }),
    apField,
    ui.field({ label: 'Note', control: note }),
    ui.field({ label: 'Expires after (days)', control: days, hint: 'Empty: keeps until you remove it.' }),
    err);
  const showErr = (t) => { err.textContent = t; err.classList.toggle('hidden', !t); };
  const save = async () => {
    const r = overrideBody({ mode: mode.value, airports: airports.value, note: note.value, days: days.value });
    if (r.error) { showErr(r.error); return false; }
    try {
      await ctx.api.put('/api/routes/overrides/' + encodeURIComponent(cs), r.body);
      ui.toast(`Route override for ${cs} saved.`);
      return true;
    } catch (e) {
      showErr(e.status === 403 || e.status === 401 ? 'Only an administrator can correct routes.' : e.message || 'Saving failed.');
      return false;
    }
  };
  const remove = async () => {
    try {
      await ctx.api.del('/api/routes/overrides/' + encodeURIComponent(cs));
      ui.toast(`Route override for ${cs} removed.`);
      return true;
    } catch (e) {
      showErr(e.message || 'Removing failed.');
      return false;
    }
  };
  const actions = [
    existing && !locked ? { label: 'Remove override', variant: 'danger', value: 'removed', onClick: remove } : null,
    { label: 'Cancel', variant: 'ghost', value: undefined },
    locked ? null : { label: 'Save', variant: 'primary', value: 'saved', onClick: save },
  ].filter(Boolean);
  return ui.modal({ title: `Correct route of ${cs}`, icon: 'route', body, actions, width: 460 }).closed;
}
