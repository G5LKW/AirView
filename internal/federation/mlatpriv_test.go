package federation

import (
	"testing"

	"airview/internal/model"
	"airview/internal/privacy/publicdto"
)

// PRIV-A2: own MLAT fixes get the island guard and NoGround / NoMilitary
// exactly as STATE records; PRIV-A6: synthetic Mode A/C identities are
// never shared.
func TestOwnFixPrivacy(t *testing.T) {
	yes := true
	n := &Node{opts: Options{Guard: func(*model.Aircraft) bool { return false }}}
	n.prep = []local{
		{key: 0xAAAAAA, island: true},
		{key: 0xBBBBBB, a: publicdto.Aircraft{OnGround: &yes}},
		{key: 0xCCCCCC, a: publicdto.Aircraft{Military: true}},
		{key: 0xDDDDDD},
	}
	fixes := []Fix{
		{ICAO: 0xAAAAAA, Lat: 50.123456, Lon: 8.654321, SigmaHM: 30, Vel: &[3]float32{1, 2, 3}},
		{ICAO: 0xBBBBBB, Lat: 50, Lon: 8},
		{ICAO: 0xCCCCCC, Lat: 50, Lon: 8},
		{ICAO: 0xDDDDDD, Lat: 50, Lon: 8},
		{ICAO: 0xEEEEEE, Lat: 50, Lon: 8}, // not in the local picture
	}
	meta := n.fixMeta(fixes)
	run := func(p Policy) map[uint32]Fix {
		out := map[uint32]Fix{}
		for _, f := range fixes {
			if p.ownFix(&f, meta) {
				out[f.ICAO] = f
			}
		}
		return out
	}
	got := run(Policy{Island: IslandDrop})
	if _, ok := got[0xAAAAAA]; ok {
		t.Fatal("island aircraft shared with island=drop")
	}
	if _, ok := got[0xEEEEEE]; ok {
		t.Fatal("unknown aircraft shared while a guard is configured")
	}
	if _, ok := got[0xDDDDDD]; !ok {
		t.Fatal("ordinary aircraft dropped")
	}
	got = run(Policy{Island: IslandCoarsen, NoGround: true, NoMilitary: true})
	f, ok := got[0xAAAAAA]
	if !ok || f.Lat != 50.1 || f.Lon != 8.65 || f.Vel != nil || f.SigmaHM < 1000 {
		t.Fatalf("island fix not coarsened: %+v ok=%v", f, ok)
	}
	if _, ok := got[0xBBBBBB]; ok {
		t.Fatal("ground aircraft shared with noGround")
	}
	if _, ok := got[0xCCCCCC]; ok {
		t.Fatal("military aircraft shared with noMilitary")
	}
	if _, ok := FixFromModel(&model.MLATFix{ICAO: 1<<25 | 7, Grade: model.GradeA}); ok {
		t.Fatal("Mode A/C synthetic identity converted for sharing")
	}
}
