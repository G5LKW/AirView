package survey

import (
	"time"

	"airview/internal/model"
)

// BundleConfigFromModel maps the config file block
// aggregator.mlat.survey.bundle onto a BundleConfig (zero = default; the
// returned config is not normalised).
func BundleConfigFromModel(m model.AggMLATBundle) BundleConfig {
	c := BundleConfig{
		WindowS:        m.WindowS,
		MinStationRecs: m.MinStationRecs,
		MaxStations:    m.MaxStations,
		OutlierMinM:    m.OutlierMinM,
		OutlierK:       m.OutlierK,
		MaxReleases:    m.MaxReleases,
		Workers:        m.Workers,
		MaxFrames:      m.MaxFrames,
	}
	if m.DeclaredSigmaM > 0 {
		c.DeclaredSigmaHM, c.DeclaredSigmaVM = m.DeclaredSigmaM, m.DeclaredSigmaM
	}
	if m.AnchorSigmaM > 0 {
		c.AnchorSigmaHM = m.AnchorSigmaM
	}
	if m.EveryMin > 0 {
		c.Every = time.Duration(m.EveryMin) * time.Minute
	}
	if m.SpanMin > 0 {
		c.Span = time.Duration(m.SpanMin) * time.Minute
	}
	if m.ThinIntervalS > 0 {
		c.ThinInterval = time.Duration(m.ThinIntervalS) * time.Second
	}
	return c
}
