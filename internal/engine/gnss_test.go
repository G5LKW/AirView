package engine

import (
	"context"
	"math"
	"testing"

	"airview/internal/avf"
	"airview/internal/model"
	"airview/internal/registry"
)

func TestOnAVFGNSS(t *testing.T) {
	reg, err := registry.OpenDir(t.TempDir(), registry.Options{Logger: quietLogger()})
	if err != nil {
		t.Fatal(err)
	}
	defer reg.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	host, err := reg.EnsureHost(ctx, "host", 0)
	if err != nil {
		t.Fatal(err)
	}
	nc := &netCore{log: quietLogger(), reg: reg, ctx: ctx, cancel: cancel}
	nc.onAVFGNSS(host.Idx, avf.GNSSPos{Lat: 48.1, Lon: 11.5, AltM: 500, AltRef: model.AltRefHAE})
	nc.wg.Wait()
	st, _ := reg.Station(host.Idx)
	if !st.GNSS.Valid || st.GNSS.Lat != 48.1 || st.GNSS.Alt != 500 {
		t.Fatalf("GNSS %+v", st.GNSS)
	}
	// A fix within a few metres is not rewritten; a move is.
	nc.onAVFGNSS(host.Idx, avf.GNSSPos{Lat: 48.10001, Lon: 11.5, AltM: 501, AltRef: model.AltRefHAE})
	nc.wg.Wait()
	if st, _ := reg.Station(host.Idx); st.GNSS.Lat != 48.1 {
		t.Fatalf("small jitter rewritten: %+v", st.GNSS)
	}
	nc.onAVFGNSS(host.Idx, avf.GNSSPos{Lat: 48.2, Lon: 11.5, AltM: 500, AltRef: model.AltRefHAE})
	nc.wg.Wait()
	if st, _ := reg.Station(host.Idx); st.GNSS.Lat != 48.2 {
		t.Fatalf("move not written: %+v", st.GNSS)
	}
	// Invalid fixes are ignored.
	nc.onAVFGNSS(host.Idx, avf.GNSSPos{Lat: math.NaN(), Lon: 11.5})
	nc.onAVFGNSS(host.Idx, avf.GNSSPos{})
	nc.wg.Wait()
	if d := approxDistM(registry.Position{Lat: 48, Lon: 11}, registry.Position{Lat: 48.001, Lon: 11}); math.Abs(d-111.2) > 1 {
		t.Fatalf("distance %.1f", d)
	}
}
