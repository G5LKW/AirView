package auth

import (
	"context"
	"errors"
	"testing"
	"time"

	"airview/internal/model"
)

func newTestService(clk *fakeClock) *Service {
	ipT := NewThrottle(5, 15*time.Minute, 5*time.Minute, 0)
	userT := NewThrottle(10, 15*time.Minute, 5*time.Minute, 0)
	ipT.now, userT.now = clk.now, clk.now
	return NewService(NewMemoryAccounts(), NewMemorySessions(0, 0), Options{
		Issuer: "Test Net", Now: clk.now, IPThrottle: ipT, UserThrottle: userT,
	})
}

func codeAt(t *testing.T, secret string, at time.Time) string {
	t.Helper()
	g, err := ParseTOTPSecret(secret)
	if err != nil {
		t.Fatal(err)
	}
	return g.Code(at)
}

func TestOwnerLoginAndSessions(t *testing.T) {
	ctx := context.Background()
	clk := newClock()
	s := newTestService(clk)

	if _, err := s.Register(ctx, "x", "long-enough-pw", "c1"); !errors.Is(err, ErrUsernameInvalid) {
		t.Errorf("bad name: %v", err)
	}
	if _, err := s.Register(ctx, "alice", "short", "c1"); !errors.Is(err, ErrPasswordTooShort) {
		t.Errorf("short pw: %v", err)
	}
	a, err := s.Register(ctx, "Alice", "alice-password", "c1")
	if err != nil || a.ID == 0 || a.Username != "alice" || a.Role != RoleOwner {
		t.Fatalf("register: %+v %v", a, err)
	}
	if _, err := s.Register(ctx, "ALICE", "alice-password", "c1"); !errors.Is(err, ErrUsernameTaken) {
		t.Errorf("duplicate: %v", err)
	}

	if _, err := s.Login(ctx, LoginRequest{Username: "alice", Password: "wrong-password", Client: "c1"}); !errors.Is(err, ErrBadCredentials) {
		t.Errorf("wrong password: %v", err)
	}
	if _, err := s.Login(ctx, LoginRequest{Username: "nobody", Password: "alice-password", Client: "c1"}); !errors.Is(err, ErrBadCredentials) {
		t.Errorf("unknown user: %v", err)
	}
	res, err := s.Login(ctx, LoginRequest{Username: " ALICE ", Password: "alice-password", Client: "c1"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Session.Scope != model.ScopeOwner || res.Session.CSRF == "" || res.Token == "" || res.Session.ID != SessionID(res.Token) {
		t.Fatalf("session: %+v", res.Session)
	}
	if v := res.Session.Viewer(); v.Scope != model.ScopeOwner || v.OwnerID != a.ID {
		t.Errorf("viewer %+v", v)
	}
	sess, acct, err := s.Authenticate(ctx, res.Token)
	if err != nil || acct.ID != a.ID || sess.ID != res.Session.ID {
		t.Fatalf("authenticate: %v", err)
	}
	if _, _, err := s.Authenticate(ctx, res.Session.ID); err == nil {
		t.Error("the stored hash works as a token")
	}

	// Idle timeout (owner: 14 days) and touch.
	clk.add(13 * 24 * time.Hour)
	if _, _, err := s.Authenticate(ctx, res.Token); err != nil {
		t.Fatalf("after 13 days: %v", err)
	}
	clk.add(13 * 24 * time.Hour)
	if _, _, err := s.Authenticate(ctx, res.Token); err != nil {
		t.Fatalf("touched session expired: %v", err)
	}
	clk.add(5 * 24 * time.Hour) // 31 days: absolute TTL
	if _, _, err := s.Authenticate(ctx, res.Token); !errors.Is(err, ErrSessionNotFound) {
		t.Errorf("absolute TTL: %v", err)
	}

	// Password change ends the other sessions, keeps the current one.
	r1, _ := s.Login(ctx, LoginRequest{Username: "alice", Password: "alice-password", Client: "c1"})
	r2, _ := s.Login(ctx, LoginRequest{Username: "alice", Password: "alice-password", Client: "c2"})
	if err := s.ChangePassword(ctx, &r1.Session, "wrong-password", "new-password-1"); !errors.Is(err, ErrBadCredentials) {
		t.Errorf("change with wrong old: %v", err)
	}
	if err := s.ChangePassword(ctx, &r1.Session, "alice-password", "new-password-1"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Authenticate(ctx, r1.Token); err != nil {
		t.Errorf("current session ended: %v", err)
	}
	if _, _, err := s.Authenticate(ctx, r2.Token); err == nil {
		t.Error("other session survived the password change")
	}
	if _, err := s.Login(ctx, LoginRequest{Username: "alice", Password: "alice-password", Client: "c1"}); err == nil {
		t.Error("old password still works")
	}

	// Disabling ends sessions and refuses logins.
	if err := s.SetDisabled(ctx, a.ID, true); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Authenticate(ctx, r1.Token); err == nil {
		t.Error("session of disabled account")
	}
	if _, err := s.Login(ctx, LoginRequest{Username: "alice", Password: "new-password-1", Client: "c1"}); !errors.Is(err, ErrAccountDisabled) {
		t.Errorf("disabled login: %v", err)
	}
	_ = s.SetDisabled(ctx, a.ID, false)
	r3, err := s.Login(ctx, LoginRequest{Username: "alice", Password: "new-password-1", Client: "c1"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Logout(ctx, r3.Token); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Authenticate(ctx, r3.Token); err == nil {
		t.Error("session after logout")
	}
	if err := s.DeleteAccount(ctx, a.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Accounts().ByID(ctx, a.ID); !errors.Is(err, ErrAccountNotFound) {
		t.Error("account not deleted")
	}
}

func TestOwnerTOTP(t *testing.T) {
	ctx := context.Background()
	clk := newClock()
	s := newTestService(clk)
	if _, err := s.Register(ctx, "bob", "bob-password", "c"); err != nil {
		t.Fatal(err)
	}
	res, _ := s.Login(ctx, LoginRequest{Username: "bob", Password: "bob-password", Client: "c"})
	other, _ := s.Login(ctx, LoginRequest{Username: "bob", Password: "bob-password", Client: "d"})
	cur := res.Session

	if _, err := s.ConfirmTOTP(ctx, &cur, "123456"); !errors.Is(err, ErrNoPendingTOTP) {
		t.Errorf("confirm without begin: %v", err)
	}
	if _, err := s.BeginTOTP(ctx, &cur, "wrong-password"); !errors.Is(err, ErrBadCredentials) {
		t.Errorf("begin with wrong password: %v", err)
	}
	setup, err := s.BeginTOTP(ctx, &cur, "bob-password")
	if err != nil || setup.Secret == "" || setup.URI == "" {
		t.Fatalf("begin: %v", err)
	}
	if _, err := s.ConfirmTOTP(ctx, &cur, "000000"); !errors.Is(err, ErrBadCode) && codeAt(t, setup.Secret, clk.now()) != "000000" {
		t.Errorf("wrong confirm code: %v", err)
	}
	codes, err := s.ConfirmTOTP(ctx, &cur, codeAt(t, setup.Secret, clk.now()))
	if err != nil || len(codes) != RecoveryCodeCount {
		t.Fatalf("confirm: %v", err)
	}
	if _, _, err := s.Authenticate(ctx, res.Token); err != nil {
		t.Errorf("enrolling session ended: %v", err)
	}
	if _, _, err := s.Authenticate(ctx, other.Token); err == nil {
		t.Error("other session survived TOTP enrolment")
	}

	// Login now needs the code.
	if _, err := s.Login(ctx, LoginRequest{Username: "bob", Password: "bob-password", Client: "c"}); !errors.Is(err, ErrCodeRequired) {
		t.Fatalf("no code: %v", err)
	}
	// The confirmation code cannot be replayed.
	if _, err := s.Login(ctx, LoginRequest{Username: "bob", Password: "bob-password", Code: codeAt(t, setup.Secret, clk.now()), Client: "c"}); !errors.Is(err, ErrBadCode) {
		t.Errorf("replayed confirmation code: %v", err)
	}
	clk.add(30 * time.Second)
	code := codeAt(t, setup.Secret, clk.now())
	r, err := s.Login(ctx, LoginRequest{Username: "bob", Password: "bob-password", Code: code, Client: "c"})
	if err != nil || !r.Session.MFA {
		t.Fatalf("login with code: %v", err)
	}
	if _, err := s.Login(ctx, LoginRequest{Username: "bob", Password: "bob-password", Code: code, Client: "c"}); !errors.Is(err, ErrBadCode) {
		t.Errorf("code reused: %v", err)
	}
	// Recovery code: once.
	r, err = s.Login(ctx, LoginRequest{Username: "bob", Password: "bob-password", Code: codes[0], Client: "c"})
	if err != nil || !r.UsedRecovery || r.RecoveryLeft != RecoveryCodeCount-1 {
		t.Fatalf("recovery login: %v %+v", err, r)
	}
	if _, err := s.Login(ctx, LoginRequest{Username: "bob", Password: "bob-password", Code: codes[0], Client: "c"}); !errors.Is(err, ErrBadCode) {
		t.Errorf("recovery code reused: %v", err)
	}
	// Wrong password with a valid code is still a wrong password.
	clk.add(30 * time.Second)
	if _, err := s.Login(ctx, LoginRequest{Username: "bob", Password: "nope-nope", Code: codeAt(t, setup.Secret, clk.now()), Client: "c"}); !errors.Is(err, ErrBadCredentials) {
		t.Errorf("wrong password + code: %v", err)
	}
	// Regenerate and disable.
	cur = r.Session
	clk.add(30 * time.Second)
	newCodes, err := s.RegenerateRecovery(ctx, &cur, "bob-password", codeAt(t, setup.Secret, clk.now()))
	if err != nil || len(newCodes) != RecoveryCodeCount {
		t.Fatalf("regenerate: %v", err)
	}
	if _, err := s.Login(ctx, LoginRequest{Username: "bob", Password: "bob-password", Code: codes[1], Client: "c"}); !errors.Is(err, ErrBadCode) {
		t.Error("old recovery code survived regeneration")
	}
	if err := s.DisableTOTP(ctx, &cur, "bob-password", newCodes[0]); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if _, err := s.Login(ctx, LoginRequest{Username: "bob", Password: "bob-password", Client: "c"}); err != nil {
		t.Errorf("login after disabling TOTP: %v", err)
	}
}

func TestAdminRequiresTOTP(t *testing.T) {
	ctx := context.Background()
	clk := newClock()
	s := newTestService(clk)
	if ok, _ := s.AdminReady(ctx); ok {
		t.Fatal("ready without admin")
	}
	if _, err := s.InitAdmin(ctx, "root", "short-pw"); !errors.Is(err, ErrPasswordTooShort) {
		t.Errorf("admin min length: %v", err)
	}
	setup, err := s.InitAdmin(ctx, "root", "admin-password-1")
	if err != nil {
		t.Fatal(err)
	}
	// Password alone is never enough, even before TOTP is confirmed.
	if _, err := s.Login(ctx, LoginRequest{Username: "root", Password: "admin-password-1", Client: "c"}); !errors.Is(err, ErrAdminNoTOTP) {
		t.Errorf("admin without TOTP: %v", err)
	}
	if ok, _ := s.AdminReady(ctx); ok {
		t.Error("ready before confirmation")
	}
	// Restarting the unfinished setup is allowed and replaces the secret.
	setup2, err := s.InitAdmin(ctx, "root", "admin-password-2")
	if err != nil || setup2.Secret == setup.Secret {
		t.Fatalf("restart setup: %v", err)
	}
	codes, err := s.ConfirmAdminTOTP(ctx, "root", codeAt(t, setup2.Secret, clk.now()))
	if err != nil || len(codes) != RecoveryCodeCount {
		t.Fatalf("confirm: %v", err)
	}
	if ok, _ := s.AdminReady(ctx); !ok {
		t.Fatal("not ready after confirmation")
	}
	if _, err := s.InitAdmin(ctx, "other", "admin-password-3"); !errors.Is(err, ErrAdminExists) {
		t.Errorf("second init: %v", err)
	}
	if _, err := s.Login(ctx, LoginRequest{Username: "root", Password: "admin-password-2", Client: "c"}); !errors.Is(err, ErrCodeRequired) {
		t.Errorf("admin without code: %v", err)
	}
	clk.add(30 * time.Second)
	r, err := s.Login(ctx, LoginRequest{Username: "root", Password: "admin-password-2", Code: codeAt(t, setup2.Secret, clk.now()), Client: "c"})
	if err != nil || r.Session.Scope != model.ScopeAdmin || !r.Session.MFA {
		t.Fatalf("admin login: %v %+v", err, r.Session)
	}
	cur := r.Session
	if err := s.DisableTOTP(ctx, &cur, "admin-password-2", codes[0]); !errors.Is(err, ErrTOTPMandatory) {
		t.Errorf("admin disable TOTP: %v", err)
	}
	// Admin idle timeout is short.
	clk.add(3 * time.Hour)
	if _, _, err := s.Authenticate(ctx, r.Token); err == nil {
		t.Error("admin session survived 3 idle hours")
	}
	// Break-glass reset: new password, new authenticator, sessions gone.
	setup3, err := s.ResetAdmin(ctx, "root", "brand-new-admin-pw")
	if err != nil {
		t.Fatal(err)
	}
	if ok, _ := s.AdminReady(ctx); ok {
		t.Error("ready after reset before confirmation")
	}
	if _, err := s.ConfirmAdminTOTP(ctx, "root", codeAt(t, setup3.Secret, clk.now())); err != nil {
		t.Fatal(err)
	}
	if ok, _ := s.AdminReady(ctx); !ok {
		t.Error("not ready after reset confirmation")
	}
	// Owners cannot be confirmed as admins.
	_, _ = s.Register(ctx, "owner1", "owner-password", "c")
	if _, err := s.ConfirmAdminTOTP(ctx, "owner1", "123456"); !errors.Is(err, ErrNotAdmin) {
		t.Errorf("owner as admin: %v", err)
	}
	if _, err := s.ResetAdmin(ctx, "owner1", "brand-new-admin-pw"); !errors.Is(err, ErrNotAdmin) {
		t.Errorf("reset owner: %v", err)
	}
}

func TestBootstrapToken(t *testing.T) {
	ctx := context.Background()
	clk := newClock()
	s := newTestService(clk)
	tok, err := s.MintBootstrapToken(ctx)
	if err != nil || tok == "" || !s.BootstrapPending() {
		t.Fatal(err)
	}
	if _, err := s.BootstrapBegin(ctx, "wrong", "c", "root", "admin-password-1"); !errors.Is(err, ErrBootstrapToken) {
		t.Errorf("wrong token: %v", err)
	}
	setup, err := s.BootstrapBegin(ctx, tok, "c", "root", "admin-password-1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.BootstrapFinish(ctx, tok, "c", "root", codeAt(t, setup.Secret, clk.now())); err != nil {
		t.Fatal(err)
	}
	if s.BootstrapPending() {
		t.Error("token not consumed")
	}
	if _, err := s.BootstrapBegin(ctx, tok, "c", "root2", "admin-password-1"); !errors.Is(err, ErrBootstrapToken) {
		t.Errorf("token reused: %v", err)
	}
	if _, err := s.MintBootstrapToken(ctx); !errors.Is(err, ErrAdminExists) {
		t.Errorf("mint with admin: %v", err)
	}
	// Expiry.
	s2 := newTestService(clk)
	tok2, _ := s2.MintBootstrapToken(ctx)
	clk.add(BootstrapTTL + time.Second)
	if _, err := s2.BootstrapBegin(ctx, tok2, "c", "root", "admin-password-1"); !errors.Is(err, ErrBootstrapToken) {
		t.Errorf("expired token: %v", err)
	}
	// Guessing is throttled.
	s3 := newTestService(clk)
	_, _ = s3.MintBootstrapToken(ctx)
	var last error
	for range 8 {
		_, last = s3.BootstrapBegin(ctx, "guess", "evil", "root", "admin-password-1")
	}
	var te *ThrottledError
	if !errors.As(last, &te) {
		t.Errorf("bootstrap guessing not throttled: %v", last)
	}
}

func TestLoginThrottle(t *testing.T) {
	ctx := context.Background()
	clk := newClock()
	s := newTestService(clk)
	_, _ = s.Register(ctx, "carol", "carol-password", "c")
	var err error
	for range 5 {
		_, err = s.Login(ctx, LoginRequest{Username: "carol", Password: "guess-guess", Client: "evil"})
	}
	var te *ThrottledError
	if !errors.As(err, &te) || !errors.Is(err, ErrBadCredentials) {
		t.Fatalf("fifth failure: %v", err)
	}
	// Even the right password is refused while throttled.
	if _, err := s.Login(ctx, LoginRequest{Username: "carol", Password: "carol-password", Client: "evil"}); !errors.As(err, &te) {
		t.Errorf("correct password while throttled: %v", err)
	}
	// Another client is not affected by the per-IP throttle...
	if _, err := s.Login(ctx, LoginRequest{Username: "carol", Password: "carol-password", Client: "good"}); err != nil {
		t.Errorf("other client: %v", err)
	}
	// SEC-02: failures from many other addresses do not lock the real owner
	// out (the user throttle is keyed per user name and client)...
	for i := range 12 {
		_, err = s.Login(ctx, LoginRequest{Username: "carol", Password: "guess-guess", Client: "bot" + string(rune('a'+i))})
	}
	if errors.As(err, &te) {
		t.Fatalf("one failure per address throttled: %v", err)
	}
	if _, err := s.Login(ctx, LoginRequest{Username: "carol", Password: "carol-password", Client: "fresh"}); err != nil {
		t.Errorf("owner locked out by others' failures: %v", err)
	}
	// ...while one client guessing is throttled per user name too.
	for range 12 {
		clk.add(5*time.Minute + time.Second) // past the per-IP delay, inside the user window
		_, err = s.Login(ctx, LoginRequest{Username: "carol", Password: "guess-guess", Client: "2001:db8:5::1"})
	}
	if !errors.As(err, &te) {
		t.Fatalf("repeated guessing from one client: %v", err)
	}
	// An IPv6 /64 is one client.
	if _, err := s.Login(ctx, LoginRequest{Username: "carol", Password: "carol-password", Client: "2001:db8:5::ffff"}); !errors.As(err, &te) {
		t.Errorf("another address of the same /64 is not throttled: %v", err)
	}
	clk.add(16 * time.Minute)
	// Registration limit per client.
	for i := range 6 {
		_, err = s.Register(ctx, "user"+string(rune('a'+i))+"xx", "some-password", "spammer")
	}
	if !errors.As(err, &te) {
		t.Errorf("registration flood: %v", err)
	}
}

func TestRehashOnLogin(t *testing.T) {
	ctx := context.Background()
	clk := newClock()
	s := newTestService(clk)
	a, _ := s.Register(ctx, "dave", "dave-password", "c")
	a.PasswordHash, _ = HashPasswordCost("dave-password", 1000)
	_ = s.Accounts().Update(ctx, a)
	if _, err := s.Login(ctx, LoginRequest{Username: "dave", Password: "dave-password", Client: "c"}); err != nil {
		t.Fatal(err)
	}
	b, _ := s.Accounts().ByID(ctx, a.ID)
	if NeedsRehash(b.PasswordHash) || !VerifyPassword(b.PasswordHash, "dave-password") {
		t.Error("hash not upgraded")
	}
}

func BenchmarkAuthenticate(b *testing.B) {
	ctx := context.Background()
	clk := newClock()
	s := newTestService(clk)
	_, _ = s.Register(ctx, "bench", "bench-password", "c")
	r, _ := s.Login(ctx, LoginRequest{Username: "bench", Password: "bench-password", Client: "c"})
	b.ReportAllocs()
	for b.Loop() {
		if _, _, err := s.Authenticate(ctx, r.Token); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkTOTPVerify(b *testing.B) {
	g := NewTOTPSecret()
	now := time.Now()
	code := g.Code(now)
	for b.Loop() {
		g.Verify(code, now, 0)
	}
}

func TestRegistrationClosed(t *testing.T) {
	ctx := context.Background()
	s := NewService(NewMemoryAccounts(), NewMemorySessions(0, 0), Options{RegistrationClosed: true})
	if _, err := s.Register(ctx, "erin", "erin-password", "c"); !errors.Is(err, ErrRegistrationOff) {
		t.Errorf("closed sign-up: %v", err)
	}
	a, err := s.CreateOwner(ctx, "erin", "erin-password")
	if err != nil || a.Role != RoleOwner {
		t.Fatalf("admin-created owner: %v", err)
	}
}

// SEC-06: failures for unknown user names are counted apart from real
// accounts, and attempts per client are bounded before any hashing.
func TestUnknownUserThrottleSeparate(t *testing.T) {
	ctx := context.Background()
	clk := newClock()
	s := newTestService(clk)
	_, _ = s.Register(ctx, "erin", "erin-password", "c")
	before := s.userT.Len()
	for i := range 3 {
		_, _ = s.Login(ctx, LoginRequest{Username: "nobody" + string(rune('a'+i)), Password: "x-x-x-x-x", Client: "203.0.113.1"})
	}
	if s.userT.Len() != before || s.unknownT.Len() != 3 {
		t.Fatalf("unknown names in the account throttle: user %d unknown %d", s.userT.Len(), s.unknownT.Len())
	}
	var te *ThrottledError
	var err error
	for range 40 {
		clk.add(10 * time.Minute) // clears the failure throttles
		_, err = s.Login(ctx, LoginRequest{Username: "erin", Password: "erin-password", Client: "203.0.113.9"})
		if err != nil {
			break
		}
	}
	if err != nil {
		t.Fatalf("successful logins spaced out: %v", err)
	}
	for range 31 {
		_, err = s.Login(ctx, LoginRequest{Username: "erin", Password: "erin-password", Client: "203.0.113.10"})
	}
	if !errors.As(err, &te) {
		t.Fatalf("attempt flood from one client not limited: %v", err)
	}
}
