package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"runtime"
	"sync"
	"time"
)

// Policy holds session lifetimes.
type Policy struct {
	// OwnerTTL / AdminTTL are absolute session lifetimes.
	OwnerTTL, AdminTTL time.Duration
	// OwnerIdle / AdminIdle end a session without activity (0 = default,
	// negative = never).
	OwnerIdle, AdminIdle time.Duration
	// TouchEvery coalesces last-seen writes to the session store.
	TouchEvery time.Duration
}

// DefaultPolicy: owners 30 days (idle 14 days), admins 12 hours (idle 2
// hours), last-seen written at most once a minute per session.
func DefaultPolicy() Policy {
	return Policy{
		OwnerTTL: 30 * 24 * time.Hour, OwnerIdle: 14 * 24 * time.Hour,
		AdminTTL: 12 * time.Hour, AdminIdle: 2 * time.Hour,
		TouchEvery: time.Minute,
	}
}

func (p Policy) norm() Policy {
	d := DefaultPolicy()
	if p.OwnerTTL <= 0 {
		p.OwnerTTL = d.OwnerTTL
	}
	if p.AdminTTL <= 0 {
		p.AdminTTL = d.AdminTTL
	}
	switch {
	case p.OwnerIdle == 0:
		p.OwnerIdle = d.OwnerIdle
	case p.OwnerIdle < 0:
		p.OwnerIdle = 0
	}
	switch {
	case p.AdminIdle == 0:
		p.AdminIdle = d.AdminIdle
	case p.AdminIdle < 0:
		p.AdminIdle = 0
	}
	if p.TouchEvery <= 0 {
		p.TouchEvery = d.TouchEvery
	}
	return p
}

// Options configures a Service. Zero values take defaults.
type Options struct {
	// Issuer is the TOTP issuer shown in authenticator apps
	// (Aggregator.Name; default "AirView").
	Issuer string
	Policy Policy
	// MaxConcurrentHashes bounds simultaneous PBKDF2 computations so a
	// login flood cannot starve ingest of CPU (default GOMAXPROCS/2, >= 1).
	MaxConcurrentHashes int
	// IPThrottle / UserThrottle override the login throttles. Defaults:
	// per client 5 free failures, then 2 s doubling to 5 min (the
	// standalone rule); per user name 10 free failures, then 2 s doubling
	// to 5 min, so one account cannot be guessed from many addresses.
	IPThrottle, UserThrottle *Throttle
	// Registrations limits owner sign-ups per client (default 5 per hour).
	Registrations *WindowLimiter
	// Attempts limits password checks (logins, sign-ups) per client key
	// before any hashing (default 30 per minute).
	Attempts *WindowLimiter
	// RegistrationClosed refuses self-service owner sign-ups (accounts are
	// then created by an admin with CreateOwner).
	RegistrationClosed bool
	// PasswordIterations is the PBKDF2 work factor for new hashes
	// (default DefaultIterations = 600 000). Lower values are for tests.
	PasswordIterations int
	// Now replaces the clock (tests).
	Now func() time.Time
}

// Service implements login, sessions and account maintenance on top of an
// AccountStore and a SessionStore. It is safe for concurrent use.
type Service struct {
	accounts AccountStore
	sessions SessionStore
	policy   Policy
	issuer   string
	now      func() time.Time

	ipT, userT *Throttle
	// unknownT counts failures for user names without an account, so a
	// flood of random names cannot evict the entries of real accounts.
	unknownT *Throttle
	// attempts bounds password checks per client before any PBKDF2 work.
	attempts  *WindowLimiter
	reg       *WindowLimiter
	regClosed bool
	hashSem   chan struct{}
	iter      int
	dummy     func() string
	locks     [64]sync.Mutex

	bootMu      sync.Mutex
	bootHash    [32]byte
	bootExpires time.Time
}

// NewService returns a Service. accounts and sessions must not be nil.
func NewService(accounts AccountStore, sessions SessionStore, o Options) *Service {
	if o.Issuer == "" {
		o.Issuer = "AirView"
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.MaxConcurrentHashes <= 0 {
		o.MaxConcurrentHashes = max(runtime.GOMAXPROCS(0)/2, 1)
	}
	if o.IPThrottle == nil {
		o.IPThrottle = NewThrottle(5, 15*time.Minute, 5*time.Minute, 16384)
	}
	if o.UserThrottle == nil {
		o.UserThrottle = NewThrottle(10, 15*time.Minute, 5*time.Minute, 16384)
	}
	if o.Registrations == nil {
		o.Registrations = NewWindowLimiter(5, time.Hour, 16384)
	}
	if o.Attempts == nil {
		o.Attempts = NewWindowLimiter(30, time.Minute, 65536)
		o.Attempts.now = o.Now
	}
	if o.PasswordIterations <= 0 {
		o.PasswordIterations = iterations
	}
	iter := min(max(o.PasswordIterations, MinVerifyIterations), MaxVerifyIterations)
	return &Service{
		accounts: accounts, sessions: sessions, policy: o.Policy.norm(), issuer: o.Issuer, now: o.Now,
		ipT: o.IPThrottle, userT: o.UserThrottle, reg: o.Registrations, regClosed: o.RegistrationClosed,
		unknownT: NewThrottle(o.UserThrottle.Free, o.UserThrottle.Window, o.UserThrottle.MaxDelay, o.UserThrottle.MaxKeys),
		attempts: o.Attempts,
		hashSem:  make(chan struct{}, o.MaxConcurrentHashes), iter: iter,
		dummy: sync.OnceValue(func() string {
			h, _ := HashPasswordCost("airview-dummy-password", iter)
			return h
		}),
	}
}

// Accounts returns the account store.
func (s *Service) Accounts() AccountStore { return s.accounts }

// Sessions returns the session store.
func (s *Service) Sessions() SessionStore { return s.sessions }

// Policy returns the effective session policy.
func (s *Service) Policy() Policy { return s.policy }

// Errors returned by Service. Handlers map them to HTTP status codes; the
// messages are safe to show to users.
var (
	ErrBadCredentials  = errors.New("wrong user name or password")
	ErrCodeRequired    = errors.New("a one-time code is required")
	ErrBadCode         = errors.New("wrong or already used one-time code")
	ErrAdminNoTOTP     = errors.New("the admin account has no authenticator yet: finish setup with 'airview admin init' or the console bootstrap token")
	ErrAccountDisabled = errors.New("this account is disabled")
	ErrNoPendingTOTP   = errors.New("no authenticator setup is in progress")
	ErrTOTPMandatory   = errors.New("an admin account cannot turn off its authenticator")
	ErrTOTPNotEnabled  = errors.New("no authenticator is set up")
	ErrBusy            = errors.New("too many logins in progress, try again")
	ErrBootstrapToken  = errors.New("invalid or expired setup token")
	ErrAdminExists     = errors.New("an admin account is already set up")
	ErrNotAdmin        = errors.New("not an admin account")
	ErrRegistrationOff = errors.New("sign-up is closed on this network; ask the operator for an account")
)

// ThrottledError is returned while a client or user name is throttled.
type ThrottledError struct{ RetryAfter time.Duration }

func (e *ThrottledError) Error() string {
	return fmt.Sprintf("too many failed attempts, try again in %d s", int(e.RetryAfter.Seconds())+1)
}

func (s *Service) lockAcct(id int64) func() {
	m := &s.locks[uint64(id)%uint64(len(s.locks))]
	m.Lock()
	return m.Unlock
}

func (s *Service) acquireHash(ctx context.Context) (func(), error) {
	select {
	case s.hashSem <- struct{}{}:
		return func() { <-s.hashSem }, nil
	case <-ctx.Done():
		return nil, ErrBusy
	}
}

func (s *Service) verify(ctx context.Context, hash, pw string) (bool, error) {
	release, err := s.acquireHash(ctx)
	if err != nil {
		return false, err
	}
	defer release()
	return VerifyPassword(hash, pw), nil
}

func (s *Service) hash(ctx context.Context, pw string) (string, error) {
	release, err := s.acquireHash(ctx)
	if err != nil {
		return "", err
	}
	defer release()
	return HashPasswordCost(pw, s.iter)
}

// LoginRequest is one login attempt.
type LoginRequest struct {
	Username string
	Password string
	// Code is a TOTP code or a recovery code ("" on the first step).
	Code string
	// Client keys the per-client throttle (the resolved client IP).
	Client string
}

// LoginResult is a successful login.
type LoginResult struct {
	Session Session
	Token   string // the bearer token for the cookie; never stored
	Account Account
	// RecoveryLeft is the number of unused recovery codes (when a recovery
	// code was used, so the UI can warn).
	RecoveryLeft int
	UsedRecovery bool
}

func (s *Service) throttled(ipKey, userKey string) error {
	if d := max(s.ipT.Blocked(ipKey), s.userT.Blocked(userKey), s.unknownT.Blocked(userKey)); d > 0 {
		return &ThrottledError{RetryAfter: d}
	}
	return nil
}

func (s *Service) failed(ipKey, userKey string, known bool, err error) error {
	ut := s.userT
	if !known {
		ut = s.unknownT
	}
	d := max(s.ipT.Fail(ipKey), ut.Fail(userKey))
	if d > 0 {
		return fmt.Errorf("%w (%w)", err, &ThrottledError{RetryAfter: d})
	}
	return err
}

// loginKeys returns the throttle keys of a login: the client (IPv4
// address or IPv6 /64) and the user name together with the client. The
// user key includes the client so that failures from other addresses
// cannot lock the real owner out (SEC-02): anyone could otherwise keep the
// (guessable) admin name blocked indefinitely. Guessing one account from
// many addresses is bounded by the per-client throttle, the per-client
// attempt limit and the PBKDF2 cost.
func loginKeys(name, client string) (ipKey, userKey string) {
	ck := ClientKeyString(client)
	return "ip:" + ck, "user:" + name + "|" + ck
}

// Login checks credentials and creates a session.
//
// Admin accounts always need a second factor; owner accounts need one when
// they have enabled TOTP. Without Code such accounts get ErrCodeRequired
// after a correct password (the UI then asks for the code and repeats the
// request with all three fields). Wrong passwords and wrong codes both count
// against the per-client and per-user-name throttles.
func (s *Service) Login(ctx context.Context, req LoginRequest) (LoginResult, error) {
	name, nameOK := NormalizeUsername(req.Username)
	ipKey, userKey := loginKeys(name, req.Client)
	if err := s.throttled(ipKey, userKey); err != nil {
		return LoginResult{}, err
	}
	if ok, d := s.attempts.Allow(ipKey); !ok {
		return LoginResult{}, &ThrottledError{RetryAfter: d}
	}
	var acct Account
	var err error
	if nameOK {
		acct, err = s.accounts.ByUsername(ctx, name)
		if err != nil && !errors.Is(err, ErrAccountNotFound) {
			return LoginResult{}, err
		}
	} else {
		err = ErrAccountNotFound
	}
	if err != nil {
		// Unknown user: spend the same time as a wrong password.
		if _, herr := s.verify(ctx, s.dummy(), req.Password); herr != nil {
			return LoginResult{}, herr
		}
		return LoginResult{}, s.failed(ipKey, userKey, false, ErrBadCredentials)
	}
	ok, err := s.verify(ctx, acct.PasswordHash, req.Password)
	if err != nil {
		return LoginResult{}, err
	}
	if !ok {
		return LoginResult{}, s.failed(ipKey, userKey, true, ErrBadCredentials)
	}
	if acct.Disabled {
		return LoginResult{}, ErrAccountDisabled
	}
	if acct.Role == RoleAdmin && !acct.TOTPEnabled() {
		return LoginResult{}, ErrAdminNoTOTP
	}
	res := LoginResult{}
	unlock := s.lockAcct(acct.ID)
	defer unlock()
	// Re-read under the lock: TOTPLastStep / recovery codes may have moved.
	if acct, err = s.accounts.ByID(ctx, acct.ID); err != nil {
		return LoginResult{}, err
	}
	now := s.now()
	mfa := false
	if acct.TOTPEnabled() {
		if req.Code == "" {
			return LoginResult{}, ErrCodeRequired
		}
		used, ok := s.secondFactor(&acct, req.Code, now)
		if !ok {
			return LoginResult{}, s.failed(ipKey, userKey, true, ErrBadCode)
		}
		mfa, res.UsedRecovery, res.RecoveryLeft = true, used, len(acct.RecoveryHashes)
	}
	s.ipT.Success(ipKey)
	s.userT.Success(userKey)
	if needsRehash(acct.PasswordHash, s.iter) {
		if h, err := s.hash(ctx, req.Password); err == nil {
			acct.PasswordHash = h
		}
	}
	acct.LastLogin = now
	acct.Updated = now
	if err := s.accounts.Update(ctx, acct); err != nil {
		return LoginResult{}, err
	}
	sess, tok, err := s.newSession(ctx, acct, mfa)
	if err != nil {
		return LoginResult{}, err
	}
	res.Session, res.Token, res.Account = sess, tok, acct
	return res, nil
}

// secondFactor checks a TOTP or recovery code and updates the account's
// replay state in place (the caller stores it). It reports whether a
// recovery code was used.
func (s *Service) secondFactor(a *Account, code string, now time.Time) (recovery, ok bool) {
	if LooksLikeRecoveryCode(code) {
		rest, ok := UseRecoveryCode(a.RecoveryHashes, code)
		if ok {
			a.RecoveryHashes = rest
		}
		return true, ok
	}
	t, err := ParseTOTPSecret(a.TOTPSecret)
	if err != nil {
		return false, false
	}
	step, ok := t.Verify(code, now, a.TOTPLastStep)
	if ok {
		a.TOTPLastStep = step
	}
	return false, ok
}

func (s *Service) newSession(ctx context.Context, a Account, mfa bool) (Session, string, error) {
	tok, id := NewSessionToken()
	now := s.now()
	sess := Session{
		ID: id, Scope: a.Role.Scope(), AccountID: a.ID, Username: a.Username,
		CredEpoch: a.CredEpoch, CSRF: NewCSRFToken(), Created: now, LastSeen: now, MFA: mfa,
	}
	if a.Role == RoleAdmin {
		sess.Expires, sess.Idle = now.Add(s.policy.AdminTTL), s.policy.AdminIdle
	} else {
		sess.Expires, sess.Idle = now.Add(s.policy.OwnerTTL), s.policy.OwnerIdle
	}
	if err := s.sessions.Put(ctx, sess); err != nil {
		return Session{}, "", err
	}
	return sess, tok, nil
}

// Authenticate resolves a bearer token to its session and account. Every
// failure (unknown, expired, idle, account gone or disabled, credentials
// changed, role changed) is ErrSessionNotFound, and stale sessions are
// deleted.
func (s *Service) Authenticate(ctx context.Context, token string) (Session, Account, error) {
	if token == "" || len(token) > 128 {
		return Session{}, Account{}, ErrSessionNotFound
	}
	id := SessionID(token)
	sess, err := s.sessions.Get(ctx, id)
	if err != nil {
		return Session{}, Account{}, err
	}
	now := s.now()
	drop := func() (Session, Account, error) {
		_ = s.sessions.Delete(ctx, id)
		return Session{}, Account{}, ErrSessionNotFound
	}
	if !sess.Valid(now) {
		return drop()
	}
	acct, err := s.accounts.ByID(ctx, sess.AccountID)
	if errors.Is(err, ErrAccountNotFound) {
		return drop()
	}
	if err != nil {
		return Session{}, Account{}, err
	}
	if acct.Disabled || acct.CredEpoch != sess.CredEpoch || acct.Role.Scope() != sess.Scope ||
		(acct.Role == RoleAdmin && !sess.MFA) {
		return drop()
	}
	if now.Sub(sess.LastSeen) >= s.policy.TouchEvery {
		_ = s.sessions.Touch(ctx, id, now)
		sess.LastSeen = now
	}
	return sess, acct, nil
}

// Logout ends the session of token (no error if it does not exist).
func (s *Service) Logout(ctx context.Context, token string) error {
	if token == "" {
		return nil
	}
	return s.sessions.Delete(ctx, SessionID(token))
}

// LogoutOthers ends every session of the account except keepID.
func (s *Service) LogoutOthers(ctx context.Context, accountID int64, keepID string) (int, error) {
	return s.sessions.DeleteAccount(ctx, accountID, keepID)
}

// Register creates an owner account (self-service sign-up). client keys
// the sign-up rate limit.
func (s *Service) Register(ctx context.Context, username, password, client string) (Account, error) {
	if s.regClosed {
		return Account{}, ErrRegistrationOff
	}
	ck := "ip:" + ClientKeyString(client)
	if ok, d := s.attempts.Allow(ck); !ok {
		return Account{}, &ThrottledError{RetryAfter: d}
	}
	if ok, d := s.reg.Allow(ck); !ok {
		return Account{}, &ThrottledError{RetryAfter: d}
	}
	return s.CreateOwner(ctx, username, password)
}

// CreateOwner creates an owner account without the sign-up checks (admin
// action, or Register after its checks).
func (s *Service) CreateOwner(ctx context.Context, username, password string) (Account, error) {
	name, ok := NormalizeUsername(username)
	if !ok {
		return Account{}, ErrUsernameInvalid
	}
	if err := CheckPasswordPolicy(password, false); err != nil {
		return Account{}, err
	}
	if _, err := s.accounts.ByUsername(ctx, name); err == nil {
		return Account{}, ErrUsernameTaken
	}
	h, err := s.hash(ctx, password)
	if err != nil {
		return Account{}, err
	}
	now := s.now()
	return s.accounts.Create(ctx, Account{Username: name, Role: RoleOwner, PasswordHash: h, CredEpoch: 1, Created: now, Updated: now})
}

// reauth checks the current password of an account (throttled like a
// login, keyed by user name) for sensitive changes.
func (s *Service) reauth(ctx context.Context, a Account, password string) error {
	userKey := "user:" + a.Username
	if d := s.userT.Blocked(userKey); d > 0 {
		return &ThrottledError{RetryAfter: d}
	}
	ok, err := s.verify(ctx, a.PasswordHash, password)
	if err != nil {
		return err
	}
	if !ok {
		s.userT.Fail(userKey)
		return ErrBadCredentials
	}
	return nil
}

// bumpEpoch increases the credential epoch, stores the account, ends all
// other sessions and carries cur (if any) over to the new epoch.
func (s *Service) bumpEpoch(ctx context.Context, a *Account, cur *Session) error {
	a.CredEpoch++
	a.Updated = s.now()
	if err := s.accounts.Update(ctx, *a); err != nil {
		return err
	}
	keep := ""
	if cur != nil && cur.AccountID == a.ID {
		keep = cur.ID
		cur.CredEpoch = a.CredEpoch
		if err := s.sessions.Put(ctx, *cur); err != nil {
			return err
		}
	}
	_, err := s.sessions.DeleteAccount(ctx, a.ID, keep)
	return err
}

// ChangePassword sets a new password after checking the old one. Other
// sessions of the account end; cur stays valid and is updated in place.
func (s *Service) ChangePassword(ctx context.Context, cur *Session, oldPW, newPW string) error {
	unlock := s.lockAcct(cur.AccountID)
	defer unlock()
	a, err := s.accounts.ByID(ctx, cur.AccountID)
	if err != nil {
		return err
	}
	if err := CheckPasswordPolicy(newPW, a.Role == RoleAdmin); err != nil {
		return err
	}
	if err := s.reauth(ctx, a, oldPW); err != nil {
		return err
	}
	h, err := s.hash(ctx, newPW)
	if err != nil {
		return err
	}
	a.PasswordHash = h
	return s.bumpEpoch(ctx, &a, cur)
}

// TOTPSetup is a pending authenticator enrolment.
type TOTPSetup struct {
	Secret string `json:"secret"` // base32, for manual entry
	URI    string `json:"uri"`    // otpauth:// for a QR code
}

func (s *Service) pending(ctx context.Context, a *Account) (TOTPSetup, error) {
	t := NewTOTPSecret()
	a.TOTPPending = t.Base32()
	a.Updated = s.now()
	if err := s.accounts.Update(ctx, *a); err != nil {
		return TOTPSetup{}, err
	}
	return TOTPSetup{Secret: t.Base32(), URI: t.URI(s.issuer, a.Username)}, nil
}

// BeginTOTP starts (or restarts) authenticator enrolment for the session's
// account after re-checking the password. An existing confirmed secret
// stays active until ConfirmTOTP replaces it.
func (s *Service) BeginTOTP(ctx context.Context, cur *Session, password string) (TOTPSetup, error) {
	unlock := s.lockAcct(cur.AccountID)
	defer unlock()
	a, err := s.accounts.ByID(ctx, cur.AccountID)
	if err != nil {
		return TOTPSetup{}, err
	}
	if err := s.reauth(ctx, a, password); err != nil {
		return TOTPSetup{}, err
	}
	return s.pending(ctx, &a)
}

func (s *Service) confirm(ctx context.Context, a *Account, code string, cur *Session) ([]string, error) {
	if a.TOTPPending == "" {
		return nil, ErrNoPendingTOTP
	}
	t, err := ParseTOTPSecret(a.TOTPPending)
	if err != nil {
		return nil, err
	}
	step, ok := t.Verify(code, s.now(), -1)
	if !ok {
		s.userT.Fail("user:" + a.Username)
		return nil, ErrBadCode
	}
	codes, hashes := NewRecoveryCodes(RecoveryCodeCount)
	a.TOTPSecret, a.TOTPPending, a.TOTPLastStep, a.RecoveryHashes = a.TOTPPending, "", step, hashes
	if cur != nil {
		cur.MFA = true
	}
	if err := s.bumpEpoch(ctx, a, cur); err != nil {
		return nil, err
	}
	return codes, nil
}

// ConfirmTOTP activates the pending secret with a first code and returns
// fresh recovery codes (shown once). Other sessions end.
func (s *Service) ConfirmTOTP(ctx context.Context, cur *Session, code string) ([]string, error) {
	unlock := s.lockAcct(cur.AccountID)
	defer unlock()
	if d := s.userT.Blocked("user:" + cur.Username); d > 0 {
		return nil, &ThrottledError{RetryAfter: d}
	}
	a, err := s.accounts.ByID(ctx, cur.AccountID)
	if err != nil {
		return nil, err
	}
	return s.confirm(ctx, &a, code, cur)
}

// DisableTOTP turns the authenticator off for an owner account (password
// and a current code or recovery code required). Admins cannot.
func (s *Service) DisableTOTP(ctx context.Context, cur *Session, password, code string) error {
	unlock := s.lockAcct(cur.AccountID)
	defer unlock()
	a, err := s.accounts.ByID(ctx, cur.AccountID)
	if err != nil {
		return err
	}
	if a.Role == RoleAdmin {
		return ErrTOTPMandatory
	}
	if !a.TOTPEnabled() {
		return ErrTOTPNotEnabled
	}
	if err := s.reauth(ctx, a, password); err != nil {
		return err
	}
	if _, ok := s.secondFactor(&a, code, s.now()); !ok {
		s.userT.Fail("user:" + a.Username)
		return ErrBadCode
	}
	a.TOTPSecret, a.TOTPPending, a.TOTPLastStep, a.RecoveryHashes = "", "", 0, nil
	return s.bumpEpoch(ctx, &a, cur)
}

// RegenerateRecovery replaces all recovery codes (password and a current
// TOTP code required) and returns the new ones.
func (s *Service) RegenerateRecovery(ctx context.Context, cur *Session, password, code string) ([]string, error) {
	unlock := s.lockAcct(cur.AccountID)
	defer unlock()
	a, err := s.accounts.ByID(ctx, cur.AccountID)
	if err != nil {
		return nil, err
	}
	if !a.TOTPEnabled() {
		return nil, ErrTOTPNotEnabled
	}
	if err := s.reauth(ctx, a, password); err != nil {
		return nil, err
	}
	if _, ok := s.secondFactor(&a, code, s.now()); !ok {
		s.userT.Fail("user:" + a.Username)
		return nil, ErrBadCode
	}
	codes, hashes := NewRecoveryCodes(RecoveryCodeCount)
	a.RecoveryHashes = hashes
	a.Updated = s.now()
	if err := s.accounts.Update(ctx, a); err != nil {
		return nil, err
	}
	return codes, nil
}

// SetDisabled enables or disables an account (admin action). Disabling
// ends all its sessions.
func (s *Service) SetDisabled(ctx context.Context, accountID int64, disabled bool) error {
	unlock := s.lockAcct(accountID)
	defer unlock()
	a, err := s.accounts.ByID(ctx, accountID)
	if err != nil {
		return err
	}
	a.Disabled = disabled
	return s.bumpEpoch(ctx, &a, nil)
}

// AdminSetPassword sets an owner's password (admin-assisted recovery);
// clearTOTP also removes the authenticator. All sessions of the account end.
func (s *Service) AdminSetPassword(ctx context.Context, accountID int64, newPW string, clearTOTP bool) error {
	unlock := s.lockAcct(accountID)
	defer unlock()
	a, err := s.accounts.ByID(ctx, accountID)
	if err != nil {
		return err
	}
	if err := CheckPasswordPolicy(newPW, a.Role == RoleAdmin); err != nil {
		return err
	}
	if a.Role == RoleAdmin && clearTOTP {
		return ErrTOTPMandatory // use ResetAdmin (break-glass CLI)
	}
	h, err := s.hash(ctx, newPW)
	if err != nil {
		return err
	}
	a.PasswordHash = h
	if clearTOTP {
		a.TOTPSecret, a.TOTPPending, a.TOTPLastStep, a.RecoveryHashes = "", "", 0, nil
	}
	return s.bumpEpoch(ctx, &a, nil)
}

// DeleteAccount removes an account and all its sessions.
func (s *Service) DeleteAccount(ctx context.Context, accountID int64) error {
	unlock := s.lockAcct(accountID)
	defer unlock()
	if _, err := s.sessions.DeleteAccount(ctx, accountID, ""); err != nil {
		return err
	}
	return s.accounts.Delete(ctx, accountID)
}

// ---------------------------------------------------------------- admin setup

// AdminReady reports whether at least one enabled admin account with a
// confirmed authenticator exists. Aggregator mode refuses to start (or
// serves only the bootstrap flow) until it does (DESIGN §7).
func (s *Service) AdminReady(ctx context.Context) (bool, error) {
	admins, err := s.accounts.ListRole(ctx, RoleAdmin)
	if err != nil {
		return false, err
	}
	for _, a := range admins {
		if !a.Disabled && a.TOTPEnabled() && ValidPasswordHash(a.PasswordHash) {
			return true, nil
		}
	}
	return false, nil
}

// InitAdmin creates an admin account (or restarts the setup of an admin
// account whose authenticator was never confirmed) and returns the pending
// authenticator. It refuses when an admin is already set up; adding more
// admins later goes through an admin session (AddAdmin) and the
// break-glass path is ResetAdmin. Used by `airview admin init` and the
// console bootstrap flow.
func (s *Service) InitAdmin(ctx context.Context, username, password string) (TOTPSetup, error) {
	ready, err := s.AdminReady(ctx)
	if err != nil {
		return TOTPSetup{}, err
	}
	if ready {
		return TOTPSetup{}, ErrAdminExists
	}
	return s.createOrRestartAdmin(ctx, username, password)
}

// AddAdmin creates an additional admin account (called from an admin
// session). It returns the pending authenticator, confirmed with
// ConfirmAdminTOTP.
func (s *Service) AddAdmin(ctx context.Context, username, password string) (TOTPSetup, error) {
	name, ok := NormalizeUsername(username)
	if !ok {
		return TOTPSetup{}, ErrUsernameInvalid
	}
	if _, err := s.accounts.ByUsername(ctx, name); err == nil {
		return TOTPSetup{}, ErrUsernameTaken
	}
	return s.createOrRestartAdmin(ctx, name, password)
}

func (s *Service) createOrRestartAdmin(ctx context.Context, username, password string) (TOTPSetup, error) {
	name, ok := NormalizeUsername(username)
	if !ok {
		return TOTPSetup{}, ErrUsernameInvalid
	}
	if err := CheckPasswordPolicy(password, true); err != nil {
		return TOTPSetup{}, err
	}
	h, err := s.hash(ctx, password)
	if err != nil {
		return TOTPSetup{}, err
	}
	now := s.now()
	a, err := s.accounts.ByUsername(ctx, name)
	switch {
	case err == nil:
		if a.Role != RoleAdmin || a.TOTPEnabled() {
			return TOTPSetup{}, ErrUsernameTaken
		}
		unlock := s.lockAcct(a.ID)
		defer unlock()
		a.PasswordHash = h
		a.CredEpoch++
	case errors.Is(err, ErrAccountNotFound):
		a, err = s.accounts.Create(ctx, Account{Username: name, Role: RoleAdmin, PasswordHash: h, CredEpoch: 1, Created: now, Updated: now})
		if err != nil {
			return TOTPSetup{}, err
		}
		unlock := s.lockAcct(a.ID)
		defer unlock()
	default:
		return TOTPSetup{}, err
	}
	return s.pending(ctx, &a)
}

// ConfirmAdminTOTP completes InitAdmin / AddAdmin / ResetAdmin with the
// first code from the authenticator and returns the recovery codes.
func (s *Service) ConfirmAdminTOTP(ctx context.Context, username, code string) ([]string, error) {
	name, _ := NormalizeUsername(username)
	if d := s.userT.Blocked("user:" + name); d > 0 {
		return nil, &ThrottledError{RetryAfter: d}
	}
	a, err := s.accounts.ByUsername(ctx, name)
	if err != nil {
		return nil, err
	}
	if a.Role != RoleAdmin {
		return nil, ErrNotAdmin
	}
	unlock := s.lockAcct(a.ID)
	defer unlock()
	if a, err = s.accounts.ByID(ctx, a.ID); err != nil {
		return nil, err
	}
	return s.confirm(ctx, &a, code, nil)
}

// ResetAdmin is the break-glass path (`airview admin reset --data <dir>`,
// run by somebody with access to the data directory): it sets a new
// password on an admin account, removes its authenticator and recovery
// codes, ends its sessions and returns a new pending authenticator to be
// confirmed with ConfirmAdminTOTP. If the account does not exist it is
// created.
func (s *Service) ResetAdmin(ctx context.Context, username, newPW string) (TOTPSetup, error) {
	name, ok := NormalizeUsername(username)
	if !ok {
		return TOTPSetup{}, ErrUsernameInvalid
	}
	a, err := s.accounts.ByUsername(ctx, name)
	if errors.Is(err, ErrAccountNotFound) {
		return s.createOrRestartAdmin(ctx, name, newPW)
	}
	if err != nil {
		return TOTPSetup{}, err
	}
	if a.Role != RoleAdmin {
		return TOTPSetup{}, ErrNotAdmin
	}
	if err := CheckPasswordPolicy(newPW, true); err != nil {
		return TOTPSetup{}, err
	}
	h, err := s.hash(ctx, newPW)
	if err != nil {
		return TOTPSetup{}, err
	}
	unlock := s.lockAcct(a.ID)
	defer unlock()
	if a, err = s.accounts.ByID(ctx, a.ID); err != nil {
		return TOTPSetup{}, err
	}
	a.PasswordHash, a.Disabled = h, false
	a.TOTPSecret, a.TOTPLastStep, a.RecoveryHashes = "", 0, nil
	if err := s.bumpEpoch(ctx, &a, nil); err != nil {
		return TOTPSetup{}, err
	}
	s.userT.ForgetPrefix("user:" + name + "|")
	return s.pending(ctx, &a)
}

// ---------------------------------------------------------------- bootstrap token

// BootstrapTTL is how long a console setup token stays valid.
const BootstrapTTL = 24 * time.Hour

// MintBootstrapToken creates the one-time console token (DESIGN §7) used to
// set the first admin password and authenticator from a browser on a
// headless server. The caller prints it to the console / journal only.
// A new token replaces the previous one. It fails with ErrAdminExists once
// an admin is set up.
func (s *Service) MintBootstrapToken(ctx context.Context) (string, error) {
	ready, err := s.AdminReady(ctx)
	if err != nil {
		return "", err
	}
	if ready {
		return "", ErrAdminExists
	}
	var b [20]byte
	_, _ = rand.Read(b[:])
	tok := base64.RawURLEncoding.EncodeToString(b[:])
	s.bootMu.Lock()
	s.bootHash = sha256.Sum256([]byte(tok))
	s.bootExpires = s.now().Add(BootstrapTTL)
	s.bootMu.Unlock()
	return tok, nil
}

func (s *Service) bootOK(token string) bool {
	sum := sha256.Sum256([]byte(token))
	s.bootMu.Lock()
	defer s.bootMu.Unlock()
	if s.bootExpires.IsZero() || !s.now().Before(s.bootExpires) {
		return false
	}
	return subtle.ConstantTimeCompare(sum[:], s.bootHash[:]) == 1
}

// BootstrapPending reports whether a console token is outstanding.
func (s *Service) BootstrapPending() bool {
	s.bootMu.Lock()
	defer s.bootMu.Unlock()
	return !s.bootExpires.IsZero() && s.now().Before(s.bootExpires)
}

func (s *Service) bootCheck(token, client string) error {
	key := "boot:" + ClientKeyString(client)
	if d := s.ipT.Blocked(key); d > 0 {
		return &ThrottledError{RetryAfter: d}
	}
	if !s.bootOK(token) {
		s.ipT.Fail(key)
		return ErrBootstrapToken
	}
	return nil
}

// BootstrapBegin checks the console token and runs InitAdmin.
func (s *Service) BootstrapBegin(ctx context.Context, token, client, username, password string) (TOTPSetup, error) {
	if err := s.bootCheck(token, client); err != nil {
		return TOTPSetup{}, err
	}
	return s.InitAdmin(ctx, username, password)
}

// BootstrapFinish checks the console token, confirms the authenticator and
// consumes the token. It returns the recovery codes.
func (s *Service) BootstrapFinish(ctx context.Context, token, client, username, code string) ([]string, error) {
	if err := s.bootCheck(token, client); err != nil {
		return nil, err
	}
	codes, err := s.ConfirmAdminTOTP(ctx, username, code)
	if err != nil {
		return nil, err
	}
	s.bootMu.Lock()
	s.bootHash, s.bootExpires = [32]byte{}, time.Time{}
	s.bootMu.Unlock()
	return codes, nil
}

// ---------------------------------------------------------------- maintenance

// Purge removes expired sessions from the store.
func (s *Service) Purge(ctx context.Context) (int, error) {
	return s.sessions.Purge(ctx, s.now())
}

// RunJanitor purges expired sessions every interval until ctx ends.
func (s *Service) RunJanitor(ctx context.Context, every time.Duration) {
	if every <= 0 {
		every = 10 * time.Minute
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			_, _ = s.Purge(ctx)
		}
	}
}
