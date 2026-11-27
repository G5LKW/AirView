package tracker

import (
	"testing"
	"time"

	"airview/internal/model"
)

// Network MLAT results carry their grade and sigma into the aircraft DTO
// (HOOKS: WP0-contract -> internal/tracker); a later non-MLAT position
// clears them.
func TestNetworkMLATGradeAndSigma(t *testing.T) {
	cfg := model.DefaultConfig()
	cfg.Receiver.Located, cfg.Receiver.Lat, cfg.Receiver.Lon = true, 50.0, 8.5
	cfg.Feeds = []model.Feed{{ID: "sbs", Enabled: true, Type: model.FeedSBS}}
	tr := New(Options{Config: &cfg})
	const icao = 0x3C4B26
	upd := func(i int, src string, grade uint8, sigma *float64) {
		tr.HandleUpdate(model.DecodedUpdate{
			Recv: simStart.Add(time.Duration(i) * time.Second), FeedID: "sbs", ICAO: icao,
			Lat: model.F(50.5 + 0.001*float64(i)), Lon: model.F(9.0), AltBaro: model.F(30000),
			Src: src, Grade: grade, SigmaM: sigma, Msgs: 1,
		})
	}
	for i := 0; i < 5; i++ {
		upd(i, model.SrcMLATAV, model.GradeB, model.F(42.5))
	}
	ac, ok := tr.Aircraft(icao)
	if !ok {
		t.Fatal("aircraft not tracked")
	}
	if ac.Source != model.SrcMLAT || ac.MLATGrade != "B" || ac.MLATSigmaM == nil || *ac.MLATSigmaM != 42.5 {
		t.Fatalf("mlat fields: source %q grade %q sigma %v", ac.Source, ac.MLATGrade, ac.MLATSigmaM)
	}
	// A grade that is not displayable keeps the sigma but no grade.
	upd(5, model.SrcMLATExt, model.GradeNone, model.F(900))
	ac, _ = tr.Aircraft(icao)
	if ac.MLATGrade != "" || ac.MLATSigmaM == nil || *ac.MLATSigmaM != 900 {
		t.Fatalf("ungraded: grade %q sigma %v", ac.MLATGrade, ac.MLATSigmaM)
	}
	// A plain position replaces the MLAT one.
	upd(6, "", 0, nil)
	ac, _ = tr.Aircraft(icao)
	if ac.MLATGrade != "" || ac.MLATSigmaM != nil || ac.Source == model.SrcMLAT {
		t.Fatalf("after plain position: source %q grade %q sigma %v", ac.Source, ac.MLATGrade, ac.MLATSigmaM)
	}
}

// An aircraft first heard through Mode S frames (a Mode S-only aircraft)
// gets its position from MLAT results: decoded positions are independent
// fixes, so two agreeing results start the track (regression: the update
// path treated them as relative fixes and rejected every one).
func TestMLATStartsModeSOnlyTrack(t *testing.T) {
	cfg := model.DefaultConfig()
	cfg.Receiver.Located, cfg.Receiver.Lat, cfg.Receiver.Lon = true, 50.0, 8.5
	tr := New(Options{Config: &cfg})
	const icao = 0x3C4B27
	for i := 0; i < 4; i++ {
		tr.HandleFrame(model.RawFrame{Recv: simStart.Add(time.Duration(i) * 200 * time.Millisecond), GPSNs: -1, Signal: -20,
			Kind: model.FrameModeSShort, Data: df11(icao), FeedID: "x", Channel: -1})
	}
	for i := 0; i < 3; i++ {
		tr.HandleUpdate(model.DecodedUpdate{Recv: simStart.Add(time.Second + time.Duration(i)*time.Second), ICAO: icao,
			Lat: model.F(50.5 + 0.002*float64(i)), Lon: model.F(9.0), MLAT: true, Src: model.SrcMLATAV,
			Grade: model.GradeB, SigmaM: model.F(40), Msgs: 1})
	}
	ac, ok := tr.Aircraft(icao)
	if !ok || ac.Lat == nil || ac.Source != model.SrcMLAT {
		t.Fatalf("no MLAT position: ok %v %+v", ok, ac)
	}
}
