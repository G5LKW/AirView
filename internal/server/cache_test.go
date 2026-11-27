package server

import (
	"math"
	"testing"
	"time"

	"airview/internal/store"
)

type ttlValue struct {
	N   int           `json:"n"`
	TTL time.Duration `json:"-"`
}

func (v ttlValue) CacheTTL() time.Duration { return v.TTL }

// TestRespCacheResultTTL: a result that implements CacheTTL caps the route
// TTL, and one that returns <= 0 is not stored at all.
func TestRespCacheResultTTL(t *testing.T) {
	c := newRespCache()
	calls := 0
	get := func(ttl time.Duration) {
		t.Helper()
		if _, err := c.do("k", time.Minute, func() (any, error) {
			calls++
			return ttlValue{N: calls, TTL: ttl}, nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	get(0)
	get(0)
	if calls != 2 || c.size() != 0 {
		t.Fatalf("ttl 0: calls %d, size %d; want 2 calls, nothing stored", calls, c.size())
	}
	get(30 * time.Millisecond)
	get(30 * time.Millisecond)
	if calls != 3 {
		t.Fatalf("short ttl: calls %d, want 3 (second served from cache)", calls)
	}
	time.Sleep(50 * time.Millisecond)
	get(time.Hour) // capped at the route's minute
	if calls != 4 {
		t.Fatalf("after expiry: calls %d, want 4", calls)
	}
	c.mu.Lock()
	exp := time.Until(c.m["k"].expires)
	c.mu.Unlock()
	if exp > time.Minute {
		t.Errorf("expiry %v exceeds the route ttl", exp)
	}
}

// TestStatsTopNotCachedWhenEmpty: on a fresh database the top lists must
// show the first flights as soon as they exist, not after the route TTL.
func TestStatsTopNotCachedWhenEmpty(t *testing.T) {
	r := newRig(t, rigOptions{})
	type top struct {
		Flights int64 `json:"flights"`
	}
	const url = "/api/stats/top?hours=2"
	first := decode[top](t, r.get(url).Body.Bytes())
	if first.Flights != 0 {
		t.Skipf("demo already closed %d flights", first.Flights)
	}
	now := time.Now().Unix()
	rows := make([]store.Flight, 3)
	for i := range rows {
		rows[i] = store.Flight{
			ID: 1_000_000 + int64(i), ICAO: 0xabc000 + uint32(i), Callsign: "TST", Start: now - 600, End: now - 60, Msgs: 100, Positions: 10,
			MaxDistKm: 50, MinDistKm: 10, MaxAlt: 30000, MinAlt: 1000, MaxGS: 400, MaxDistBrg: math.NaN(),
			RSSIMin: -20, RSSIMax: -5, RSSIAvg: -10,
		}
	}
	r.eng.Store().UpsertFlights(rows)
	if err := r.eng.Store().Flush(); err != nil {
		t.Fatal(err)
	}
	second := decode[top](t, r.get(url).Body.Bytes())
	if second.Flights < 3 {
		t.Fatalf("top after inserting flights: %d flights, want >= 3 (empty answer was cached)", second.Flights)
	}
}
