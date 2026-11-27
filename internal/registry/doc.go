// Package registry keeps the station registry of aggregator mode in
// stations.db (docs/aggregator/DESIGN.md §3, §6.6, §7, §8).
//
// It owns these tables (all created by this package's own migrations, so
// they can live next to the namespaced statistics tables that
// internal/store keeps in the same file):
//
//	registry_meta      schema version
//	registry_keys      server secrets (32 random bytes each), created once
//	feeders            native AVF feeders: feeder ID, public key, state
//	stations           one row per receiver clock (identity, clock, location,
//	                   privacy, owner, admission, health)
//	station_public     the stored public pin point per station (§8.3)
//	location_estimates survey / Locate Me results, latest N per method
//	accounts           owner and admin accounts (auth.AccountStore)
//	sessions           login sessions by token hash (auth.SessionStore)
//	claims             single-use claim codes, stored as keyed hashes
//	enrol_tokens       admin-minted enrol tokens, stored hashed
//	conn_log           connection log with a daily-salted IP hash
//	conn_salt          today's random salt for conn_log
//	audit              admin / owner actions; pseudonymised on deletion
//	deletions          queued station deletions (survive restarts)
//
// # Secrets and privacy
//
//   - Raw IP addresses are never written. conn_log stores
//     HMAC(daySalt, ip) where daySalt is random per UTC day and is deleted
//     when the day is over, so hashes of different days cannot be linked
//     and a hash cannot be recomputed later.
//   - Claim codes and enrol tokens are bearer secrets: only keyed hashes are
//     stored and they are never logged.
//   - Session rows hold SHA-256(token) only (auth.SessionID).
//   - Public pin points are computed with a keyed PRF from a key kept in
//     registry_keys, so they are stable across restarts and cannot be
//     recomputed without the database.
//
// # Concurrency
//
// A Registry is safe for concurrent use. Writes go through one writer
// connection (BEGIN IMMEDIATE); reads use a small read-only pool. The
// station table is mirrored in memory: Resolver lookups (Info, Geo,
// PublicPoint) are lock-free atomic loads and never touch the database.
//
// # Timing trust (timing.go, H-mlatc)
//
// New AVF and mlat-client stations start in timing quarantine
// (FlagTimingQuarantine; Policy.NoTimingQuarantine turns it off): the gate
// in internal/registry/trust keeps their copies out of MLAT until their
// timestamps agree with the trusted network, then calls ProveTiming. The
// admin overrides with SetTiming (trusted, excluded, quarantine, auto).
// Stations created before the quarantine existed are trusted.
//
// # Feeder positions (position.go, finding PRIV-1)
//
// Positions a feeder announces itself (AVF RECEIVER, mlat-client
// handshake) go through FeederPosition: the first is applied, changes below
// JitterM are ignored, moderate changes are applied at most once per
// MinInterval, and moves of HoldBeyondM or more are held until the survey
// (SetEstimated) confirms them, the owner or admin confirms them
// (ConfirmPendingPosition), or they were announced for HoldMax. The frozen
// peer-band reference of the owner views is persisted per viewer station
// and tag period (FrozenBandRef), so a restart does not refreeze it.
//
// # Jobs
//
// Run starts the background work: last-seen flushes, the retention schedule
// of Aggregator.Retention (with the release of due pending positions and the
// pruning of old band references) and the station deletion queue. Each job
// can also be invoked directly (RunRetention, ReleasePending,
// ProcessDeletions, FlushSeen).
package registry
