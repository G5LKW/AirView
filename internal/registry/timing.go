package registry

import (
	"context"
	"fmt"

	"airview/internal/model"
)

// Timing trust (H-mlatc, DESIGN §9.6 "trust/quarantine of new stations").
//
// With open admission anyone can connect a station, and a station whose
// timestamps are wrong (broken hardware, a wrong position, or on purpose)
// degrades every MLAT fix it takes part in. New stations therefore start
// in timing quarantine: their copies are kept out of the MLAT engine by
// the gate in internal/registry/trust, which compares their timestamps
// with those of trusted stations on ADS-B position messages; once the
// timing is proven the gate calls ProveTiming and the station joins MLAT.
//
// The admin overrides the automatic state with SetTiming: "trusted" (join
// MLAT without proof), "excluded" (never MLAT), "quarantine" (start the
// proof again) or "auto" (clear the overrides; a station that was never
// proven stays quarantined).
//
// Stations created before this existed carry no flag and are trusted.

// Timing states (TimingState, SetTiming).
const (
	TimingTrusted     = "trusted"
	TimingQuarantined = "quarantined"
	TimingExcluded    = "excluded"
	// TimingAuto (SetTiming only) clears the admin overrides.
	TimingAuto = "auto"
	// TimingRequarantine (SetTiming only) puts the station back into
	// quarantine.
	TimingRequarantine = "quarantine"
)

// Timing returns the station's timing state: TimingExcluded,
// TimingTrusted or TimingQuarantined.
func (s *Station) Timing() string {
	switch {
	case s.Flags&FlagTimingExcluded != 0:
		return TimingExcluded
	case s.Flags&FlagTimingTrusted != 0, s.Flags&FlagTimingQuarantine == 0:
		return TimingTrusted
	}
	return TimingQuarantined
}

// TimingOverride returns the admin override in force ("" = automatic).
func (s *Station) TimingOverride() string {
	switch {
	case s.Flags&FlagTimingExcluded != 0:
		return TimingExcluded
	case s.Flags&FlagTimingTrusted != 0:
		return TimingTrusted
	}
	return ""
}

// TimingState returns the timing state of station idx (ok false when it
// does not exist).
func (r *Registry) TimingState(idx model.StationIdx) (string, bool) {
	st, ok := r.Station(idx)
	if !ok {
		return "", false
	}
	return st.Timing(), true
}

// ProveTiming ends the quarantine of a station whose timing the gate has
// proven (internal/registry/trust). detail is a short JSON object for the
// audit log (no positions or addresses). A station not in quarantine is
// left unchanged.
func (r *Registry) ProveTiming(ctx context.Context, idx model.StationIdx, detail string) (Station, error) {
	changed := false
	st, err := r.Update(ctx, idx, func(s *Station) error {
		changed = s.Flags&FlagTimingQuarantine != 0
		s.Flags &^= FlagTimingQuarantine
		return nil
	})
	if err != nil || !changed {
		return st, err
	}
	if detail == "" {
		detail = "{}"
	}
	return st, r.Audit(ctx, SystemActor, "station.timing.proven", st.SID, detail)
}

// SetTiming applies an admin decision about a station's timing: one of
// TimingTrusted, TimingExcluded, TimingRequarantine or TimingAuto. It is
// audited.
func (r *Registry) SetTiming(ctx context.Context, actor Actor, idx model.StationIdx, mode string) (Station, error) {
	st, err := r.Update(ctx, idx, func(s *Station) error {
		switch mode {
		case TimingTrusted:
			s.Flags = s.Flags&^FlagTimingExcluded | FlagTimingTrusted
		case TimingExcluded:
			s.Flags = s.Flags&^FlagTimingTrusted | FlagTimingExcluded
		case TimingRequarantine:
			s.Flags = s.Flags&^(FlagTimingTrusted|FlagTimingExcluded) | FlagTimingQuarantine
		case TimingAuto:
			s.Flags &^= FlagTimingTrusted | FlagTimingExcluded
		default:
			return fmt.Errorf("%w: timing mode %q", ErrInvalid, mode)
		}
		return nil
	})
	if err != nil {
		return st, err
	}
	return st, r.Audit(ctx, actor, "station.timing", st.SID, fmt.Sprintf(`{"mode":%q,"state":%q}`, mode, st.Timing()))
}

// Quarantined lists the stations in timing quarantine (admin view).
func (r *Registry) Quarantined() []Station {
	var out []Station
	for _, st := range r.List(Filter{}) {
		if st.Timing() == TimingQuarantined {
			out = append(out, st)
		}
	}
	return out
}
