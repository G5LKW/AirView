package model

import (
	"crypto/sha256"
	"encoding/base32"
	"encoding/base64"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// Station identity and privacy types of the AirView network
// (docs/aggregator/DESIGN.md §3, §6.1, §8).
//
// A station is one receiver timestamp clock: an EVO 2CH is one station with
// two channels; a Pi running readsb and a Radarcape is two stations under one
// feeder.

// StationIdx is a dense in-process station index. 0 is the local station of
// standalone mode (and "no station"); the aggregator's host station is 1.
// It is never shown to users and never persisted outside stations.db.
type StationIdx uint32

// LocalStation is the StationIdx of standalone mode's own receiver.
const LocalStation StationIdx = 0

// HostStation is the StationIdx of the aggregator's in-process host station.
const HostStation StationIdx = 1

// idEncoding is RFC 4648 base32 (standard alphabet) without padding, written
// in lower case for SIDs and feeder IDs. 10 bytes encode to exactly 16
// characters.
var idEncoding = base32.StdEncoding.WithPadding(base32.NoPadding)

// IDBytes is the raw length of a SID or feeder ID (80 bits).
const IDBytes = 10

// IDLen is the encoded length of a SID or feeder ID.
const IDLen = 16

// NewSID returns a fresh public station ID: lower-case base32 of 10 random
// bytes read from r (use crypto/rand.Reader). It is never derived from the
// location or IP address.
func NewSID(r io.Reader) (string, error) {
	b := make([]byte, IDBytes)
	if _, err := io.ReadFull(r, b); err != nil {
		return "", fmt.Errorf("model: new SID: %w", err)
	}
	return strings.ToLower(idEncoding.EncodeToString(b)), nil
}

// ValidID reports whether s is a well-formed SID or feeder ID (16 lower- or
// upper-case base32 characters).
func ValidID(s string) bool {
	if len(s) != IDLen {
		return false
	}
	_, err := idEncoding.DecodeString(strings.ToUpper(s))
	return err == nil
}

// CanonicalID returns the canonical (lower-case) form of a SID or feeder ID
// and whether it is well-formed.
func CanonicalID(s string) (string, bool) {
	s = strings.ToLower(strings.TrimSpace(s))
	return s, ValidID(s)
}

// FeederIDFromPub derives a native feeder's ID from its Ed25519 public key
// (32 bytes): lower-case base32 of the first 10 bytes of SHA-256(pub).
func FeederIDFromPub(pub []byte) string {
	sum := sha256.Sum256(pub)
	return strings.ToLower(idEncoding.EncodeToString(sum[:IDBytes]))
}

// ClaimCodeLen is the length of a claim code: 8 base32 characters (40 bits).
const ClaimCodeLen = 8

// NewClaimCode returns a fresh claim code (upper-case base32, 8 characters)
// from r (use crypto/rand.Reader).
func NewClaimCode(r io.Reader) (string, error) {
	b := make([]byte, 5)
	if _, err := io.ReadFull(r, b); err != nil {
		return "", fmt.Errorf("model: new claim code: %w", err)
	}
	return idEncoding.EncodeToString(b), nil
}

// NormalizeClaimCode upper-cases a typed claim code, removes spaces and
// dashes, maps the look-alikes 0->O, 1->I and 8->B, and reports whether the
// result is a well-formed code.
func NormalizeClaimCode(s string) (string, bool) {
	var b strings.Builder
	for _, r := range strings.ToUpper(s) {
		switch r {
		case ' ', '-', '\t':
			continue
		case '0':
			r = 'O'
		case '1':
			r = 'I'
		case '8':
			r = 'B'
		}
		b.WriteRune(r)
	}
	c := b.String()
	if len(c) != ClaimCodeLen {
		return c, false
	}
	_, err := idEncoding.DecodeString(c)
	return c, err == nil
}

// NormalizeUUID normalizes a compat feeder UUID (0xE4 escape on 30004,
// "uuid" in the 31090 handshake): lower-case hex with dashes and braces
// removed. At least 28 hex digits are required (some feeders truncate);
// at most 32 are kept.
func NormalizeUUID(s string) (string, bool) {
	var b strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(s)) {
		switch {
		case r == '-' || r == '{' || r == '}':
		case (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f'):
			b.WriteRune(r)
		default:
			return "", false
		}
	}
	u := b.String()
	if len(u) < 28 || len(u) > 32 {
		return "", false
	}
	return u, true
}

// Station kinds: how a station is connected (stations.kind).
const (
	StationKindAVF   = "avf"   // native AirView feeder over AVF/1
	StationKindBeast = "beast" // compat Beast on 30004 (map and stats only)
	StationKindMLATC = "mlatc" // compat mlat-client on 31090
	StationKindHost  = "host"  // the aggregator's own in-process receiver
)

// Location status of a station (DESIGN §10.1).
const (
	LocUnknown    = "unknown"    // no usable position
	LocEstimating = "estimating" // a survey / Locate Me run is in progress
	LocEstimated  = "estimated"  // a survey result exists, not yet used for MLAT
	LocConfirmed  = "confirmed"  // a survey result confirmed by the owner (or AutoUseSurvey)
	LocDeclared   = "declared"   // declared by the owner (cross-checked by the survey)
	LocGNSS       = "gnss"       // the receiver's own GNSS position
	LocSuspect    = "suspect"    // the declared position contradicts the survey
)

// Location sources (StationGeo.Source, AVF RECEIVER location.source).
const (
	LocSrcManual    = "manual"    // declared by the owner / installer
	LocSrcGNSS      = "gnss"      // receiver GNSS (Radarcape type '5')
	LocSrcEstimated = "estimated" // reverse-MLAT survey
	LocSrcLocateMe  = "locateme"  // Locate Me v2 (coarse prior only)
	LocSrcHost      = "host"      // the aggregator's own configured receiver
)

// Altitude references.
const (
	AltRefMSL       = "msl"       // above mean sea level (geoid)
	AltRefHAE       = "hae"       // height above the WGS-84 ellipsoid
	AltRefAmbiguous = "ambiguous" // mlat-client altitude: resolved by the survey
)

// Admission states of a station (stations.admission).
const (
	AdmPending  = "pending"  // held for admin approval: own lane only
	AdmApproved = "approved" // full participation
	AdmDenied   = "denied"   // refused
	AdmRevoked  = "revoked"  // previously approved, revoked by the admin
)

// Public pin precision (privacy tier of a station's map pin, DESIGN §8.3).
const (
	PinHidden = "hidden" // no public pin (default)
	PinRegion = "region" // keyed point inside a 0.5° cell
	PinCell01 = "cell01" // keyed point inside a 0.1° cell
)

// PinCellDeg returns the cell size of a pin precision in degrees (0 for
// hidden or unknown values).
func PinCellDeg(pin string) float64 {
	switch pin {
	case PinRegion:
		return 0.5
	case PinCell01:
		return 0.1
	}
	return 0
}

// ValidPin reports whether pin is a known pin precision.
func ValidPin(pin string) bool { return pin == PinHidden || pin == PinRegion || pin == PinCell01 }

// PeerBand is the coarse distance band in which an owner sees a sync peer
// (DESIGN §8.2). No bearing or position is ever shown.
type PeerBand uint8

// Peer distance bands.
const (
	PeerBandNear PeerBand = iota // < 50 km
	PeerBandMid                  // 50-150 km
	PeerBandFar                  // > 150 km
)

// PeerBandFor returns the band of a distance in kilometres.
func PeerBandFor(km float64) PeerBand {
	switch {
	case km < 50:
		return PeerBandNear
	case km <= 150:
		return PeerBandMid
	}
	return PeerBandFar
}

// String returns "<50", "50-150" or ">150".
func (b PeerBand) String() string {
	switch b {
	case PeerBandNear:
		return "<50"
	case PeerBandMid:
		return "50-150"
	}
	return ">150"
}

// StationFeedID returns the namespaced FeedID of receiver rx of station sid
// in aggregator mode: "st/<sid>/<rx>".
func StationFeedID(sid string, rx int) string { return "st/" + sid + "/" + strconv.Itoa(rx) }

// ParseStationFeedID splits a FeedID made by StationFeedID.
func ParseStationFeedID(feedID string) (sid string, rx int, ok bool) {
	rest, found := strings.CutPrefix(feedID, "st/")
	if !found {
		return "", 0, false
	}
	sid, rxs, found := strings.Cut(rest, "/")
	if !found || sid == "" {
		return "", 0, false
	}
	rx, err := strconv.Atoi(rxs)
	if err != nil || rx < 0 {
		return "", 0, false
	}
	return sid, rx, true
}

// StationAntKey returns the stats key of a station in stations.db: "s<idx>"
// for the station combined, "s<idx>/<channel>" for one antenna channel
// (channel < 0 = combined).
func StationAntKey(idx StationIdx, channel int) string {
	k := "s" + strconv.FormatUint(uint64(idx), 10)
	if channel >= 0 {
		k += "/" + strconv.Itoa(channel)
	}
	return k
}

// StationMeta is the non-locating description of a station that ingest,
// tracker and lanes may use freely. It deliberately has no coordinates.
//
// DESIGN §6.1 calls this type "StationInfo"; that name was already taken by
// the effective-station-position DTO of Status (see StationInfo), so the
// contract type is StationMeta.
type StationMeta struct {
	Idx  StationIdx // dense index
	SID  string     // public opaque ID
	Name string     // display name ("" = none)
	Kind string     // StationKind* constant
	// ClockCls is the station's current clock class (after any server-side
	// downgrade).
	ClockCls ClockClass
	// Located is true when the station has a position usable for geometry
	// products (StationResolver.Geo succeeds).
	Located bool
	// Admission is the Adm* state; only AdmApproved stations reach the
	// public map and MLAT.
	Admission string
	// Pin is the station's public pin precision (Pin* constant).
	Pin string
	// ShowName and PublicStats are the owner's public opt-ins.
	ShowName    bool
	PublicStats bool
	// Anchor is true while the station is an accepted MLAT time anchor.
	Anchor bool
}

// StationGeo is a station's exact position. It is private: only the MLAT
// engine, lanes and owner/admin DTOs may see it.
type StationGeo struct {
	Lat, Lon float64 // WGS-84 degrees
	HAE      float64 // antenna height above the ellipsoid, metres
	SigmaM   float64 // 1-sigma horizontal uncertainty, metres (0 = exact / unknown)
	Source   string  // LocSrc* constant
}

// StationResolver resolves station indices. Implementations must be safe for
// concurrent use and must not block (they are called on hot paths).
type StationResolver interface {
	// Info returns the station's non-locating description.
	Info(StationIdx) (StationMeta, bool)
	// Geo returns the station's exact position when it is located.
	Geo(StationIdx) (StationGeo, bool)
}

// decodeB64Len decodes standard or URL base64 (padded or not) and checks the
// decoded length.
func decodeB64Len(s string, n int) ([]byte, error) {
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if b, err := enc.DecodeString(s); err == nil {
			if len(b) != n {
				return nil, fmt.Errorf("decoded %d bytes, want %d", len(b), n)
			}
			return b, nil
		}
	}
	return nil, fmt.Errorf("not base64")
}
