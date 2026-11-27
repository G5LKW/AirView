package selfupdate

import (
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Settings is the file-configurable part of the updater (DESIGN §20: every
// setting in the configuration file, overridable by AIRVIEW_* environment
// variables, checked by "config check"). The owner of the configuration
// root (model.Config / model.FeederConfig) embeds it as "update".
//
// Self-update is opt-in for the main airview program (Enabled = false by
// default). airview-feeder maps its existing FeederConfig.AutoUpdate onto
// Enabled.
type Settings struct {
	// Enabled turns on the periodic automatic check-and-install. The
	// manual "update" command works regardless when a manifest URL and a
	// key are configured.
	Enabled bool `json:"enabled"`
	// Channel is "stable" (default) or "beta".
	Channel string `json:"channel,omitempty"`
	// ManifestURL is the URL of the signed manifest. "{channel}" is
	// replaced by the channel; without it, a beta channel inserts "beta/"
	// before the file name (https://h/feeder/manifest.json ->
	// https://h/feeder/beta/manifest.json). The signature is fetched from
	// the same URL + ".sig".
	ManifestURL string `json:"manifestUrl,omitempty"`
	// Keys are the trusted Ed25519 release public keys (base64 of the raw
	// 32 bytes). Several keys allow rotation. Never a private key.
	Keys []string `json:"keys,omitempty"`
	// CheckIntervalMin is the automatic check period in minutes (default
	// 1440 = daily; each check is jittered by ±25 % so a fleet of feeders
	// does not hit the server at the same moment). Minimum 15.
	CheckIntervalMin int `json:"checkIntervalMin,omitempty"`
	// HealthWindowS is how long a new version must stay healthy after the
	// restart before it is kept (default 60 s, 10-900).
	HealthWindowS int `json:"healthWindowS,omitempty"`
	// HealthURL is polled during the health window (optional; the program
	// wiring supplies its own default, e.g. http://127.0.0.1:8099/api/health
	// for the feeder or http://127.0.0.1:<port>/healthz for airview).
	HealthURL string `json:"healthUrl,omitempty"`
	// MaxDownloadMB bounds an artifact download (default 256).
	MaxDownloadMB int `json:"maxDownloadMB,omitempty"`
}

// Defaults.
const (
	DefaultCheckIntervalMin = 1440
	DefaultHealthWindowS    = 60
	DefaultMaxDownloadMB    = 256
)

// DefaultSettings returns the defaults (disabled, stable channel).
func DefaultSettings() Settings {
	return Settings{Channel: ChannelStable, CheckIntervalMin: DefaultCheckIntervalMin,
		HealthWindowS: DefaultHealthWindowS, MaxDownloadMB: DefaultMaxDownloadMB}
}

// Normalized returns s with zero values replaced by the defaults.
func (s Settings) Normalized() Settings {
	if s.Channel == "" {
		s.Channel = ChannelStable
	}
	s.Channel = strings.ToLower(strings.TrimSpace(s.Channel))
	if s.CheckIntervalMin == 0 {
		s.CheckIntervalMin = DefaultCheckIntervalMin
	}
	if s.HealthWindowS == 0 {
		s.HealthWindowS = DefaultHealthWindowS
	}
	if s.MaxDownloadMB == 0 {
		s.MaxDownloadMB = DefaultMaxDownloadMB
	}
	return s
}

// Check validates the settings ("config check"). Messages name the JSON
// path below the "update" block.
func (s Settings) Check() error {
	n := s.Normalized()
	var errs []error
	if !ValidChannel(n.Channel) {
		errs = append(errs, fmt.Errorf("update.channel: %q is not stable or beta", s.Channel))
	}
	if n.ManifestURL != "" {
		if _, err := checkURL(strings.ReplaceAll(n.ManifestURL, "{channel}", n.Channel)); err != nil {
			errs = append(errs, fmt.Errorf("update.manifestUrl: %v", err))
		}
	}
	if _, err := ParsePublicKeys(n.Keys); err != nil {
		errs = append(errs, fmt.Errorf("update.keys: %v", err))
	}
	if n.Enabled && (n.ManifestURL == "" || len(n.Keys) == 0) {
		errs = append(errs, errors.New("update.enabled needs update.manifestUrl and at least one update.keys entry"))
	}
	if n.CheckIntervalMin < 15 || n.CheckIntervalMin > 60*24*31 {
		errs = append(errs, fmt.Errorf("update.checkIntervalMin: %d outside 15..44640", s.CheckIntervalMin))
	}
	if n.HealthWindowS < 10 || n.HealthWindowS > 900 {
		errs = append(errs, fmt.Errorf("update.healthWindowS: %d outside 10..900", s.HealthWindowS))
	}
	if n.HealthURL != "" {
		if _, err := checkURL(n.HealthURL); err != nil {
			errs = append(errs, fmt.Errorf("update.healthUrl: %v", err))
		}
	}
	if n.MaxDownloadMB < 1 || n.MaxDownloadMB > 4096 {
		errs = append(errs, fmt.Errorf("update.maxDownloadMB: %d outside 1..4096", s.MaxDownloadMB))
	}
	return errors.Join(errs...)
}

func checkURL(s string) (*url.URL, error) {
	u, err := url.Parse(s)
	if err != nil {
		return nil, err
	}
	if (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return nil, fmt.Errorf("%q: want an absolute http(s) URL", s)
	}
	if u.User != nil {
		return nil, errors.New("credentials in the URL are not allowed")
	}
	return u, nil
}

// EnvNames lists the environment variables ApplyEnv reads, without the
// prefix. With prefix "AIRVIEW_UPDATE_" (main program) or
// "AIRVIEW_FEEDER_UPDATE_" (feeder): ENABLED, CHANNEL, MANIFEST_URL, KEYS
// (comma-separated), CHECK_INTERVAL_MIN, HEALTH_WINDOW_S, HEALTH_URL,
// MAX_DOWNLOAD_MB.
var EnvNames = []string{"ENABLED", "CHANNEL", "MANIFEST_URL", "KEYS", "CHECK_INTERVAL_MIN", "HEALTH_WINDOW_S", "HEALTH_URL", "MAX_DOWNLOAD_MB"}

// ApplyEnv overrides settings from environment variables prefix+NAME (see
// EnvNames). Unset or empty variables leave the value alone. getenv is
// os.Getenv in production.
func (s *Settings) ApplyEnv(prefix string, getenv func(string) string) error {
	var errs []error
	get := func(name string) (string, bool) {
		v := strings.TrimSpace(getenv(prefix + name))
		return v, v != ""
	}
	atoi := func(name string, dst *int) {
		if v, ok := get(name); ok {
			n, err := strconv.Atoi(v)
			if err != nil {
				errs = append(errs, fmt.Errorf("%s%s: %q is not an integer", prefix, name, v))
				return
			}
			*dst = n
		}
	}
	if v, ok := get("ENABLED"); ok {
		b, err := strconv.ParseBool(v)
		if err != nil {
			errs = append(errs, fmt.Errorf("%sENABLED: %q is not a boolean", prefix, v))
		} else {
			s.Enabled = b
		}
	}
	if v, ok := get("CHANNEL"); ok {
		s.Channel = v
	}
	if v, ok := get("MANIFEST_URL"); ok {
		s.ManifestURL = v
	}
	if v, ok := get("KEYS"); ok {
		s.Keys = strings.FieldsFunc(v, func(r rune) bool { return r == ',' || r == ' ' })
	}
	atoi("CHECK_INTERVAL_MIN", &s.CheckIntervalMin)
	atoi("HEALTH_WINDOW_S", &s.HealthWindowS)
	if v, ok := get("HEALTH_URL"); ok {
		s.HealthURL = v
	}
	atoi("MAX_DOWNLOAD_MB", &s.MaxDownloadMB)
	return errors.Join(errs...)
}

// ChannelManifestURL returns the manifest URL for the configured channel.
func (s Settings) ChannelManifestURL() (string, error) {
	n := s.Normalized()
	if n.ManifestURL == "" {
		return "", errors.New("no update manifest URL configured (update.manifestUrl)")
	}
	return ChannelURL(n.ManifestURL, n.Channel)
}

// ChannelURL maps a stable manifest URL to the URL of channel (see
// Settings.ManifestURL).
func ChannelURL(base, channel string) (string, error) {
	if strings.Contains(base, "{channel}") {
		base = strings.ReplaceAll(base, "{channel}", channel)
	} else if channel != ChannelStable {
		u, err := checkURL(base)
		if err != nil {
			return "", err
		}
		dir, file := "", u.Path
		if i := strings.LastIndexByte(u.Path, '/'); i >= 0 {
			dir, file = u.Path[:i+1], u.Path[i+1:]
		}
		u.Path = dir + channel + "/" + file
		base = u.String()
	}
	if _, err := checkURL(base); err != nil {
		return "", err
	}
	return base, nil
}

// HealthWindow returns the health window as a duration.
func (s Settings) HealthWindow() time.Duration {
	return time.Duration(s.Normalized().HealthWindowS) * time.Second
}

// CheckInterval returns the check period as a duration.
func (s Settings) CheckInterval() time.Duration {
	return time.Duration(s.Normalized().CheckIntervalMin) * time.Minute
}
