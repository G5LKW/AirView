package server

// shareLimits are the effective aggregator.shareLinks settings.
type shareLimits struct {
	Disabled    bool
	MaxPerOwner int
	MaxDays     int
	DefaultDays int
	CoarseLow   bool
}

// shareCfg reads aggregator.shareLinks from the effective configuration
// (zero = the code defaults).
func (s *Server) shareCfg() shareLimits {
	var l shareLimits
	if s.eng != nil {
		c := s.eng.EffectiveConfig().Aggregator.ShareLinks
		l.Disabled = c.Disabled
		l.MaxPerOwner, l.MaxDays, l.DefaultDays, l.CoarseLow = c.Effective()
		return l
	}
	l.MaxPerOwner, l.MaxDays, l.DefaultDays, l.CoarseLow = shareMaxPerOwner, shareMaxDays, shareDefaultDays, true
	return l
}

// shareGate answers 404 on the owner share routes when share links are
// switched off (aggregator.shareLinks.disabled).
func (s *Server) shareGate() error {
	if s.shareCfg().Disabled {
		return notFound("share links are switched off on this network")
	}
	return nil
}
