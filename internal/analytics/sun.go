package analytics

import (
	"context"
	"time"

	"airview/internal/astro"
)

// SunInfo is the sun / day-night state for the UI.
type SunInfo struct {
	T        int64   `json:"t"` // Unix ms the values are valid for
	Located  bool    `json:"located"`
	Receiver *LatLon `json:"receiver,omitempty"`
	// Position, Phase and Events are for the receiver position and absent
	// until it is set.
	Position *astro.SunPosition `json:"position,omitempty"`
	Phase    string             `json:"phase,omitempty"` // "day" | "civil" | "nautical" | "astronomical" | "night"
	IsDay    bool               `json:"isDay"`
	Events   *astro.EventsMs    `json:"events,omitempty"` // today's sunrise / sunset / twilights
	// NightSide holds the terminator: four nested rings ([lon, lat]) of the
	// region where the sun is below -0.833 / -6 / -12 / -18 degrees, and the
	// sub-solar point.
	NightSide astro.NightSide `json:"nightSide"`
	Moon      astro.Moon      `json:"moon"`
}

// Sun returns the current sun elevation / azimuth and phase at the
// receiver, today's sunrise / sunset / twilight times and the terminator
// rings for the map. A zero t means now.
func (a *Analyzer) Sun(ctx context.Context, t time.Time) (*SunInfo, error) {
	if t.IsZero() {
		t, _ = a.clock()
	}
	t = t.Truncate(30 * time.Second)
	cfg := a.cfg()
	return cached(ctx, a, a.key("sun", "", cfg, t.Unix()), ttlSun, func(ctx context.Context) (*SunInfo, error) {
		s := siteOf(cfg, "")
		res := &SunInfo{T: t.UnixMilli(), Located: s.located, Receiver: s.pos(),
			NightSide: astro.NightSideAt(t, astro.DefaultTerminatorStepDeg), Moon: astro.MoonAt(t)}
		if s.located {
			pos := astro.SunAt(t, s.lat, s.lon)
			ph := astro.PhaseOfElevation(pos.TrueElevationDeg)
			ev := astro.DayEvents(t, s.lat, s.lon).Ms()
			res.Position, res.Phase, res.IsDay, res.Events = &pos, ph.String(), ph == astro.Day, &ev
		}
		return res, nil
	})
}
