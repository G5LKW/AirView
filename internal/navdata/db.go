package navdata

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"airview/internal/geo"
)

// DatasetInfo describes what is currently stored for one dataset.
type DatasetInfo struct {
	Dataset      Dataset `json:"dataset"`
	Rows         int64   `json:"rows"`
	URL          string  `json:"url,omitempty"`
	Imported     int64   `json:"imported,omitempty"` // Unix ms of the last import, 0 = never
	Records      int     `json:"records"`
	Skipped      int     `json:"skipped"`
	Bytes        int64   `json:"bytes"`
	LastModified string  `json:"lastModified,omitempty"`
	ETag         string  `json:"-"`
	Source       Source  `json:"source"`
}

func (n *NavData) db() (*sql.DB, error) {
	if n.st == nil {
		return nil, ErrNoStore
	}
	return n.st.DB(), nil
}

func (n *NavData) writeMeta(ctx context.Context, dr DatasetResult) error {
	return n.st.Write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO navdata_meta(dataset, url, imported, records, skipped, bytes,
  last_modified, etag) VALUES(?,?,?,?,?,?,?,?)`, string(dr.Dataset), dr.URL, time.Now().Unix(), dr.Records, dr.Skipped,
			dr.Bytes, dr.LastModified, dr.ETag)
		return err
	})
}

func (n *NavData) meta(ctx context.Context, ds Dataset) (DatasetInfo, bool) {
	db, err := n.db()
	if err != nil {
		return DatasetInfo{}, false
	}
	m := DatasetInfo{Dataset: ds}
	err = db.QueryRowContext(ctx, `SELECT url, imported, records, skipped, bytes, last_modified, etag FROM navdata_meta
  WHERE dataset = ?`, string(ds)).Scan(&m.URL, &m.Imported, &m.Records, &m.Skipped, &m.Bytes, &m.LastModified, &m.ETag)
	if err != nil {
		return m, false
	}
	m.Imported *= 1000
	return m, true
}

// Datasets reports row counts and import metadata for every dataset.
func (n *NavData) Datasets(ctx context.Context) ([]DatasetInfo, error) {
	db, err := n.db()
	if err != nil {
		return nil, err
	}
	tables := map[Dataset]string{DatasetCountries: "navdata_country", DatasetAirports: "navdata_airport",
		DatasetRunways: "navdata_runway", DatasetNavaids: "navdata_navaid", DatasetRoutes: "navdata_route"}
	var out []DatasetInfo
	for _, ds := range AllDatasets {
		m, _ := n.meta(ctx, ds)
		m.Dataset = ds
		m.Source, _ = SourceFor(ds)
		if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+tables[ds]).Scan(&m.Rows); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, nil
}

const airportCols = `a.id, a.ident, a.type, a.size, a.name, a.lat, a.lon, a.elev_ft, a.continent, a.country,
  IFNULL(c.name, ''), a.region, a.municipality, a.scheduled, a.icao, a.iata, a.gps, a.local`

const airportFrom = ` FROM navdata_airport a LEFT JOIN navdata_country c ON c.code = a.country `

func scanAirport(sc interface{ Scan(...any) error }) (Airport, error) {
	var a Airport
	var elev sql.NullInt64
	var sched int
	err := sc.Scan(&a.ID, &a.Ident, &a.Type, &a.Size, &a.Name, &a.Lat, &a.Lon, &elev, &a.Continent, &a.Country,
		&a.CountryName, &a.Region, &a.Municipality, &sched, &a.ICAO, &a.IATA, &a.GPS, &a.Local)
	if elev.Valid {
		a.ElevFt = iptr(int(elev.Int64))
	}
	a.Scheduled = sched != 0
	return a, err
}

func (n *NavData) queryAirports(ctx context.Context, where string, args ...any) ([]Airport, error) {
	db, err := n.db()
	if err != nil {
		return nil, err
	}
	rows, err := db.QueryContext(ctx, `SELECT `+airportCols+airportFrom+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Airport
	for rows.Next() {
		a, err := scanAirport(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// BBox is a geographic box. W > E means the box crosses the antimeridian.
type BBox struct {
	S, W, N, E float64
}

// Valid reports whether the box has sane values.
func (b BBox) Valid() bool {
	for _, v := range []float64{b.S, b.W, b.N, b.E} {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return false
		}
	}
	return b.S >= -90 && b.N <= 90 && b.S <= b.N && b.W >= -180 && b.W <= 180 && b.E >= -180 && b.E <= 180
}

func (b BBox) where(prefix string) (string, []any) {
	if b.W <= b.E {
		return prefix + "lat BETWEEN ? AND ? AND " + prefix + "lon BETWEEN ? AND ?", []any{b.S, b.N, b.W, b.E}
	}
	return prefix + "lat BETWEEN ? AND ? AND (" + prefix + "lon >= ? OR " + prefix + "lon <= ?)", []any{b.S, b.N, b.W, b.E}
}

// BoxAround returns the bounding box of a circle (clamped at the poles).
func BoxAround(lat, lon, radiusKm float64) BBox {
	dLat := radiusKm / 111.195
	b := BBox{S: math.Max(-90, lat-dLat), N: math.Min(90, lat+dLat)}
	c := math.Cos(geo.Rad(math.Min(89.9, math.Max(math.Abs(b.S), math.Abs(b.N)))))
	dLon := radiusKm / (111.195 * c)
	if dLon >= 180 || b.N >= 89.9 || b.S <= -89.9 {
		b.W, b.E = -180, 180
		return b
	}
	b.W, b.E = geo.NormalizeLon(lon-dLon), geo.NormalizeLon(lon+dLon)
	if lon+dLon == 180 {
		b.E = 180
	}
	return b
}

// AirportQuery selects airports for map overlays.
type AirportQuery struct {
	BBox    BBox      `json:"bbox"`
	MinSize SizeClass `json:"minSize"` // default SizeSmall (closed airports never unless MinSize = SizeClosed)
	Limit   int       `json:"limit"`   // default 2000, max 10000; biggest airports first
	Runways bool      `json:"runways"` // attach runways
}

// AirportsInBBox returns airports inside q.BBox, largest first.
func (n *NavData) AirportsInBBox(ctx context.Context, q AirportQuery) ([]Airport, error) {
	if !q.BBox.Valid() {
		return nil, fmt.Errorf("%w: bounding box", ErrBadInput)
	}
	if q.Limit <= 0 {
		q.Limit = 2000
	}
	q.Limit = min(q.Limit, 10000)
	w, args := q.BBox.where("a.")
	args = append(args, int(q.MinSize), q.Limit)
	out, err := n.queryAirports(ctx, "WHERE "+w+" AND a.size >= ? ORDER BY a.size DESC, a.scheduled DESC, a.id LIMIT ?", args...)
	if err != nil || !q.Runways {
		return out, err
	}
	return out, n.attachRunways(ctx, out)
}

// AirportsNear returns airports within radiusKm of (lat, lon), nearest first.
func (n *NavData) AirportsNear(ctx context.Context, lat, lon, radiusKm float64, minSize SizeClass, limit int, runways bool) ([]Airport, error) {
	if !geo.ValidLatLon(lat, lon) || !(radiusKm > 0) || radiusKm > 5000 {
		return nil, fmt.Errorf("%w: position or radius", ErrBadInput)
	}
	if limit <= 0 {
		limit = 500
	}
	limit = min(limit, 10000)
	b := BoxAround(lat, lon, radiusKm)
	w, args := b.where("a.")
	args = append(args, int(minSize))
	all, err := n.queryAirports(ctx, "WHERE "+w+" AND a.size >= ?", args...)
	if err != nil {
		return nil, err
	}
	out := all[:0]
	for _, a := range all {
		d := geo.HaversineKm(lat, lon, a.Lat, a.Lon)
		if d <= radiusKm {
			a.DistKm = fptr(round(d, 2))
			out = append(out, a)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return *out[i].DistKm < *out[j].DistKm })
	if len(out) > limit {
		out = out[:limit]
	}
	if runways {
		return out, n.attachRunways(ctx, out)
	}
	return out, nil
}

// NearestAirport returns the nearest airport of at least minSize within maxKm.
func (n *NavData) NearestAirport(ctx context.Context, lat, lon float64, minSize SizeClass, maxKm float64) (Airport, bool, error) {
	if maxKm <= 0 {
		maxKm = 100
	}
	for r := math.Min(10, maxKm); ; r = math.Min(r*3, maxKm) {
		as, err := n.AirportsNear(ctx, lat, lon, r, minSize, 1, false)
		if err != nil {
			return Airport{}, false, err
		}
		if len(as) > 0 {
			return as[0], true, nil
		}
		if r >= maxKm {
			return Airport{}, false, nil
		}
	}
}

// AirportByCode looks an airport up by ICAO, ident, IATA or GPS code (in that
// order of preference; non-closed airports win). runways attaches runways.
func (n *NavData) AirportByCode(ctx context.Context, code string, runways bool) (Airport, bool, error) {
	code = strings.ToUpper(strings.TrimSpace(code))
	if code == "" || len(code) > 16 {
		return Airport{}, false, nil
	}
	as, err := n.queryAirports(ctx, `WHERE a.icao = ?1 OR a.ident = ?1 OR a.iata = ?1 OR a.gps = ?1
  ORDER BY (a.icao = ?1) DESC, (a.ident = ?1) DESC, (a.iata = ?1) DESC, (a.size >= 0) DESC, a.size DESC, a.scheduled DESC
  LIMIT 1`, code)
	if err != nil || len(as) == 0 {
		return Airport{}, false, err
	}
	if runways {
		if err := n.attachRunways(ctx, as); err != nil {
			return Airport{}, false, err
		}
	}
	return as[0], true, nil
}

// SearchAirports finds airports whose code, name or municipality matches q
// (case-insensitive substring; codes rank first).
func (n *NavData) SearchAirports(ctx context.Context, q string, limit int) ([]Airport, error) {
	q = strings.TrimSpace(q)
	if q == "" || len(q) > 64 {
		return nil, nil
	}
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	up := strings.ToUpper(q)
	like := "%" + strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(q) + "%"
	return n.queryAirports(ctx, `WHERE a.icao = ?1 OR a.iata = ?1 OR a.ident = ?1 OR a.gps = ?1
  OR a.name LIKE ?2 ESCAPE '\' OR a.municipality LIKE ?2 ESCAPE '\'
  ORDER BY (a.icao = ?1 OR a.iata = ?1 OR a.ident = ?1 OR a.gps = ?1) DESC, a.size DESC, a.scheduled DESC LIMIT ?3`,
		up, like, limit)
}

func (n *NavData) attachRunways(ctx context.Context, as []Airport) error {
	if len(as) == 0 {
		return nil
	}
	db, err := n.db()
	if err != nil {
		return err
	}
	byID := make(map[int64]int, len(as))
	for i := range as {
		byID[as[i].ID] = i
	}
	// Query in chunks to stay below SQLite's parameter limit.
	ids := make([]any, 0, len(as))
	for i := range as {
		ids = append(ids, as[i].ID)
	}
	for start := 0; start < len(ids); start += 500 {
		end := min(start+500, len(ids))
		chunk := ids[start:end]
		rws, err := queryRunways(ctx, db, `WHERE airport_id IN (`+strings.TrimSuffix(strings.Repeat("?,", len(chunk)), ",")+`) ORDER BY airport_id, id`, chunk...)
		if err != nil {
			return err
		}
		for _, r := range rws {
			if i, ok := byID[r.AirportID]; ok {
				as[i].Runways = append(as[i].Runways, r)
			}
		}
	}
	return nil
}

func queryRunways(ctx context.Context, db *sql.DB, where string, args ...any) ([]Runway, error) {
	rows, err := db.QueryContext(ctx, `SELECT id, airport_id, airport_ident, length_ft, width_ft, surface, lighted, closed,
  le_ident, le_lat, le_lon, le_elev, le_hdg, le_disp, he_ident, he_lat, he_lon, he_elev, he_hdg, he_disp
  FROM navdata_runway `+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Runway
	for rows.Next() {
		var r Runway
		var length, width, leElev, leDisp, heElev, heDisp sql.NullInt64
		var leLat, leLon, leHdg, heLat, heLon, heHdg sql.NullFloat64
		var lighted, closed int
		if err := rows.Scan(&r.ID, &r.AirportID, &r.AirportIdent, &length, &width, &r.Surface, &lighted, &closed,
			&r.LE.Ident, &leLat, &leLon, &leElev, &leHdg, &leDisp, &r.HE.Ident, &heLat, &heLon, &heElev, &heHdg, &heDisp); err != nil {
			return nil, err
		}
		r.LengthFt, r.WidthFt = nip(length), nip(width)
		r.Lighted, r.Closed = lighted != 0, closed != 0
		r.LE.Lat, r.LE.Lon, r.LE.HeadingT = nfp(leLat), nfp(leLon), nfp(leHdg)
		r.LE.ElevFt, r.LE.DisplacedFt = nip(leElev), nip(leDisp)
		r.HE.Lat, r.HE.Lon, r.HE.HeadingT = nfp(heLat), nfp(heLon), nfp(heHdg)
		r.HE.ElevFt, r.HE.DisplacedFt = nip(heElev), nip(heDisp)
		out = append(out, r)
	}
	return out, rows.Err()
}

func nip(v sql.NullInt64) *int {
	if !v.Valid {
		return nil
	}
	return iptr(int(v.Int64))
}

func nfp(v sql.NullFloat64) *float64 {
	if !v.Valid {
		return nil
	}
	return fptr(v.Float64)
}

// RunwayHeadings returns the usable runway ends of the airport with the given
// code (see RunwayEndInfo; closed runways excluded).
func (n *NavData) RunwayHeadings(ctx context.Context, code string) ([]RunwayEndInfo, error) {
	a, ok, err := n.AirportByCode(ctx, code, true)
	if err != nil || !ok {
		return nil, err
	}
	return RunwayEnds(&a, nil), nil
}

// NavaidQuery selects navaids for map overlays.
type NavaidQuery struct {
	BBox  BBox     `json:"bbox"`
	Types []string `json:"types,omitempty"` // e.g. VOR, VOR-DME, NDB; empty = all
	Limit int      `json:"limit"`           // default 2000, max 10000
}

// NavaidsInBBox returns navaids inside q.BBox.
func (n *NavData) NavaidsInBBox(ctx context.Context, q NavaidQuery) ([]Navaid, error) {
	if !q.BBox.Valid() {
		return nil, fmt.Errorf("%w: bounding box", ErrBadInput)
	}
	db, err := n.db()
	if err != nil {
		return nil, err
	}
	if q.Limit <= 0 {
		q.Limit = 2000
	}
	q.Limit = min(q.Limit, 10000)
	w, args := q.BBox.where("")
	if len(q.Types) > 0 && len(q.Types) <= 16 {
		w += " AND type IN (" + strings.TrimSuffix(strings.Repeat("?,", len(q.Types)), ",") + ")"
		for _, t := range q.Types {
			args = append(args, strings.ToUpper(t))
		}
	}
	args = append(args, q.Limit)
	rows, err := db.QueryContext(ctx, `SELECT id, ident, name, type, freq_khz, lat, lon, elev_ft, country, dme_channel, magvar,
  usage, power, airport FROM navdata_navaid WHERE `+w+` ORDER BY id LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Navaid
	for rows.Next() {
		var x Navaid
		var freq, elev sql.NullInt64
		var mv sql.NullFloat64
		if err := rows.Scan(&x.ID, &x.Ident, &x.Name, &x.Type, &freq, &x.Lat, &x.Lon, &elev, &x.Country, &x.DMEChannel, &mv,
			&x.Usage, &x.Power, &x.Airport); err != nil {
			return nil, err
		}
		x.FreqKHz, x.ElevFt, x.MagVarDeg = nip(freq), nip(elev), nfp(mv)
		out = append(out, x)
	}
	return out, rows.Err()
}

// Countries returns all stored countries ordered by code.
func (n *NavData) Countries(ctx context.Context) ([]Country, error) {
	db, err := n.db()
	if err != nil {
		return nil, err
	}
	rows, err := db.QueryContext(ctx, `SELECT code, name, continent FROM navdata_country ORDER BY code`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Country
	for rows.Next() {
		var c Country
		if err := rows.Scan(&c.Code, &c.Name, &c.Continent); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// StoredRoute returns the standing-data route of a callsign.
func (n *NavData) StoredRoute(ctx context.Context, callsign string) (RouteRecord, bool, error) {
	db, err := n.db()
	if err != nil {
		return RouteRecord{}, false, err
	}
	cs := normCallsign(callsign)
	if cs == "" {
		return RouteRecord{}, false, nil
	}
	rr := RouteRecord{Callsign: cs}
	var airports string
	err = db.QueryRowContext(ctx, `SELECT airline, number, airports FROM navdata_route WHERE callsign = ?`, cs).
		Scan(&rr.Airline, &rr.Number, &airports)
	if errors.Is(err, sql.ErrNoRows) {
		return RouteRecord{}, false, nil
	}
	if err != nil {
		return RouteRecord{}, false, err
	}
	rr.Codes = splitCodes(airports)
	return rr, len(rr.Codes) >= 2, nil
}
