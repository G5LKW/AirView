package engine

import (
	"context"
	"fmt"
	"time"

	"airview/internal/met"
	"airview/internal/tracker"
)

// Winds and temperatures aloft on a grid, and BDS 4,5 hazard reports
// (internal/met/grid.go).

// hazardHook converts a tracker hazard report (called under the tracker's
// lock: only a slice append).
func (e *Engine) hazardHook(r tracker.HazardReport) {
	h := met.Hazard{T: r.T, ICAO: fmt.Sprintf("%06X", r.ICAO&0xFFFFFF), Callsign: r.Callsign}
	if r.PosOK {
		la, lo := r.Lat, r.Lon
		h.Lat, h.Lon = &la, &lo
	}
	if r.AltOK {
		a := r.AltFt
		h.AltFt = &a
	}
	u8 := func(ok bool, v uint8) *uint8 {
		if !ok {
			return nil
		}
		return &v
	}
	d := r.H
	h.Turbulence, h.WindShear, h.Microburst = u8(d.TurbulenceOK, d.Turbulence), u8(d.WindShearOK, d.WindShear), u8(d.MicroburstOK, d.Microburst)
	h.Icing, h.WakeVortex = u8(d.IcingOK, d.Icing), u8(d.WakeVortexOK, d.WakeVortex)
	if d.TempOK {
		t := d.Temp
		h.TempC = &t
	}
	if d.PressureOK {
		p := d.Pressure
		h.PressureHPa = &p
	}
	if d.RadioHeightOK {
		rh := d.RadioHeight
		h.RadioHtFt = &rh
	}
	e.hazards.Add(h)
}

// Hazards returns the BDS 4,5 reports since from.
func (e *Engine) Hazards(from time.Time) []met.Hazard {
	if e.hazards == nil {
		return []met.Hazard{}
	}
	return e.hazards.Since(from.UnixMilli())
}

// maxGridSamples bounds the observations read for one grid.
const maxGridSamples = 400_000

// MetGrid grids the stored aircraft observations of the last window.
func (e *Engine) MetGrid(ctx context.Context, q met.GridQuery, window time.Duration) (met.Grid, error) {
	now := time.Now()
	from := now.Add(-window)
	obs, err := e.st.MetSamplesSince(ctx, from.Unix(), maxGridSamples)
	if err != nil {
		return met.Grid{}, err
	}
	return met.BuildGrid(obs, q, from.UnixMilli(), now.UnixMilli()), nil
}
