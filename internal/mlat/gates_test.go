package mlat

import (
	"context"
	"encoding/json"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"airview/internal/mlat/clock"
	"airview/internal/mlat/geo"
	"airview/internal/model"
	"airview/internal/sim/simnet"
)

// envDur returns a duration from an environment variable (minutes) or def.
func envDur(name string, def time.Duration) time.Duration {
	if v, err := strconv.ParseFloat(os.Getenv(name), 64); err == nil && v > 0 {
		return time.Duration(v * float64(time.Minute))
	}
	return def
}

func (h *harness) report(t *testing.T, name string) (pub, snap dist) {
	pub = summarize(h.pub, true)
	pubAll := summarize(h.pub, false)
	snap = summarize(h.snap, true)
	hold := summarize(h.hold, false)
	hsnap := summarize(h.holdSnap, false)
	t.Logf("%s: published (inside %0.f km) n=%d P50 %.1f m P95 %.1f m P99 %.1f m, within 1σ %.1f %% 2σ %.1f %%, gross %d",
		name, h.cfg.radiusKm, pub.n, pub.p50, pub.p95, pub.p99, 100*pub.cal1, 100*pub.cal2, pub.gross)
	t.Logf("%s: published (all) n=%d P50 %.1f P95 %.1f P99 %.1f, 1σ %.1f %%, gross %d", name, pubAll.n, pubAll.p50, pubAll.p95, pubAll.p99, 100*pubAll.cal1, pubAll.gross)
	t.Logf("%s: snapshot fixes (inside) n=%d P50 %.1f P95 %.1f P99 %.1f, 1σ %.1f %%, gross %d", name, snap.n, snap.p50, snap.p95, snap.p99, 100*snap.cal1, snap.gross)
	t.Logf("%s: hold-out vs truth: track n=%d P50 %.1f P95 %.1f; fixes n=%d P50 %.1f P95 %.1f", name, hold.n, hold.p50, hold.p95, hsnap.n, hsnap.p50, hsnap.p95)
	q := h.e.QualityReport(60, false)
	t.Logf("%s: hold-out vs ADS-B reference (engine's live numbers): track n=%d P50 %.1f P95 %.1f P99 %.1f cross P50 %.1f, 1σ %.1f %%; fixes n=%d P50 %.1f P95 %.1f",
		name, q.TrackTotal.N, q.TrackTotal.P50, q.TrackTotal.P95, q.TrackTotal.P99, q.TrackTotal.CrossP50, 100*q.TrackTotal.Within1Sigma,
		q.FixTotal.N, q.FixTotal.P50, q.FixTotal.P95)
	if testing.Verbose() {
		st := h.e.Stats()
		b, _ := json.Marshal(struct {
			Groups, Clusters, Solves, SolveOK, SolveErr, Rejected, Applied, Gated, Published, FixToTrack, ClusterToTrack, Dropped, SyncAdmitted uint64
			Rejects                                                                                                                             map[string]uint64
			Grades                                                                                                                              map[string]uint64
			ProcUs                                                                                                                              float64
		}{st.Groups, st.Clusters, st.Solves, st.SolveOK, st.SolveErr, st.Rejected, st.Applied, st.Gated, st.Published, st.FixToTrack, st.ClusterToTrack, st.Dropped, st.SyncAdmitted, st.Rejects, st.FixGrades, st.ProcUsPerGroup})
		t.Logf("%s: engine %s", name, b)
		b, _ = json.Marshal(st.Group)
		t.Logf("%s: group %s", name, b)
		b, _ = json.Marshal(st.Track)
		t.Logf("%s: track %s", name, b)
		cs := st.Clock
		t.Logf("%s: clock epochs %d admitted %d accepted %d rejected %d steps %d comps %d stations %d", name, cs.Epochs, cs.Admitted, cs.Accepted, cs.Rejected, cs.Steps, cs.Components, cs.Stations)
		synced, anchors := 0, 0
		for _, sc := range h.e.Filter().Snapshot().Stations() {
			if sc.Synced {
				synced++
			}
			if sc.Anchor {
				anchors++
			}
		}
		t.Logf("%s: synced %d anchors %d of %d", name, synced, anchors, h.n.NumReceivers())
	}
	return pub, snap
}

func (h *harness) footprint(minTx int, anchorsOnly bool) (n, fixed int) {
	for _, a := range h.ac {
		if !a.known || !a.modeSOnly {
			continue
		}
		c := a.heard4
		if anchorsOnly {
			c = a.heard4anch
		}
		if c < minTx {
			continue
		}
		n++
		if a.published > 0 {
			fixed++
		}
	}
	return n, fixed
}

func gateRun(t *testing.T, name string, hc harnessCfg, dur time.Duration) (*harness, dist) {
	start := time.Now()
	h := newHarness(t, hc)
	h.run(dur)
	pub, _ := h.report(t, name)
	hin := summarize(h.hold, true)
	t.Logf("%s: hold-out vs truth inside: n=%d P50 %.1f P95 %.1f 1σ %.1f %%", name, hin.n, hin.p50, hin.p95, 100*hin.cal1)
	t.Logf("%s: wall %v for %v simulated (%d frames)", name, time.Since(start).Round(time.Millisecond), dur, h.frames)
	return h, pub
}

// TestDiagDirectClock feeds the clock filter directly from simnet (truth
// positions at the transmit instant, no ingest) for the same receivers as
// the harness: it separates filter behaviour from pipeline effects.
func TestDiagDirectClock(t *testing.T) {
	if os.Getenv("MLAT_DIRECT") == "" {
		t.Skip("diagnostic: MLAT_DIRECT=1 MLAT_SEED=n")
	}
	seed, _ := strconv.ParseUint(os.Getenv("MLAT_SEED"), 10, 64)
	h := newHarness(t, harnessCfg{seed: max(seed, 13), devs: map[simnet.Device]int{simnet.DeviceSDR: 20},
		radiusKm: 150, modeSFrac: 0.3, evalFromS: 1e9})
	f := h.e.Filter()
	type grp struct {
		key  uint32
		kind model.FrameKind
		host int64
		pos  geo.Vec3
		txS  float64
		ms   []clock.Member
	}
	groups := map[uint64]*grp{}
	next := 0.5
	d := &directSink{obs: func(obs []simnet.Observation) {
		for i := range obs {
			o := &obs[i]
			if o.Kind == model.FrameModeAC || o.Tx.NACp < 8 || !clock.SyncDF(o.Data[0]>>3) {
				continue
			}
			g := groups[o.Seq]
			if g == nil {
				g = &grp{key: o.ICAO, kind: o.Kind, host: o.HostNs, pos: toGeo(o.Tx.Pos), txS: o.Tx.TxS}
				groups[o.Seq] = g
			}
			g.ms = append(g.ms, clock.Member{Station: model.StationIdx(o.Rx + 2), Epoch: o.Epoch, Ticks: o.Ticks})
			g.host = min(g.host, o.HostNs)
		}
	}, adv: func(hz float64) {
		for seq, g := range groups {
			if g.txS > hz-0.05 {
				continue
			}
			delete(groups, seq)
			if len(g.ms) >= 2 {
				f.AddSync(&clock.SyncObs{Key: g.key, Kind: g.kind, HostNs: g.host, Pos: g.pos, PosCov: [6]float64{400, 0, 0, 400, 0, 400}, Members: g.ms})
			}
		}
		for ; next <= hz-1; next += 0.5 {
			f.Step(next)
		}
	}}
	// detach the harness sink: rebuild a network without it
	n, _ := simnet.New(h.n.Config())
	n.AddSink(d)
	if err := n.Advance(context.Background(), envDur("MLAT_MIN", 8*time.Minute)); err != nil {
		t.Fatal(err)
	}
	cs := f.Stats()
	t.Logf("direct: accepted %d rejected %d steps %d", cs.Accepted, cs.Rejected, cs.Steps)
}

type directSink struct {
	obs func([]simnet.Observation)
	adv func(float64)
}

func (d *directSink) Observe(o []simnet.Observation) { d.obs(o) }
func (d *directSink) Advance(h float64)              { d.adv(h) }

// TestSeedSweepSDR runs the row-3 network (20 RTL-SDRs, hold-out 10 %) for
// several seeds and reports the clock steps per minute and the published
// accuracy: the SDR-only clock filter is seed-sensitive (WIRING.md).
// Opt-in: MLAT_SEEDS="13 14 15" [MLAT_MIN=16].
func TestSeedSweepSDR(t *testing.T) {
	seeds := strings.Fields(os.Getenv("MLAT_SEEDS"))
	if len(seeds) == 0 {
		t.Skip("set MLAT_SEEDS")
	}
	for _, sv := range seeds {
		seed, _ := strconv.ParseUint(sv, 10, 64)
		h := newHarness(t, harnessCfg{seed: seed, devs: map[simnet.Device]int{simnet.DeviceSDR: 20},
			radiusKm: 150, modeSFrac: 0.3, holdout: 0.1, evalFromS: envDur("MLAT_EVAL", 12*time.Minute).Seconds()})
		mins := int(envDur("MLAT_MIN", 16*time.Minute) / time.Minute)
		steps := make([]uint64, mins)
		var last uint64
		for m := 0; m < mins; m++ {
			h.run(time.Minute)
			s := h.e.Filter().Stats().Steps
			steps[m], last = s-last, s
		}
		pub := summarize(h.pub, true)
		t.Logf("seed %d: steps/min %v; published inside n=%d P50 %.1f P95 %.1f 1σ %.1f %%; blacklisted %d",
			seed, steps, pub.n, pub.p50, pub.p95, 100*pub.cal1, h.e.ctr.blacklisted.Load())
	}
}
