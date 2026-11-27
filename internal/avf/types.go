package avf

import (
	"errors"
	"fmt"
	"strconv"
	"time"
)

// Protocol identifiers and limits (AVF.md §2-§4, §8, §9).
const (
	// ALPN is the mandatory TLS application protocol of AVF/1.
	ALPN = "avf/1"
	// Revision is the AVF/1 revision this package speaks (HELLO_S proto).
	Revision = 1
	// ExporterLabel is the TLS exporter label of the HELLO_C signature.
	ExporterLabel = "EXPORTER-AirView-AVF1"
	// ExporterLen is the length of the exported keying material.
	ExporterLen = 32
	// SigMagic starts the signed HELLO_C message.
	SigMagic = "AVF1"
	// NonceLen is the length of the HELLO_S nonce.
	NonceLen = 32

	// MaxRecordLen is the largest record payload (64 KiB).
	MaxRecordLen = 64 << 10
	// MaxHandshakeLen is the largest handshake message (64 KiB).
	MaxHandshakeLen = 64 << 10
	// FlushThreshold is the amount of pending uncompressed data that forces
	// a sync flush (32 KiB).
	FlushThreshold = 32 << 10
	// MaxFramesPerRecord bounds n of a FRAMES record.
	MaxFramesPerRecord = 4096
	// MaxFixesPerRecord bounds n of an MLAT_RESULTS record.
	MaxFixesPerRecord = 1000
	// MaxClockStatusRaw bounds the raw type-'4' payload of CLOCK_STATUS.
	MaxClockStatusRaw = 64

	// DefaultBatch is the default client flush interval (WELCOME batchMs).
	DefaultBatch = 50 * time.Millisecond
	// DefaultHandshakeTimeout bounds TCP accept to WELCOME.
	DefaultHandshakeTimeout = 10 * time.Second
	// PingInterval is the server's PING period.
	PingInterval = 5 * time.Second
	// AckInterval is the server's minimum ACK rate while records arrive.
	AckInterval = time.Second
	// ClientLiveness: the client reconnects after this long with neither
	// PING nor ACK.
	ClientLiveness = 30 * time.Second
	// ServerIdleTimeout: the server drops a session after this long without
	// any record.
	ServerIdleTimeout = 60 * time.Second
	// ResumeWindow is the age limit of data retransmitted after a reconnect.
	ResumeWindow = 30 * time.Second
	// ResumeTokenTTL is the lifetime of a resume token.
	ResumeTokenTTL = 10 * time.Minute
	// WatermarkTTL is how long the server keeps a watermark after a session.
	WatermarkTTL = 10 * time.Minute
	// DeniedMinBackoff is the client back-off after a denied WELCOME that
	// carries no reconnectIn.
	DeniedMinBackoff = 10 * time.Minute
)

// Features (AVF.md §3.6). Unknown names are ignored.
const (
	FeatFlate       = "flate" // mandatory in AVF/1
	FeatBackfill    = "backfill"
	FeatSelective   = "selective"
	FeatResults     = "results"
	FeatClaim       = "claim"
	FeatGNSS        = "gnss"
	FeatDecoded     = "decoded"
	FeatMinuteStats = "minute_stats"
)

// AllFeatures lists every feature defined by revision 1.
var AllFeatures = []string{FeatFlate, FeatBackfill, FeatSelective, FeatResults,
	FeatClaim, FeatGNSS, FeatDecoded, FeatMinuteStats}

// WELCOME status values.
const (
	StatusApproved = "approved"
	StatusPending  = "pending"
	StatusDenied   = "denied"
)

// Traffic modes (WELCOME config.mode, INTEREST mode).
const (
	ModeFull      = "full"
	ModeSelective = "selective"
)

// Software names of HELLO_C.
const (
	SoftwareFeeder  = "airview-feeder"
	SoftwareAirView = "airview"
)

// Error codes (AVF.md §10), used in WELCOME.code, GOODBYE and NOTICE.
const (
	CodeProtoVersion  = "proto_version"
	CodeBadHello      = "bad_hello"
	CodeBadSignature  = "bad_signature"
	CodeEnrolRequired = "enrol_required"
	CodeEnrolInvalid  = "enrol_invalid"
	CodeDenied        = "denied"
	CodeRevoked       = "revoked"
	CodeClonedFeeder  = "cloned_feeder"
	CodeLimitIP       = "limit_ip"
	CodeLimitStations = "limit_stations"
	CodeServerFull    = "server_full"
	CodeRateLimited   = "rate_limited"
	CodeProtocolError = "protocol_error"
	CodeReplaced      = "replaced"
	CodeShuttingDown  = "shutting_down"
	CodeInternal      = "internal"
)

// NOTICE codes that are not error codes (AVF.md §6.2).
const (
	NoticeApproved        = "approved"
	NoticePending         = "pending"
	NoticeLocationSuspect = "location_suspect"
	NoticeClockDowngraded = "clock_class_downgraded"
	NoticeSpoolIgnored    = "spool_ignored"
	NoticeUpdateAvailable = "update_available"
)

// NOTICE levels.
const (
	LevelInfo  = "info"
	LevelWarn  = "warn"
	LevelError = "error"
)

// ClientAction is what a client should do after an error code (AVF.md §10).
type ClientAction int

// Client actions.
const (
	ActionBackoff   ClientAction = iota // normal reconnect back-off
	ActionStop                          // stop and tell the owner
	ActionHonour                        // honour reconnectIn (or the minimum back-off)
	ActionBackoff10                     // back off at least 10 minutes
	ActionNoRetry                       // do not reconnect this connection (replaced)
)

// ActionFor maps an error code to the client action of AVF.md §10. Unknown
// codes are treated like "internal".
func ActionFor(code string) ClientAction {
	switch code {
	case CodeProtoVersion, CodeEnrolRequired, CodeEnrolInvalid, CodeRevoked, CodeClonedFeeder:
		return ActionStop
	case CodeDenied, CodeServerFull, CodeRateLimited, CodeShuttingDown:
		return ActionHonour
	case CodeLimitStations:
		return ActionBackoff10
	case CodeReplaced:
		return ActionNoRetry
	}
	return ActionBackoff
}

// ErrProtocol is wrapped by every protocol violation (malformed record,
// wrong direction, bad seq, guard violation). A server answers it with
// GOODBYE protocol_error.
var ErrProtocol = errors.New("avf: protocol error")

// protoErr returns an error wrapping ErrProtocol.
func protoErr(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrProtocol, fmt.Sprintf(format, args...))
}

// protoErrWrap returns an error wrapping base (which itself wraps
// ErrProtocol).
func protoErrWrap(base error, format string, args ...any) error {
	return fmt.Errorf("%w: %s", base, fmt.Sprintf(format, args...))
}

// Direction says which way a record travels.
type Direction uint8

// Directions.
const (
	FromClient Direction = 1 // client → server (types 0x01-0x7F)
	FromServer Direction = 2 // server → client (types 0x81-0xFE)
)

// RecordType is the u8 type code of a record (AVF.md §4.3). The codes are
// frozen within AVF/1.
type RecordType uint8

// Client → server record types.
const (
	TReceiver    RecordType = 0x01
	TFrames      RecordType = 0x02
	TClockStatus RecordType = 0x03
	TClockPair   RecordType = 0x04
	TClockEvent  RecordType = 0x05
	TInputStatus RecordType = 0x06
	TGNSSPos     RecordType = 0x07
	TDecoded     RecordType = 0x08
	TMinuteStats RecordType = 0x09
	TPong        RecordType = 0x0A
	TBackfill    RecordType = 0x0B
	TClaim       RecordType = 0x0C
	TGoodbyeC    RecordType = 0x0D
)

// Server → client record types.
const (
	TPing        RecordType = 0x81
	TAck         RecordType = 0x82
	TInterest    RecordType = 0x83
	TFlow        RecordType = 0x84
	TMLATResults RecordType = 0x85
	TFeedback    RecordType = 0x86
	TClaimCode   RecordType = 0x87
	TNotice      RecordType = 0x88
	TGoodbyeS    RecordType = 0x89
)

var typeNames = map[RecordType]string{
	TReceiver: "RECEIVER", TFrames: "FRAMES", TClockStatus: "CLOCK_STATUS",
	TClockPair: "CLOCK_PAIR", TClockEvent: "CLOCK_EVENT", TInputStatus: "INPUT_STATUS",
	TGNSSPos: "GNSS_POS", TDecoded: "DECODED", TMinuteStats: "MINUTE_STATS",
	TPong: "PONG", TBackfill: "BACKFILL", TClaim: "CLAIM", TGoodbyeC: "GOODBYE",
	TPing: "PING", TAck: "ACK", TInterest: "INTEREST", TFlow: "FLOW",
	TMLATResults: "MLAT_RESULTS", TFeedback: "FEEDBACK", TClaimCode: "CLAIM_CODE",
	TNotice: "NOTICE", TGoodbyeS: "GOODBYE",
}

// String returns the AVF.md name of a known type, else "0x%02X".
func (t RecordType) String() string {
	if n, ok := typeNames[t]; ok {
		return n
	}
	return "0x" + strconv.FormatUint(uint64(t)|0x100, 16)[1:]
}

// Reserved reports whether t is one of the reserved codes 0x00, 0x80, 0xFF.
func (t RecordType) Reserved() bool { return t == 0x00 || t == 0x80 || t == 0xFF }

// Direction returns the direction implied by the type code's range, or 0
// for a reserved code.
func (t RecordType) Direction() Direction {
	switch {
	case t.Reserved():
		return 0
	case t < 0x80:
		return FromClient
	default:
		return FromServer
	}
}

// Known reports whether t is defined by this revision.
func (t RecordType) Known() bool { _, ok := typeNames[t]; return ok }

// HasSeq reports whether the payload of t starts with "uvarint seq".
func (t RecordType) HasSeq() bool {
	switch t {
	case TReceiver, TFrames, TClockStatus, TClockPair, TClockEvent, TInputStatus,
		TGNSSPos, TDecoded, TMinuteStats, TBackfill:
		return true
	}
	return false
}

// Control reports whether writing t forces an immediate sync flush
// (AVF.md §4.1: ACK, PING, PONG, FLOW, INTEREST, NOTICE, GOODBYE, CLAIM,
// CLAIM_CODE).
func (t RecordType) Control() bool {
	switch t {
	case TAck, TPing, TPong, TFlow, TInterest, TNotice, TGoodbyeC, TGoodbyeS, TClaim, TClaimCode:
		return true
	}
	return false
}

// GoodbyeType returns the GOODBYE type code for a sender direction.
func GoodbyeType(from Direction) RecordType {
	if from == FromServer {
		return TGoodbyeS
	}
	return TGoodbyeC
}
