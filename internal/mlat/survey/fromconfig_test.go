package survey

import (
	"reflect"
	"testing"
	"time"

	"airview/internal/model"
)

// TestConfigFromModelSurveyBlock: zero maps to the defaults, and a
// non-zero value of every field of aggregator.mlat.survey arrives.
func TestConfigFromModelSurveyBlock(t *testing.T) {
	d := DefaultConfig()
	m := model.DefaultAggregator().MLAT
	if got := ConfigFromModel(m); !reflect.DeepEqual(got, d) {
		t.Fatalf("defaults differ:\n%+v\n%+v", got, d)
	}
	m.Survey = model.AggMLATSurvey{MaxJobs: 7, MaxCheckFrac: 0.5, MinDurationSec: 60, EvalEverySec: 30,
		MaxDurationMin: 20, NoDataAfterMin: 5, RetryAfterMin: 15, OwnerMinIntervalMin: 10, MaxObs: 1000,
		MinIntervalMs: 500, MinPeers: 3, MaxPeerSigmaNs: 300, MaskDeg: 5, HeightSigmaM: 30, DefaultAglM: 4,
		SearchRadiusKm: 20, StableSigmaGpsM: 25, StableSigmaM: 150, MinAzSpreadDeg: 120, MinAircraft: 8,
		SuspectMinM: 80, SuspectK: 4}
	c := ConfigFromModel(m)
	want := d
	want.MaxJobs, want.MaxCheckFrac, want.MinDuration, want.EvalEvery = 7, 0.5, time.Minute, 30*time.Second
	want.MaxDuration, want.NoDataAfter, want.RetryAfter, want.OwnerMinInterval = 20*time.Minute, 5*time.Minute, 15*time.Minute, 10*time.Minute
	want.MaxObs, want.MinInterval, want.MinPeers, want.MaxPeerSigmaNs = 1000, 500*time.Millisecond, 3, 300
	want.MaskDeg, want.HeightSigmaM, want.DefaultAGLM, want.SearchRadiusM = 5, 30, 4, 20e3
	want.StableSigmaGPSM, want.StableSigmaM, want.MinAzSpreadDeg, want.MinAircraft = 25, 150, 120, 8
	want.SuspectMinM, want.SuspectK = 80, 4
	if !reflect.DeepEqual(c, want) {
		t.Fatalf("got  %+v\nwant %+v", c, want)
	}
	m.Survey = model.AggMLATSurvey{OwnerMinIntervalMin: -1}
	if c := ConfigFromModel(m); c.OwnerMinInterval > time.Millisecond {
		t.Fatalf("negative ownerMinIntervalMin: %v", c.OwnerMinInterval)
	}
}
