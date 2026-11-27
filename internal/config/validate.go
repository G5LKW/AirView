package config

import (
	"fmt"
	"math"
	"net/netip"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"airview/internal/model"
)

// Limits applied by Validate.
const (
	MaxAntennas   = 16
	MaxFeeds      = 32
	MaxRangeRings = 12
	maxIDLen      = 32
	maxNameLen    = 80
	maxNotesLen   = 2000
	maxHostLen    = 253
	maxURLLen     = 2048
	maxSecretLen  = 256
)

// antennaPalette supplies overlay colours for antennas that have none.
var antennaPalette = []string{
	"#38bdf8", "#f59e0b", "#34d399", "#f472b6", "#a78bfa", "#fb7185", "#facc15", "#22d3ee",
}

// defaultFeedPort is the conventional TCP port per feed type (Radarcape numbering
// for Beast/AVR, BaseStation for SBS, AirView hub for hub).
var defaultFeedPort = map[string]int{
	model.FeedBeast: 10003,
	model.FeedAVR:   10002,
	model.FeedSBS:   30003,
	model.FeedHub:   30154,
	// rtl_tcp (an RTL-SDR dongle; see model.FeedRTLTCP).
	model.FeedRTLTCP: model.DefaultRTLTCPPort,
}

type validator struct{ problems []string }

func (v *validator) note(format string, a ...any) {
	v.problems = append(v.problems, fmt.Sprintf(format, a...))
}

// Validate normalises cfg and clamps every setting into its legal range. It
// returns the corrected copy plus a human-readable list of what had to be
// changed (empty when cfg was already clean). It never panics, whatever the
// input, and it is idempotent: validating its own output changes nothing.
//
// Rules, in short: version is stamped; unknown enum values fall back to their
// default; ports must be 1-65535; lat is clamped to +-90 and lon wrapped to
// +-180 (non-finite coordinates clear "located"); antenna and feed IDs are
// sanitised to unique lower-case slugs ([a-z0-9_-], max 32 chars) and feed ->
// antenna references follow renamed IDs or are cleared when dangling; azimuth
// is wrapped to [0,360); beamwidths, gains, losses, heights, intervals and
// retention periods are clamped; text is trimmed, stripped of control
// characters and length-limited; a malformed admin password hash is replaced
// by an unusable one (remote admin access stays locked rather than opening
// up); nil lists become empty lists.
//
// Wire-only secret markers ("set", "clear", "plain:...") are not understood
// here: run [MergeSecrets] first (or use [ApplyUpdate]).
func Validate(cfg model.Config) (model.Config, []string) {
	v := &validator{}
	out := Clone(cfg)
	def := model.DefaultConfig()

	out.Version = model.ConfigVersion

	out.Mode = v.enum("mode", out.Mode, def.Mode,
		model.ModeStandalone, model.ModeRemoteData, model.ModeRemoteWeb, model.ModeAggregator)

	v.http(&out.HTTP, def.HTTP)
	v.receiver(&out.Receiver, def.Receiver)
	renamed := v.antennas(&out)
	v.feeds(&out, renamed)

	// NTP
	out.NTP.Server = v.hostPort("ntp.server", out.NTP.Server)
	out.NTP.IntervalSec = v.intRange("ntp.intervalSec", out.NTP.IntervalSec, 2, 86400, def.NTP.IntervalSec)
	if out.NTP.Enabled && out.NTP.Server == "" {
		out.NTP.Enabled = false
		v.note("ntp: no server configured, NTP sampling disabled")
	}

	// Weather
	out.Weather.IntervalMin = v.intRange("weather.intervalMin", out.Weather.IntervalMin, 5, 1440, def.Weather.IntervalMin)

	// Hub
	out.Hub.Port = v.port("hub.port", out.Hub.Port, def.Hub.Port)
	out.Hub.Password = v.secret("hub.password", out.Hub.Password)

	// Remote
	host, _, ok := splitHost(out.Remote.Host)
	if !ok {
		v.note("remote.host %q is not a valid host name or address; cleared", clip(out.Remote.Host))
		host = ""
	}
	out.Remote.Host = host
	out.Remote.DataPort = v.port("remote.dataPort", out.Remote.DataPort, def.Remote.DataPort)
	out.Remote.HTTPPort = v.port("remote.httpPort", out.Remote.HTTPPort, def.Remote.HTTPPort)
	out.Remote.Password = v.secret("remote.password", out.Remote.Password)
	if (out.Mode == model.ModeRemoteData || out.Mode == model.ModeRemoteWeb) && out.Remote.Host == "" {
		v.note("mode %q needs remote.host; falling back to %q", out.Mode, model.ModeStandalone)
		out.Mode = model.ModeStandalone
	}

	// Storage
	out.Storage.TrailIntervalSec = v.intRange("storage.trailIntervalSec", out.Storage.TrailIntervalSec, 1, 3600, def.Storage.TrailIntervalSec)
	out.Storage.TrailRetentionDays = v.intRange("storage.trailRetentionDays", out.Storage.TrailRetentionDays, 1, 3650, def.Storage.TrailRetentionDays)
	if out.Storage.FlightRetentionDays < 0 {
		v.note("storage.flightRetentionDays %d is negative; using %d", out.Storage.FlightRetentionDays, def.Storage.FlightRetentionDays)
		out.Storage.FlightRetentionDays = def.Storage.FlightRetentionDays
	} else if out.Storage.FlightRetentionDays > 36500 {
		v.note("storage.flightRetentionDays %d is too large; using 36500", out.Storage.FlightRetentionDays)
		out.Storage.FlightRetentionDays = 36500
	}
	out.Storage.MinuteRetentionDays = v.intRange("storage.minuteRetentionDays", out.Storage.MinuteRetentionDays, 1, 3650, def.Storage.MinuteRetentionDays)
	v.problems = append(v.problems, out.Storage.Normalize()...)

	// UI
	out.UI.Units = v.enum("ui.units", out.UI.Units, def.UI.Units, "aviation", "metric", "imperial")
	out.UI.TrailMinutes = v.intRange("ui.trailMinutes", out.UI.TrailMinutes, 1, 1440, def.UI.TrailMinutes)
	out.UI.RangeRingsKm = v.rangeRings(out.UI.RangeRingsKm)
	out.UI.AircraftTTLs = v.intRange("ui.aircraftTtlS", out.UI.AircraftTTLs, 5, 3600, def.UI.AircraftTTLs)
	out.UI.MapStyle = v.text("ui.mapStyle", out.UI.MapStyle, 64, false)
	if out.UI.MapStyle == "" {
		out.UI.MapStyle = def.UI.MapStyle
	}
	out.UI.ColorTrailsBy = v.enum("ui.colorTrailsBy", out.UI.ColorTrailsBy, def.UI.ColorTrailsBy,
		"altitude", "speed", "signal", "source")

	// Demo
	out.Demo.Aircraft = v.intRange("demo.aircraft", out.Demo.Aircraft, 1, 1000, def.Demo.Aircraft)

	// Prediction
	out.Prediction.CoastSec = v.intRange("prediction.coastSec", out.Prediction.CoastSec, 5, 120, def.Prediction.CoastSec)
	out.Prediction.PathSec = v.intRange("prediction.pathSec", out.Prediction.PathSec, 10, 900, def.Prediction.PathSec)

	// Terrain
	out.Terrain.RadiusKm = v.intRange("terrain.radiusKm", out.Terrain.RadiusKm, 50, 1000, def.Terrain.RadiusKm)
	if z := out.Terrain.Zoom; z != 0 && (z < 6 || z > 12) {
		v.note("terrain.zoom %d is outside 6..12; automatic zoom is used", z)
		out.Terrain.Zoom = 0
	}
	if out.Terrain.KFactor == 0 {
		out.Terrain.KFactor = def.Terrain.KFactor
	}
	out.Terrain.KFactor = v.floatRange("terrain.kFactor", out.Terrain.KFactor, 0.5, 10, def.Terrain.KFactor)

	// Navigation data
	out.NavData.PhotoContact = v.text("navData.photoContact", out.NavData.PhotoContact, 200, false)

	// Network (DESIGN §1)
	v.uplinks(&out)
	v.problems = append(v.problems, out.UplinkTuning.Normalize("uplinkTuning")...)
	v.problems = append(v.problems, out.Aggregator.Normalize()...)
	v.problems = append(v.problems, out.Update.Normalize("update")...)
	v.problems = append(v.problems, out.Locate.Normalize()...)
	v.problems = append(v.problems, out.SSR.Normalize()...)
	v.problems = append(v.problems, out.Process.Normalize()...)
	if out.HTTP.SSEGzipMax < 0 || out.HTTP.SSEGzipMax > 100000 {
		v.note("http.sseGzipMax %d is out of range 0..100000; automatic is used", out.HTTP.SSEGzipMax)
		out.HTTP.SSEGzipMax = 0
	}
	out.Aggregator.OutputToken = v.secret("aggregator.outputToken", out.Aggregator.OutputToken)
	out.Aggregator.Name = v.text("aggregator.name", out.Aggregator.Name, maxNameLen, false)
	if out.Aggregator.Name == "" {
		out.Aggregator.Name = model.DefaultAggregator().Name
	}
	if ph := out.Aggregator.PublicHost; ph != "" {
		host, port, ok := splitHost(ph)
		if !ok || port != 0 {
			v.note("aggregator.publicHost %q is not a plain host name or address; cleared", clip(ph))
			host = ""
		}
		out.Aggregator.PublicHost = host
	}
	v.aggregatorPorts(&out)

	return out, v.problems
}

// aggregatorPorts checks the aggregator's listener ports against the web
// and hub ports (one TCP port namespace, SO-3): a feeder listener that
// collides with http.port or hub.port is switched off, as for collisions
// between the feeder listeners themselves (model.Aggregator.Normalize).
func (v *validator) aggregatorPorts(c *model.Config) {
	if c.Mode != model.ModeAggregator {
		return
	}
	a := &c.Aggregator
	used := map[int]string{}
	if c.HTTP.Port > 0 {
		used[c.HTTP.Port] = "http.port"
	}
	if c.Hub.Enabled && c.Hub.Port > 0 {
		if other, dup := used[c.Hub.Port]; dup {
			v.note("hub.port %d is already used by %s", c.Hub.Port, other)
		} else {
			used[c.Hub.Port] = "hub.port"
		}
	}
	for _, p := range []struct {
		name string
		v    *int
	}{
		{"avfPort", &a.AVFPort}, {"beastPort", &a.BeastPort}, {"mlatPort", &a.MLATPort},
		{"sbsInLocalPort", &a.SBSInLocalPort}, {"beastOutPort", &a.BeastOutPort}, {"sbsOutPort", &a.SBSOutPort},
	} {
		if *p.v == 0 {
			continue
		}
		if other, dup := used[*p.v]; dup {
			v.note("aggregator.%s %d is already used by %s; the listener is off", p.name, *p.v, other)
			*p.v = 0
		}
	}
}

// MaxUplinks is the maximum number of configured uplinks.
const MaxUplinks = 16

// uplinks validates Config.Uplinks: model normalisation (ports, pins, unique
// IDs), host syntax, the enrol token as a secret, and Inputs checked against
// the feed IDs. An empty list becomes nil (the key is omitted from the file).
func (v *validator) uplinks(c *model.Config) {
	if len(c.Uplinks) > MaxUplinks {
		v.note("%d uplinks configured; only the first %d are kept", len(c.Uplinks), MaxUplinks)
		c.Uplinks = c.Uplinks[:MaxUplinks]
	}
	if len(c.Uplinks) == 0 {
		c.Uplinks = nil
		return
	}
	feedIDs := make(map[string]bool, len(c.Feeds))
	for _, f := range c.Feeds {
		feedIDs[f.ID] = true
	}
	for i := range c.Uplinks {
		u := &c.Uplinks[i]
		p := "uplinks[" + strconv.Itoa(i) + "]."
		host, port, ok := splitHost(u.Host)
		if !ok {
			v.note("%shost %q is not a valid host name or address; cleared", p, clip(u.Host))
			host, port = "", 0
		}
		u.Host = host
		if port != 0 && u.Port == model.DefaultAVFPort {
			u.Port = port // "host:port" typed into the host field
		}
		u.EnrolToken = v.secret(p+"enrolToken", u.EnrolToken)
		var inputs []string
		for _, in := range u.Inputs {
			id := Slug(in)
			switch {
			case !feedIDs[id]:
				v.note("%sinputs: %q does not match any feed id; dropped", p, clip(in))
			case slices.Contains(inputs, id):
			default:
				inputs = append(inputs, id)
			}
		}
		u.Inputs = inputs
	}
	v.problems = append(v.problems, model.NormalizeUplinks(c.Uplinks)...)
}

func (v *validator) http(h *model.HTTPConfig, def model.HTTPConfig) {
	h.Port = v.port("http.port", h.Port, def.Port)

	bind := strings.TrimSpace(h.BindAddr)
	switch strings.ToLower(bind) {
	case "", "*":
		bind = ""
	case "localhost":
		bind = "127.0.0.1"
	default:
		if addr, err := netip.ParseAddr(bind); err == nil {
			bind = addr.String()
		} else {
			v.note("http.bindAddr %q is not an IP address; listening on all interfaces", clip(h.BindAddr))
			bind = ""
		}
	}
	h.BindAddr = bind

	if h.AdminPasswordHash != "" && !validHash(h.AdminPasswordHash) {
		// Fail closed: never keep junk (possibly a plain password or a wire
		// marker) in the file, and never silently drop the protection.
		h.AdminPasswordHash = HashPassword(randomSecret())
		v.note("http.adminPasswordHash is malformed; remote admin access is locked until a new admin password is set from this computer")
	}
}

func (v *validator) receiver(r *model.Receiver, def model.Receiver) {
	r.Name = v.text("receiver.name", r.Name, maxNameLen, false)
	if r.Name == "" {
		r.Name = def.Name
	}
	r.Model = v.text("receiver.model", r.Model, maxNameLen, false)

	if !finite(r.Lat) || !finite(r.Lon) {
		v.note("receiver coordinates are not numbers; location cleared")
		r.Lat, r.Lon, r.Located = 0, 0, false
	}
	if r.Lat < -90 || r.Lat > 90 {
		v.note("receiver.lat %g is outside -90..90; clamped", r.Lat)
		r.Lat = math.Max(-90, math.Min(90, r.Lat))
	}
	if r.Lon < -180 || r.Lon > 180 {
		lon := math.Mod(r.Lon+180, 360)
		if lon < 0 {
			lon += 360
		}
		lon -= 180
		v.note("receiver.lon %g is outside -180..180; wrapped to %g", r.Lon, lon)
		r.Lon = lon
	}
	r.GroundElevM = v.floatRange("receiver.groundElevM", r.GroundElevM, -500, 9000, 0)
}

// antennas validates the antenna list and returns a map from every original
// antenna ID to its sanitised ID (for fixing feed references).
func (v *validator) antennas(c *model.Config) map[string]string {
	if len(c.Antennas) > MaxAntennas {
		v.note("%d antennas configured; only the first %d are kept", len(c.Antennas), MaxAntennas)
		c.Antennas = c.Antennas[:MaxAntennas]
	}
	if c.Antennas == nil {
		c.Antennas = []model.Antenna{}
	}
	renamed := make(map[string]string, len(c.Antennas))
	used := make(map[string]bool, len(c.Antennas))
	for i := range c.Antennas {
		a := &c.Antennas[i]
		p := "antennas[" + strconv.Itoa(i) + "]."

		a.Name = v.text(p+"name", a.Name, maxNameLen, false)
		orig := a.ID
		a.ID = v.uniqueID(p+"id", a.ID, a.Name, "antenna", used)
		if _, dup := renamed[orig]; !dup {
			renamed[orig] = a.ID
		}
		if a.Name == "" {
			a.Name = a.ID
		}

		a.Type = v.enum(p+"type", a.Type, model.AntOther,
			model.AntVertical, model.AntCollinear, model.AntYagi, model.AntPatch, model.AntOther)
		if a.Channel < -1 || a.Channel > 15 {
			v.note("%schannel %d is out of range; set to -1 (not applicable)", p, a.Channel)
			a.Channel = -1
		}
		a.GainDBi = v.floatRange(p+"gainDbi", a.GainDBi, -20, 40, 0)
		a.HeightAGLM = v.floatRange(p+"heightAglM", a.HeightAGLM, 0, 1000, 0)

		if !finite(a.AzimuthDeg) {
			v.note("%sazimuthDeg is not a number; set to 0", p)
			a.AzimuthDeg = 0
		} else if a.AzimuthDeg < 0 || a.AzimuthDeg >= 360 {
			az := math.Mod(a.AzimuthDeg, 360)
			if az < 0 {
				az += 360
			}
			if az >= 360 { // -1e-20 + 360 rounds to 360
				az = 0
			}
			if a.AzimuthDeg != 360 {
				v.note("%sazimuthDeg %g wrapped to %g", p, a.AzimuthDeg, az)
			}
			a.AzimuthDeg = az
		}
		a.TiltDeg = v.floatRange(p+"tiltDeg", a.TiltDeg, -90, 90, 0)
		a.HBeamwidthDeg = v.floatRange(p+"hBeamwidthDeg", a.HBeamwidthDeg, 0, 360, 0)
		a.VBeamwidthDeg = v.floatRange(p+"vBeamwidthDeg", a.VBeamwidthDeg, 0, 180, 0)
		a.FrontToBackDB = v.floatRange(p+"frontToBackDb", a.FrontToBackDB, 0, 60, 0)
		if a.Elements < 0 || a.Elements > 100 {
			v.note("%selements %d is out of range; clamped", p, a.Elements)
			a.Elements = max(0, min(100, a.Elements))
		}

		switch pol := strings.ToLower(strings.TrimSpace(a.Polarization)); pol {
		case "", "v", "vertical":
			a.Polarization = "vertical"
		case "h", "horizontal":
			a.Polarization = "horizontal"
		default:
			v.note("%spolarization %q is unknown; using \"vertical\"", p, clip(a.Polarization))
			a.Polarization = "vertical"
		}

		a.CableLossDB = v.floatRange(p+"cableLossDb", a.CableLossDB, 0, 60, 0)
		a.LNAGainDB = v.floatRange(p+"lnaGainDb", a.LNAGainDB, 0, 60, 0)
		a.LNANoiseFigDB = v.floatRange(p+"lnaNoiseFigDb", a.LNANoiseFigDB, 0, 30, 0)
		a.FilterLossDB = v.floatRange(p+"filterLossDb", a.FilterLossDB, 0, 30, 0)
		a.Notes = v.text(p+"notes", a.Notes, maxNotesLen, true)

		if col, ok := normColor(a.Color); ok {
			a.Color = col
		} else {
			if strings.TrimSpace(a.Color) != "" {
				v.note("%scolor %q is not a hex colour; a default is used", p, clip(a.Color))
			}
			a.Color = antennaPalette[i%len(antennaPalette)]
		}
	}
	return renamed
}

func (v *validator) feeds(c *model.Config, renamed map[string]string) {
	if len(c.Feeds) > MaxFeeds {
		v.note("%d feeds configured; only the first %d are kept", len(c.Feeds), MaxFeeds)
		c.Feeds = c.Feeds[:MaxFeeds]
	}
	if c.Feeds == nil {
		c.Feeds = []model.Feed{}
	}
	antIDs := make(map[string]bool, len(c.Antennas))
	for _, a := range c.Antennas {
		antIDs[a.ID] = true
	}
	used := make(map[string]bool, len(c.Feeds))
	for i := range c.Feeds {
		f := &c.Feeds[i]
		p := "feeds[" + strconv.Itoa(i) + "]."

		f.Name = v.text(p+"name", f.Name, maxNameLen, false)
		f.ID = v.uniqueID(p+"id", f.ID, f.Name, "feed", used)
		if f.Name == "" {
			f.Name = f.ID
		}

		typ := strings.ToLower(strings.TrimSpace(f.Type))
		switch typ {
		case model.FeedBeast, model.FeedAVR, model.FeedSBS, model.FeedJSON, model.FeedHub, model.FeedDemo, model.FeedRTLTCP:
			f.Type = typ
		default:
			v.note("%stype %q is unknown; set to %q and the feed disabled", p, clip(f.Type), model.FeedBeast)
			f.Type = model.FeedBeast
			f.Enabled = false
		}

		host, hostPort, ok := splitHost(f.Host)
		if !ok {
			v.note("%shost %q is not a valid host name or address; cleared", p, clip(f.Host))
			host, hostPort = "", 0
		}
		f.Host = host
		if hostPort != 0 && (f.Port < 1 || f.Port > 65535) {
			f.Port = hostPort // "host:port" typed into the host box
		}
		if defPort, tcp := defaultFeedPort[f.Type]; tcp {
			f.Port = v.port(p+"port", f.Port, defPort)
		} else if f.Port < 0 || f.Port > 65535 {
			v.note("%sport %d is out of range; cleared", p, f.Port)
			f.Port = 0
		}

		f.URL = v.feedURL(p+"url", f.URL)
		if f.Type == model.FeedRTLTCP {
			for _, note := range f.SDR.Normalize(p + "sdr") {
				v.note("%s", note)
			}
		} else {
			f.SDR = model.FeedSDR{}
		}
		if cc := strings.TrimSpace(f.ClockClass); cc != "" {
			if _, ok := model.ParseClockClass(cc); ok {
				f.ClockClass = cc
			} else {
				v.note("%sclockClass %q is unknown; detection is used", p, clip(f.ClockClass))
				f.ClockClass = ""
			}
		} else {
			f.ClockClass = ""
		}
		f.Notes = v.text(p+"notes", f.Notes, maxNotesLen, true)

		if f.AntennaID != "" {
			ref := f.AntennaID
			if id, ok := renamed[ref]; ok {
				f.AntennaID = id
			} else if id := Slug(ref); antIDs[id] {
				f.AntennaID = id
			} else {
				v.note("%santennaId %q does not match any antenna; cleared", p, clip(ref))
				f.AntennaID = ""
			}
		}
	}
}

// feedURL normalises the polling URL of a JSON feed: http/https only, a host
// is required, "host/path" gets "http://" prepended.
func (v *validator) feedURL(name, s string) string {
	raw := strings.TrimSpace(s)
	if raw == "" {
		return ""
	}
	if len(raw) > maxURLLen || strings.ContainsFunc(raw, func(r rune) bool { return unicode.IsControl(r) || unicode.IsSpace(r) }) {
		v.note("%s is not a valid URL; cleared", name)
		return ""
	}
	if !strings.Contains(raw, "://") {
		raw = "http://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" {
		v.note("%s %q is not a valid http(s) URL; cleared", name, clip(s))
		return ""
	}
	return u.String()
}

func (v *validator) rangeRings(in []int) []int {
	out := make([]int, 0, len(in))
	dropped := false
	for _, km := range in {
		if km < 1 || km > 5000 || slices.Contains(out, km) {
			dropped = true
			continue
		}
		if len(out) >= 4*MaxRangeRings { // bound the work on hostile input
			dropped = true
			break
		}
		out = append(out, km)
	}
	slices.Sort(out)
	if len(out) > MaxRangeRings {
		out = out[:MaxRangeRings]
		dropped = true
	}
	if dropped {
		v.note("ui.rangeRingsKm: duplicate, out-of-range (1-5000 km) or surplus rings removed")
	}
	return out
}

// uniqueID sanitises id to a slug (falling back to the name, then to
// fallback) and makes it unique within used by appending -2, -3, ...
func (v *validator) uniqueID(field, id, name, fallback string, used map[string]bool) string {
	s := Slug(id)
	if s == "" {
		s = Slug(name)
	}
	if s == "" {
		s = fallback
	}
	base := s
	for n := 2; used[s]; n++ {
		suffix := "-" + strconv.Itoa(n)
		b := base
		if len(b)+len(suffix) > maxIDLen {
			b = strings.TrimRight(b[:maxIDLen-len(suffix)], "-_")
		}
		s = b + suffix
	}
	used[s] = true
	if s != id && strings.TrimSpace(id) != "" {
		v.note("%s %q changed to %q (IDs are unique lower-case slugs)", field, clip(id), s)
	}
	return s
}

// Slug converts s to an ID: lower-case ASCII letters, digits, '-' and '_',
// at most 32 characters; other characters become '-' (runs collapsed, ends
// trimmed). Returns "" when nothing usable remains.
func Slug(s string) string {
	var b strings.Builder
	lastDash := true // suppress leading separators
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_':
			b.WriteRune(r)
			lastDash = false
		default:
			if !lastDash {
				b.WriteByte('-')
				lastDash = true
			}
		}
		if b.Len() >= maxIDLen {
			break
		}
	}
	return strings.Trim(b.String(), "-_")
}

func (v *validator) enum(name, val, def string, allowed ...string) string {
	s := strings.ToLower(strings.TrimSpace(val))
	if s == "" {
		return def
	}
	if slices.Contains(allowed, s) {
		return s
	}
	v.note("%s %q is unknown; using %q", name, clip(val), def)
	return def
}

// port accepts 1-65535; 0 (unset) silently becomes def, anything else is
// reported and replaced by def.
func (v *validator) port(name string, val, def int) int {
	if val >= 1 && val <= 65535 {
		return val
	}
	if val != 0 {
		v.note("%s %d is not a valid port (1-65535); using %d", name, val, def)
	}
	return def
}

// intRange clamps val into [lo,hi]; 0 (unset) silently becomes def.
func (v *validator) intRange(name string, val, lo, hi, def int) int {
	switch {
	case val == 0 && lo > 0:
		return def
	case val < lo:
		v.note("%s %d is below the minimum; using %d", name, val, lo)
		return lo
	case val > hi:
		v.note("%s %d is above the maximum; using %d", name, val, hi)
		return hi
	}
	return val
}

// floatRange clamps val into [lo,hi]; NaN/Inf become def.
func (v *validator) floatRange(name string, val, lo, hi, def float64) float64 {
	switch {
	case !finite(val):
		v.note("%s is not a number; using %g", name, def)
		return def
	case val < lo:
		v.note("%s %g is below the minimum; using %g", name, val, lo)
		return lo
	case val > hi:
		v.note("%s %g is above the maximum; using %g", name, val, hi)
		return hi
	}
	return val
}

// text cleans free text: valid UTF-8, no control characters (newlines and
// tabs survive when multiline), surrounding space trimmed, at most max runes.
func (v *validator) text(name, s string, max int, multiline bool) string {
	c := cleanText(s, max, multiline)
	if c != strings.TrimSpace(s) {
		v.note("%s contained invalid characters or was too long; cleaned", name)
	}
	return c
}

func cleanText(s string, max int, multiline bool) string {
	if !utf8.ValidString(s) {
		s = strings.ToValidUTF8(s, "")
	}
	s = strings.Map(func(r rune) rune {
		if multiline && (r == '\n' || r == '\t') {
			return r
		}
		if unicode.IsControl(r) || r == utf8.RuneError || r == ' ' || r == ' ' {
			return -1
		}
		return r
	}, s)
	s = strings.TrimSpace(s)
	if utf8.RuneCountInString(s) > max {
		n := 0
		for i := range s {
			if n == max {
				s = s[:i]
				break
			}
			n++
		}
		s = strings.TrimSpace(s)
	}
	return s
}

// secret keeps a password byte-exact apart from enforcing valid UTF-8 and a
// length limit.
func (v *validator) secret(name, s string) string {
	c := s
	if !utf8.ValidString(c) {
		c = strings.ToValidUTF8(c, "")
	}
	if len(c) > maxSecretLen {
		c = c[:maxSecretLen]
		for !utf8.ValidString(c) { // do not cut a rune in half
			c = c[:len(c)-1]
		}
	}
	if c != s {
		v.note("%s was too long or not valid text; shortened", name)
	}
	return c
}

// hostPort normalises a "host" or "host:port" setting (NTP server).
func (v *validator) hostPort(name, s string) string {
	host, port, ok := splitHost(s)
	if !ok {
		v.note("%s %q is not a valid host name or address; cleared", name, clip(s))
		return ""
	}
	if host == "" {
		return ""
	}
	if port == 0 {
		return host
	}
	if strings.Contains(host, ":") {
		return "[" + host + "]:" + strconv.Itoa(port)
	}
	return host + ":" + strconv.Itoa(port)
}

// splitHost normalises a host setting typed by a user. It strips a URL
// scheme, user info, path and IPv6 brackets and returns an embedded port
// (0 when there is none). ok is false when what remains cannot be a host name
// or IP address.
func splitHost(s string) (host string, port int, ok bool) {
	s = strings.TrimSpace(s)
	if i := strings.Index(s, "://"); i >= 0 {
		s = s[i+3:]
	}
	if i := strings.IndexAny(s, "/?#"); i >= 0 {
		s = s[:i]
	}
	if i := strings.LastIndex(s, "@"); i >= 0 {
		s = s[i+1:]
	}
	if s == "" {
		return "", 0, true
	}
	if len(s) > maxHostLen+8 {
		return "", 0, false
	}

	portStr := ""
	switch {
	case strings.HasPrefix(s, "["):
		end := strings.IndexByte(s, ']')
		if end < 0 {
			return "", 0, false
		}
		host = s[1:end]
		rest := s[end+1:]
		if rest != "" {
			if rest[0] != ':' {
				return "", 0, false
			}
			portStr = rest[1:]
		}
	case strings.Count(s, ":") == 1:
		host, portStr, _ = strings.Cut(s, ":")
	default:
		host = s
	}
	if portStr != "" {
		n, err := strconv.Atoi(portStr)
		if err != nil || n < 1 || n > 65535 {
			return "", 0, false
		}
		port = n
	}
	if host == "" || len(host) > maxHostLen {
		return "", 0, false
	}
	if strings.Contains(host, ":") {
		// Only an IPv6 literal (optionally with a zone) may contain colons.
		addr, err := netip.ParseAddr(host)
		if err != nil {
			return "", 0, false
		}
		return addr.String(), port, true
	}
	for _, r := range host {
		switch {
		case r == '.', r == '-', r == '_':
		case r < utf8.RuneSelf:
			if !(r >= 'a' && r <= 'z') && !(r >= 'A' && r <= 'Z') && !(r >= '0' && r <= '9') {
				return "", 0, false
			}
		case unicode.IsLetter(r) || unicode.IsDigit(r) || unicode.IsMark(r):
			// internationalised host names
		default:
			return "", 0, false
		}
	}
	return host, port, true
}

// normColor accepts #rgb, #rrggbb and #rrggbbaa and returns it lower-cased.
func normColor(s string) (string, bool) {
	s = strings.ToLower(strings.TrimSpace(s))
	if len(s) != 4 && len(s) != 7 && len(s) != 9 {
		return "", false
	}
	if s[0] != '#' {
		return "", false
	}
	for i := 1; i < len(s); i++ {
		c := s[i]
		if !(c >= '0' && c <= '9') && !(c >= 'a' && c <= 'f') {
			return "", false
		}
	}
	return s, true
}

func finite(f float64) bool { return !math.IsNaN(f) && !math.IsInf(f, 0) }

// clip shortens hostile input before it is echoed in a problem message.
func clip(s string) string {
	s = cleanText(s, 60, false)
	return s
}
