package main

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"regexp"
	"strconv"
	"strings"
)

// values are the answers of the setup page or of feeder-setup.txt. Strings
// are as typed; validate normalises them.
type values struct {
	Server, Pin, Token, Name   string
	Lat, Lon, Alt, AltRef, AGL string
	NoLocation                 bool
	Input, Privacy             string
	Timestamps, AutoUpdate     bool
	ReadsbResults              bool
	Password, Password2        string
	// UpdateURL and ManifestKey come from the image's update.conf (baked in
	// by airview-piimage -manifest-key / -update-url) or feeder-setup.txt:
	// the release manifest base URL (install.sh --url; default https://
	// <aggregator host>) and the Ed25519 public key of the release signing
	// key (--manifest-key). Without a key automatic updates are impossible,
	// so AutoUpdate is switched off and the page says so (DLV-03).
	UpdateURL, ManifestKey string
}

// CanAutoUpdate reports whether signed automatic updates can be set up.
func (v values) CanAutoUpdate() bool { return v.ManifestKey != "" }

func defaults(host string) values {
	name := host
	if !nameRE.MatchString(name) {
		name = "airview-feeder"
	}
	return values{Name: name, AltRef: "msl", AGL: "10m", Input: "auto", Privacy: "hidden", Timestamps: true, AutoUpdate: true}
}

var (
	nameRE   = regexp.MustCompile(`^[A-Za-z0-9_-]{1,32}$`)
	hostRE   = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9.-]{0,251}[A-Za-z0-9])?$`)
	pinRE    = regexp.MustCompile(`^sha256/[A-Za-z0-9+/]{43}=$`)
	tokenRE  = regexp.MustCompile(`^[A-Za-z0-9._~+/=-]{4,255}$`)
	lengthRE = regexp.MustCompile(`^(-?[0-9]+(?:\.[0-9]+)?)\s*(m|ft)$`)
	inputRE  = regexp.MustCompile(`^[A-Za-z0-9._:\[\]-]{1,200}$`)
	mkeyRE   = regexp.MustCompile(`^[A-Za-z0-9+/]{43}=$`)
	urlRE    = regexp.MustCompile(`^https?://[A-Za-z0-9.-]+(:[0-9]{1,5})?(/[A-Za-z0-9._~%/+-]*)?$`)
)

// fieldError is a validation error for one form field.
type fieldError struct{ Field, Msg string }

func (e fieldError) Error() string { return e.Msg }

func parseLength(s string, min, max float64) (string, error) {
	m := lengthRE.FindStringSubmatch(strings.ToLower(strings.TrimSpace(s)))
	if m == nil {
		return "", errors.New("give a number with a unit, for example 120m or 400ft")
	}
	v, _ := strconv.ParseFloat(m[1], 64)
	meters := v
	if m[2] == "ft" {
		meters = v * 0.3048
	}
	if meters < min || meters > max {
		return "", fmt.Errorf("must be between %g m and %g m", min, max)
	}
	return m[1] + m[2], nil
}

// validate checks and normalises v. requirePassword is set for the web form
// (the status page must be reachable on the network afterwards).
func (v *values) validate(requirePassword bool) error {
	v.Server = strings.TrimSpace(v.Server)
	if v.Server == "" {
		return fieldError{"server", "Enter the aggregator address given by its operator, for example feed.example.net or feed.example.net:30160."}
	}
	if err := checkServer(v.Server); err != nil {
		return fieldError{"server", err.Error()}
	}
	v.Pin = strings.TrimSpace(v.Pin)
	if v.Pin != "" && !pinRE.MatchString(v.Pin) {
		return fieldError{"pin", "The server key looks like sha256/ followed by 44 characters; copy it from the aggregator's install instructions or leave it empty."}
	}
	v.Token = strings.TrimSpace(v.Token)
	if v.Token != "" && !tokenRE.MatchString(v.Token) {
		return fieldError{"token", "The enrol token is 4 to 255 characters of A-Z a-z 0-9 . _ ~ + / = -; copy it exactly as given."}
	}
	v.Name = strings.TrimSpace(v.Name)
	if !nameRE.MatchString(v.Name) {
		return fieldError{"name", "Use 1 to 32 letters, digits, - or _ for the station name."}
	}
	if !v.NoLocation {
		lat, err1 := strconv.ParseFloat(strings.TrimSpace(v.Lat), 64)
		lon, err2 := strconv.ParseFloat(strings.TrimSpace(v.Lon), 64)
		if err1 != nil || lat < -90 || lat > 90 {
			return fieldError{"lat", "Latitude must be a decimal number between -90 and 90 (or choose 'let the network locate me')."}
		}
		if err2 != nil || lon < -180 || lon > 180 {
			return fieldError{"lon", "Longitude must be a decimal number between -180 and 180."}
		}
		v.Lat = strconv.FormatFloat(lat, 'f', -1, 64)
		v.Lon = strconv.FormatFloat(lon, 'f', -1, 64)
		alt, err := parseLength(v.Alt, -500, 9000)
		if err != nil {
			return fieldError{"alt", "Antenna altitude: " + err.Error() + "."}
		}
		v.Alt = alt
		v.AltRef = strings.ToLower(strings.TrimSpace(v.AltRef))
		if v.AltRef == "" {
			v.AltRef = "msl"
		}
		if v.AltRef != "msl" && v.AltRef != "hae" {
			return fieldError{"altRef", "The altitude reference is msl (sea level) or hae (GPS ellipsoid)."}
		}
	} else {
		v.Lat, v.Lon, v.Alt = "", "", ""
	}
	if strings.TrimSpace(v.AGL) != "" {
		agl, err := parseLength(v.AGL, 0, 1000)
		if err != nil {
			return fieldError{"agl", "Antenna height above ground: " + err.Error() + "."}
		}
		v.AGL = agl
	}
	v.Input = strings.TrimSpace(v.Input)
	if v.Input == "" {
		v.Input = "auto"
	}
	if !inputRE.MatchString(v.Input) {
		return fieldError{"input", "The receiver input is 'auto' or host:port of its Beast output."}
	}
	switch v.Privacy = strings.ToLower(strings.TrimSpace(v.Privacy)); v.Privacy {
	case "":
		v.Privacy = "hidden"
	case "hidden", "region", "cell01":
	default:
		return fieldError{"privacy", "Choose Hidden, Region or Area for the public pin."}
	}
	v.UpdateURL = strings.TrimRight(strings.TrimSpace(v.UpdateURL), "/")
	if v.UpdateURL != "" && !urlRE.MatchString(v.UpdateURL) {
		return fieldError{"updateUrl", "UPDATE_URL must be an http(s) base URL such as https://feed.example.net."}
	}
	v.ManifestKey = strings.TrimSpace(v.ManifestKey)
	if v.ManifestKey != "" && !mkeyRE.MatchString(v.ManifestKey) {
		return fieldError{"manifestKey", "MANIFEST_KEY must be the base64 Ed25519 public key of the release signing key (44 characters ending in =)."}
	}
	if !v.CanAutoUpdate() {
		v.AutoUpdate = false
	}
	if requirePassword || v.Password != "" || v.Password2 != "" {
		if len(v.Password) < 8 || len(v.Password) > 200 {
			return fieldError{"password", "Choose a status page password of at least 8 characters."}
		}
		if requirePassword && v.Password != v.Password2 {
			return fieldError{"password2", "The two passwords differ."}
		}
	}
	return nil
}

func checkServer(s string) error {
	host, port := s, ""
	if h, p, err := net.SplitHostPort(s); err == nil {
		host, port = h, p
	} else if strings.HasPrefix(s, "[") {
		return errors.New("write an IPv6 address as [2001:db8::1]:30160")
	}
	if port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return errors.New("the port must be between 1 and 65535")
		}
	}
	if _, err := netip.ParseAddr(host); err == nil {
		return nil
	}
	if !hostRE.MatchString(host) || strings.Contains(host, "..") {
		return errors.New("that is not a valid host name or IP address")
	}
	return nil
}

// installerArgs builds the arguments of the feeder installer (deploy/feeder/pi/install.sh).
func (v values) installerArgs(binary string) []string {
	a := []string{"--binary", binary, "--server", v.Server}
	if v.Pin != "" {
		a = append(a, "--pin", v.Pin)
	}
	if v.Token != "" {
		a = append(a, "--token", v.Token)
	}
	a = append(a, "--name", v.Name)
	if v.NoLocation {
		a = append(a, "--no-location")
	} else {
		a = append(a, "--lat", v.Lat, "--lon", v.Lon, "--alt", v.Alt, "--alt-ref", v.AltRef)
	}
	if v.AGL != "" {
		a = append(a, "--agl", v.AGL)
	}
	a = append(a, "--input", v.Input, "--privacy", v.Privacy)
	if !v.Timestamps {
		a = append(a, "--no-timestamps")
	}
	// The release URL and key are written to feeder.json (update.manifestUrl
	// / update.keys) even with automatic updates off, so a manual
	// "airview-feeder-setup --update" is verified against the key.
	if v.UpdateURL != "" {
		a = append(a, "--url", v.UpdateURL)
	}
	if v.ManifestKey != "" {
		a = append(a, "--manifest-key", v.ManifestKey)
	}
	if !v.AutoUpdate || !v.CanAutoUpdate() {
		a = append(a, "--no-auto-update")
	}
	if v.ReadsbResults {
		a = append(a, "--readsb-results")
	}
	return append(a, "--non-interactive", "--no-start")
}

// redact hides the enrol token and the password hash in logged arguments.
func redact(args []string) []string {
	out := append([]string(nil), args...)
	for i := range out {
		if out[i] == "--token" && i+1 < len(out) {
			out[i+1] = "(redacted)"
		}
		if strings.HasPrefix(out[i], "ui.passwordHash=") {
			out[i] = "ui.passwordHash=(redacted)"
		}
	}
	return out
}

// parseSetupFile reads feeder-setup.txt: KEY=VALUE lines, # comments, CRLF
// tolerated (the file is edited on Windows), optional quotes. It is never
// executed. ok reports whether SERVER is set (unattended setup requested).
func parseSetupFile(b []byte, base values) (values, bool, error) {
	v := base
	ok := false
	sc := bufio.NewScanner(bytes.NewReader(b))
	lineNo := 0
	for sc.Scan() {
		lineNo++
		line := strings.TrimSpace(strings.TrimPrefix(sc.Text(), string(rune(0xFEFF))))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, val, found := strings.Cut(line, "=")
		if !found {
			return v, false, fmt.Errorf("line %d: expected KEY=value", lineNo)
		}
		k = strings.ToUpper(strings.TrimSpace(k))
		val = strings.TrimSpace(val)
		if len(val) >= 2 && (val[0] == '"' && val[len(val)-1] == '"' || val[0] == '\'' && val[len(val)-1] == '\'') {
			val = val[1 : len(val)-1]
		}
		yes := func() (bool, error) {
			switch strings.ToLower(val) {
			case "1", "yes", "true", "on", "y":
				return true, nil
			case "0", "no", "false", "off", "n", "":
				return false, nil
			}
			return false, fmt.Errorf("line %d: %s must be yes or no", lineNo, k)
		}
		var err error
		switch k {
		case "SERVER":
			v.Server = val
			ok = val != ""
		case "PIN", "SERVER_PIN":
			v.Pin = val
		case "TOKEN", "ENROL_TOKEN", "ENROL":
			v.Token = val
		case "NAME":
			if val != "" {
				v.Name = val
			}
		case "LAT":
			v.Lat = val
		case "LON":
			v.Lon = val
		case "ALT":
			v.Alt = val
		case "ALT_REF":
			v.AltRef = val
		case "AGL":
			v.AGL = val
		case "NO_LOCATION":
			v.NoLocation, err = yes()
		case "INPUT":
			v.Input = val
		case "PRIVACY":
			v.Privacy = val
		case "TIMESTAMPS":
			v.Timestamps, err = yes()
		case "AUTO_UPDATE":
			v.AutoUpdate, err = yes()
		case "READSB_RESULTS":
			v.ReadsbResults, err = yes()
		case "UPDATE_URL", "URL":
			v.UpdateURL = val
		case "MANIFEST_KEY":
			v.ManifestKey = val
		case "UI_PASSWORD", "PASSWORD":
			v.Password, v.Password2 = val, val
		default:
			return v, false, fmt.Errorf("line %d: unknown setting %s", lineNo, k)
		}
		if err != nil {
			return v, false, err
		}
	}
	if !v.NoLocation && v.Lat == "" && v.Lon == "" && v.Alt == "" {
		v.NoLocation = true
	}
	return v, ok, sc.Err()
}

// readSetupFile reads the file when it exists.
func readSetupFile(path string, base values) (values, bool, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return base, false, nil
	}
	if err != nil {
		return base, false, err
	}
	return parseSetupFile(b, base)
}
