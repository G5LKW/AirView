// timing/ref.js - which instant of a frame a receiver delay is measured from, and the latency findings that
// concern the host clock.
//
// The Radarcape stamps Mode S frames at the END of the frame. The per-feed latency (tracker, /api/timing/latency
// `feeds[]`) has always compared that raw stamp with the arrival time; the latency chain (internal/latency,
// `chain`) moves the stamp back to the START of the frame (preamble) by `chain.counts.frameEndCorrUs` (64 µs for
// short, 120 µs for long frames) before its "Receiver → host" stage. Newer servers say so explicitly with a
// `ref` field (on the reply, on each feed and/or on the chain); when the two use the same reference the
// numbers are one and the same quantity.

const ok = (v) => typeof v === 'number' && Number.isFinite(v);

/** Normalise a reference value to 'start' | 'end' | null (unknown). */
export function refKind(ref) {
  if (ref === null || ref === undefined || ref === '') return null;
  const k = String(ref).toLowerCase();
  if (/start|preamble|toa|arrival/.test(k)) return 'start';
  if (/end/.test(k)) return 'end';
  return null;
}

export const REF_TEXT = { start: 'start of frame (preamble)', end: 'end of frame' };

/** Reference of the per-feed latency of feed `f` in latency reply `lat`. */
export function feedRef(lat, f) {
  return refKind(f && f.ref) || refKind(lat && lat.ref) || refKind(lat && lat.feedRef) || 'end';
}

/** Reference of the latency chain's "Receiver → host" stage. */
export function chainRef(chain) {
  if (!chain) return null;
  const explicit = refKind(chain.ref) || refKind(chain.receiverRef) || refKind(chain.counts && chain.counts.ref);
  if (explicit) return explicit;
  // the server's own fields (internal/latency): `ref` on the "receiver" stage and on each feed clock
  const stage = Array.isArray(chain.stages) ? chain.stages.find((s) => s && s.key === 'receiver' && s.ref) : null;
  if (stage && refKind(stage.ref)) return refKind(stage.ref);
  const feed = Array.isArray(chain.feeds) ? chain.feeds.find((f) => f && refKind(f.ref)) : null;
  if (feed) return refKind(feed.ref);
  const corr = chain.counts && chain.counts.frameEndCorrUs;
  return ok(corr) ? (corr > 0 ? 'start' : 'end') : 'start';
}

/** Frame-end correction the chain applies (µs), or null. */
export function frameEndCorrUs(chain) {
  const v = chain && chain.counts && chain.counts.frameEndCorrUs;
  return ok(v) ? v : null;
}

/**
 * Latency findings about the host clock not being disciplined by NTP (key "ntp", or any warn/bad finding
 * whose key mentions the clock / offset). Most severe first.
 */
export function clockFindings(chain) {
  const list = (chain && Array.isArray(chain.findings) ? chain.findings : []).filter((f) => f && f.text
    && (f.level === 'warn' || f.level === 'bad')
    && /^(ntp|negative)$|clock|offset|ntp/i.test(String(f.key || '')));
  return list.sort((a, b) => (a.level === 'bad' ? 0 : 1) - (b.level === 'bad' ? 0 : 1));
}
