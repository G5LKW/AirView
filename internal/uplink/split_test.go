package uplink

import (
	"testing"

	"airview/internal/model"
)

func TestSplitUplinks(t *testing.T) {
	a := model.DefaultUplink()
	a.ID, a.Host = "net", "agg.example"
	b := model.DefaultUplink()
	b.ID, b.Host, b.Type, b.Format, b.ReduceMs, b.Timestamps = "rb", "readsb.example", model.UplinkBeast, "", 500, false
	notes := model.NormalizeUplinks([]model.Uplink{a, b})
	_ = notes
	list := []model.Uplink{a, b}
	model.NormalizeUplinks(list)
	avfUps, beast := SplitUplinks(list)
	if len(avfUps) != 1 || avfUps[0].ID != "net" {
		t.Fatalf("avf %+v", avfUps)
	}
	if len(beast) != 1 {
		t.Fatalf("beast %+v", beast)
	}
	g := beast[0]
	if g.ID != "rb" || g.Port != DefaultBeastPort || g.Format != BeastReducePlus || g.ReduceMs != 500 || !g.NoTimestamps {
		t.Fatalf("beast entry %+v", g)
	}
}
