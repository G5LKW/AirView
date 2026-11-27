package engine

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"strings"
	"testing"
	"time"

	"airview/internal/analytics"
	"airview/internal/mlat"
	"airview/internal/mlat/clock"
	"airview/internal/mlat/geo"
	"airview/internal/mlat/ref"
	"airview/internal/mlat/survey"
	"airview/internal/model"
	"airview/internal/registry"
)

// fakeSurveyLoc is the engine view of TestReconcileSurveyLocations.
type fakeSurveyLoc struct {
	src map[model.StationIdx]mlat.PosSource
	sv  map[model.StationIdx]mlat.Survey
}

func (f *fakeSurveyLoc) PositionSource(st model.StationIdx) mlat.PosSource { return f.src[st] }
func (f *fakeSurveyLoc) SurveyOf(st model.StationIdx) (mlat.Survey, bool) {
	s, ok := f.sv[st]
	return s, ok
}

func admitAVF(t *testing.T, reg *registry.Registry, name string) registry.Station {
	t.Helper()
	seed := sha256.Sum256([]byte("surveyloc-" + name))
	res, err := reg.Admit(context.Background(), registry.AdmitRequest{Kind: model.StationKindAVF,
		FeederPub: ed25519.NewKeyFromSeed(seed[:]).Public().(ed25519.PublicKey), Name: name})
	if err != nil || res.Status != registry.StatusApproved {
		t.Fatalf("admit %s: %v %+v", name, err, res)
	}
	return res.Station
}

// TestDecideSurveyLoc: the pure decision (DESIGN §10.1, §19.3).
func TestDecideSurveyLoc(t *testing.T) {
	ecef := geo.ToECEF(50, 9, 300)
	used := mlat.Survey{Valid: true, ECEF: ecef, SigmaHM: 6, SigmaVM: 9, Stable: true}
	bare := registry.Station{Idx: 7}
	if act, g := decideSurveyLoc(&bare, mlat.PosSurvey, used, false); act != locBySurvey || g.Source != model.LocSrcEstimated ||
		abs(g.Lat-50) > 1e-9 || abs(g.HAE-300) > 1e-3 || g.SigmaM != 6 {
		t.Fatalf("unlocated + used survey: %v %+v", act, g)
	}
	// Already located by the survey at the same place: nothing to do; moved: rewrite.
	by := registry.Station{Idx: 7, Located: true, LocStatus: model.LocEstimated,
		Effective: model.StationGeo{Lat: 50, Lon: 9, HAE: 300, SigmaM: 6, Source: model.LocSrcEstimated}}
	if act, _ := decideSurveyLoc(&by, mlat.PosSurvey, used, false); act != locKeep {
		t.Fatalf("same survey rewritten: %v", act)
	}
	moved := used
	moved.ECEF = geo.ToECEF(50.001, 9, 300)
	if act, _ := decideSurveyLoc(&by, mlat.PosSurvey, moved, false); act != locBySurvey {
		t.Fatalf("moved survey not rewritten: %v", act)
	}
	// PRIV-A4: refinements within the survey's uncertainty (a check survey
	// 20 m away with σ 8 m against σ 6 m) do not move the effective
	// position, so the public pin does not hop across a cell edge.
	near := mlat.Survey{Valid: true, ECEF: geo.ToECEF(50.00018, 9, 300), SigmaHM: 8, SigmaVM: 9, Stable: true}
	if act, _ := decideSurveyLoc(&by, mlat.PosSurvey, near, false); act != locKeep {
		t.Fatalf("refinement within 3σ rewritten: %v", act)
	}
	// A much better survey (σ halved) is taken.
	better := used
	better.SigmaHM = 2
	if act, _ := decideSurveyLoc(&by, mlat.PosSurvey, better, false); act != locBySurvey {
		t.Fatalf("much better survey not taken: %v", act)
	}
	// The engine stopped using it: unlocate.
	if act, _ := decideSurveyLoc(&by, mlat.PosNone, used, false); act != locUnlocate {
		t.Fatalf("rejected survey kept: %v", act)
	}
	// Own positions are never touched.
	for _, st := range []registry.Station{
		{Idx: 3, LocStatus: model.LocDeclared},
		{Idx: 3, LocStatus: model.LocGNSS},
		{Idx: 3, LocStatus: model.LocConfirmed, Located: true, Effective: model.StationGeo{Source: model.LocSrcEstimated}},
		{Idx: 3, LocStatus: model.LocSuspect},
		{Idx: 3, Located: true, LocStatus: model.LocEstimated, Effective: model.StationGeo{Source: model.LocSrcManual}},
		{Idx: model.HostStation},
	} {
		if act, _ := decideSurveyLoc(&st, mlat.PosSurvey, used, true); act != locKeep {
			t.Errorf("own position %+v: %v", st, act)
		}
	}
	// Running survey of an unlocated station: estimating, and back.
	if act, _ := decideSurveyLoc(&bare, mlat.PosNone, mlat.Survey{}, true); act != locEstimating {
		t.Fatalf("running: %v", act)
	}
	est := registry.Station{Idx: 7, LocStatus: model.LocEstimating}
	if act, _ := decideSurveyLoc(&est, mlat.PosNone, mlat.Survey{}, true); act != locKeep {
		t.Fatalf("still running: %v", act)
	}
	if act, _ := decideSurveyLoc(&est, mlat.PosNone, mlat.Survey{}, false); act != locUnknown {
		t.Fatalf("finished without result: %v", act)
	}
}

func abs(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}

// TestReconcileSurveyLocations: the registry follows the engine's
// AutoUseSurvey verdict for an unlocated feeder (located "estimated",
// audited without coordinates) and drops it again when the engine stops
// using it; a declared station is left alone.
func TestReconcileSurveyLocations(t *testing.T) {
	reg, err := registry.OpenDir(t.TempDir(), registry.Options{Logger: quietLogger(), Policy: registry.Policy{Admission: model.AdmissionOpen}})
	if err != nil {
		t.Fatal(err)
	}
	defer reg.Close()
	ctx := context.Background()
	un := admitAVF(t, reg, "unlocated")
	decl := admitAVF(t, reg, "declared")
	if _, err := reg.SetEffective(ctx, decl.Idx, model.StationGeo{Lat: 51, Lon: 10, HAE: 200, Source: model.LocSrcManual}, true, model.LocDeclared); err != nil {
		t.Fatal(err)
	}
	ecef := geo.ToECEF(50.5, 9.5, 250)
	eng := &fakeSurveyLoc{
		src: map[model.StationIdx]mlat.PosSource{un.Idx: mlat.PosSurvey, decl.Idx: mlat.PosSurvey},
		sv: map[model.StationIdx]mlat.Survey{
			un.Idx:   {Valid: true, ECEF: ecef, SigmaHM: 4, SigmaVM: 7, Stable: true},
			decl.Idx: {Valid: true, ECEF: ecef, SigmaHM: 4, SigmaVM: 7, Stable: true},
		}}
	list := func() []registry.Station { return reg.List(registry.Filter{Admission: model.AdmApproved}) }
	if n := reconcileSurveyLocations(ctx, reg, list(), eng, nil); n != 1 {
		t.Fatalf("writes %d, want 1", n)
	}
	st, _ := reg.Station(un.Idx)
	if !st.Located || st.LocStatus != model.LocEstimated || st.Effective.Source != model.LocSrcEstimated ||
		abs(st.Effective.Lat-50.5) > 1e-9 || abs(st.Effective.HAE-250) > 1e-3 || st.Effective.SigmaM != 4 {
		t.Fatalf("unlocated feeder not located by the survey: %+v", st)
	}
	if d, _ := reg.Station(decl.Idx); d.Effective.Source != model.LocSrcManual || d.LocStatus != model.LocDeclared {
		t.Fatalf("declared station touched: %+v", d)
	}
	// A second pass changes nothing.
	if n := reconcileSurveyLocations(ctx, reg, list(), eng, nil); n != 0 {
		t.Fatalf("idempotence: %d writes", n)
	}
	// The owner rejected it: back to "not located".
	eng.src[un.Idx] = mlat.PosNone
	if n := reconcileSurveyLocations(ctx, reg, list(), eng, nil); n != 1 {
		t.Fatalf("unlocate writes %d", n)
	}
	if st, _ = reg.Station(un.Idx); st.Located || st.LocStatus != model.LocUnknown {
		t.Fatalf("not unlocated: %+v", st)
	}
	// A running survey shows "estimating".
	reconcileSurveyLocations(ctx, reg, list(), eng, func(model.StationIdx) bool { return true })
	if st, _ = reg.Station(un.Idx); st.LocStatus != model.LocEstimating {
		t.Fatalf("running survey: %+v", st)
	}
	// Audit rows: no coordinates.
	rows, err := reg.AuditLog(ctx, registry.AuditFilter{SID: un.SID, ActionPrefix: "station.location.survey"})
	if err != nil || len(rows) != 2 {
		t.Fatalf("audit %v %+v", err, rows)
	}
	for _, r := range rows {
		if strings.Contains(r.Detail, "50.5") || strings.Contains(r.Detail, "lat") {
			t.Fatalf("audit carries a position: %s", r.Detail)
		}
	}
}

// TestSurveyHints: due list, retry window, pruning and the conversion of
// Locate Me v2 results; syncSurveyStations hands the hint to the survey
// only for stations without a position and re-pushes on a new hint.
func TestSurveyHints(t *testing.T) {
	h := newSurveyHints()
	now := time.Unix(1_763_000_000, 0)
	list := []registry.Station{
		{Idx: 11}, {Idx: 12, Located: true, LocStatus: model.LocDeclared}, {Idx: model.HostStation}, {Idx: 14},
		{Idx: 15, Located: true, LocStatus: model.LocEstimated, Effective: model.StationGeo{Source: model.LocSrcEstimated}},
	}
	due := h.due(list, now, 10)
	if len(due) != 2 || due[0].Idx != 11 || due[1].Idx != 14 {
		t.Fatalf("due: %+v", due)
	}
	if d := h.due(list, now, 1); len(d) != 1 {
		t.Fatalf("batch limit: %d", len(d))
	}
	h.set(11, hintFromLocate(&analytics.Locate2Result{OK: true, Lat: 50.1, Lon: 9.2, R95Km: 12}, now))
	h.set(14, hintFromLocate(&analytics.Locate2Result{OK: true, Lat: 50.1, Lon: 9.2, R95Km: 400}, now)) // too vague
	if p, ok := h.get(11); !ok || p.Lat != 50.1 {
		t.Fatalf("hint 1: %+v %v", p, ok)
	}
	if _, ok := h.get(14); ok {
		t.Fatal("a 400 km circle is used as a hint")
	}
	if d := h.due(list, now.Add(30*time.Minute), 10); len(d) != 0 {
		t.Fatalf("retried within the hour: %+v", d)
	}
	if d := h.due(list, now.Add(2*time.Hour), 10); len(d) != 2 {
		t.Fatalf("not retried after the hour: %+v", d)
	}
	// Station 11 got located: its hint is dropped.
	list[0].Located, list[0].LocStatus = true, model.LocDeclared
	h.due(list, now, 10)
	if _, ok := h.get(11); ok {
		t.Fatal("hint of a located station kept")
	}
	if e := hintFromLocate(nil, now); e.ok || !e.tried.Equal(now) {
		t.Fatalf("nil result: %+v", e)
	}
	if e := hintFromLocate(&analytics.Locate2Result{OK: false}, now); e.ok {
		t.Fatal("failed estimate used")
	}

	// syncSurveyStations: the hint reaches the surveyor and a new hint is
	// pushed again (change detection includes it).
	surv, err := survey.New(survey.Options{Clock: nilClock{}, Refs: nilRefs{}, Synchronous: true})
	if err != nil {
		t.Skipf("survey.New without an engine: %v", err)
	}
	last := map[model.StationIdx]surveyKey{}
	sts := []registry.Station{{Idx: 9, ClockCls: model.ClockGPSUTC}}
	h.set(9, surveyHint{ok: true, llh: geo.LLH{Lat: 48, Lon: 11}, tried: now})
	syncSurveyStations(sts, surv, last, nil, h)
	if k := last[9]; !k.hasHint || k.h.Lat != 48 {
		t.Fatalf("hint not pushed: %+v", k)
	}
	h.set(9, surveyHint{ok: true, llh: geo.LLH{Lat: 48.2, Lon: 11}, tried: now})
	syncSurveyStations(sts, surv, last, nil, h)
	if k := last[9]; k.h.Lat != 48.2 {
		t.Fatalf("new hint not pushed: %+v", k)
	}
	// A declared station gets no hint.
	sts[0].Declared = registry.Position{Lat: 1, Lon: 2, Valid: true}
	syncSurveyStations(sts, surv, last, nil, h)
	if k := last[9]; k.hasHint {
		t.Fatal("declared station got a hint")
	}
}

// nilClock and nilRefs let a Surveyor be built without an MLAT engine.
type nilClock struct{}

func (nilClock) Snapshot() *clock.Snapshot { return nil }

type nilRefs struct{}

func (nilRefs) TransmitInstant(uint32, int64, []ref.Arrival, *geo.Atmosphere, *ref.State) (float64, float64, bool) {
	return 0, 0, false
}
