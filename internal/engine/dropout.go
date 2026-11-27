package engine

import "airview/internal/model"

// dropoutSink makes the demo feed realistic for the trajectory filter: about
// a quarter of the aircraft lose their airborne position squitters for 15 to
// 45 s once every four minutes (terrain shadowing, antenna nulls, fading),
// while their other messages keep arriving. The live map then shows them
// coasting at predicted positions (model.Aircraft.Predicted).
type dropoutSink struct {
	next model.FrameSink
}

const (
	dropoutPeriodS = 240
	dropoutShare   = 4 // 1 in N aircraft
)

func (d *dropoutSink) HandleFrame(f model.RawFrame) {
	if len(f.Data) == 14 && !f.MLAT {
		df := f.Data[0] >> 3
		tc := f.Data[4] >> 3
		if (df == 17 || df == 18) && (tc >= 9 && tc <= 18 || tc >= 20 && tc <= 22) {
			icao := uint32(f.Data[1])<<16 | uint32(f.Data[2])<<8 | uint32(f.Data[3])
			h := icao * 2654435761
			if h>>28%dropoutShare == 0 {
				phase := int64(h>>8) % dropoutPeriodS
				dur := 15 + int64(h>>16)%31
				if (f.Recv.Unix()+phase)%dropoutPeriodS < dur {
					return
				}
			}
		}
	}
	d.next.HandleFrame(f)
}

func (d *dropoutSink) HandleUpdate(u model.DecodedUpdate) { d.next.HandleUpdate(u) }
