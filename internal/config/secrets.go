package config

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"net/url"
	"strings"

	"airview/internal/auth"
	"airview/internal/model"
)

// Wire conventions for secret fields in GET/PUT /api/config (see the package
// comment for the full table).
const (
	// SecretSet is what Redact puts in place of a stored secret, and what a
	// client sends back (or simply "") to keep it.
	SecretSet = "set"
	// SecretClear, sent by a client, removes the stored secret.
	SecretClear = "clear"
	// PlainPrefix marks the rest of the value as the literal new secret
	// ("plain:hunter2"). It is needed only for secrets that would otherwise
	// be mistaken for a marker ("set", "clear") but is always accepted.
	// "plain:" with nothing after it removes the secret.
	PlainPrefix = "plain:"
)

const (
	hashScheme = "sha256"
	saltBytes  = 16
)

// HashIterations is the PBKDF2 work factor of HashPassword. It is lower
// than the account default (auth.DefaultIterations) because these hashes
// guard pages checked with HTTP Basic auth on small boards; callers cache
// successful checks (CheckCache) and throttle failures.
const HashIterations = 200_000

// HashPassword returns the stored form of an admin / page password: a
// PBKDF2-SHA256 hash in the auth package format
// ("pbkdf2-sha256$<iterations>$<salt>$<key>"). Hashes of the former format
//
//	"sha256:<hex salt>:<hex SHA-256(salt bytes || password bytes)>"
//
// are still accepted by CheckPassword (SEC-08: a single salted SHA-256 can
// be brute-forced at line rate). The empty password hashes to "" (no admin
// password, mutating endpoints are open).
func HashPassword(pw string) string {
	if pw == "" {
		return ""
	}
	h, err := auth.HashPasswordCost(pw, HashIterations)
	if err != nil { // longer than 1 KiB: unusable rather than open
		return HashPassword(randomSecret())
	}
	return h
}

// legacyHash returns the former single-SHA-256 form (tests of the upgrade
// path).
func legacyHash(pw string) string {
	salt := make([]byte, saltBytes)
	_, _ = rand.Read(salt) // never fails (crypto/rand aborts the process instead)
	return hashScheme + ":" + hex.EncodeToString(salt) + ":" + hex.EncodeToString(digest(salt, pw))
}

func digest(salt []byte, pw string) []byte {
	h := sha256.New()
	h.Write(salt)
	h.Write([]byte(pw))
	return h.Sum(nil)
}

// CheckPassword reports whether pw matches a hash produced by HashPassword.
// The comparison is constant-time. An empty or malformed hash matches
// nothing (callers decide separately that an empty hash means "no password
// required").
func CheckPassword(hash, pw string) bool {
	if auth.ValidPasswordHash(hash) {
		return auth.VerifyPassword(hash, pw)
	}
	salt, want, ok := parseHash(hash)
	if !ok {
		return false
	}
	got := digest(salt, pw)
	return subtle.ConstantTimeCompare(got, want) == 1
}

// IsLegacyHash reports whether hash has the former single-SHA-256 form
// (callers may re-hash after a successful check).
func IsLegacyHash(hash string) bool {
	_, _, ok := parseHash(hash)
	return ok
}

// validHash reports whether s is a well-formed password hash (PBKDF2, or
// the former "sha256:<salt>:<hash>").
func validHash(s string) bool {
	if auth.ValidPasswordHash(s) {
		return true
	}
	_, _, ok := parseHash(s)
	return ok
}

func parseHash(s string) (salt, sum []byte, ok bool) {
	if len(s) > 512 {
		return nil, nil, false
	}
	parts := strings.Split(s, ":")
	if len(parts) != 3 || parts[0] != hashScheme {
		return nil, nil, false
	}
	salt, err := hex.DecodeString(parts[1])
	if err != nil || len(salt) == 0 {
		return nil, nil, false
	}
	sum, err = hex.DecodeString(parts[2])
	if err != nil || len(sum) != sha256.Size {
		return nil, nil, false
	}
	return salt, sum, true
}

// randomSecret returns an unguessable throw-away password.
func randomSecret() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// Redact returns a copy of cfg that is safe to send to API clients: the admin
// password hash, the hub / remote passwords, every uplinks[].enrolToken,
// aggregator.outputToken and the aggregator.tlsKey path are replaced by ""
// (not set) or "set". MergeSecrets reverses this for all of them (uplink
// enrol tokens are matched by uplink id).
func Redact(cfg model.Config) model.Config {
	out := Clone(cfg)
	out.HTTP.AdminPasswordHash = redacted(out.HTTP.AdminPasswordHash)
	out.Hub.Password = redacted(out.Hub.Password)
	out.Remote.Password = redacted(out.Remote.Password)
	out.Aggregator.OutputToken = redacted(out.Aggregator.OutputToken)
	out.Aggregator.TLSKey = redacted(out.Aggregator.TLSKey)
	for i := range out.Uplinks {
		out.Uplinks[i].EnrolToken = redacted(out.Uplinks[i].EnrolToken)
	}
	return out
}

// RedactPublic is Redact for callers that are not admin: it additionally
// strips credentials and query strings from feed URLs (see RedactURL) and
// hides navData.photoContact (the owner's e-mail or URL). Admins get Redact
// so that a GET/PUT round trip keeps these fields.
func RedactPublic(cfg model.Config) model.Config {
	out := Redact(cfg)
	for i := range out.Feeds {
		out.Feeds[i].URL = RedactURL(out.Feeds[i].URL)
	}
	out.NavData.PhotoContact = redacted(out.NavData.PhotoContact)
	out.NavData.Routes.Contact = redacted(out.NavData.Routes.Contact)
	return out
}

// RedactURL removes the user:password and the query string / fragment of a
// URL ("http://u:p@h/x?apikey=k" -> "http://h/x"). Values that do not parse
// as an absolute URL are returned with everything after '?' and before the
// last '@' removed.
func RedactURL(s string) string {
	if s == "" {
		return ""
	}
	u, err := url.Parse(s)
	if err != nil || u.Host == "" {
		if i := strings.IndexAny(s, "?#"); i >= 0 {
			s = s[:i]
		}
		if i := strings.LastIndexByte(s, '@'); i >= 0 {
			s = s[i+1:]
		}
		return s
	}
	u.User = nil
	u.RawQuery, u.ForceQuery, u.Fragment, u.RawFragment = "", false, "", ""
	return u.String()
}

// SameUplinkTarget reports whether two uplinks address the same server:
// host (case-insensitive), port and server pin. A kept (redacted) enrol
// token is only carried over when they do, so a settings change cannot
// send the stored token to another server.
func SameUplinkTarget(a, b model.Uplink) bool {
	return strings.EqualFold(strings.TrimSpace(a.Host), strings.TrimSpace(b.Host)) && a.Port == b.Port &&
		strings.TrimSpace(a.ServerPin) == strings.TrimSpace(b.ServerPin)
}

// IsPasswordHash reports whether s is a stored password hash (as produced
// by HashPassword, or the former SHA-256 form) rather than a plain
// password.
func IsPasswordHash(s string) bool { return validHash(s) }

func redacted(s string) string {
	if s == "" {
		return ""
	}
	return SecretSet
}

// MergeSecrets resolves the secret fields of a config received from an API
// client against the currently stored config and returns the result (all
// other fields are taken from incoming unchanged):
//
//	"" or "set"      -> keep current secret
//	"clear"          -> no secret
//	"plain:<text>"   -> <text> is the new secret ("plain:" -> no secret)
//	other            -> the value is the new secret
//
// For http.adminPasswordHash the new secret is the plain admin password and
// is stored hashed (see HashPassword); a value that already is a well-formed
// hash is stored as is. The returned config therefore never contains a plain
// admin password or a marker.
func MergeSecrets(incoming, current model.Config) model.Config {
	out := Clone(incoming)
	out.Hub.Password = mergeSecret(incoming.Hub.Password, current.Hub.Password)
	out.Remote.Password = mergeSecret(incoming.Remote.Password, current.Remote.Password)
	out.Aggregator.OutputToken = mergeSecret(incoming.Aggregator.OutputToken, current.Aggregator.OutputToken)
	out.Aggregator.TLSKey = mergeSecret(incoming.Aggregator.TLSKey, current.Aggregator.TLSKey)
	for i := range out.Uplinks {
		cur := ""
		for _, c := range current.Uplinks {
			if c.ID != "" && c.ID == out.Uplinks[i].ID {
				// The stored token goes only to the server it was issued
				// for (SEC-11): pointing the uplink elsewhere needs the
				// token to be entered again.
				if SameUplinkTarget(c, out.Uplinks[i]) {
					cur = c.EnrolToken
				}
				break
			}
		}
		out.Uplinks[i].EnrolToken = mergeSecret(out.Uplinks[i].EnrolToken, cur)
	}

	switch in := incoming.HTTP.AdminPasswordHash; {
	case in == "" || in == SecretSet:
		out.HTTP.AdminPasswordHash = current.HTTP.AdminPasswordHash
	case in == SecretClear:
		out.HTTP.AdminPasswordHash = ""
	case strings.HasPrefix(in, PlainPrefix):
		out.HTTP.AdminPasswordHash = HashPassword(in[len(PlainPrefix):])
	case validHash(in):
		out.HTTP.AdminPasswordHash = in
	default:
		out.HTTP.AdminPasswordHash = HashPassword(in)
	}
	return out
}

func mergeSecret(in, cur string) string {
	switch {
	case in == "" || in == SecretSet:
		return cur
	case in == SecretClear:
		return ""
	case strings.HasPrefix(in, PlainPrefix):
		return in[len(PlainPrefix):]
	}
	return in
}

// ApplyUpdate is the PUT /api/config pipeline: MergeSecrets(incoming,
// current) followed by Validate. It returns the config to save and apply
// plus the list of corrections Validate made.
func ApplyUpdate(incoming, current model.Config) (model.Config, []string) {
	return Validate(MergeSecrets(incoming, current))
}
