package ref

import "airview/internal/mlat/geo"

// RefPosition implements clock.RefSource (DESIGN §9.2.2): the reference
// position (ECEF) of aircraft key at tUnixNs (network time) and its
// covariance as the upper triangle xx, xy, xz, yy, yz, zz (m²). ok is false
// when there is no track or the state fails the sync gates (NACp, NIC,
// position age, probation, suspension, ...). df is not needed: every
// message of an aircraft with a usable reference track carries the same
// transmitter position; the caller limits the formats (clock.SyncDF).
func (s *Store) RefPosition(key uint32, df uint8, tUnixNs int64) (pos geo.Vec3, cov [6]float64, ok bool) {
	_ = df
	var st State
	if !s.At(key, tUnixNs, 0, &st) || !st.Usable() {
		return geo.Vec3{}, cov, false
	}
	c := &st.Cov
	cov = [6]float64{c[0], c[1], c[2], c[4], c[5], c[8]}
	return st.Pos, cov, true
}

// Usable implements clock.RefSource: whether aircraft key has a reference
// track that is usable for sync at its newest report (the grouper's cheap
// sync-candidate test).
func (s *Store) Usable(key uint32) bool {
	sh := s.shard(key)
	sh.mu.Lock()
	defer sh.mu.Unlock()
	tr := sh.m[key]
	if tr == nil || tr.n == 0 {
		return false
	}
	var st State
	return s.state(tr, tr.info.lastSeen, 0, &st) && st.Usable()
}
