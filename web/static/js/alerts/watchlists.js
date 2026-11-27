// alerts/watchlists.js - the Watch-lists card: lists with their sizes, the entries of the selected list
// (add / remove / filter), import (paste a hex list with notes or upload a CSV / text file, add or replace)
// and downloads (txt / csv). Admin calls go through the page's login-and-retry helper.

import { parseWatchText } from './model.js';

const NAME_RE = /^[A-Za-z0-9 _.-]{1,40}$/;
const MAX_ROWS = 400;

/**
 * @param ctx page context
 * @param {{ admin, onLists(names: string[]) }} o
 * @returns {{ el: HTMLElement, reload(): Promise<void>, names(): string[] }}
 */
export function createWatchlists(ctx, o) {
  const { ui, api, units } = ctx;
  const { h } = ui;
  const listEl = h('div.alw-lists');
  const detailEl = h('div.alw-detail');
  let lists = null;
  let selected = null;
  let entries = null;
  let filter = '';
  let seq = 0;

  const importBtn = ui.button({ label: 'Import', icon: 'upload', size: 'sm', onClick: () => openImport(selected) });
  const card = ui.card({ title: 'Watch-lists', icon: 'eye', sub: 'Aircraft you want to know about - use them in rules with "On a watch-list"', actions: importBtn,
    body: h('div.alw', listEl, detailEl) });
  card.classList.add('alw-card');

  const enc = (s) => encodeURIComponent(s);
  const dl = (href, label, tip) => h('a.btn.btn--sm.btn--ghost', { href, download: '', tip }, ui.icon('download'), h('span', label));

  async function reload(keep = true) {
    const my = ++seq;
    if (!lists) ui.mount(listEl, ui.skeleton({ lines: 3 }));
    try {
      const data = await api.get('/api/alerts/watchlists', { signal: ctx.signal });
      if (my !== seq) return;
      lists = (data || []).slice().sort((a, b) => a.name.localeCompare(b.name));
      o.onLists?.(lists.map((l) => l.name));
      if (!keep || !lists.some((l) => l.name === selected)) selected = lists[0]?.name || null;
      renderLists();
      await loadEntries();
    } catch (e) {
      if (e.aborted || my !== seq) return;
      ui.mount(listEl, ui.error(e, { compact: true, retry: () => reload() }));
      ui.clear(detailEl);
    }
  }

  function renderLists() {
    if (!lists.length) {
      ui.mount(listEl, ui.empty({ icon: 'eye', title: 'No watch-lists yet', compact: true,
        text: 'Paste ICAO addresses (one per line, a note after a comma) or upload a CSV to create one.',
        action: ui.button({ label: 'Create a list', icon: 'plus', size: 'sm', variant: 'primary', onClick: () => openImport(null) }) }));
      ui.clear(detailEl);
      return;
    }
    ui.mount(listEl, lists.map((l) => h('button.alw-list' + (l.name === selected ? '.active' : ''), { type: 'button', onclick: () => { if (selected !== l.name) { selected = l.name; filter = ''; renderLists(); loadEntries(); } } },
      ui.icon('list'), h('span.alw-list-name', l.name), h('span.alw-list-n.num', units.int(l.entries)))));
  }

  async function loadEntries() {
    if (!selected) return;
    const name = selected;
    entries = null;
    ui.mount(detailEl, ui.skeleton({ lines: 4 }));
    try {
      const rows = await api.get('/api/alerts/watchlists/' + enc(name), { params: { format: 'json' }, signal: ctx.signal });
      if (name !== selected) return;
      entries = rows || [];
      renderDetail();
    } catch (e) {
      if (e.aborted || name !== selected) return;
      ui.mount(detailEl, ui.error(e, { compact: true, retry: loadEntries }));
    }
  }

  function renderDetail() {
    const name = selected;
    const icaoIn = ui.input({ placeholder: 'ICAO hex', mono: true, size: 'sm', maxlength: 6 });
    const noteIn = ui.input({ placeholder: 'Note (optional)', size: 'sm', maxlength: 120 });
    const addBtn = ui.button({ label: 'Add', icon: 'plus', size: 'sm', variant: 'primary', onClick: add });
    const addF = ui.field({ control: h('div.alw-add', icaoIn, noteIn, addBtn) });
    icaoIn.input.addEventListener('keydown', (e) => { if (e.key === 'Enter') add(); });
    noteIn.input.addEventListener('keydown', (e) => { if (e.key === 'Enter') add(); });
    async function add() {
      const icao = String(icaoIn.value || '').trim().toUpperCase();
      if (!/^[0-9A-F]{6}$/.test(icao)) { addF.setError('Six hex digits, e.g. 3C6444.'); return; }
      addF.setError(null);
      addBtn.setLoading(true);
      try {
        await o.admin.run(() => api.put(`/api/alerts/watchlists/${enc(name)}/${icao}`, { note: String(noteIn.value || '').trim() }), 'Changing watch-lists needs the admin password.');
        icaoIn.value = ''; noteIn.value = '';
        await reload();
      } catch (e) { if (!e.cancelled) addF.setError(e.message); } finally { addBtn.setLoading(false); }
    }
    const filterIn = ui.input({ placeholder: 'Filter', icon: 'search', size: 'sm', clearable: true, value: filter, onInput: (v) => { filter = v; paintTable(); } });
    const tableHost = h('div.alw-table');
    const countEl = h('span.alx-small.muted');
    const tbl = ui.table({
      columns: [
        { key: 'icao', label: 'ICAO', sort: true, render: (r) => h('a.num.alw-icao', { href: '#/map/' + r.icao, tip: 'Show on the Live Map' }, r.icao) },
        { key: 'note', label: 'Note', sort: true, render: (r) => r.note || '' },
        { key: 'added', label: 'Added', sort: true, num: true, sortDir: 'desc', render: (r) => units.date(r.added) },
        { key: 'x', label: '', align: 'right', render: (r) => ui.iconButton('trash', 'Remove', () => remove(r.icao), { variant: 'ghost', size: 'sm' }) },
      ],
      rows: [], sort: { key: 'added', dir: 'desc' }, empty: 'This list is empty',
    });
    tableHost.append(tbl);
    function paintTable() {
      const f = filter.trim().toLowerCase();
      const rows = f ? entries.filter((e) => e.icao.toLowerCase().includes(f) || (e.note || '').toLowerCase().includes(f)) : entries;
      tbl.setRows(rows.slice(0, MAX_ROWS));
      ui.setText(countEl, rows.length > MAX_ROWS ? `${units.int(MAX_ROWS)} of ${units.int(rows.length)} shown - filter to narrow down` : `${units.int(rows.length)} ${rows.length === 1 ? 'entry' : 'entries'}${f ? ' match' : ''}`);
    }
    async function remove(icao) {
      try {
        await o.admin.run(() => api.del(`/api/alerts/watchlists/${enc(name)}/${icao}`), 'Changing watch-lists needs the admin password.');
        entries = entries.filter((e) => e.icao !== icao);
        const l = lists.find((x) => x.name === name);
        if (l) l.entries = entries.length;
        renderLists();
        paintTable();
      } catch (e) { if (!e.cancelled) ui.toast.error(e.message, { title: 'Not removed' }); }
    }
    async function dropList() {
      const ok = await ui.confirm({ title: 'Delete watch-list?', message: `"${name}" and its ${units.int(entries.length)} entries are removed. Rules that name this list stop matching it.`, confirmLabel: 'Delete list', danger: true });
      if (!ok) return;
      try {
        await o.admin.run(() => api.del('/api/alerts/watchlists/' + enc(name)), 'Changing watch-lists needs the admin password.');
        ui.toast.success(`Watch-list "${name}" deleted.`);
        await reload(false);
      } catch (e) { if (!e.cancelled) ui.toast.error(e.message, { title: 'Not deleted' }); }
    }
    const base = '/api/alerts/watchlists/' + enc(name);
    ui.mount(detailEl,
      h('div.alw-detail-head', h('h4', name), h('span.spacer'),
        dl(base + '?format=txt', 'TXT', 'Download as text (one address per line)'), dl(base + '?format=csv', 'CSV', 'Download as CSV'),
        ui.button({ label: 'Import', icon: 'upload', size: 'sm', variant: 'ghost', onClick: () => openImport(name) }),
        ui.iconButton('trash', 'Delete this list', dropList, { variant: 'ghost', size: 'sm' })),
      addF, h('div.alw-tools', filterIn, countEl), tableHost);
    paintTable();
  }

  function openImport(name) {
    const nameIn = ui.input({ value: name || '', placeholder: 'e.g. vips', maxlength: 40 });
    const nameF = ui.field({ label: 'List', control: nameIn, hint: lists?.length ? 'An existing list name adds to it; a new name creates a list.' : 'Letters, digits, space, _ . - (up to 40).' });
    const mode = ui.segmented({ options: [{ value: 'add', label: 'Add to the list' }, { value: 'replace', label: 'Replace the list' }], value: 'add' });
    const ta = h('textarea.textarea.alw-paste', { rows: 9, spellcheck: 'false', placeholder: '# one address per line, note after a comma\n3C6444, Lufthansa A319\n4CA2D1 Ryanair\n400A0B;British Airways' });
    const preview = h('div.alx-small.muted');
    const fileIn = h('input', { type: 'file', accept: '.csv,.txt,text/csv,text/plain', class: 'hidden' });
    const fileBtn = ui.button({ label: 'Choose a file…', icon: 'upload', size: 'sm', variant: 'ghost', onClick: () => fileIn.click() });
    const paint = () => {
      const { ok, bad } = parseWatchText(ta.value);
      ui.setText(preview, ta.value.trim() ? `${units.int(ok.length)} ${ok.length === 1 ? 'address' : 'addresses'} recognised${bad.length ? `, ${units.int(bad.length)} ${bad.length === 1 ? 'line' : 'lines'} will be skipped (e.g. "${bad[0].slice(0, 40)}")` : ''}.` : 'Paste a list or choose a CSV / text file (up to 16 MB).');
    };
    ta.addEventListener('input', paint);
    fileIn.addEventListener('change', async () => {
      const f = fileIn.files?.[0];
      if (!f) return;
      if (f.size > 16 * 1024 * 1024) { ui.toast.error('The file is larger than 16 MB.'); return; }
      ta.value = await f.text();
      if (!nameIn.value) nameIn.value = f.name.replace(/\.[^.]+$/, '').replace(/[^A-Za-z0-9 _.-]/g, '_').slice(0, 40);
      paint();
    });
    paint();
    const m = ui.modal({
      title: 'Import watch-list', icon: 'upload', width: 560,
      body: h('div.col.gap-3', nameF, ui.field({ label: 'Mode', control: mode }), ui.field({ label: 'Addresses', control: h('div.col.gap-1', ta, h('div.row.gap-2', fileBtn, fileIn, preview)) })),
      actions: [
        { label: 'Cancel', variant: 'ghost', value: false },
        { label: 'Import', variant: 'primary', icon: 'upload', value: true, onClick: async () => {
          const n = String(nameIn.value || '').trim();
          if (!NAME_RE.test(n)) { nameF.setError('Letters, digits, space, _ . - (1 to 40 characters).'); return false; }
          nameF.setError(null);
          if (!parseWatchText(ta.value).ok.length) { ui.setText(preview, 'No ICAO address recognised - nothing to import.'); return false; }
          try {
            const res = await o.admin.run(() => api.post(`/api/alerts/watchlists/${enc(n)}/import`, ta.value, { params: { replace: mode.value === 'replace' ? 1 : undefined }, headers: { 'Content-Type': 'text/plain' }, timeout: 60000 }),
              'Importing watch-lists needs the admin password.');
            ui.toast.success(`${units.int(res?.imported ?? 0)} imported${res?.rejected ? `, ${units.int(res.rejected)} rejected` : ''}.`, { title: `Watch-list "${n}"` });
            selected = n;
            await reload();
            return true;
          } catch (e) {
            if (!e.cancelled) nameF.setError(e.message);
            return false;
          }
        } },
      ],
    });
    return m;
  }

  reload();
  return { el: card, reload, names: () => (lists || []).map((l) => l.name) };
}
