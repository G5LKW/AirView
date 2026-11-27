// pages/check.js - Feed Check (#/me/check, DESIGN §11): the feeder links that come from the visitor's own
// address (exact IPv4, IPv6 /64), with only non-locating fields: protocol, the last 6 characters of the UUID,
// UUID warnings, connected since, msg/s, pos/s, kbps, RTT, clock class, MLAT synced - and a Claim button.
// The server matches the address. One public address can be shared by many subscribers (carrier-grade NAT), so
// without a login only the number of links is shown; signed in, the links of your own stations show in full and
// the others only with what claiming needs (PRIV-A8).
//
// API: GET /api/me/check -> { links: [...] } (net/endpoints.js).

import { EP, normCheck } from '../net/endpoints.js';
import { clockLabel, PROTO_LABELS } from '../net/model.js';
import { pageHead, failState, v } from '../net/view.js';
import { loginDialog } from '../net/auth.js';

const REFRESH_MS = 5000;

export default {
  id: 'check',
  title: 'Feed Check',

  async mount(container, ctx) {
    const { api, ui, units } = ctx;
    const { h } = ui;
    const head = pageHead('Feed Check', 'Feeder links from your internet address',
      ui.button({ label: 'Refresh', icon: 'refresh', onClick: () => refresh(true) }));
    const body = h('div');
    container.append(h('div.page-inner.net-narrow', head, body,
      h('p.muted.net-privacy-note', ui.icon('info'),
        'Only links from the address you are browsing from are shown (IPv6: the same /64). Locations, receivers per aircraft and full UUIDs are never shown here. ',
        'Your internet provider may share one address between many customers (carrier-grade NAT), so the details of a link are shown only for your own stations.')));

    const linkCard = (l) => {
      const warn = [];
      if (l.uuidMissing) warn.push(h('div.net-warn', ui.icon('alert'), h('span', h('b', 'No UUID. '), 'This feeder does not send a UUID, so the network cannot tell it apart from other feeders behind the same address and it cannot be claimed. Set a UUID in the feeder (readsb: --uuid-file / mlat-client: --uuid).')));
      if (l.uuidMismatch) warn.push(h('div.net-warn', ui.icon('alert'), h('span', h('b', 'UUID mismatch. '), 'Another link from your address uses a different UUID for what looks like the same receiver (e.g. the Beast feed and mlat-client). Use one UUID per receiver so both links count as one station.')));
      if (l.admission === 'pending') warn.push(h('div.net-note', ui.icon('info'), h('span', 'Waiting for the network administrator to approve this station. Its data is kept apart until then.')));
      if (l.admission === 'denied' || l.admission === 'revoked') warn.push(h('div.net-warn', ui.icon('x-circle'), h('span', 'This station is not admitted to the network. Contact the operator.')));
      const claimBtn = l.claimable && !l.claimed
        ? ui.button({ label: 'Claim', icon: 'claim', variant: 'primary', size: 'sm', onClick: () => ctx.navigate(`me/claim?${new URLSearchParams({ proto: l.proto || '', kind: l.claimKind })}`) })
        : null;
      if (!l.claimed && l.claimKind === 'admin') warn.push(h('div.net-note', ui.icon('info'), h('span', 'This station has no UUID, so it cannot be claimed here. Ask the network administrator to assign it to your account.')));
      if (l.claimKind === 'code' && !l.claimed) warn.push(h('div.net-note', ui.icon('info'), h('span', 'Claim it with the code from "airview-feeder claim" or the status page of the feeder.')));
      if (!l.details) {
        return ui.card({
          title: PROTO_LABELS[l.proto] || l.proto || 'Link',
          sub: l.idTail ? `ID ${l.idTail}` : '',
          icon: l.proto === 'mlatc' ? 'target' : 'antenna',
          actions: [l.claimed ? ui.badge('claimed', 'other') : null, claimBtn].filter(Boolean),
          body: h('div.col.gap-3',
            warn.length ? h('div.col.gap-2', warn) : null,
            ui.kv([['Connected', l.since ? `${units.ago(l.since)} (${units.dateTime(l.since)})` : units.DASH]]),
            h('p.muted', 'Rates, clock and MLAT state are shown once the station is claimed by your account.')),
        });
      }
      const synced = l.mlatSynced === true ? ui.badge('synced', 'green') : l.mlatSynced === false ? ui.badge('not synced', 'amber') : null;
      return ui.card({
        title: (PROTO_LABELS[l.proto] || l.proto || 'Link') + (l.software ? ` · ${l.software}` : ''),
        sub: [l.stationName, l.idTail ? `ID ${l.idTail}` : null].filter(Boolean).join(' · '),
        icon: l.proto === 'mlatc' ? 'target' : 'antenna',
        actions: [l.claimed ? ui.badge(l.yours ? 'yours' : 'claimed', l.yours ? 'green' : 'other') : null, claimBtn].filter(Boolean),
        body: h('div.col.gap-3',
          warn.length ? h('div.col.gap-2', warn) : null,
          ui.kv([
            ['Connected', l.since ? `${units.ago(l.since)} (${units.dateTime(l.since)})` : units.DASH],
            ['Messages', v(l.msgRate, (x) => units.num(x, x < 100 ? 1 : 0) + ' /s')],
            ['Positions', v(l.posRate, (x) => units.num(x, 1) + ' /s')],
            ['Data rate', v(l.kBps, (x) => units.num(x, 1) + ' kB/s')],
            ['Round trip', v(l.rttMs, (x) => units.num(x, 0) + ' ms')],
            ['Clock', l.clockClass !== undefined ? clockLabel(l.clockClass) : units.DASH],
            ['MLAT', synced || (l.proto === 'beast' ? 'not used (Beast feeds are map-only)' : units.DASH)],
          ], { columns: 2 })),
      });
    };

    let shown = false;
    async function refresh(manual) {
      try {
        const r = await api.get(EP.feedCheck(), { signal: ctx.signal, timeout: 10000 });
        const links = normCheck(r);
        if (r && r.limited) {
          const n = r.count || 0;
          head.setSub(`${n} ${n === 1 ? 'link' : 'links'} from your address · updated ${units.clock(Date.now())}`);
          ui.mount(body, ui.empty({ icon: 'antenna', title: `${n} feeder ${n === 1 ? 'link' : 'links'} connected from your address`,
            text: 'Log in to see the details. Your address may be shared with other customers of your internet provider, so the details are shown only to the owner of a station.',
            action: ui.button({ label: 'Log in', icon: 'lock', variant: 'primary', onClick: () => loginDialog({ what: 'Log in to see your feeder links.' }) }) }));
          shown = true;
          return;
        }
        head.setSub(links.length ? `${links.length} ${links.length === 1 ? 'link' : 'links'} from your address · updated ${units.clock(Date.now())}` : 'Feeder links from your internet address');
        if (!links.length) {
          ui.mount(body, ui.empty({ icon: 'antenna', title: 'No feeder connected from your address',
            text: 'Start your feeder and wait a few seconds. If it uses another internet connection than this browser (mobile data, a VPN, a different site), open this page from there, or claim the feeder with its claim code instead.',
            action: ui.button({ label: 'Claim with a code', icon: 'claim', href: '#/me/claim' }) }));
        } else {
          ui.mount(body, h('div.col.gap-4', links.map(linkCard)));
        }
        shown = true;
      } catch (e) {
        if (e.aborted) return;
        if (!shown || manual) ui.mount(body, failState(e, { retry: () => refresh(true) }));
      }
    }
    ui.mount(body, ui.skeleton({ lines: 5, title: true }));
    await refresh();
    ctx.every(REFRESH_MS, () => { if (!document.hidden) refresh(false); });
  },

  onNet() {},
};
