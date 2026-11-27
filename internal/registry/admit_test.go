package registry

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"airview/internal/model"
)

func TestAdmissionOpenApprove(t *testing.T) {
	r, _, _ := openTest(t, model.AdmissionApprove)
	ctx := context.Background()
	res := admitAVF(t, r, 1, 0, "")
	if res.Status != StatusPending || !res.New || !res.Allowed() || res.Station.Admission != model.AdmPending {
		t.Fatalf("approve mode: %+v", res)
	}
	// Reconnect: same station, still pending.
	res2 := admitAVF(t, r, 1, 0, "")
	if res2.New || res2.Station.Idx != res.Station.Idx || res2.Status != StatusPending {
		t.Fatalf("reconnect: %+v", res2)
	}
	if m, _ := r.Resolver().Info(res.Station.Idx); m.Admission != model.AdmPending {
		t.Error("pending station must resolve as pending (own lane only)")
	}
	if _, err := r.SetAdmission(ctx, Actor{ID: 1, Name: "admin"}, res.Station.Idx, model.AdmApproved); err != nil {
		t.Fatal(err)
	}
	if f, ok, _ := r.Feeder(ctx, res.Station.FeederID); !ok || f.State != FeederApproved {
		t.Errorf("feeder after approval: %+v", f)
	}
	mustApproved(t, admitAVF(t, r, 1, 0, ""))

	// Switching to open approves held stations on their next connect.
	p := admitAVF(t, r, 2, 0, "")
	if p.Status != StatusPending {
		t.Fatal(p)
	}
	r.SetPolicy(Policy{Admission: model.AdmissionOpen})
	mustApproved(t, admitAVF(t, r, 2, 0, ""))

	// Denied and revoked stations are refused.
	d := mustApproved(t, admitAVF(t, r, 3, 0, ""))
	if _, err := r.SetAdmission(ctx, SystemActor, d.Idx, model.AdmDenied); err != nil {
		t.Fatal(err)
	}
	if res := admitAVF(t, r, 3, 0, ""); res.Status != StatusDenied || res.Code != CodeDenied {
		t.Errorf("denied station: %+v", res)
	}
	if err := r.RevokeFeeder(ctx, SystemActor, d.FeederID); err != nil {
		t.Fatal(err)
	}
	if res := admitAVF(t, r, 3, 5, ""); res.Status != StatusDenied || res.Code != CodeRevoked {
		t.Errorf("revoked feeder, new rx: %+v", res)
	}
	logs, _ := r.AuditLog(ctx, AuditFilter{ActionPrefix: "station.approved"})
	if len(logs) != 1 || logs[0].Actor != "admin" {
		t.Errorf("approval audit %+v", logs)
	}
}

func TestAdmissionInviteTokens(t *testing.T) {
	r, clk, _ := openTest(t, model.AdmissionInvite)
	ctx := context.Background()
	admin := Actor{ID: 1, Name: "admin"}

	if res := admitAVF(t, r, 1, 0, ""); res.Status != StatusDenied || res.Code != CodeEnrolRequired {
		t.Fatalf("no token: %+v", res)
	}
	if res := admitAVF(t, r, 1, 0, "avf-nonsense"); res.Code != CodeEnrolInvalid {
		t.Fatalf("bad token: %+v", res)
	}
	secret, tok, err := r.MintToken(ctx, admin, "for Bob", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(secret, TokenPrefix) || tok.MaxUses != 1 || !tok.Expires.Equal(clk.now().Add(7*24*time.Hour)) {
		t.Fatalf("token defaults: %q %+v", secret, tok)
	}
	var n int
	_ = r.rdb.QueryRow(`SELECT COUNT(*) FROM enrol_tokens WHERE hash=? OR id=?`, secret, secret).Scan(&n)
	if n != 0 {
		t.Fatal("plain token stored")
	}
	if ok, _ := r.CheckToken(ctx, secret); !ok {
		t.Error("CheckToken")
	}
	st := mustApproved(t, admitAVF(t, r, 1, 0, "  "+strings.ToUpper(secret)+" "))
	if st.TokenID != tok.ID {
		t.Errorf("token id %q", st.TokenID)
	}
	// Used up.
	if res := admitAVF(t, r, 2, 0, secret); res.Code != CodeEnrolInvalid {
		t.Errorf("second use: %+v", res)
	}
	// The enrolled feeder adds a receiver and reconnects without a token.
	mustApproved(t, admitAVF(t, r, 1, 1, ""))
	mustApproved(t, admitAVF(t, r, 1, 0, ""))

	// Multi-use, expiry and revocation.
	multi, mt, _ := r.MintToken(ctx, admin, "club", 3, time.Hour)
	for i := 10; i < 13; i++ {
		mustApproved(t, admitAVF(t, r, i, 0, multi))
	}
	if res := admitAVF(t, r, 13, 0, multi); res.Code != CodeEnrolInvalid {
		t.Errorf("4th use of a 3-use token: %+v", res)
	}
	exp, _, _ := r.MintToken(ctx, admin, "", 5, time.Hour)
	clk.add(time.Hour)
	if res := admitAVF(t, r, 20, 0, exp); res.Code != CodeEnrolInvalid {
		t.Errorf("expired token: %+v", res)
	}
	rev, rt, _ := r.MintToken(ctx, admin, "", 5, 0)
	if err := r.RevokeToken(ctx, admin, rt.ID); err != nil {
		t.Fatal(err)
	}
	if res := admitAVF(t, r, 21, 0, rev); res.Code != CodeEnrolInvalid {
		t.Errorf("revoked token: %+v", res)
	}
	list, _ := r.Tokens(ctx)
	if len(list) != 4 {
		t.Fatalf("tokens: %d", len(list))
	}
	for _, x := range list {
		if x.ID == mt.ID && (x.Uses != 3 || x.Usable(clk.now())) {
			t.Errorf("multi token state %+v", x)
		}
	}

	// Compat with UUID: held pending (cannot present a token).
	res, err := r.Admit(ctx, AdmitRequest{Kind: model.StationKindMLATC, UUID: "0123456789abcdef0123456789abcdef", PrivacyOff: true})
	if err != nil || res.Status != StatusPending || res.Station.Pin != model.PinRegion {
		t.Errorf("compat under invite: %+v %v", res, err)
	}
	// Compat without UUID: refused.
	res, _ = r.Admit(ctx, AdmitRequest{Kind: model.StationKindBeast, IdentKey: r.UnidentifiedKey(mustAddr("192.0.2.1"), 0)})
	if res.Status != StatusDenied || res.Code != CodeEnrolRequired {
		t.Errorf("unidentified under invite: %+v", res)
	}
}

func TestAdmitCompatIdentity(t *testing.T) {
	r, _, _ := openTest(t, model.AdmissionOpen)
	ctx := context.Background()
	u := "0123456789abcdef0123456789abcd" // 30 digits: accepted
	b, err := r.Admit(ctx, AdmitRequest{Kind: model.StationKindBeast, UUID: u})
	if err != nil {
		t.Fatal(err)
	}
	m, _ := r.Admit(ctx, AdmitRequest{Kind: model.StationKindMLATC, UUID: strings.ToUpper(u), ClockCls: model.ClockHW12})
	if m.New || m.Station.Idx != b.Station.Idx {
		t.Fatalf("30004 and 31090 links with one UUID must be one station: %+v", m)
	}
	if m.Station.Kind != model.StationKindMLATC || !m.Station.Has(FlagBeast|FlagMLATC) || m.Station.ClockCls != model.ClockHW12 {
		t.Errorf("merged station %+v", m.Station)
	}
	sub, _ := r.Admit(ctx, AdmitRequest{Kind: model.StationKindBeast, UUID: u, RelaySub: "A1B2"})
	if !sub.New || sub.Station.UUID != u+"/a1b2" || !sub.Station.Has(FlagRelaySub) {
		t.Errorf("relay sub-station %+v", sub.Station)
	}
	if got, ok := r.ByUUID(u + "/a1b2"); !ok || got.Idx != sub.Station.Idx {
		t.Error("ByUUID of a sub-station")
	}
	if res, _ := r.Admit(ctx, AdmitRequest{Kind: model.StationKindBeast, UUID: "xyz"}); res.Code != CodeBadHello {
		t.Errorf("bad UUID: %+v", res)
	}
	if res, _ := r.Admit(ctx, AdmitRequest{Kind: model.StationKindBeast}); res.Code != CodeBadHello {
		t.Errorf("no identity: %+v", res)
	}
	if res, _ := r.Admit(ctx, AdmitRequest{Kind: model.StationKindAVF, FeederPub: []byte{1, 2}}); res.Code != CodeBadHello {
		t.Errorf("short key: %+v", res)
	}
	if _, err := r.Admit(ctx, AdmitRequest{Kind: model.StationKindHost}); !errors.Is(err, ErrInvalid) {
		t.Errorf("host via Admit: %v", err)
	}
	k := r.UnidentifiedKey(mustAddr("2001:db8::1"), 0)
	a1, _ := r.Admit(ctx, AdmitRequest{Kind: model.StationKindBeast, IdentKey: k})
	a2, _ := r.Admit(ctx, AdmitRequest{Kind: model.StationKindBeast, IdentKey: k})
	if !a1.New || a2.New || a1.Station.Idx != a2.Station.Idx || !strings.HasPrefix(k, "ip:") {
		t.Errorf("unidentified identity: %+v %+v", a1, a2)
	}
	if strings.Contains(k, "2001") {
		t.Error("identity key contains the IP")
	}
}

func TestAdmitStationLimit(t *testing.T) {
	r, _, _ := openTest(t, model.AdmissionOpen)
	r.SetPolicy(Policy{Admission: model.AdmissionOpen, MaxStations: 2})
	mustApproved(t, admitAVF(t, r, 1, 0, ""))
	mustApproved(t, admitAVF(t, r, 2, 0, ""))
	if res := admitAVF(t, r, 3, 0, ""); res.Code != CodeLimitStations {
		t.Errorf("over the limit: %+v", res)
	}
	// Existing stations still reconnect.
	mustApproved(t, admitAVF(t, r, 1, 0, ""))
}

func TestConnLog(t *testing.T) {
	r, clk, path := openTest(t, model.AdmissionOpen)
	ctx := context.Background()
	st := mustApproved(t, admitAVF(t, r, 1, 0, ""))
	ip := mustAddr("198.51.100.20")
	id1, err := r.ConnOpen(ctx, st.Idx, ip, "avf")
	if err != nil {
		t.Fatal(err)
	}
	clk.add(time.Minute)
	if err := r.ConnClose(ctx, id1, 1234); err != nil {
		t.Fatal(err)
	}
	id2, _ := r.ConnOpen(ctx, st.Idx, ip, "avf")
	clk.add(24 * time.Hour)
	id3, _ := r.ConnOpen(ctx, st.Idx, ip, "avf")
	list, _ := r.ConnLog(ctx, st.Idx, time.Time{}, 0)
	if len(list) != 3 {
		t.Fatalf("conn log %d", len(list))
	}
	byID := map[int64]ConnEntry{}
	for _, e := range list {
		byID[e.ID] = e
		if strings.Contains(e.IPHash, "198") || len(e.IPHash) != 16 {
			t.Errorf("ip hash %q", e.IPHash)
		}
	}
	if byID[id1].IPHash != byID[id2].IPHash {
		t.Error("same IP, same day: hashes differ")
	}
	if byID[id1].IPHash == byID[id3].IPHash {
		t.Error("same IP on another day: hashes linkable")
	}
	if byID[id1].Bytes != 1234 || byID[id1].Off.Sub(byID[id1].On) != time.Minute {
		t.Errorf("closed entry %+v", byID[id1])
	}
	var salts int
	_ = r.rdb.QueryRow(`SELECT COUNT(*) FROM conn_salt`).Scan(&salts)
	if salts != 1 {
		t.Errorf("%d salts kept, want only today's", salts)
	}
	// Dangling sessions are closed on the next Open.
	r.Seen(st.Idx, clk.now().Add(time.Minute))
	_ = r.Close()
	r2 := openAt(t, path, clk, Policy{})
	defer r2.Close()
	list, _ = r2.ConnLog(ctx, st.Idx, time.Time{}, 0)
	for _, e := range list {
		if e.Off.IsZero() {
			t.Errorf("dangling entry %d still open", e.ID)
		}
	}
}
