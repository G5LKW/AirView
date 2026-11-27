// map/basemaps.js - base map styles, attribution and the tile-cache request rewrite.
//
// Verified 2026-11 against the providers' own pages:
//  - OpenFreeMap (https://openfreemap.org/quick_start/): free, no key; styles at
//    https://tiles.openfreemap.org/styles/{liberty,bright,positron,dark,fiord}; TileJSON at /planet;
//    glyphs at /fonts/{fontstack}/{range}.pbf; attribution "OpenFreeMap (c) OpenMapTiles Data from OpenStreetMap".
//  - OpenStreetMap standard tiles (https://operations.osmfoundation.org/policies/tiles/): the URL must be exactly
//    https://tile.openstreetmap.org/{z}/{x}/{y}.png, interactive viewing only, visible attribution, Referer intact.
//  - RainViewer (https://www.rainviewer.com/api/weather-maps-api.html): index JSON at
//    https://api.rainviewer.com/public/weather-maps.json, tiles {host}{path}/{size}/{z}/{x}/{y}/{color}/{smooth}_{snow}.png,
//    size 256|512, max zoom 7, colour scheme 2 ("Universal Blue"), personal/educational use with attribution.

export const OFM_GLYPHS = 'https://tiles.openfreemap.org/fonts/{fontstack}/{range}.pbf';
export const LABEL_FONT = ['Noto Sans Bold'];
export const LABEL_FONT_REGULAR = ['Noto Sans Regular'];

const OSM_ATTRIBUTION = '&copy; <a href="https://www.openstreetmap.org/copyright" target="_blank" rel="noopener">OpenStreetMap</a> contributors';
export const RAINVIEWER_ATTRIBUTION = 'Radar: <a href="https://www.rainviewer.com/" target="_blank" rel="noopener">RainViewer</a>';
export const RAINVIEWER_INDEX = 'https://api.rainviewer.com/public/weather-maps.json';

/** Upstream hosts the UI will route through /tilecache/<host>/<path> when status.tileCache is on. */
export const TILE_CACHE_HOSTS = new Set(['tiles.openfreemap.org', 'tile.openstreetmap.org', 'tilecache.rainviewer.com']);

function osmRaster(dim) {
  return {
    version: 8,
    name: dim ? 'OpenStreetMap (night)' : 'OpenStreetMap',
    glyphs: OFM_GLYPHS,
    sources: {
      osm: { type: 'raster', tiles: ['https://tile.openstreetmap.org/{z}/{x}/{y}.png'], tileSize: 256, maxzoom: 19, attribution: OSM_ATTRIBUTION },
    },
    layers: [
      { id: 'background', type: 'background', paint: { 'background-color': dim ? '#050c1b' : '#dfe6ee' } },
      {
        id: 'osm', type: 'raster', source: 'osm',
        // brightness min > max inverts the tiles; the hue rotation brings water back to blue.
        paint: dim
          ? { 'raster-brightness-min': 0.86, 'raster-brightness-max': 0.04, 'raster-hue-rotate': 195, 'raster-saturation': -0.35, 'raster-contrast': 0.08, 'raster-fade-duration': 0 }
          : { 'raster-fade-duration': 0 },
      },
    ],
  };
}

/**
 * Selectable base maps. `style` is a URL (relative ones resolve against the document) or a style object.
 * `dark` tells the page whether overlays should use light-on-dark or dark-on-light strokes.
 */
export const BASEMAPS = [
  { id: 'airview-dark', label: 'AirView Dark (vector)', dark: true, style: 'map/airview-dark.json' },
  { id: 'osm-night', label: 'OpenStreetMap night (raster)', dark: true, style: osmRaster(true) },
  { id: 'osm', label: 'OpenStreetMap (raster)', dark: false, style: osmRaster(false) },
  { id: 'ofm-liberty', label: 'OpenFreeMap Liberty (vector, light)', dark: false, style: 'https://tiles.openfreemap.org/styles/liberty' },
];

export function basemapById(id) {
  return BASEMAPS.find((b) => b.id === id) || BASEMAPS[0];
}

/** Absolute URL / object suitable for map.setStyle(). */
export function styleFor(basemap) {
  return typeof basemap.style === 'string' ? new URL(basemap.style, document.baseURI).href : basemap.style;
}

/**
 * transformRequest for MapLibre: when the tile cache is enabled, rewrite whitelisted upstream
 * https URLs (styles, TileJSON, tiles, glyphs, sprites, radar frames) to the local caching proxy
 *   https://<host>/<path>?<query>  ->  /tilecache/<host>/<path>?<query>
 * @param {() => boolean} enabled evaluated per request (follows status.tileCache live)
 */
export function makeTransformRequest(enabled) {
  return (url) => {
    const local = rewriteForTileCache(url, enabled());
    return local ? { url: local } : undefined;
  };
}

/** Returns the /tilecache URL for a whitelisted upstream URL, or null when it should be fetched directly. */
export function rewriteForTileCache(url, enabled) {
  if (!enabled || typeof url !== 'string') return null;
  let u;
  try { u = new URL(url, document.baseURI); } catch { return null; }
  if (u.protocol !== 'https:' || u.origin === location.origin || !TILE_CACHE_HOSTS.has(u.hostname) || u.port) return null;
  return `${location.origin}/tilecache/${u.hostname}${u.pathname}${u.search}`;
}
