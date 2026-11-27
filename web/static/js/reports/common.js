// reports/common.js - small helpers shared by the Reports & Replay views: a per-view lifetime scope, UTC
// date-time input conversion and time-range presets. No page state lives here.

/** A lifetime for one view of the page: aborted / cleaned up when the view changes or the page unmounts. */
export function makeScope(ctx) {
  const ac = new AbortController();
  const cleanups = [];
  const scope = {
    signal: ac.signal,
    get alive() { return !ac.signal.aborted; },
    onCleanup(fn) { cleanups.push(fn); return fn; },
    every(ms, fn) { const t = setInterval(() => { if (!document.hidden && !ac.signal.aborted) fn(); }, ms); cleanups.push(() => clearInterval(t)); },
    subscribe(type, fn, opts) { cleanups.push(ctx.live.on(type, fn, opts)); },
    dispose() {
      if (ac.signal.aborted) return;
      ac.abort();
      for (const fn of cleanups.splice(0).reverse()) { try { fn(); } catch (e) { console.error('[reports] cleanup failed', e); } }
    },
  };
  return scope;
}

const pad = (n) => String(n).padStart(2, '0');

/** Unix ms -> "YYYY-MM-DDTHH:MM" in UTC (value of an <input type="datetime-local"> that the page labels UTC). */
export function toInputUTC(ms) {
  if (!Number.isFinite(ms)) return '';
  const d = new Date(ms);
  return `${d.getUTCFullYear()}-${pad(d.getUTCMonth() + 1)}-${pad(d.getUTCDate())}T${pad(d.getUTCHours())}:${pad(d.getUTCMinutes())}`;
}

/** "YYYY-MM-DDTHH:MM" read as UTC -> Unix ms, or null. */
export function fromInputUTC(s) {
  const m = /^(\d{4})-(\d{2})-(\d{2})T(\d{2}):(\d{2})/.exec(String(s || ''));
  if (!m) return null;
  const ms = Date.UTC(+m[1], +m[2] - 1, +m[3], +m[4], +m[5]);
  return Number.isFinite(ms) ? ms : null;
}

export const HOUR = 3600e3;
export const DAY = 86400e3;

/** Finite number test (null / NaN / undefined are "unknown"). */
export const fin = (v) => typeof v === 'number' && Number.isFinite(v);

/** Source bit mask (history rows) -> labels. */
export function srcMaskLabels(mask) {
  const out = [];
  if (mask & 1) out.push(['adsb', 'ADS-B']);
  if (mask & 2) out.push(['mlat', 'MLAT']);
  if (mask & 4) out.push(['tisb', 'TIS-B']);
  if (mask & 8) out.push(['other', 'Other']);
  if (mask & 16) out.push(['modes', 'Mode S']);
  return out;
}

/** Build a URL with query parameters (empty values skipped). */
export function urlWith(path, params) {
  const q = new URLSearchParams();
  for (const [k, v] of Object.entries(params || {})) if (v !== undefined && v !== null && v !== '') q.append(k, String(v));
  const s = q.toString();
  return s ? `${path}?${s}` : path;
}

/** Card body state helper: loading skeleton, error with retry, or "not enough data yet". */
export function stateView(ui, kind, o = {}) {
  if (kind === 'loading') return ui.skeleton({ lines: o.lines || 4, block: o.block || 0 });
  if (kind === 'error') return ui.error(o.error, { title: o.title || 'Could not be loaded', retry: o.retry, compact: true });
  return ui.empty({ icon: o.icon || 'box', title: o.title || 'Not enough data yet', text: o.text, compact: true, action: o.action });
}

/**
 * The playback start for a loaded window: one replay step before the earliest recorded position, so Play
 * never runs through an empty lead-in. Falls back to the window start when there is no data inside it.
 * @param {Array<{first:number}>} tracks  @param {number} from  @param {number} to  @param {number} stepS
 */
export function firstDataTime(tracks, from, to, stepS) {
  let first = Infinity;
  for (const x of tracks || []) if (x.first < first) first = x.first;
  if (!(first > from && first < to)) return from;
  return Math.max(from, first - (stepS || 0) * 1000);
}
