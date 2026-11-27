package acars

import (
	"fmt"
	"math/rand/v2"
	"time"
)

// DemoMessage builds a synthetic message for the demo mode: plausible
// labels and texts for the given aircraft, marked with the station id
// "DEMO" so it is never mistaken for a received one.
func DemoMessage(rnd *rand.Rand, icao, reg, flight string, now time.Time) Message {
	vdl := rnd.IntN(3) > 0
	m := Message{
		T: now.UnixMilli(), Station: "DEMO", Reg: reg, Flight: flight, Mode: "2",
		BlockID: string(rune('1' + rnd.IntN(9))), MsgNo: fmt.Sprintf("M%02d%c", rnd.IntN(100), 'A'+rnd.IntN(26)),
	}
	if vdl {
		m.Source, m.Link, m.FreqMHz = SrcDumpVDL2, LinkVDL2, []float64{136.725, 136.775, 136.975}[rnd.IntN(3)]
		m.ICAO, m.ICAOFrom = icao, "message"
	} else {
		m.Source, m.Link, m.FreqMHz = SrcAcarsdec, LinkACARS, []float64{131.525, 131.725, 131.825}[rnd.IntN(3)]
	}
	lvl := -10 - float64(rnd.IntN(250))/10
	m.LevelDB = &lvl
	hhmm := now.UTC().Format("1504")
	switch rnd.IntN(6) {
	case 0:
		m.Label, m.Text = "Q0", ""
	case 1:
		m.Label, m.Text = "_d", ""
	case 2:
		m.Label, m.Text = "5Z", fmt.Sprintf("/B6 %s ETA %s FOB %03d", flight, hhmm, 50+rnd.IntN(150))
	case 3:
		m.Label, m.Text = "H1", fmt.Sprintf("#DFBA01/A%s/FL%03d/TAT%+03d/SAT%+03d", hhmm, 100+rnd.IntN(300), -rnd.IntN(30), -20-rnd.IntN(40))
	case 4:
		m.Label, m.Text = "QB", "OFF"+hhmm
	default:
		m.Label, m.Uplink, m.BlockID = "RA", true, string(rune('A'+rnd.IntN(26)))
		m.Text = "DEMO UPLINK: SYNTHETIC MESSAGE, NOT RECEIVED"
	}
	return m
}
