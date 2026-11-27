package registry

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"airview/internal/auth"
	"airview/internal/model"
)

func mustAddr(s string) netip.Addr { return netip.MustParseAddr(s) }

func mkOwner(t testing.TB, r *Registry, name string) int64 {
	t.Helper()
	a, err := r.Accounts().Create(context.Background(), auth.Account{Username: name, Role: auth.RoleOwner, PasswordHash: "x"})
	if err != nil {
		t.Fatal(err)
	}
	return a.ID
}

func TestClaimSingleUseAndTTL(t *testing.T) {
	r, clk, _ := openTest(t, model.AdmissionOpen)
	ctx := context.Background()
	st := mustApproved(t, admitAVF(t, r, 1, 0, ""))
	alice, bob := mkOwner(t, r, "alice"), mkOwner(t, r, "bob")

	code, exp, err := r.MintClaim(ctx, st.Idx)
	if err != nil {
		t.Fatal(err)
	}
	if len(code) != model.ClaimCodeLen || !exp.Equal(clk.now().Add(24*time.Hour)) {
		t.Fatalf("code %q expires %v", code, exp)
	}
	// Stored hashed: the plain code is nowhere in the table.
	var n int
	if err := r.rdb.QueryRow(`SELECT COUNT(*) FROM claims WHERE code_hash=? OR code_hash=?`, code, strings.ToLower(code)).Scan(&n); err != nil || n != 0 {
		t.Fatalf("plain claim code stored (%d, %v)", n, err)
	}
	// Typed with dashes, lower case and look-alikes still works.
	typed := strings.ToLower(code[:4] + "-" + code[4:])
	typed = strings.NewReplacer("o", "0", "i", "1", "b", "8").Replace(typed)
	got, err := r.RedeemClaim(ctx, alice, typed, "c1")
	if err != nil || got.OwnerID != alice {
		t.Fatalf("redeem: %+v %v", got, err)
	}
	if s, _ := r.Station(st.Idx); s.OwnerID != alice {
		t.Error("owner not set in the cache")
	}
	if _, err := r.RedeemClaim(ctx, bob, code, "c2"); !errors.Is(err, ErrClaimInvalid) {
		t.Errorf("second use: %v", err)
	}

	// TTL: valid until 24 h, invalid after.
	code2, _, _ := r.MintClaim(ctx, st.Idx)
	clk.add(24*time.Hour + time.Second)
	if _, err := r.RedeemClaim(ctx, bob, code2, "c3"); !errors.Is(err, ErrClaimInvalid) {
		t.Errorf("expired code: %v", err)
	}
	code3, _, _ := r.MintClaim(ctx, st.Idx)
	clk.add(24*time.Hour - time.Second)
	got, err = r.RedeemClaim(ctx, bob, code3, "c4")
	if err != nil || got.OwnerID != bob {
		t.Fatalf("code just before expiry / transfer: %+v %v", got, err)
	}
	logs, _ := r.AuditLog(ctx, AuditFilter{SID: st.SID, ActionPrefix: "station.claim"})
	if len(logs) != 2 || !strings.Contains(logs[0].Detail, "transferredFrom") {
		t.Errorf("claim audit %+v", logs)
	}

	// Unknown codes, malformed codes, unknown accounts.
	if _, err := r.RedeemClaim(ctx, bob, "AAAAAAAA", "c5"); !errors.Is(err, ErrClaimInvalid) {
		t.Errorf("unknown code: %v", err)
	}
	if _, err := r.RedeemClaim(ctx, bob, "short", "c5"); !errors.Is(err, ErrClaimInvalid) {
		t.Errorf("malformed: %v", err)
	}
	code4, _, _ := r.MintClaim(ctx, st.Idx)
	if _, err := r.RedeemClaim(ctx, 999, code4, "c6"); !errors.Is(err, ErrUnknownAccount) {
		t.Errorf("unknown account: %v", err)
	}
	if _, err := r.RedeemClaim(ctx, alice, code4, "c6"); err != nil {
		t.Errorf("code not consumed by the failed attempt should still work: %v", err)
	}
}

func TestClaimConcurrentSingleUse(t *testing.T) {
	r, _, _ := openTest(t, model.AdmissionOpen)
	ctx := context.Background()
	st := mustApproved(t, admitAVF(t, r, 1, 0, ""))
	code, _, _ := r.MintClaim(ctx, st.Idx)
	owners := make([]int64, 8)
	for i := range owners {
		owners[i] = mkOwner(t, r, fmt.Sprintf("user%d", i))
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	wins := 0
	for i := range owners {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if _, err := r.RedeemClaim(ctx, owners[i], code, fmt.Sprint("ip", i)); err == nil {
				mu.Lock()
				wins++
				mu.Unlock()
			}
		}(i)
	}
	wg.Wait()
	if wins != 1 {
		t.Fatalf("%d concurrent redemptions succeeded, want 1", wins)
	}
}

func TestClaimOpenCodesBounded(t *testing.T) {
	r, clk, _ := openTest(t, model.AdmissionOpen)
	ctx := context.Background()
	st := mustApproved(t, admitAVF(t, r, 1, 0, ""))
	var codes []string
	for range 7 {
		c, _, err := r.MintClaim(ctx, st.Idx)
		if err != nil {
			t.Fatal(err)
		}
		codes = append(codes, c)
		clk.add(time.Second)
	}
	var n int
	_ = r.rdb.QueryRow(`SELECT COUNT(*) FROM claims WHERE idx=?`, st.Idx).Scan(&n)
	if n != r.opts.MaxOpenClaims {
		t.Errorf("%d open codes, want %d", n, r.opts.MaxOpenClaims)
	}
	alice := mkOwner(t, r, "alice")
	if _, err := r.RedeemClaim(ctx, alice, codes[0], ""); !errors.Is(err, ErrClaimInvalid) {
		t.Errorf("oldest code survived: %v", err)
	}
	if _, err := r.RedeemClaim(ctx, alice, codes[6], ""); err != nil {
		t.Errorf("newest code: %v", err)
	}
	// Redeeming invalidates the other open codes of the station.
	_ = r.rdb.QueryRow(`SELECT COUNT(*) FROM claims WHERE idx=? AND used=0`, st.Idx).Scan(&n)
	if n != 0 {
		t.Errorf("%d open codes left after a claim", n)
	}
}

func TestClaimRateLimits(t *testing.T) {
	r, clk, _ := openTest(t, model.AdmissionOpen)
	ctx := context.Background()
	alice := mkOwner(t, r, "alice")
	for i := range 5 {
		if _, err := r.RedeemClaim(ctx, alice, "AAAAAAAA", "ip1"); !errors.Is(err, ErrClaimInvalid) {
			t.Fatalf("attempt %d: %v", i, err)
		}
	}
	_, err := r.RedeemClaim(ctx, alice, "AAAAAAAA", "ip1")
	var rl *RateLimitError
	if !errors.As(err, &rl) || !errors.Is(err, ErrRateLimited) || rl.RetryAfter <= 0 {
		t.Fatalf("6th attempt per IP: %v", err)
	}
	clk.add(10 * time.Minute)
	if _, err := r.RedeemClaim(ctx, alice, "AAAAAAAA", "ip1"); !errors.Is(err, ErrClaimInvalid) {
		t.Errorf("after the window: %v", err)
	}
	// 20 per day per account, from any number of addresses (6 used so far).
	for i := range 14 {
		if _, err := r.RedeemClaim(ctx, alice, "AAAAAAAA", fmt.Sprint("other", i)); !errors.Is(err, ErrClaimInvalid) {
			t.Fatalf("account attempt %d: %v", i, err)
		}
	}
	if _, err := r.RedeemClaim(ctx, alice, "AAAAAAAA", "fresh"); !errors.Is(err, ErrRateLimited) {
		t.Errorf("21st attempt per account: %v", err)
	}
	clk.add(24 * time.Hour)
	if _, err := r.RedeemClaim(ctx, alice, "AAAAAAAA", "fresh"); !errors.Is(err, ErrClaimInvalid) {
		t.Errorf("next day: %v", err)
	}
}

func TestClaimCompat(t *testing.T) {
	r, _, _ := openTest(t, model.AdmissionOpen)
	ctx := context.Background()
	uuid := "0123456789abcdef0123456789abcdef"
	res, err := r.Admit(ctx, AdmitRequest{Kind: model.StationKindBeast, UUID: "01234567-89AB-CDEF-0123-456789ABCDEF"})
	if err != nil {
		t.Fatal(err)
	}
	st := mustApproved(t, res)
	alice, bob := mkOwner(t, r, "alice"), mkOwner(t, r, "bob")
	if _, err := r.ClaimCompat(ctx, alice, uuid, false, "a"); !errors.Is(err, ErrIPMismatch) {
		t.Errorf("wrong IP: %v", err)
	}
	if _, err := r.ClaimCompat(ctx, alice, uuid[:30]+"00", true, "a"); !errors.Is(err, ErrNotClaimable) {
		t.Errorf("wrong UUID: %v", err)
	}
	got, err := r.ClaimCompat(ctx, alice, uuid, true, "a")
	if err != nil || got.OwnerID != alice || got.Idx != st.Idx {
		t.Fatalf("claim: %+v %v", got, err)
	}
	if _, err := r.ClaimCompat(ctx, bob, uuid, true, "b"); !errors.Is(err, ErrAlreadyOwned) {
		t.Errorf("owned by another: %v", err)
	}
	if _, _, err := r.MintClaim(ctx, st.Idx); !errors.Is(err, ErrNotClaimable) {
		t.Errorf("claim code for a compat station: %v", err)
	}
	// UUID-less stations need the admin.
	key := r.UnidentifiedKey(mustAddr("203.0.113.9"), 0)
	ures, _ := r.Admit(ctx, AdmitRequest{Kind: model.StationKindBeast, IdentKey: key})
	if !ures.Station.Has(FlagUnidentified) {
		t.Fatalf("unidentified flag: %+v", ures)
	}
	if _, err := r.Unclaim(ctx, bob, st.Idx); !errors.Is(err, ErrNotOwner) {
		t.Errorf("unclaim by non-owner: %v", err)
	}
	if got, err := r.Unclaim(ctx, alice, st.Idx); err != nil || got.OwnerID != 0 {
		t.Errorf("unclaim: %v", err)
	}
	if got, err := r.AssignOwner(ctx, SystemActor, ures.Station.Idx, bob); err != nil || got.OwnerID != bob {
		t.Errorf("assign: %v", err)
	}
	if _, err := r.AssignOwner(ctx, SystemActor, ures.Station.Idx, 12345); !errors.Is(err, ErrUnknownAccount) {
		t.Errorf("assign to unknown account: %v", err)
	}
}
