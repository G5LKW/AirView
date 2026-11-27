package server

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/csv"
	"encoding/xml"
	"fmt"
	"io"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"airview/internal/geo"
	"airview/internal/model"
	"airview/internal/modes"
	"airview/internal/navdata"
	"airview/internal/store"
)

// History replay, flight traces, exports (KML / GeoJSON / CSV) and the
// VRS-style report query.

// ---------------------------------------------------------------------------
// Replay
// ---------------------------------------------------------------------------

// Replay limits.
const (
	replayMaxSpan   = 24 * time.Hour
	replayMaxFrames = 2000
	replayMaxPoints = 400_000
)

// replayAircraft identifies one flight of a replay (index = position in
// replayReply.Aircraft).
type replayAircraft struct {
	ICAO     string `json:"icao"`
	FlightID int64  `json:"flightId"`
	Callsign string `json:"callsign,omitempty"`
	Reg      string `json:"reg,omitempty"`
	Type     string `json:"type,omitempty"`
	Operator string `json:"operator,omitempty"`
	Category string `json:"category,omitempty"` // ADS-B emitter category ("A3", "B1", ...)
	Military bool   `json:"military,omitempty"`
}

// replayFrame is the picture at one instant: rows
// [aircraftIndex, lat, lon, altFt|null, gsKt|null, trackDeg|null, src, ageS].
type replayFrame struct {
	T int64        `json:"t"` // Unix ms
	P replayPoints `json:"p"`
}

type replayPoint struct {
	idx            int32
	lat, lon       float64
	alt, gs, track float64
	src            uint8
	age            int32
}

type replayPoints []replayPoint

func (ps replayPoints) MarshalJSON() ([]byte, error) {
	b := make([]byte, 0, 2+len(ps)*48)
	b = append(b, '[')
	for i, p := range ps {
		if i > 0 {
			b = append(b, ',')
		}
		b = append(b, '[')
		b = strconv.AppendInt(b, int64(p.idx), 10)
		b = append(b, ',')
		b = appendOpt(b, p.lat, 5)
		b = append(b, ',')
		b = appendOpt(b, p.lon, 5)
		b = append(b, ',')
		b = appendOpt(b, p.alt, 0)
		b = append(b, ',')
		b = appendOpt(b, p.gs, 0)
		b = append(b, ',')
		b = appendOpt(b, p.track, 0)
		b = append(b, ',')
		b = strconv.AppendInt(b, int64(p.src), 10)
		b = append(b, ',')
		b = strconv.AppendInt(b, int64(p.age), 10)
		b = append(b, ']')
	}
	return append(b, ']'), nil
}

// replayReply is GET /api/replay.
type replayReply struct {
	From      int64            `json:"from"`      // Unix ms of the first frame
	To        int64            `json:"to"`        // Unix ms (end of the window)
	Step      int              `json:"step"`      // seconds between frames
	StaleSec  int              `json:"staleSec"`  // a stored point is shown for at most this long after its time
	Aircraft  []replayAircraft `json:"aircraft"`  // indexed by the first element of every frame row
	Frames    []replayFrame    `json:"frames"`    // oldest first, one every step seconds
	Points    int              `json:"points"`    // stored positions read
	Truncated bool             `json:"truncated"` // the point budget (points the frames use) was exhausted: later frames are incomplete
}

func apiReplay(s *Server, r *http.Request) (any, error) {
	from, to, err := qSpanDefault(r, time.Hour)
	if err != nil {
		return nil, err
	}
	if to.Sub(from) > replayMaxSpan {
		return nil, badRequest("the replay window is limited to %d hours", int(replayMaxSpan.Hours()))
	}
	span := to.Sub(from)
	defStep := int(math.Max(5, math.Ceil(span.Seconds()/600)))
	step, err := qInt(r, "step", defStep, 1, 3600)
	if err != nil {
		return nil, err
	}
	if n := int(span.Seconds())/step + 1; n > replayMaxFrames {
		return nil, badRequest("too many frames (%d): use a larger step (at least %d s) or a shorter window", n, int(math.Ceil(span.Seconds()/replayMaxFrames)))
	}
	cfg := s.eng.EffectiveConfig()
	stale := max(step*2, 3*cfg.Storage.TrailIntervalSec, 30)
	// Aggregator trails of flights that ended more than trailFullDays ago
	// keep one point per trailThinSec: widen the window so an aircraft does
	// not blink between two kept points.
	if st := cfg.Aggregator.Storage; cfg.Mode == model.ModeAggregator && st.TrailFullDays > 0 && st.TrailThinSec > 1 &&
		from.Before(time.Now().Add(-time.Duration(st.TrailFullDays)*24*time.Hour)) {
		stale = max(stale, st.TrailThinSec+cfg.Storage.TrailIntervalSec+5)
	}
	f0, f1 := from.Unix(), to.Unix()
	nFrames := int((f1-f0)/int64(step)) + 1
	out := replayReply{From: f0 * 1000, To: f1 * 1000, Step: step, StaleSec: stale, Aircraft: []replayAircraft{}}
	out.Frames = make([]replayFrame, nFrames)
	for i := range out.Frames {
		out.Frames[i] = replayFrame{T: (f0 + int64(i*step)) * 1000, P: replayPoints{}}
	}

	// Positions of all flights overlapping the window, grouped by flight
	// in time order (flights("end") index + trail chunks). The store hands
	// over only the points a frame uses (plus each flight's first point), so
	// the budget counts points shown, not points read; the frames equal the
	// frames built from every stored point.
	idx := map[int64]int32{}
	var cur []store.Position
	curID := int64(-1)
	var curICAO uint32
	flush := func() {
		if len(cur) == 0 {
			return
		}
		i, ok := idx[curID]
		if !ok {
			i = int32(len(out.Aircraft))
			idx[curID] = i
			out.Aircraft = append(out.Aircraft, replayAircraft{ICAO: modes.FormatICAO(curICAO), FlightID: curID})
		}
		// Frame k shows the newest point with t <= frame time, if it is
		// at most stale seconds old.
		j := 0
		k0 := int((cur[0].T - f0 + int64(step) - 1) / int64(step))
		if cur[0].T <= f0 {
			k0 = 0
		}
		for k := max(k0, 0); k < nFrames; k++ {
			ft := f0 + int64(k*step)
			for j+1 < len(cur) && cur[j+1].T <= ft {
				j++
			}
			p := cur[j]
			if p.T > ft {
				continue
			}
			age := ft - p.T
			if age > int64(stale) {
				if j == len(cur)-1 {
					break
				}
				continue
			}
			out.Frames[k].P = append(out.Frames[k].P, replayPoint{idx: i, lat: p.Lat, lon: p.Lon, alt: p.Alt, gs: p.GS,
				track: p.Track, src: p.Src, age: int32(age)})
		}
		cur = cur[:0]
	}
	used := 0
	stats, err := s.eng.Store().ForEachPositionStep(r.Context(), f0, f1, int64(step), int64(stale), func(icao uint32, p store.Position) error {
		if p.FlightID != curID {
			flush()
			curID, curICAO = p.FlightID, icao
		}
		cur = append(cur, p)
		used++
		if used >= replayMaxPoints {
			out.Truncated = true
			return errStopReplay
		}
		return nil
	})
	out.Points = stats.Read
	if err != nil && err != errStopReplay {
		return nil, err
	}
	flush()
	if err := s.fillReplayAircraft(r.Context(), out.Aircraft); err != nil {
		return nil, err
	}
	return out, nil
}

var errStopReplay = fmt.Errorf("replay budget exhausted")

// fillReplayAircraft adds callsign and reference data to the replay table.
func (s *Server) fillReplayAircraft(ctx context.Context, acs []replayAircraft) error {
	if len(acs) == 0 {
		return nil
	}
	ids := make([]string, 0, len(acs))
	pos := make(map[int64]int, len(acs))
	for i, a := range acs {
		ids = append(ids, strconv.FormatInt(a.FlightID, 10))
		pos[a.FlightID] = i
	}
	for start := 0; start < len(ids); start += 500 {
		end := min(start+500, len(ids))
		// ids are integers formatted above: safe to inline.
		q := `SELECT f.id, f.callsign,
  CASE WHEN a.reg != '' THEN a.reg ELSE COALESCE(d.reg, '') END,
  CASE WHEN a.type != '' THEN a.type ELSE COALESCE(d.type, '') END,
  CASE WHEN a.operator != '' THEN a.operator ELSE COALESCE(d.operator, '') END,
  (COALESCE(a.flags, 0) | COALESCE(d.flags, 0)) & 1,
  COALESCE(a.category, '')
FROM flights f LEFT JOIN aircraft a ON a.icao = f.icao LEFT JOIN acdb d ON d.icao = f.icao
WHERE f.id IN (` + strings.Join(ids[start:end], ",") + `)`
		rows, err := s.eng.Store().DB().QueryContext(ctx, q)
		if err != nil {
			return err
		}
		for rows.Next() {
			var id, mil int64
			var cs, reg, typ, op, cat string
			if err := rows.Scan(&id, &cs, &reg, &typ, &op, &mil, &cat); err != nil {
				rows.Close()
				return err
			}
			if i, ok := pos[id]; ok {
				a := &acs[i]
				a.Callsign, a.Reg, a.Type, a.Operator, a.Military, a.Category = cs, reg, typ, op, mil != 0, cat
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Flight trace and airport association
// ---------------------------------------------------------------------------

// tracePoints encodes stored positions as compact arrays
// [tMs, lat, lon, altFt|null, gsKt|null, trackDeg|null, rssi|null, src].
type tracePoints []store.Position

func (t tracePoints) MarshalJSON() ([]byte, error) {
	b := make([]byte, 0, 2+len(t)*64)
	b = append(b, '[')
	for i, p := range t {
		if i > 0 {
			b = append(b, ',')
		}
		b = append(b, '[')
		b = strconv.AppendInt(b, p.T*1000, 10)
		b = append(b, ',')
		b = appendOpt(b, p.Lat, 5)
		b = append(b, ',')
		b = appendOpt(b, p.Lon, 5)
		b = append(b, ',')
		b = appendOpt(b, p.Alt, 0)
		b = append(b, ',')
		b = appendOpt(b, p.GS, 0)
		b = append(b, ',')
		b = appendOpt(b, p.Track, 0)
		b = append(b, ',')
		b = appendOpt(b, p.RSSI, 1)
		b = append(b, ',')
		b = strconv.AppendInt(b, int64(p.Src), 10)
		b = append(b, ']')
	}
	return append(b, ']'), nil
}

// flightAirports are the departure / arrival airports guessed from the
// first / last low positions of a flight (needs imported airport data).
type flightAirports struct {
	Departure *navdata.AirportGuess `json:"departure"` // null = not near an airport when first seen
	Arrival   *navdata.AirportGuess `json:"arrival"`   // null = not near an airport when last seen
}

func (s *Server) flightAirports(ps []store.Position) flightAirports {
	var out flightAirports
	reg := s.eng.Region()
	if reg == nil || reg.Len() == 0 || len(ps) == 0 {
		return out
	}
	tp := make([]navdata.TrackPoint, len(ps))
	for i, p := range ps {
		tp[i] = navdata.TrackPoint{T: p.T * 1000, Lat: p.Lat, Lon: p.Lon, AltFt: p.Alt, GS: p.GS, Track: p.Track}
	}
	if g, ok := navdata.GuessDeparture(tp, reg); ok {
		out.Departure = &g
	}
	if g, ok := navdata.GuessArrival(tp, reg); ok {
		out.Arrival = &g
	}
	return out
}

// traceReply is GET /api/history/flights/{id}/trace.
type traceReply struct {
	Flight   store.Flight   `json:"flight"`
	Points   tracePoints    `json:"points"` // every stored position: [tMs, lat, lon, altFt|null, gsKt|null, trackDeg|null, rssi|null, src]
	Airports flightAirports `json:"airports"`
}

func (s *Server) loadFlight(ctx context.Context, idStr string) (store.Flight, []store.Position, error) {
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil || id <= 0 {
		return store.Flight{}, nil, badRequest("invalid flight id")
	}
	st := s.eng.Store()
	f, ok, err := st.FlightByID(ctx, id)
	if err != nil {
		return f, nil, err
	}
	if !ok {
		return f, nil, notFound("flight %d not found", id)
	}
	ps, err := st.Positions(ctx, id, 0, 0)
	if err != nil {
		return f, nil, err
	}
	return f, ps, nil
}

func apiFlightTrace(s *Server, r *http.Request) (any, error) {
	f, ps, err := s.loadFlight(r.Context(), r.PathValue("id"))
	if err != nil {
		return nil, err
	}
	if ps == nil {
		ps = []store.Position{}
	}
	return traceReply{Flight: f, Points: ps, Airports: s.flightAirports(ps)}, nil
}

// ---------------------------------------------------------------------------
// Exports
// ---------------------------------------------------------------------------

const (
	ctKML     = "application/vnd.google-earth.kml+xml"
	ctGeoJSON = "application/geo+json"
	ctCSV     = "text/csv; charset=utf-8"
)

// exportBegin takes an export slot and the reset lock; it writes the error
// itself and returns false when the export cannot run.
// The returned request carries a context that a factory reset cancels (see
// beginStream); use it for the export's queries.
func (s *Server) exportBegin(w http.ResponseWriter, r *http.Request) (_ *http.Request, done func(), ok bool) {
	select {
	case s.exportSem <- struct{}{}:
	default:
		w.Header().Set("Retry-After", "10")
		writeError(w, http.StatusTooManyRequests, "too many exports are running, try again shortly")
		return r, nil, false
	}
	s.resetMu.RLock()
	if !s.eng.Running() {
		s.resetMu.RUnlock()
		<-s.exportSem
		writeError(w, http.StatusServiceUnavailable, "engine is not running")
		return r, nil, false
	}
	r, end := s.beginStream(w, r)
	return r, func() { end(); s.resetMu.RUnlock(); <-s.exportSem }, true
}

// sendFile writes a complete in-memory export.
func sendFile(w http.ResponseWriter, r *http.Request, body []byte, ctype, name string) {
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	writeBody(w, r, http.StatusOK, body, nil, ctype)
}

func iso(sec int64) string { return time.Unix(sec, 0).UTC().Format(time.RFC3339) }

func ftToM(ft float64) float64 { return ft * geo.MetresPerFoot }

func xmlEsc(s string) string {
	var b bytes.Buffer
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}

func flightLabel(f store.Flight) string {
	l := modes.FormatICAO(f.ICAO)
	if f.Callsign != "" {
		l = f.Callsign + " (" + l + ")"
	}
	return l
}

// rawExportFlight is GET /api/export/flight/{file} with {file} = <id>.kml | <id>.geojson | <id>.csv.
func rawExportFlight(s *Server, w http.ResponseWriter, r *http.Request) {
	file := r.PathValue("file")
	dot := strings.LastIndexByte(file, '.')
	if dot <= 0 {
		writeError(w, http.StatusNotFound, "use /api/export/flight/<id>.kml | .geojson | .csv")
		return
	}
	ext := file[dot+1:]
	if ext != "kml" && ext != "geojson" && ext != "csv" {
		writeError(w, http.StatusNotFound, "unknown format: kml, geojson or csv")
		return
	}
	r, done, ok := s.exportBegin(w, r)
	if !ok {
		return
	}
	defer done()
	ctx, cancel := context.WithTimeout(r.Context(), apiTimeout)
	defer cancel()
	f, ps, err := s.loadFlight(ctx, file[:dot])
	if err != nil {
		s.writeAPIError(w, r, err)
		return
	}
	name := fmt.Sprintf("airview-flight-%d.%s", f.ID, ext)
	switch ext {
	case "kml":
		var b bytes.Buffer
		b.WriteString(xml.Header)
		b.WriteString(`<kml xmlns="http://www.opengis.net/kml/2.2"><Document>`)
		fmt.Fprintf(&b, "<name>%s</name>", xmlEsc(flightLabel(f)))
		b.WriteString(`<Style id="trk"><LineStyle><color>ffffb000</color><width>3</width></LineStyle></Style>`)
		writeKMLFlight(&b, f, ps)
		b.WriteString(`</Document></kml>`)
		sendFile(w, r, b.Bytes(), ctKML, name)
	case "geojson":
		fc := geoFC{Type: "FeatureCollection", Features: []geoFeature{flightFeature(f, ps)}}
		body, err := marshal(fc)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		sendFile(w, r, body, ctGeoJSON, name)
	case "csv":
		var b bytes.Buffer
		cw := csv.NewWriter(&b)
		_ = cw.Write([]string{"time", "t_ms", "icao", "callsign", "lat", "lon", "alt_ft", "gs_kt", "track_deg", "rssi_dbfs", "src"})
		hex := modes.FormatICAO(f.ICAO)
		for _, p := range ps {
			_ = cw.Write([]string{iso(p.T), strconv.FormatInt(p.T*1000, 10), hex, f.Callsign, fmtF(p.Lat, 5), fmtF(p.Lon, 5),
				fmtF(p.Alt, 0), fmtF(p.GS, 0), fmtF(p.Track, 0), fmtF(p.RSSI, 1), srcName(p.Src)})
		}
		cw.Flush()
		sendFile(w, r, b.Bytes(), ctCSV, name)
	}
}

func fmtF(v float64, d int) string {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return ""
	}
	return strconv.FormatFloat(v, 'f', d, 64)
}

func srcName(src uint8) string {
	switch src {
	case store.SrcADSB:
		return "adsb"
	case store.SrcMLAT:
		return "mlat"
	case store.SrcTISB:
		return "tisb"
	}
	return "other"
}

// writeKMLFlight writes one flight as a Placemark with an absolute-altitude LineString.
func writeKMLFlight(b *bytes.Buffer, f store.Flight, ps []store.Position) {
	fmt.Fprintf(b, `<Placemark><name>%s</name><description>%s</description><styleUrl>#trk</styleUrl>`,
		xmlEsc(flightLabel(f)), xmlEsc(fmt.Sprintf("Flight %d, %s to %s, %d positions", f.ID, iso(f.Start), iso(f.End), len(ps))))
	if len(ps) > 0 {
		fmt.Fprintf(b, `<TimeSpan><begin>%s</begin><end>%s</end></TimeSpan>`, iso(ps[0].T), iso(ps[len(ps)-1].T))
	}
	b.WriteString(`<LineString><tessellate>1</tessellate><altitudeMode>absolute</altitudeMode><coordinates>`)
	for _, p := range ps {
		alt := 0.0
		if !math.IsNaN(p.Alt) {
			alt = ftToM(p.Alt)
		}
		fmt.Fprintf(b, "%.5f,%.5f,%.0f ", p.Lon, p.Lat, alt)
	}
	b.WriteString(`</coordinates></LineString></Placemark>`)
}

type geoFC struct {
	Type     string       `json:"type"`
	Features []geoFeature `json:"features"`
}

type geoFeature struct {
	Type       string         `json:"type"`
	Geometry   geoGeometry    `json:"geometry"`
	Properties map[string]any `json:"properties"`
}

type geoGeometry struct {
	Type        string `json:"type"`
	Coordinates any    `json:"coordinates"`
}

// flightFeature is a LineString [lon, lat, altM] with the flight as properties
// and the point times in properties.times (Unix ms).
func flightFeature(f store.Flight, ps []store.Position) geoFeature {
	coords := make([][3]num, 0, len(ps))
	times := make([]int64, 0, len(ps))
	for _, p := range ps {
		coords = append(coords, [3]num{rnd(p.Lon, 5), rnd(p.Lat, 5), rnd(ftToM(p.Alt), 0)})
		times = append(times, p.T*1000)
	}
	props := map[string]any{
		"flightId": f.ID, "icao": modes.FormatICAO(f.ICAO), "callsign": f.Callsign, "reg": f.Reg, "type": f.Type,
		"operator": f.Operator, "start": f.Start * 1000, "end": f.End * 1000, "positions": len(ps), "times": times,
	}
	if len(coords) == 1 {
		return geoFeature{Type: "Feature", Geometry: geoGeometry{Type: "Point", Coordinates: coords[0]}, Properties: props}
	}
	return geoFeature{Type: "Feature", Geometry: geoGeometry{Type: "LineString", Coordinates: coords}, Properties: props}
}

// liveExport collects the live picture with real positions (predicted
// aircraft are exported at their last real position) and trails.
func (s *Server) liveExport(r *http.Request) (model.LiveSnapshot, map[string][]model.TrailPoint, error) {
	minutes, err := qInt(r, "minutes", s.eng.EffectiveConfig().UI.TrailMinutes, 0, 240)
	if err != nil {
		return model.LiveSnapshot{}, nil, err
	}
	snap := s.eng.Tracker().Snapshot()
	var trails map[string][]model.TrailPoint
	if minutes > 0 {
		trails = s.eng.Tracker().Trails(minutes)
	}
	return snap, trails, nil
}

func liveLatLon(a *model.Aircraft) (lat, lon float64, ok bool) {
	if a.Predicted && a.LastRealLat != nil && a.LastRealLon != nil {
		return *a.LastRealLat, *a.LastRealLon, true
	}
	if a.Lat != nil && a.Lon != nil {
		return *a.Lat, *a.Lon, true
	}
	return 0, 0, false
}

func liveAltFt(a *model.Aircraft) float64 {
	switch {
	case a.AltBaro != nil:
		return *a.AltBaro
	case a.AltGeom != nil:
		return *a.AltGeom
	}
	return math.NaN()
}

func liveLabel(a *model.Aircraft) string {
	if a.Callsign != "" {
		return a.Callsign + " (" + a.ICAO + ")"
	}
	return a.ICAO
}

// rawExportLive is GET /api/export/live.kml and /api/export/live.geojson.
func rawExportLive(s *Server, w http.ResponseWriter, r *http.Request) {
	kml := strings.HasSuffix(r.URL.Path, ".kml")
	r, done, ok := s.exportBegin(w, r)
	if !ok {
		return
	}
	defer done()
	snap, trails, err := s.liveExport(r)
	if err != nil {
		s.writeAPIError(w, r, err)
		return
	}
	stamp := time.UnixMilli(snap.Now).UTC().Format("20060102-150405")
	if kml {
		var b bytes.Buffer
		b.WriteString(xml.Header)
		b.WriteString(`<kml xmlns="http://www.opengis.net/kml/2.2"><Document>`)
		fmt.Fprintf(&b, "<name>AirView live %s</name>", stamp)
		b.WriteString(`<Style id="ac"><IconStyle><scale>0.8</scale></IconStyle></Style><Style id="trk"><LineStyle><color>ccffb000</color><width>2</width></LineStyle></Style>`)
		for i := range snap.Aircraft {
			a := &snap.Aircraft[i]
			lat, lon, ok := liveLatLon(a)
			if !ok {
				continue
			}
			alt := liveAltFt(a)
			altM := 0.0
			if !math.IsNaN(alt) {
				altM = ftToM(alt)
			}
			fmt.Fprintf(&b, `<Folder><name>%s</name><Placemark><name>%s</name><description>%s</description><styleUrl>#ac</styleUrl><Point><altitudeMode>absolute</altitudeMode><coordinates>%.5f,%.5f,%.0f</coordinates></Point></Placemark>`,
				xmlEsc(liveLabel(a)), xmlEsc(liveLabel(a)), xmlEsc(liveDesc(a)), lon, lat, altM)
			if tr := trails[a.ICAO]; len(tr) > 1 {
				b.WriteString(`<Placemark><name>trail</name><styleUrl>#trk</styleUrl><LineString><tessellate>1</tessellate><altitudeMode>absolute</altitudeMode><coordinates>`)
				for _, p := range tr {
					pa := 0.0
					if !math.IsNaN(p.Alt) {
						pa = ftToM(p.Alt)
					}
					fmt.Fprintf(&b, "%.5f,%.5f,%.0f ", p.Lon, p.Lat, pa)
				}
				b.WriteString(`</coordinates></LineString></Placemark>`)
			}
			b.WriteString(`</Folder>`)
		}
		b.WriteString(`</Document></kml>`)
		sendFile(w, r, b.Bytes(), ctKML, "airview-live-"+stamp+".kml")
		return
	}
	fc := geoFC{Type: "FeatureCollection", Features: []geoFeature{}}
	for i := range snap.Aircraft {
		a := &snap.Aircraft[i]
		lat, lon, ok := liveLatLon(a)
		if !ok {
			continue
		}
		props := map[string]any{"kind": "aircraft", "icao": a.ICAO, "callsign": a.Callsign, "reg": a.Registration,
			"type": a.TypeCode, "operator": a.Operator, "altFt": rnd(liveAltFt(a), 0), "source": a.Source,
			"predicted": a.Predicted, "lastSeen": a.LastSeen}
		if a.GS != nil {
			props["gsKt"] = rnd(*a.GS, 0)
		}
		if a.Track != nil {
			props["track"] = rnd(*a.Track, 0)
		}
		fc.Features = append(fc.Features, geoFeature{Type: "Feature", Properties: props,
			Geometry: geoGeometry{Type: "Point", Coordinates: [2]num{rnd(lon, 5), rnd(lat, 5)}}})
		if tr := trails[a.ICAO]; len(tr) > 1 {
			coords := make([][3]num, len(tr))
			times := make([]int64, len(tr))
			for j, p := range tr {
				coords[j] = [3]num{rnd(p.Lon, 5), rnd(p.Lat, 5), rnd(ftToM(p.Alt), 0)}
				times[j] = p.T
			}
			fc.Features = append(fc.Features, geoFeature{Type: "Feature",
				Properties: map[string]any{"kind": "trail", "icao": a.ICAO, "times": times},
				Geometry:   geoGeometry{Type: "LineString", Coordinates: coords}})
		}
	}
	body, err := marshal(fc)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	sendFile(w, r, body, ctGeoJSON, "airview-live-"+stamp+".geojson")
}

func liveDesc(a *model.Aircraft) string {
	var parts []string
	if a.Registration != "" {
		parts = append(parts, a.Registration)
	}
	if a.TypeCode != "" {
		parts = append(parts, a.TypeCode)
	}
	if alt := liveAltFt(a); !math.IsNaN(alt) {
		parts = append(parts, fmt.Sprintf("%.0f ft", alt))
	}
	if a.GS != nil {
		parts = append(parts, fmt.Sprintf("%.0f kt", *a.GS))
	}
	if a.Predicted {
		parts = append(parts, "position not received recently")
	}
	return strings.Join(parts, ", ")
}

// csvOut starts a streamed CSV download (gzip when accepted).
func csvOut(w http.ResponseWriter, r *http.Request, name string) (*csv.Writer, func()) {
	h := w.Header()
	h.Set("Content-Type", ctCSV)
	h.Set("Content-Disposition", `attachment; filename="`+name+`"`)
	h.Set("Cache-Control", "no-store")
	h.Set("X-Content-Type-Options", "nosniff")
	var out io.Writer = w
	var zw *gzip.Writer
	if acceptsGzip(r) {
		h.Set("Content-Encoding", "gzip")
		h.Add("Vary", "Accept-Encoding")
		zw, _ = gzip.NewWriterLevel(w, gzip.BestSpeed)
		out = zw
	}
	w.WriteHeader(http.StatusOK)
	cw := csv.NewWriter(out)
	return cw, func() {
		cw.Flush()
		if zw != nil {
			_ = zw.Close()
		}
	}
}

// rawExportAircraft is GET /api/export/aircraft.csv: every airframe ever seen.
func rawExportAircraft(s *Server, w http.ResponseWriter, r *http.Request) {
	r, done, ok := s.exportBegin(w, r)
	if !ok {
		return
	}
	defer done()
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Minute)
	defer cancel()
	s.freshAircraftRows(ctx)
	cw, finish := csvOut(w, r, "airview-aircraft.csv")
	defer finish()
	_ = cw.Write([]string{"icao", "reg", "type", "type_desc", "operator", "country", "category", "military",
		"first_seen", "last_seen", "msgs", "flights", "positions", "max_dist_km", "max_alt_ft", "year"})
	rc := http.NewResponseController(w)
	for offset := 0; ; offset += 5000 {
		rows, _, err := s.eng.Store().ListAircraft(ctx, store.AircraftQuery{Sort: "icao", Limit: 5000, Offset: offset})
		if err != nil {
			return
		}
		for _, a := range rows {
			yr := ""
			if a.Year > 0 {
				yr = strconv.Itoa(a.Year)
			}
			_ = cw.Write([]string{modes.FormatICAO(a.ICAO), a.Reg, a.Type, a.TypeDesc, a.Operator, a.Country, a.Category,
				strconv.FormatBool(a.Flags&store.FlagMilitary != 0), iso(a.FirstSeen), iso(a.LastSeen),
				strconv.FormatInt(a.Msgs, 10), strconv.FormatInt(a.Flights, 10), strconv.FormatInt(a.Positions, 10),
				fmtF(a.MaxDistKm, 1), fmtF(a.MaxAlt, 0), yr})
		}
		_ = rc.SetWriteDeadline(time.Now().Add(60 * time.Second))
		cw.Flush()
		if cw.Error() != nil || len(rows) < 5000 {
			return
		}
	}
}

// ---------------------------------------------------------------------------
// Reports (VirtualRadar style)
// ---------------------------------------------------------------------------

// reportRow is one flight of a report with the airframe's reference data.
type reportRow struct {
	FlightID    int64  `json:"flightId"`
	ICAO        string `json:"icao"`
	Callsign    string `json:"callsign,omitempty"`
	Squawk      string `json:"squawk,omitempty"`
	Reg         string `json:"reg,omitempty"`
	Type        string `json:"type,omitempty"`
	TypeDesc    string `json:"typeDesc,omitempty"`
	Operator    string `json:"operator,omitempty"`
	Country     string `json:"country,omitempty"`
	Category    string `json:"category,omitempty"`
	Military    bool   `json:"military,omitempty"`
	Interesting bool   `json:"interesting,omitempty"`
	Start       int64  `json:"start"` // Unix ms
	End         int64  `json:"end"`
	DurationS   int64  `json:"durationS"`
	Msgs        int64  `json:"msgs"`
	Positions   int64  `json:"positions"`
	SrcMask     int    `json:"srcMask"`
	MinDistKm   num    `json:"minDistKm"`
	MaxDistKm   num    `json:"maxDistKm"`
	MaxDistBrg  num    `json:"maxDistBrg"`
	MinAlt      num    `json:"minAlt"` // ft
	MaxAlt      num    `json:"maxAlt"`
	MaxGS       num    `json:"maxGs"` // kt
	RSSIMax     num    `json:"rssiMax"`
	RSSIAvg     num    `json:"rssiAvg"`
	FirstLat    num    `json:"firstLat"`
	FirstLon    num    `json:"firstLon"`
	LastLat     num    `json:"lastLat"`
	LastLon     num    `json:"lastLon"`
	Emergency   string `json:"emergency,omitempty"`
	Dep         string `json:"dep"` // departure airport code (stored once the flight is closed; "" = unknown / not yet resolved)
	Arr         string `json:"arr"` // arrival airport code ("" = unknown)
}

// reportReply is GET /api/reports.
type reportReply struct {
	From   int64       `json:"from"`
	To     int64       `json:"to"`
	Total  int         `json:"total"` // matching flights
	Limit  int         `json:"limit"`
	Offset int         `json:"offset"`
	Sort   string      `json:"sort"`
	Desc   bool        `json:"desc"`
	Rows   []reportRow `json:"rows"`
}

var reportSortCols = map[string]string{
	"start": `f.start`, "end": `f."end"`, "callsign": `f.callsign`, "icao": `f.icao`, "reg": `reg`, "type": `type`,
	"operator": `operator`, "country": `country`, "maxDistKm": `f.max_dist_km`, "minDistKm": `f.min_dist_km`,
	"maxAlt": `f.max_alt`, "minAlt": `f.min_alt`, "maxGs": `f.max_gs`, "msgs": `f.msgs`, "positions": `f.positions`,
	"duration": `(f."end" - f.start)`, "rssiMax": `f.rssi_max`, "dep": `COALESCE(f.dep, '')`, "arr": `COALESCE(f.arr, '')`,
}

const reportSelect = `SELECT f.id, f.icao, f.callsign, f.squawk, f.start, f."end", f.msgs, f.positions, f.src_mask,
  f.min_dist_km, f.max_dist_km, f.max_dist_brg, f.min_alt, f.max_alt, f.max_gs, f.rssi_max, f.rssi_avg,
  f.first_lat, f.first_lon, f.last_lat, f.last_lon, f.emergency,
  CASE WHEN COALESCE(a.reg, '') != '' THEN a.reg ELSE COALESCE(d.reg, '') END AS reg,
  CASE WHEN COALESCE(a.type, '') != '' THEN a.type ELSE COALESCE(d.type, '') END AS type,
  COALESCE(d."desc", '') AS type_desc,
  CASE WHEN COALESCE(a.operator, '') != '' THEN a.operator ELSE COALESCE(d.operator, '') END AS operator,
  COALESCE(a.country, '') AS country, COALESCE(a.category, '') AS category,
  COALESCE(a.flags, 0) | COALESCE(d.flags, 0) AS flags, COALESCE(f.dep, ''), COALESCE(f.arr, '')
FROM flights f LEFT JOIN aircraft a ON a.icao = f.icao LEFT JOIN acdb d ON d.icao = f.icao`

// globLike turns a user pattern (* and ? wildcards, otherwise substring
// for a plain text) into a LIKE pattern.
func globLike(v string, substring bool) string {
	var b strings.Builder
	wild := strings.ContainsAny(v, "*?")
	if substring && !wild {
		b.WriteByte('%')
	}
	for _, c := range v {
		switch c {
		case '*':
			b.WriteByte('%')
		case '?':
			b.WriteByte('_')
		case '%', '_', '\\':
			b.WriteByte('\\')
			b.WriteRune(c)
		default:
			b.WriteRune(c)
		}
	}
	if substring && !wild {
		b.WriteByte('%')
	}
	return b.String()
}

// airportCodes returns the stored codes an airport filter value matches:
// the value itself (upper case) plus the stored code of the region airport
// it names (so an IATA code finds the ICAO code that is stored).
func airportCodes(v string, reg *navdata.Region) []string {
	v = strings.ToUpper(strings.TrimSpace(v))
	out := []string{v}
	if reg != nil {
		if a, ok := reg.AirportByCode(v); ok {
			if c := strings.ToUpper(a.Code()); c != v {
				out = append(out, c)
			}
		}
	}
	return out
}

// reportQuery builds the WHERE clause of a report from the request. reg
// (may be nil) maps IATA / alternative codes of the dep / arr filters to the
// stored airport code.
func reportQuery(r *http.Request, reg *navdata.Region) (where string, args []any, from, to time.Time, err error) {
	from, to, err = qSpanDefault(r, 24*time.Hour)
	if err != nil {
		return
	}
	conds := []string{`f."end" >= ?`, `f.start <= ?`}
	args = []any{from.Unix(), to.Unix()}
	text := func(param, expr string, substring bool) {
		if v := qStr(r, param); v != "" {
			if len(v) > 64 {
				v = v[:64]
			}
			conds = append(conds, expr+` LIKE ? ESCAPE '\'`)
			args = append(args, globLike(strings.ToUpper(v), substring))
		}
	}
	text("callsign", `UPPER(f.callsign)`, false)
	text("reg", `UPPER(CASE WHEN COALESCE(a.reg, '') != '' THEN a.reg ELSE COALESCE(d.reg, '') END)`, false)
	text("type", `UPPER(CASE WHEN COALESCE(a.type, '') != '' THEN a.type ELSE COALESCE(d.type, '') END)`, false)
	text("operator", `UPPER(CASE WHEN COALESCE(a.operator, '') != '' THEN a.operator ELSE COALESCE(d.operator, '') END)`, true)
	text("country", `UPPER(COALESCE(a.country, ''))`, true)
	text("squawk", `f.squawk`, false)
	if v := qStr(r, "icao"); v != "" {
		v = strings.ToUpper(strings.TrimPrefix(v, "~"))
		if strings.ContainsAny(v, "*?") {
			conds = append(conds, `printf('%06X', f.icao & 16777215) LIKE ? ESCAPE '\'`)
			args = append(args, globLike(v, false))
		} else {
			key, perr := parseICAOParam(v)
			if perr != nil {
				err = perr
				return
			}
			conds = append(conds, `f.icao = ?`)
			args = append(args, int64(key))
		}
	}
	if v := qStr(r, "military"); v != "" {
		if qBool(r, "military", false) {
			conds = append(conds, `((COALESCE(a.flags, 0) | COALESCE(d.flags, 0)) & 1) != 0`)
		} else {
			conds = append(conds, `((COALESCE(a.flags, 0) | COALESCE(d.flags, 0)) & 1) = 0`)
		}
	}
	if qBool(r, "emergency", false) {
		conds = append(conds, `f.emergency != ''`)
	}
	airport := func(param, col string) {
		v := qStr(r, param)
		if v == "" {
			return
		}
		if len(v) > 16 {
			v = v[:16]
		}
		codes := airportCodes(v, reg)
		// The raw column (stored upper case by SetFlightAirports), so the
		// flights_dep / flights_arr indexes can be used.
		conds = append(conds, col+` IN (`+strings.TrimSuffix(strings.Repeat("?,", len(codes)), ",")+`)`)
		for _, c := range codes {
			args = append(args, c)
		}
	}
	airport("dep", "f.dep")
	airport("arr", "f.arr")
	rangeCond := func(param, expr, op string) {
		if qStr(r, param) == "" || err != nil {
			return
		}
		var v float64
		if v, err = qFloat(r, param, 0); err == nil {
			conds = append(conds, expr+" "+op+" ?")
			args = append(args, v)
		}
	}
	rangeCond("minDistKm", "f.max_dist_km", ">=")
	rangeCond("maxDistKm", "f.min_dist_km", "<=")
	rangeCond("minAlt", "f.max_alt", ">=")
	rangeCond("maxAlt", "f.min_alt", "<=")
	rangeCond("minPositions", "f.positions", ">=")
	if err != nil {
		return
	}
	where = " WHERE " + strings.Join(conds, " AND ")
	return
}

func scanReport(rows interface{ Scan(...any) error }) (reportRow, error) {
	var x reportRow
	var icao, flags int64
	var minD, maxD, brg, minA, maxA, gs, rmax, ravg, fla, flo, lla, llo nfloat
	err := rows.Scan(&x.FlightID, &icao, &x.Callsign, &x.Squawk, &x.Start, &x.End, &x.Msgs, &x.Positions, &x.SrcMask,
		&minD, &maxD, &brg, &minA, &maxA, &gs, &rmax, &ravg, &fla, &flo, &lla, &llo, &x.Emergency,
		&x.Reg, &x.Type, &x.TypeDesc, &x.Operator, &x.Country, &x.Category, &flags, &x.Dep, &x.Arr)
	if err != nil {
		return x, err
	}
	x.ICAO = modes.FormatICAO(uint32(icao))
	x.DurationS = x.End - x.Start
	x.Start, x.End = x.Start*1000, x.End*1000
	x.MinDistKm, x.MaxDistKm, x.MaxDistBrg = rnd(float64(minD), 2), rnd(float64(maxD), 2), rnd(float64(brg), 1)
	x.MinAlt, x.MaxAlt, x.MaxGS = rnd(float64(minA), 0), rnd(float64(maxA), 0), rnd(float64(gs), 0)
	x.RSSIMax, x.RSSIAvg = rnd(float64(rmax), 1), rnd(float64(ravg), 1)
	x.FirstLat, x.FirstLon, x.LastLat, x.LastLon = rnd(float64(fla), 5), rnd(float64(flo), 5), rnd(float64(lla), 5), rnd(float64(llo), 5)
	x.Military = flags&int64(store.FlagMilitary) != 0
	x.Interesting = flags&int64(store.FlagInteresting) != 0
	return x, nil
}

// nfloat scans a nullable REAL (NULL -> NaN).
type nfloat float64

func (n *nfloat) Scan(v any) error {
	switch x := v.(type) {
	case nil:
		*n = nfloat(math.NaN())
	case float64:
		*n = nfloat(x)
	case int64:
		*n = nfloat(x)
	default:
		*n = nfloat(math.NaN())
	}
	return nil
}

func reportOrder(r *http.Request) (string, string, bool, error) {
	key := qStr(r, "sort")
	if key == "" {
		key = "start"
	}
	col, ok := reportSortCols[key]
	if !ok {
		keys := make([]string, 0, len(reportSortCols))
		for k := range reportSortCols {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		return "", "", false, badRequest("parameter sort: one of %s", strings.Join(keys, ", "))
	}
	desc := qBool(r, "desc", key == "start" || key == "end" || strings.HasPrefix(key, "max") || key == "msgs" || key == "positions" || key == "duration")
	dir := " ASC"
	if desc {
		dir = " DESC"
	}
	return key, " ORDER BY " + col + dir + ", f.id" + dir, desc, nil
}

func apiReports(s *Server, r *http.Request) (any, error) {
	where, args, from, to, err := reportQuery(r, s.eng.Region())
	if err != nil {
		return nil, err
	}
	key, order, desc, err := reportOrder(r)
	if err != nil {
		return nil, err
	}
	limit, err := qInt(r, "limit", 100, 1, 1000)
	if err != nil {
		return nil, err
	}
	offset, err := qInt(r, "offset", 0, 0, 1<<30)
	if err != nil {
		return nil, err
	}
	db := s.eng.Store().DB()
	out := reportReply{From: from.UnixMilli(), To: to.UnixMilli(), Limit: limit, Offset: offset, Sort: key, Desc: desc, Rows: []reportRow{}}
	if err := db.QueryRowContext(r.Context(), `SELECT COUNT(*) FROM flights f LEFT JOIN aircraft a ON a.icao = f.icao LEFT JOIN acdb d ON d.icao = f.icao`+where, args...).Scan(&out.Total); err != nil {
		return nil, err
	}
	rows, err := db.QueryContext(r.Context(), reportSelect+where+order+` LIMIT ? OFFSET ?`, append(args, limit, offset)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		x, err := scanReport(rows)
		if err != nil {
			return nil, err
		}
		out.Rows = append(out.Rows, x)
	}
	return out, rows.Err()
}

// rawReportsCSV is GET /api/reports.csv: the same query as a CSV download.
func rawReportsCSV(s *Server, w http.ResponseWriter, r *http.Request) {
	where, args, _, _, err := reportQuery(r, s.eng.Region())
	if err != nil {
		s.writeAPIError(w, r, err)
		return
	}
	_, order, _, err := reportOrder(r)
	if err != nil {
		s.writeAPIError(w, r, err)
		return
	}
	limit, err := qInt(r, "limit", 100_000, 1, 1_000_000)
	if err != nil {
		s.writeAPIError(w, r, err)
		return
	}
	r, done, ok := s.exportBegin(w, r)
	if !ok {
		return
	}
	defer done()
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Minute)
	defer cancel()
	rows, err := s.eng.Store().DB().QueryContext(ctx, reportSelect+where+order+` LIMIT ?`, append(args, limit)...)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer rows.Close()
	cw, finish := csvOut(w, r, "airview-report.csv")
	defer finish()
	_ = cw.Write([]string{"flight_id", "icao", "callsign", "squawk", "reg", "type", "type_desc", "operator", "country",
		"category", "military", "start", "end", "duration_s", "msgs", "positions", "min_dist_km", "max_dist_km",
		"max_dist_brg", "min_alt_ft", "max_alt_ft", "max_gs_kt", "rssi_max", "rssi_avg", "first_lat", "first_lon",
		"last_lat", "last_lon", "emergency", "dep", "arr"})
	n := 0
	rc := http.NewResponseController(w)
	for rows.Next() {
		x, err := scanReport(rows)
		if err != nil {
			return
		}
		nf := func(v num, d int) string { return fmtF(float64(v), d) }
		_ = cw.Write([]string{strconv.FormatInt(x.FlightID, 10), x.ICAO, x.Callsign, x.Squawk, x.Reg, x.Type, x.TypeDesc,
			x.Operator, x.Country, x.Category, strconv.FormatBool(x.Military), iso(x.Start / 1000), iso(x.End / 1000),
			strconv.FormatInt(x.DurationS, 10), strconv.FormatInt(x.Msgs, 10), strconv.FormatInt(x.Positions, 10),
			nf(x.MinDistKm, 2), nf(x.MaxDistKm, 2), nf(x.MaxDistBrg, 1), nf(x.MinAlt, 0), nf(x.MaxAlt, 0), nf(x.MaxGS, 0),
			nf(x.RSSIMax, 1), nf(x.RSSIAvg, 1), nf(x.FirstLat, 5), nf(x.FirstLon, 5), nf(x.LastLat, 5), nf(x.LastLon, 5), x.Emergency, x.Dep, x.Arr})
		if n++; n%2000 == 0 {
			_ = rc.SetWriteDeadline(time.Now().Add(60 * time.Second))
			cw.Flush()
			if cw.Error() != nil {
				return
			}
		}
	}
}
