package server

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"airview/internal/analytics"
	"airview/internal/geo"
	"airview/internal/model"
	"airview/internal/radio"
	"airview/internal/store"
)

// rangeAnt parses the two parameters nearly every analysis takes.
func rangeAnt(r *http.Request) (analytics.Range, string, error) {
	rg, err := qRange(r)
	if err != nil {
		return rg, "", err
	}
	ant, err := qAnt(r)
	return rg, ant, err
}

func apiStatsOverview(s *Server, r *http.Request) (any, error) {
	ant, err := qAnt(r)
	if err != nil {
		return nil, err
	}
	s.freshAircraftRows(r.Context()) // lifetime totals are sums over the aircraft rows
	return s.eng.Analyzer().Overview(r.Context(), ant)
}

func apiStatsRates(s *Server, r *http.Request) (any, error) {
	rg, ant, err := rangeAnt(r)
	if err != nil {
		return nil, err
	}
	return s.eng.Analyzer().Traffic(r.Context(), rg, ant)
}

type dfReply struct {
	From    int64                 `json:"from"`
	To      int64                 `json:"to"`
	Ant     string                `json:"ant"`
	DF      []analytics.DFCount   `json:"df"`      // stored counts over the range
	Sources analytics.SourceShare `json:"sources"` //
	Frames  int64                 `json:"frames"`  // frames in the range
	CRCBad  int64                 `json:"crcBad"`
	// Live counters since the program started (all antennas).
	LiveDF  map[string]uint64 `json:"liveDf"`  // accepted frames per downlink format: "df0" .. "df_other"
	LiveBDS map[string]uint64 `json:"liveBds"` // identified Comm-B registers: "4,0", "5,0", "6,0", "4,4"
}

func apiStatsDF(s *Server, r *http.Request) (any, error) {
	rg, ant, err := rangeAnt(r)
	if err != nil {
		return nil, err
	}
	t, err := s.eng.Analyzer().Traffic(r.Context(), rg, ant)
	if err != nil {
		return nil, err
	}
	c := s.eng.Tracker().Counters()
	return dfReply{
		From: t.From, To: t.To, Ant: t.Ant, DF: t.DF, Sources: t.Sources,
		Frames: t.Totals.Frames, CRCBad: t.Totals.CRCBad, LiveDF: c.DF, LiveBDS: c.BDS,
	}, nil
}

func apiStatsHeatmap(s *Server, r *http.Request) (any, error) {
	rg, ant, err := rangeAnt(r)
	if err != nil {
		return nil, err
	}
	return s.eng.Analyzer().WeekHeatmap(r.Context(), rg, ant)
}

func apiStatsDaily(s *Server, r *http.Request) (any, error) {
	rg, ant, err := rangeAnt(r)
	if err != nil {
		return nil, err
	}
	return s.eng.Analyzer().Daily(r.Context(), rg, ant)
}

func apiStatsTop(s *Server, r *http.Request) (any, error) {
	rg, err := qRange(r)
	if err != nil {
		return nil, err
	}
	limit, err := qInt(r, "limit", 15, 1, 200)
	if err != nil {
		return nil, err
	}
	return s.eng.Analyzer().Top(r.Context(), rg, limit)
}

func apiStatsAltitudes(s *Server, r *http.Request) (any, error) {
	rg, ant, err := rangeAnt(r)
	if err != nil {
		return nil, err
	}
	return s.eng.Analyzer().AltitudeDistribution(r.Context(), rg, ant)
}

func apiStatsRecords(s *Server, r *http.Request) (any, error) {
	recs, err := s.eng.Analyzer().Records(r.Context())
	if err != nil {
		return nil, err
	}
	if recs == nil {
		recs = []store.Record{}
	}
	return map[string]any{"records": recs}, nil
}

func apiStatsEvents(s *Server, r *http.Request) (any, error) {
	limit, err := qInt(r, "limit", 100, 1, 1000)
	if err != nil {
		return nil, err
	}
	q := store.EventQuery{Kind: qStr(r, "kind"), Limit: limit}
	if v := qStr(r, "icao"); v != "" {
		if q.ICAO, err = parseICAOParam(v); err != nil {
			return nil, err
		}
	}
	since, err := qInt64(r, "since", 0)
	if err != nil {
		return nil, err
	}
	q.Since = since / 1000
	if q.BeforeID, err = qInt64(r, "before", 0); err != nil {
		return nil, err
	}
	ev, err := s.eng.Store().Events(r.Context(), q)
	if err != nil {
		return nil, err
	}
	if ev == nil {
		ev = []store.Event{}
	}
	return map[string]any{"events": ev}, nil
}

func apiStatsCounters(s *Server, r *http.Request) (any, error) {
	return s.eng.Tracker().Counters(), nil
}

type rangeReply struct {
	From  int64             `json:"from"`
	To    int64             `json:"to"`
	Ant   string            `json:"ant"`
	FarKm float64           `json:"farKm"` // threshold behind farCount
	Hours []store.RangeHour `json:"hours"`
}

func apiStatsRange(s *Server, r *http.Request) (any, error) {
	from, to, err := qSpanDefault(r, 7*24*3600e9)
	if err != nil {
		return nil, err
	}
	ant, err := qAnt(r)
	if err != nil {
		return nil, err
	}
	rows, err := s.eng.Analyzer().RangeHours(r.Context(), ant, from.Unix(), to.Unix())
	if err != nil {
		return nil, err
	}
	if rows == nil {
		rows = []store.RangeHour{}
	}
	if !qBool(r, "hist", false) {
		for i := range rows {
			rows[i].DistHist, rows[i].SectorMax = nil, nil
		}
	}
	return rangeReply{From: from.UnixMilli(), To: to.UnixMilli(), Ant: ant, FarKm: trackerFarKm, Hours: rows}, nil
}

func apiDayNight(s *Server, r *http.Request) (any, error) {
	rg, ant, err := rangeAnt(r)
	if err != nil {
		return nil, err
	}
	return s.eng.Analyzer().DayNight(r.Context(), rg, ant)
}

// ---------------------------------------------------------------------------
// Antenna
// ---------------------------------------------------------------------------

func (s *Server) coverage(r *http.Request) (*analytics.Coverage, error) {
	rg, ant, err := rangeAnt(r)
	if err != nil {
		return nil, err
	}
	return s.eng.Analyzer().Coverage(r.Context(), rg, ant)
}

// qBand parses band=<0..6>; -1 = not given.
func qBand(r *http.Request) (int, error) {
	if qStr(r, "band") == "" {
		return -1, nil
	}
	return qInt(r, "band", -1, 0, geo.AltBands-1)
}

func apiAntennaCoverage(s *Server, r *http.Request) (any, error) {
	cov, err := s.coverage(r)
	if err != nil {
		return nil, err
	}
	band, err := qBand(r)
	if err != nil {
		return nil, err
	}
	if band < 0 {
		return cov, nil
	}
	cp := *cov // the cached result is shared: never modify it
	cp.Bands = nil
	for _, b := range cov.Bands {
		if b.Band == band {
			cp.Bands = append(cp.Bands, b)
		}
	}
	return &cp, nil
}

func apiAntennaCoverageGeoJSON(s *Server, r *http.Request) (any, error) {
	rg, ant, err := rangeAnt(r)
	if err != nil {
		return nil, err
	}
	if v := qStr(r, "altBand"); v != "" {
		band, ok := analytics.ParseOutlineBand(v)
		if !ok {
			return nil, badRequest("parameter altBand: all, -1 (all altitudes together) or a band 0..%d", geo.AltBands-1)
		}
		kind := qStr(r, "kind")
		if kind != "" && kind != "max" && kind != "robust" {
			return nil, badRequest("parameter kind: max or robust")
		}
		return s.eng.Analyzer().CoverageOutline(r.Context(), rg, ant, analytics.OutlineOptions{Band: band, Kind: kind})
	}
	fc, err := s.eng.Analyzer().CoverageGeoJSON(r.Context(), rg, ant)
	if err != nil {
		return nil, err
	}
	band, err := qBand(r)
	if err != nil {
		return nil, err
	}
	kind := qStr(r, "kind")
	if kind != "" && kind != "max" && kind != "robust" {
		return nil, badRequest("parameter kind: max or robust")
	}
	if band < 0 && kind == "" && qStr(r, "band") == "" {
		return fc, nil
	}
	cp := *fc
	cp.Features = nil
	for _, f := range fc.Features {
		if kind != "" && f.Properties["kind"] != kind {
			continue
		}
		if band >= 0 {
			if fmt.Sprint(f.Properties["band"]) != strconv.Itoa(band) {
				continue
			}
		}
		cp.Features = append(cp.Features, f)
	}
	if cp.Features == nil {
		cp.Features = fc.Features[:0]
	}
	return &cp, nil
}

type horizonAntenna struct {
	ID         string            `json:"id"`
	Name       string            `json:"name"`
	HeightAGLM float64           `json:"heightAglM"`
	AMSLM      float64           `json:"amslM"` // feed point above mean sea level
	Table      []radio.BandRange `json:"table"` // line of sight per altitude band
}

type horizonBand struct {
	Band         int           `json:"band"`
	Label        string        `json:"label"`
	AltFt        float64       `json:"altFt"`
	HorizonKm    analytics.Num `json:"horizonKm"`    // line of sight at the top of the band
	HorizonTypKm analytics.Num `json:"horizonTypKm"` // at the representative altitude
	BestKm       analytics.Num `json:"bestKm"`       // farthest reception in the band
	BestBrg      analytics.Num `json:"bestBrg"`
	MeanPct      analytics.Num `json:"meanPct"` // robust range as % of the horizon, mean over all bearings
}

type horizonReply struct {
	From         int64            `json:"from"`
	To           int64            `json:"to"`
	Ant          string           `json:"ant"`
	Located      bool             `json:"located"`
	K            float64          `json:"k"` // effective earth radius factor used for the tables
	GroundElevM  float64          `json:"groundElevM"`
	AntennaAMSLM float64          `json:"antennaAmslM"` // height used for the measured comparison
	Antennas     []horizonAntenna `json:"antennas"`
	Bands        []horizonBand    `json:"bands"` // measured against the horizon
}

func apiAntennaHorizon(s *Server, r *http.Request) (any, error) {
	cov, err := s.coverage(r)
	if err != nil {
		return nil, err
	}
	k, err := qFloat(r, "k", geo.DefaultK)
	if err != nil {
		return nil, err
	}
	if k <= 0 || k > 1000 {
		return nil, badRequest("parameter k: must be between 0 and 1000")
	}
	cfg := s.eng.EffectiveConfig()
	out := horizonReply{
		From: cov.From, To: cov.To, Ant: cov.Ant, Located: cov.Located, K: k,
		GroundElevM: cfg.Receiver.GroundElevM, AntennaAMSLM: cov.AntennaAMSLM,
		Antennas: []horizonAntenna{}, Bands: []horizonBand{},
	}
	for _, a := range cfg.Antennas {
		h := radio.AntennaHeightAMSL(cfg.Receiver, a)
		out.Antennas = append(out.Antennas, horizonAntenna{
			ID: a.ID, Name: a.Name, HeightAGLM: a.HeightAGLM, AMSLM: h, Table: radio.HorizonTable(h, k),
		})
	}
	for _, b := range cov.Bands {
		out.Bands = append(out.Bands, horizonBand{
			Band: b.Band, Label: b.Label, AltFt: b.AltFt, HorizonKm: b.HorizonKm, HorizonTypKm: b.HorizonTypKm,
			BestKm: b.BestKm, BestBrg: b.BestBrg, MeanPct: b.MeanPct,
		})
	}
	return out, nil
}

type skylineReply struct {
	From         int64                   `json:"from"`
	To           int64                   `json:"to"`
	Ant          string                  `json:"ant"`
	Located      bool                    `json:"located"`
	BinDeg       float64                 `json:"binDeg"`
	BearingDeg   analytics.Nums          `json:"bearingDeg"`
	Skyline      analytics.Skyline       `json:"skyline"`
	Obstructions []analytics.Obstruction `json:"obstructions"`
}

func apiAntennaSkyline(s *Server, r *http.Request) (any, error) {
	cov, err := s.coverage(r)
	if err != nil {
		return nil, err
	}
	return skylineReply{
		From: cov.From, To: cov.To, Ant: cov.Ant, Located: cov.Located, BinDeg: cov.BinDeg,
		BearingDeg: cov.BearingDeg, Skyline: cov.Skyline, Obstructions: cov.Obstructions,
	}, nil
}

func apiAntennaHeat(s *Server, r *http.Request) (any, error) {
	ant, err := qAnt(r)
	if err != nil {
		return nil, err
	}
	var opt analytics.HeatOptions
	mc, err := qInt(r, "minCount", 1, 1, 1<<30)
	if err != nil {
		return nil, err
	}
	opt.MinCount = int64(mc)
	if opt.MaxCells, err = qInt(r, "maxCells", 20000, 100, 60000); err != nil {
		return nil, err
	}
	if bb := qStr(r, "bbox"); bb != "" {
		parts := strings.Split(bb, ",")
		if len(parts) != 4 {
			return nil, badRequest("parameter bbox: west,south,east,north")
		}
		var b [4]float64
		for i, p := range parts {
			f, err := parseFloat(p)
			if err != nil {
				return nil, badRequest("parameter bbox: west,south,east,north")
			}
			b[i] = f
		}
		if b[1] >= b[3] || b[1] < -90 || b[3] > 90 {
			return nil, badRequest("parameter bbox: west,south,east,north")
		}
		opt.Bounds = &b
	}
	return s.eng.Analyzer().HeatGrid(r.Context(), ant, opt)
}

func (s *Server) antennaReport(r *http.Request) (*analytics.AntennaReport, error) {
	rg, ant, err := rangeAnt(r)
	if err != nil {
		return nil, err
	}
	return s.eng.Analyzer().Antenna(r.Context(), rg, ant)
}

func apiAntennaReport(s *Server, r *http.Request) (any, error) { return s.antennaReport(r) }

// reportHead is the common header of the parts of the antenna report.
type reportHead struct {
	From    int64  `json:"from"`
	To      int64  `json:"to"`
	Ant     string `json:"ant"`
	Located bool   `json:"located"`
}

func headOf(rep *analytics.AntennaReport) reportHead {
	return reportHead{rep.From, rep.To, rep.Ant, rep.Located}
}

type signalReply struct {
	reportHead
	Signal *analytics.SignalDistance `json:"signal"` // null until there is data
}

func apiAntennaSignal(s *Server, r *http.Request) (any, error) {
	rep, err := s.antennaReport(r)
	if err != nil {
		return nil, err
	}
	return signalReply{headOf(rep), rep.Signal}, nil
}

type azimuthReply struct {
	reportHead
	Azimuth *analytics.AzimuthPattern `json:"azimuth"` // measured pattern, null until there is enough data
	Yagi    *analytics.YagiFit        `json:"yagi"`    // lobe fitted to it
}

func apiAntennaAzimuth(s *Server, r *http.Request) (any, error) {
	rep, err := s.antennaReport(r)
	if err != nil {
		return nil, err
	}
	return azimuthReply{headOf(rep), rep.Azimuth, rep.Yagi}, nil
}

type elevationReply struct {
	reportHead
	Elevation *analytics.ElevationPattern `json:"elevation"`
}

func apiAntennaElevation(s *Server, r *http.Request) (any, error) {
	rep, err := s.antennaReport(r)
	if err != nil {
		return nil, err
	}
	return elevationReply{headOf(rep), rep.Elevation}, nil
}

type fitReply struct {
	reportHead
	Yagi  *analytics.YagiFit `json:"yagi"`
	Notes []string           `json:"notes"`
}

func apiAntennaFit(s *Server, r *http.Request) (any, error) {
	rep, err := s.antennaReport(r)
	if err != nil {
		return nil, err
	}
	return fitReply{headOf(rep), rep.Yagi, rep.Notes}, nil
}

type linkBudgetReply struct {
	reportHead
	LinkBudget []analytics.LinkRow `json:"linkBudget"`
}

func apiAntennaLinkBudget(s *Server, r *http.Request) (any, error) {
	rep, err := s.antennaReport(r)
	if err != nil {
		return nil, err
	}
	return linkBudgetReply{headOf(rep), rep.LinkBudget}, nil
}

type patternReply struct {
	reportHead
	Antennas []analytics.AntennaModel  `json:"antennas"` // modelled (and, with one feed per antenna, measured) patterns
	Azimuth  *analytics.AzimuthPattern `json:"azimuth"`  // measured pattern of the selected antenna / combined
}

func apiAntennaPattern(s *Server, r *http.Request) (any, error) {
	rep, err := s.antennaReport(r)
	if err != nil {
		return nil, err
	}
	return patternReply{headOf(rep), rep.Antennas, rep.Azimuth}, nil
}

func apiAntennaScore(s *Server, r *http.Request) (any, error) {
	rg, ant, err := rangeAnt(r)
	if err != nil {
		return nil, err
	}
	return s.eng.Analyzer().Score(r.Context(), rg, ant)
}

// findAntenna returns the configured antenna with the given ID ("" = the
// first one).
func findAntenna(cfg model.Config, id string) (model.Antenna, error) {
	if len(cfg.Antennas) == 0 {
		return model.Antenna{}, notFound("no antenna is configured")
	}
	if id == "" {
		return cfg.Antennas[0], nil
	}
	for _, a := range cfg.Antennas {
		if a.ID == id {
			return a, nil
		}
	}
	return model.Antenna{}, notFound("unknown antenna %q", id)
}

type budgetReply struct {
	Antenna string       `json:"antenna"`
	AltFt   float64      `json:"altFt"`
	Budget  radio.Budget `json:"budget"`
	// The fixed part of the chain.
	SystemTempK    num    `json:"systemTempK"`
	SystemNFDB     num    `json:"systemNfDb"`
	NoiseFloorDBm  num    `json:"noiseFloorDbm"`
	SensitivityDBm num    `json:"sensitivityDbm"`
	MaxRangeKm     num    `json:"maxRangeKm"` // predicted maximum range on this bearing and altitude
	Limit          string `json:"limit"`      // "horizon" | "power" | "none"
}

func apiAntennaBudget(s *Server, r *http.Request) (any, error) {
	cfg := s.eng.EffectiveConfig()
	a, err := findAntenna(cfg, qStr(r, "ant"))
	if err != nil {
		return nil, err
	}
	az, err := qFloat(r, "az", a.AzimuthDeg)
	if err != nil {
		return nil, err
	}
	km, err := qFloat(r, "km", 200)
	if err != nil {
		return nil, err
	}
	altFt, err := qFloat(r, "altFt", 35000)
	if err != nil {
		return nil, err
	}
	if km <= 0 || km > 2000 || altFt < -1500 || altFt > 100000 {
		return nil, badRequest("km must be 0..2000 and altFt -1500..100000")
	}
	link := radio.NewLink(a, radio.AntennaHeightAMSL(cfg.Receiver, a), radio.LinkParams{})
	altM := geo.FeetToMetres(altFt)
	maxKm, limit := link.MaxRangeKm(az, altM)
	return budgetReply{
		Antenna: a.ID, AltFt: altFt, Budget: link.Budget(az, km, altM),
		SystemTempK: rnd(link.SystemTempK, 1), SystemNFDB: rnd(link.SystemNFDB, 2),
		NoiseFloorDBm: rnd(link.NoiseFloorDBm, 2), SensitivityDBm: rnd(link.SensitivityDBm, 2),
		MaxRangeKm: rnd(maxKm, 1), Limit: limit,
	}, nil
}

// ---------------------------------------------------------------------------
// Radio models
// ---------------------------------------------------------------------------

type cutJSON struct {
	AngleDeg []num `json:"angleDeg"`
	GainDBi  []num `json:"gainDbi"`
	RelDB    []num `json:"relDb"` // relative to the peak of the cut
	PeakDeg  num   `json:"peakDeg"`
	PeakDBi  num   `json:"peakDbi"`
	// BeamwidthDeg is the -3 dB width of the cut, null when it has none.
	BeamwidthDeg num `json:"beamwidthDeg"`
}

func cutOf(c radio.Cut) cutJSON {
	pa, pg := c.Peak()
	rel := make([]float64, len(c.GainDBi))
	for i, g := range c.GainDBi {
		rel[i] = g - pg
	}
	out := cutJSON{
		AngleDeg: nums(c.AngleDeg, 2), GainDBi: nums(c.GainDBi, 2), RelDB: nums(rel, 2),
		PeakDeg: rnd(pa, 2), PeakDBi: rnd(pg, 2), BeamwidthDeg: num(nan),
	}
	if w, ok := c.Beamwidth(3); ok {
		out.BeamwidthDeg = rnd(w, 1)
	}
	return out
}

type radioPattern struct {
	ID           string                 `json:"id"`
	Name         string                 `json:"name"`
	Type         string                 `json:"type"`
	Color        string                 `json:"color,omitempty"`
	Info         radio.PatternInfo      `json:"info"`    // resolved model parameters, "estimated" lists what was derived
	AMSLM        float64                `json:"amslM"`   // feed point height above mean sea level
	ElevDeg      float64                `json:"elevDeg"` // elevation of the azimuth cut
	AzDeg        float64                `json:"azDeg"`   // azimuth of the elevation cut
	Azimuth      cutJSON                `json:"azimuth"`
	Elevation    cutJSON                `json:"elevation"`
	RangeAltFt   float64                `json:"rangeAltFt"` // altitude of the range prediction
	RangeKm      []num                  `json:"rangeKm"`    // predicted maximum range per azimuth step
	RangeAzDeg   []num                  `json:"rangeAzDeg"` //
	HorizonKm    num                    `json:"horizonKm"`  // line of sight at rangeAltFt
	RangeTable   []radio.BandPrediction `json:"rangeTable"` // per altitude band, on boresight
	SystemNFDB   num                    `json:"systemNfDb"` //
	Sensitivity  num                    `json:"sensitivityDbm"`
	FrontEndLoss num                    `json:"frontEndLossDb"`
}

type radioPatternReply struct {
	StepDeg  float64        `json:"stepDeg"`
	Antennas []radioPattern `json:"antennas"`
}

func apiRadioPattern(s *Server, r *http.Request) (any, error) {
	cfg := s.eng.EffectiveConfig()
	ants := cfg.Antennas
	if id := qStr(r, "ant"); id != "" {
		a, err := findAntenna(cfg, id)
		if err != nil {
			return nil, err
		}
		ants = []model.Antenna{a}
	}
	step, err := qFloat(r, "step", 2)
	if err != nil {
		return nil, err
	}
	if step < 0.5 || step > 30 {
		return nil, badRequest("parameter step: 0.5 to 30 degrees")
	}
	elev, err := qFloat(r, "elev", 3)
	if err != nil {
		return nil, err
	}
	altFt, err := qFloat(r, "altFt", 35000)
	if err != nil {
		return nil, err
	}
	if elev < -90 || elev > 90 || altFt < 0 || altFt > 100000 {
		return nil, badRequest("elev must be -90..90 and altFt 0..100000")
	}
	out := radioPatternReply{StepDeg: step, Antennas: []radioPattern{}}
	for _, a := range ants {
		info := radio.Resolve(a)
		p := info.Pattern()
		az, err := qFloat(r, "az", info.AzimuthDeg)
		if err != nil {
			return nil, err
		}
		amsl := radio.AntennaHeightAMSL(cfg.Receiver, a)
		link := radio.NewLink(a, amsl, radio.LinkParams{})
		rc := link.RangeByAzimuth(geo.FeetToMetres(altFt), step)
		table := link.RangeTable(info.AzimuthDeg)
		if table == nil {
			table = []radio.BandPrediction{}
		}
		out.Antennas = append(out.Antennas, radioPattern{
			ID: a.ID, Name: a.Name, Type: a.Type, Color: a.Color, Info: info, AMSLM: amsl,
			ElevDeg: elev, AzDeg: az,
			Azimuth:    cutOf(radio.AzimuthCut(p, elev, step)),
			Elevation:  cutOf(radio.ElevationCutRange(p, az, -10, 90, min(step, 1))),
			RangeAltFt: altFt, RangeKm: nums(rc.RangeKm, 1), RangeAzDeg: nums(rc.AngleDeg, 2),
			HorizonKm: rnd(rc.HorizonKm, 1), RangeTable: table,
			SystemNFDB: rnd(link.SystemNFDB, 2), Sensitivity: rnd(link.SensitivityDBm, 2),
			FrontEndLoss: rnd(link.FrontEndLossDB, 2),
		})
	}
	return out, nil
}

func apiRadioTransponders(s *Server, r *http.Request) (any, error) {
	return map[string]any{"classes": radio.TransponderClasses(), "freqMHz": radio.FreqMHz}, nil
}
