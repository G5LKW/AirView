package registry

import (
	"archive/zip"
	"bytes"
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"airview/internal/model"
)

// PRIV-2: when a station leaves its owner, the next owner gets neither the
// previous name, positions, opt-ins and location history nor the previous
// owner's connection log or audit identity.
func TestOwnerChangeResetsStation(t *testing.T) {
	ctx := context.Background()
	r, clk, _ := openTest(t, model.AdmissionOpen)
	bob, alice := mkOwner(t, r, "bob"), mkOwner(t, r, "alice")
	st := mustApproved(t, admitAVF(t, r, 1, 0, ""))
	if _, err := r.AssignOwner(ctx, SystemActor, st.Idx, bob); err != nil {
		t.Fatal(err)
	}
	locate(t, r, st.Idx, 51.050091, 5.612712)
	if _, err := r.SetName(ctx, st.Idx, "Bob Smith home loft"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.SetDeclared(ctx, st.Idx, Position{Lat: 51.050091, Lon: 5.612712, Alt: 40, Valid: true}, model.AltRefMSL); err != nil {
		t.Fatal(err)
	}
	if _, err := r.SetPrivacy(ctx, st.Idx, model.PinRegion, true, true); err != nil {
		t.Fatal(err)
	}
	if _, err := r.AddEstimate(ctx, Estimate{Idx: st.Idx, Method: "survey", Lat: 51.05, Lon: 5.6127, HAE: 90, SigmaE: 12, SigmaN: 15}); err != nil {
		t.Fatal(err)
	}
	cid, err := r.ConnOpen(ctx, st.Idx, mustAddr("203.0.113.7"), "avf")
	if err != nil {
		t.Fatal(err)
	}
	_ = r.ConnClose(ctx, cid, 100)

	clk.add(time.Hour)
	if _, err := r.Unclaim(ctx, bob, st.Idx); err != nil {
		t.Fatal(err)
	}
	clk.add(time.Minute)
	got, err := r.AssignOwner(ctx, SystemActor, st.Idx, alice)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "" || got.Declared.Valid || got.GNSS.Valid || got.Estimated.Valid || got.Located {
		t.Errorf("previous owner's name/position kept: %+v", got)
	}
	if got.ShowName || got.PublicStats || got.Pin != r.policy.Load().DefaultPin || got.HasPublic {
		t.Errorf("previous owner's opt-ins kept: pin %q show %v stats %v public %v", got.Pin, got.ShowName, got.PublicStats, got.HasPublic)
	}
	if got.OwnedSince.IsZero() {
		t.Errorf("OwnedSince not set")
	}
	if es, _ := r.Estimates(ctx, st.Idx); len(es) != 0 {
		t.Errorf("location history kept: %d", len(es))
	}

	var buf bytes.Buffer
	if err := r.ExportOwner(ctx, alice, &buf); err != nil {
		t.Fatal(err)
	}
	zr, err := zip.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	if err != nil {
		t.Fatal(err)
	}
	all := ""
	for _, f := range zr.File {
		rc, _ := f.Open()
		b, _ := io.ReadAll(rc)
		rc.Close()
		all += f.Name + "\n" + string(b)
	}
	for _, leak := range []string{"Bob Smith", "51.050091", "5.612712", "station.unclaim", `"owner":`} {
		if strings.Contains(all, leak) {
			t.Errorf("alice's export contains %q", leak)
		}
	}
	if strings.Contains(all, `"IPHash"`) && !strings.Contains(all, `"connections.json"`) {
		t.Errorf("unexpected")
	}
	if c, _ := r.ConnLog(ctx, st.Idx, got.OwnedSince, 10); len(c) != 0 {
		t.Errorf("connections from before the assignment: %d", len(c))
	}
}

// A first claim of a never-owned station keeps the feeder-reported data.
func TestFirstClaimKeepsFeederData(t *testing.T) {
	ctx := context.Background()
	r, _, _ := openTest(t, model.AdmissionOpen)
	alice := mkOwner(t, r, "alice")
	st := mustApproved(t, admitAVF(t, r, 1, 0, ""))
	locate(t, r, st.Idx, 48.1, 11.5)
	got, err := r.AssignOwner(ctx, SystemActor, st.Idx, alice)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Located || !got.OwnedSince.IsZero() {
		t.Errorf("first claim reset the station: located %v since %v", got.Located, got.OwnedSince)
	}
}
