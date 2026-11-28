// map/route-query.js - route / airport terms in the aircraft list's search box (docs/ROUTES-DESIGN.md §6.4).
// Evaluated client-side on the live picture, no server call. Terms (case-insensitive, * and ? wildcards, a leading
// "-" negates):
//   from:EGLL  to:JFK  via:FRA  airport:LHR (from, to or via)    IATA and ICAO both match (the live route carries both)
//   route:LHR-JFK (this direction)  route:LHR~JFK (either direction)
//   airline:BAW (callsign prefix)   state:doubtful   src:learned   kind:positioning
//   eta<30m  eta<45 (minutes; routes with an ETA)   noroute (no route shown)
// Everything else is plain text matched against callsign, ICAO, registration, type, operator, country, squawk and
// the route codes, as before.

const KEYS = new Set(['from', 'to', 'via', 'airport', 'route', 'airline', 'state', 'src', 'kind']);

/** Glob (* ?) -> anchored, case-insensitive RegExp. */
export function globRe(g) {
  const esc = String(g).replace(/[.+^${}()|[\]\\]/g, '\\$&').replace(/\*/g, '.*').replace(/\?/g, '.');
  return new RegExp('^' + esc + '$', 'i');
}

/**
 * Splits a query into plain text tokens (lower case) and route terms [{ key, re | re2, neg, max }].
 */
export function parseQuery(text) {
  const plain = [];
  const terms = [];
  for (let raw of String(text || '').split(/\s+/).filter(Boolean)) {
    let neg = false;
    if (raw.length > 1 && raw[0] === '-') { neg = true; raw = raw.slice(1); }
    const lower = raw.toLowerCase();
    if (lower === 'noroute') { terms.push({ key: 'noroute', neg }); continue; }
    const eta = /^eta<(\d+(?:\.\d+)?)(m|min)?$/i.exec(raw);
    if (eta) { terms.push({ key: 'eta', max: Number(eta[1]), neg }); continue; }
    const i = raw.indexOf(':');
    const key = i > 0 ? lower.slice(0, i) : '';
    const val = i > 0 ? raw.slice(i + 1) : '';
    if (!KEYS.has(key) || !val) { plain.push((neg ? '-' : '') + lower); continue; }
    if (key === 'route') {
      const either = val.includes('~');
      const [a, b] = val.split(either ? '~' : '-');
      if (!a || !b) { plain.push((neg ? '-' : '') + lower); continue; }
      terms.push({ key, a: globRe(a), b: globRe(b), either, neg });
      continue;
    }
    terms.push({ key, re: globRe(key === 'airline' ? val + '*' : val), neg });
  }
  return { plain, terms };
}

const any = (re, ...vals) => vals.some((v) => v && re.test(v));

function termMatches(a, t, nowS) {
  const r = a.route;
  switch (t.key) {
    case 'noroute': return !r;
    case 'airline': return any(t.re, (a.callsign || '').trim());
    case 'eta': return !!r && r.eta > 0 && (r.eta - nowS) / 60 <= t.max && r.eta - nowS >= -60;
  }
  if (!r) return false;
  const stops = Array.isArray(r.stops) ? r.stops : [];
  switch (t.key) {
    case 'from': return any(t.re, r.from, r.fromIata);
    case 'to': return any(t.re, r.to, r.toIata);
    case 'via': return stops.slice(1, -1).some((s) => t.re.test(s));
    case 'airport': return any(t.re, r.from, r.fromIata, r.to, r.toIata) || stops.some((s) => t.re.test(s));
    case 'state': return any(t.re, r.state);
    case 'src': return any(t.re, r.source);
    case 'kind': return any(t.re, r.kind);
    case 'route': {
      const fwd = any(t.a, r.from, r.fromIata) && any(t.b, r.to, r.toIata);
      return fwd || (t.either && any(t.b, r.from, r.fromIata) && any(t.a, r.to, r.toIata));
    }
  }
  return false;
}

/** True when the aircraft satisfies every route term (nowS: server time, Unix s). */
export function matchTerms(a, terms, nowS) {
  for (const t of terms) if (termMatches(a, t, nowS) === t.neg) return false;
  return true;
}
