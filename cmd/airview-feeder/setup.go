package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"regexp"
	"strconv"
	"strings"

	"airview/internal/config"
	"airview/internal/model"
)

// consentText is DESIGN §10.5, shown before timestamps are enabled for a
// station without a declared position, and in the setup help.
const consentText = "If your receiver sends timestamps, the network operator can determine its location precisely, whatever you enter. Other users and the public cannot. Choose 'No timestamps (no MLAT)' if you do not want this."

var nameRE = regexp.MustCompile(`^[A-Za-z0-9_-]{1,32}$`)

func hashPassword(pw string) string { return config.HashPassword(pw) }

// setupOptions are the setup flags (the same names as the Pi installer,
// DESIGN §12.2). Strings are "" when not given.
type setupOptions struct {
	server, pin, enrol, name, lat, lon, alt, altRef, agl, input, inputType, privacy string
	uplinkID, readsbPush, uiPassword                                                string
	noLocation, noTimestamps, timestamps, publicStats, showName, nonInteractive     bool
	resultsPort                                                                     int
	given                                                                           map[string]bool

	// Beast outputs ("also feed others", DESIGN §12.1 phase 5).
	beastOut, beastFormat, beastUUID string
	// RTL-SDR input through the user's own rtl_tcp (-input-type rtltcp).
	sdrGain, sdrRTLTCPPath                 string
	sdrPPM, sdrSampleRate, sdrDevice       int
	sdrBiasTee, sdrSpawn, sdrAcceptUnverAP bool
}

func cmdSetup(args []string, stdout, stderr io.Writer) int {
	var c common
	var o setupOptions
	fs := newFlagSet("setup", stderr)
	c.register(fs)
	fs.StringVar(&o.server, "server", "", "aggregator `host[:port]` (default port 30160)")
	fs.StringVar(&o.pin, "pin", "", "aggregator server pin `sha256/...` (from the aggregator's install command)")
	fs.StringVar(&o.enrol, "enrol", "", "enrol `token` from the aggregator operator")
	fs.StringVar(&o.name, "name", "", "station `name` ([A-Za-z0-9_-], at most 32)")
	fs.StringVar(&o.lat, "lat", "", "antenna latitude, decimal `degrees`")
	fs.StringVar(&o.lon, "lon", "", "antenna longitude, decimal `degrees`")
	fs.StringVar(&o.alt, "alt", "", "antenna altitude with unit, e.g. `120m` or 400ft")
	fs.StringVar(&o.altRef, "alt-ref", "", "altitude reference: `msl` (default) or hae")
	fs.StringVar(&o.agl, "agl", "", "antenna height above ground, e.g. `8m` or 25ft")
	fs.StringVar(&o.input, "input", "", "receiver: `auto` (default), or host[:port] of its Beast/AVR/SBS output")
	fs.StringVar(&o.inputType, "input-type", "", "input `type` for host[:port]: beast, avr or sbs (default: detect)")
	fs.StringVar(&o.privacy, "privacy", "", "public map pin: `hidden` (default), region or cell01")
	fs.StringVar(&o.uplinkID, "uplink-id", "", "`id` of the aggregator entry to create or change (default: the first)")
	fs.StringVar(&o.readsbPush, "readsb-push", "", "also push MLAT results into readsb at `host:port` (e.g. 127.0.0.1:30104; \"off\" to stop)")
	fs.StringVar(&o.uiPassword, "ui-password", "", "status page `password` (needed only for a LAN address)")
	fs.IntVar(&o.resultsPort, "results-port", -1, "local Beast `port` for MLAT results (0 = off, default 30105)")
	fs.BoolVar(&o.noLocation, "no-location", false, "do not declare a position (the network locates the station)")
	fs.BoolVar(&o.noTimestamps, "no-timestamps", false, "send no timestamps (no MLAT, no timing-based location)")
	fs.BoolVar(&o.timestamps, "timestamps", false, "send timestamps again (undo -no-timestamps)")
	fs.BoolVar(&o.publicStats, "public-stats", false, "publish non-locating statistics of the station")
	fs.BoolVar(&o.showName, "show-name", false, "show the station name publicly")
	fs.BoolVar(&o.nonInteractive, "non-interactive", false, "do not ask; use the options and the current values")
	registerExtraSetupFlags(fs, &o)
	if code := parseFlags(fs, args); code >= 0 {
		return code
	}
	o.given = map[string]bool{}
	fs.Visit(func(f *flag.Flag) { o.given[f.Name] = true })
	c.resolve()

	lc, err := loadConfig(loadOptions{File: c.config})
	if err != nil && lc.Exists {
		fmt.Fprintf(stderr, "airview-feeder: %v (fix or remove the file first)\n", err)
		return 1
	}
	cfg := lc.FileCfg
	var p prompter
	if !o.nonInteractive {
		p = prompter{in: bufio.NewReader(os.Stdin), out: stdout}
		fmt.Fprintf(stdout, "AirView Feeder setup (%s)\nPress Enter to keep the value in [brackets].\n\n", c.config)
	}
	if err := applySetup(&cfg, o, &p); err != nil {
		fmt.Fprintf(stderr, "airview-feeder: %v\n", err)
		return 2
	}
	for _, n := range normalizeFeeder(&cfg) {
		fmt.Fprintf(stdout, "note: %s\n", n)
	}
	if err := saveConfig(c.config, cfg); err != nil {
		fmt.Fprintf(stderr, "airview-feeder: cannot write %s: %v\n", c.config, err)
		return 1
	}
	fmt.Fprintf(stdout, "\nSaved %s.\n", c.config)
	fmt.Fprintf(stdout, "Status page:   %s\n", baseURL(cfg.UI.Addr))
	fmt.Fprintln(stdout, "Start or restart the feeder service, then run \"airview-feeder claim\" to link it to your account.")
	return 0
}

// prompter asks questions; a zero prompter (non-interactive) returns the
// defaults.
type prompter struct {
	in  *bufio.Reader
	out io.Writer
}

func (p *prompter) ask(q, def string) string {
	if p == nil || p.in == nil {
		return def
	}
	if def != "" {
		fmt.Fprintf(p.out, "%s [%s]: ", q, def)
	} else {
		fmt.Fprintf(p.out, "%s: ", q)
	}
	line, err := p.in.ReadString('\n')
	line = strings.TrimSpace(line)
	if line == "" || (err != nil && line == "") {
		return def
	}
	return line
}

func (p *prompter) yes(q string, def bool) bool {
	d := "n"
	if def {
		d = "y"
	}
	for {
		a := strings.ToLower(p.ask(q+" (y/n)", d))
		switch a {
		case "y", "yes":
			return true
		case "n", "no":
			return false
		}
	}
}

func (p *prompter) interactive() bool { return p != nil && p.in != nil }

// applySetup merges the options (and answers) into cfg.
func applySetup(cfg *model.FeederConfig, o setupOptions, p *prompter) error {
	// ---- aggregator ----
	ui := -1
	for i, u := range cfg.Uplinks {
		if (o.uplinkID == "" && !u.IsBeast()) || u.ID == o.uplinkID {
			ui = i
			break
		}
	}
	var up model.Uplink
	if ui >= 0 {
		up = cfg.Uplinks[ui]
	} else {
		up = model.DefaultUplink()
		up.ID = o.uplinkID
	}
	server := o.server
	if server == "" && up.Host != "" {
		server = up.Addr()
	}
	server = p.ask("Aggregator address (host or host:port)", server)
	if server != "" {
		h, port, err := splitHostPortDefault(server, model.DefaultAVFPort)
		if err != nil || h == "" {
			return fmt.Errorf("aggregator address %q: want host or host:port", server)
		}
		up.Host, up.Port = h, port
		if up.ID == "" {
			up.ID = slugFromHost(h)
		}
		pin := o.pin
		if pin == "" {
			pin = up.ServerPin
		}
		up.ServerPin = p.ask("Server pin (sha256/..., empty = trust public certificates)", pin)
		if up.ServerPin != "" && !strings.HasPrefix(up.ServerPin, "sha256/") {
			return fmt.Errorf("server pin %q must start with sha256/", up.ServerPin)
		}
		tok := o.enrol
		if tok == "" && p.interactive() {
			tok = p.ask("Enrol token (empty = keep / none)", "")
		}
		if tok != "" {
			up.EnrolToken = tok
		}
		privacy := o.privacy
		if privacy == "" {
			privacy = up.Pin
		}
		if p.interactive() && !o.given["privacy"] {
			fmt.Fprintf(p.out, "\n%s\n(Area is the value cell01.)\n", consentPin)
		}
		up.Pin = p.ask("Public map pin: hidden, region or cell01", privacy)
		switch up.Pin {
		case model.PinHidden, model.PinRegion, model.PinCell01:
		default:
			return fmt.Errorf("privacy %q: want hidden, region or cell01", up.Pin)
		}
		switch {
		case o.given["public-stats"]:
			up.PublicStats = o.publicStats
		case p.interactive():
			fmt.Fprintf(p.out, "\n%s\n", consentStats)
			up.PublicStats = p.yes("Publish the statistics?", up.PublicStats)
		}
		switch {
		case o.given["show-name"]:
			up.ShowName = o.showName
		case p.interactive():
			fmt.Fprintf(p.out, "\n%s\n", consentName)
			up.ShowName = p.yes("Show the name publicly?", up.ShowName)
		}
		switch {
		case o.noTimestamps:
			up.Timestamps = false
		case o.timestamps:
			up.Timestamps = true
		case p.interactive():
			fmt.Fprintf(p.out, "\n%s\n", consentText)
			up.Timestamps = p.yes("Send timestamps (needed for MLAT)?", up.Timestamps)
		}
		if ui >= 0 {
			cfg.Uplinks[ui] = up
		} else {
			cfg.Uplinks = append(cfg.Uplinks, up)
		}
	} else if o.given["pin"] || o.given["enrol"] {
		return errors.New("-pin and -enrol need an aggregator address (-server)")
	}

	// ---- receiver (the first one) ----
	if len(cfg.Receivers) == 0 {
		cfg.Receivers = []model.FeederReceiver{defaultReceiver(1)}
	}
	r := &cfg.Receivers[0]
	name := o.name
	if name == "" {
		name = r.Name
	}
	for {
		name = p.ask("Station name (letters, digits, - and _)", name)
		if nameRE.MatchString(name) {
			break
		}
		if !p.interactive() {
			return fmt.Errorf("name %q: use 1-32 of A-Z a-z 0-9 _ -", name)
		}
		fmt.Fprintln(p.out, "  use 1-32 letters, digits, - or _")
	}
	r.Name = name

	in := o.input
	if in == "" {
		in = r.Input.Type
		if in != model.FeedAuto && r.Input.Host != "" {
			in = fmt.Sprintf("%s:%d", r.Input.Host, r.Input.Port)
		}
	}
	in = p.ask("Receiver input: auto, or host:port of its Beast output", in)
	if err := setInput(r, in, o.inputType); err != nil {
		return err
	}
	if err := applySDRFlags(r, o); err != nil {
		return err
	}

	// position
	switch {
	case o.noLocation:
		r.Located, r.Lat, r.Lon = false, 0, 0
	case o.lat != "" || o.lon != "" || o.alt != "":
		if err := setPosition(r, o.lat, o.lon, o.alt, o.altRef); err != nil {
			return err
		}
	case p.interactive():
		def := ""
		if r.Located {
			def = strconv.FormatFloat(r.Lat, 'f', -1, 64)
		}
		lat := p.ask("Antenna latitude (decimal degrees; empty = let the network locate the station)", def)
		if lat == "" {
			r.Located = false
			fmt.Fprintf(p.out, "  %s\n", consentText)
			break
		}
		defLon, defAlt := "", ""
		if r.Located {
			defLon = strconv.FormatFloat(r.Lon, 'f', -1, 64)
			defAlt = strconv.FormatFloat(r.AltM, 'f', -1, 64) + "m"
		}
		lon := p.ask("Antenna longitude (decimal degrees, east positive)", defLon)
		alt := p.ask("Antenna altitude with unit (e.g. 120m or 400ft)", defAlt)
		ref := p.ask("Altitude reference: msl (sea level) or hae (GPS ellipsoid)", r.AltRef)
		if err := setPosition(r, lat, lon, alt, ref); err != nil {
			return err
		}
	}
	if o.altRef != "" && r.Located {
		r.AltRef = strings.ToLower(o.altRef)
	}
	agl := o.agl
	if agl == "" && p.interactive() {
		agl = p.ask("Antenna height above ground (e.g. 8m)", strconv.FormatFloat(r.AGLM, 'f', -1, 64)+"m")
	}
	if agl != "" {
		v, err := parseLength(agl, true)
		if err != nil {
			return fmt.Errorf("antenna height: %v", err)
		}
		r.AGLM = v
	}

	// ---- outputs and page ----
	if o.resultsPort >= 0 {
		cfg.Results.BeastPort = o.resultsPort
	}
	switch o.readsbPush {
	case "":
	case "off", "none":
		cfg.Results.ReadsbAddr = ""
	default:
		cfg.Results.ReadsbAddr = o.readsbPush
	}
	if o.uiPassword != "" {
		cfg.UI.PasswordHash = hashPassword(o.uiPassword)
	}
	return applyBeastOutFlags(cfg, o)
}

// setInput applies "auto" or "host[:port]".
func setInput(r *model.FeederReceiver, in, typ string) error {
	in = strings.TrimSpace(in)
	if in == "" || strings.EqualFold(in, model.FeedAuto) {
		r.Input.Type, r.Input.Host, r.Input.Port = model.FeedAuto, "", 0
		r.Input.Enabled = true
		return nil
	}
	h, port, err := splitHostPortDefault(in, 0)
	if err != nil || h == "" {
		return fmt.Errorf("input %q: want auto or host[:port]", in)
	}
	r.Input.Enabled = true
	switch strings.ToLower(typ) {
	case "":
		// port known: its type; otherwise detect at run time on that host
		switch {
		case port == 0:
			r.Input.Type, r.Input.Host, r.Input.Port = model.FeedAuto, h, 0
		default:
			r.Input.Type, r.Input.Host, r.Input.Port = model.FeedBeast, h, port
			for _, c := range candidates {
				if c.Port == port {
					r.Input.Type = c.Type
				}
			}
		}
	case model.FeedBeast, model.FeedAVR, model.FeedSBS, model.FeedRTLTCP:
		r.Input.Type, r.Input.Host, r.Input.Port = strings.ToLower(typ), h, port
		if port == 0 {
			r.Input.Port = defaultInputPort[r.Input.Type]
		}
	default:
		return fmt.Errorf("input type %q: want beast, avr, sbs or rtltcp", typ)
	}
	return nil
}

func setPosition(r *model.FeederReceiver, lat, lon, alt, ref string) error {
	la, err1 := strconv.ParseFloat(strings.TrimSpace(lat), 64)
	lo, err2 := strconv.ParseFloat(strings.TrimSpace(lon), 64)
	if err1 != nil || err2 != nil || math.Abs(la) > 90 || math.Abs(lo) > 180 || (la == 0 && lo == 0) {
		return fmt.Errorf("position %q, %q: want decimal degrees, latitude -90..90 and longitude -180..180", lat, lon)
	}
	if strings.TrimSpace(alt) == "" {
		return errors.New("altitude: give it with a unit, e.g. 120m or 400ft")
	}
	a, err := parseLength(alt, false)
	if err != nil {
		return fmt.Errorf("altitude: %v", err)
	}
	if a < -500 || a > 9000 {
		return fmt.Errorf("altitude %.0f m is out of range -500..9000 m", a)
	}
	ref = strings.ToLower(strings.TrimSpace(ref))
	switch ref {
	case "", model.AltRefMSL:
		ref = model.AltRefMSL
	case model.AltRefHAE:
	default:
		return fmt.Errorf("altitude reference %q: want msl or hae", ref)
	}
	r.Located, r.Lat, r.Lon, r.AltM, r.AltRef = true, la, lo, math.Round(a*10)/10, ref
	return nil
}

// parseLength parses "120m", "120 m", "400ft", "400 ft" into metres. A bare
// number is accepted only when bareMetres is set (antenna heights); the
// altitude needs its unit because feet and metres are both common.
func parseLength(s string, bareMetres bool) (float64, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	mult := 0.0
	switch {
	case strings.HasSuffix(s, "ft"):
		mult, s = 0.3048, strings.TrimSuffix(s, "ft")
	case strings.HasSuffix(s, "m"):
		mult, s = 1, strings.TrimSuffix(s, "m")
	case bareMetres:
		mult = 1
	default:
		return 0, fmt.Errorf("%q needs a unit: m or ft (e.g. 120m or 400ft)", s)
	}
	v, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil || math.IsNaN(v) || math.IsInf(v, 0) {
		return 0, fmt.Errorf("%q is not a number", s)
	}
	return v * mult, nil
}

// slugFromHost derives an uplink ID from a host name.
func slugFromHost(h string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(h) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '.' || r == '-' || r == ':' || r == '_':
			b.WriteByte('-')
		}
	}
	s := strings.Trim(b.String(), "-")
	if len(s) > 32 {
		s = strings.Trim(s[:32], "-")
	}
	if !model.ValidSlug(s) {
		return "aggregator"
	}
	return s
}

// exampleConfig is "airview-feeder config example": every setting with its
// default and a short explanation (JSON with comments; airview-feeder reads
// comments, so the file can be used as is).
func exampleConfig() string {
	return `// airview-feeder configuration (feeder.json). JSON; comments are allowed
// but are dropped when "setup", "config set" or the status page rewrite the
// file. Every setting can also be given as an environment variable
// (airview-feeder config env) or with -set path=value. Validate with
// "airview-feeder config check".
{
  "version": 1,
  "mode": "feeder",
  // Receivers attached to this computer; each is its own station.
  "receivers": [
    {
      "rxNo": 1,                 // receiver number 1..255, unique
      "name": "receiver",        // station name, [A-Za-z0-9_-], at most 32
      "input": {
        "id": "rx1",             // stable input id (slug)
        "enabled": true,
        // auto: probe 10003 (Radarcape), 30005 (readsb/dump1090 Beast),
        // 30002 (AVR) and 30003 (SBS, map only) on host (default 127.0.0.1).
        // Or beast | avr | sbs | json with host/port (json: url).
        "type": "auto",
        "host": "",
        "port": 0
      },
      "clockClass": "",          // "" = detect; gps_utc, gps_gpst, gps_emul, hw12, sdr12, sdr12_hires, sbs20, none
      "located": false,          // true = lat/lon/altM are the declared antenna position
      "lat": 0,
      "lon": 0,
      "altM": 0,                 // antenna altitude in metres
      "altRef": "msl",           // msl (sea level) or hae (WGS-84 ellipsoid)
      "aglM": 10                 // antenna height above ground, metres
    }
  ],
  // Aggregators to feed (AVF/1 over TLS). A hand-written entry needs only id and host.
  "uplinks": [
    {
      "id": "club",
      "host": "feed.example.org",
      "port": 30160,
      "serverPin": "",           // "sha256/<base64>" from the aggregator's install command; "" = public CA
      "enrolToken": "",          // from the operator (invite-only aggregators); a secret
      "sendLocation": true,      // send the declared position
      "shareGnss": false,        // forward the receiver's own GNSS position (Radarcape)
      "timestamps": true,        // false = no MLAT; the operator can locate a station that sends timestamps
      "returnMlat": true,        // receive MLAT results
      "spoolMb": -1,             // disk buffer while offline: -1 = auto (0 on SD cards), 0 = RAM only
      "pin": "hidden",           // public map pin: hidden | region | cell01
      "publicStats": false,
      "showName": false,
      "disabled": false
    }
  ],
  // Uplink client tunables (rarely changed).
  "uplinkTuning": {
    "ringSec": 600, "ringMb": 12, "backfillQueueMb": 24, "resumeWindowSec": 30,
    "backfillMinKBps": 64, "backoffMinSec": 1, "backoffMaxSec": 60, "livenessSec": 30, "spoolSegmentMb": 4
  },
  // Optional NTP reference used to estimate the host clock error.
  "ntp": { "enabled": false, "server": "", "intervalSec": 64 },
  // Local status page. A non-loopback address needs passwordHash
  // (set it with: airview-feeder config set ui.passwordHash=<password>).
  "ui": { "addr": "127.0.0.1:8099", "passwordHash": "" },
  // MLAT results: Beast with the MLAT magic timestamp on 127.0.0.1:beastPort
  // (0 = off); readsbAddr pushes them into a readsb Beast input (e.g. 127.0.0.1:30104).
  "results": { "beastPort": 30105, "readsbAddr": "" },
  "autoUpdate": true
}
`
}
