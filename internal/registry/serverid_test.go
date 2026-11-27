package registry

import (
	"context"
	"testing"

	"airview/internal/model"
)

func TestServerID(t *testing.T) {
	ctx := context.Background()
	r, clk, path := openTest(t, model.AdmissionOpen)
	legacy := "0123456789abcdef" // shape check only below
	if _, ok := model.CanonicalID(legacy); !ok {
		var err error
		if legacy, err = model.NewSID(r.rnd); err != nil {
			t.Fatal(err)
		}
	}
	id, err := r.ServerID(ctx, legacy)
	if err != nil || id != legacy {
		t.Fatalf("legacy adoption: %q %v (want %q)", id, err, legacy)
	}
	again, err := r.ServerID(ctx, "")
	if err != nil || again != id {
		t.Fatalf("second call %q %v", again, err)
	}
	_ = r.Close()
	r2 := openAt(t, path, clk, Policy{Admission: model.AdmissionOpen})
	defer r2.Close()
	if got, err := r2.ServerID(ctx, "something-else"); err != nil || got != id {
		t.Fatalf("after reopen %q %v", got, err)
	}

	// No legacy value: a new valid ID.
	r3, _, _ := openTest(t, model.AdmissionOpen)
	fresh, err := r3.ServerID(ctx, "not an id")
	if err != nil || !model.ValidID(fresh) {
		t.Fatalf("fresh %q %v", fresh, err)
	}
}
