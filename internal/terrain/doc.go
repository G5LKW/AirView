// Package terrain provides terrain elevation data and 1090 MHz radio
// line-of-sight analysis for an AirView station.
//
// # Elevation data
//
// The default source is the AWS Open Data "Terrain Tiles" dataset (Mapzen /
// Tilezen Joerd, bucket elevation-tiles-prod, us-east-1), Terrarium encoding:
//
//	https://s3.amazonaws.com/elevation-tiles-prod/terrarium/{z}/{x}/{y}.png
//
// 256×256 Web-Mercator PNG tiles, zoom 0..15, no API key. A pixel decodes to
// metres as (R·256 + G + B/256) − 32768 (ocean pixels carry bathymetry, which
// the Provider clamps to sea level by default). Attribution is required, see
// Attribution. Tiles are cached on disk forever (subject to a size cap) and
// decoded tiles are kept in a small in-memory cache sized for a Raspberry Pi.
// Missing data (offline, never downloaded) reads as sea level with ok=false.
//
// # Radio geometry
//
// Everything uses the effective-earth model (radius k·R, k = 4/3 by default)
// of internal/geo: straight rays over an earth of radius k·R. The terrain
// profile along a path is lifted by the earth bulge d1·d2/(2kR) (ITU-R P.526
// §4 / P.452 convention) for LOS, Fresnel and knife-edge calculations; the
// polar sweeps (horizon panorama, coverage) use exact elevation angles
// (geo.ElevationDeg) instead.
//
// Knife-edge diffraction follows ITU-R P.526 (eq. 31 approximation of J(v));
// multiple edges use the Deygout construction (main edge + one secondary edge
// each side).
//
// # Concurrency and cost
//
// Provider, Panorama, Analysis and Service are safe for concurrent use.
// Analysis.Visible / Panorama.Visible cost a few hundred nanoseconds and do not
// allocate: they are meant for 1 Hz per-aircraft use. Heavy work (tile
// download, polar terrain sampling, panoramas, coverage) runs inside the
// Service background job with progress and cancellation.
package terrain

// Default data source.
const (
	// DefaultTerrariumURL is the AWS Open Data Terrain Tiles endpoint
	// (verified: HTTP 200 image/png, ~100-150 kB per tile; z ≤ 15).
	DefaultTerrariumURL = "https://s3.amazonaws.com/elevation-tiles-prod/terrarium/{z}/{x}/{y}.png"

	// MaxTerrariumZoom is the highest zoom level published for Terrarium tiles.
	MaxTerrariumZoom = 15

	// AttributionShort is a one-line credit for map/UI footers.
	AttributionShort = "Terrain: Mapzen Terrain Tiles on AWS Open Data (SRTM, GMTED2010, 3DEP courtesy USGS; ETOPO1 NOAA; EU-DEM Copernicus; and others)"

	// AttributionURL is the dataset's licence/attribution page.
	AttributionURL = "https://github.com/tilezen/joerd/blob/master/docs/attribution.md"
)

// Attribution is the full attribution text required by the Terrain Tiles
// dataset (tilezen/joerd docs/attribution.md, "Required attribution").
const Attribution = `* ArcticDEM terrain data DEM(s) were created from DigitalGlobe, Inc., imagery and funded under National Science Foundation awards 1043681, 1559691, and 1542736;
* Australia terrain data © Commonwealth of Australia (Geoscience Australia) 2017;
* Austria terrain data © offene Daten Österreichs – Digitales Geländemodell (DGM) Österreich;
* Canada terrain data contains information licensed under the Open Government Licence – Canada;
* Europe terrain data produced using Copernicus data and information funded by the European Union - EU-DEM layers;
* Global ETOPO1 terrain data U.S. National Oceanic and Atmospheric Administration
* Mexico terrain data source: INEGI, Continental relief, 2016;
* New Zealand terrain data Copyright 2011 Crown copyright (c) Land Information New Zealand and the New Zealand Government (All rights reserved);
* Norway terrain data © Kartverket;
* United Kingdom terrain data © Environment Agency copyright and/or database right 2015. All rights reserved;
* United States 3DEP (formerly NED) and global GMTED2010 and SRTM terrain data courtesy of the U.S. Geological Survey.`
