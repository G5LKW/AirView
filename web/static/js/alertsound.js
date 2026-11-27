// alertsound.js - the app-wide alert chime (Web Audio, no audio files) and its per-browser mute switch.
//
// The shell (app.js) plays it for every new alert that asks for a sound, on any page, when
// config.alerts.sound is on, the tab is visible and this browser is not muted. Browsers only allow sound
// after a user gesture on the page: the shell calls unlock() on the first pointer / key press, and play()
// before that fails silently.
//
//   import { play, unlock, isMuted, setMuted, onMuteChange } from './alertsound.js';
//   play('critical');            // 'info' | 'warning' | 'critical'
//   setMuted(true);              // remembered in localStorage (per browser), never on the server

let ac = null;
let unlocked = false;        // unlock() was called from a user gesture
const MUTE_KEY = 'airview.alerts.muted';
const muteSubs = new Set();

function context() {
  if (!ac) {
    const Ctor = window.AudioContext || window.webkitAudioContext;
    if (!Ctor) return null;
    try { ac = new Ctor(); } catch { return null; }
  }
  if (ac.state === 'suspended') ac.resume().catch(() => {});
  return ac;
}

// [frequency Hz, start s, duration s]
const PATTERNS = {
  info: [[880, 0, 0.12]],
  warning: [[740, 0, 0.13], [988, 0.16, 0.16]],
  critical: [[988, 0, 0.12], [740, 0.15, 0.12], [988, 0.3, 0.12], [740, 0.45, 0.2]],
};

/** Play the chime of a severity ('info' | 'warning' | 'critical'). Ignores the mute switch (the caller decides). */
export function play(severity = 'info', volume) {
  const c = context();
  if (!c) return false;
  if (c.state !== 'running') {
    // right after unlock() the context is still resuming: play once it runs; never queue chimes before a gesture
    if (unlocked) c.resume().then(() => schedule(c, severity, volume)).catch(() => {});
    return false;
  }
  schedule(c, severity, volume);
  return true;
}

function schedule(c, severity, volume) {
  const vol = volume ?? (severity === 'info' ? 0.12 : 0.18);
  const t0 = c.currentTime + 0.02;
  for (const [f, at, dur] of PATTERNS[severity] || PATTERNS.info) {
    const o = c.createOscillator();
    const g = c.createGain();
    o.type = 'sine';
    o.frequency.value = f;
    g.gain.setValueAtTime(0.0001, t0 + at);
    g.gain.exponentialRampToValueAtTime(vol, t0 + at + 0.015);
    g.gain.exponentialRampToValueAtTime(0.0001, t0 + at + dur);
    o.connect(g).connect(c.destination);
    o.start(t0 + at);
    o.stop(t0 + at + dur + 0.02);
  }
}

/** Call from a user gesture (click, key press) so later chimes are allowed. */
export function unlock() { unlocked = true; context(); }

// in-memory fallback when localStorage is unavailable (private window, blocked site data)
let memMuted = false;

/** True when alert sounds are muted in this browser. */
export function isMuted() {
  try { return localStorage.getItem(MUTE_KEY) === '1'; } catch { return memMuted; }
}

/** Mute / unmute alert sounds in this browser (stored in localStorage; falls back to this tab only). */
export function setMuted(on) {
  on = !!on;
  memMuted = on;
  try { if (on) localStorage.setItem(MUTE_KEY, '1'); else localStorage.removeItem(MUTE_KEY); } catch { /* memory only */ }
  for (const fn of muteSubs) { try { fn(on); } catch (e) { console.error('[alertsound] subscriber failed', e); } }
}

/** Subscribe to mute changes (from any page). Returns an unsubscribe function. */
export function onMuteChange(fn) { muteSubs.add(fn); return () => muteSubs.delete(fn); }

export const alertSound = { play, unlock, isMuted, setMuted, onMuteChange };
export default alertSound;
