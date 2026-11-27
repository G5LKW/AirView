package plan

import (
	"airview/internal/ingest"
	"airview/internal/model"
)

// IngestOut forwards deltas to the ingest as AVF INTEREST updates
// (ingest.SetInterest keeps the sets and sends them while the station is in
// selective mode, DESIGN §4.4).
type IngestOut struct {
	Ingest interface {
		SetInterest(st model.StationIdx, in ingest.InterestCmd) error
	}
}

// Interest implements Out. The slices are copied: the ingest may queue them.
func (o IngestOut) Interest(st model.StationIdx, d *Delta) {
	cp := func(s []uint32) []uint32 {
		if len(s) == 0 {
			return nil
		}
		return append([]uint32(nil), s...)
	}
	_ = o.Ingest.SetInterest(st, ingest.InterestCmd{Reset: d.Reset, SyncAdd: cp(d.SyncAdd), SyncDel: cp(d.SyncDel),
		MLATAdd: cp(d.MLATAdd), MLATDel: cp(d.MLATDel)})
}

// SendingController is implemented by an mlat-client session server
// (internal/ingest/mlatc, DESIGN §5.2): it tells a station's client which
// aircraft to send (start_sending / stop_sending).
type SendingController interface {
	StartSending(st model.StationIdx, keys []uint32)
	StopSending(st model.StationIdx, keys []uint32)
}

// MLATCOut forwards the union of a station's sets as start/stop_sending.
// A reset first stops nothing (the client starts with an empty set) and
// then starts the whole union. Keys are copied.
type MLATCOut struct{ C SendingController }

// Interest implements Out.
func (o MLATCOut) Interest(st model.StationIdx, d *Delta) {
	if len(d.SendDel) > 0 {
		o.C.StopSending(st, append([]uint32(nil), d.SendDel...))
	}
	if len(d.SendAdd) > 0 {
		o.C.StartSending(st, append([]uint32(nil), d.SendAdd...))
	}
}

// Fanout sends every delta to several outputs.
type Fanout []Out

// Interest implements Out.
func (f Fanout) Interest(st model.StationIdx, d *Delta) {
	for _, o := range f {
		o.Interest(st, d)
	}
}
