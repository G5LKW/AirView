package geoid

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
)

// cmScale is the storage resolution of grids built from text or PGM
// sources: 1 cm, so int16 covers ±327 m (EGM96 spans -107…+86 m).
const cmScale = 0.01

func toCM(v float64) (int16, error) {
	c := math.Round(v / cmScale)
	if math.IsNaN(c) || c < math.MinInt16 || c > math.MaxInt16 {
		return 0, fmt.Errorf("%w: undulation %v m out of range", ErrFormat, v)
	}
	return int16(c), nil
}

// ParseNGA parses NGA's ASCII geoid grid layout (WW15MGH.GRD for EGM96):
// a header line "south north west east dlat dlon" in degrees followed by
// the undulations in metres, row by row from north to south, each row
// from west to east, whitespace separated (line breaks are not
// significant). A final column that repeats the first one (east = west +
// 360) is dropped so the grid wraps cleanly.
func ParseNGA(r io.Reader) (*Grid, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	sc.Split(bufio.ScanWords)
	next := func() (float64, bool, error) {
		if !sc.Scan() {
			return 0, false, sc.Err()
		}
		v, err := strconv.ParseFloat(sc.Text(), 64)
		if err != nil {
			return 0, false, fmt.Errorf("%w: %q is not a number", ErrFormat, sc.Text())
		}
		return v, true, nil
	}
	var hdr [6]float64
	for i := range hdr {
		v, ok, err := next()
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, fmt.Errorf("%w: short header", ErrFormat)
		}
		hdr[i] = v
	}
	south, north, west, east, dlat, dlon := hdr[0], hdr[1], hdr[2], hdr[3], hdr[4], hdr[5]
	if !(dlat > 0 && dlon > 0 && north > south && east > west) || north > 90 || south < -90 || east-west > 360+1e-9 {
		return nil, fmt.Errorf("%w: bad header %v", ErrFormat, hdr)
	}
	nlat := int(math.Round((north-south)/dlat)) + 1
	nlonFile := int(math.Round((east-west)/dlon)) + 1
	if nlat < 2 || nlonFile < 2 || nlat*nlonFile > 50_000_000 {
		return nil, fmt.Errorf("%w: bad grid size %d×%d", ErrFormat, nlat, nlonFile)
	}
	dup := math.Abs(east-west-360) < 1e-9 // last column repeats the first
	nlon := nlonFile
	if dup {
		nlon--
	}
	data := make([]int16, nlat*nlon)
	for i := 0; i < nlat; i++ {
		for j := 0; j < nlonFile; j++ {
			v, ok, err := next()
			if err != nil {
				return nil, err
			}
			if !ok {
				return nil, fmt.Errorf("%w: data ends at row %d column %d of %d×%d", ErrFormat, i, j, nlat, nlonFile)
			}
			if j >= nlon {
				continue
			}
			c, err := toCM(v)
			if err != nil {
				return nil, err
			}
			data[i*nlon+j] = c
		}
	}
	if _, ok, _ := next(); ok {
		return nil, fmt.Errorf("%w: trailing data after %d×%d values", ErrFormat, nlat, nlonFile)
	}
	return NewGrid(Grid{NLat: nlat, NLon: nlon, Lat0: north, Lon0: west, DLat: dlat, DLon: dlon,
		Scale: cmScale, Data: data})
}

// ParsePGM parses a GeographicLib geoid PGM (binary "P5", 16-bit
// big-endian samples, header comments "# Offset <m>" and "# Scale <m>").
// The image covers latitudes +90 (first row) to -90 (last row) and
// longitudes 0 to 360-Δ from the first column, so it wraps in longitude.
func ParsePGM(r io.Reader) (*Grid, error) {
	br := bufio.NewReader(r)
	var fields []string
	offset, scale := math.NaN(), math.NaN()
	for len(fields) < 4 {
		line, err := br.ReadString('\n')
		if err != nil {
			return nil, fmt.Errorf("%w: PGM header: %v", ErrFormat, err)
		}
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "#") {
			f := strings.Fields(strings.TrimPrefix(line, "#"))
			if len(f) == 2 {
				v, err := strconv.ParseFloat(f[1], 64)
				if err == nil {
					switch f[0] {
					case "Offset":
						offset = v
					case "Scale":
						scale = v
					}
				}
			}
			continue
		}
		fields = append(fields, strings.Fields(line)...)
	}
	if fields[0] != "P5" {
		return nil, fmt.Errorf("%w: not a binary PGM", ErrFormat)
	}
	w, err1 := strconv.Atoi(fields[1])
	h, err2 := strconv.Atoi(fields[2])
	maxv, err3 := strconv.Atoi(fields[3])
	if err1 != nil || err2 != nil || err3 != nil || w < 2 || h < 2 || maxv != 65535 || w*h > 50_000_000 {
		return nil, fmt.Errorf("%w: unsupported PGM header %v", ErrFormat, fields)
	}
	if math.IsNaN(offset) || math.IsNaN(scale) || !(scale > 0) {
		return nil, fmt.Errorf("%w: PGM lacks Offset/Scale comments", ErrFormat)
	}
	raw := make([]byte, 2*w*h)
	if _, err := io.ReadFull(br, raw); err != nil {
		return nil, fmt.Errorf("%w: PGM data: %v", ErrFormat, err)
	}
	data := make([]int16, w*h)
	for i := range data {
		v := offset + scale*float64(binary.BigEndian.Uint16(raw[2*i:]))
		c, err := toCM(v)
		if err != nil {
			return nil, err
		}
		data[i] = c
	}
	return NewGrid(Grid{NLat: h, NLon: w, Lat0: 90, Lon0: 0, DLat: 180 / float64(h-1), DLon: 360 / float64(w),
		Scale: cmScale, Data: data})
}
