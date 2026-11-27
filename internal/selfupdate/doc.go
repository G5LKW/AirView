// Package selfupdate installs signed releases of airview and airview-feeder
// in place, with a health check after the restart and automatic rollback
// (DESIGN §12.1-§12.3, §14 WP-UP).
//
// # Trust
//
// A release is described by a manifest (manifest.json: product, version,
// channel, publication and optional expiry time, and per target the file
// URL, size and SHA-256), signed with an Ed25519 release key
// (manifest.json.sig: base64 of the 64-byte signature over the exact
// manifest bytes). Clients trust one or more public keys (base64 of the raw
// 32 bytes; several keys allow rotation). The signature is checked before
// the manifest is parsed; a manifest for another product or channel, one
// published more than a day in the future, an expired one, or one older
// than a manifest already accepted (a replayed old release) is refused. The
// artifact is downloaded with a size bound, hashed while streaming, compared
// with the manifest, and checked to be an executable for the target's OS
// before anything on disk changes. The layout of manifest.json is also a
// contract with the Raspberry Pi installer, which parses it with awk
// (docs/aggregator/HOOKS.md, WP-PI-installer -> WP-UP); Manifest.Marshal
// writes that layout and TestManifestInstallerContract keeps it.
//
// Keys are created and releases signed with the release tool in
// deploy/release (see its README.md). The private key never reaches a
// client or a server; ReleaseHandler only serves the files the tool wrote.
//
// # Install transaction
//
// Updater.Install copies the verified file next to the program
// (<exe>.new, same mode and owner), then swaps it in:
//
//   - Windows: rename-aside. A running image cannot be overwritten or
//     deleted but can be renamed: <exe> -> <exe>.old, <exe>.new -> <exe>.
//   - Linux and other POSIX: <exe> is hard-linked to <exe>.old and
//     <exe>.new renamed over <exe> (rename(2) is atomic; the program path
//     never disappears); the directory is fsynced.
//
// A journal (StateDir/selfupdate.json, written atomically) records the
// transaction (phase swapped / committed / rolledback, versions, SHA-256 of
// both binaries, paths). A lock file serialises transactions between
// processes (the service and a root-run "update" command). The program is
// then restarted through a Restarter, and the new version is observed:
//
//   - Supervised mode (Options.Probe set; the "update" command): the
//     updater restarts the service (systemctl restart <unit>, or the
//     Windows service control manager) and polls a Probe for the health
//     window (default 60 s): the program must answer, report the expected
//     version, and must not restart (PID / NRestarts / uptime change).
//     Failure restores <exe>.old (the rejected binary is kept as
//     <exe>.failed), restarts the previous version and returns an error
//     wrapping ErrRolledBack.
//   - Self-confirm mode (Probe nil; in-process automatic updates through
//     Attach): the updater restarts the program (SelfRestarter: execve on
//     POSIX, a new process on Windows) and returns OutcomePending. The new
//     process calls Recover at start-up; it counts start-ups and rolls back
//     after MaxTrialBoots (crash loop), and Trial.Confirm keeps the version
//     after the health window when the program's own check passes, or rolls
//     back otherwise. The supervisor (systemd Restart=, Windows service
//     recovery actions) then starts the restored binary.
//
// Every step is idempotent and safe to repeat after a crash or power loss:
// restoreOld does nothing when the program file already holds the previous
// version, the journal tells the next start what was in progress, and
// leftover files (.new, .del) are cleaned at the next transaction.
//
// # Configuration
//
// Settings is the "update" block of a program's configuration file
// (enabled, channel, manifestUrl, keys, checkIntervalMin, healthWindowS,
// healthUrl, maxDownloadMB), with Settings.ApplyEnv for AIRVIEW_UPDATE_* /
// AIRVIEW_FEEDER_UPDATE_* variables and Settings.Check for "config check".
// Self-update is opt-in for airview; airview-feeder maps its
// FeederConfig.AutoUpdate onto Enabled. The beta channel lives below
// <manifest dir>/beta/ (or wherever "{channel}" in the URL points).
//
// # Entry points
//
//   - Attach / Attachment.Run: in-process integration for long-running
//     programs (Recover at start, Trial.Confirm, the automatic Loop).
//   - RunCommand: the shared "update" subcommand (check, status, install
//     with service restart and supervision; exit codes ExitOK ...
//     ExitAvailable).
//   - ReleaseHandler: serves a release directory (an aggregator's
//     /feeder/manifest.json, /feeder/manifest.json.sig, /feeder/dl/<target>
//     and /feeder/beta/...).
//   - Publish, ScanArtifacts, GenerateKey, SignManifest, VerifyManifest:
//     the release side, used by deploy/release.
//
// See WIRING.md for how airview, airview-feeder, the server routes and the
// installers connect to this package.
package selfupdate
