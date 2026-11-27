package server

import (
	"testing"

	"airview/internal/model"
	"airview/internal/refdata"
	"airview/internal/store"
)

// TestRefSources checks the registration / type source labels of buildInfo.
func TestRefSources(t *testing.T) {
	const us = 0xA00001 // US block: the registration is derivable (N1)
	derived := refdata.Registration(us)
	if derived == "" {
		t.Fatal("test address has no derived registration")
	}
	cases := []struct {
		name         string
		key          uint32
		live         *model.Aircraft
		row          *store.AircraftRow
		db           *store.ACDBEntry
		reg, regSrc  string
		typ, typeSrc string
	}{
		{name: "address only", key: us, reg: derived, regSrc: "derived"},
		{name: "database wins", key: us, db: &store.ACDBEntry{Reg: "N999XX", Type: "B738"}, reg: "N999XX", regSrc: "acdb", typ: "B738", typeSrc: "acdb"},
		{name: "database equal to derived", key: us, db: &store.ACDBEntry{Reg: derived}, reg: derived, regSrc: "acdb"},
		{name: "history row", key: us, row: &store.AircraftRow{Reg: "N123AB", Type: "C172"}, reg: "N123AB", regSrc: "history", typ: "C172", typeSrc: "history"},
		{name: "live from database (no db entry passed)", key: us, live: &model.Aircraft{Registration: "N555ZZ", TypeCode: "A320"}, reg: "N555ZZ", regSrc: "acdb", typ: "A320", typeSrc: "acdb"},
		{name: "live derived", key: us, live: &model.Aircraft{Registration: derived}, reg: derived, regSrc: "derived"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			in := buildInfo(c.key, c.live, c.row, c.db)
			if in.Registration != c.reg || in.RegSource != c.regSrc {
				t.Errorf("registration %q (%q), want %q (%q)", in.Registration, in.RegSource, c.reg, c.regSrc)
			}
			if in.TypeCode != c.typ || in.TypeSource != c.typeSrc {
				t.Errorf("type %q (%q), want %q (%q)", in.TypeCode, in.TypeSource, c.typ, c.typeSrc)
			}
			if in.fromACDB() != (c.regSrc == "acdb" || c.typeSrc == "acdb") {
				t.Errorf("fromACDB %v", in.fromACDB())
			}
		})
	}
	// A registration flown as the callsign.
	in := buildInfo(0x400001, &model.Aircraft{Callsign: "GABCD", Registration: "G-ABCD"}, nil, nil)
	if in.Registration != "G-ABCD" || in.RegSource != "callsign" {
		t.Errorf("callsign registration %q labelled %q", in.Registration, in.RegSource)
	}
}
