#!/usr/bin/env node
// AirView manual builder.
//
// Converts docs/manual/*.md into one print-ready HTML file and prints it to PDF with
// headless Microsoft Edge. Uses Node built-ins only (no npm packages).
//
//   node docs/manual/build/build-manual.mjs [--html-only] [--no-copy] [--edge <path>]
//
// Output: docs/manual/AirView-Manual-1.0.pdf, copied to dist/AirView-Manual-1.0.pdf
// (unless --no-copy). Intermediate files go to docs/manual/build/out/.
//
// Page numbers in the table of contents come from a two-pass build: the first PDF is
// parsed for the named destinations Chromium writes for every element id, the page
// numbers are put into the contents, and the PDF is printed again until stable.

import fs from 'node:fs';
import path from 'node:path';
import os from 'node:os';
import zlib from 'node:zlib';
import { spawnSync } from 'node:child_process';
import { fileURLToPath, pathToFileURL } from 'node:url';

const HERE = path.dirname(fileURLToPath(import.meta.url));
const MANUAL = path.resolve(HERE, '..');
const ROOT = path.resolve(MANUAL, '..', '..');
const OUT = path.join(HERE, 'out');
const PDF_NAME = 'AirView-Manual-1.0.pdf';
const PDF = path.join(MANUAL, PDF_NAME);
const DIST_PDF = path.join(ROOT, 'dist', PDF_NAME);
const LOGO = path.join(ROOT, 'web', 'static', 'img', 'logo.svg');

const args = process.argv.slice(2);
const opt = {
  htmlOnly: args.includes('--html-only'),
  noCopy: args.includes('--no-copy'),
  edge: args.includes('--edge') ? args[args.indexOf('--edge') + 1] : null,
};

const warnings = [];
const warn = (m) => warnings.push(m);

// ---------------------------------------------------------------- helpers

const esc = (s) => s.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;').replace(/"/g, '&quot;');

// GitHub-style heading slug.
function slugify(text) {
  return text
    .trim()
    .toLowerCase()
    .replace(/<[^>]*>/g, '')
    .replace(/[^\p{L}\p{N}\s_-]/gu, '')
    .replace(/\s/g, '-');
}

// Plain text of an inline Markdown string (for slugs, TOC and headers).
function plain(md) {
  return md
    .replace(/`([^`]*)`/g, '$1')
    .replace(/!\[([^\]]*)\]\([^)]*\)/g, '$1')
    .replace(/\[([^\]]*)\]\([^)]*\)/g, '$1')
    .replace(/\*\*([^*]+)\*\*/g, '$1')
    .replace(/(^|[^\w*])\*([^*]+)\*/g, '$1$2')
    .replace(/(^|\W)_([^_]+)_(?!\w)/g, '$1$2')
    .replace(/\\([\\`*_{}[\]()#+\-.!|<>~])/g, '$1')
    .trim();
}

// ---------------------------------------------------------------- documents

function loadDocs() {
  const files = fs.readdirSync(MANUAL).filter((f) => f.endsWith('.md'));
  const docs = [];
  for (const f of files) {
    let src = fs.readFileSync(path.join(MANUAL, f), 'utf8').replace(/\r\n?/g, '\n');
    let meta = {};
    const fm = /^---\n([\s\S]*?)\n---\n/.exec(src);
    if (fm) {
      for (const line of fm[1].split('\n')) {
        const m = /^(\w+):\s*(.*)$/.exec(line);
        if (m) meta[m[1]] = m[2].trim();
      }
      src = src.slice(fm[0].length);
    }
    const h1 = /^# (.+)$/m.exec(src);
    const title = h1 ? plain(h1[1]) : f;
    const num = /^(\d+(?:\.\d+)*)\.?\s/.exec(title);
    const key = f.startsWith('00-') ? [0] : num ? num[1].split('.').map(Number) : [999];
    docs.push({ file: f, stem: f.replace(/\.md$/, ''), src, meta, title, key });
  }
  docs.sort((a, b) => {
    for (let i = 0; i < Math.max(a.key.length, b.key.length); i++) {
      const x = a.key[i] ?? -1, y = b.key[i] ?? -1;
      if (x !== y) return x - y;
    }
    return a.file.localeCompare(b.file);
  });
  return docs;
}

// ---------------------------------------------------------------- inline Markdown

function makeInline(ctx) {
  function resolveHref(url) {
    if (/^(https?:|mailto:)/i.test(url)) return { href: url, external: true };
    const [file, frag] = url.split('#');
    if (file === '') return { href: '#' + ctx.stem + '--' + frag };
    const base = path.basename(file);
    if (file.endsWith('.md') && ctx.stems.has(base.replace(/\.md$/, '')) && !file.includes('/')) {
      const stem = base.replace(/\.md$/, '');
      return { href: '#' + (frag ? stem + '--' + frag : stem) };
    }
    warn(`${ctx.file}: link to outside the manual left as text: ${url}`);
    return null;
  }

  function inline(s) {
    const tokens = [];
    const hold = (html) => `\u0000${tokens.push(html) - 1}\u0001`;
    let out = '';
    let i = 0;
    while (i < s.length) {
      const c = s[i];
      if (c === '\\' && i + 1 < s.length && /[\\`*_{}[\]()#+\-.!|<>~]/.test(s[i + 1])) {
        out += hold(esc(s[i + 1]));
        i += 2;
        continue;
      }
      if (c === '`') {
        let n = 0;
        while (s[i + n] === '`') n++;
        const fence = '`'.repeat(n);
        let j = i + n;
        let end = -1;
        while ((j = s.indexOf(fence, j)) !== -1) {
          if (s[j + n] !== '`' && s[j - 1] !== '`') { end = j; break; }
          j += n;
        }
        if (end !== -1) {
          let code = s.slice(i + n, end);
          if (/^ .* $/.test(code) && code.trim()) code = code.slice(1, -1);
          out += hold(`<code>${esc(code).replace(/([._/=,:|\]-])(?=\S)/g, '$1<wbr>')}</code>`);
          i = end + n;
          continue;
        }
        out += hold(esc(fence));
        i += n;
        continue;
      }
      if (c === '[' || (c === '!' && s[i + 1] === '[')) {
        const img = c === '!';
        const start = img ? i + 1 : i;
        let depth = 0, j = start, close = -1;
        for (; j < s.length; j++) {
          if (s[j] === '\\') { j++; continue; }
          if (s[j] === '`') { const k = s.indexOf('`', j + 1); if (k > 0) { j = k; continue; } }
          if (s[j] === '[') depth++;
          else if (s[j] === ']') { depth--; if (depth === 0) { close = j; break; } }
        }
        if (close !== -1 && s[close + 1] === '(') {
          const pend = s.indexOf(')', close + 2);
          if (pend !== -1) {
            const text = s.slice(start + 1, close);
            const url = s.slice(close + 2, pend).trim().replace(/\s+"[^"]*"$/, '');
            if (img) {
              out += hold(ctx.image(url, text));
            } else {
              const r = resolveHref(url);
              const inner = inline(text);
              out += hold(r ? `<a href="${esc(r.href)}"${r.external ? ' class="ext"' : ''}>${inner}</a>` : inner);
            }
            i = pend + 1;
            continue;
          }
        }
      }
      if (c === '<') {
        const m = /^<(https?:\/\/[^>\s]+)>/.exec(s.slice(i));
        if (m) {
          out += hold(`<a class="ext" href="${esc(m[1])}">${esc(m[1])}</a>`);
          i += m[0].length;
          continue;
        }
      }
      out += c;
      i++;
    }
    // Hard line breaks, escaping, emphasis.
    let h = esc(out)
      .replace(/ {2,}\n/g, '<br>')
      .replace(/\\\n/g, '<br>')
      .replace(/\n/g, ' ');
    h = h.replace(/\*\*(?=\S)([\s\S]*?\S)\*\*/g, '<strong>$1</strong>');
    h = h.replace(/__(?=\S)([\s\S]*?\S)__(?!\w)/g, '<strong>$1</strong>');
    h = h.replace(/(^|[^\w*])\*(?=[^\s*])([\s\S]*?[^\s*])\*(?![\w*])/g, '$1<em>$2</em>');
    h = h.replace(/(^|[^\w*])\*([^\s*])\*(?![\w*])/g, '$1<em>$2</em>');
    h = h.replace(/(^|[^\w])_(?=[^\s_])([\s\S]*?[^\s_])_(?!\w)/g, '$1<em>$2</em>');
    // Restore held tokens (they may nest).
    for (let k = 0; k < 4 && h.includes('\u0000'); k++) {
      h = h.replace(/\u0000(\d+)\u0001/g, (_, n) => tokens[+n]);
    }
    return h;
  }
  return inline;
}

// ---------------------------------------------------------------- block Markdown

const RE_FENCE = /^(\s*)(`{3,}|~{3,})\s*([\w+-]*)\s*$/;
const RE_HEADING = /^(#{1,6})\s+(.*?)\s*#*\s*$/;
const RE_LIST = /^(\s*)([-*+]|\d{1,9}[.)])(\s+|$)/;
const RE_HR = /^\s{0,3}([-*_])(\s*\1){2,}\s*$/;
const isBlank = (l) => /^\s*$/.test(l);
const isTableDelim = (l) => /^\s*\|?\s*:?-{2,}:?\s*(\|\s*:?-{2,}:?\s*)*\|?\s*$/.test(l) && l.includes('-');

function splitRow(line) {
  let s = line.trim();
  if (s.startsWith('|')) s = s.slice(1);
  if (s.endsWith('|') && !s.endsWith('\\|')) s = s.slice(0, -1);
  const cells = [];
  let cur = '', inCode = 0;
  for (let i = 0; i < s.length; i++) {
    const c = s[i];
    if (c === '\\' && s[i + 1] === '|') { cur += inCode ? '|' : '\\|'; i++; continue; }
    if (c === '`') {
      let n = 0;
      while (s[i + n] === '`') n++;
      if (!inCode) { if (s.indexOf('`'.repeat(n), i + n) !== -1) inCode = n; }
      else if (n === inCode) inCode = 0;
      cur += '`'.repeat(n);
      i += n - 1;
      continue;
    }
    if (c === '|' && !inCode) { cells.push(cur.trim()); cur = ''; continue; }
    cur += c;
  }
  cells.push(cur.trim());
  return cells;
}

function makeBlocks(ctx) {
  const inline = ctx.inline;

  function render(lines) {
    const out = [];
    let i = 0;
    let para = [];
    const flush = () => {
      if (!para.length) return;
      const text = para.join('\n');
      para = [];
      out.push({ type: 'p', text });
    };

    while (i < lines.length) {
      const line = lines[i];
      if (isBlank(line)) { flush(); i++; continue; }
      if (/^\s*<!--[\s\S]*-->\s*$/.test(line)) { flush(); i++; continue; }

      const fence = RE_FENCE.exec(line);
      if (fence) {
        flush();
        const ind = fence[1].length;
        const marker = fence[2];
        const lang = fence[3];
        const body = [];
        i++;
        while (i < lines.length && !new RegExp('^\\s*' + marker[0] + '{' + marker.length + ',}\\s*$').test(lines[i])) {
          body.push(lines[i].replace(new RegExp('^ {0,' + ind + '}'), ''));
          i++;
        }
        i++;
        out.push({ type: 'html', html: codeBlock(body, lang) });
        continue;
      }

      const h = RE_HEADING.exec(line);
      if (h && /^#/.test(line)) {
        flush();
        out.push({ type: 'html', html: ctx.heading(h[1].length, h[2]) });
        i++;
        continue;
      }

      if (RE_HR.test(line) && !para.length) {
        flush();
        out.push({ type: 'html', html: '<hr>' });
        i++;
        continue;
      }

      if (line.includes('|') && i + 1 < lines.length && isTableDelim(lines[i + 1])) {
        flush();
        const head = splitRow(line);
        const aligns = splitRow(lines[i + 1]).map((c) =>
          c.startsWith(':') && c.endsWith(':') ? 'center' : c.endsWith(':') ? 'right' : '');
        i += 2;
        const rows = [];
        while (i < lines.length && !isBlank(lines[i]) && lines[i].includes('|')) {
          rows.push(splitRow(lines[i]));
          i++;
        }
        out.push({ type: 'html', html: table(head, aligns, rows) });
        continue;
      }

      if (/^\s{0,3}>/.test(line)) {
        flush();
        const body = [];
        while (i < lines.length && !isBlank(lines[i]) && (/^\s{0,3}>/.test(lines[i]) || body.length)) {
          if (!/^\s{0,3}>/.test(lines[i]) && (RE_LIST.test(lines[i]) || RE_FENCE.test(lines[i]))) break;
          body.push(lines[i].replace(/^\s{0,3}> ?/, ''));
          i++;
        }
        out.push({ type: 'html', html: `<blockquote>${render(body)}</blockquote>` });
        continue;
      }

      const li = RE_LIST.exec(line);
      if (li && !(para.length && /^\d/.test(li[2]) && li[2] !== '1.' && li[2] !== '1)')) {
        flush();
        const r = list(lines, i);
        out.push({ type: 'html', html: r.html });
        i = r.next;
        continue;
      }

      if (!para.length && /^ {4,}\S/.test(line)) {
        const body = [];
        while (i < lines.length && (/^ {4}/.test(lines[i]) || isBlank(lines[i]))) {
          body.push(lines[i].slice(4));
          i++;
        }
        while (body.length && isBlank(body[body.length - 1])) body.pop();
        out.push({ type: 'html', html: codeBlock(body, '') });
        continue;
      }

      para.push(line.replace(/^\s+/, ''));
      i++;
    }
    flush();

    // Paragraphs: figures and their captions.
    const html = [];
    for (let k = 0; k < out.length; k++) {
      const b = out[k];
      if (b.type === 'html') { html.push(b.html); continue; }
      const img = /^!\[([^\]]*)\]\(([^)]+)\)$/.exec(b.text.trim());
      if (img) {
        let caption = '';
        const nx = out[k + 1];
        if (nx && nx.type === 'p' && /^\*[^*][\s\S]*\*$/.test(nx.text.trim()) && !/^\*\*/.test(nx.text.trim())) {
          caption = inline(nx.text.trim().slice(1, -1));
          k++;
        } else {
          caption = inline(img[1]);
        }
        html.push(`<figure>${ctx.image(img[2], img[1])}<figcaption>${caption}</figcaption></figure>`);
        continue;
      }
      html.push(`<p>${inline(b.text)}</p>`);
    }
    return html.join('\n');
  }

  function list(lines, start) {
    const first = RE_LIST.exec(lines[start]);
    const baseIndent = first[1].length;
    const ordered = /\d/.test(first[2]);
    const startNum = ordered ? parseInt(first[2], 10) : 1;
    const items = [];
    let i = start;
    let loose = false;
    while (i < lines.length) {
      const m = RE_LIST.exec(lines[i]);
      if (!m || m[1].length !== baseIndent || /\d/.test(m[2]) !== ordered) break;
      const contentIndent = m[1].length + m[2].length + Math.min(Math.max(m[3].length, 1), 4);
      const body = [lines[i].slice(Math.min(lines[i].length, contentIndent)).replace(/^\s+/, '')];
      i++;
      let sawBlank = false;
      while (i < lines.length) {
        const l = lines[i];
        if (isBlank(l)) { body.push(''); sawBlank = true; i++; continue; }
        const ind = l.match(/^\s*/)[0].length;
        if (ind >= contentIndent) { body.push(l.slice(contentIndent)); sawBlank = false; i++; continue; }
        const lm = RE_LIST.exec(l);
        if (lm && lm[1].length <= baseIndent) break;
        if (lm && lm[1].length > baseIndent) {
          // A nested list indented less than the content indent.
          body.push(l.slice(Math.min(ind, baseIndent + 2)));
          sawBlank = false;
          i++;
          continue;
        }
        if (sawBlank) break;
        if (RE_HEADING.test(l) || RE_FENCE.test(l) || /^\s*>/.test(l)) break;
        body.push(l.trim()); // lazy continuation
        i++;
      }
      // trailing blanks
      let trail = 0;
      while (body.length && isBlank(body[body.length - 1])) { body.pop(); trail++; }
      if (trail && i < lines.length) {
        const nm = RE_LIST.exec(lines[i]);
        if (nm && nm[1].length === baseIndent) loose = true;
      }
      if (body.some((l, k) => isBlank(l) && k > 0 && k < body.length - 1 && !RE_LIST.test(body[k + 1] || ''))) {
        // blank lines inside an item (other than before a nested list) make it loose
        loose = loose || body.slice(1).some((l) => isBlank(l));
      }
      items.push(body);
    }
    const tag = ordered ? 'ol' : 'ul';
    const startAttr = ordered && startNum !== 1 ? ` start="${startNum}"` : '';
    const lis = items.map((body) => {
      let inner = render(body);
      if (!loose) inner = inner.replace(/^<p>([\s\S]*?)<\/p>/, '$1');
      return `<li>${inner}</li>`;
    });
    return { html: `<${tag}${startAttr}${loose ? ' class="loose"' : ''}>${lis.join('\n')}</${tag}>`, next: i };
  }

  function codeBlock(body, lang) {
    while (body.length && isBlank(body[0])) body.shift();
    while (body.length && isBlank(body[body.length - 1])) body.pop();
    const cls = body.length <= 30 ? ' class="keep"' : '';
    const wide = Math.max(0, ...body.map((l) => [...l].length));
    const size = wide > 100 ? ' style="font-size:7.2pt"' : '';
    return `<pre${cls}${size} data-lang="${esc(lang)}"><code>${esc(body.join('\n'))}</code></pre>`;
  }

  function table(head, aligns, rows) {
    const n = head.length;
    const cell = (tag, c, k) => `<${tag}${aligns[k] ? ` style="text-align:${aligns[k]}"` : ''}>${inline(c ?? '')}</${tag}>`;
    let cls = n >= 5 ? 'wide' : n >= 4 ? 'mid' : '';
    const hp = head.map((c) => plain(c).toLowerCase()).join('|');
    if (hp === 'setting|type|default|environment variable|description') cls += ' settings';
    let h = `<table${cls ? ` class="${cls}"` : ''}><thead><tr>${head.map((c, k) => cell('th', c, k)).join('')}</tr></thead><tbody>`;
    for (const r of rows) {
      if (r.length > n) warn(`${ctx.file}: table row has ${r.length} cells, header has ${n}: ${r.join(' | ').slice(0, 80)}`);
      const cells = [];
      for (let k = 0; k < n; k++) cells.push(cell('td', r[k], k));
      h += `<tr>${cells.join('')}</tr>`;
    }
    return h + '</tbody></table>';
  }

  return render;
}

// ---------------------------------------------------------------- assemble

function build(pageMap) {
  const docs = loadDocs();
  const stems = new Set(docs.map((d) => d.stem));
  const front = docs.find((d) => d.file.startsWith('00-'));
  if (!front) throw new Error('00-cover-and-contents.md is missing');
  const meta = front.meta;
  const ids = new Set();
  const toc = [];
  const sections = [];
  const pageRules = [];

  docs.forEach((doc, idx) => {
    const slugCount = new Map();
    const ctx = {
      file: doc.file,
      stem: doc.stem,
      stems,
      image(url, alt) {
        const abs = path.resolve(MANUAL, url);
        if (!fs.existsSync(abs)) warn(`${doc.file}: missing image ${url}`);
        const rel = path.relative(OUT, abs).split(path.sep).join('/');
        return `<img src="${esc(rel)}" alt="${esc(alt)}">`;
      },
      heading(level, md) {
        const text = plain(md);
        let slug = slugify(text);
        const n = slugCount.get(slug) || 0;
        slugCount.set(slug, n + 1);
        if (n) slug += '-' + n;
        const id = level === 1 ? doc.stem : `${doc.stem}--${slug}`;
        ids.add(id);
        if (level === 1) ids.add(`${doc.stem}--${slug}`);
        if (level === 1 || (level === 2 && !doc.file.startsWith('00-'))) toc.push({ level, id, text, chapter: level === 1 && /^\d+\.?\s/.test(text) && !/^\d+\.\d/.test(text) });
        const anchor = level === 1 ? `<a id="${esc(doc.stem + '--' + slug)}"></a>` : '';
        return `<h${level} id="${esc(id)}">${anchor}${ctx.inline(md)}</h${level}>`;
      },
    };
    ctx.inline = makeInline(ctx);
    const render = makeBlocks(ctx);
    const body = render(doc.src.split('\n'));
    const isChapter = /^\d+\.?\s/.test(doc.title) && !/^\d+\.\d/.test(doc.title);
    const isFront = doc === front;
    const pageName = `p${idx}`;
    const headerText = isFront ? 'About this manual' : doc.title;
    pageRules.push(`@page ${pageName} { @top-right { content: "${headerText.replace(/"/g, '\\"')}"; } }`);
    sections.push(`<section class="doc${isChapter ? ' chapter' : ''}${isFront ? ' front' : ''}" style="page:${pageName}" data-file="${esc(doc.file)}">\n${body}\n</section>`);
  });

  // Check internal links.
  const html0 = sections.join('\n');
  for (const m of html0.matchAll(/href="#([^"]+)"/g)) {
    if (!ids.has(m[1])) warn(`broken internal link: #${m[1]}`);
  }

  const tocHtml = toc
    .filter((e) => !e.id.startsWith(front.stem) || e.level === 1)
    .map((e) => {
      const pg = pageMap && pageMap.get(e.id) ? String(pageMap.get(e.id)) : '000';
      const cls = e.chapter ? 'toc-ch' : e.level === 1 ? 'toc-1' : 'toc-2';
      return `<li class="${cls}"><a href="#${esc(e.id)}"><span class="t">${esc(e.text)}</span><span class="dots"></span><span class="pg">${pg}</span></a></li>`;
    })
    .join('\n');

  const logo = fs.existsSync(LOGO) ? fs.readFileSync(LOGO, 'utf8').replace(/<\?xml[^>]*>/, '') : '';
  const css = fs.readFileSync(path.join(HERE, 'manual.css'), 'utf8');

  const cover = `<section class="cover">
  <div class="cover-band"></div>
  <div class="cover-logo">${logo}</div>
  <div class="cover-title">${esc(meta.title || 'AirView')}</div>
  <div class="cover-sub">${esc(meta.subtitle || 'Manual')}</div>
  <div class="cover-rule"></div>
  <div class="cover-covers">${esc(meta.covers || '')}</div>
  <div class="cover-foot">
    <div class="cover-edition">${esc(meta.edition || '')}</div>
    <div>${esc(meta.software || '')}</div>
    <div>${esc(meta.date || '')}</div>
  </div>
</section>`;

  const html = `<!doctype html>
<html lang="en-GB">
<head>
<meta charset="utf-8">
<title>${esc(meta.title || 'AirView')} ${esc(meta.subtitle || '')}, ${esc(meta.edition || '')}</title>
<meta name="author" content="AirView">
<meta name="description" content="${esc(meta.subtitle || '')} for ${esc(meta.software || '')}">
<style>
${css}
@page { @top-left { content: "${esc(meta.title || 'AirView')} · ${esc(meta.subtitle || '')}"; } }
${pageRules.join('\n')}
</style>
</head>
<body>
${cover}
<section class="toc" id="contents">
<h1 class="toc-title">Contents</h1>
<ol class="toc-list">
${tocHtml}
</ol>
</section>
${sections.join('\n')}
</body>
</html>
`;
  return { html, ids, toc };
}

// ---------------------------------------------------------------- lint

function lint(html) {
  const text = html
    .replace(/<style>[\s\S]*?<\/style>/g, '')
    .replace(/<pre[\s\S]*?<\/pre>/g, '')
    .replace(/<code>[\s\S]*?<\/code>/g, '')
    .replace(/<svg[\s\S]*?<\/svg>/g, '')
    .replace(/<[^>]+>/g, '');
  const checks = [
    [/\*\*/, 'raw ** emphasis'],
    [/\]\(/, 'raw link ]('],
    [/!\[/, 'raw image ![ '],
    [/(^|\n)\s*#{1,6} /, 'raw heading #'],
    [/\|\s*-{3,}/, 'raw table delimiter'],
    [/(^|\s)`[^`\s]/, 'raw backtick'],
    [/(^|\s)_[A-Za-z][^_]*_(\s|$)/, 'raw _emphasis_'],
  ];
  for (const [re, what] of checks) {
    const lines = text.split('\n').filter((l) => re.test(l));
    for (const l of lines.slice(0, 5)) warn(`lint: ${what}: ${l.trim().slice(0, 120)}`);
  }
}

// ---------------------------------------------------------------- Edge

function findEdge() {
  const cands = [
    opt.edge,
    process.env.AIRVIEW_MANUAL_EDGE,
    'C:\\Program Files (x86)\\Microsoft\\Edge\\Application\\msedge.exe',
    'C:\\Program Files\\Microsoft\\Edge\\Application\\msedge.exe',
    '/usr/bin/microsoft-edge',
    '/usr/bin/microsoft-edge-stable',
  ].filter(Boolean);
  for (const c of cands) if (fs.existsSync(c)) return c;
  throw new Error('Microsoft Edge not found; pass --edge <path to msedge>');
}

function printPdf(htmlFile, pdfFile) {
  const edge = findEdge();
  const profile = fs.mkdtempSync(path.join(os.tmpdir(), 'airview-manual-edge-'));
  try {
    if (fs.existsSync(pdfFile)) fs.unlinkSync(pdfFile);
    const r = spawnSync(edge, [
      '--headless',
      '--disable-gpu',
      '--no-first-run',
      '--no-default-browser-check',
      '--disable-extensions',
      '--allow-file-access-from-files',
      `--user-data-dir=${profile}`,
      '--no-pdf-header-footer',
      '--generate-pdf-document-outline',
      '--virtual-time-budget=20000',
      `--print-to-pdf=${pdfFile}`,
      pathToFileURL(htmlFile).href,
    ], { stdio: ['ignore', 'pipe', 'pipe'], timeout: 600000 });
    if (!fs.existsSync(pdfFile)) {
      throw new Error(`Edge did not write the PDF (exit ${r.status}): ${String(r.stderr).slice(0, 2000)}`);
    }
  } finally {
    try { fs.rmSync(profile, { recursive: true, force: true }); } catch { /* Edge may still hold files */ }
  }
}

// ---------------------------------------------------------------- PDF reading

// Minimal PDF reader: enough to map named destinations to page numbers and count pages.
function readPdf(file) {
  const buf = fs.readFileSync(file);
  const s = buf.toString('latin1');
  const objs = new Map();
  const re = /(\d+)\s+0\s+obj\b/g;
  let m;
  const starts = [];
  while ((m = re.exec(s))) starts.push([+m[1], m.index + m[0].length]);
  for (let k = 0; k < starts.length; k++) {
    const [num, from] = starts[k];
    let body = s.slice(from, k + 1 < starts.length ? starts[k + 1][1] : s.length);
    const sm = body.indexOf('stream');
    const dictPart = sm !== -1 ? body.slice(0, sm) : body.slice(0, body.indexOf('endobj') === -1 ? body.length : body.indexOf('endobj'));
    let stream = null;
    if (sm !== -1) {
      let ds = from + sm + 6;
      if (s[ds] === '\r') ds++;
      if (s[ds] === '\n') ds++;
      const lm = /\/Length\s+(\d+)(?!\s+\d+\s+R)/.exec(dictPart);
      const len = lm ? +lm[1] : s.indexOf('endstream', ds) - ds;
      stream = buf.subarray(ds, ds + len);
    }
    objs.set(num, { dict: dictPart, stream });
  }
  // Object streams (PDF 1.5+).
  for (const [, o] of [...objs]) {
    if (!/\/Type\s*\/ObjStm/.test(o.dict) || !o.stream) continue;
    let data = o.stream;
    if (/\/FlateDecode/.test(o.dict)) data = zlib.inflateSync(data);
    const t = data.toString('latin1');
    const n = +/\/N\s+(\d+)/.exec(o.dict)[1];
    const first = +/\/First\s+(\d+)/.exec(o.dict)[1];
    const hdr = t.slice(0, first).trim().split(/\s+/).map(Number);
    for (let k = 0; k < n; k++) {
      const on = hdr[2 * k], off = hdr[2 * k + 1];
      const end = k + 1 < n ? hdr[2 * k + 3] : t.length - first;
      if (!objs.has(on)) objs.set(on, { dict: t.slice(first + off, first + end), stream: null });
    }
  }
  const get = (ref) => objs.get(ref)?.dict ?? '';
  const trailerRoot = /\/Root\s+(\d+)\s+0\s+R/.exec(s);
  const catalog = get(+trailerRoot[1]);
  const pagesRoot = +/\/Pages\s+(\d+)\s+0\s+R/.exec(catalog)[1];
  const pages = [];
  const walk = (n) => {
    const d = get(n);
    if (/\/Type\s*\/Pages\b/.test(d)) {
      const kids = /\/Kids\s*\[([^\]]*)\]/.exec(d)[1];
      for (const km of kids.matchAll(/(\d+)\s+0\s+R/g)) walk(+km[1]);
    } else pages.push(n);
  };
  walk(pagesRoot);
  const pageIndex = new Map(pages.map((p, k) => [p, k + 1]));
  const dests = new Map();
  const decodeName = (n) => n.replace(/#([0-9A-Fa-f]{2})/g, (_, h) => String.fromCharCode(parseInt(h, 16)));
  const addFrom = (text) => {
    for (const dm of text.matchAll(/\/([^\s/[\]<>()]+)\s*\[\s*(\d+)\s+0\s+R/g)) {
      const pg = pageIndex.get(+dm[2]);
      if (pg) dests.set(Buffer.from(decodeName(dm[1]), 'latin1').toString('utf8'), pg);
    }
    for (const dm of text.matchAll(/\(((?:\\.|[^\\)])*)\)\s*\[\s*(\d+)\s+0\s+R/g)) {
      const pg = pageIndex.get(+dm[2]);
      if (pg) dests.set(dm[1].replace(/\\(.)/g, '$1'), pg);
    }
  };
  const dm = /\/Dests\s+(\d+)\s+0\s+R/.exec(catalog);
  if (dm) addFrom(get(+dm[1]));
  // Name tree (/Names << /Dests ... >>)
  const nm = /\/Names\s+(\d+)\s+0\s+R/.exec(catalog);
  if (nm) {
    const nd = get(+nm[1]);
    const dd = /\/Dests\s+(\d+)\s+0\s+R/.exec(nd);
    if (dd) {
      const walkTree = (n) => {
        const d = get(n);
        addFrom(d);
        const kids = /\/Kids\s*\[([^\]]*)\]/.exec(d);
        if (kids) for (const km of kids[1].matchAll(/(\d+)\s+0\s+R/g)) walkTree(+km[1]);
      };
      walkTree(+dd[1]);
    }
  }
  const mb = /\/MediaBox\s*\[\s*([\d.\s-]+)\]/.exec(get(pages[0]) || s);
  return { pageCount: pages.length, dests, mediaBox: mb ? mb[1].trim().split(/\s+/).map(Number) : null };
}

// ---------------------------------------------------------------- main

function main() {
  fs.mkdirSync(OUT, { recursive: true });
  const htmlFile = path.join(OUT, 'AirView-Manual.html');
  const tmpPdf = path.join(OUT, 'pass.pdf');

  let pageMap = null;
  let built;
  for (let pass = 1; pass <= 4; pass++) {
    warnings.length = 0;
    built = build(pageMap);
    fs.writeFileSync(htmlFile, built.html);
    if (opt.htmlOnly) break;
    printPdf(htmlFile, tmpPdf);
    const info = readPdf(tmpPdf);
    const next = new Map();
    for (const e of built.toc) if (info.dests.has(e.id)) next.set(e.id, info.dests.get(e.id));
    const missing = built.toc.filter((e) => !next.has(e.id)).length;
    console.log(`pass ${pass}: ${info.pageCount} pages, ${info.dests.size} destinations, ${built.toc.length - missing}/${built.toc.length} contents entries located` +
      (info.mediaBox ? `, page ${(info.mediaBox[2] / 72 * 25.4).toFixed(0)}x${(info.mediaBox[3] / 72 * 25.4).toFixed(0)} mm` : ''));
    if (missing) warn(`${missing} contents entries have no PDF destination (page numbers shown as 000)`);
    const stable = pageMap && [...next].every(([k, v]) => pageMap.get(k) === v) && next.size === pageMap.size;
    if (stable || next.size === 0) break;
    pageMap = next;
  }
  lint(built.html);

  if (!opt.htmlOnly) {
    fs.copyFileSync(tmpPdf, PDF);
    fs.unlinkSync(tmpPdf);
    console.log(`wrote ${PDF}`);
    if (!opt.noCopy) {
      fs.mkdirSync(path.dirname(DIST_PDF), { recursive: true });
      fs.copyFileSync(PDF, DIST_PDF);
      console.log(`copied to ${DIST_PDF}`);
    }
  } else {
    console.log(`wrote ${htmlFile}`);
  }
  if (warnings.length) {
    console.log(`\n${warnings.length} warning(s):`);
    for (const w of warnings) console.log('  ' + w);
  }
}

main();
