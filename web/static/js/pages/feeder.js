// pages/feeder.js - "Feed this network" (#/feeder, network / aggregator mode, public): how to connect a receiver
// (DESIGN §12.2 "the aggregator's /feeder page also shows the two-line readsb/Ultrafeeder snippets"):
//   - the native AirView feeder (AVF/1, encrypted): `airview-feeder setup --server host:port`, then claim it;
//   - readsb / Ultrafeeder (Beast 30004 with a UUID) and mlat-client (31090): configuration lines with the
//     consent text C5 next to them (these links are not encrypted);
//   - C1 (timestamps reveal the location to the operator).
// Only listeners that are open are offered. The host is aggregator.publicHost, else the host name this page was
// loaded from (the server never echoes request headers).
//
// API: GET /api/public/join -> { network, name?, host, avfPort, beastPort, mlatPort, mlat, admission, registration,
// backupDays, privacyNotice }.

import { EP } from '../net/endpoints.js';
import { pageHead, failState } from '../net/view.js';
import { consentBlock } from '../net/consent.js';
import { snippetHost, feederSnippets } from '../net/model.js';

const ADMISSION_TEXT = {
  open: 'New stations are admitted automatically.',
  approve: 'New stations are admitted once the operator approves them; until then their data stays out of the public map and MLAT.',
  invite: 'New AirView feeders need an enrol token from the operator. Ask for one before you set up the feeder.',
};

export default {
  id: 'feeder',
  title: 'Feed this network',

  async mount(container, ctx) {
    const { api, ui } = ctx;
    const { h } = ui;
    const head = pageHead('Feed this network', 'Connect your receiver',
      ui.button({ label: 'Feed Check', icon: 'signal', href: '#/me/check' }),
      ui.button({ label: 'Claim a feeder', icon: 'claim', variant: 'primary', href: '#/me/claim' }));
    const body = h('div.col.gap-4');
    container.append(h('div.page-inner.net-narrow', head, body));
    body.append(ui.skeleton({ lines: 6 }));

    let join;
    try {
      join = await api.get(EP.publicJoin(), { signal: ctx.signal, timeout: 15000 });
    } catch (e) {
      if (e.aborted) return;
      ui.mount(body, failState(e));
      return;
    }
    if (ctx.signal.aborted) return;
    if (!join?.network) {
      ui.mount(body, ui.empty({ icon: 'antenna', title: 'This AirView is not a network', text: 'Feeding is available on AirView aggregators.' }));
      return;
    }
    if (join.name) head.setSub(`Connect your receiver to ${join.name}`);
    const host = snippetHost(join);
    const sn = feederSnippets(join, host);
    const line = (text) => h('div.net-snippet-row', h('code.net-snippet', text),
      ui.iconButton('copy', 'Copy', async () => { try { await ui.copy(text); ui.toast.success('Copied'); } catch { ui.toast.error('Copy failed'); } }));

    const cards = [];
    cards.push(ui.card({ title: 'Before you start', icon: 'shield', body: h('div.col.gap-3',
      h('p', ADMISSION_TEXT[join.admission] || ''),
      join.mlat ? consentBlock('C1') : null,
      h('p.muted', 'After the feeder connects, create an account and claim the station: you then see its statistics, timing and exact location, and you choose what the public map shows about it (nothing, by default).')) }));
    if (sn.avf) {
      cards.push(ui.card({ title: 'AirView feeder (recommended)', icon: 'antenna', sub: `Encrypted AVF/1 link on port ${join.avfPort}`, body: h('div.col.gap-3',
        h('p', 'Runs next to your decoder (readsb, dump1090, a Radarcape or a Beast) on Linux or Windows and sends its data with precise timestamps. Set it up, start the service, then claim it with the code it prints:'),
        ...sn.avf.map(line),
        h('p', 'On a Raspberry Pi (or another Debian-based Linux), the installer does all of this, with this network already filled in:'),
        line(`curl -fsSL ${location.origin}/feeder/install.sh | sudo bash`),
        h('p.muted', 'airview-feeder setup asks for the antenna position and what to show publicly; each choice can be changed later on your station page.')) }));
    }
    if (sn.readsb || sn.mlatclient) {
      cards.push(ui.card({ title: 'readsb / Ultrafeeder / mlat-client', icon: 'target', sub: 'For an existing feeder stack', body: h('div.col.gap-3',
        consentBlock('C5'),
        sn.readsb ? h('div.col.gap-2', h('b', `readsb: Beast data (port ${join.beastPort})`), line(sn.readsb)) : null,
        sn.mlatclient ? h('div.col.gap-2', h('b', `mlat-client: MLAT (port ${join.mlatPort})`), line(sn.mlatclient)) : null,
        sn.ultrafeeder ? h('div.col.gap-2', h('b', 'Ultrafeeder (ULTRAFEEDER_CONFIG entries, separated by ";")'), line(sn.ultrafeeder), sn.ultrafeederMlat ? line(sn.ultrafeederMlat) : null) : null,
        h('p.muted', 'Use the UUID your feeder already has (for example in /boot/adsb-config.txt); it is how the network tells your station apart, and how you claim it later from the same internet connection.')) }));
    }
    if (!cards.slice(1).length) {
      cards.push(ui.empty({ icon: 'antenna', title: 'No feeder listener is open', text: 'The operator has not opened a port for feeders yet.' }));
    }
    ui.mount(body, ...cards);
  },

  onNet() { /* nothing station-specific */ },
};
