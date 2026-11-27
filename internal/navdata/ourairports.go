package navdata

import (
	"bufio"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
)

// ErrBadHeader is returned when a CSV file lacks a required column.
var ErrBadHeader = errors.New("navdata: CSV header does not match the expected format")

// csvReader wraps encoding/csv with a column index taken from the header row.
type csvReader struct {
	r    *csv.Reader
	idx  map[string]int
	rec  []string
	line int
}

// newCSV reads the header and checks that every required column exists.
func newCSV(r io.Reader, required ...string) (*csvReader, error) {
	br := bufio.NewReaderSize(r, 64<<10)
	// Strip a UTF-8 byte order mark (the standing-data files carry one).
	if b, err := br.Peek(3); err == nil && b[0] == 0xEF && b[1] == 0xBB && b[2] == 0xBF {
		_, _ = br.Discard(3)
	}
	cr := csv.NewReader(br)
	cr.ReuseRecord = true
	cr.FieldsPerRecord = -1
	cr.LazyQuotes = true
	head, err := cr.Read()
	if err != nil {
		if errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("%w: empty file", ErrBadHeader)
		}
		return nil, fmt.Errorf("navdata: reading CSV header: %w", err)
	}
	c := &csvReader{r: cr, idx: make(map[string]int, len(head))}
	for i, h := range head {
		c.idx[strings.ToLower(strings.TrimSpace(h))] = i
	}
	for _, req := range required {
		if _, ok := c.idx[req]; !ok {
			return nil, fmt.Errorf("%w: missing column %q", ErrBadHeader, req)
		}
	}
	return c, nil
}

// next reads the next record; io.EOF at the end. Malformed lines are reported
// as errSkip so the caller can count and continue.
func (c *csvReader) next() error {
	rec, err := c.r.Read()
	c.line++
	if err != nil {
		var pe *csv.ParseError
		if errors.As(err, &pe) {
			return errSkip
		}
		return err
	}
	c.rec = rec
	return nil
}

var errSkip = errors.New("skip")

// s returns the trimmed value of column name ("" when absent).
func (c *csvReader) s(name string) string {
	i, ok := c.idx[name]
	if !ok || i >= len(c.rec) {
		return ""
	}
	return strings.TrimSpace(c.rec[i])
}

// f returns column name as float (NaN when empty/invalid).
func (c *csvReader) f(name string) float64 {
	v := c.s(name)
	if v == "" {
		return math.NaN()
	}
	x, err := strconv.ParseFloat(v, 64)
	if err != nil || math.IsInf(x, 0) {
		return math.NaN()
	}
	return x
}

// i returns column name as *int (nil when empty/invalid).
func (c *csvReader) i(name string) *int {
	x := c.f(name)
	if math.IsNaN(x) || math.Abs(x) > 1e9 {
		return nil
	}
	return iptr(int(math.Round(x)))
}

// id returns column name as int64 (0, false when invalid).
func (c *csvReader) id(name string) (int64, bool) {
	v, err := strconv.ParseInt(c.s(name), 10, 64)
	return v, err == nil && v > 0
}

func (c *csvReader) bool(name string) bool {
	switch strings.ToLower(c.s(name)) {
	case "1", "yes", "true", "y":
		return true
	}
	return false
}

func validLatLon(lat, lon float64) bool {
	return !math.IsNaN(lat) && !math.IsNaN(lon) && lat >= -90 && lat <= 90 && lon >= -180 && lon <= 180
}

// ParseStats counts the rows of one parse.
type ParseStats struct {
	Records int `json:"records"`
	Skipped int `json:"skipped"`
}

// loop drives a parse: fn converts the current record (false = skip it).
func (c *csvReader) loop(fn func() (bool, error)) (ParseStats, error) {
	var st ParseStats
	for {
		err := c.next()
		if errors.Is(err, io.EOF) {
			return st, nil
		}
		if errors.Is(err, errSkip) {
			st.Skipped++
			continue
		}
		if err != nil {
			return st, err
		}
		ok, err := fn()
		if err != nil {
			return st, err
		}
		if ok {
			st.Records++
		} else {
			st.Skipped++
		}
	}
}

// ParseAirports parses OurAirports airports.csv, calling fn for every valid
// row in file order. An error from fn aborts the parse.
func ParseAirports(r io.Reader, fn func(Airport) error) (ParseStats, error) {
	c, err := newCSV(r, "id", "ident", "type", "name", "latitude_deg", "longitude_deg")
	if err != nil {
		return ParseStats{}, err
	}
	return c.loop(func() (bool, error) {
		id, ok := c.id("id")
		a := Airport{ID: id, Ident: c.s("ident"), Type: c.s("type"), Name: c.s("name"),
			Lat: c.f("latitude_deg"), Lon: c.f("longitude_deg")}
		if !ok || a.Ident == "" || !validLatLon(a.Lat, a.Lon) {
			return false, nil
		}
		a.Size = SizeOf(a.Type)
		a.ElevFt = c.i("elevation_ft")
		a.Continent = c.s("continent")
		a.Country = c.s("iso_country")
		a.Region = c.s("iso_region")
		a.Municipality = c.s("municipality")
		a.Scheduled = c.bool("scheduled_service")
		a.ICAO = strings.ToUpper(c.s("icao_code")) // column added by OurAirports in 2024
		a.IATA = strings.ToUpper(c.s("iata_code"))
		a.GPS = strings.ToUpper(c.s("gps_code"))
		a.Local = c.s("local_code")
		if a.ICAO == "" && isICAOAirportCode(a.GPS) && a.GPS == strings.ToUpper(a.Ident) {
			// Older files have no icao_code column: an ident equal to a
			// 4-letter GPS code is the ICAO location indicator.
			if _, has := c.idx["icao_code"]; !has {
				a.ICAO = a.GPS
			}
		}
		return true, fn(a)
	})
}

// isICAOAirportCode reports whether s looks like a 4-letter ICAO location indicator.
func isICAOAirportCode(s string) bool {
	if len(s) != 4 {
		return false
	}
	for i := 0; i < 4; i++ {
		if s[i] < 'A' || s[i] > 'Z' {
			return false
		}
	}
	return true
}

// ParseRunways parses OurAirports runways.csv.
func ParseRunways(r io.Reader, fn func(Runway) error) (ParseStats, error) {
	c, err := newCSV(r, "id", "airport_ref", "le_ident", "he_ident")
	if err != nil {
		return ParseStats{}, err
	}
	end := func(p string) RunwayEnd {
		e := RunwayEnd{Ident: c.s(p + "_ident")}
		lat, lon := c.f(p+"_latitude_deg"), c.f(p+"_longitude_deg")
		if validLatLon(lat, lon) {
			e.Lat, e.Lon = fptr(lat), fptr(lon)
		}
		e.ElevFt = c.i(p + "_elevation_ft")
		if h := c.f(p + "_heading_degt"); !math.IsNaN(h) && h >= 0 && h <= 360 {
			e.HeadingT = fptr(h)
		}
		e.DisplacedFt = c.i(p + "_displaced_threshold_ft")
		return e
	}
	return c.loop(func() (bool, error) {
		id, ok := c.id("id")
		apt, ok2 := c.id("airport_ref")
		if !ok || !ok2 {
			return false, nil
		}
		rw := Runway{ID: id, AirportID: apt, AirportIdent: c.s("airport_ident"),
			LengthFt: c.i("length_ft"), WidthFt: c.i("width_ft"), Surface: c.s("surface"),
			Lighted: c.bool("lighted"), Closed: c.bool("closed"), LE: end("le"), HE: end("he")}
		return true, fn(rw)
	})
}

// ParseNavaids parses OurAirports navaids.csv.
func ParseNavaids(r io.Reader, fn func(Navaid) error) (ParseStats, error) {
	c, err := newCSV(r, "id", "ident", "type", "latitude_deg", "longitude_deg")
	if err != nil {
		return ParseStats{}, err
	}
	return c.loop(func() (bool, error) {
		id, ok := c.id("id")
		n := Navaid{ID: id, Ident: c.s("ident"), Name: c.s("name"), Type: c.s("type"),
			Lat: c.f("latitude_deg"), Lon: c.f("longitude_deg")}
		if !ok || !validLatLon(n.Lat, n.Lon) {
			return false, nil
		}
		n.FreqKHz = c.i("frequency_khz")
		n.ElevFt = c.i("elevation_ft")
		n.Country = c.s("iso_country")
		n.DMEChannel = c.s("dme_channel")
		n.MagVarDeg = fptr(c.f("magnetic_variation_deg"))
		n.Usage = c.s("usagetype")
		n.Power = c.s("power")
		n.Airport = c.s("associated_airport")
		return true, fn(n)
	})
}

// ParseCountries parses OurAirports countries.csv.
func ParseCountries(r io.Reader, fn func(Country) error) (ParseStats, error) {
	c, err := newCSV(r, "code", "name")
	if err != nil {
		return ParseStats{}, err
	}
	return c.loop(func() (bool, error) {
		k := Country{Code: strings.ToUpper(c.s("code")), Name: c.s("name"), Continent: c.s("continent")}
		if k.Code == "" {
			return false, nil
		}
		return true, fn(k)
	})
}

// ParseRoutesCSV parses one standing-data routes file
// (header "Callsign,Code,Number,AirlineCode,AirportCodes", AirportCodes =
// ICAO codes joined with '-', e.g. "EGLL-KJFK" or "EGLL-OMDB-YSSY").
func ParseRoutesCSV(r io.Reader, fn func(RouteRecord) error) (ParseStats, error) {
	c, err := newCSV(r, "callsign", "airportcodes")
	if err != nil {
		return ParseStats{}, err
	}
	return c.loop(func() (bool, error) {
		rr := RouteRecord{Callsign: normCallsign(c.s("callsign")), Number: c.s("number")}
		rr.Airline = strings.ToUpper(c.s("airlinecode"))
		if rr.Airline == "" {
			rr.Airline = strings.ToUpper(c.s("code"))
		}
		rr.Codes = splitCodes(c.s("airportcodes"))
		if rr.Callsign == "" || len(rr.Codes) < 2 {
			return false, nil
		}
		return true, fn(rr)
	})
}

// splitCodes splits "EGLL-KJFK" into upper-case codes; nil when any code is
// empty or implausible.
func splitCodes(s string) []string {
	if s == "" || len(s) > 200 {
		return nil
	}
	parts := strings.Split(strings.ToUpper(s), "-")
	for _, p := range parts {
		if len(p) < 3 || len(p) > 8 || !isAlnum(p) {
			return nil
		}
	}
	return parts
}

func isAlnum(s string) bool {
	for i := 0; i < len(s); i++ {
		ch := s[i]
		if !(ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9') {
			return false
		}
	}
	return true
}

// normCallsign trims, upper-cases and validates a callsign (1..8 chars of
// A-Z 0-9); "" when invalid.
func normCallsign(s string) string {
	s = strings.ToUpper(strings.TrimSpace(s))
	if s == "" || len(s) > 8 || !isAlnum(s) {
		return ""
	}
	return s
}

// NormalizeCallsign is the exported form of the callsign normaliser used for
// route keys ("" when the input is not a valid callsign).
func NormalizeCallsign(s string) string { return normCallsign(s) }
