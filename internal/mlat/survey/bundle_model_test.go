package survey

import (
	"testing"
	"time"

	"airview/internal/model"
)

func TestBundleConfigFromModel(t *testing.T) {
	c := BundleConfigFromModel(model.AggMLATBundle{EveryMin: 30, SpanMin: 90, MaxStations: 100, WindowS: 20, ThinIntervalS: 4,
		MaxFrames: 50000, MinStationRecs: 200, OutlierMinM: 150, OutlierK: 4, DeclaredSigmaM: 40, AnchorSigmaM: 2, MaxReleases: 8, Workers: 3})
	c.Normalize()
	if c.Every != 30*time.Minute || c.Span != 90*time.Minute || c.MaxStations != 100 || c.WindowS != 20 || c.ThinInterval != 4*time.Second ||
		c.MaxFrames != 50000 || c.MinStationRecs != 200 || c.OutlierMinM != 150 || c.OutlierK != 4 || c.DeclaredSigmaHM != 40 ||
		c.AnchorSigmaHM != 2 || c.MaxReleases != 8 || c.Workers != 3 {
		t.Fatalf("%+v", c)
	}
	d := BundleConfigFromModel(model.AggMLATBundle{})
	d.Normalize()
	if d.Every != time.Hour || d.MaxStations != 64 {
		t.Fatalf("defaults %+v", d)
	}
}
