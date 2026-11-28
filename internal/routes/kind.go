package routes

// Flight kind from the VRS standing-data airline list (airlines/schema-01,
// CC0): per airline a positioning and a charter regular expression over the
// flight number (the callsign after the three-letter ICAO airline code).
// Patterns are written for .NET regular expressions; the ones Go's RE2 engine
// cannot compile are skipped and counted.

import (
	"context"
	"regexp"

	"airview/internal/navdata"
)

// Flight kinds (model.RouteShort.Kind).
const (
	KindPositioning = "positioning"
	KindCharter     = "charter"
)

type airlineKinds struct{ pos, chr *regexp.Regexp }

// airlineSource is implemented by *navdata.NavData.
type airlineSource interface {
	Airlines(ctx context.Context) ([]navdata.Airline, error)
}

// loadAirlines (re)loads and compiles the airline patterns.
func (s *Service) loadAirlines(ctx context.Context) {
	src, ok := s.o.Nav.(airlineSource)
	if !ok {
		return
	}
	list, err := src.Airlines(ctx)
	if err != nil {
		s.log.Debug("routes: airline list", "err", err)
		return
	}
	m := make(map[string]airlineKinds, len(list)/8)
	bad := 0
	compile := func(p string) *regexp.Regexp {
		if p == "" {
			return nil
		}
		re, err := regexp.Compile(p)
		if err != nil {
			bad++
			return nil
		}
		return re
	}
	for _, a := range list {
		if a.ICAO == "" || len(a.ICAO) != 3 || (a.Positioning == "" && a.Charter == "") {
			continue
		}
		k := airlineKinds{pos: compile(a.Positioning), chr: compile(a.Charter)}
		if k.pos != nil || k.chr != nil {
			m[a.ICAO] = k
		}
	}
	s.kindMu.Lock()
	s.kinds = m
	s.kindMu.Unlock()
	if bad > 0 {
		s.log.Debug("routes: airline patterns Go cannot compile were skipped", "n", bad)
	}
}

// kindOf returns the flight kind of an airline callsign ("" = scheduled or
// unknown). Positioning wins over charter.
func (s *Service) kindOf(cs string) string {
	if len(cs) < 4 {
		return ""
	}
	s.kindMu.RLock()
	k, ok := s.kinds[cs[:3]]
	s.kindMu.RUnlock()
	if !ok {
		return ""
	}
	num := cs[3:]
	switch {
	case k.pos != nil && k.pos.MatchString(num):
		return KindPositioning
	case k.chr != nil && k.chr.MatchString(num):
		return KindCharter
	}
	return ""
}
