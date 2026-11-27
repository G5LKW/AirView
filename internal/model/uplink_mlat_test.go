package model

import "testing"

func TestUplinkMLATNormalize(t *testing.T) {
	u := DefaultUplink()
	u.ID, u.Host, u.Type, u.Port, u.User, u.Format = "m1", "feed.example.org", "MLAT", DefaultAVFPort, "My Station <x>", "beast_out"
	notes := u.Normalize("uplinks[0]")
	if u.Type != UplinkMLAT || u.Port != DefaultMLATOutPort || u.User != "My Station x" || u.Format != "" || !u.IsMLAT() || u.IsAVF() || u.IsBeast() {
		t.Fatalf("%+v %v", u, notes)
	}
	if len(notes) != 2 {
		t.Fatalf("notes %v", notes)
	}
	a := DefaultUplink()
	a.ID, a.Host, a.User = "a", "agg.example.org", "x"
	a.Normalize("uplinks[1]")
	if a.User != "" || !a.IsAVF() {
		t.Fatalf("user kept on an AVF uplink: %+v", a)
	}
}
