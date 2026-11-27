// map/alerts.js - alert notifications on the Live Map.
//
// While the map is mounted it follows alerts through live.followAlerts(): pushed by the SSE
// `event: alert`, with a /api/alerts/recent?after= catch-up on (re)connect and polling only while
// the stream is down (older shell only, see below).
//
// Current shell (js/alertsound.js present): the shell follows alerts on every page and shows exactly
// one toast (with a "Show" action) and one chime per alert, built-in emergency / military included.
// The map then shows no alert toasts of its own and does not beep; its bell only drives the app-wide
// mute switch (alertsound.setMuted / isMuted).
//
// Older shell (no js/alertsound.js, see loadAlertSound()): the map toasts alerts itself and sets
// ctx.shared 'alertToasts' so the shell skips notices of kind "alert"; the built-in emergency /
// military / record rules are left to the shell's tracker notices; it beeps unless muted.

import { ui, h, icon } from '../ui.js';
import { units } from '../units.js';
import { live } from '../live.js';

// rules whose alerts the shell already toasts through the tracker's own notices
const SHELL_TOASTED = new Set(['builtin-emergency', 'builtin-military', 'builtin-record']);
const SEV = { critical: { cls: 'crit', label: 'Critical' }, warning: { cls: 'warn', label: 'Warning' }, info: { cls: 'info', label: 'Info' } };

let actx = null;
function audio() {
  if (!actx) {
    const AC = window.AudioContext || window.webkitAudioContext;
    if (!AC) return null;
    try { actx = new AC(); } catch { return null; }
  }
  if (actx.state === 'suspended') actx.resume().catch(() => {});
  return actx;
}

/** Unlock audio from a user gesture (browsers block sound until the page was interacted with). */
export function unlockAudio() { if (!appSound) audio(); }

// The shell's app-wide alert sound module, or null when this build has none (resolved once).
let appSound = null;
let appSoundLoad = null;
/**
 * Load js/alertsound.js. Resolves to the module ({ play, isMuted, setMuted }) or null when it is
 * missing or incomplete, in which case the map falls back to its own beep and mute preference.
 */
export function loadAlertSound() {
  if (!appSoundLoad) {
    appSoundLoad = import('../alertsound.js')
      .then((m) => (m && typeof m.play === 'function' && typeof m.isMuted === 'function' && typeof m.setMuted === 'function' ? m : null))
      .catch(() => null)
      .then((m) => { appSound = m; return m; });
  }
  return appSoundLoad;
}
/** The app-wide sound module if it has loaded, else null. */
export function alertSound() { return appSound; }

/** Short alert tone: critical = two-tone x2, warning = rising pair, info = single soft ping. */
export function beep(severity = 'info') {
  const a = audio();
  if (!a || a.state !== 'running') return;
  const seq = severity === 'critical' ? [[880, 0], [660, 0.17], [880, 0.34], [660, 0.51]] : severity === 'warning' ? [[740, 0], [988, 0.15]] : [[1046, 0]];
  const vol = severity === 'info' ? 0.08 : 0.16;
  for (const [f, dt] of seq) {
    const o = a.createOscillator();
    const g = a.createGain();
    o.type = severity === 'critical' ? 'square' : 'sine';
    o.frequency.value = f;
    const t0 = a.currentTime + dt + 0.01;
    g.gain.setValueAtTime(0.0001, t0);
    g.gain.exponentialRampToValueAtTime(vol, t0 + 0.015);
    g.gain.exponentialRampToValueAtTime(0.0001, t0 + 0.14);
    o.connect(g).connect(a.destination);
    o.start(t0);
    o.stop(t0 + 0.16);
  }
}

/**
 * @param {object} ctx page context
 * @param {{ host: HTMLElement, onSelect(icao), soundAllowed(): boolean, muted(): boolean }} hooks
 * @returns {{ stop(): void }}
 */
export function createAlerts(ctx, hooks) {
  let stopped = false;
  let follow = null;
  const stack = h('div.mapv-alerts', { role: 'status', 'aria-live': 'polite' });
  hooks.host.appendChild(stack);

  function show(al) {
    const sev = SEV[al.severity] || SEV.info;
    const meta = [al.altFt != null ? units.alt(al.altFt, { fl: true }) : null, al.distKm != null ? units.dist(al.distKm) : null, units.clock(al.t, { seconds: false })].filter(Boolean).join(' · ');
    const el = h('div.mapv-alert.' + sev.cls, { style: al.highlight ? { '--hl': al.highlight } : null },
      h('span.mapv-alert-ic', icon(al.kind === 'system' ? 'radar' : 'bell')),
      h('div.grow',
        h('div.mapv-alert-title', h('b', al.ruleName || 'Alert'), h('span', sev.label)),
        h('div.mapv-alert-text', units.alertText(al)),
        h('div.mapv-alert-meta', meta)),
      al.icao && ui.button({ label: 'Show', size: 'sm', variant: 'ghost', onClick: () => { close(); hooks.onSelect(al.icao); } }),
      ui.button({ icon: 'x', size: 'sm', variant: 'ghost', tip: 'Dismiss', onClick: () => close() }));
    let t = 0;
    function close() { clearTimeout(t); if (!el.isConnected) return; el.classList.add('leaving'); setTimeout(() => el.remove(), 200); }
    stack.prepend(el);
    while (stack.children.length > 4) stack.lastChild.remove();
    t = setTimeout(close, al.severity === 'critical' ? 20000 : 9000);
  }

  // Legacy shell only (no js/alertsound.js): the map toasts alerts itself and beeps. With the app-wide
  // module the shell already shows one toast (with "Show" -> #/map/<ICAO>, handled by onRoute) and one
  // chime per alert on every page, so the map stays out of it and must not set 'alertToasts' (that
  // would silence the shell's chime as well).
  loadAlertSound().then((m) => {
    if (stopped || m) return;
    ctx.shared?.set('alertToasts', 'map');
    // high-water mark only (initial: 0): never replay alerts that fired before the map opened
    follow = (ctx.live || live).followAlerts({
      signal: ctx.signal,
      initial: 0,
      onAlerts(list) {
        if (stopped) return;
        // a long catch-up (e.g. after the laptop slept) shows only the newest few
        const shown = list.filter((al) => al.notify && !SHELL_TOASTED.has(al.ruleId)).slice(-4);
        for (const al of shown) show(al);
        let sound = null;
        for (const al of list) if (al.sound && (!sound || al.severity === 'critical')) sound = al.severity;
        if (sound && document.visibilityState === 'visible' && hooks.soundAllowed() && !hooks.muted()) beep(sound);
      },
    });
  });

  return {
    stop() {
      stopped = true;
      if (follow) { follow.stop(); ctx.shared?.set('alertToasts', null); }
      stack.remove();
    },
  };
}
