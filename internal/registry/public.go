package registry

import (
	"context"
	"database/sql"
	"fmt"
	"math"

	"airview/internal/model"
)

// RuralGuardMin is the number of located, approved stations a 1° cell needs
// before cell01 pins in it are shown at cell01 precision; below it they are
// shown as region (DESIGN §8.3 step 5).
const RuralGuardMin = 3

type cell struct{ lat, lon int64 }

func degCell(st *Station) (cell, bool) {
	if st == nil || !countsForGuard(st) {
		return cell{}, false
	}
	la, lo := cellIndex(st.Effective.Lat, st.Effective.Lon, 1)
	return cell{la, lo}, true
}

// countsForGuard: stations that make a 1° cell "populated". Only stations
// whose own pin is public count (PRIV-A3): otherwise the precision of a
// visible pin would tell how many hidden stations share its 1° cell.
func countsForGuard(st *Station) bool {
	return st.Located && !st.Deleting && st.Admission == model.AdmApproved &&
		st.Pin != model.PinHidden && model.ValidPin(st.Pin)
}

// Public-point hysteresis (PRIV-A4): a stored public point keeps its cell
// until the effective position is more than max(pinHystMinM, 3σ) outside
// that cell (σ capped at pinHystMaxM/3), so survey refinements around a
// grid line do not make the pin hop between cells and reveal the line.
const (
	pinHystMinM = 200.0
	pinHystMaxM = 2000.0
)

// outsideCellM is how far (lat, lon) lies outside cell (la, lo) of size
// degrees, in metres (0 inside).
func outsideCellM(lat, lon float64, la, lo int64, size float64) float64 {
	out := func(v, a, b float64) float64 { return max(a-v, v-b, 0) }
	dLat := out(lat, float64(la)*size, float64(la+1)*size) * 111_320
	lonW := math.Mod(lon+180, 360)
	if lonW < 0 {
		lonW += 360
	}
	lonW -= 180
	dLon := out(lonW, float64(lo)*size, float64(lo+1)*size) * 111_320 * math.Cos(lat*math.Pi/180)
	return math.Hypot(dLat, dLon)
}

// wantPublic computes the public point st should have given the number of
// guard stations in its 1° cell. ok=false means no public point.
func (r *Registry) wantPublic(st *Station, inCell int) (PublicPoint, cell, bool) {
	if st.Pin == model.PinHidden || !st.Located || st.Deleting || !model.ValidPin(st.Pin) {
		return PublicPoint{}, cell{}, false
	}
	eff := st.Pin
	if eff == model.PinCell01 && inCell < RuralGuardMin {
		eff = model.PinRegion
	}
	size := model.PinCellDeg(eff)
	la, lo := cellIndex(st.Effective.Lat, st.Effective.Lon, size)
	c := cell{la, lo}
	// Stability (§8.3 step 3): a stored point with the same effective
	// precision inside the same cell is kept as it is.
	if st.HasPublic && st.Public.Effective == eff && st.Public.Precision == st.Pin {
		pla, plo := cellIndex(st.Public.Lat, st.Public.Lon, size)
		if pla == la && plo == lo {
			return st.Public, c, true
		}
		margin := min(max(pinHystMinM, 3*st.Effective.SigmaM), pinHystMaxM)
		if outsideCellM(st.Effective.Lat, st.Effective.Lon, pla, plo, size) <= margin {
			return st.Public, cell{pla, plo}, true
		}
	}
	lat, lon := r.pinPoint(st.SID, eff, la, lo)
	lat, lon = avoidAxis(lat, st.Effective.Lat, la, size), avoidAxis(lon, st.Effective.Lon, lo, size)
	return PublicPoint{Precision: st.Pin, Effective: eff, Lat: lat, Lon: lon}, c, true
}

// refreshPublic recomputes the public point of next and, when the 1° cell
// population changed, of every station in the old and new 1° cells. It
// writes station_public inside tx and returns the station values to put in
// the cache (always including next).
func (r *Registry) refreshPublic(ctx context.Context, tx *sql.Tx, old, next *Station) ([]*Station, error) {
	oldCell, oldIn := degCell(old)
	newCell, newIn := degCell(next)
	cells := map[cell]bool{}
	if oldIn != newIn || oldCell != newCell {
		if oldIn {
			cells[oldCell] = true
		}
		if newIn {
			cells[newCell] = true
		}
	}
	// Population of the affected cells, with next replacing old.
	pop := map[cell]int{}
	var neigh []*Station
	if len(cells) > 0 || next.Located {
		r.cache.each(func(st *Station) {
			if st.Idx == next.Idx {
				return
			}
			if c, ok := degCell(st); ok {
				if c == newCell || cells[c] {
					pop[c]++
				}
				if cells[c] && st.Pin == model.PinCell01 {
					neigh = append(neigh, st)
				}
			}
		})
	}
	if newIn {
		pop[newCell]++
	}

	out := []*Station{}
	apply := func(st *Station) (*Station, error) {
		c, _ := degCell(st)
		pp, pc, ok := r.wantPublic(st, pop[c])
		if !ok {
			if st.HasPublic {
				if _, err := tx.ExecContext(ctx, `DELETE FROM station_public WHERE idx=?`, st.Idx); err != nil {
					return nil, fmt.Errorf("registry: public point: %w", err)
				}
			}
			n := *st
			n.Public, n.HasPublic = PublicPoint{}, false
			return &n, nil
		}
		if st.HasPublic && pp == st.Public {
			return st, nil
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO station_public(idx, precision, effective, cell_lat, cell_lon, pub_lat, pub_lon, computed)
VALUES(?,?,?,?,?,?,?,?) ON CONFLICT(idx) DO UPDATE SET precision=excluded.precision, effective=excluded.effective,
 cell_lat=excluded.cell_lat, cell_lon=excluded.cell_lon, pub_lat=excluded.pub_lat, pub_lon=excluded.pub_lon, computed=excluded.computed`,
			st.Idx, pp.Precision, pp.Effective, pc.lat, pc.lon, pp.Lat, pp.Lon, r.now().Unix()); err != nil {
			return nil, fmt.Errorf("registry: public point: %w", err)
		}
		n := *st
		n.Public, n.HasPublic = pp, true
		return &n, nil
	}
	n, err := apply(next)
	if err != nil {
		return nil, err
	}
	out = append(out, n)
	for _, st := range neigh {
		n, err := apply(st)
		if err != nil {
			return nil, err
		}
		if n != st {
			out = append(out, n)
		}
	}
	return out, nil
}

// pinAxisGuardDeg is the smallest distance, per axis, between a public pin
// and the exact coordinate: a random point inside the cell that happens to
// share the station's latitude or longitude (to about 200 m) would
// disclose that axis, so that axis is moved by half a cell inside the same
// cell. Deterministic, so the pin stays stable.
const pinAxisGuardDeg = 0.002

func avoidAxis(v, exact float64, idx int64, size float64) float64 {
	if math.Abs(v-exact) >= pinAxisGuardDeg || size < 4*pinAxisGuardDeg {
		return v
	}
	lo := float64(idx) * size
	u := (v - lo) / size
	u = math.Mod(u+0.5, 1)
	u = 0.01 + 0.98*u
	return lo + u*size
}
