package registry

import (
	"context"
	"testing"

	"airview/internal/model"
)

// FlagNoAnchor (finding ROB-4) is set and cleared by SetNoAnchor and
// survives a reopen without touching the other flags.
func TestSetNoAnchorPersists(t *testing.T) {
	r, clk, path := openTest(t, model.AdmissionOpen)
	st := mustApproved(t, admitAVF(t, r, 1, 0, ""))
	ctx := context.Background()
	if _, err := r.Update(ctx, st.Idx, func(s *Station) error { s.Flags |= FlagRelaySub; return nil }); err != nil {
		t.Fatal(err)
	}
	got, err := r.SetNoAnchor(ctx, st.Idx, true)
	if err != nil || !got.Has(FlagNoAnchor) || !got.Has(FlagRelaySub) {
		t.Fatalf("set: %+v %v", got.Flags, err)
	}
	_ = r.Close()
	r2 := openAt(t, path, clk, Policy{Admission: model.AdmissionOpen})
	defer r2.Close()
	s2, ok := r2.Station(st.Idx)
	if !ok || !s2.Has(FlagNoAnchor) || !s2.Has(FlagRelaySub) {
		t.Fatalf("after reopen: %+v", s2.Flags)
	}
	got, _ = r2.SetNoAnchor(ctx, st.Idx, false)
	if got.Has(FlagNoAnchor) || !got.Has(FlagRelaySub) {
		t.Fatalf("clear: %+v", got.Flags)
	}
}
