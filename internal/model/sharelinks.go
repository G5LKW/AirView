package model

// AggShareLinks configures aggregator.shareLinks: the read-only share links
// station owners create for their own stations (internal/server
// api_share.go). Zero = default; the links are on by default.
type AggShareLinks struct {
	// Disabled switches share links off (default false). The share routes
	// then answer 404, existing links stop working and the web UI hides the
	// feature; the stored links are kept.
	Disabled bool `json:"disabled"`
	// MaxPerOwner is the number of active links one account may hold
	// (default 20, 1..1000).
	MaxPerOwner int `json:"maxPerOwner"`
	// MaxDays is the longest validity of a link in days (default 365,
	// 1..3650).
	MaxDays int `json:"maxDays"`
	// DefaultDays is the validity of a new link when the owner does not
	// choose one (default 30, 1..maxDays).
	DefaultDays int `json:"defaultDays"`
	// ExactLowDefault makes new links show low aircraft (below 1000 ft AGL
	// and on the ground) at their exact position unless the owner chooses
	// otherwise (default false: new links round them to the island-guard
	// grid). The owner can still choose per link.
	ExactLowDefault bool `json:"exactLowDefault"`
}

// Share link defaults (the zero values of AggShareLinks).
const (
	ShareLinksDefaultMaxPerOwner = 20
	ShareLinksDefaultMaxDays     = 365
	ShareLinksDefaultDays        = 30
)

// Effective returns the limits with the defaults filled in.
func (s AggShareLinks) Effective() (maxPerOwner, maxDays, defaultDays int, coarseLow bool) {
	maxPerOwner, maxDays, defaultDays = s.MaxPerOwner, s.MaxDays, s.DefaultDays
	if maxPerOwner <= 0 {
		maxPerOwner = ShareLinksDefaultMaxPerOwner
	}
	if maxDays <= 0 {
		maxDays = ShareLinksDefaultMaxDays
	}
	if defaultDays <= 0 {
		defaultDays = min(ShareLinksDefaultDays, maxDays)
	}
	if defaultDays > maxDays {
		defaultDays = maxDays
	}
	return maxPerOwner, maxDays, defaultDays, !s.ExactLowDefault
}

// normalizeShareLinks checks aggregator.shareLinks.
func (a *Aggregator) normalizeShareLinks(n *notes) {
	s := &a.ShareLinks
	p := "aggregator.shareLinks."
	zeroOrRangeI(n, p+"maxPerOwner", &s.MaxPerOwner, 1, 1000)
	zeroOrRangeI(n, p+"maxDays", &s.MaxDays, 1, 3650)
	zeroOrRangeI(n, p+"defaultDays", &s.DefaultDays, 1, 3650)
	maxDays := s.MaxDays
	if maxDays == 0 {
		maxDays = ShareLinksDefaultMaxDays
	}
	if s.DefaultDays > maxDays {
		n.addf("%sdefaultDays %d is above maxDays %d; maxDays is used", p, s.DefaultDays, maxDays)
		s.DefaultDays = maxDays
	}
}
