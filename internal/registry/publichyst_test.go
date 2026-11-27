package registry

import (
	"context"
	"testing"

	"airview/internal/model"
)

// PRIV-A4: a station 20 m from a cell line whose surveys alternate by
// ±30 m keeps one public cell; a real move (several km) changes it.
func TestPublicPointHysteresis(t *testing.T) {
	r, _, _ := openTest(t, model.AdmissionOpen)
	ctx := context.Background()
	a := mustApproved(t, admitAVF(t, r, 1, 0, ""))
	if _, err := r.SetPrivacy(ctx, a.Idx, model.PinRegion, false, false); err != nil {
		t.Fatal(err)
	}
	const line = 51.5 // a 0.5° region cell edge
	const m = 1 / 111_320.0
	locate(t, r, a.Idx, line+20*m, -0.7)
	p0, ok := r.PublicPoint(a.Idx)
	if !ok {
		t.Fatal("no public point")
	}
	for i := range 10 {
		d := 20.0 + 30
		if i%2 == 1 {
			d = 20.0 - 30 // across the line
		}
		locate(t, r, a.Idx, line+d*m, -0.7)
		if p, _ := r.PublicPoint(a.Idx); p != p0 {
			t.Fatalf("step %d (%+.0f m): public point hopped %+v -> %+v", i, d, p0, p)
		}
	}
	locate(t, r, a.Idx, line-5000*m, -0.7)
	if p, _ := r.PublicPoint(a.Idx); p == p0 || p.Lat >= line {
		t.Fatalf("5 km move kept the old cell: %+v", p)
	}
}

func TestOutsideCellM(t *testing.T) {
	if d := outsideCellM(51.2, -0.7, 102, -2, 0.5); d != 0 {
		t.Fatalf("inside: %v", d)
	}
	if d := outsideCellM(51.5+0.001, -0.7, 102, -2, 0.5); d < 100 || d > 120 {
		t.Fatalf("111 m outside: %v", d)
	}
}

func TestOwnerActorName(t *testing.T) {
	ctx := WithActorName(context.Background(), "alice")
	if a := ownerActor(ctx, 7); a.ID != 7 || a.Name != "alice" {
		t.Fatalf("got %+v", a)
	}
	if a := ownerActor(context.Background(), 7); a.Name != "" {
		t.Fatalf("got %+v", a)
	}
}
