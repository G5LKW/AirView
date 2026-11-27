package analytics

import "airview/internal/tracker"

// TrackerHook returns the function to install with
// tracker.SetGuessSampleHook: it records the delivered samples.
func (r *GuessRecorder) TrackerHook() func([]tracker.GuessSample) {
	var in []GuessInput // only used under the tracker's guess lock
	return func(s []tracker.GuessSample) {
		in = in[:0]
		for i := range s {
			g := &s[i]
			in = append(in, GuessInput{T: g.T, Station: g.Station, ICAO: g.ICAO, Lat: g.Lat, Lon: g.Lon,
				AltFt: g.AltFt, RSSI: g.RSSI, Channel: g.Channel, OnGround: g.OnGround})
		}
		r.Record(in)
	}
}
