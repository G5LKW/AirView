package feederui

import (
	"context"
	"time"

	"airview/internal/model"
	"airview/internal/uplink"
)

// Backend is what the status page needs from the running feeder. The
// airview-feeder program implements it; tests use a fake. Every method must
// be safe for concurrent use and must not block for long (Claim may wait
// for the aggregator, bounded by ctx).
type Backend interface {
	// Snapshot returns the current status. It must not contain secrets
	// (enrol tokens, password hashes, claim codes, the identity key).
	Snapshot() Snapshot
	// Claim asks aggregator uplinkID ("" = first) for a claim code for
	// receiver rxNo (0 = first). The code is a bearer secret: it is shown
	// to the local user once and never logged.
	Claim(ctx context.Context, uplinkID string, rxNo int) (code string, expires time.Time, err error)
	// Retry reconnects uplinkID now and leaves the stopped state.
	Retry(uplinkID string) error
	// Settings returns the editable configuration (the configuration file
	// layer, before environment / command-line overrides) with secrets
	// replaced by Redacted, plus the paths locked by overrides.
	Settings() SettingsDoc
	// SaveSettings validates cfg (secrets equal to Redacted keep their
	// stored value), writes the configuration file and applies it. It
	// returns the normalisation notes.
	SaveSettings(cfg model.FeederConfig) ([]string, error)
}

// Redacted replaces secrets in Settings; a value equal to it in
// SaveSettings means "unchanged".
const Redacted = "********"

// SettingsDoc is the GET /api/settings body.
type SettingsDoc struct {
	Config model.FeederConfig `json:"config"`
	// File is the configuration file the settings are saved to.
	File string `json:"file"`
	// Locked lists settings overridden by the environment or the command
	// line ("uplinks[0].host" ...); edits to them are saved but have no
	// effect until the override is removed.
	Locked []string `json:"locked,omitempty"`
	// ReadOnly is set when the page may not save (no writable file).
	ReadOnly bool `json:"readOnly,omitempty"`
}

// Snapshot is the GET /api/status body.
type Snapshot struct {
	Software  string `json:"software"`
	Version   string `json:"version"`
	OS        string `json:"os"`
	Arch      string `json:"arch"`
	FeederID  string `json:"feederId"`
	UUID      string `json:"uuid"`
	StartedMs int64  `json:"startedMs"`
	NowMs     int64  `json:"nowMs"`
	// ConfigFile and DataDir are local paths (the page is local only).
	ConfigFile string `json:"configFile"`
	DataDir    string `json:"dataDir"`
	// Notes are configuration corrections and warnings from the last load.
	Notes     []string         `json:"notes,omitempty"`
	Receivers []ReceiverStatus `json:"receivers"`
	Uplinks   []uplink.Status  `json:"uplinks"`
	// Beast are the Beast outputs to other aggregators ("also feed
	// others", uplinks[] of type "beast").
	Beast   []uplink.BeastStatus `json:"beast,omitempty"`
	Results ResultsStatus        `json:"results"`
	NTP     *model.NTPStatus     `json:"ntp,omitempty"`
}

// ReceiverStatus is one configured receiver.
type ReceiverStatus struct {
	RxNo int    `json:"rxNo"`
	Name string `json:"name"`
	// FeedID is the receiver's input ID.
	FeedID string `json:"feedId"`
	// InputType is the configured type ("auto" until detection succeeds);
	// Detected describes what auto-detection found ("" = not detected yet).
	InputType string `json:"inputType"`
	Detected  string `json:"detected,omitempty"`
	Addr      string `json:"addr,omitempty"`
	Enabled   bool   `json:"enabled"`
	Connected bool   `json:"connected"`
	SinceMs   int64  `json:"sinceMs,omitempty"`
	LastError string `json:"lastError,omitempty"`
	Info      string `json:"info,omitempty"`
	// Clock: effective and detected class names, current epoch, events.
	ClockClass    string `json:"clockClass,omitempty"`
	ClockDetected string `json:"clockDetected,omitempty"`
	Epoch         uint32 `json:"epoch,omitempty"`
	ClockEvents   uint64 `json:"clockEvents,omitempty"`
	GPSTime       bool   `json:"gpsTime,omitempty"`
	// Located is true when a position is declared; the position itself is
	// shown (this page is local).
	Located bool    `json:"located"`
	Lat     float64 `json:"lat,omitempty"`
	Lon     float64 `json:"lon,omitempty"`
	AltM    float64 `json:"altM,omitempty"`
	AltRef  string  `json:"altRef,omitempty"`
	Stats   RxStats `json:"stats"`
}

// ResultsStatus describes the local delivery of returned MLAT results.
type ResultsStatus struct {
	BeastPort  int    `json:"beastPort"`
	Listening  bool   `json:"listening"`
	ListenErr  string `json:"listenError,omitempty"`
	Clients    int    `json:"clients"`
	ReadsbAddr string `json:"readsbAddr,omitempty"`
	ReadsbUp   bool   `json:"readsbConnected,omitempty"`
	ReadsbErr  string `json:"readsbError,omitempty"`
	Received   uint64 `json:"received"` // fixes received from aggregators
	Frames     uint64 `json:"frames"`   // Beast frames written (all outputs)
}
