package analytics

import "math"

// Skyline fit of the endpoint term.
//
// Seen from the true position, low aircraft that are lost in mid-air are
// lost where they sink below the local skyline: on a smooth earth that is
// the radio horizon, with hills, buildings or trees it is some higher
// elevation angle that is constant within an azimuth sector but unknown.
// Without a terrain model the absolute "ends lie on the smooth horizon"
// rule is wrong wherever terrain blocks, and it pulls the estimate towards
// places from which the blocked ends happen to sit on the smooth horizon.
// A terrain model has the same problem on a smaller scale (trees,
// buildings, a coarse DEM). What stays true is that, from the right place, the ends of one sector
// line up at ONE elevation angle, while from a wrong place their angles
// spread (they are at different distances and heights). So per sector the
// skyline H is profiled out: the sector's endpoint log-likelihood ratio is
// maximised over H in [l2SkyMinDeg, l2SkyMaxDeg] (relative to the smooth
// horizon of the candidate), and heard aircraft below H - free are charged
// like horizon violations, so H cannot rise above traffic that was heard.

const (
	l2Sectors   = 36   // 10 degree azimuth sectors
	l2SkyMinDeg = -0.5 // the station may stand above its surroundings
	l2SkyMaxDeg = 3.0
	l2SkyShrink = 2.0 // pseudo-counts of the global margin density in a sector's reference
)

// sector returns the azimuth sector of q seen from the node.
func (nd *l2Node) sector(q *l2pt) int {
	e := nd.e[0]*q.x + nd.e[1]*q.y + nd.e[2]*q.z
	n := nd.n[0]*q.x + nd.n[1]*q.y + nd.n[2]*q.z
	az := math.Atan2(e, n)
	if az < 0 {
		az += 2 * math.Pi
	}
	s := int(az / (2 * math.Pi) * l2Sectors)
	if s >= l2Sectors {
		s = l2Sectors - 1
	}
	return s
}

func l2BinCentre(b int) float64 { return l2HistLo + (float64(b)+0.5)*l2HistW }

// l2SkylineLL returns the tempered endpoint log-likelihood ratio of a node
// with the skyline profiled per sector. secE / secH are the per-sector
// margin histograms of the session ends / heard low aircraft, glob the
// histogram of all heard low aircraft (nLow of them). It also returns the
// number of sectors with session ends and how many of them the fit moved
// off the model horizon (evidence of terrain the model does not know).
func l2SkylineLL(tun *l2Tuning, secE, secH, glob []float64, nLow int, nzb *[]int) (total float64, sectors, raised int) {
	es2 := tun.endSigmaDeg * tun.endSigmaDeg
	gn := 1 / (tun.endSigmaDeg * math.Sqrt(2*math.Pi))
	s2 := tun.violScaleDeg * tun.violScaleDeg
	hLo, hHi := l2HistBin(l2SkyMinDeg), l2HistBin(l2SkyMaxDeg)
	gScale := l2SkyShrink / float64(nLow)
	for s := 0; s < l2Sectors; s++ {
		e := secE[s*l2HistBins : (s+1)*l2HistBins]
		h := secH[s*l2HistBins : (s+1)*l2HistBins]
		bs := (*nzb)[:0]
		for b, c := range e {
			if c > 0 {
				bs = append(bs, b)
			}
		}
		*nzb = bs
		if len(bs) == 0 {
			continue
		}
		var nS float64
		for _, c := range h {
			nS += c
		}
		// the sector's reference density (per degree), shrunk towards the
		// global one so that sparse sectors stay sensible
		norm := 1 / ((nS + l2SkyShrink + 0.05*l2HistBins) * l2HistW)
		best, bestH := math.Inf(-1), 0.0
		for hb := hLo; hb <= hHi; hb++ {
			H := l2BinCentre(hb)
			var ll float64
			for _, b := range bs {
				u := l2BinCentre(b) - H
				other := (h[b] + gScale*glob[b] + 0.05) * norm
				ll += e[b] * math.Log(tun.endPi*gn*math.Exp(-0.5*u*u/es2)/other+(1-tun.endPi))
			}
			ll /= tun.tauEnd
			// heard aircraft below the raised skyline (beyond the slack)
			if H > 0 {
				var pen float64
				lim := H - tun.violFreeDeg
				for b := 0; b < l2HistBins; b++ {
					u := l2BinCentre(b)
					if u >= lim {
						break
					}
					if h[b] > 0 {
						d := (lim - u) - math.Max(0, -u) // only the part above the smooth horizon is new
						if d > 0 {
							pen += h[b] * math.Min(tun.violCap, 0.5*d*d/s2)
						}
					}
				}
				ll -= pen / tun.tauHorizon
			}
			if math.Abs(H) > 0.15 {
				ll -= tun.skyPen
			}
			if ll > best {
				best, bestH = ll, H
			}
		}
		total += best
		sectors++
		if math.Abs(bestH) > 0.15 {
			raised++
		}
	}
	return total, sectors, raised
}
