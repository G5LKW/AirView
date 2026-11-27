// net/consent.js - the canonical consent and notice texts C1-C8 of docs/aggregator/CONSENT.md, word for word
// (DESIGN §10.5, §8; LIA.md). Pages refer to the text by its ID and render it with consentBlock(); the Go test
// TestConsentTextsMatchDoc (web/consent_test.go) fails when a text here no longer matches CONSENT.md.
//
// Placement (CONSENT.md "Shown"):
//   C1  claim page, station Location tab, feeder page                      (informs: timestamps reveal the location)
//   C2  station Privacy & data tab, next to the pin control                 (asks: public pin, opt-in)
//   C3  station Privacy & data tab, next to "show the name"                 (asks: public name, opt-in)
//   C4  station Privacy & data tab, next to "publish statistics"            (asks: public statistics, opt-in)
//   C5  feeder page next to the readsb / mlat-client lines, claim page (UUID tab)
//   C6  claim page; station Location tab as a notice when a surveyed position is in use
//   C7  under the account registration form
//   C8  station Privacy & data tab and the Account page (export / delete)
//
// Placeholders: C6 notice {σ} {Δ}, C8 {backup days}; fill() replaces them.

import { h, icon } from '../ui.js';

export const C1 = "If your receiver sends timestamps, the network operator can determine its location precisely, whatever you enter. Other users and the public cannot. Choose 'No timestamps (no MLAT)' if you do not want this.";

export const C2 = {
  title: 'Show my station on the public map?',
  options: [
    ['Hidden', '(default): no pin.'],
    ['Region', ': a point placed at random inside a 0.5-degree area around your station, about 55 by 35 km in the UK.'],
    ['Area', ': the same inside a 0.1-degree area, about 11 by 7 km. Where fewer than 3 stations are nearby it is shown as Region.'],
  ],
  text: 'The point is chosen once and stays put, so it cannot be averaged out. Your exact position is never public. You can change this at any time.',
};

export const C3 = {
  title: 'Show my station name publicly?',
  text: 'Others will see the name next to the station\'s public statistics and, if you chose a pin, next to the pin. Do not use your callsign or real name if you want to stay anonymous: a name together with even a rough area can identify you.',
};

export const C4 = {
  title: 'Publish my station\'s statistics?',
  text: 'Only figures that do not reveal where your station is: message rate, uptime, aircraft per day, positions per day and MLAT contribution. Range, coverage, polar plots and records stay private to you.',
};

export const C5 = 'These connections are not encrypted: your receiver\'s timing data, your UUID and, for MLAT, the position you enter cross the internet in clear text. Your UUID identifies your station; keep it private. If your mlat-client does not set privacy mode, the network shows a Region pin and your mlat-client user name, as other networks do. You can change both after you claim the station.';

export const C6 = 'The network can measure your receiver\'s position from its timing. If that measurement is precise (50 m or better) and consistent, it is used for MLAT automatically, even if you entered a different position or none. Only you and the network admins can see it. You can correct it or override it on your station page at any time.';

export const C6_NOTICE = 'We measured your station\'s position to about {σ} m and now use it for MLAT. It differs from the position you entered by {Δ} m.';
export const C6_NOTICE_LINK = 'Review location';

export const C7 = 'We store your user name, a one-way hash of your password, your authenticator secret if you turn on two-factor sign-in, and the times you sign in. We ask for no e-mail address, so we cannot reset your password by e-mail: keep your recovery codes. How we use your data:';
export const C7_LINK = 'privacy notice';

export const C8 = {
  before: 'You can switch off public display at any time; it takes effect immediately. ',
  export: 'Export',
  middle: ' downloads everything we hold about you. ',
  delete: 'Delete account',
  after: ' removes your account and, if you choose, your stations, their positions, statistics and logs at once; backups expire within {backup days} days. Aircraft positions already merged into the shared map cannot be separated out again.',
};

/** Replaces {σ}, {Δ}, {backup days} placeholders. */
export function fill(text, values = {}) {
  return String(text).replace(/\{(σ|Δ|backup days)\}/g, (m, k) => (values[k] !== undefined && values[k] !== null && values[k] !== '' ? String(values[k]) : m));
}

/** The privacy notice is published by the operator (aggregator.privacyNoticeUrl; template docs/aggregator/PRIVACY-NOTICE.md). */
export function privacyHref(join) {
  const u = join?.privacyNotice;
  return typeof u === 'string' && (/^https?:\/\/[^\s"'<>@]+$/.test(u) || /^\/(?!\/)[^\s"'<>]*$/.test(u)) ? u : '';
}

/**
 * A consent / notice block: <div class="net-consent" data-consent="C2"> with the canonical text.
 * `extra` (optional) is appended after the text (a checkbox, a link ...).
 */
export function consentBlock(id, opts = {}) {
  const box = h('div.net-consent', { 'data-consent': id }, icon('shield'));
  const body = h('div.net-consent-body');
  box.append(body);
  switch (id) {
    case 'C1': body.append(h('p', C1)); break;
    case 'C2':
      body.append(h('p', h('b', C2.title)),
        h('ul', C2.options.map(([label, rest]) => h('li', h('b', label), rest.startsWith(':') ? rest : ' ' + rest))),
        h('p', C2.text));
      break;
    case 'C3': body.append(h('p', h('b', C3.title), ' ', C3.text)); break;
    case 'C4': body.append(h('p', h('b', C4.title), ' ', C4.text)); break;
    case 'C5': body.append(h('p', C5)); break;
    case 'C6': body.append(h('p', C6)); break;
    case 'C7': body.append(h('p', C7, ' ', opts.privacyHref ? h('a', { href: opts.privacyHref, target: '_blank', rel: 'noopener' }, C7_LINK) : C7_LINK, '.')); break;
    case 'C8': {
      const days = opts.backupDays;
      body.append(h('p', C8.before, h('b', C8.export), C8.middle, h('b', C8.delete), fill(C8.after, { 'backup days': Number.isFinite(days) ? days : 30 })));
      break;
    }
    default: throw new Error('unknown consent text ' + id);
  }
  if (opts.extra) body.append(opts.extra);
  if (opts.compact) box.classList.add('compact');
  return box;
}

let joinCache = null; // { at, promise }

/**
 * The network's join facts (GET /api/public/join): backup days for C8, the privacy-notice link for C7, the
 * listener ports for the feeder page. Cached for a minute; resolves to {} on any error (texts then use their
 * defaults).
 */
export function joinInfo(api) {
  const now = Date.now();
  if (!joinCache || now - joinCache.at > 60000) {
    joinCache = { at: now, promise: api.get('/api/public/join', { timeout: 10000 }).then((r) => r || {}, () => ({})) };
  }
  return joinCache.promise;
}

/** Plain text of a consent ID (tests, the feeder CLI mirror). */
export function consentText(id) {
  switch (id) {
    case 'C1': return C1;
    case 'C2': return [C2.title, ...C2.options.map(([l, r]) => l + (r.startsWith(':') ? r : ' ' + r)), C2.text].join('\n');
    case 'C3': return C3.title + ' ' + C3.text;
    case 'C4': return C4.title + ' ' + C4.text;
    case 'C5': return C5;
    case 'C6': return C6;
    case 'C7': return C7 + ' [' + C7_LINK + '].';
    case 'C8': return C8.before + C8.export + C8.middle + C8.delete + C8.after;
    default: return '';
  }
}
