// net/locationview.js - the parts of the station Location section (pages/me.js) added for phase 3
// (DESIGN §10, §11 "Location", §19.3; WP-W3-wire):
//
//   comparisonCard  declared versus measured: a north-up plot of the declared position, the survey with its
//                   95 % ellipse, the receiver GNSS and the approximate Locate Me v2 circle, plus the distance
//                   and the verdict of the declared-position check
//   surveyCard      the reverse-MLAT survey: running job, last result (σ, aircraft, stable / suspect and why,
//                   MSL / HAE verdict), history, next check; "Survey now", "Use this location" (the confirm
//                   flow) and the owner override of the automatic use for MLAT
//   locateCard      Locate Me v2 for a station without a position: the approximate position and its 95 % circle
//   noticesCard     the station's location notices (finished surveys, declared check, automatic use, ...)
//
// Exact positions are owner / admin data: these cards exist only on /me pages, which the server serves to the
// owner and the admin only.

import { ui } from '../ui.js';
import { units } from '../units.js';
import { EP } from './endpoints.js';
import { v } from './view.js';
import { C1 } from './consent.js';
import * as L from './location.js';

const { h } = ui;
const SVG_NS = 'http://www.w3.org/2000/svg';
function s(tag, attrs = {}, ...kids) {
  const el = document.createElementNS(SVG_NS, tag);
  for (const [k, val] of Object.entries(attrs)) if (val !== null && val !== undefined) el.setAttribute(k, String(val));
  for (const k of kids.flat()) if (k) el.append(k);
  return el;
}

const COLORS = { declared: 'var(--amber)', survey: 'var(--cyan)', gnss: 'var(--green)', locate: 'var(--violet)' };
const LABELS = { declared: 'Declared', survey: 'Network survey (95 % ellipse)', gnss: 'Receiver GNSS', locate: 'Approximate (Locate Me v2, 95 %)' };

const posText = (p) => (L.hasPos(p) ? `${units.latlon(p.lat, p.lon)}${Number.isFinite(p.alt ?? p.hae) ? ' · ' + units.height(p.alt ?? p.hae) : ''}` : units.DASH);

/** The survey result shown in the plot: the last valid one (status) or the engine's (mlatPosition.survey). */
export function surveyPoint(status, mp) {
  const l = status?.last;
  if (l && l.valid && L.hasPos(l)) return { lat: l.lat, lon: l.lon, alt: l.alt, sigmaHM: l.sigmaHM, declaredLimitM: l.declaredLimitM, ellipse: l.ellipse95A > 0 ? { a: l.ellipse95A, b: l.ellipse95B, azDeg: l.ellipseAzDeg } : { a: 2.45 * l.sigmaHM, b: 2.45 * l.sigmaHM, azDeg: 0 }, stable: l.stable, suspect: l.suspect };
  if (mp?.survey && L.hasPos(mp.survey)) return { ...mp.survey, ellipse: { a: 2.45 * mp.survey.sigmaHM, b: 2.45 * mp.survey.sigmaHM, azDeg: 0 } };
  return null;
}

/**
 * Declared versus measured. data: { declared, survey (surveyPoint), gnss, locate (Locate2Result) }.
 */
export function comparisonCard(data) {
  const { declared, survey, gnss, locate } = data;
  const items = [];
  if (survey) items.push({ id: 'survey', pos: survey, ellipse: survey.ellipse });
  if (L.hasPos(declared)) items.push({ id: 'declared', pos: declared });
  if (L.hasPos(gnss)) items.push({ id: 'gnss', pos: gnss });
  const loc = locate && locate.ok && L.hasPos(locate) ? locate : null;
  if (loc && !survey) items.push({ id: 'locate', pos: loc, r: (loc.r95Km || 0) * 1000 });
  const lay = L.plotLayout(items, 260, 22);
  const body = h('div.col.gap-3');
  if (!lay) {
    body.append(ui.empty({ icon: 'target', title: 'Nothing to compare yet', text: 'Declare the antenna position below, or wait for the network survey.', compact: true }));
    return ui.card({ title: 'Declared and measured', icon: 'target', body });
  }
  const svg = s('svg', { viewBox: `0 0 ${lay.size} ${lay.size}`, class: 'net-locplot', role: 'img', 'aria-label': 'Declared and measured positions, north up' });
  svg.append(s('rect', { x: 0, y: 0, width: lay.size, height: lay.size, rx: 8, fill: 'var(--bg-1)', stroke: 'var(--line)' }));
  // cross hairs and north arrow
  svg.append(s('path', { d: `M${lay.size / 2} 8V${lay.size - 8}M8 ${lay.size / 2}H${lay.size - 8}`, stroke: 'var(--line)', 'stroke-dasharray': '2 4' }));
  svg.append(s('text', { x: lay.size - 14, y: 18, 'text-anchor': 'middle', 'font-size': 11, fill: 'var(--text-3)' }, 'N'));
  svg.append(s('path', { d: `M${lay.size - 14} 22l-4 9h8z`, fill: 'var(--text-3)' }));
  for (const it of lay.items) {
    const c = COLORS[it.id];
    if (it.ellipse) svg.append(s('ellipse', { cx: it.x, cy: it.y, rx: Math.max(it.ellipse.rx, 1.5), ry: Math.max(it.ellipse.ry, 1.5), transform: `rotate(${it.ellipse.rotDeg} ${it.x} ${it.y})`, fill: c, 'fill-opacity': 0.15, stroke: c }));
    if (it.r) svg.append(s('circle', { cx: it.x, cy: it.y, r: it.r, fill: c, 'fill-opacity': 0.1, stroke: c, 'stroke-dasharray': '4 3' }));
  }
  for (const it of lay.items) {
    const c = COLORS[it.id];
    const mark = it.id === 'declared'
      ? s('path', { d: `M${it.x - 5} ${it.y - 5}l10 10M${it.x + 5} ${it.y - 5}l-10 10`, stroke: c, 'stroke-width': 2.5 })
      : s('circle', { cx: it.x, cy: it.y, r: 4, fill: c, stroke: 'var(--bg-0)', 'stroke-width': 1.5 });
    svg.append(s('g', {}, s('title', {}, LABELS[it.id]), mark));
  }
  // scale bar
  const bar = L.niceLength(lay.spanM / 4);
  if (bar > 0) {
    const px = bar * lay.scale;
    svg.append(s('path', { d: `M12 ${lay.size - 14}h${px}M12 ${lay.size - 18}v8M${12 + px} ${lay.size - 18}v8`, stroke: 'var(--text-2)', 'stroke-width': 1.5 }));
    svg.append(s('text', { x: 12 + px / 2, y: lay.size - 22, 'text-anchor': 'middle', 'font-size': 11, fill: 'var(--text-2)' }, L.fmtDistance(bar)));
  }
  body.append(h('div.net-locplot-wrap', svg, ui.legend(lay.items.map((it) => ({ label: LABELS[it.id], color: COLORS[it.id] })))));
  const rows = [];
  const cmp = survey ? L.compareDeclared(declared, survey) : null;
  if (cmp) {
    rows.push(['Declared vs survey', `${L.fmtDistance(cmp.distM)} to the ${cmp.dir} (limit ${L.fmtDistance(cmp.limitM)})`]);
    body.append(cmp.agrees
      ? h('div.net-note', ui.icon('check'), h('span', 'Your declared position agrees with the network\'s measurement.'))
      : h('div.net-warn', ui.icon('alert'), h('span', `Your declared position is ${L.fmtDistance(cmp.distM)} from where the network measures your antenna. Check it, or use the measured position.`)));
  }
  if (survey && L.hasPos(gnss)) rows.push(['GNSS vs survey', L.fmtDistance(L.distanceM(gnss, survey))]);
  if (loc && survey) rows.push(['Locate Me v2 vs survey', `${L.fmtDistance(L.distanceM(loc, survey))} (its 95 % circle: ${L.fmtDistance((loc.r95Km || 0) * 1000)})`]);
  if (rows.length) body.append(ui.kv(rows));
  return ui.card({ title: 'Declared and measured', icon: 'target', sub: 'North up; only you and the network operator see this', body });
}

/**
 * The network survey card. opts: { ctx, s (station detail), status (GET survey), estimates, mp (mlatPosition),
 * admin, reload() }.
 */
export function surveyCard(opts) {
  const { ctx, s: st, status, mp, admin, reload } = opts;
  const { api } = ctx;
  const body = h('div.col.gap-3');
  const statusLine = h('p', L.surveyStatusText(status));
  body.append(statusLine);
  const l = status?.last;
  if (status?.running) {
    body.append(ui.kv([
      ['Started', status.started ? `${units.dateTime(status.started)} (${units.ago(status.started)})` : units.DASH],
      ['Observations', v(status.observed, units.int)],
      ['Why', L.reasonText(status.reason) || units.DASH],
    ]));
  }
  if (l) {
    const rows = [
      ['Last survey', l.finished ? `${units.dateTime(l.finished)} (${units.ago(l.finished)})` : units.DASH],
      ['Result', h('span.row.gap-2', l.valid ? (l.suspect ? ui.badge('inconsistent', 'amber') : l.stable ? ui.badge('stable', 'green') : ui.badge('not stable yet', 'other')) : ui.badge(l.state || 'no result', 'other'), l.autoUse ? ui.badge('used for MLAT automatically', 'cyan') : '')],
    ];
    if (l.valid) {
      rows.push(['Position', posText(l)]);
      rows.push(['Uncertainty (1σ)', `±${L.fmtDistance(l.sigmaHM)} horizontal · ±${L.fmtDistance(l.sigmaVM)} vertical`]);
      // A result restored after a restart keeps σ but not the ellipse or the aircraft count.
      if (Number.isFinite(l.ellipse95A) && l.ellipse95A > 0) rows.push(['95 % ellipse', `${L.fmtDistance(l.ellipse95A)} × ${L.fmtDistance(l.ellipse95B)}, long axis ${Math.round(l.ellipseAzDeg || 0)}°`]);
      rows.push(['Measured from', l.aircraft > 0 ? `${units.int(l.aircraft)} aircraft, ${units.int(l.observations || 0)} observations` : `${units.int(l.observations || 0)} observations`]);
    }
    if (l.suspectWhy?.length) rows.push(['Why not used', l.suspectWhy.join('; ')]);
    if (l.altRefTested) rows.push(['Altitude reference', l.altRef === 'msl' ? 'above mean sea level (MSL)' : l.altRef === 'hae' ? 'above the WGS-84 ellipsoid (HAE)' : 'not decided yet']);
    if (status.nextCheck) rows.push(['Next check', `${units.dateTime(status.nextCheck)}`]);
    body.append(ui.kv(rows));
  }
  // actions
  const actions = h('div.row.gap-2', { style: 'flex-wrap:wrap' });
  const start = ui.button({ label: status?.running ? 'Survey running' : 'Survey now', icon: 'target', size: 'sm', disabled: !status?.enabled || !!status?.running, onClick: async () => {
    start.setLoading(true);
    try {
      const r = await api.post(admin ? EP.adminSurvey(st.sid) : EP.mySurvey(st.sid), {});
      ui.toast.success(r?.queued ? 'Survey queued behind other surveys' : 'Survey started; it takes a few minutes');
      reload?.();
    } catch (e) {
      if (e.status === 409) ui.alert('This station cannot be surveyed', h('div.col.gap-2', h('p', 'The station sends no timestamps, so the network cannot measure its position.'), h('p.muted', C1)));
      else if (e.status === 429) ui.toast.info('A survey of this station ran within the last hour. Try again later.');
      else ui.toast.error(e.message);
    } finally { start.setLoading(false); }
  } });
  actions.append(start);
  if (admin && status?.running) {
    actions.append(ui.button({ label: 'Cancel', icon: 'x', size: 'sm', variant: 'ghost', onClick: async () => {
      try { await api.del(EP.adminSurvey(st.sid)); ui.toast.success('Survey cancelled'); reload?.(); } catch (e) { ui.toast.error(e.message); }
    } }));
  }
  const est = L.confirmableSurvey(opts.estimates);
  if (est && st.location?.status !== 'confirmed') {
    actions.append(ui.button({ label: 'Use this location', icon: 'check', size: 'sm', variant: 'primary', onClick: () => confirmSurveyFlow(ctx, st, est, reload) }));
  }
  if (mp && mp.survey) {
    actions.append(ui.button({ label: mp.surveyRejected ? 'Allow it for MLAT' : 'Do not use it for MLAT', size: 'sm', variant: 'ghost',
      tip: 'Precise survey positions (σ ≤ 50 m) are used for MLAT automatically unless you reject them here.',
      onClick: async () => {
        try { await api.put(EP.mySurveyUse(st.sid), { reject: !mp.surveyRejected }); ui.toast.success('Saved'); reload?.(); } catch (e) { ui.toast.error(e.message); }
      } }));
  }
  body.append(actions);
  const hist = (status?.history || []).filter((x) => x && x.job !== l?.job);
  if (hist.length) {
    body.append(h('details.net-details', h('summary', `Earlier surveys (${hist.length})`), ui.table({
      columns: [
        { key: 'finished', label: 'When', render: (x) => (x.finished ? units.ago(x.finished) : units.DASH) },
        { key: 'reason', label: 'Why', render: (x) => x.reason || units.DASH },
        { key: 'state', label: 'Outcome', render: (x) => (x.valid ? (x.suspect ? 'inconsistent' : x.stable ? 'stable' : 'not stable') : x.state) },
        { key: 'sigmaHM', label: '1σ', align: 'right', render: (x) => (Number.isFinite(x.sigmaHM) ? '±' + L.fmtDistance(x.sigmaHM) : units.DASH) },
        { key: 'declaredDistM', label: 'Declared off by', align: 'right', render: (x) => (x.declaredChecked ? L.fmtDistance(x.declaredDistM) : units.DASH) },
      ], rows: hist })));
  }
  return ui.card({ title: 'Network survey', icon: 'radar', sub: 'Reverse MLAT: your timestamps against aircraft with known positions', body });
}

/**
 * The confirm flow: shows what will happen and the measured position against the declared one, then makes
 * the survey estimate the station's confirmed position (POST .../location/confirm).
 */
export async function confirmSurveyFlow(ctx, st, est, reload) {
  const declared = st.location?.declared;
  const cmp = L.compareDeclared(declared, { ...est, sigmaHM: est.sigmaM });
  const msg = h('div.col.gap-3',
    h('p', 'The measured position becomes your station\'s confirmed location. MLAT uses it from now on, and the network keeps checking it every few hours.'),
    ui.kv([
      ['Measured position', posText(est)],
      ['Uncertainty (1σ)', '±' + L.fmtDistance(est.sigmaM)],
      ['Measured', est.t ? units.ago(est.t) : units.DASH],
      cmp ? ['Your declared position', `${L.fmtDistance(cmp.distM)} away (${cmp.dir})`] : null,
    ]),
    cmp && !cmp.agrees ? h('div.net-warn', ui.icon('alert'), h('span', 'This is far from the position you declared. If you are sure about your declared position, keep it instead and check the antenna cable and the receiver clock.')) : null,
    h('p.muted', 'You can change it at any time by declaring another position.'));
  if (!(await ui.confirm({ title: 'Use the measured position?', message: msg, confirmLabel: 'Use this location', width: 520 }))) return false;
  try {
    const r = await ctx.api.post(EP.myLocationConfirm(st.sid), { estimate: est.id });
    ui.toast.success('Location confirmed');
    if (r?.sid) ctx.net.updateStation(r);
    reload?.();
    return true;
  } catch (e) {
    ui.toast.error(e.message);
    return false;
  }
}

/** Locate Me v2 for a station without a position. r: Locate2Result (or an error). */
export function locateCard(r, err) {
  const body = h('div.col.gap-3');
  if (err) {
    body.append(h('p.muted', err.status === 404 ? 'Not available on this aggregator.' : err.message));
  } else if (!r || !r.ok) {
    body.append(h('p', r?.reason || 'Not enough reception yet: the estimate needs about an hour of traffic.'));
  } else {
    body.append(ui.kv([
      ['Approximate position', units.latlon(r.lat, r.lon)],
      ['Within (95 %)', L.fmtDistance((r.r95Km || 0) * 1000)],
      ['From', `${units.int(r.samples || 0)} positions of ${units.int(r.aircraft || 0)} aircraft`],
      Number.isFinite(r.errorKm) ? ['Off by', `${L.fmtDistance(r.errorKm * 1000)} (vs ${r.refSource || 'reference'})`] : null,
    ]));
    if (Array.isArray(r.notes) && r.notes.length) body.append(h('ul.muted', r.notes.slice(0, 4).map((n) => h('li', n))));
  }
  body.append(h('p.muted', 'Estimated from what your station receives (radio horizon, skyline, signal levels). It is only a coarse hint for the network survey and is never used for MLAT.'));
  return ui.card({ title: 'Approximate location', icon: 'locate', body });
}

/** The station's location notices. list: [{ t, level, text }]. */
export function noticesCard(list) {
  const items = (list || []).slice(0, 20);
  const icon = { ok: 'check-circle', warn: 'alert', info: 'info' };
  const body = items.length
    ? h('ul.net-notices', items.map((n) => h('li.net-notice.' + (n.level || 'info'), ui.icon(icon[n.level] || 'info'), h('span.grow', n.text), h('span.muted', n.t ? units.ago(n.t) : ''))))
    : ui.empty({ icon: 'bell', title: 'No location notices in the last 30 days', compact: true });
  return ui.card({ title: 'Location notices', icon: 'bell', body });
}
