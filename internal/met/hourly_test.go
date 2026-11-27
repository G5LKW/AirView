package met

import (
	"context"
	"encoding/json"
	"math/rand"
	"testing"
	"time"

	"airview/internal/store"
)

// TestProfileWrittenOncePerClosedHour: Run's periodic flush hands the
// thinned observations over every time but the met_profile increments of
// an hour only once the hour has closed; History answers the same before
// and after (pending increments come from memory), and the final Flush
// writes the open hour.
func TestProfileWrittenOncePerClosedHour(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	rng := rand.New(rand.NewSource(11))
	h0 := time.Now().Add(-3 * time.Hour).Truncate(time.Hour)
	c := New(st, Options{})
	acs := fleet(rng, 40, false)

	profileRows := func() (hours int, nw int64) {
		t.Helper()
		if err := st.Flush(); err != nil {
			t.Fatal(err)
		}
		if err := st.DB().QueryRow(`SELECT COUNT(DISTINCT hour), COALESCE(SUM(n_wind), 0) FROM met_profile`).Scan(&hours, &nw); err != nil {
			t.Fatal(err)
		}
		return
	}
	history := func() string {
		t.Helper()
		th, err := c.History(ctx, h0.Add(-time.Hour), h0.Add(3*time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		b, _ := json.Marshal(th)
		return string(b)
	}

	runFleet(c, acs, h0.Add(time.Minute), 50*time.Minute, 10*time.Second, rng)
	c.flush(false, h0.Add(51*time.Minute))
	if hours, _ := profileRows(); hours != 0 {
		t.Fatalf("open hour written: %d hours in met_profile", hours)
	}
	var samples int
	_ = st.DB().QueryRow(`SELECT COUNT(*) FROM met_sample`).Scan(&samples)
	if samples == 0 {
		t.Fatal("observations not handed over")
	}
	h1 := history()

	firstHourWind := c.Stats().Wind
	runFleet(c, acs, h0.Add(61*time.Minute), 10*time.Minute, 10*time.Second, rng)
	c.flush(false, h0.Add(72*time.Minute))
	hours, nw := profileRows()
	if hours != 1 || nw != firstHourWind {
		t.Fatalf("after the hour closed: %d hours, n_wind %d (want 1, %d)", hours, nw, firstHourWind)
	}
	h2 := history()
	if len(h2) <= len(h1) {
		t.Fatalf("history did not grow: %d -> %d bytes", len(h1), len(h2))
	}

	// An export writes the open hour now; the answer is unchanged and the
	// rest of the hour is merged into the same rows later.
	if err := c.FlushOpenHour(ctx); err != nil {
		t.Fatal(err)
	}
	if hours, nw := profileRows(); hours != 2 || nw != c.Stats().Wind {
		t.Fatalf("after FlushOpenHour: %d hours, n_wind %d (want 2, %d)", hours, nw, c.Stats().Wind)
	}
	if h := history(); h != h2 {
		t.Fatal("history changed when the open hour was written for an export")
	}
	runFleet(c, acs, h0.Add(73*time.Minute), 5*time.Minute, 10*time.Second, rng)
	h2 = history()

	// Shutdown: everything, the open hour too; the answer is unchanged.
	c.Flush()
	hours, nw = profileRows()
	if hours != 2 || nw != c.Stats().Wind {
		t.Fatalf("after Flush: %d hours, n_wind %d (want 2, %d)", hours, nw, c.Stats().Wind)
	}
	if h3 := history(); h3 != h2 {
		t.Fatal("history changed when the pending increments were written")
	}
}
