package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"crypto/subtle"
	"encoding/base32"
	"encoding/binary"
	"errors"
	"hash"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// TOTP implements RFC 6238 time-based one-time passwords (HOTP, RFC 4226,
// over the counter T = floor((unix - T0) / period) with T0 = 0).
//
// Authenticator apps almost universally support only SHA-1, 6 digits and
// 30 s, which is what NewTOTPSecret produces; SHA-256 / SHA-512 and 7-8
// digits are supported for verification and for the RFC test vectors.
type TOTP struct {
	Secret    []byte // shared key (raw bytes)
	Algorithm string // "SHA1" (default), "SHA256" or "SHA512"
	Digits    int    // 6 (default), 7 or 8
	Period    int    // seconds (default 30)
}

// TOTP defaults and limits.
const (
	TOTPDigits      = 6
	TOTPPeriod      = 30
	TOTPSecretBytes = 20 // 160 bits, as RFC 4226 recommends for SHA-1
	// TOTPSkew is the number of steps accepted either side of the current
	// one (clock drift between server and phone).
	TOTPSkew = 1
)

// ErrTOTPSecret reports an unusable shared secret.
var ErrTOTPSecret = errors.New("invalid TOTP secret")

var b32 = base32.StdEncoding.WithPadding(base32.NoPadding)

// NewTOTPSecret returns a fresh random SHA-1 / 6-digit / 30 s generator.
func NewTOTPSecret() TOTP {
	k := make([]byte, TOTPSecretBytes)
	_, _ = rand.Read(k)
	return TOTP{Secret: k, Algorithm: "SHA1", Digits: TOTPDigits, Period: TOTPPeriod}
}

// ParseTOTPSecret decodes a base32 secret as shown to users (spaces,
// hyphens, lower case and padding are tolerated).
func ParseTOTPSecret(s string) (TOTP, error) {
	s = strings.ToUpper(strings.NewReplacer(" ", "", "-", "", "=", "").Replace(s))
	k, err := b32.DecodeString(s)
	if err != nil || len(k) < 10 || len(k) > 128 {
		return TOTP{}, ErrTOTPSecret
	}
	return TOTP{Secret: k, Algorithm: "SHA1", Digits: TOTPDigits, Period: TOTPPeriod}, nil
}

// Base32 returns the secret in the unpadded base32 form authenticator apps
// accept for manual entry.
func (t TOTP) Base32() string { return b32.EncodeToString(t.Secret) }

func (t TOTP) norm() (func() hash.Hash, int, int64) {
	h := sha1.New
	switch strings.ToUpper(t.Algorithm) {
	case "SHA256":
		h = sha256.New
	case "SHA512":
		h = sha512.New
	}
	d := t.Digits
	if d < 6 || d > 8 {
		d = TOTPDigits
	}
	p := int64(t.Period)
	if p <= 0 {
		p = TOTPPeriod
	}
	return h, d, p
}

// Step returns the RFC 6238 time step containing at.
func (t TOTP) Step(at time.Time) int64 {
	_, _, p := t.norm()
	u := at.Unix()
	if u < 0 {
		return 0
	}
	return u / p
}

// hotp computes the RFC 4226 value for one counter.
func (t TOTP) hotp(counter int64) string {
	hf, digits, _ := t.norm()
	var msg [8]byte
	binary.BigEndian.PutUint64(msg[:], uint64(counter))
	m := hmac.New(hf, t.Secret)
	m.Write(msg[:])
	sum := m.Sum(nil)
	// Dynamic truncation: the low nibble of the last byte selects 4 bytes.
	off := int(sum[len(sum)-1] & 0x0f)
	bin := binary.BigEndian.Uint32(sum[off:off+4]) & 0x7fffffff
	mod := uint32(1)
	for range digits {
		mod *= 10
	}
	s := strconv.FormatUint(uint64(bin%mod), 10)
	if len(s) < digits {
		s = strings.Repeat("0", digits-len(s)) + s
	}
	return s
}

// Code returns the code valid at the given time.
func (t TOTP) Code(at time.Time) string { return t.hotp(t.Step(at)) }

// Verify checks code against the steps around at (±TOTPSkew). lastStep is
// the step of the last code this account used successfully: that step and
// every earlier one are refused, so a code cannot be replayed (RFC 6238
// §5.2). On success it returns the matched step, which the caller must
// store as the new lastStep.
func (t TOTP) Verify(code string, at time.Time, lastStep int64) (int64, bool) {
	_, digits, _ := t.norm()
	code = strings.ReplaceAll(strings.TrimSpace(code), " ", "")
	if len(code) != digits || len(t.Secret) == 0 {
		return 0, false
	}
	for _, c := range code {
		if c < '0' || c > '9' {
			return 0, false
		}
	}
	now := t.Step(at)
	matched, ok := int64(0), false
	// Check every candidate (no early exit) so timing does not reveal which
	// window matched.
	for d := int64(-TOTPSkew); d <= TOTPSkew; d++ {
		st := now + d
		if st < 0 {
			continue
		}
		eq := subtle.ConstantTimeCompare([]byte(t.hotp(st)), []byte(code)) == 1
		if eq && st > lastStep && !ok {
			matched, ok = st, true
		}
	}
	return matched, ok
}

// URI returns the otpauth:// provisioning URI (the de-facto "Key URI
// format" understood by authenticator apps; render it as a QR code).
func (t TOTP) URI(issuer, account string) string {
	_, digits, period := t.norm()
	alg := strings.ToUpper(t.Algorithm)
	if alg == "" {
		alg = "SHA1"
	}
	label := url.PathEscape(issuer) + ":" + url.PathEscape(account)
	q := url.Values{}
	q.Set("secret", t.Base32())
	q.Set("issuer", issuer)
	q.Set("algorithm", alg)
	q.Set("digits", strconv.Itoa(digits))
	q.Set("period", strconv.FormatInt(period, 10))
	return "otpauth://totp/" + label + "?" + q.Encode()
}
