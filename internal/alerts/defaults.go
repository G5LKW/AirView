package alerts

func fp(v float64) *float64 { return &v }

// DefaultRules returns the shipped rules: emergencies, military and new
// all-time records are enabled; everything else is a disabled template.
func DefaultRules() []Rule {
	return []Rule{
		{ID: "builtin-emergency", Name: "Emergency", Builtin: true, Enabled: true, Severity: SeverityCritical,
			Description: "Emergency squawk 7500/7600/7700 or an ADS-B emergency state",
			Match: Cond{Op: "or", Conds: []Cond{
				{Type: CondSquawk, Values: []string{"7500", "7600", "7700"}},
				{Type: CondEmergency},
			}},
			CooldownSec: 1800, Message: "{rule}: {ident} squawk {squawk} at {alt} ft, {dist} km",
			Actions: Actions{Notify: true, Sound: true, Log: true, Highlight: "#ff3b30"}},
		{ID: "builtin-military", Name: "Military aircraft", Builtin: true, Enabled: true, Severity: SeverityInfo,
			Match: Cond{Type: CondMilitary}, CooldownSec: 3600,
			Actions: Actions{Notify: true, Log: true, Highlight: "#ffb020"}},
		{ID: "builtin-record", Name: "New all-time record", Builtin: true, Enabled: true, Severity: SeverityInfo,
			Match: Cond{Type: CondSystem, Values: []string{SysRecord}}, CooldownSec: 600,
			Actions: Actions{Notify: true, Log: true}},
		{ID: "builtin-feed-down", Name: "Feed down", Builtin: true, Severity: SeverityWarning,
			Match: Cond{Type: CondSystem, Values: []string{SysFeedDown, SysFeedUp}}, CooldownSec: 300,
			Actions: Actions{Notify: true, Sound: true, Log: true}},
		{ID: "builtin-ntp", Name: "NTP not synchronised", Builtin: true, Severity: SeverityWarning,
			Match: Cond{Type: CondSystem, Values: []string{SysNTPUnsync}}, CooldownSec: 3600,
			Actions: Actions{Notify: true, Log: true}},
		{ID: "builtin-range-collapse", Name: "Range collapse", Builtin: true, Severity: SeverityWarning,
			Description: "Reception range far below normal - antenna, cable or receiver problem?",
			Match:       Cond{Type: CondSystem, Values: []string{SysRangeCollapse, SysRangeRestored}}, CooldownSec: 3600,
			Actions: Actions{Notify: true, Sound: true, Log: true}},
		{ID: "builtin-new-airframe", Name: "New airframe", Builtin: true, Severity: SeverityInfo,
			Description: "An aircraft never seen before by this station",
			Match:       Cond{Type: CondNewAirframe}, CooldownSec: 86400,
			Actions: Actions{Notify: true, Log: true, Highlight: "#34c759"}},
		{ID: "builtin-watchlist", Name: "Watch-list aircraft", Builtin: true, Severity: SeverityWarning,
			Match: Cond{Type: CondWatchlist}, CooldownSec: 3600, Message: "{rule}: {ident} [{icao}] {note}",
			Actions: Actions{Notify: true, Sound: true, Log: true, Highlight: "#bf5af2"}},
		{ID: "builtin-overhead", Name: "Low overhead", Builtin: true, Severity: SeverityInfo,
			Description: "Below 3000 ft within 5 km of the receiver",
			Match: Cond{Op: "and", Conds: []Cond{
				{Type: CondDistance, Max: fp(5)},
				{Type: CondAltitude, Max: fp(3000)},
				{Type: CondOnGround, Value: bp(false)},
			}}, CooldownSec: 1800,
			Actions: Actions{Notify: true, Highlight: "#0a84ff"}},
		{ID: "builtin-unusual", Name: "Unusual category", Builtin: true, Severity: SeverityInfo,
			Description: "Gliders, balloons, parachutists, ultralights, UAVs, space vehicles (emitter category B1-B7)",
			Match:       Cond{Type: CondCategory, Values: []string{"B1", "B2", "B3", "B4", "B6", "B7"}}, CooldownSec: 3600,
			Actions: Actions{Notify: true, Log: true}},
	}
}

func bp(v bool) *bool { return &v }
