// Package auth holds the account, session and request-trust primitives of
// aggregator mode (docs/aggregator/DESIGN.md §7).
//
// It is deliberately independent of net/http routing and of storage:
//
//   - password.go: PBKDF2-SHA256 password hashes (600 000 iterations by
//     default), self-describing so the cost can be raised later.
//   - totp.go, recovery.go: RFC 6238 time-based one-time passwords with
//     replay protection, and single-use recovery codes.
//   - proxy.go: which peer may set X-Forwarded-For, the resolved client
//     address, and the rule that a proxied request is never "local".
//   - csrf.go: per-session synchroniser tokens for cookie sessions.
//   - session.go: scoped sessions (public / owner / admin) and the
//     SessionStore interface with an in-memory implementation. A persistent
//     store (stations.db) is provided later by internal/registry.
//   - account.go: owner and admin accounts and the AccountStore interface
//     with an in-memory implementation.
//   - ratelimit.go: the login throttle (exponential back-off per key) and a
//     sliding-window limiter (claims, registrations).
//   - service.go: Service ties these together: login with optional or
//     mandatory TOTP, owner registration, password change, TOTP enrolment,
//     recovery codes, admin initialisation, break-glass reset and the
//     one-time console bootstrap token.
//
// Standalone mode does not use this package for its own admin login (it
// keeps the single admin password in config.json, see internal/server/auth.go);
// only the trusted-proxy rule ("a proxied request from loopback is not
// local") and the optional CSRF header check are shared.
//
// Secrets at rest: session tokens are stored as SHA-256 hashes (Session.ID),
// passwords as PBKDF2 hashes, recovery codes as SHA-256 hashes. TOTP shared
// secrets must be stored as they are (the server needs them to compute
// codes); a persistent AccountStore should keep them in stations.db, which
// is a secret file like config.json.
package auth
