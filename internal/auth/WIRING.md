# internal/auth — wiring notes

Owner: WP-A (Phase 0). Design: `docs/aggregator/DESIGN.md` §7, §8.4, §8.5, §15.

## What the package provides

| File | Contents |
|---|---|
| `password.go` | `HashPassword` / `HashPasswordCost` / `VerifyPassword` / `NeedsRehash` / `CheckPasswordPolicy`. Format `pbkdf2-sha256$<iter>$<b64url salt>$<b64url key>`, 600 000 iterations by default, verification bounded to 1 000 … 10 000 000 iterations. Owners need 8+ characters, admins 12+. |
| `totp.go` | `TOTP` (RFC 6238 over RFC 4226 HOTP; SHA1/SHA256/SHA512, 6-8 digits), `NewTOTPSecret`, `ParseTOTPSecret`, `Verify(code, now, lastStep)` with ±1 step skew and replay protection, `URI` (otpauth://). |
| `recovery.go` | 10 single-use recovery codes `XXXXX-XXXXX` (50 bits each), stored as SHA-256. |
| `proxy.go` | `ProxyTrust` (`ParseTrustedProxies`, `MustTrustedProxies`, `Resolve`), `Client{IP, Peer, Proxied, ViaTrustedProxy, Secure}`, `Client.Local()`, `HasForwardingHeaders`, `PeerAddr`, `SameClient` (IPv4 exact / IPv6 /64). |
| `csrf.go` | `NewCSRFToken`, `CheckCSRF(r, want)`, `SafeMethod`, header `X-CSRF-Token`. |
| `session.go` | `Session` (scoped, stores only the token hash), `SessionStore` interface, `MemorySessions`, `NewSessionToken`, `SessionID`. |
| `account.go` | `Account`, `Role` (owner/admin), `AccountStore` interface, `MemoryAccounts`, `NormalizeUsername`. |
| `ratelimit.go` | `Throttle` (the standalone login back-off, per key), `WindowLimiter` (N per sliding window; claims, sign-ups). |
| `service.go` | `Service`: `Login`, `Authenticate`, `Logout`, `LogoutOthers`, `Register` (off with `Options.RegistrationClosed`), `CreateOwner`, `ChangePassword`, `BeginTOTP`/`ConfirmTOTP`/`DisableTOTP`, `RegenerateRecovery`, `SetDisabled`, `AdminSetPassword`, `DeleteAccount`, `AdminReady`, `InitAdmin`, `AddAdmin`, `ConfirmAdminTOTP`, `ResetAdmin`, `MintBootstrapToken`/`BootstrapBegin`/`BootstrapFinish`, `Purge`, `RunJanitor`. |

## Rules implemented

- **Trusted proxies.** `X-Forwarded-For` / `X-Forwarded-Proto` are honoured only when the TCP peer is inside `Aggregator.TrustedProxies`. The client is the rightmost untrusted XFF entry (entries further left are client-written and ignored). Standalone trusts no proxy.
- **Proxied loopback is not local.** A request carrying `X-Forwarded-For`, `Forwarded`, `X-Real-IP` or `X-Forwarded-Host` is never "from this machine", even with a loopback peer. This also applies in standalone mode (`internal/server.isLoopback`), so a reverse proxy on the same host no longer turns internet clients into admins when an admin password is set. Direct loopback in standalone is unchanged.
- **Aggregator admin** = an account with role admin, a correct password **and** a TOTP or recovery code (`Session.MFA`). Loopback is never admin; there is no "no password = open".
- **Owners**: user name (3-32 of `a-z 0-9 . _ -`, case-insensitive) + PBKDF2 password, optional TOTP with recovery codes. No e-mail.
- **Sessions**: token = 32 random bytes; only `SHA-256(token)` is stored. Owner: 30 days absolute, 14 days idle. Admin: 12 h absolute, 2 h idle. A password / TOTP change, disable or reset bumps `Account.CredEpoch`, which ends all older sessions (the session making the change is carried over). No raw IPs are stored in sessions (§8.5).
- **CSRF**: every session has its own token, returned by login and `GET /api/session` as `csrfToken`. In aggregator mode every non-GET request with a session cookie must send it as `X-CSRF-Token`. Cookies are `HttpOnly; SameSite=Strict; Secure` when the request came over TLS (directly or per trusted `X-Forwarded-Proto`).
- **Rate limits**: per client 5 free failures then 2 s doubling to 5 min (the standalone rule); per user name 10 free failures, same back-off, so one account cannot be guessed from many addresses. Sign-ups: 5 per hour per client. Bootstrap token guesses: same as per-client logins. At most GOMAXPROCS/2 PBKDF2 computations run at once. Unknown user names cost one PBKDF2 like a wrong password.

## How the server uses it (`internal/server/auth*.go`, WP-A)

- `s.aggregatorMode()` — `config.Mode == "aggregator"`.
- `s.isAdmin(r)` — standalone rules, or admin-scoped MFA session in aggregator mode.
- `s.client(r)` / `s.remoteIP(r)` — trusted-proxy aware client address (aggregator), TCP peer (standalone). **Use `s.remoteIP(r)` instead of `clientIP(r)` for rate limits, per-IP caps and feed-check matching.**
- `s.viewer(r) model.Viewer` — aggregator: from the session (public without one); standalone: admin scope (all data visible, only mutations gated).
- `s.requireScope(w, r, need)` — 401/403 helper for scoped routes.
- `s.netSession(r)` — the aggregator `Session` and `Account`.
- `s.SetAccountService(svc)` / `s.AccountService()` — install the persistent service; without it an in-memory one is used.
- `AuthRoutes()` — the account endpoints (`/api/auth/*`), to be appended to the route table by WP-H.

## What others must wire (requests filed in docs/aggregator/HOOKS.md)

1. **Registry (WP-R)**: implement `auth.AccountStore` and `auth.SessionStore` on `stations.db` (tables `accounts`, `sessions`; store `Session.ID`, never a token; keep the TOTP secret as stored; `RecoveryHashes` as JSON). Coalesce `Touch` writes if needed.
2. **Engine / cmd (WP-E)**: build `auth.NewService(registry.Accounts(), registry.Sessions(), auth.Options{Issuer: cfg.Aggregator.Name})`, call `server.SetAccountService`, run `svc.RunJanitor(ctx, 10*time.Minute)`. At startup in aggregator mode: if `!svc.AdminReady()`, mint `svc.MintBootstrapToken()` and print it to the console / journal only (never to logs shipped elsewhere), and refuse to open the feeder listeners until an admin exists. CLI: `airview admin init` (`InitAdmin` + prompt for the first code + `ConfirmAdminTOTP`, print recovery codes) and `airview admin reset --data <dir>` (`ResetAdmin` + `ConfirmAdminTOTP`).
3. **Server (WP-H)**: append `AuthRoutes()` to `routes`, regenerate `docs/API.md` (new `sessionInfo` fields), exempt `/api/auth/*` from the "mutating route is not admin" lint, and switch `clientIP(r)` → `s.remoteIP(r)` in `sse.go` and `api_core.go` (also avoid logging raw IPs in aggregator mode).
4. **Web (WP-W)**: send `X-CSRF-Token: <csrfToken>` on every non-GET request; aggregator login form with user name, password and a second step when the reply has `codeRequired: true`; account pages for TOTP enrolment (`/api/auth/totp/*`), recovery codes, password change and sessions; first-run setup page using `/api/auth/setup*`.
