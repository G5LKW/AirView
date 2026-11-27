// AirView icon set: original hand-drawn 24x24 stroke icons (no third-party icon font).
// Use ui.icon(name) / icon(name); unknown names fall back to a neutral dot so a typo never throws.

const P = {
  // navigation
  map: '<path d="M3 6.5 9 4l6 2.5L21 4v13.5L15 20l-6-2.5L3 20z"/><path d="M9 4v13.5M15 6.5V20"/>',
  plane: '<path d="M12 2.5c.9 0 1.5 1.2 1.5 2.6V9l7 4.2v2l-7-2v4.3l2.3 1.7v1.6L12 20l-3.800.8v-1.600l2.300-1.700v-4.300l-7 2v-2l7-4.200V5.100c0-1.400.6-2.600 1.500-2.600z"/>',
  chart: '<path d="M3 20.500h18"/><path d="M6.500 20.500v-7M11 20.500v-15M15.500 20.500v-10M20 20.500v-5"/>',
  antenna: '<path d="M12 9.500V21"/><path d="m8 21 4-11.500L16 21"/><path d="M9.700 16.500h4.600"/><circle cx="12" cy="7.500" r="1.600"/><path d="M7.800 11.200a5.200 5.200 0 0 1 0-7.400M16.200 3.800a5.200 5.200 0 0 1 0 7.400M4.900 13.400a9.300 9.300 0 0 1 0-11.800M19.100 1.600a9.300 9.300 0 0 1 0 11.800"/>',
  cloud: '<path d="M7 18.500h10a4 4 0 0 0 .6-7.960A6 6 0 0 0 6.200 9.100 4.700 4.700 0 0 0 7 18.500z"/>',
  forecast: '<path d="m3 17 5.500-5.500 4 4L21 7"/><path d="M15 7h6v6"/>',
  timer: '<circle cx="12" cy="13" r="8"/><path d="M12 9v4l2.500 2M9.500 2h5M12 2v3"/>',
  target: '<circle cx="12" cy="12" r="8"/><circle cx="12" cy="12" r="3"/><path d="M12 1.500v4M12 18.500v4M1.500 12h4M18.500 12h4"/>',
  settings: '<path d="M4 6h9M17 6h3M4 12h3M11 12h9M4 18h11M19 18h1"/><circle cx="15" cy="6" r="2"/><circle cx="9" cy="12" r="2"/><circle cx="17" cy="18" r="2"/>',
  wave: '<path d="M2 12h2.500l2-6 3 13 3-16 3 13 2-4H22"/>',
  bell: '<path d="M6 16.500V11a6 6 0 0 1 12 0v5.500l1.500 2h-15z"/><path d="M10 20.500a2 2 0 0 0 4 0"/>',
  replay: '<path d="M4 12a8 8 0 1 0 2.500-5.800L4 8.500M4 4v4.500h4.500"/><path d="m10.500 9 4.500 3-4.500 3z"/>',
  report: '<path d="M6 3h8.500L19 7.500V20a1 1 0 0 1-1 1H6a1 1 0 0 1-1-1V4a1 1 0 0 1 1-1z"/><path d="M14 3v5h5M8.500 12.500h7M8.500 16h7M8.500 9h3"/>',
  mountain: '<path d="m2.500 19.500 6.500-11 4 6.500 2.500-3.500 6 8z"/><path d="m7 11.500 2 1.500 2-1.500"/>',  // chrome
  menu: '<path d="M4 6.500h16M4 12h16M4 17.500h16"/>',
  x: '<path d="M6 6l12 12M18 6 6 18"/>',
  search: '<circle cx="11" cy="11" r="6.500"/><path d="m20 20-4.200-4.200"/>',
  layers: '<path d="m12 3 9 5-9 5-9-5z"/><path d="m3 12.500 9 5 9-5"/>',
  maximize: '<path d="M4 9V4h5M20 9V4h-5M4 15v5h5M20 15v5h-5"/>',
  minimize: '<path d="M9 4v5H4M15 4v5h5M9 20v-5H4M15 20v-5h5"/>',
  locate: '<circle cx="12" cy="12" r="7"/><circle cx="12" cy="12" r="1.500" fill="currentColor"/><path d="M12 2v3M12 19v3M2 12h3M19 12h3"/>',
  fit: '<path d="M3 8V5a2 2 0 0 1 2-2h3M16 3h3a2 2 0 0 1 2 2v3M21 16v3a2 2 0 0 1-2 2h-3M8 21H5a2 2 0 0 1-2-2v-3"/><rect x="8" y="8" width="8" height="8" rx="1"/>',
  plus: '<path d="M12 5v14M5 12h14"/>',
  minus: '<path d="M5 12h14"/>',
  'chevron-left': '<path d="m14.500 5.500-6.500 6.500 6.500 6.500"/>',
  'chevron-right': '<path d="m9.500 5.500 6.500 6.500-6.500 6.500"/>',
  'chevron-up': '<path d="m5.500 14.500 6.500-6.500 6.500 6.500"/>',
  'chevron-down': '<path d="m5.500 9.500 6.500 6.500 6.500-6.500"/>',
  'chevrons-right': '<path d="m6 6 6 6-6 6M13 6l6 6-6 6"/>',
  'chevrons-left': '<path d="m18 6-6 6 6 6M11 6l-6 6 6 6"/>',
  share: '<circle cx="18" cy="5" r="2.500"/><circle cx="6" cy="12" r="2.500"/><circle cx="18" cy="19" r="2.500"/><path d="m8.200 10.800 7.600-4.400M8.200 13.200l7.600 4.400"/>',
  'arrow-up': '<path d="M12 19V5M5.500 11.500 12 5l6.500 6.500"/>',
  'arrow-down': '<path d="M12 5v14M5.500 12.500 12 19l6.500-6.500"/>',
  'arrow-right': '<path d="M5 12h14M12.500 5.500 19 12l-6.500 6.500"/>',
  more: '<circle cx="5" cy="12" r="1.200" fill="currentColor"/><circle cx="12" cy="12" r="1.200" fill="currentColor"/><circle cx="19" cy="12" r="1.200" fill="currentColor"/>',
  panel: '<rect x="3" y="4" width="18" height="16" rx="2"/><path d="M15 4v16"/>',
  list: '<path d="M8.500 6h11.500M8.500 12h11.500M8.500 18h11.500"/><path d="M4 6h.500M4 12h.500M4 18h.500"/>',
  // status
  sun: '<circle cx="12" cy="12" r="4"/><path d="M12 2v2.500M12 19.500V22M2 12h2.500M19.500 12H22M4.900 4.900l1.800 1.800M17.300 17.300l1.800 1.800M4.900 19.100l1.800-1.800M17.300 6.700l1.800-1.800"/>',
  moon: '<path d="M20 14.500A8.500 8.500 0 0 1 9.500 4a8.500 8.500 0 1 0 10.500 10.500z"/>',
  twilight: '<path d="M2.500 18h19M6.500 18a5.500 5.500 0 0 1 11 0M12 6.500v3M4.800 11l2 2M19.200 11l-2 2"/>',
  alert: '<path d="M12 3.500 2.500 20h19z"/><path d="M12 10v4.500M12 17.200v.300"/>',
  info: '<circle cx="12" cy="12" r="9"/><path d="M12 11v6M12 7.400v.300"/>',
  check: '<path d="m4.500 12.500 5 5 10-11"/>',
  'check-circle': '<circle cx="12" cy="12" r="9"/><path d="m8 12.500 3 3 5-6"/>',
  'x-circle': '<circle cx="12" cy="12" r="9"/><path d="m9 9 6 6M15 9l-6 6"/>',
  signal: '<path d="M4 20v-3M9 20v-7M14 20V9M19 20V4"/>',
  radar: '<circle cx="12" cy="12" r="9"/><circle cx="12" cy="12" r="5"/><path d="M12 12 18.400 5.600"/><circle cx="12" cy="12" r="1" fill="currentColor"/>',
  compass: '<circle cx="12" cy="12" r="9"/><path d="m15.500 8.500-2 5-5 2 2-5z"/>',
  globe: '<circle cx="12" cy="12" r="9"/><path d="M3 12h18M12 3c2.800 3 2.800 15 0 18M12 3c-2.800 3-2.800 15 0 18"/>',
  pin: '<path d="M12 21.500s7-6.200 7-11.500a7 7 0 1 0-14 0c0 5.300 7 11.500 7 11.500z"/><circle cx="12" cy="10" r="2.500"/>',
  route: '<circle cx="6" cy="18" r="2.500"/><circle cx="18" cy="6" r="2.500"/><path d="M8.500 18H15a3.500 3.500 0 0 0 0-7H9a3.500 3.500 0 0 1 0-7h6.500"/>',
  rain: '<path d="M7 15.500h10a4 4 0 0 0 .6-7.960A6 6 0 0 0 6.200 6.100 4.700 4.700 0 0 0 7 15.500z"/><path d="m8.500 18.500-1 2.500M12.500 18.500l-1 2.500M16.500 18.500l-1 2.500"/>',
  wind: '<path d="M3 9h10.500a2.500 2.500 0 1 0-2.400-3.200M3 13h15a2.500 2.500 0 1 1-2.400 3.200M3 17h7"/>',
  thermo: '<path d="M10 13.500V5a2 2 0 0 1 4 0v8.500a4 4 0 1 1-4 0z"/><path d="M12 9v7"/>',
  database: '<ellipse cx="12" cy="6" rx="8" ry="3"/><path d="M4 6v6c0 1.700 3.600 3 8 3s8-1.300 8-3V6M4 12v6c0 1.700 3.600 3 8 3s8-1.300 8-3v-6"/>',
  ruler: '<path d="M3 16 16 3l5 5L8 21z"/><path d="m7 12 2 2M10 9l2 2M13 6l2 2"/>',
  // actions
  external: '<path d="M14 4h6v6M20 4l-9 9M18 14v4a2 2 0 0 1-2 2H6a2 2 0 0 1-2-2V8a2 2 0 0 1 2-2h4"/>',
  refresh: '<path d="M20 11a8 8 0 0 0-14.500-3.500L4 9M4 4v5h5M4 13a8 8 0 0 0 14.500 3.500L20 15M20 20v-5h-5"/>',
  power: '<path d="M12 3v8"/><path d="M6.300 7a8 8 0 1 0 11.400 0"/>',
  filter: '<path d="M3 5h18l-7 8.500V19l-4 2v-7.500z"/>',
  eye: '<path d="M2 12s3.600-7 10-7 10 7 10 7-3.600 7-10 7S2 12 2 12z"/><circle cx="12" cy="12" r="3"/>',
  'eye-off': '<path d="M4 4l16 16M9.900 5.300A9.600 9.600 0 0 1 12 5c6.400 0 10 7 10 7a17 17 0 0 1-3.200 4M6.300 6.800C3.600 8.700 2 12 2 12s3.600 7 10 7a9.600 9.600 0 0 0 4.200-1"/><path d="M9.900 9.900a3 3 0 0 0 4.200 4.200"/>',
  follow: '<path d="M12 2v4M12 18v4M2 12h4M18 12h4"/><circle cx="12" cy="12" r="3"/>',
  download: '<path d="M12 4v11M7 10.500l5 5 5-5M4.500 20h15"/>',
  upload: '<path d="M12 16V5M7 9.500l5-5 5 5M4.500 20h15"/>',
  copy: '<rect x="8" y="8" width="12" height="12" rx="2"/><path d="M16 8V6a2 2 0 0 0-2-2H6a2 2 0 0 0-2 2v8a2 2 0 0 0 2 2h2"/>',
  trash: '<path d="M4 7h16M9.500 7V4.500h5V7M6.500 7l1 13h9l1-13M10 11v5.500M14 11v5.500"/>',
  edit: '<path d="M4 20h4L19.500 8.500a2.100 2.100 0 0 0-3-3L5 17z"/><path d="m14.500 7.500 3 3"/>',
  save: '<path d="M5 4h11l4 4v11a1 1 0 0 1-1 1H5a1 1 0 0 1-1-1V5a1 1 0 0 1 1-1z"/><path d="M8 4v5h7V4M8 20v-6h8v6"/>',
  play: '<path d="M7 4.500v15l12-7.500z"/>',
  pause: '<path d="M8 5v14M16 5v14"/>',
  link: '<path d="M10 14a4 4 0 0 0 5.700 0l3-3a4 4 0 0 0-5.700-5.700l-1 1"/><path d="M14 10a4 4 0 0 0-5.700 0l-3 3a4 4 0 0 0 5.700 5.700l1-1"/>',
  lock: '<rect x="5" y="11" width="14" height="9" rx="2"/><path d="M8 11V8a4 4 0 0 1 8 0v3"/>',
  box: '<path d="M12 2.800 20.500 7.500v9L12 21.200 3.500 16.500v-9z"/><path d="M3.800 7.600 12 12.200l8.200-4.600M12 12.200v8.800"/>',
  help: '<circle cx="12" cy="12" r="9"/><path d="M9.500 9.500a2.500 2.500 0 1 1 3.700 2.200c-.8.4-1.200 1-1.200 1.800M12 16.800v.300"/>',
  bolt: '<path d="M13 2.500 5 13.500h6l-1 8 8-11h-6z"/>',
  history: '<path d="M4 12a8 8 0 1 0 2.500-5.800L4 8.500M4 4v4.500h4.500"/><path d="M12 8v4.500l3 1.500"/>',
  shield: '<path d="M12 3 4.500 6v5.500c0 4.500 3 8 7.500 9.500 4.500-1.500 7.500-5 7.500-9.500V6z"/>',
  user: '<circle cx="12" cy="8" r="4"/><path d="M4 21a8 8 0 0 1 16 0"/>',
  users: '<circle cx="9" cy="8" r="3.500"/><path d="M2.500 20a6.500 6.500 0 0 1 13 0"/><path d="M15.500 4.700a3.500 3.500 0 0 1 0 6.600M17.500 14.300A6.500 6.500 0 0 1 21.500 20"/>',
  logout: '<path d="M15 4h3a2 2 0 0 1 2 2v12a2 2 0 0 1-2 2h-3"/><path d="m10 8-4 4 4 4"/><path d="M6 12h10"/>',
  key: '<circle cx="8" cy="15" r="4"/><path d="m11 12 9-9M17 6l3 3M14 9l2 2"/>',
  claim: '<path d="M12 3v12"/><path d="m7 10 5 5 5-5"/><path d="M5 21h14"/>',
  dot: '<circle cx="12" cy="12" r="3" fill="currentColor"/>',
};

const NS = 'http://www.w3.org/2000/svg';

/** Names of all available icons. */
export const iconNames = Object.keys(P);

/**
 * Create an inline SVG icon element.
 * @param {string} name icon name (see iconNames)
 * @param {{size?: number, class?: string, title?: string}} [opts]
 * @returns {SVGSVGElement}
 */
export function icon(name, opts = {}) {
  const svg = document.createElementNS(NS, 'svg');
  svg.setAttribute('viewBox', '0 0 24 24');
  svg.setAttribute('class', 'icon' + (opts.class ? ' ' + opts.class : ''));
  svg.setAttribute('aria-hidden', opts.title ? 'false' : 'true');
  if (opts.size) { svg.style.width = opts.size + 'px'; svg.style.height = opts.size + 'px'; }
  svg.innerHTML = (opts.title ? `<title>${String(opts.title).replace(/[<&]/g, '')}</title>` : '') + (P[name] || P.dot);
  return svg;
}

/** Raw inner SVG markup for an icon (for building HTML strings). */
export function iconMarkup(name) {
  return `<svg class="icon" viewBox="0 0 24 24" aria-hidden="true">${P[name] || P.dot}</svg>`;
}
