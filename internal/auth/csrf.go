package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"net/http"
)

// CSRF protection for cookie sessions (DESIGN §7: "a CSRF token is
// required on every mutating request").
//
// Synchroniser-token pattern: every session gets its own random token at
// creation (Session.CSRF). The client reads it from the session endpoint
// (GET /api/session, never from a cookie) and echoes it in the
// X-CSRF-Token header on every request that is not GET/HEAD/OPTIONS. A
// foreign page can make the browser send the cookie, but it cannot read
// the token (same-origin policy, no credentialed CORS), so it cannot
// produce the header. SameSite=Strict and the Origin check stay in place
// as further layers.
const (
	// CSRFHeader carries the token on mutating requests.
	CSRFHeader = "X-CSRF-Token"
	csrfBytes  = 32
)

// NewCSRFToken returns a fresh random token (base64url, 43 characters).
func NewCSRFToken() string {
	var b [csrfBytes]byte
	_, _ = rand.Read(b[:])
	return base64.RawURLEncoding.EncodeToString(b[:])
}

// SafeMethod reports whether method is one CSRF protection does not apply
// to (it must not change state).
func SafeMethod(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions, http.MethodTrace:
		return true
	}
	return false
}

// CheckCSRF reports whether r carries the token want in CSRFHeader. An
// empty want never matches (a session without a token cannot mutate).
func CheckCSRF(r *http.Request, want string) bool {
	got := r.Header.Get(CSRFHeader)
	if want == "" || got == "" || len(got) != len(want) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}
