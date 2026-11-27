// settings/form.js - form-field builders bound to the Settings store (S): every control reads the draft config
// by dotted path, writes back on input and registers itself so validation problems and server corrections can
// be painted next to it.

import { secretState } from './model.js';

export function makeForm(S) {
  const { ui, units } = S.ctx;
  const { h } = ui;

  /** Register a field under a config path (adds the "server note" line and paints the current problem). */
  function reg(path, field) {
    const note = h('div.set-note.hidden');
    field.appendChild(note);
    field.__note = note;
    field.dataset.path = path;
    S.fields.set(path, field);
    S.paint(path);
    markLocked(path, field);
    return field;
  }

  /**
   * Settings fixed by the operator file, an AIRVIEW_* variable or a -set flag (GET /api/config `locked`:
   * [{path, origin}], values never sent) cannot be changed here: the control is disabled and the field says
   * where the value comes from. A locked parent path ("aggregator") locks every field below it.
   */
  function markLocked(path, field) {
    const list = S.ctx.config.current?.locked;
    if (!Array.isArray(list) || !list.length) return;
    const hit = list.find((l) => l && (l.path === path || path.startsWith(l.path + '.') || path.startsWith(l.path + '[')));
    if (!hit) return;
    field.classList.add('set-locked');
    for (const el of field.querySelectorAll('input, select, textarea, button')) el.disabled = true;
    field.appendChild(h('div.field-hint.set-locked-hint', ui.icon('lock'), ` Set by ${hit.origin || 'the operator configuration'} - change it there.`));
  }

  /** Round for display without inventing digits (base value -> shown number). */
  const shown = (v, digits) => (typeof v === 'number' && Number.isFinite(v) ? Number(v.toFixed(digits)) : '');

  const F = {
    reg,

    /** Free text. o: { label, hint, placeholder, mono, maxLength, onChange(value), icon, row } */
    text(path, o = {}) {
      const inp = ui.input({
        value: S.get(path) ?? '', placeholder: o.placeholder, mono: o.mono, icon: o.icon, maxlength: o.maxLength,
        autocomplete: 'off', spellcheck: 'false',
        onInput: (v) => { S.set(path, v); if (o.onChange) o.onChange(v); },
      });
      const f = reg(path, ui.field({ label: o.label, hint: o.hint, control: inp, tip: o.tip }));
      f.control = inp;
      return f;
    },

    /**
     * Number. o: { label, hint, suffix, min, max, step, kind (units kind: stored in base units, shown in the
     * user's unit), digits, onChange(baseValue) }. An empty box stores null, which validation reports.
     */
    number(path, o = {}) {
      const kind = o.kind;
      const digits = o.digits ?? (kind ? 1 : 3);
      const cur = S.get(path);
      const inp = ui.input({
        type: 'number', value: shown(kind ? units.value(kind, cur) : cur, digits), suffix: o.suffix ?? (kind ? units.label(kind) : undefined),
        min: o.min, max: o.max, step: o.step ?? 'any', inputmode: 'decimal', placeholder: o.placeholder,
        onInput: (v) => {
          const base = v === null ? null : kind ? Number(units.toBase(kind, v).toFixed(4)) : v;
          S.set(path, base);
          if (o.onChange) o.onChange(base);
        },
      });
      const f = reg(path, ui.field({ label: o.label, hint: o.hint, control: inp, tip: o.tip }));
      f.control = inp;
      /** Programmatic update (does not fire onChange). */
      f.show = (base) => { inp.value = shown(kind ? units.value(kind, base) : base, digits); };
      return f;
    },

    /** Switch in a label-left / control-right row. o: { label, hint, onChange } */
    toggle(path, o = {}) {
      const t = ui.toggle({ checked: !!S.get(path), onChange: (v) => { S.set(path, v); if (o.onChange) o.onChange(v); } });
      const f = reg(path, ui.field({ label: o.label, hint: o.hint, control: t, row: true }));
      f.control = t;
      return f;
    },

    /** Drop-down. o: { label, hint, options: [{value,label}], numeric, onChange } */
    select(path, o = {}) {
      const cur = S.get(path);
      const opts = o.options.slice();
      if (cur !== undefined && cur !== null && !opts.some((x) => String(x.value) === String(cur))) opts.push({ value: cur, label: `${cur} (unknown)` });
      const sel = ui.select({ options: opts, value: String(cur ?? ''), onChange: (v) => { const val = o.numeric ? Number(v) : v; S.set(path, val); if (o.onChange) o.onChange(val); } });
      const f = reg(path, ui.field({ label: o.label, hint: o.hint, control: sel }));
      f.control = sel;
      return f;
    },

    /** Segmented choice. o: { label, hint, options, onChange, block } */
    seg(path, o = {}) {
      const seg = ui.segmented({ options: o.options, value: S.get(path), block: o.block, onChange: (v) => { S.set(path, v); if (o.onChange) o.onChange(v); } });
      const f = reg(path, ui.field({ label: o.label, hint: o.hint, control: seg }));
      f.control = seg;
      return f;
    },

    /** Multi-line notes. */
    area(path, o = {}) {
      const ta = h('textarea.textarea', { rows: o.rows || 3, placeholder: o.placeholder, maxlength: o.maxLength || 2000, spellcheck: 'false' });
      ta.value = S.get(path) ?? '';
      ta.addEventListener('input', () => S.set(path, ta.value));
      const f = reg(path, ui.field({ label: o.label, hint: o.hint, control: ta }));
      f.control = ta;
      return f;
    },

    /**
     * Secret (password) editor following the wire conventions: the draft keeps "" / "set" while untouched,
     * "plain:<new>" for a new value and "clear" to remove the stored one. The stored secret is never shown.
     * o: { label, hint, confirm (ask twice), what ('password'), minLength, autocomplete }
     */
    secret(path, o = {}) {
      const what = o.what || 'password';
      const saved = S.savedValue(path) ?? '';
      const stateEl = h('span');
      const inp = ui.input({ type: 'password', placeholder: saved === 'set' ? `New ${what} (leave empty to keep)` : `New ${what}`, autocomplete: 'new-password', onInput: sync });
      const inp2 = o.confirm ? ui.input({ type: 'password', placeholder: `Repeat the new ${what}`, autocomplete: 'new-password', onInput: sync }) : null;
      const show = ui.button({ icon: 'eye', variant: 'ghost', size: 'sm', tip: 'Show / hide', onClick: () => {
        const t = inp.input.type === 'password' ? 'text' : 'password';
        inp.input.type = t;
        if (inp2) inp2.input.type = t;
      } });
      const removeBtn = ui.button({ label: `Remove ${what}`, icon: 'trash', size: 'sm', variant: 'ghost', onClick: () => { inp.value = ''; if (inp2) inp2.value = ''; S.local.delete(path); S.set(path, 'clear'); paint(); } });
      const undoBtn = ui.button({ label: 'Undo', icon: 'history', size: 'sm', variant: 'ghost', onClick: () => { inp.value = ''; if (inp2) inp2.value = ''; S.local.delete(path); S.set(path, saved); paint(); } });
      const control = h('div.col.gap-2', h('div.row', h('div.grow', inp), show), inp2, h('div.row-wrap', stateEl, h('span.spacer'), removeBtn, undoBtn));
      const f = reg(path, ui.field({ label: o.label, hint: o.hint, control }));

      function sync() {
        const v = inp.value;
        if (v === '') { S.local.delete(path); S.set(path, saved); paint(); return; }
        if (o.minLength && v.length < o.minLength) S.local.set(path, `Use at least ${o.minLength} characters.`);
        else if (inp2 && inp2.value !== v) S.local.set(path, 'The two entries do not match yet.');
        else S.local.delete(path);
        S.set(path, 'plain:' + v);
        paint();
      }
      function paint() {
        const st = secretState(saved, S.get(path));
        const badge = st.pending === 'clear' ? ui.badge(`will be removed when you save`, 'amber')
          : st.pending === 'set' ? ui.badge(st.stored ? 'will be replaced when you save' : 'will be set when you save', 'amber')
            : st.stored ? ui.badge(`${what} is set`, 'green') : ui.badge(`no ${what}`, 'other');
        ui.mount(stateEl, badge);
        removeBtn.classList.toggle('hidden', !(st.stored && st.pending !== 'clear'));
        undoBtn.classList.toggle('hidden', !st.pending);
      }
      paint();
      return f;
    },

    /** Titled group of fields laid out on a responsive grid. */
    group(title, icon, ...children) {
      return h('div.set-group', title && ui.section(title, icon), h('div.set-fields', children));
    },

    /** Paragraph of explanatory text. */
    help(...children) { return h('p.set-help', children); },

    /** Callout box. kind: info | warn | danger */
    callout(kind, iconName, ...children) {
      return h('div.set-callout.set-callout--' + kind, ui.icon(iconName || (kind === 'info' ? 'info' : 'alert')), h('div', children));
    },
  };
  return F;
}
