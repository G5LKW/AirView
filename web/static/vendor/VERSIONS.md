# Vendored front-end libraries

Everything here is served from the AirView binary - nothing is loaded from a CDN at runtime.
Files were taken unmodified from the npm registry tarballs (`npm pack <name>@<version>`) on 2026-11-11,
except where noted.

| Library | Version | Licence | Files (from the tarball) |
|---|---|---|---|
| [MapLibre GL JS](https://maplibre.org/) (`maplibre-gl`) | 6.18.0 | BSD-3-Clause (`maplibre/LICENSE.txt`) | `dist/maplibre-gl.mjs`, `dist/maplibre-gl-worker.mjs`, `dist/maplibre-gl.css` |
| [Apache ECharts](https://echarts.apache.org/) (`echarts`) | 6.1.0 | Apache-2.0 (`echarts/LICENSE`, `echarts/NOTICE`) | `dist/echarts.esm.min.mjs` |
| [Inter](https://rsms.me/inter/) variable (`@fontsource-variable/inter`) | 5.3.0 | SIL OFL-1.1 (`fonts/LICENSE-Inter.txt`) | `files/inter-latin-wght-normal.woff2`, `files/inter-latin-ext-wght-normal.woff2` |
| [JetBrains Mono](https://www.jetbrains.com/lp/mono/) variable (`@fontsource-variable/jetbrains-mono`) | 5.3.0 | SIL OFL-1.1 (`fonts/LICENSE-JetBrainsMono.txt`) | `files/jetbrains-mono-latin-wght-normal.woff2`, `files/jetbrains-mono-latin-ext-wght-normal.woff2` |

## Notes

- **MapLibre GL JS 6 is ESM-only.** It is imported as a module
  (`import * as maplibregl from '../../vendor/maplibre/maplibre-gl.mjs'`) and starts its worker as a
  *module worker* from `maplibre-gl-worker.mjs` in the same directory (the URL is derived from
  `import.meta.url`, with a `?v=<version>` query). Both files must therefore be served with a JavaScript
  MIME type (`text/javascript`) - `web.Handler()` / `web.ContentType()` take care of that.
- The only modification: the trailing `//# sourceMappingURL=...` comment was removed from the two MapLibre
  `.mjs` files because the (multi-megabyte) source maps are not vendored.
- ECharts is loaded lazily by `js/charts.js` (dynamic `import()`), so pages that draw no chart never parse it.
  ECharts bundles zrender (BSD-3-Clause) and d3-derived code (BSD-3-Clause), see the upstream `licenses/` folder.
- Fonts: only the `latin` and `latin-ext` subsets of the upright variable fonts are shipped
  (`@font-face` rules live in `css/tokens.css`).

## Updating

```
npm pack maplibre-gl echarts @fontsource-variable/inter @fontsource-variable/jetbrains-mono
```

Unpack each tarball, copy the files listed above over the ones here, strip the `sourceMappingURL` comment
from the MapLibre modules, and update this table.

## Map data (not vendored - fetched at runtime, optionally through `/tilecache/`)

| Service | Used for | Terms |
|---|---|---|
| [OpenFreeMap](https://openfreemap.org/) `tiles.openfreemap.org` | vector tiles, glyphs, sprite | Free, no key, no registration. Attribution shown on the map: "OpenFreeMap © OpenMapTiles Data from OpenStreetMap". |
| [OpenStreetMap](https://www.openstreetmap.org/copyright) `tile.openstreetmap.org` | optional raster base map | OSMF tile usage policy: interactive viewing only, no bulk/offline download, visible "© OpenStreetMap contributors" attribution, Referer must not be stripped, a proxy must send an identifying User-Agent. |
| [RainViewer](https://www.rainviewer.com/api.html) `api.rainviewer.com`, `tilecache.rainviewer.com` | optional precipitation radar overlay | Free for personal/educational use, attribution with link required, max zoom 7, colour scheme 2. |
