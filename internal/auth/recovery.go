package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"strings"
)

// Recovery codes let an account holder log in without the authenticator
// (lost phone; DESIGN §15 "account recovery without e-mail"). Each code is
// 10 characters of Crockford-style base32 (50 bits), shown as XXXXX-XXXXX,
// valid once. Only SHA-256 hashes are stored: the codes are random and long
// enough that a slow hash adds nothing.
const (
	RecoveryCodeCount = 10
	recoveryChars     = 10
	recoveryAlphabet  = "0123456789ABCDEFGHJKMNPQRSTVWXYZ" // no I, L, O, U
)

// NewRecoveryCodes returns n fresh codes for display and their hashes for
// storage.
func NewRecoveryCodes(n int) (codes, hashes []string) {
	for range n {
		var b [recoveryChars]byte
		_, _ = rand.Read(b[:])
		var sb strings.Builder
		for i, x := range b {
			if i == recoveryChars/2 {
				sb.WriteByte('-')
			}
			sb.WriteByte(recoveryAlphabet[x&31])
		}
		c := sb.String()
		codes = append(codes, c)
		hashes = append(hashes, hashRecovery(c))
	}
	return codes, hashes
}

// normRecovery canonicalises user input: case, separators and the
// look-alike letters are forgiven.
func normRecovery(c string) string {
	c = strings.ToUpper(c)
	c = strings.NewReplacer("-", "", " ", "", "I", "1", "L", "1", "O", "0").Replace(c)
	return c
}

func hashRecovery(c string) string {
	sum := sha256.Sum256([]byte("airview-recovery\x00" + normRecovery(c)))
	return hex.EncodeToString(sum[:])
}

// LooksLikeRecoveryCode reports whether s has the shape of a recovery code
// (as opposed to a TOTP code).
func LooksLikeRecoveryCode(s string) bool {
	n := normRecovery(s)
	if len(n) != recoveryChars {
		return false
	}
	for i := 0; i < len(n); i++ {
		if !strings.ContainsRune(recoveryAlphabet, rune(n[i])) {
			return false
		}
	}
	return true
}

// UseRecoveryCode looks for code among hashes. It returns the remaining
// hashes (the matched one removed) and whether it matched.
func UseRecoveryCode(hashes []string, code string) ([]string, bool) {
	if !LooksLikeRecoveryCode(code) {
		return hashes, false
	}
	h := hashRecovery(code)
	idx := -1
	for i, x := range hashes {
		if subtle.ConstantTimeCompare([]byte(x), []byte(h)) == 1 && idx < 0 {
			idx = i
		}
	}
	if idx < 0 {
		return hashes, false
	}
	out := make([]string, 0, len(hashes)-1)
	out = append(out, hashes[:idx]...)
	out = append(out, hashes[idx+1:]...)
	return out, true
}
