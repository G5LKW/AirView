package feed

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"airview/internal/model"
	"airview/internal/ntp"
)

// JSON aircraft-list dialects.
const (
	JSONReadsb    = "readsb"    // readsb / dump1090-fa / dump1090-mutability aircraft.json
	JSONRadarcape = "radarcape" // Radarcape aircraftlist.json
)

// flexNum accepts a JSON number, a numeric string, or null/garbage (= absent).
type flexNum struct {
	V  float64
	OK bool
}

func (f *flexNum) UnmarshalJSON(b []byte) error {
	*f = flexNum{}
	if len(b) == 0 || b[0] == 'n' {
		return nil
	}
	if b[0] == '"' {
		if len(b) < 2 {
			return nil
		}
		b = bytes.TrimSpace(b[1 : len(b)-1])
	}
	v, err := strconv.ParseFloat(string(b), 64)
	if err == nil && !math.IsNaN(v) && !math.IsInf(v, 0) {
		f.V, f.OK = v, true
	}
	return nil
}

// flexStr accepts a JSON string or a bare number/boolean (kept as its
// literal text), or null (= absent).
type flexStr struct {
	S  string
	OK bool
}

func (f *flexStr) UnmarshalJSON(b []byte) error {
	*f = flexStr{}
	if len(b) == 0 || b[0] == 'n' {
		return nil
	}
	if b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return nil
		}
		f.S, f.OK = strings.TrimSpace(s), true
		return nil
	}
	if b[0] == '{' || b[0] == '[' {
		return nil
	}
	f.S, f.OK = string(b), true
	return nil
}

// jsonAircraft is the union of the fields of both dialects that map onto a
// DecodedUpdate. Unknown fields are ignored.
type jsonAircraft struct {
	Hex flexStr `json:"hex"`
	Lat flexNum `json:"lat"`
	Lon flexNum `json:"lon"`

	// readsb / dump1090-fa (and the older dump1090-mutability names).
	Type     flexStr         `json:"type"`
	Flight   flexStr         `json:"flight"`
	AltBaro  json.RawMessage `json:"alt_baro"` // number or "ground"
	Altitude json.RawMessage `json:"altitude"` // old name; number or "ground"
	AltGeom  flexNum         `json:"alt_geom"`
	GS       flexNum         `json:"gs"`
	Speed    flexNum         `json:"speed"` // old name
	Track    flexNum         `json:"track"`
	BaroRate flexNum         `json:"baro_rate"`
	GeomRate flexNum         `json:"geom_rate"`
	VertRate flexNum         `json:"vert_rate"` // old name
	Squawk   flexStr         `json:"squawk"`
	Seen     flexNum         `json:"seen"`
	SeenPos  flexNum         `json:"seen_pos"`
	RSSI     flexNum         `json:"rssi"`
	Messages flexNum         `json:"messages"`
	MLAT     []string        `json:"mlat"`

	// Radarcape aircraftlist.json.
	UTI  flexNum `json:"uti"`  // time of last message, Unix seconds
	NS   flexNum `json:"ns"`   // ... nanoseconds part
	Fli  flexStr `json:"fli"`  // callsign
	Src  flexStr `json:"src"`  // selected position source: A, M, F, L, O, S, D
	Alt  flexNum `json:"alt"`  // barometric altitude, ft
	AltG flexNum `json:"altg"` // GNSS altitude, ft
	Spd  flexNum `json:"spd"`  // ground speed, kt
	Trk  flexNum `json:"trk"`  // true track, deg
	Vrt  flexNum `json:"vrt"`  // vertical rate, ft/min
	Squ  flexStr `json:"squ"`  // squawk
	GDA  flexStr `json:"gda"`  // "G"round / "A"irborne (lower case = TIS-B)
	DBM  flexNum `json:"dbm"`  // approximate signal level at the antenna connector, dBm
	LLA  flexNum `json:"lla"`  // age of the last position, s
	Tru  flexNum `json:"tru"`  // number of decoded Mode S frames
}

func (a *jsonAircraft) isRadarcape() bool {
	return a.UTI.OK || a.Fli.OK || a.Src.OK || a.GDA.OK || a.Tru.OK || a.Spd.OK || a.Trk.OK || a.Squ.OK || a.LLA.OK
}

// jsonState remembers what was last reported for one aircraft so that only
// changes produce updates.
type jsonState struct {
	msgs  float64 // message counter (readsb "messages", Radarcape "tru")
	stamp float64 // Radarcape uti+ns
	gen   uint32  // poll generation in which the aircraft was last listed
}

// JSONDecoder converts successive aircraft-list documents into
// DecodedUpdates. Each call to Decode handles one complete poll result; an
// update is emitted for every aircraft that received new messages since the
// previous poll. It auto-detects the dialect per document.
type JSONDecoder struct {
	Sink     model.FrameSink
	FeedID   string
	MLATOnly bool
	// Interval is the polling period; it bounds how old a position may be
	// to be reported as new (Interval + 1 s). Default 1 s.
	Interval time.Duration

	Format   string // dialect of the last document: JSONReadsb or JSONRadarcape
	Aircraft int    // aircraft listed in the last document
	Updates  uint64 // updates delivered in total

	seen map[uint32]jsonState
	gen  uint32
}

// errNoAircraft is returned for documents that are valid JSON but contain
// no aircraft array.
var errNoAircraft = errors.New("JSON document contains no aircraft list")

// Decode parses one document fetched at time now and delivers the updates.
func (d *JSONDecoder) Decode(doc []byte, now time.Time) error {
	doc = bytes.TrimSpace(doc)
	doc = bytes.TrimPrefix(doc, []byte("\xEF\xBB\xBF"))
	if len(doc) == 0 {
		return errors.New("empty JSON document")
	}
	var list []jsonAircraft
	switch doc[0] {
	case '[':
		if err := json.Unmarshal(doc, &list); err != nil {
			return fmt.Errorf("aircraft list: %w", err)
		}
	case '{':
		var top map[string]json.RawMessage
		if err := json.Unmarshal(doc, &top); err != nil {
			return fmt.Errorf("aircraft list: %w", err)
		}
		raw, ok := top["aircraft"]
		if !ok {
			// Unknown wrapper: take the first member that is an array.
			for _, k := range []string{"aircraftlist", "list", "ac", "data"} {
				if v, has := top[k]; has && len(v) > 0 && v[0] == '[' {
					raw, ok = v, true
					break
				}
			}
		}
		if !ok {
			return errNoAircraft
		}
		if err := json.Unmarshal(raw, &list); err != nil {
			return fmt.Errorf("aircraft list: %w", err)
		}
	default:
		return errors.New("not a JSON aircraft list")
	}

	if d.seen == nil {
		d.seen = make(map[uint32]jsonState)
	}
	interval := d.Interval
	if interval <= 0 {
		interval = time.Second
	}
	fresh := interval.Seconds() + 1
	d.gen++
	d.Aircraft = len(list)
	radarcape := 0

	for i := range list {
		a := &list[i]
		if !a.Hex.OK {
			continue
		}
		icao, ok := parseHexAddr(a.Hex.S)
		if !ok {
			continue
		}
		prev, known := d.seen[icao]
		var (
			u   model.DecodedUpdate
			cur jsonState
		)
		if a.isRadarcape() {
			radarcape++
			u, cur, ok = d.fromRadarcape(a, prev, known, fresh)
		} else {
			u, cur, ok = d.fromReadsb(a, prev, known, fresh, now)
		}
		cur.gen = d.gen
		d.seen[icao] = cur
		if !ok {
			continue
		}
		u.ICAO = icao
		u.FeedID = d.FeedID
		if u.Recv.IsZero() {
			u.Recv = now
		}
		if d.MLATOnly {
			u.MLAT = true
		}
		if u.Msgs < 1 {
			u.Msgs = 1
		}
		d.Updates++
		d.Sink.HandleUpdate(u)
	}

	// Forget aircraft that left the list (bounds the map to the list size).
	for icao, st := range d.seen {
		if st.gen != d.gen {
			delete(d.seen, icao)
		}
	}
	switch {
	case len(list) == 0:
		// Nothing to judge by: a bare array is the Radarcape's shape, an
		// object with an "aircraft" member is readsb's.
		if d.Format == "" {
			d.Format = JSONReadsb
			if doc[0] == '[' {
				d.Format = JSONRadarcape
			}
		}
	case radarcape*2 > len(list):
		d.Format = JSONRadarcape
	default:
		d.Format = JSONReadsb
	}
	return nil
}

// msgCount converts a message-counter difference to DecodedUpdate.Msgs
// (at least 1, and safe on platforms with a 32-bit int).
func msgCount(delta float64) int {
	switch {
	case !(delta >= 1): // also catches NaN
		return 1
	case delta > math.MaxInt32:
		return math.MaxInt32
	}
	return int(delta)
}

func optF(n flexNum) *float64 {
	if !n.OK {
		return nil
	}
	v := n.V
	return &v
}

func optPos(lat, lon flexNum) (*float64, *float64) {
	if !lat.OK || !lon.OK || lat.V < -90 || lat.V > 90 || lon.V < -180 || lon.V > 180 || (lat.V == 0 && lon.V == 0) {
		return nil, nil
	}
	la, lo := lat.V, lon.V
	return &la, &lo
}

// altOrGround decodes readsb's alt_baro: a number of feet or "ground".
func altOrGround(raw json.RawMessage) (alt *float64, ground bool) {
	if len(raw) == 0 {
		return nil, false
	}
	if raw[0] == '"' {
		return nil, strings.EqualFold(strings.Trim(string(raw), `"`), "ground")
	}
	var n flexNum
	n.UnmarshalJSON(raw)
	return optF(n), false
}

func (d *JSONDecoder) fromReadsb(a *jsonAircraft, prev jsonState, known bool, fresh float64, now time.Time) (model.DecodedUpdate, jsonState, bool) {
	var u model.DecodedUpdate
	cur := jsonState{msgs: a.Messages.V}

	// Has anything been received since the previous poll?
	switch {
	case a.Messages.OK && known:
		delta := a.Messages.V - prev.msgs
		if delta <= 0 {
			if delta < 0 { // counter restarted
				cur.msgs = a.Messages.V
			}
			return u, cur, false
		}
		u.Msgs = msgCount(delta)
	case a.Seen.OK:
		if a.Seen.V > fresh {
			return u, cur, false
		}
		u.Msgs = 1
	default:
		u.Msgs = 1
	}

	if a.Seen.OK && a.Seen.V >= 0 && a.Seen.V <= fresh {
		u.Recv = now.Add(-time.Duration(a.Seen.V * float64(time.Second)))
	}
	if a.Flight.OK && a.Flight.S != "" {
		cs := a.Flight.S
		u.Callsign = &cs
	}
	if a.Squawk.OK && isSquawk(a.Squawk.S) {
		sq := normSquawk(a.Squawk.S)
		u.Squawk = &sq
	}
	raw := a.AltBaro
	if len(raw) == 0 {
		raw = a.Altitude
	}
	alt, ground := altOrGround(raw)
	u.AltBaro = alt
	if ground {
		u.OnGround = model.B(true)
	} else if alt != nil {
		u.OnGround = model.B(false)
	}
	u.AltGeom = optF(a.AltGeom)
	if u.GS = optF(a.GS); u.GS == nil {
		u.GS = optF(a.Speed)
	}
	u.Track = optF(a.Track)
	if u.VertRate = optF(a.BaroRate); u.VertRate == nil {
		if u.VertRate = optF(a.GeomRate); u.VertRate == nil {
			u.VertRate = optF(a.VertRate)
		}
	}
	if !a.SeenPos.OK || a.SeenPos.V <= fresh {
		u.Lat, u.Lon = optPos(a.Lat, a.Lon)
	}
	if a.RSSI.OK && a.RSSI.V < 0 {
		u.Signal = optF(a.RSSI)
	}
	if a.Type.OK && a.Type.S == "mlat" {
		u.MLAT = true
	}
	for _, f := range a.MLAT {
		if f == "lat" || f == "lon" {
			u.MLAT = true
		}
	}
	return u, cur, true
}

func (d *JSONDecoder) fromRadarcape(a *jsonAircraft, prev jsonState, known bool, fresh float64) (model.DecodedUpdate, jsonState, bool) {
	var u model.DecodedUpdate
	cur := jsonState{msgs: a.Tru.V, stamp: a.UTI.V + a.NS.V/1e9}

	// Sources other than 1090 MHz Mode S / MLAT are not aircraft this
	// station tracks (FLARM, OGN, ADS-L on 868 MHz).
	src := strings.ToUpper(a.Src.S)
	switch src {
	case "L", "O", "S", "D":
		return u, cur, false
	}

	u.Msgs = 1
	if known {
		newMsgs := a.Tru.OK && a.Tru.V > prev.msgs
		newStamp := a.UTI.OK && cur.stamp != prev.stamp
		if a.Tru.OK || a.UTI.OK {
			if !newMsgs && !newStamp {
				return u, cur, false
			}
		}
		if newMsgs {
			u.Msgs = msgCount(a.Tru.V - prev.msgs)
		}
	}

	if a.Fli.OK && a.Fli.S != "" {
		cs := a.Fli.S
		u.Callsign = &cs
	}
	if a.Squ.OK {
		sq := a.Squ.S
		if i := strings.IndexByte(sq, '.'); i >= 0 { // number rendered as 7000.0
			sq = sq[:i]
		}
		if isSquawk(sq) {
			sq = normSquawk(sq)
			u.Squawk = &sq
		}
	}
	u.AltBaro = optF(a.Alt)
	u.AltGeom = optF(a.AltG)
	u.GS = optF(a.Spd)
	u.Track = optF(a.Trk)
	u.VertRate = optF(a.Vrt)
	if a.GDA.OK && a.GDA.S != "" {
		switch a.GDA.S[0] {
		case 'G', 'g':
			u.OnGround = model.B(true)
		case 'A', 'a':
			u.OnGround = model.B(false)
		}
	}
	if !a.LLA.OK || a.LLA.V <= fresh {
		u.Lat, u.Lon = optPos(a.Lat, a.Lon)
	}
	// "dbm" is an approximate level at the antenna connector in dBm, not
	// dBFS; it is passed through as the best available signal figure.
	if a.DBM.OK && a.DBM.V < 0 {
		u.Signal = optF(a.DBM)
	}
	if src == "M" || src == "F" {
		u.MLAT = true
	}
	return u, cur, true
}

// --- HTTP poller --------------------------------------------------------------

// jsonProbePaths are tried in order when the feed has no explicit document URL.
var jsonProbePaths = []string{
	"/aircraftlist.json",              // Radarcape
	"/data/aircraft.json",             // readsb / dump1090 built-in web server, tar1090 root installs
	"/tar1090/data/aircraft.json",     // tar1090
	"/skyaware/data/aircraft.json",    // dump1090-fa (PiAware)
	"/dump1090-fa/data/aircraft.json", // older dump1090-fa
	"/dump1090/data/aircraft.json",    // dump1090-mutability
}

const (
	jsonMaxBody     = 16 << 20
	jsonHTTPTimeout = 8 * time.Second
)

// jsonCandidates returns the URLs to try for a feed: the configured URL
// when it names a document, otherwise the well-known paths on the
// configured base URL or host/port.
func jsonCandidates(cfg model.Feed) ([]string, error) {
	raw := strings.TrimSpace(cfg.URL)
	if raw == "" {
		if strings.TrimSpace(cfg.Host) == "" {
			return nil, errors.New("json feed needs a URL or a host")
		}
		host := strings.TrimSpace(cfg.Host)
		if cfg.Port > 0 && cfg.Port != 80 {
			host = net.JoinHostPort(host, strconv.Itoa(cfg.Port))
		} else if strings.Contains(host, ":") { // bare IPv6 literal
			host = "[" + host + "]"
		}
		raw = "http://" + host
	} else if !strings.Contains(raw, "://") {
		raw = "http://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("bad feed URL: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("unsupported URL scheme %q", u.Scheme)
	}
	if u.Host == "" {
		return nil, errors.New("feed URL has no host")
	}
	if strings.HasSuffix(strings.ToLower(u.Path), ".json") || u.RawQuery != "" {
		return []string{u.String()}, nil
	}
	base := strings.TrimRight(u.String(), "/")
	out := make([]string, len(jsonProbePaths))
	for i, p := range jsonProbePaths {
		out[i] = base + p
	}
	return out, nil
}

// jsonRunner polls an aircraft list over HTTP.
type jsonRunner struct {
	cfg      model.Feed
	interval time.Duration
	client   *http.Client
}

func newJSONRunner(cfg model.Feed, interval time.Duration) *jsonRunner {
	if interval <= 0 {
		interval = time.Second
	}
	tr := &http.Transport{
		DialContext:           (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		MaxIdleConns:          2,
		MaxIdleConnsPerHost:   2,
		IdleConnTimeout:       90 * time.Second,
		ResponseHeaderTimeout: 5 * time.Second,
	}
	return &jsonRunner{cfg: cfg, interval: interval, client: &http.Client{Transport: tr, Timeout: jsonHTTPTimeout}}
}

func (r *jsonRunner) fetch(ctx context.Context, u string, buf *bytes.Buffer) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", model.AppName+"/"+model.Version)
	resp, err := r.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		return fmt.Errorf("HTTP %s", resp.Status)
	}
	buf.Reset()
	n, err := buf.ReadFrom(io.LimitReader(resp.Body, jsonMaxBody+1))
	if err != nil {
		return err
	}
	if n > jsonMaxBody {
		return errors.New("aircraft list larger than 16 MiB")
	}
	return nil
}

// Run implements Runner.
func (r *jsonRunner) Run(ctx context.Context, s *Session) error {
	defer r.client.CloseIdleConnections()
	cands, err := jsonCandidates(r.cfg)
	if err != nil {
		s.SetDisconnected(err)
		<-ctx.Done() // a configuration error cannot heal by retrying
		return ctx.Err()
	}
	dec := &JSONDecoder{Sink: s, FeedID: r.cfg.ID, MLATOnly: r.cfg.MLATOnly, Interval: r.interval}
	var (
		buf       bytes.Buffer
		bo        = s.newBackoff()
		active    = -1 // index of the working candidate
		failures  int
		connected bool
		lastInfo  string
	)
	for {
		var (
			ferr error
			used string
		)
		if active >= 0 {
			used = cands[active]
			if ferr = r.fetch(ctx, used, &buf); ferr == nil {
				ferr = dec.Decode(buf.Bytes(), ntp.Now())
			}
		} else {
			// Probe: the first candidate that yields an aircraft list wins.
			for i, c := range cands {
				err := r.fetch(ctx, c, &buf)
				if err == nil {
					err = dec.Decode(buf.Bytes(), ntp.Now())
				}
				if err == nil {
					active, used, ferr = i, c, nil
					break
				}
				if ferr == nil || i == 0 {
					ferr = fmt.Errorf("%s: %w", c, err)
				}
				if ctx.Err() != nil {
					break
				}
			}
			if active < 0 && len(cands) > 1 && ferr != nil {
				ferr = fmt.Errorf("no aircraft list found (tried %d well-known paths; first error: %w)", len(cands), ferr)
			}
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}

		wait := r.interval
		if ferr == nil {
			s.AddBytes(buf.Len())
			failures = 0
			bo.reset()
			if !connected {
				connected = true
				s.SetConnected(used)
			}
			name := "readsb/dump1090 aircraft.json"
			if dec.Format == JSONRadarcape {
				name = "Radarcape aircraftlist.json"
			}
			if info := fmt.Sprintf("%s, %d aircraft listed", name, dec.Aircraft); info != lastInfo {
				lastInfo = info
				s.SetInfo(info)
			}
		} else {
			failures++
			connected = false
			s.SetDisconnected(ferr)
			if failures >= 3 {
				active = -1 // the document may have moved: probe again
			}
			wait = bo.next()
		}
		if !sleepCtx(ctx, wait) {
			return ctx.Err()
		}
	}
}
