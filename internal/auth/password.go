package auth

import (
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Password hash format (self-describing, one line, no colons so it can be
// stored anywhere):
//
//	pbkdf2-sha256$<iterations>$<base64url salt>$<base64url key>
//
// with a 16-byte random salt and a 32-byte derived key.
const (
	// DefaultIterations is the PBKDF2-SHA256 work factor for new hashes
	// (DESIGN §7; OWASP 2023 recommendation for PBKDF2-HMAC-SHA256).
	DefaultIterations = 600_000
	// MinVerifyIterations / MaxVerifyIterations bound the cost read from a
	// stored hash, so a corrupt or hostile value can neither make
	// verification trivially cheap nor tie up a CPU for minutes.
	MinVerifyIterations = 1_000
	MaxVerifyIterations = 10_000_000

	hashPrefix   = "pbkdf2-sha256"
	pwSaltBytes  = 16
	pwKeyBytes   = 32
	maxHashLen   = 256
	minPassword  = 8    // owners
	minAdminPass = 12   // admin
	maxPassword  = 1024 // bytes; longer inputs are refused before hashing
)

// Password policy errors.
var (
	ErrPasswordTooShort = errors.New("password is too short")
	ErrPasswordTooLong  = errors.New("password is too long")
	ErrPasswordInvalid  = errors.New("password must be valid UTF-8 without control characters")
)

// iterations is the cost used by HashPassword; tests lower it.
var iterations = DefaultIterations

// HashPassword returns the stored form of pw using DefaultIterations.
func HashPassword(pw string) (string, error) {
	return HashPasswordCost(pw, iterations)
}

// HashPasswordCost hashes pw with an explicit iteration count (clamped to
// [MinVerifyIterations, MaxVerifyIterations]).
func HashPasswordCost(pw string, iter int) (string, error) {
	if len(pw) > maxPassword {
		return "", ErrPasswordTooLong
	}
	iter = min(max(iter, MinVerifyIterations), MaxVerifyIterations)
	salt := make([]byte, pwSaltBytes)
	_, _ = rand.Read(salt)
	key, err := pbkdf2.Key(sha256.New, pw, salt, iter, pwKeyBytes)
	if err != nil {
		return "", err
	}
	enc := base64.RawURLEncoding
	return hashPrefix + "$" + strconv.Itoa(iter) + "$" + enc.EncodeToString(salt) + "$" + enc.EncodeToString(key), nil
}

type parsedHash struct {
	iter      int
	salt, key []byte
}

func parsePasswordHash(h string) (parsedHash, bool) {
	if len(h) > maxHashLen {
		return parsedHash{}, false
	}
	parts := strings.Split(h, "$")
	if len(parts) != 4 || parts[0] != hashPrefix {
		return parsedHash{}, false
	}
	iter, err := strconv.Atoi(parts[1])
	if err != nil || iter < MinVerifyIterations || iter > MaxVerifyIterations {
		return parsedHash{}, false
	}
	enc := base64.RawURLEncoding
	salt, err1 := enc.DecodeString(parts[2])
	key, err2 := enc.DecodeString(parts[3])
	if err1 != nil || err2 != nil || len(salt) < 8 || len(key) < 16 || len(key) > 64 {
		return parsedHash{}, false
	}
	return parsedHash{iter: iter, salt: salt, key: key}, true
}

// ValidPasswordHash reports whether h is a well-formed hash of this package.
func ValidPasswordHash(h string) bool {
	_, ok := parsePasswordHash(h)
	return ok
}

// VerifyPassword reports whether pw matches hash. The comparison is
// constant-time; a malformed hash matches nothing.
func VerifyPassword(hash, pw string) bool {
	p, ok := parsePasswordHash(hash)
	if !ok || len(pw) > maxPassword {
		return false
	}
	got, err := pbkdf2.Key(sha256.New, pw, p.salt, p.iter, len(p.key))
	if err != nil {
		return false
	}
	return subtle.ConstantTimeCompare(got, p.key) == 1
}

// NeedsRehash reports whether hash was made with fewer iterations than the
// current default (callers re-hash after a successful login).
func NeedsRehash(hash string) bool { return needsRehash(hash, iterations) }

func needsRehash(hash string, iter int) bool {
	p, ok := parsePasswordHash(hash)
	return !ok || p.iter < iter
}

// CheckPasswordPolicy validates a new password. admin selects the stricter
// minimum length. Length is counted in characters (runes).
func CheckPasswordPolicy(pw string, admin bool) error {
	if len(pw) > maxPassword {
		return ErrPasswordTooLong
	}
	if !utf8.ValidString(pw) {
		return ErrPasswordInvalid
	}
	for _, r := range pw {
		if r < 0x20 || r == 0x7f {
			return ErrPasswordInvalid
		}
	}
	need := minPassword
	if admin {
		need = minAdminPass
	}
	if utf8.RuneCountInString(pw) < need {
		return ErrPasswordTooShort
	}
	return nil
}
