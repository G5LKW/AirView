package server

// Large JPEG export of the all-time position heat map (internal/heatexport).

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"airview/internal/analytics"
	"airview/internal/heatexport"
)

func init() {
	routes = append(routes, Route{Method: "GET", Path: "/api/antenna/heat.jpg", Group: grpAntenna, Access: AccessAdmin, Raw: rawHeatJPEG, NoAuto: true, ContentType: "image/jpeg",
		Sample:  "/api/antenna/heat.jpg?w=1280&h=720",
		Summary: "The all-time position heat map as a JPEG, by default 7680 x 4320 (8K UHD): heat in the Live Map's colours on a plain background with a graticule, the range rings, the receiver, a scale bar and a caption. No map tiles are fetched.",
		Params: params(pAnt, Param{"w, h", "size in pixels, 640..16384 (default 7680 x 4320)"}, Param{"q", "JPEG quality 1..100 (default 90)"},
			Param{"light", "`1` = light background"}, Param{"download", "`1` = as an attachment"}),
		Returns: "`image/jpeg` (8K: about 1-3 MB, a few seconds, about 35 MB of memory); `404` when no position was stored yet; `429` while another export runs"})
}

// heatExportBusy allows one export at a time.
var heatExportBusy = make(chan struct{}, 1)

func rawHeatJPEG(s *Server, w http.ResponseWriter, r *http.Request) {
	ant, err := qAnt(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	num := func(name string, def, lo, hi int) (int, bool) {
		v := qStr(r, name)
		if v == "" {
			return def, true
		}
		n, err := strconv.Atoi(v)
		if err != nil || n < lo || n > hi {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("%s must be %d..%d", name, lo, hi))
			return 0, false
		}
		return n, true
	}
	wd, ok := num("w", 7680, 640, 16384)
	if !ok {
		return
	}
	ht, ok := num("h", 4320, 320, 16384)
	if !ok {
		return
	}
	q, ok := num("q", 90, 1, 100)
	if !ok {
		return
	}
	select {
	case heatExportBusy <- struct{}{}:
		defer func() { <-heatExportBusy }()
	default:
		writeError(w, http.StatusTooManyRequests, "another heat map export is running")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	g, err := s.eng.Analyzer().HeatGrid(ctx, ant, analytics.HeatOptions{MaxCells: 60000})
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	cells := make([]heatexport.Cell, len(g.Lat))
	for i := range g.Lat {
		cells[i] = heatexport.Cell{Lat: g.Lat[i], Lon: g.Lon[i], Count: g.Count[i]}
	}
	cfg := s.eng.EffectiveConfig()
	rx := cfg.Receiver
	name := rx.Name
	if name == "" {
		name = "AirView station"
	}
	o := heatexport.Options{Width: wd, Height: ht, Quality: q, CellDeg: g.CellDeg, Light: qStr(r, "light") == "1",
		RxOK: rx.Lat != 0 || rx.Lon != 0, RxLat: rx.Lat, RxLon: rx.Lon, RingsKm: cfg.UI.RangeRingsKm,
		Title:   "AirView heat map",
		Caption: fmt.Sprintf("%s · %d positions · all time to %s", name, g.Positions, time.Now().UTC().Format("2006-01-02"))}
	if ant != "" {
		o.Caption += " · antenna " + ant
	}
	h := w.Header()
	h.Set("Content-Type", "image/jpeg")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Content-Type-Options", "nosniff")
	if qStr(r, "download") == "1" {
		h.Set("Content-Disposition", `attachment; filename="airview-heatmap-`+time.Now().UTC().Format("20060102")+`.jpg"`)
	}
	// Render into the response: a failure before the first byte becomes an
	// error status, later ones only end the stream.
	ew := &errWriter{w: w}
	if err := heatexport.Render(ew, cells, o); err != nil && !ew.wrote {
		h.Del("Content-Disposition")
		if errors.Is(err, heatexport.ErrNoData) {
			writeError(w, http.StatusNotFound, err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
	}
}

type errWriter struct {
	w     http.ResponseWriter
	wrote bool
}

func (e *errWriter) Write(b []byte) (int, error) {
	e.wrote = true
	return e.w.Write(b)
}
