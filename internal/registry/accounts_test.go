package registry

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"airview/internal/auth"
	"airview/internal/model"
)

func newService(r *Registry, clk *fakeClock) *auth.Service {
	return auth.NewService(r.Accounts(), r.Sessions(), auth.Options{Issuer: "Test", Now: clk.now, PasswordIterations: 1000})
}

func TestAuthServiceOnRegistry(t *testing.T) {
	ctx := context.Background()
	r, clk, path := openTest(t, model.AdmissionOpen)
	svc := newService(r, clk)

	a, err := svc.Register(ctx, "Alice", "alice-password", "c1")
	if err != nil || a.Username != "alice" {
		t.Fatalf("register: %+v %v", a, err)
	}
	if _, err := svc.Register(ctx, "ALICE", "alice-password", "c1"); !errors.Is(err, auth.ErrUsernameTaken) {
		t.Errorf("duplicate: %v", err)
	}
	res, err := svc.Login(ctx, auth.LoginRequest{Username: "alice", Password: "alice-password", Client: "c1"})
	if err != nil {
		t.Fatal(err)
	}
	other, _ := svc.Login(ctx, auth.LoginRequest{Username: "alice", Password: "alice-password", Client: "c2"})
	if _, _, err := svc.Authenticate(ctx, res.Token); err != nil {
		t.Fatal(err)
	}
	// The raw token is never stored.
	var n int
	_ = r.rdb.QueryRow(`SELECT COUNT(*) FROM sessions WHERE id=?`, res.Token).Scan(&n)
	if n != 0 {
		t.Fatal("raw token stored")
	}

	// Restart: the session survives.
	_ = r.Close()
	r2 := openAt(t, path, clk, Policy{})
	defer r2.Close()
	svc2 := newService(r2, clk)
	sess, acct, err := svc2.Authenticate(ctx, res.Token)
	if err != nil || acct.ID != a.ID || sess.Scope != model.ScopeOwner || sess.CSRF != res.Session.CSRF {
		t.Fatalf("after restart: %+v %v", sess, err)
	}
	list, _ := r2.Sessions().List(ctx, a.ID)
	if len(list) != 2 || list[0].Created.Before(list[1].Created) {
		t.Errorf("list %d", len(list))
	}

	// Password change ends the other session.
	if err := svc2.ChangePassword(ctx, &sess, "alice-password", "new-password-1"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := svc2.Authenticate(ctx, other.Token); err == nil {
		t.Error("other session survived the password change")
	}
	if _, _, err := svc2.Authenticate(ctx, res.Token); err != nil {
		t.Errorf("current session ended: %v", err)
	}

	// TOTP round trip (recovery hashes are a JSON column).
	setup, err := svc2.BeginTOTP(ctx, &sess, "new-password-1")
	if err != nil {
		t.Fatal(err)
	}
	g, _ := auth.ParseTOTPSecret(setup.Secret)
	codes, err := svc2.ConfirmTOTP(ctx, &sess, g.Code(clk.now()))
	if err != nil || len(codes) == 0 {
		t.Fatalf("confirm TOTP: %v", err)
	}
	got, _ := r2.Accounts().ByID(ctx, a.ID)
	if !got.TOTPEnabled() || len(got.RecoveryHashes) != len(codes) {
		t.Errorf("stored account %+v", got)
	}

	// Idle and absolute expiry, purge.
	clk.add(15 * 24 * time.Hour)
	if _, _, err := svc2.Authenticate(ctx, res.Token); err == nil {
		t.Error("idle session still valid")
	}
	if n, err := r2.Sessions().Purge(ctx, clk.now()); err != nil || n != 0 && n != 1 {
		t.Errorf("purge %d %v", n, err)
	}
	if c, _ := r2.Sessions().Count(ctx); c != 0 {
		t.Errorf("%d sessions after purge", c)
	}

	// Admin bootstrap works over the registry stores.
	if ready, _ := svc2.AdminReady(ctx); ready {
		t.Fatal("admin ready without admin")
	}
	as, err := svc2.InitAdmin(ctx, "root", "admin-password-123")
	if err != nil {
		t.Fatal(err)
	}
	ag, _ := auth.ParseTOTPSecret(as.Secret)
	if _, err := svc2.ConfirmAdminTOTP(ctx, "root", ag.Code(clk.now())); err != nil {
		t.Fatal(err)
	}
	if ready, _ := svc2.AdminReady(ctx); !ready {
		t.Error("admin not ready")
	}
	admins, _ := r2.Accounts().ListRole(ctx, auth.RoleAdmin)
	if len(admins) != 1 || admins[0].Username != "root" {
		t.Errorf("admins %+v", admins)
	}
}

func TestSessionCapPerAccount(t *testing.T) {
	ctx := context.Background()
	r, clk, _ := openTest(t, model.AdmissionOpen)
	r.opts.MaxSessionsPerAccount = 3
	s := r.Sessions()
	for i := range 5 {
		_, id := auth.NewSessionToken()
		if err := s.Put(ctx, auth.Session{ID: id, AccountID: 9, Scope: model.ScopeOwner, Username: "u", CSRF: "c",
			Created: clk.now(), LastSeen: clk.now(), Expires: clk.now().Add(time.Duration(i+1) * time.Hour)}); err != nil {
			t.Fatal(err)
		}
	}
	list, _ := s.List(ctx, 9)
	if len(list) != 3 {
		t.Fatalf("%d sessions, want 3", len(list))
	}
	for _, x := range list {
		if x.Expires.Before(clk.now().Add(3 * time.Hour)) {
			t.Errorf("kept an early-expiring session: %v", x.Expires)
		}
	}
	if err := s.Touch(ctx, list[0].ID, clk.now().Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	got, _ := s.Get(ctx, list[0].ID)
	if !got.LastSeen.Equal(clk.now().Add(time.Minute)) {
		t.Errorf("touch: %v", got.LastSeen)
	}
	if n, _ := s.DeleteAccount(ctx, 9, list[0].ID); n != 2 {
		t.Errorf("DeleteAccount removed %d", n)
	}
	if _, err := s.Get(ctx, "nope"); !errors.Is(err, auth.ErrSessionNotFound) {
		t.Errorf("missing session: %v", err)
	}
	if _, err := r.Accounts().ByUsername(ctx, "nobody"); !errors.Is(err, auth.ErrAccountNotFound) {
		t.Errorf("missing account: %v", err)
	}
	if err := r.Accounts().Update(ctx, auth.Account{ID: 77, Username: "x"}); !errors.Is(err, auth.ErrAccountNotFound) {
		t.Errorf("update missing: %v", err)
	}
}

func TestOwnerExportAndErasure(t *testing.T) {
	ctx := context.Background()
	r, clk, _ := openTest(t, model.AdmissionOpen)
	svc := newService(r, clk)
	a, _ := svc.Register(ctx, "alice", "alice-password", "c1")
	if _, err := svc.Login(ctx, auth.LoginRequest{Username: "alice", Password: "alice-password", Client: "c1"}); err != nil {
		t.Fatal(err)
	}
	st := mustApproved(t, admitAVF(t, r, 1, 0, ""))
	code, _, _ := r.MintClaim(ctx, st.Idx)
	if _, err := r.RedeemClaim(ctx, a.ID, code, ""); err != nil {
		t.Fatal(err)
	}
	locate(t, r, st.Idx, 51.123456, -1.654321)
	if _, err := r.AddEstimate(ctx, Estimate{Idx: st.Idx, Method: "survey", Lat: 51.1235, Lon: -1.6543, HAE: 90, SigmaE: 12, SigmaN: 15}); err != nil {
		t.Fatal(err)
	}
	other := mustApproved(t, admitAVF(t, r, 2, 0, ""))

	var buf bytes.Buffer
	if err := r.ExportOwner(ctx, a.ID, &buf); err != nil {
		t.Fatal(err)
	}
	zr, err := zip.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	if err != nil {
		t.Fatal(err)
	}
	files := map[string]string{}
	for _, f := range zr.File {
		rc, _ := f.Open()
		b, _ := io.ReadAll(rc)
		rc.Close()
		files[f.Name] = string(b)
	}
	for _, name := range []string{"README.json", "account.json", "sessions.json", "stations.json", "location_estimates.json", "connections.json", "audit.json"} {
		if _, ok := files[name]; !ok {
			t.Errorf("export lacks %s", name)
		}
	}
	all := strings.Join([]string{files["account.json"], files["sessions.json"], files["stations.json"]}, "")
	if strings.Contains(all, "pbkdf2") || strings.Contains(all, a.PasswordHash) {
		t.Error("export contains the password hash")
	}
	if !strings.Contains(files["stations.json"], st.SID) || !strings.Contains(files["stations.json"], "51.123456") {
		t.Error("export lacks the owner's own station with its exact location")
	}
	if strings.Contains(files["stations.json"], other.SID) {
		t.Error("export contains another station")
	}
	if !strings.Contains(files["audit.json"], "station.claim") {
		t.Error("export lacks the claim audit entry")
	}

	// Erasure: account, sessions and the station are gone; audit is pseudonymised.
	if err := r.DeleteOwner(ctx, SystemActor, a.ID, true); err != nil {
		t.Fatal(err)
	}
	if _, ok := r.BySID(st.SID); ok {
		t.Error("station still visible after erasure")
	}
	if _, err := r.ProcessDeletions(ctx); err != nil {
		t.Fatal(err)
	}
	if _, ok := r.Station(st.Idx); ok {
		t.Error("station row survived the deletion job")
	}
	if _, err := r.Accounts().ByID(ctx, a.ID); !errors.Is(err, auth.ErrAccountNotFound) {
		t.Error("account survived")
	}
	if c, _ := r.Sessions().Count(ctx); c != 0 {
		t.Errorf("%d sessions survived", c)
	}
	var n int
	_ = r.rdb.QueryRow(`SELECT COUNT(*) FROM audit WHERE sid=? OR actor_id=? OR instr(detail, ?)>0`, st.SID, a.ID, st.SID).Scan(&n)
	if n != 0 {
		t.Errorf("%d audit rows still name the erased owner or station", n)
	}
	_ = r.rdb.QueryRow(`SELECT COUNT(*) FROM location_estimates WHERE idx=?`, st.Idx).Scan(&n)
	if n != 0 {
		t.Error("location estimates survived")
	}

	// Deleting an account without the stations releases them.
	b, _ := svc.Register(ctx, "bob", "bob-password-1", "c2")
	if _, err := r.AssignOwner(ctx, SystemActor, other.Idx, b.ID); err != nil {
		t.Fatal(err)
	}
	if err := svc.DeleteAccount(ctx, b.ID); err != nil {
		t.Fatal(err)
	}
	if s, ok := r.Station(other.Idx); !ok || s.OwnerID != 0 {
		t.Errorf("station of a deleted account: %+v", s)
	}
}
