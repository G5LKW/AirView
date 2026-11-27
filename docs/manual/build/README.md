# Building the AirView manual (PDF)

```
node docs/manual/build/build-manual.mjs            # build docs/manual/AirView-Manual-1.0.pdf and copy it to dist/
node docs/manual/build/build-manual.mjs --no-copy  # do not copy to dist/
node docs/manual/build/build-manual.mjs --html-only
node docs/manual/build/build-manual.mjs --edge "C:\path\to\msedge.exe"   # or AIRVIEW_MANUAL_EDGE
```

Needs Node.js 18 or newer (built-in modules only) and Microsoft Edge, which ships with Windows 11.

- Sources: `docs/manual/*.md`, ordered by the number in each file's `# ` heading. `00-cover-and-contents.md` holds
  the cover data (front matter between `---` lines) and "About this manual".
- `manual.css`: the print stylesheet: A4, CSS paged media, running headers per section, page numbers.
- The script converts Markdown to `build/out/AirView-Manual.html` with its own small converter (headings, lists,
  tables, fenced code, block quotes, images with captions, links between chapters), then prints with
  `msedge --headless --print-to-pdf`. It reads the named destinations back from the PDF to put page numbers into
  the contents and prints again until they are stable.
- Warnings list broken links between chapters, missing images, table rows with too many cells, and Markdown
  that was left in the text.
