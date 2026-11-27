# Embedded geoid data

`internal/geoid` embeds this directory. The file it looks for is
`egm96-1deg.avg`: the EGM96 geoid undulations on a 1° global grid
(181 rows from 90°N to 90°S × 360 columns from 0°E), int16 centimetres,
in the AVGEOID1 format documented in `../geoid.go` (≈130 KB).

## Licence

EGM96 is a product of the US National Geospatial-Intelligence Agency
(NGA) and NASA. It is a work of the US Government and is in the public
domain. No AirView or third-party code is involved in producing the grid;
`cmd/geoidgen` (our own code) only resamples the published values.

## How to (re)generate

1. Download NGA's EGM96 15′ geoid grid `WW15MGH.GRD` (≈ 9 MB ASCII) from the
   NGA Office of Geomatics EGM96 page (earth-info.nga.mil, "EGM96" ->
   "Interpolation grid"), or GeographicLib's `egm96-15.pgm` (the same NGA
   values, 3 mm quantisation). Keep the download in a scratch directory,
   not in the repository.
2. Run from the repository root:

   ```
   go run ./internal/geoid/cmd/geoidgen -in <path>/WW15MGH.GRD -out internal/geoid/data/egm96-1deg.avg
   ```

   The tool keeps every 4th node of the 15′ grid (the exact 1° nodes; no
   smoothing), writes the file and prints the NGA reference test points
   with their interpolated values.
3. `go test ./internal/geoid` then also runs the reference-point checks
   (they are skipped while the file is absent).

Until the file exists, `geoid.Available()` is false and the conversion
functions return `ok = false`; operators can also point the application at
any grid file with `geoid.Load` + `geoid.SetDefault` (all three formats are
accepted).
