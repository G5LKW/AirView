// Package navdata holds free aviation reference data for AirView: airports,
// runways, navaids and countries from OurAirports (public domain), callsign
// routes from the Virtual Radar Server "standing data" repository (CC0), an
// optional on-demand route lookup (adsbdb.com) and an optional aircraft photo
// metadata lookup (planespotters.net). Everything is stored in SQLite through
// internal/store in tables prefixed "navdata_" (registered from init).
//
// Nothing is downloaded or sent to a third party unless the user asks for it:
// imports run only on request (NavData.StartImport / Import), and the online
// route and photo lookups are off by default (Options.Routes.Online,
// Options.Photos.Enabled).
//
// Hot-path users (a trajectory predictor recognising approaches, the tracker
// guessing departure/arrival airports) should load an in-memory Region around
// the receiver once (LoadRegion) and query that; it implements RunwayIndex.
//
// All result types are JSON-ready: unknown values are omitted (pointers), never
// NaN. Coordinates in GeoJSON-style arrays are [lon, lat].
package navdata
