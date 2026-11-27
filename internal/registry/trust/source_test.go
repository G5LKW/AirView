package trust

import (
	"context"
	"math"
	"path/filepath"
	"testing"

	"airview/internal/geoid"
	"airview/internal/model"
	"airview/internal/registry"
)

// TestRegistrySourceDeclared: a station with only a declared position (an
// mlat-client handshake) is located for the gate, so it can be judged and
// proven (found by the integrator's end-to-end run: mlat-clients stayed in
// quarantine for good).
func TestRegistrySourceDeclared(t *testing.T) {
	r, err := registry.Open(filepath.Join(t.TempDir(), registry.FileName), registry.Options{Policy: registry.Policy{Admission: model.AdmissionOpen}})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	ctx := context.Background()
	res, err := r.Admit(ctx, registry.AdmitRequest{Kind: model.StationKindMLATC, UUID: "11111111-2222-3333-4444-555555555555"})
	if err != nil {
		t.Fatal(err)
	}
	st := res.Station
	if _, _, err := r.FeederPosition(ctx, st.Idx, registry.FeederPos{Lat: 50, Lon: 9, Alt: 120, AltRef: model.AltRefMSL}); err != nil {
		t.Fatal(err)
	}
	in, ok := RegistrySource{R: r}.TrustInfo(st.Idx)
	n, _ := geoid.Undulation(50, 9)
	if !ok || !in.Located || in.Lat != 50 || in.Lon != 9 || math.Abs(in.HAE-(120+n)) > 0.01 {
		t.Fatalf("declared-only station: %+v (N %.1f)", in, n)
	}
}
