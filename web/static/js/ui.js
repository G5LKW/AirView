// ui.js - DOM helpers and the AirView component kit (styles: css/components.css).
// Everything returns plain DOM elements; "controller" members are attached as properties
// (documented per function). See js/README.md for an overview with examples.

import { icon, iconMarkup, iconNames } from './icons.js';

// ---------------------------------------------------------------------------------------------
// DOM helpers
// ---------------------------------------------------------------------------------------------

/**
 * Hyperscript element factory.
 *   h('div.card.wide#main', { onclick: fn, title: 'x', style: { width: '10px' }, dataset: { id: 1 } }, child, 'text', [more])
 * Props: class/className, style (string|object), dataset, html (innerHTML), text, on<event> handlers,
 * boolean attributes (true = present, false/null = omitted); everything else via setAttribute.
 * Children: nodes, strings, numbers, arrays; null/undefined/false are skipped.
 */
export function h(spec, props, ...children) {
  let tag = 'div';
  let id = '';
  const classes = [];
  String(spec || 'div').split(/(?=[.#])/).forEach((part, i) => {
    if (part[0] === '.') classes.push(part.slice(1));
    else if (part[0] === '#') id = part.slice(1);
    else if (i === 0) tag = part;
  });
  const el = document.createElement(tag);
  if (id) el.id = id;
  if (props !== null && props !== undefined && (typeof props !== 'object' || props instanceof Node || Array.isArray(props))) {
    children.unshift(props);
    props = null;
  }
  if (props) {
    for (const [k, v] of Object.entries(props)) {
      if (v === null || v === undefined || v === false) continue;
      if (k === 'class' || k === 'className') classes.push(...String(v).split(/\s+/).filter(Boolean));
      else if (k === 'style') { if (typeof v === 'string') el.style.cssText = v; else for (const [sk, sv] of Object.entries(v)) { if (sk.startsWith('--')) el.style.setProperty(sk, sv); else el.style[sk] = sv; } }
      else if (k === 'dataset') Object.assign(el.dataset, v);
      else if (k === 'html') el.innerHTML = v;
      else if (k === 'text') el.textContent = v;
      else if (k === 'tip') el.dataset.tip = v;
      else if (k.startsWith('on') && typeof v === 'function') el.addEventListener(k.slice(2).toLowerCase(), v);
      else if (k === 'value' || k === 'checked' || k === 'disabled' || k === 'selected' || k === 'htmlFor' || k === 'tabIndex') el[k] = v;
      else if (v === true) el.setAttribute(k, '');
      else el.setAttribute(k, v);
    }
  }
  if (classes.length) el.className = classes.join(' ');
  append(el, children);
  return el;
}

/** Append children (nodes, strings, numbers, nested arrays; falsy skipped) to a parent. */
export function append(parent, ...children) {
  for (const c of children.flat(Infinity)) {
    if (c === null || c === undefined || c === false || c === true) continue;
    parent.appendChild(c instanceof Node ? c : document.createTextNode(String(c)));
  }
  return parent;
}

/** Replace all children of el. */
export function mount(el, ...children) {
  el.replaceChildren();
  return append(el, ...children);
}

/** Remove all children. */
export function clear(el) { el.replaceChildren(); return el; }

/** Set textContent only when it changed (cheap 1 Hz updates). Returns true when it wrote. */
export function setText(el, text) {
  const s = text === null || text === undefined ? '' : String(text);
  if (el.__t === s) return false;
  el.__t = s;
  el.textContent = s;
  return true;
}

/** Toggle a class only when needed. */
export function setClass(el, name, on) {
  if (el.classList.contains(name) !== !!on) el.classList.toggle(name, !!on);
}

/** Escape text for use in innerHTML. */
export function escapeHtml(s) {
  return String(s ?? '').replace(/[&<>"']/g, (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));
}

/** Debounce: run fn after `ms` of silence. The returned function has .cancel(). */
export function debounce(fn, ms = 200) {
  let t = 0;
  const d = (...a) => { clearTimeout(t); t = setTimeout(() => fn(...a), ms); };
  d.cancel = () => clearTimeout(t);
  return d;
}

/** Throttle: run fn at most once per `ms` (leading + trailing). */
export function throttle(fn, ms = 200) {
  let last = 0;
  let t = 0;
  let args = null;
  const run = () => { last = Date.now(); t = 0; fn(...args); };
  return (...a) => {
    args = a;
    const wait = ms - (Date.now() - last);
    if (wait <= 0) { clearTimeout(t); run(); } else if (!t) t = setTimeout(run, wait);
  };
}

/** Coalesce calls into one per animation frame. */
export function rafThrottle(fn) {
  let id = 0;
  let args = null;
  const f = (...a) => { args = a; if (!id) id = requestAnimationFrame(() => { id = 0; fn(...args); }); };
  f.cancel = () => { cancelAnimationFrame(id); id = 0; };
  return f;
}

/** Observe element size changes. Returns a disconnect function. */
export function onResize(el, fn) {
  const ro = new ResizeObserver((entries) => { const r = entries[entries.length - 1].contentRect; fn(r.width, r.height); });
  ro.observe(el);
  return () => ro.disconnect();
}

/** Copy text to the clipboard; resolves true on success. */
export async function copy(text) {
  try {
    await navigator.clipboard.writeText(text);
    return true;
  } catch {
    const ta = h('textarea', { style: 'position:fixed;opacity:0;left:-999px' });
    ta.value = text;
    document.body.appendChild(ta);
    ta.select();
    let done = false;
    try { done = document.execCommand('copy'); } catch { done = false; }
    ta.remove();
    return done;
  }
}

const cssLoads = new Map();
/**
 * Load a stylesheet once (page-specific CSS). Resolves when it is applied; a failed load resolves
 * too (after logging) so a missing stylesheet never blocks a page.
 *   await ui.loadCss(new URL('../../css/map.css', import.meta.url))
 */
export function loadCss(href) {
  const url = new URL(href, document.baseURI).href;
  let p = cssLoads.get(url);
  if (!p) {
    p = new Promise((resolve) => {
      const link = h('link', { rel: 'stylesheet', href: url });
      link.addEventListener('load', () => resolve(true));
      link.addEventListener('error', () => { console.error('[ui] stylesheet failed to load:', url); resolve(false); });
      document.head.appendChild(link);
    });
    cssLoads.set(url, p);
  }
  return p;
}

/**
 * localStorage-backed preferences object.
 *   const prefs = ui.prefs('airview.map', { trails: true, minutes: 15 });
 *   prefs.get().trails;  prefs.set({ trails: false });  prefs.on((v) => ...);  prefs.reset();
 */
export function prefs(key, defaults = {}) {
  let val = { ...defaults };
  try {
    const raw = localStorage.getItem(key);
    if (raw) { const o = JSON.parse(raw); if (o && typeof o === 'object') val = { ...defaults, ...o }; }
  } catch { /* corrupt or unavailable storage: use defaults */ }
  const subs = new Set();
  const persist = () => { try { localStorage.setItem(key, JSON.stringify(val)); } catch { /* ignore quota */ } };
  return {
    get: () => val,
    set(patch) { val = { ...val, ...patch }; persist(); for (const fn of subs) fn(val, patch); return val; },
    reset() { val = { ...defaults }; persist(); for (const fn of subs) fn(val, val); return val; },
    on(fn) { subs.add(fn); return () => subs.delete(fn); },
  };
}

// ---------------------------------------------------------------------------------------------
// overlay root, tooltips
// ---------------------------------------------------------------------------------------------

let overlayRoot = null;
function overlays() {
  if (!overlayRoot || !overlayRoot.isConnected) {
    overlayRoot = document.getElementById('overlays') || document.body.appendChild(h('div#overlays'));
  }
  return overlayRoot;
}

let tipEl = null;
let tipTarget = null;
let tipTimer = 0;
function showTip(target) {
  const text = target.dataset.tip;
  if (!text) return;
  if (!tipEl) tipEl = overlays().appendChild(h('div.tip', { role: 'tooltip' }));
  tipEl.textContent = text;
  tipEl.classList.remove('show');
  const r = target.getBoundingClientRect();
  const tw = tipEl.offsetWidth;
  const th = tipEl.offsetHeight;
  const place = target.dataset.tipPlace || 'top';
  let x = r.left + r.width / 2 - tw / 2;
  let y = r.top - th - 7;
  if (place === 'bottom' || y < 4) y = r.bottom + 7;
  if (place === 'right') { x = r.right + 8; y = r.top + r.height / 2 - th / 2; }
  if (place === 'left') { x = r.left - tw - 8; y = r.top + r.height / 2 - th / 2; }
  x = Math.max(6, Math.min(window.innerWidth - tw - 6, x));
  y = Math.max(6, Math.min(window.innerHeight - th - 6, y));
  tipEl.style.left = Math.round(x) + 'px';
  tipEl.style.top = Math.round(y) + 'px';
  tipEl.classList.add('show');
}
function hideTip() {
  clearTimeout(tipTimer);
  tipTarget = null;
  if (tipEl) tipEl.classList.remove('show');
}
function initTips() {
  const find = (e) => (e.target instanceof Element ? e.target.closest('[data-tip]') : null);
  document.addEventListener('pointerover', (e) => {
    if (e.pointerType === 'touch') return;
    const t = find(e);
    if (t === tipTarget) return;
    hideTip();
    if (!t) return;
    tipTarget = t;
    tipTimer = setTimeout(() => { if (tipTarget === t && t.isConnected) showTip(t); }, 350);
  }, { passive: true });
  document.addEventListener('pointerdown', hideTip, { passive: true });
  document.addEventListener('scroll', hideTip, { passive: true, capture: true });
  document.addEventListener('focusin', (e) => { const t = find(e); if (t && t.matches(':focus-visible')) { tipTarget = t; showTip(t); } });
  document.addEventListener('focusout', hideTip);
  document.addEventListener('keydown', (e) => { if (e.key === 'Escape') hideTip(); });
}
if (typeof document !== 'undefined') initTips();

/** Attach / update a tooltip on an element (same as the data-tip attribute). place: top|bottom|left|right */
export function tip(el, text, place) {
  if (text) el.dataset.tip = text; else delete el.dataset.tip;
  if (place) el.dataset.tipPlace = place;
  return el;
}

// ---------------------------------------------------------------------------------------------
// basic components
// ---------------------------------------------------------------------------------------------

/**
 * Glass card.  opts: { title, sub, icon, actions (node|nodes), body (node|nodes), foot, flush, accent, class }
 * Returns the card element with: .body (the .card-body element), .head, .setTitle(text), .setSub(text)
 */
export function card(opts = {}) {
  const titleEl = h('div.card-title', opts.title || '');
  const subEl = h('div.card-sub', opts.sub || '');
  const hasHead = opts.title || opts.actions || opts.icon;
  const head = hasHead ? h('div.card-head', opts.icon && icon(opts.icon), h('div.grow', titleEl, opts.sub !== undefined ? subEl : null), opts.actions ? h('div.card-actions', opts.actions) : null) : null;
  const body = h('div.card-body' + (opts.flush ? '.flush' : ''), opts.body);
  const el = h('section.card' + (opts.accent ? '.card--accent' : ''), { class: opts.class }, head, body, opts.foot ? h('div.card-foot', opts.foot) : null);
  el.body = body;
  el.head = head;
  el.setTitle = (t) => setText(titleEl, t);
  el.setSub = (t) => setText(subEl, t);
  return el;
}

/**
 * Stat tile.  opts: { label, value, unit, sub, icon, accent: 'cyan'|'amber'|'violet'|'red'|'green'|'teal', tip, spark: true }
 * Returns the element with: .set(value, unit?), .setSub(text|node), .spark (empty element to host a sparkline chart)
 */
export function stat(opts = {}) {
  const val = h('span');
  const unit = h('span.unit');
  const sub = h('div.stat-sub');
  const spark = opts.spark ? h('div.stat-spark') : null;
  const el = h('div.stat' + (opts.accent ? '.stat--' + opts.accent : ''), { tip: opts.tip, class: opts.class },
    h('div.stat-label', opts.icon && icon(opts.icon), opts.label || ''),
    h('div.stat-value', val, unit), sub, spark);
  el.set = (v, u) => { setText(val, v === null || v === undefined ? '—' : v); if (u !== undefined) setText(unit, u); };
  el.setSub = (s) => { if (s instanceof Node) mount(sub, s); else setText(sub, s || ''); };
  el.spark = spark;
  el.set(opts.value, opts.unit || '');
  el.setSub(opts.sub);
  return el;
}

/**
 * Button.  opts: { label, icon, iconRight, variant: 'primary'|'ghost'|'danger', size: 'sm'|'lg', onClick, tip,
 *                  disabled, pressed, href, target, type, class }
 * Returns <button> (or <a> when href is given) with .setLoading(bool), .setLabel(text), .setPressed(bool)
 */
export function button(opts = {}) {
  if (typeof opts === 'string') opts = { label: opts };
  const cls = ['btn'];
  if (opts.variant) cls.push('btn--' + opts.variant);
  if (opts.size) cls.push('btn--' + opts.size);
  if (!opts.label && opts.icon) cls.push('btn--icon');
  if (opts.class) cls.push(opts.class);
  const labelEl = opts.label ? h('span', opts.label) : null;
  const iconEl = opts.icon ? icon(opts.icon) : null;
  const el = h(opts.href ? 'a' : 'button', {
    class: cls.join(' '),
    href: opts.href,
    target: opts.target,
    rel: opts.target === '_blank' ? 'noopener noreferrer' : null,
    type: opts.href ? null : opts.type || 'button',
    tip: opts.tip,
    'aria-label': !opts.label ? opts.tip || opts.icon : null,
    'aria-pressed': opts.pressed === undefined ? null : String(!!opts.pressed),
    disabled: opts.disabled ? true : null,
  }, iconEl, labelEl, opts.iconRight && icon(opts.iconRight));
  if (opts.onClick) el.addEventListener('click', opts.onClick);
  let spin = null;
  el.setLoading = (on) => {
    el.disabled = !!on;
    if (on && !spin) { spin = h('span.spinner'); if (iconEl) iconEl.replaceWith(spin); else el.prepend(spin); }
    else if (!on && spin) { if (iconEl) spin.replaceWith(iconEl); else spin.remove(); spin = null; }
  };
  el.setLabel = (t) => { if (labelEl) setText(labelEl, t); };
  el.setPressed = (on) => el.setAttribute('aria-pressed', String(!!on));
  return el;
}

/** Icon-only button with a tooltip. */
export function iconButton(name, tipText, onClick, opts = {}) {
  return button({ icon: name, tip: tipText, onClick, ...opts });
}

/** Badge. kind: blue|cyan|amber|violet|teal|green|red|solid-red|adsb|mlat|modes|tisb|adsr|other  */
export function badge(text, kind, opts = {}) {
  return h('span.badge' + (kind ? '.badge--' + kind : '') + (opts.large ? '.badge--lg' : ''), { tip: opts.tip }, text);
}

const SOURCE_LABELS = { adsb: 'ADS-B', mlat: 'MLAT', tisb: 'TIS-B', adsr: 'ADS-R', modes: 'Mode S', other: 'Other' };
/** Label for a model source constant (adsb|mlat|tisb|adsr|modes|other). */
export function sourceLabel(src) { return SOURCE_LABELS[src] || 'Other'; }
/** Coloured badge for a model source constant. */
export function sourceBadge(src, opts) { return badge(sourceLabel(src), SOURCE_LABELS[src] ? src : 'other', opts); }

let flagEmojiOK = null;
function flagEmojiSupported() {
  if (flagEmojiOK !== null) return flagEmojiOK;
  try {
    // Windows ships no flag glyphs: the regional-indicator pair falls back to two separate letter glyphs.
    // Where flags exist the pair is one ligature, clearly narrower than its two indicators drawn apart.
    const c = document.createElement('canvas').getContext('2d');
    c.font = '32px "Segoe UI Emoji", "Apple Color Emoji", "Noto Color Emoji", sans-serif';
    const pair = c.measureText('🇬🇧').width;
    const apart = c.measureText('🇬').width + c.measureText('🇧').width;
    flagEmojiOK = pair > 0 && pair < apart * 0.8;
  } catch { flagEmojiOK = false; }
  return flagEmojiOK;
}
/** Text for a country flag: the emoji when the platform can draw it, else the ISO code. */
export function flagText(cc) {
  if (!cc || !/^[A-Za-z]{2}$/.test(cc)) return '';
  const u = cc.toUpperCase();
  return flagEmojiSupported() ? String.fromCodePoint(0x1f1e6 + u.charCodeAt(0) - 65, 0x1f1e6 + u.charCodeAt(1) - 65) : u;
}
/** Country flag element (emoji or compact ISO code chip) with the country name as tooltip. */
export function flag(cc, name) {
  const t = flagText(cc);
  if (!t) return h('span.flag-code.dim', { tip: name || 'Unknown country' }, '··');
  return h(flagEmojiSupported() ? 'span.flag' : 'span.flag-code', { tip: name || cc.toUpperCase() }, t);
}

/** Map RSSI (dBFS) to 0..5 bars. */
export function signalLevel(rssi) {
  if (typeof rssi !== 'number' || !Number.isFinite(rssi)) return 0;
  if (rssi >= -6) return 5;
  if (rssi >= -12) return 4;
  if (rssi >= -18) return 3;
  if (rssi >= -25) return 2;
  return 1;
}
/** Five-bar signal indicator. Returns the element with .set(rssi). */
export function signalBar(rssi) {
  const el = h('span.sig', h('i'), h('i'), h('i'), h('i'), h('i'));
  el.set = (v) => { const l = String(signalLevel(v)); if (el.dataset.l !== l) el.dataset.l = l; };
  el.set(rssi);
  return el;
}

/** Thin progress meter 0..100. Returns element with .set(pct). */
export function meter(pct = 0) {
  const el = h('div.meter', h('i'));
  el.set = (p) => el.style.setProperty('--pct', Math.max(0, Math.min(100, p)) + '%');
  el.set(pct);
  return el;
}

/**
 * Key/value list.  rows: [[label, value, { text: true, tip, class }], ...]; value may be a string or node.
 * Returns <dl> with .set(index|label, value) for cheap live updates.
 */
export function kv(rows, opts = {}) {
  const dl = h('dl.kv' + (opts.columns === 2 ? '.kv--2' : ''));
  const dds = [];
  const byLabel = new Map();
  for (const row of rows) {
    if (!row) continue;
    const [label, value, o = {}] = row;
    const dd = h('dd' + (o.text ? '.text' : ''), { class: o.class, tip: o.tip });
    if (value instanceof Node) dd.appendChild(value); else setText(dd, value ?? '—');
    dl.append(h('dt', label), dd);
    dds.push(dd);
    byLabel.set(label, dd);
  }
  dl.set = (key, value) => {
    const dd = typeof key === 'number' ? dds[key] : byLabel.get(key);
    if (!dd) return;
    if (value instanceof Node) mount(dd, value); else setText(dd, value ?? '—');
  };
  return dl;
}

/** Small uppercase section heading with a rule. */
export function section(title, iconName) {
  return h('h4.section-title', iconName && icon(iconName), title);
}

/** Colour legend.  items: [{ label, color }] */
export function legend(items) {
  return h('div.legend', items.map((it) => h('span.legend-item', h('span.legend-swatch', { style: { '--c': it.color } }), it.label)));
}

// ---------------------------------------------------------------------------------------------
// form controls
// ---------------------------------------------------------------------------------------------

/**
 * Text / number input.  opts: { type, value, placeholder, icon, suffix, mono, size: 'sm', onInput(value, e),
 *                               onChange(value, e), onEnter(value), clearable, min, max, step, ...attrs }
 * Returns the wrapper <div.input-wrap> with: .input (the <input>), .value (get/set), .focus()
 */
export function input(opts = {}) {
  const { type = 'text', value = '', icon: ic, suffix, mono, size, onInput, onChange, onEnter, clearable, class: cls, ...attrs } = opts;
  const inp = h('input.input' + (mono || type === 'number' ? '.num' : '') + (size ? '.input--' + size : ''), { type, ...attrs });
  inp.value = value ?? '';
  const wrap = h('div.input-wrap', { class: cls }, ic && icon(ic), inp, suffix && h('span.input-suffix', suffix));
  const val = () => (type === 'number' ? (inp.value === '' ? null : Number(inp.value)) : inp.value);
  let clearBtn = null;
  if (clearable) {
    clearBtn = button({ icon: 'x', variant: 'ghost', size: 'sm', class: 'input-clear hidden', tip: 'Clear', onClick: () => { inp.value = ''; inp.dispatchEvent(new Event('input', { bubbles: true })); inp.focus(); } });
    wrap.appendChild(clearBtn);
  }
  inp.addEventListener('input', (e) => { if (clearBtn) clearBtn.classList.toggle('hidden', !inp.value); if (onInput) onInput(val(), e); });
  if (onChange) inp.addEventListener('change', (e) => onChange(val(), e));
  if (onEnter) inp.addEventListener('keydown', (e) => { if (e.key === 'Enter') onEnter(val(), e); });
  if (suffix) inp.style.paddingRight = 14 + String(suffix).length * 7 + 'px';
  wrap.input = inp;
  Object.defineProperty(wrap, 'value', { get: val, set: (v) => { inp.value = v ?? ''; if (clearBtn) clearBtn.classList.toggle('hidden', !inp.value); } });
  wrap.focus = () => inp.focus();
  return wrap;
}

/**
 * Select.  opts: { options: [{ value, label, disabled }] | ['a','b'], value, onChange(value), size: 'sm', ...attrs }
 * Returns the <select> element (use .value) with .setOptions(options).
 */
export function select(opts = {}) {
  const { options = [], value, onChange, size, class: cls, label, ...attrs } = opts;
  const el = h('select.select' + (size ? '.select--' + size : ''), { class: cls, ...attrs });
  // UX-06: a select outside a field still needs a name: opts.label, else the first (placeholder) option, e.g.
  // "Any kind". ui.field() links its label instead (aria-label is then replaced by the label element).
  if (!attrs['aria-label'] && !attrs['aria-labelledby']) {
    const first = options[0];
    const name = label || (first && typeof first === 'object' ? first.label : null);
    if (name) { el.setAttribute('aria-label', String(name)); el.dataset.autoLabel = '1'; }
  }
  el.setOptions = (list) => {
    const cur = el.value;
    mount(el, list.map((o) => {
      const opt = typeof o === 'object' ? o : { value: o, label: String(o) };
      return h('option', { value: opt.value, disabled: opt.disabled ? true : null }, opt.label ?? String(opt.value));
    }));
    if (cur) el.value = cur;
  };
  el.setOptions(options);
  if (value !== undefined && value !== null) el.value = value;
  if (onChange) el.addEventListener('change', () => onChange(el.value));
  return el;
}

/**
 * Toggle switch.  opts: { label, checked, onChange(checked), disabled, tip }
 * Returns <label.toggle> with .checked (get/set, setting does not fire onChange) and .input
 */
export function toggle(opts = {}) {
  const inp = h('input', { type: 'checkbox', role: 'switch' });
  inp.checked = !!opts.checked;
  inp.disabled = !!opts.disabled;
  const el = h('label.toggle', { tip: opts.tip, class: opts.class }, inp, h('span.toggle-track'), opts.label ? h('span', opts.label) : null);
  if (opts.onChange) inp.addEventListener('change', () => opts.onChange(inp.checked));
  el.input = inp;
  Object.defineProperty(el, 'checked', { get: () => inp.checked, set: (v) => { inp.checked = !!v; } });
  return el;
}

/** Checkbox.  opts: { label, checked, onChange(checked), disabled }.  Returns <label.check> with .checked, .input */
export function checkbox(opts = {}) {
  const inp = h('input', { type: 'checkbox' });
  inp.checked = !!opts.checked;
  inp.disabled = !!opts.disabled;
  const el = h('label.check', { tip: opts.tip }, inp, opts.label ? h('span', opts.label) : null);
  if (opts.onChange) inp.addEventListener('change', () => opts.onChange(inp.checked));
  el.input = inp;
  Object.defineProperty(el, 'checked', { get: () => inp.checked, set: (v) => { inp.checked = !!v; } });
  return el;
}

/**
 * Range slider with a live value read-out.
 * opts: { min, max, step, value, format(value) -> string, onInput(value), onChange(value), disabled }
 * Returns <div.slider> with .value (get/set) and .input
 */
export function slider(opts = {}) {
  const { min = 0, max = 100, step = 1, value = min, format = (v) => String(v), onInput, onChange } = opts;
  const inp = h('input', { type: 'range', min, max, step });
  if (opts.label) inp.setAttribute('aria-label', opts.label); // UX-06 (ui.field() links its label instead)
  inp.value = value;
  inp.disabled = !!opts.disabled;
  const out = h('span.slider-value.num');
  const sync = () => {
    const v = Number(inp.value);
    inp.style.setProperty('--pct', ((v - min) / (max - min || 1)) * 100 + '%');
    setText(out, format(v));
  };
  inp.addEventListener('input', () => { sync(); if (onInput) onInput(Number(inp.value)); });
  if (onChange) inp.addEventListener('change', () => onChange(Number(inp.value)));
  const el = h('div.slider', { class: opts.class }, inp, out);
  el.input = inp;
  Object.defineProperty(el, 'value', { get: () => Number(inp.value), set: (v) => { inp.value = v; sync(); } });
  el.refresh = sync;
  sync();
  return el;
}

/**
 * Segmented control (single choice).
 * opts: { options: [{ value, label, icon, tip }] | ['a','b'], value, onChange(value), block }
 * Returns <div.seg> with .value (get/set, setting does not fire onChange)
 */
export function segmented(opts = {}) {
  let cur = opts.value;
  const el = h('div.seg' + (opts.block ? '.seg--block' : ''), { role: 'group', class: opts.class });
  const btns = (opts.options || []).map((o) => {
    const opt = typeof o === 'object' ? o : { value: o, label: String(o) };
    const b = h('button', { type: 'button', tip: opt.tip, 'aria-pressed': String(opt.value === cur) }, opt.icon && icon(opt.icon), opt.label);
    b.addEventListener('click', () => { if (cur === opt.value) return; set(opt.value); if (opts.onChange) opts.onChange(cur); });
    b.__v = opt.value;
    return b;
  });
  const set = (v) => { cur = v; for (const b of btns) b.setAttribute('aria-pressed', String(b.__v === v)); };
  append(el, btns);
  Object.defineProperty(el, 'value', { get: () => cur, set });
  return el;
}

/**
 * Labelled form field wrapper.
 * opts: { label, hint, control (node), error, row (label left / control right), tip }
 * Returns <div.field> with .setError(msg|null), .setHint(text)
 */
let fieldSeq = 0;
const FORM_TAGS = new Set(['INPUT', 'SELECT', 'TEXTAREA']);

/** The one form element of a field control (the control itself, its .input, or its only input/select/textarea). */
function formElementOf(ctrl) {
  if (!ctrl || !ctrl.nodeType) return null;
  if (FORM_TAGS.has(ctrl.tagName)) return ctrl;
  if (ctrl.input && FORM_TAGS.has(ctrl.input.tagName)) return ctrl.input;
  const all = ctrl.querySelectorAll ? ctrl.querySelectorAll('input:not([type=hidden]), select, textarea') : [];
  return all.length === 1 ? all[0] : null;
}

export function field(opts = {}) {
  // UX-06: the label names the control (for/id, or aria-labelledby on a composite control) and the hint and
  // error text describe it (aria-describedby), so screen readers announce named fields.
  const seq = ++fieldSeq;
  const err = h('div.field-error.hidden', { id: `fld-${seq}-err`, role: 'alert' });
  const hint = h('div.field-hint' + (opts.hint ? '' : '.hidden'), { id: `fld-${seq}-hint` }, opts.hint || '');
  const label = opts.label ? h('label.field-label', { tip: opts.tip, id: `fld-${seq}-label` }, opts.label) : null;
  const target = formElementOf(opts.control);
  if (label && target) {
    if (!target.id) target.id = `fld-${seq}-ctl`;
    label.htmlFor = target.id;
    if (target.dataset && target.dataset.autoLabel) { target.removeAttribute('aria-label'); delete target.dataset.autoLabel; }
  } else if (label && opts.control && opts.control.setAttribute) {
    opts.control.setAttribute('aria-labelledby', label.id);
    if (!opts.control.getAttribute('role')) opts.control.setAttribute('role', 'group');
  }
  const described = target || (opts.control && opts.control.setAttribute ? opts.control : null);
  if (described) {
    const prev = described.getAttribute('aria-describedby');
    described.setAttribute('aria-describedby', [prev, hint.id, err.id].filter(Boolean).join(' '));
  }
  const el = opts.row
    ? h('div.field.field--row', { class: opts.class }, h('div.field-text', label, hint, err), opts.control)
    : h('div.field', { class: opts.class }, label, opts.control, hint, err);
  el.setError = (msg) => {
    err.classList.toggle('hidden', !msg);
    setText(err, msg || '');
    const inp = opts.control && (opts.control.input || opts.control);
    if (inp && inp.setAttribute) { if (msg) inp.setAttribute('aria-invalid', 'true'); else inp.removeAttribute('aria-invalid'); }
  };
  el.setHint = (t) => { hint.classList.toggle('hidden', !t); setText(hint, t || ''); };
  if (opts.error) el.setError(opts.error);
  return el;
}

/**
 * Tabs.  opts: { tabs: [{ id, label, icon, badge, render(panelEl) -> void|cleanupFn, content (node) }], active, onChange(id) }
 * Panels render lazily on first show and are kept in the DOM afterwards.
 * Returns <div.tabs> with .select(id), .active, .panel(id), .setBadge(id, text)
 */
export function tabs(opts = {}) {
  const list = h('div.tab-list', { role: 'tablist' });
  const panels = h('div.tab-panels.grow');
  const el = h('div.tabs', { class: opts.class }, list, panels);
  const items = new Map();
  let active = null;
  for (const t of opts.tabs || []) {
    const badgeEl = h('span.badge.badge--blue' + (t.badge === undefined || t.badge === null ? '.hidden' : ''), t.badge ?? '');
    const btn = h('button.tab', { type: 'button', role: 'tab', 'aria-selected': 'false' }, t.icon && icon(t.icon), t.label, badgeEl);
    const panel = h('div.tab-panel.hidden', { role: 'tabpanel' });
    btn.addEventListener('click', () => { if (active !== t.id) { el.select(t.id); if (opts.onChange) opts.onChange(t.id); } });
    list.appendChild(btn);
    panels.appendChild(panel);
    items.set(t.id, { def: t, btn, panel, badgeEl, rendered: false });
  }
  el.select = (id) => {
    const it = items.get(id);
    if (!it) return;
    for (const [k, v] of items) {
      v.btn.setAttribute('aria-selected', String(k === id));
      v.panel.classList.toggle('hidden', k !== id);
    }
    active = id;
    if (!it.rendered) {
      it.rendered = true;
      if (it.def.content) append(it.panel, it.def.content);
      if (it.def.render) it.def.render(it.panel);
    }
    if (it.def.onShow) it.def.onShow(it.panel);
  };
  el.panel = (id) => items.get(id)?.panel;
  el.setBadge = (id, text) => { const it = items.get(id); if (it) { it.badgeEl.classList.toggle('hidden', text === null || text === undefined || text === ''); setText(it.badgeEl, text ?? ''); } };
  Object.defineProperty(el, 'active', { get: () => active });
  const first = opts.active && items.has(opts.active) ? opts.active : opts.tabs?.[0]?.id;
  if (first !== undefined) el.select(first);
  return el;
}

// ---------------------------------------------------------------------------------------------
// states
// ---------------------------------------------------------------------------------------------

/** Skeleton placeholder.  opts: { lines = 3, title, block (height px) } */
export function skeleton(opts = {}) {
  const { lines = 3, title = false, block = 0 } = opts;
  const el = h('div', { 'aria-busy': 'true' });
  if (title) el.appendChild(h('span.skeleton.skeleton--title'));
  if (block) el.appendChild(h('span.skeleton.skeleton--block', { style: { height: block + 'px' } }));
  for (let i = 0; i < lines; i++) el.appendChild(h('span.skeleton.skeleton--text', { style: { width: 60 + ((i * 37) % 40) + '%' } }));
  return el;
}

/** Empty state.  opts: { icon = 'box', title, text, action (node), compact } */
export function empty(opts = {}) {
  if (typeof opts === 'string') opts = { title: opts };
  return h('div.state' + (opts.compact ? '.state--compact' : ''), icon(opts.icon || 'box'), opts.title && h('div.state-title', opts.title), opts.text && h('div.state-text', opts.text), opts.action);
}

/** Loading state with spinner. */
export function loading(text = 'Loading…', opts = {}) {
  return h('div.state.state--loading' + (opts.compact ? '.state--compact' : ''), h('span.spinner'), h('div.state-text', text));
}

/** Error state.  err: Error|string; opts: { title, retry: fn, compact } */
export function error(err, opts = {}) {
  const msg = err instanceof Error ? err.message : String(err || 'Unknown error');
  return h('div.state.state--error' + (opts.compact ? '.state--compact' : ''), { role: 'alert' }, icon('alert'),
    h('div.state-title', opts.title || 'Something went wrong'), h('div.state-text', msg),
    opts.retry && button({ label: 'Retry', icon: 'refresh', size: 'sm', onClick: opts.retry }));
}

/**
 * Load-and-render helper: shows a skeleton, awaits loader(), then render(data, container);
 * on failure shows an error state with a Retry button.
 *   const reload = ui.load(card.body, () => api.get('/api/stats/overview'), (data, el) => { ... });
 * Returns reload() (also usable for refresh; pass { quiet: true } to keep the old content while loading).
 */
export function load(container, loader, render, opts = {}) {
  let seq = 0;
  const run = async (o = {}) => {
    const my = ++seq;
    if (!o.quiet) mount(container, opts.skeleton || skeleton({ lines: 4 }));
    try {
      const data = await loader();
      if (my !== seq || !container.isConnected) return;
      if (!o.quiet) clear(container);
      const out = await render(data, container);
      if (out instanceof Node) mount(container, out);
    } catch (e) {
      if (my !== seq || !container.isConnected) return;
      if (e && e.aborted) return;
      console.error('[ui.load]', e);
      mount(container, error(e, { retry: () => run(), compact: opts.compact }));
    }
  };
  run();
  return run;
}

// ---------------------------------------------------------------------------------------------
// toasts
// ---------------------------------------------------------------------------------------------

let toastHost = null;
const TOAST_ICONS = { info: 'info', success: 'check-circle', warn: 'alert', error: 'x-circle' };

/**
 * Toast notification.
 *   ui.toast('Saved');  ui.toast.success('Settings saved');  ui.toast.error(err);  ui.toast.warn('...', { timeout: 0 })
 * opts: { type: 'info'|'success'|'warn'|'error', title, timeout (ms, 0 = sticky), action: { label, onClick } | { label, href } }
 * An action with href is a real link (e.g. '#/settings/feeds'); clicking it (or onClick) closes the toast.
 * Returns { el, close() }.
 */
export function toast(message, opts = {}) {
  if (!toastHost || !toastHost.isConnected) toastHost = overlays().appendChild(h('div.toasts', { role: 'status', 'aria-live': 'polite' }));
  const type = opts.type || 'info';
  const text = message instanceof Error ? message.message : String(message ?? '');
  const el = h('div.toast.toast--' + type, icon(TOAST_ICONS[type] || 'info'),
    h('div.toast-body.grow', opts.title && h('div.toast-title', opts.title), h('div', { class: opts.title ? 'toast-text' : '' }, text)));
  let timer = 0;
  const close = () => {
    clearTimeout(timer);
    if (!el.isConnected) return;
    el.classList.add('leaving');
    setTimeout(() => el.remove(), 220);
  };
  if (opts.action) {
    const a = opts.action;
    el.appendChild(button({ label: a.label, size: 'sm', variant: 'ghost', href: a.href || undefined, class: 'toast-action', onClick: () => { close(); if (typeof a.onClick === 'function') a.onClick(); } }));
  }
  el.appendChild(button({ icon: 'x', size: 'sm', variant: 'ghost', tip: 'Dismiss', onClick: close }));
  toastHost.appendChild(el);
  while (toastHost.children.length > 5) toastHost.firstChild.remove();
  const timeout = opts.timeout ?? (type === 'error' ? 8000 : 4500);
  if (timeout > 0) timer = setTimeout(close, timeout);
  return { el, close };
}
toast.info = (m, o) => toast(m, { ...o, type: 'info' });
toast.success = (m, o) => toast(m, { ...o, type: 'success' });
toast.warn = (m, o) => toast(m, { ...o, type: 'warn' });
toast.error = (m, o) => toast(m, { ...o, type: 'error' });

// ---------------------------------------------------------------------------------------------
// modal, confirm, drawer, popover, menu
// ---------------------------------------------------------------------------------------------

function trapFocus(root, e) {
  if (e.key !== 'Tab') return;
  const f = [...root.querySelectorAll('a[href],button:not([disabled]),input:not([disabled]),select:not([disabled]),textarea:not([disabled]),[tabindex]:not([tabindex="-1"])')].filter((n) => n.offsetParent !== null);
  if (!f.length) return;
  const first = f[0];
  const last = f[f.length - 1];
  if (e.shiftKey && document.activeElement === first) { e.preventDefault(); last.focus(); }
  else if (!e.shiftKey && document.activeElement === last) { e.preventDefault(); first.focus(); }
}

/**
 * Modal dialog.
 * opts: { title, icon, body (node|string), actions: [{ label, variant, value, icon, onClick(close) , disabled }],
 *         width (px), danger, dismissable = true, onClose(value) }
 * Clicking an action closes with its `value` unless its onClick returns false (or a Promise resolving false).
 * Returns { el, body, close(value), closed: Promise<value>, actionButtons: [button] }
 */
export function modal(opts = {}) {
  const prevFocus = document.activeElement;
  let resolve;
  const closed = new Promise((r) => { resolve = r; });
  const body = h('div.modal-body', typeof opts.body === 'string' ? h('p', opts.body) : opts.body);
  const foot = h('div.modal-foot');
  const dismissable = opts.dismissable !== false;
  const dlg = h('div.modal' + (opts.danger ? '.modal--danger' : ''), { role: 'dialog', 'aria-modal': 'true', style: opts.width ? { '--modal-w': opts.width + 'px' } : null },
    h('div.modal-head', icon(opts.icon || (opts.danger ? 'alert' : 'info')), h('h3', opts.title || ''), dismissable && button({ icon: 'x', variant: 'ghost', size: 'sm', tip: 'Close', onClick: () => close(undefined) })),
    body, foot);
  const backdrop = h('div.modal-backdrop', dlg);
  let done = false;
  function close(value) {
    if (done) return;
    done = true;
    document.removeEventListener('keydown', onKey, true);
    backdrop.remove();
    if (prevFocus && prevFocus.focus && prevFocus.isConnected) prevFocus.focus();
    if (opts.onClose) opts.onClose(value);
    resolve(value);
  }
  function onKey(e) {
    if (e.key === 'Escape' && dismissable) { e.stopPropagation(); close(undefined); }
    else trapFocus(dlg, e);
  }
  const actionButtons = (opts.actions || [{ label: 'Close', value: undefined }]).map((a) => {
    const b = button({ label: a.label, variant: a.variant, icon: a.icon, disabled: a.disabled, onClick: async () => {
      if (a.onClick) {
        b.setLoading(true);
        let keep;
        try { keep = await a.onClick(close); } finally { b.setLoading(false); }
        if (keep === false) return;
      }
      close(a.value);
    } });
    return b;
  });
  append(foot, actionButtons);
  backdrop.addEventListener('mousedown', (e) => { if (e.target === backdrop && dismissable) close(undefined); });
  document.addEventListener('keydown', onKey, true);
  overlays().appendChild(backdrop);
  const auto = dlg.querySelector('[autofocus]') || body.querySelector('input,select,textarea') || actionButtons[actionButtons.length - 1];
  if (auto) auto.focus();
  return { el: dlg, body, close, closed, actionButtons };
}

/**
 * Confirmation dialog -> Promise<boolean>.
 * opts: { title, message (string|node), confirmLabel = 'Confirm', cancelLabel = 'Cancel', danger,
 *         requireText: 'RESET' (user must type this exact text to enable the confirm button) }
 */
export function confirm(opts = {}) {
  if (typeof opts === 'string') opts = { message: opts };
  const parts = [typeof opts.message === 'string' ? h('p', opts.message) : opts.message];
  let inp = null;
  if (opts.requireText) {
    inp = input({ placeholder: opts.requireText, mono: true, autocomplete: 'off', spellcheck: 'false' });
    parts.push(field({ label: `Type ${opts.requireText} to confirm`, control: inp }));
  }
  const m = modal({
    title: opts.title || 'Are you sure?',
    icon: opts.icon,
    danger: opts.danger,
    width: opts.width,
    body: h('div.col.gap-3', parts),
    actions: [
      { label: opts.cancelLabel || 'Cancel', variant: 'ghost', value: false },
      { label: opts.confirmLabel || 'Confirm', variant: opts.danger ? 'danger' : 'primary', value: true, disabled: !!opts.requireText },
    ],
  });
  if (inp) {
    const okBtn = m.actionButtons[1];
    inp.input.addEventListener('input', () => { okBtn.disabled = inp.value.trim() !== opts.requireText; });
    inp.input.addEventListener('keydown', (e) => { if (e.key === 'Enter' && !okBtn.disabled) okBtn.click(); });
  }
  return m.closed.then((v) => v === true);
}

/** Simple message dialog -> Promise (resolves when closed). */
export function alert(title, message) {
  return modal({ title, body: message, actions: [{ label: 'OK', variant: 'primary', value: true }] }).closed;
}

/**
 * Slide-in drawer.
 * opts: { title, body, foot, side: 'right'|'left', width (px), modal = true (backdrop + Esc), onClose }
 * Returns { el, body, close() }
 */
export function drawer(opts = {}) {
  const body = h('div.drawer-body', opts.body);
  const el = h('aside.drawer' + (opts.side === 'left' ? '.drawer--left' : ''), { role: 'dialog', style: opts.width ? { '--drawer-w': opts.width + 'px' } : null },
    h('div.drawer-head', opts.icon && icon(opts.icon), h('h3', opts.title || ''), button({ icon: 'x', variant: 'ghost', tip: 'Close', onClick: () => close() })),
    body, opts.foot ? h('div.drawer-foot', opts.foot) : null);
  const isModal = opts.modal !== false;
  const backdrop = isModal ? h('div.drawer-backdrop') : null;
  let done = false;
  function close() {
    if (done) return;
    done = true;
    document.removeEventListener('keydown', onKey, true);
    el.classList.remove('open');
    if (backdrop) backdrop.remove();
    setTimeout(() => el.remove(), 240);
    if (opts.onClose) opts.onClose();
  }
  function onKey(e) { if (e.key === 'Escape') { e.stopPropagation(); close(); } else if (isModal) trapFocus(el, e); }
  if (backdrop) { backdrop.addEventListener('click', close); overlays().appendChild(backdrop); }
  overlays().appendChild(el);
  document.addEventListener('keydown', onKey, true);
  // A timer rather than requestAnimationFrame: rAF never fires while the tab is in the background.
  setTimeout(() => el.classList.add('open'), 20);
  return { el, body, close };
}

/**
 * Popover anchored to an element; closes on outside click, Escape, or .close().
 * opts: { placement: 'bottom-end'|'bottom-start'|'top-end'|'top-start'|'left-start'|'right-start', class, onClose }
 * Returns { el, close(), reposition() }
 */
export function popover(anchor, content, opts = {}) {
  const el = h('div.popover', { class: opts.class }, content);
  overlays().appendChild(el);
  const reposition = () => {
    const r = anchor.getBoundingClientRect();
    const w = el.offsetWidth;
    const ht = el.offsetHeight;
    const p = opts.placement || 'bottom-end';
    let x = p.endsWith('end') ? r.right - w : r.left;
    let y = p.startsWith('top') ? r.top - ht - 6 : r.bottom + 6;
    if (p.startsWith('left')) { x = r.left - w - 8; y = r.top; }
    if (p.startsWith('right')) { x = r.right + 8; y = r.top; }
    if (y + ht > window.innerHeight - 8 && p.startsWith('bottom') && r.top - ht - 6 > 8) y = r.top - ht - 6;
    el.style.left = Math.round(Math.max(8, Math.min(window.innerWidth - w - 8, x))) + 'px';
    el.style.top = Math.round(Math.max(8, Math.min(window.innerHeight - ht - 8, y))) + 'px';
  };
  let done = false;
  const close = () => {
    if (done) return;
    done = true;
    document.removeEventListener('pointerdown', onDown, true);
    document.removeEventListener('keydown', onKey, true);
    window.removeEventListener('resize', reposition);
    el.remove();
    if (opts.onClose) opts.onClose();
  };
  const onDown = (e) => { if (!el.contains(e.target) && !anchor.contains(e.target)) close(); };
  const onKey = (e) => { if (e.key === 'Escape') { e.stopPropagation(); close(); if (anchor.focus) anchor.focus(); } };
  document.addEventListener('pointerdown', onDown, true);
  document.addEventListener('keydown', onKey, true);
  window.addEventListener('resize', reposition);
  reposition();
  return { el, close, reposition };
}

/**
 * Dropdown menu.  items: [{ label, icon, checked, onClick, danger } | 'sep']
 * Returns the popover controller.
 */
export function menu(anchor, items, opts = {}) {
  let pop = null;
  const body = h('div.menu', { role: 'menu' }, items.map((it) => {
    if (it === 'sep') return h('hr', { style: 'margin:4px 0' });
    return h('button.menu-item', { type: 'button', role: 'menuitemradio', 'aria-checked': it.checked ? 'true' : null, style: it.danger ? 'color:#ff8b97' : null,
      onclick: () => { pop.close(); if (it.onClick) it.onClick(); } }, it.icon && icon(it.icon), it.label);
  }));
  pop = popover(anchor, body, { ...opts, class: 'popover--menu' });
  pop.el.style.padding = '0';
  return pop;
}

// ---------------------------------------------------------------------------------------------
// tables
// ---------------------------------------------------------------------------------------------

function cmp(a, b) {
  const an = a === null || a === undefined || a === '' || (typeof a === 'number' && Number.isNaN(a));
  const bn = b === null || b === undefined || b === '' || (typeof b === 'number' && Number.isNaN(b));
  if (an || bn) return an && bn ? 0 : an ? 1 : -1; // handled by caller for direction (nulls always last)
  if (typeof a === 'number' && typeof b === 'number') return a - b;
  return String(a).localeCompare(String(b), undefined, { numeric: true, sensitivity: 'base' });
}

function sortRows(rows, col, dir, keyOf) {
  if (!col) return rows;
  const get = typeof col.sort === 'function' ? col.sort : col.value || ((r) => r[col.key]);
  const sign = dir === 'desc' ? -1 : 1;
  const dec = rows.map((r) => ({ r, v: get(r), k: keyOf ? keyOf(r) : 0 }));
  dec.sort((x, y) => {
    const xn = x.v === null || x.v === undefined || x.v === '' || (typeof x.v === 'number' && Number.isNaN(x.v));
    const yn = y.v === null || y.v === undefined || y.v === '' || (typeof y.v === 'number' && Number.isNaN(y.v));
    if (xn !== yn) return xn ? 1 : -1; // missing values last in both directions
    const c = xn ? 0 : cmp(x.v, y.v) * sign;
    return c !== 0 ? c : x.k < y.k ? -1 : x.k > y.k ? 1 : 0;
  });
  return dec.map((d) => d.r);
}

/**
 * Plain HTML table for small, mostly static data (up to a few hundred rows).
 * opts: { columns: [{ key, label, align: 'right'|'center', num, render(row) -> string|node, sort: true|fn, tip,
 *                     width: 120 (px) | '8em' | '20%' }],
 *         rows, sort: { key, dir }, onRowClick(row), empty: 'text', rowClass(row),
 *         fixed: true (table-layout: fixed - widths are exact, long cells ellipsis) }
 * Column widths go into a <colgroup>; without `fixed` they are the browser's starting point (auto layout).
 * Returns <div.table-wrap> with .setRows(rows), .setSort(key, dir)
 */
export function table(opts = {}) {
  const cols = opts.columns || [];
  let rows = opts.rows || [];
  let sort = opts.sort ? { ...opts.sort } : null;
  const thead = h('thead');
  const tbody = h('tbody');
  const cssLen = (w) => (typeof w === 'number' && Number.isFinite(w) ? w + 'px' : typeof w === 'string' && w ? w : null);
  const colgroup = cols.some((c) => cssLen(c.width)) ? h('colgroup', cols.map((c) => h('col', { style: cssLen(c.width) ? { width: cssLen(c.width) } : null }))) : null;
  const tableEl = h('table.table' + (opts.fixed ? '.table--fixed' : ''), colgroup, thead, tbody);
  const el = h('div.table-wrap', { class: opts.class }, tableEl);
  const ths = cols.map((c) => {
    const th = h('th' + (c.align ? '.' + c.align : '') + (c.sort ? '.sortable' : ''), { tip: c.tip }, c.label ?? '');
    if (c.sort) th.addEventListener('click', () => { sort = { key: c.key, dir: sort && sort.key === c.key ? (sort.dir === 'asc' ? 'desc' : 'asc') : c.sortDir || 'asc' }; draw(); });
    return th;
  });
  thead.appendChild(h('tr', ths));
  function draw() {
    cols.forEach((c, i) => { if (sort && sort.key === c.key) ths[i].setAttribute('aria-sort', sort.dir === 'desc' ? 'descending' : 'ascending'); else ths[i].removeAttribute('aria-sort'); });
    const col = sort && cols.find((c) => c.key === sort.key);
    const list = col ? sortRows(rows, col, sort.dir) : rows;
    if (!list.length) { mount(tbody, h('tr', h('td', { colspan: cols.length }, empty({ title: opts.empty || 'No data', compact: true })))); return; }
    mount(tbody, list.map((r) => {
      const tr = h('tr', { class: opts.rowClass ? opts.rowClass(r) : null }, cols.map((c) => {
        const v = c.render ? c.render(r) : r[c.key];
        return h('td' + (c.align ? '.' + c.align : '') + (c.num ? '.num' : ''), v instanceof Node ? v : v ?? '—');
      }));
      if (opts.onRowClick) { tr.style.cursor = 'pointer'; tr.addEventListener('click', () => opts.onRowClick(r)); }
      return tr;
    }));
  }
  el.setRows = (r) => { rows = r || []; draw(); };
  el.setSort = (key, dir = 'asc') => { sort = key ? { key, dir } : null; draw(); };
  draw();
  return el;
}

/**
 * Virtualised, sortable data table for live data (hundreds to thousands of rows at 1 Hz).
 * Only the visible rows exist in the DOM and cells are only touched when their text changes.
 *
 * opts: {
 *   columns: [{
 *     key, label, width: '72px' | 'minmax(90px,1fr)', align: 'right'|'center', num: true (mono tabular),
 *     render(row) -> string|number|null      text content (preferred, cheapest), or
 *     update(td, row)                        mutate the cell yourself (icons, badges, bars); create children once,
 *     cls(row) -> extra class string, tip: header tooltip,
 *     sort: true | (row) -> comparable       sortable; true sorts by value(row) or row[key],
 *     value(row), sortDir: 'asc'|'desc'      first-click direction (default 'asc'),
 *   }],
 *   rowKey(row) -> string|number   (default row.id)
 *   rowHeight = 30, minWidth (px, enables horizontal scroll), sort: { key, dir },
 *   onRowClick(row, event), onRowHover(row|null), rowClass(row) -> string, onSort({key, dir}),
 *   empty: 'text' | node
 * }
 * Returns { el, setRows(rows), rows(), setSort(key, dir), getSort(), setSelected(key|null), setHover(key|null),
 *           scrollToKey(key), refresh(), destroy() }
 * The host element must have a height (flex child or fixed height): el fills 100%.
 */
export function dataTable(opts = {}) {
  const cols = opts.columns || [];
  const rowH = opts.rowHeight || 30;
  const keyOf = opts.rowKey || ((r) => r.id);
  const OVERSCAN = 4;
  let raw = [];
  let rows = [];
  let sort = opts.sort ? { ...opts.sort } : null;
  let selected = null;
  let hover = null;
  let destroyed = false;

  const head = h('div.dt-head', { role: 'row' });
  const body = h('div.dt-body', { role: 'rowgroup' });
  const scroll = h('div.dt-scroll', { tabIndex: 0 }, head, body);
  const emptyEl = h('div.dt-empty.hidden', typeof opts.empty === 'object' && opts.empty ? opts.empty : empty({ title: opts.empty || 'Nothing to show', compact: true }));
  const el = h('div.dt' + (opts.onRowClick ? '.dt--clickable' : ''), { class: opts.class, role: 'table' }, scroll, emptyEl);
  el.style.setProperty('--dt-cols', cols.map((c) => c.width || 'minmax(60px,1fr)').join(' '));
  el.style.setProperty('--dt-row-h', rowH + 'px');
  if (opts.minWidth) el.style.setProperty('--dt-min-w', opts.minWidth + 'px');

  const ths = cols.map((c) => {
    const ind = h('span.dt-sort');
    const th = h((c.sort ? 'button' : 'div') + '.dt-th' + (c.align ? '.' + c.align : '') + (c.sort ? '.sortable' : ''), { role: 'columnheader', tip: c.tip, type: c.sort ? 'button' : null }, h('span.ellipsis', c.label ?? ''), ind);
    th.__ind = ind;
    if (c.sort) th.addEventListener('click', () => {
      sort = { key: c.key, dir: sort && sort.key === c.key ? (sort.dir === 'asc' ? 'desc' : 'asc') : c.sortDir || 'asc' };
      applySort();
      if (opts.onSort) opts.onSort({ ...sort });
    });
    head.appendChild(th);
    return th;
  });

  const pool = [];
  function makeRow() {
    const cells = cols.map((c) => h('div.dt-td' + (c.align ? '.' + c.align : '') + (c.num ? '.num' : ''), { role: 'cell' }));
    const row = h('div.dt-row', { role: 'row' }, cells);
    row.__cells = cells;
    row.__txt = new Array(cols.length).fill(undefined);
    row.__cls = new Array(cols.length).fill('');
    row.__y = -1;
    row.__rc = '';
    row.__row = null;
    body.appendChild(row);
    pool.push(row);
    return row;
  }

  function paint() {
    if (destroyed) return;
    const top = scroll.scrollTop;
    const vh = scroll.clientHeight || 600;
    const first = Math.max(0, Math.floor(top / rowH) - OVERSCAN);
    const last = Math.min(rows.length, Math.ceil((top + vh) / rowH) + OVERSCAN);
    const need = Math.max(0, last - first);
    while (pool.length < need) makeRow();
    for (let i = 0; i < pool.length; i++) {
      const rowEl = pool[i];
      if (i >= need) { if (rowEl.__row !== null) { rowEl.style.display = 'none'; rowEl.__row = null; rowEl.__y = -1; } continue; }
      const r = rows[first + i];
      if (rowEl.__row === null) rowEl.style.display = '';
      rowEl.__row = r;
      const y = (first + i) * rowH;
      if (rowEl.__y !== y) { rowEl.__y = y; rowEl.style.transform = `translateY(${y}px)`; }
      const k = keyOf(r);
      let rc = opts.rowClass ? opts.rowClass(r) || '' : '';
      if (k === selected) rc += ' selected';
      if (k === hover) rc += ' hover';
      if (rowEl.__rc !== rc) { rowEl.__rc = rc; rowEl.className = 'dt-row' + (rc ? ' ' + rc : ''); }
      const cells = rowEl.__cells;
      for (let c = 0; c < cols.length; c++) {
        const col = cols[c];
        const td = cells[c];
        if (col.cls) {
          const cc = col.cls(r) || '';
          if (rowEl.__cls[c] !== cc) { rowEl.__cls[c] = cc; td.className = 'dt-td' + (col.align ? ' ' + col.align : '') + (col.num ? ' num' : '') + (cc ? ' ' + cc : ''); }
        }
        if (col.update) { col.update(td, r); continue; }
        let v = col.render ? col.render(r) : r[col.key];
        v = v === null || v === undefined ? '' : String(v);
        if (rowEl.__txt[c] !== v) { rowEl.__txt[c] = v; td.textContent = v; }
      }
    }
  }
  const schedule = rafThrottle(paint);

  function applySort() {
    cols.forEach((c, i) => {
      const on = sort && sort.key === c.key;
      if (on) ths[i].setAttribute('aria-sort', sort.dir === 'desc' ? 'descending' : 'ascending'); else ths[i].removeAttribute('aria-sort');
      ths[i].__ind.textContent = on ? (sort.dir === 'desc' ? '↓' : '↑') : '';
    });
    const col = sort && cols.find((c) => c.key === sort.key);
    rows = col ? sortRows(raw, col, sort.dir, keyOf) : raw;
    body.style.height = rows.length * rowH + 'px';
    emptyEl.classList.toggle('hidden', rows.length > 0);
    paint();
  }

  const rowFromEvent = (e) => { const r = e.target instanceof Element ? e.target.closest('.dt-row') : null; return r && r.__row ? r.__row : null; };
  scroll.addEventListener('scroll', schedule, { passive: true });
  const stopResize = onResize(scroll, schedule);
  if (opts.onRowClick) body.addEventListener('click', (e) => { const r = rowFromEvent(e); if (r) opts.onRowClick(r, e); });
  if (opts.onRowHover) {
    let last = null;
    body.addEventListener('pointerover', (e) => { const r = rowFromEvent(e); const k = r ? keyOf(r) : null; if (k !== last) { last = k; opts.onRowHover(r); } });
    body.addEventListener('pointerleave', () => { if (last !== null) { last = null; opts.onRowHover(null); } });
  }
  scroll.addEventListener('keydown', (e) => {
    if (e.key !== 'ArrowDown' && e.key !== 'ArrowUp' && e.key !== 'Enter') return;
    if (!rows.length) return;
    let i = rows.findIndex((r) => keyOf(r) === selected);
    if (e.key === 'Enter') { if (i >= 0 && opts.onRowClick) opts.onRowClick(rows[i], e); return; }
    e.preventDefault();
    i = e.key === 'ArrowDown' ? Math.min(rows.length - 1, i + 1) : Math.max(0, i - 1);
    if (opts.onRowClick) opts.onRowClick(rows[i], e); else api.setSelected(keyOf(rows[i]));
    api.scrollToKey(keyOf(rows[i]));
  });

  const api = {
    el,
    /** Replace the data. Rows are re-sorted and only visible cells are repainted. */
    setRows(list) { raw = list || []; applySort(); },
    /** Current rows in display order. */
    rows: () => rows,
    setSort(key, dir = 'asc') { sort = key ? { key, dir } : null; applySort(); },
    getSort: () => (sort ? { ...sort } : null),
    setSelected(key) { if (selected !== key) { selected = key; paint(); } },
    setHover(key) { if (hover !== key) { hover = key; paint(); } },
    /** Scroll the row with this key into view (no-op when absent). */
    scrollToKey(key) {
      const i = rows.findIndex((r) => keyOf(r) === key);
      if (i < 0) return false;
      const y = i * rowH;
      const vh = scroll.clientHeight - 30;
      if (y < scroll.scrollTop) scroll.scrollTop = y;
      else if (y + rowH > scroll.scrollTop + vh) scroll.scrollTop = y + rowH - vh;
      return true;
    },
    /** Repaint visible rows (e.g. after a unit change). */
    refresh() { for (const r of pool) { r.__txt.fill(undefined); } paint(); },
    destroy() { destroyed = true; stopResize(); schedule.cancel(); el.remove(); },
  };
  applySort();
  return api;
}

// ---------------------------------------------------------------------------------------------

export { icon, iconMarkup, iconNames };

/** The whole kit as one object (this is what pages receive as ctx.ui). */
export const ui = {
  h, append, mount, clear, setText, setClass, escapeHtml, debounce, throttle, rafThrottle, onResize, copy, prefs, loadCss,
  icon, iconMarkup, iconNames, tip,
  card, stat, button, iconButton, badge, sourceBadge, sourceLabel, flag, flagText, signalBar, signalLevel, meter, kv, section, legend,
  input, select, toggle, checkbox, slider, segmented, field, tabs,
  skeleton, empty, loading, error, load,
  toast, modal, confirm, alert, drawer, popover, menu,
  table, dataTable,
};

export default ui;
