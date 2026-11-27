package registry

import (
	"context"
	"fmt"
	"math/rand/v2"
	"path/filepath"
	"testing"
	"time"

	"airview/internal/model"
)

// TestScale5000 registers 5,000 stations (AVF and Beast), locates them,
// gives a share of them public pins, runs lookups, retention and a restart,
// and checks the time budgets that matter at the publication target
// (DESIGN §20.2: 1,000+ feeders; 5x headroom).
func TestScale5000(t *testing.T) {
	if testing.Short() {
		t.Skip("scale test")
	}
	const n = 5000
	ctx := context.Background()
	clk := newClock()
	path := filepath.Join(t.TempDir(), FileName)
	r := openAt(t, path, clk, Policy{Admission: model.AdmissionOpen, MaxStations: 10000})
	rng := rand.New(rand.NewPCG(1, 2))

	t0 := time.Now()
	idxs := make([]model.StationIdx, 0, n)
	for i := range n {
		var res AdmitResult
		var err error
		if i%5 == 4 {
			res, err = r.Admit(ctx, AdmitRequest{Kind: model.StationKindBeast, UUID: fmt.Sprintf("%032x", i+1)})
		} else {
			res, err = r.Admit(ctx, AdmitRequest{Kind: model.StationKindAVF, FeederPub: feederKey(i), RxNo: 0})
		}
		if err != nil || res.Status != StatusApproved {
			t.Fatalf("station %d: %+v %v", i, res, err)
		}
		idxs = append(idxs, res.Station.Idx)
	}
	admitDur := time.Since(t0)

	t0 = time.Now()
	for i, idx := range idxs {
		// Europe-sized area: many shared 1° cells, so the rural guard and
		// neighbour refreshes are exercised.
		lat, lon := 45+rng.Float64()*10, -5+rng.Float64()*20
		if _, err := r.SetEffective(ctx, idx, model.StationGeo{Lat: lat, Lon: lon, HAE: 100, Source: model.LocSrcManual}, true, model.LocDeclared); err != nil {
			t.Fatal(err)
		}
		if i%3 == 0 {
			pin := model.PinRegion
			if i%2 == 0 {
				pin = model.PinCell01
			}
			if _, err := r.SetPrivacy(ctx, idx, pin, false, true); err != nil {
				t.Fatal(err)
			}
		}
	}
	locDur := time.Since(t0)

	// Admission of known stations (every reconnect).
	t0 = time.Now()
	for i := range 1000 {
		if res, err := r.Admit(ctx, AdmitRequest{Kind: model.StationKindAVF, FeederPub: feederKey(i * 5 % n), RxNo: 0}); err != nil || res.New {
			t.Fatalf("reconnect %d: %+v %v", i, res, err)
		}
	}
	reconnect := time.Since(t0) / 1000

	// Resolver lookups (hot path).
	res := r.Resolver()
	t0 = time.Now()
	for range 100 {
		for _, idx := range idxs {
			if _, ok := res.Info(idx); !ok {
				t.Fatal("lookup failed")
			}
		}
	}
	lookup := time.Since(t0) / time.Duration(100*n)

	pins, checked := 0, false
	for _, idx := range idxs {
		if p, ok := r.PublicPoint(idx); ok {
			pins++
			if p.Effective == model.PinCell01 && !checked {
				checked = true
				st, _ := r.Station(idx)
				la, lo := cellIndex(st.Effective.Lat, st.Effective.Lon, 1)
				in := 0
				for _, o := range r.List(Filter{}) {
					if a, b := cellIndex(o.Effective.Lat, o.Effective.Lon, 1); o.Located && a == la && b == lo {
						in++
					}
				}
				if in < RuralGuardMin {
					t.Fatalf("cell01 shown in a 1° cell with %d stations", in)
				}
			}
		}
	}
	if pins < n/4 {
		t.Errorf("only %d public pins", pins)
	}

	for _, idx := range idxs {
		r.Seen(idx, clk.now())
	}
	t0 = time.Now()
	if err := r.FlushSeen(ctx); err != nil {
		t.Fatal(err)
	}
	flush := time.Since(t0)

	t0 = time.Now()
	rep, err := r.RunRetention(ctx)
	if err != nil {
		t.Fatal(err)
	}
	retDur := time.Since(t0)
	if rep.Purged != 0 {
		t.Errorf("retention purged %d active stations", rep.Purged)
	}

	// Snapshot the pins, restart, compare.
	before := map[model.StationIdx]PublicPoint{}
	for _, idx := range idxs {
		if p, ok := r.PublicPoint(idx); ok {
			before[idx] = p
		}
	}
	_ = r.Close()
	t0 = time.Now()
	r = openAt(t, path, clk, Policy{Admission: model.AdmissionOpen, MaxStations: 10000})
	defer r.Close()
	openDur := time.Since(t0)
	for idx, p := range before {
		if q, ok := r.PublicPoint(idx); !ok || q != p {
			t.Fatalf("pin of %d changed across restart", idx)
		}
	}
	if c := r.Count()[model.AdmApproved]; c != n {
		t.Errorf("count %d", c)
	}

	// Offline purge of 1,000 unclaimed stations, then the deletion job.
	clk.add(91 * 24 * time.Hour)
	for _, idx := range idxs[1000:] {
		r.Seen(idx, clk.now())
	}
	t0 = time.Now()
	rep, err = r.RunRetention(ctx)
	if err != nil {
		t.Fatal(err)
	}
	purgeDur := time.Since(t0)
	if rep.Purged != 1000 || rep.Deleted != 1000 || r.Count()[model.AdmApproved] != n-1000 {
		t.Errorf("purge: %+v count %v", rep, r.Count())
	}

	t.Logf("5000 stations: admit %v (%v/station), locate+pin %v, reconnect %v, Info %v, flush %v, retention %v, reopen %v, purge 1000 %v",
		admitDur, admitDur/n, locDur, reconnect, lookup, flush, retDur, openDur, purgeDur)
	// Budgets (generous for slow CI disks; typical values are 10x lower).
	if lookup > 2*time.Microsecond {
		t.Errorf("Info lookup %v, budget 2µs", lookup)
	}
	if openDur > 5*time.Second {
		t.Errorf("reopen %v, budget 5s", openDur)
	}
	if reconnect > 50*time.Millisecond {
		t.Errorf("reconnect admission %v, budget 50ms", reconnect)
	}
}

func BenchmarkResolverInfo(b *testing.B) {
	r, _, _ := openTest(b, model.AdmissionOpen)
	var idxs []model.StationIdx
	for i := range 2000 {
		idxs = append(idxs, mustApproved(b, admitAVF(b, r, i, 0, "")).Idx)
	}
	res := r.Resolver()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, ok := res.Info(idxs[i%len(idxs)]); !ok {
			b.Fatal("miss")
		}
	}
}

func BenchmarkAdmitReconnect(b *testing.B) {
	r, _, _ := openTest(b, model.AdmissionOpen)
	for i := range 1000 {
		mustApproved(b, admitAVF(b, r, i, 0, ""))
	}
	ctx := context.Background()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := r.Admit(ctx, AdmitRequest{Kind: model.StationKindAVF, FeederPub: feederKey(i % 1000)}); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkSeen(b *testing.B) {
	r, clk, _ := openTest(b, model.AdmissionOpen)
	st := mustApproved(b, admitAVF(b, r, 1, 0, ""))
	now := clk.now()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		r.Seen(st.Idx, now)
	}
}
