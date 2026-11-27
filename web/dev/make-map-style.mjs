#!/usr/bin/env node
// Regenerates web/static/map/airview-dark.json from the stock OpenFreeMap "dark" style.
//
//   node web/dev/make-map-style.mjs [path-or-url-of-stock-dark-style]
//
// Default input: https://tiles.openfreemap.org/styles/dark
// The stock style is recoloured to the AirView black / electric-blue palette, a coastline glow,
// aerodrome labels and brighter runways are added. Sources, glyphs and sprite stay on
// tiles.openfreemap.org (the UI rewrites them to /tilecache/... when the tile cache is enabled).
import { readFile, writeFile } from 'node:fs/promises';
import { fileURLToPath } from 'node:url';
import path from 'node:path';

const here = path.dirname(fileURLToPath(import.meta.url));
const outFile = path.resolve(here, '../static/map/airview-dark.json');
const input = process.argv[2] || 'https://tiles.openfreemap.org/styles/dark';

const P = {
  land: '#07122b',
  water: '#01050e',
  coast: '#2a6bd0',
  coastGlow: '#11408f',
  ice: '#0b1830',
  wood: '#06141f',
  park: '#071826',
  residential: '#081326',
  building: '#0a1a35',
  buildingLine: '#12284d',
  roadMinor: '#0e2143',
  roadMajor: '#14305f',
  roadMajorCasing: '#040a16',
  motorway: '#1b4183',
  motorwayLow: '#132f60',
  rail: '#122747',
  railDash: '#050c1b',
  path: '#0e2143',
  boundaryCountry: '#2e62b4',
  boundaryState: '#1b3b70',
  runway: '#3f78d0',
  runwayCasing: '#0b1d3d',
  taxiway: '#1f4380',
  aeroArea: '#0a1c3c',
  halo: 'rgba(1,4,11,0.85)',
  textCountry: '#6f93cc',
  textState: '#40639c',
  textCityLarge: '#b4cbee',
  textCity: '#8eaad6',
  textTown: '#6e8bb8',
  textSmall: '#56719c',
  textWater: '#2b5a9e',
  textRoad: '#4a6a9c',
  textAero: '#58b6ff',
};

async function load(src) {
  if (/^https?:/.test(src)) {
    const r = await fetch(src);
    if (!r.ok) throw new Error(`fetch ${src}: HTTP ${r.status}`);
    return r.json();
  }
  return JSON.parse(await readFile(src, 'utf8'));
}

const style = await load(input);
const byId = new Map(style.layers.map((l) => [l.id, l]));
const paint = (id, p) => {
  const l = byId.get(id);
  if (!l) { console.warn('layer missing in stock style:', id); return; }
  l.paint = { ...(l.paint || {}), ...p };
};
const layout = (id, p) => {
  const l = byId.get(id);
  if (!l) return;
  l.layout = { ...(l.layout || {}), ...p };
};
const drop = (id, ...keys) => { const l = byId.get(id); if (l && l.paint) for (const k of keys) delete l.paint[k]; };

// ---- base ----
paint('background', { 'background-color': P.land });
paint('water', { 'fill-color': P.water, 'fill-antialias': true });
paint('landcover_ice_shelf', { 'fill-color': P.ice, 'fill-opacity': 0.6 });
paint('landcover_glacier', { 'fill-color': P.ice });
paint('landuse_residential', { 'fill-color': P.residential, 'fill-opacity': 0.55 });
paint('landcover_wood', { 'fill-color': P.wood });
drop('landcover_wood', 'fill-pattern');
paint('landuse_park', { 'fill-color': P.park, 'fill-opacity': 0.6 });
paint('waterway', { 'line-color': P.coastGlow });
paint('building', { 'fill-color': P.building, 'fill-outline-color': P.buildingLine });

// ---- aeroways: these matter to an aviation display, make them read ----
paint('aeroway-area', { 'fill-color': P.aeroArea, 'fill-opacity': 0.9 });
paint('aeroway-taxiway', { 'line-color': P.taxiway });
paint('aeroway-runway-casing', { 'line-color': P.runwayCasing });
paint('aeroway-runway', { 'line-color': P.runway });
for (const id of ['aeroway-runway', 'aeroway-runway-casing']) {
  const l = byId.get(id);
  if (l) {
    l.minzoom = 8;
    l.paint['line-width'] = ['interpolate', ['exponential', 1.5], ['zoom'], 8, id.endsWith('casing') ? 2.2 : 1.2, 11, id.endsWith('casing') ? 5 : 3.2, 17, id.endsWith('casing') ? 55 : 50];
  }
}

// ---- roads & rail ----
paint('road_area_pier', { 'fill-color': P.land });
paint('road_pier', { 'line-color': P.land });
paint('highway_path', { 'line-color': P.path });
paint('highway_minor', { 'line-color': P.roadMinor });
paint('highway_major_casing', { 'line-color': P.roadMajorCasing });
paint('highway_major_inner', { 'line-color': P.roadMajor });
paint('highway_major_subtle', { 'line-color': P.roadMinor });
paint('highway_motorway_casing', { 'line-color': P.roadMajorCasing });
paint('highway_motorway_inner', { 'line-color': ['interpolate', ['linear'], ['zoom'], 5.8, P.motorwayLow, 9, P.motorway] });
paint('highway_motorway_subtle', { 'line-color': P.motorwayLow, 'line-opacity': 0.7 });
for (const id of ['railway_transit', 'railway_minor', 'railway']) paint(id, { 'line-color': P.rail });
for (const id of ['railway_transit_dashline', 'railway_minor_dashline', 'railway_dashline']) paint(id, { 'line-color': P.railDash });

// ---- boundaries ----
paint('boundary_state', { 'line-color': P.boundaryState, 'line-opacity': 0.8 });
paint('boundary_country_z0-4', { 'line-color': P.boundaryCountry, 'line-opacity': 0.75 });
paint('boundary_country_z5-', { 'line-color': P.boundaryCountry, 'line-opacity': 0.8 });

// ---- labels ----
paint('water_name', { 'text-color': P.textWater, 'text-halo-color': P.halo, 'text-halo-width': 1 });
layout('water_name', { 'text-letter-spacing': 0.12 });
paint('highway_name_other', { 'text-color': P.textRoad, 'text-halo-color': P.halo });
paint('highway_name_motorway', { 'text-color': P.textRoad, 'text-halo-color': P.halo, 'text-halo-width': 1 });
const place = (id, color, extra = {}) => {
  paint(id, { 'text-color': color, 'text-halo-color': P.halo, 'text-halo-width': 1.2, 'text-halo-blur': 0.5, ...extra });
  layout(id, { 'text-letter-spacing': 0.06 });
};
place('place_other', P.textSmall);
place('place_suburb', P.textSmall);
place('place_village', P.textSmall);
place('place_town', P.textTown);
place('place_city', P.textCity);
place('place_city_large', P.textCityLarge);
place('place_state', P.textState);
place('place_country_other', P.textCountry);
place('place_country_minor', P.textCountry);
place('place_country_major', P.textCountry);
for (const id of ['place_state', 'place_country_other', 'place_country_minor', 'place_country_major']) layout(id, { 'text-letter-spacing': 0.18 });

// The stock style draws a "circle-11" dot next to town/city names, but that image is not in the
// OpenFreeMap sprite (MapLibre warns about it); drop the icon and centre the text instead.
for (const l of style.layers) {
  if (l.type === 'symbol' && l.layout && l.layout['icon-image'] && JSON.stringify(l.layout['icon-image']).includes('circle-11')) {
    for (const k of ['icon-image', 'icon-size', 'text-offset', 'text-justify']) delete l.layout[k];
    l.layout['text-anchor'] = 'center';
    if (l.paint) delete l.paint['icon-opacity'];
  }
}

// ---- additions ----
const insertAfter = (afterId, layer) => {
  const i = style.layers.findIndex((l) => l.id === afterId);
  style.layers.splice(i < 0 ? style.layers.length : i + 1, 0, layer);
};
const waterFilter = byId.get('water')?.filter;
insertAfter('water', {
  id: 'coast_glow', type: 'line', source: 'openmaptiles', 'source-layer': 'water',
  ...(waterFilter ? { filter: waterFilter } : {}),
  paint: {
    'line-color': P.coastGlow, 'line-opacity': 0.55, 'line-blur': 3,
    'line-width': ['interpolate', ['linear'], ['zoom'], 2, 2.5, 8, 4, 14, 6],
  },
});
insertAfter('coast_glow', {
  id: 'coast_line', type: 'line', source: 'openmaptiles', 'source-layer': 'water',
  ...(waterFilter ? { filter: waterFilter } : {}),
  paint: {
    'line-color': P.coast, 'line-opacity': 0.9,
    'line-width': ['interpolate', ['linear'], ['zoom'], 2, 0.5, 8, 0.8, 14, 1.2],
  },
});
style.layers.push({
  id: 'aerodrome_label', type: 'symbol', source: 'openmaptiles', 'source-layer': 'aerodrome_label', minzoom: 7,
  filter: ['any', ['has', 'iata'], ['has', 'icao']],
  layout: {
    'text-field': ['step', ['zoom'],
      ['coalesce', ['get', 'iata'], ['get', 'icao']], 10,
      ['concat', ['coalesce', ['get', 'iata'], ['get', 'icao']], '\n', ['coalesce', ['get', 'name:en'], ['get', 'name'], '']]],
    'text-font': ['Noto Sans Bold'],
    'text-size': ['interpolate', ['linear'], ['zoom'], 7, 9.5, 12, 11.5],
    'text-letter-spacing': 0.08,
    'text-max-width': 9,
    'text-padding': 6,
  },
  paint: { 'text-color': P.textAero, 'text-halo-color': P.halo, 'text-halo-width': 1.4, 'text-opacity': ['interpolate', ['linear'], ['zoom'], 7, 0.75, 10, 1] },
});

// The stock style declares a Natural Earth raster source it never draws; drop it.
for (const [id] of Object.entries(style.sources)) {
  if (!style.layers.some((l) => l.source === id)) delete style.sources[id];
}
style.sources.openmaptiles.attribution =
  '<a href="https://openfreemap.org" target="_blank" rel="noopener">OpenFreeMap</a> ' +
  '<a href="https://www.openmaptiles.org/" target="_blank" rel="noopener">&copy; OpenMapTiles</a> ' +
  'Data from <a href="https://www.openstreetmap.org/copyright" target="_blank" rel="noopener">OpenStreetMap</a>';

const out = {
  version: 8,
  name: 'AirView Dark',
  metadata: {
    'airview:derivedFrom': 'https://tiles.openfreemap.org/styles/dark',
    'airview:generator': 'web/dev/make-map-style.mjs',
  },
  sources: style.sources,
  sprite: style.sprite,
  glyphs: style.glyphs,
  layers: style.layers,
};
await writeFile(outFile, JSON.stringify(out, null, 1) + '\n');
console.log(`wrote ${outFile}: ${out.layers.length} layers`);
