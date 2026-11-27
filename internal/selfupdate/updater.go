package selfupdate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Options configures an Updater.
type Options struct {
	// Product is the manifest product this program installs from
	// ("airview", "airview-feeder"). Required.
	Product string
	// CurrentVersion is the running program's version. Required.
	CurrentVersion string
	// Target defaults to CurrentTarget().
	Target string
	// Settings: channel, manifest URL, keys, windows, limits.
	Settings Settings
	// Keys, when non-empty, are trusted in addition to Settings.Keys (a
	// key compiled into the program, for example).
	Keys []PublicKey
	// Exe is the program file to replace (default ExecutablePath()).
	Exe string
	// StateDir holds the journal, the lock and downloads. Required; it
	// must be writable by whoever runs the update (the data directory).
	StateDir string
	// HTTPClient for manifest and artifact downloads (default: 30 s
	// connect/headers, 15 min overall).
	HTTPClient *http.Client
	// Restarter restarts the program after a swap and after a rollback.
	// Required for Install.
	Restarter Restarter
	// Probe supervises the restarted program for the health window.
	// nil = self-confirm mode: the new process confirms itself through
	// Recover / Trial.Confirm (used with SelfRestarter).
	Probe Probe
	// Health overrides the health policy (Window defaults to
	// Settings.HealthWindowS).
	Health HealthPolicy
	// MaxTrialBoots: start-ups of the new binary during its trial before
	// Recover rolls back (crash loop). Default 3.
	MaxTrialBoots int
	// Logf receives progress messages (never secrets). Default: discard.
	Logf func(format string, args ...any)
	// Now is the clock (tests). Default time.Now.
	Now func() time.Time
	// Getenv is os.Getenv by default (tests).
	Getenv func(string) string
}

// Updater checks for, downloads, verifies and installs signed releases of
// one program, with health check and automatic rollback. Methods are safe
// for concurrent use; transactions are serialised by a lock file in
// StateDir, so separate processes (a service and an "update" command) do
// not interfere.
type Updater struct {
	o        Options
	keys     []PublicKey
	strategy swapStrategy
	ua       string
}

// Release is the outcome of a successful Check.
type Release struct {
	Manifest    *Manifest
	ManifestURL string
	File        File
	FileURL     string
	// Newer: the release version is greater than the current version.
	Newer bool
}

// Outcomes of an Update / Install.
const (
	OutcomeUpToDate   = "up-to-date"
	OutcomeAvailable  = "available"   // CheckOnly: a newer version exists
	OutcomeUpdated    = "updated"     // installed and healthy
	OutcomePending    = "pending"     // installed, the new process confirms itself
	OutcomeRolledBack = "rolled-back" // new version unhealthy, previous restored
)

// Result describes what an Update / Install did.
type Result struct {
	Outcome string `json:"outcome"`
	From    string `json:"from"`
	To      string `json:"to,omitempty"`
	Detail  string `json:"detail,omitempty"`
}

// ErrRolledBack is returned (wrapped) when the new version failed its
// health check and the previous binary was restored.
var ErrRolledBack = errors.New("update rolled back")

// ErrNotConfigured is returned when no manifest URL or key is configured.
var ErrNotConfigured = errors.New("self-update is not configured (update.manifestUrl and update.keys)")

// New checks the options and returns an Updater.
func New(o Options) (*Updater, error) {
	if !ValidProduct(o.Product) {
		return nil, fmt.Errorf("selfupdate: invalid product %q", o.Product)
	}
	if o.CurrentVersion == "" {
		return nil, errors.New("selfupdate: CurrentVersion is required")
	}
	if o.StateDir == "" {
		return nil, errors.New("selfupdate: StateDir is required")
	}
	o.Settings = o.Settings.Normalized()
	if o.Target == "" {
		o.Target = CurrentTarget()
	}
	if !ValidTarget(o.Target) {
		return nil, fmt.Errorf("selfupdate: invalid target %q", o.Target)
	}
	keys, err := ParsePublicKeys(o.Settings.Keys)
	if err != nil {
		return nil, fmt.Errorf("selfupdate: %w", err)
	}
	keys = append(keys, o.Keys...)
	if o.Exe == "" {
		if o.Exe, err = ExecutablePath(); err != nil {
			return nil, fmt.Errorf("selfupdate: %w", err)
		}
	}
	if o.HTTPClient == nil {
		o.HTTPClient = &http.Client{Timeout: 15 * time.Minute}
	}
	if o.MaxTrialBoots <= 0 {
		o.MaxTrialBoots = 3
	}
	if o.Logf == nil {
		o.Logf = func(string, ...any) {}
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Getenv == nil {
		o.Getenv = os.Getenv
	}
	if o.Health.Window <= 0 {
		o.Health.Window = o.Settings.HealthWindow()
	}
	return &Updater{o: o, keys: keys, strategy: defaultStrategy(),
		ua: "airview-selfupdate/" + o.CurrentVersion + " (" + o.Product + "; " + o.Target + ")"}, nil
}

// Settings returns the normalised settings.
func (u *Updater) Settings() Settings { return u.o.Settings }

// Configured reports whether a manifest URL and at least one key exist.
func (u *Updater) Configured() bool { return u.o.Settings.ManifestURL != "" && len(u.keys) > 0 }

// Check downloads and verifies the manifest of the configured channel and
// returns the artifact for this target. It never changes the program.
func (u *Updater) Check(ctx context.Context) (*Release, error) {
	rel, err := u.check(ctx)
	st := u.loadStatus()
	st.LastCheck = u.o.Now().UTC()
	st.LastError = ""
	st.Available = ""
	if err != nil {
		st.LastError = err.Error()
	} else if rel.Newer {
		st.Available = rel.Manifest.Version
	}
	if rel != nil {
		if st.LastPublished == nil {
			st.LastPublished = map[string]time.Time{}
		}
		if rel.Manifest.Published.After(st.LastPublished[rel.Manifest.Channel]) {
			st.LastPublished[rel.Manifest.Channel] = rel.Manifest.Published.UTC()
		}
	}
	if serr := u.saveStatus(st); serr != nil {
		u.o.Logf("selfupdate: cannot write status: %v", serr)
	}
	return rel, err
}

func (u *Updater) check(ctx context.Context) (*Release, error) {
	if !u.Configured() {
		return nil, ErrNotConfigured
	}
	mu, err := u.o.Settings.ChannelManifestURL()
	if err != nil {
		return nil, err
	}
	mctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	mb, err := fetchLimited(mctx, u.o.HTTPClient, mu, u.ua, MaxManifestBytes)
	if err != nil {
		return nil, fmt.Errorf("manifest: %w", err)
	}
	sb, err := fetchLimited(mctx, u.o.HTTPClient, mu+".sig", u.ua, 4096)
	if err != nil {
		return nil, fmt.Errorf("manifest signature: %w", err)
	}
	m, err := VerifyManifest(u.keys, mb, sb)
	if err != nil {
		return nil, err
	}
	if err := u.checkPolicy(m); err != nil {
		return nil, err
	}
	f, ok := m.FileFor(u.o.Target)
	if !ok {
		return nil, fmt.Errorf("release %s has no build for %s", m.Version, u.o.Target)
	}
	fu, err := ResolveFileURL(mu, f)
	if err != nil {
		return nil, err
	}
	return &Release{Manifest: m, ManifestURL: mu, File: f, FileURL: fu,
		Newer: CompareVersions(m.Version, u.o.CurrentVersion) > 0}, nil
}

// checkPolicy applies this client's rules to a verified manifest.
func (u *Updater) checkPolicy(m *Manifest) error {
	now := u.o.Now()
	if m.Product != u.o.Product {
		return fmt.Errorf("manifest is for %q, not %q", m.Product, u.o.Product)
	}
	if m.Channel != u.o.Settings.Channel {
		return fmt.Errorf("manifest is for channel %q, not %q", m.Channel, u.o.Settings.Channel)
	}
	if m.Published.After(now.Add(24 * time.Hour)) {
		return fmt.Errorf("manifest publication time %s is in the future (check the clock)", m.Published.Format(time.RFC3339))
	}
	if !m.Expires.IsZero() && !now.Before(m.Expires) {
		return fmt.Errorf("manifest expired at %s", m.Expires.Format(time.RFC3339))
	}
	if last := u.loadStatus().LastPublished[m.Channel]; m.Published.Before(last) {
		return fmt.Errorf("manifest (published %s) is older than one already seen (%s): refusing a replayed manifest",
			m.Published.Format(time.RFC3339), last.Format(time.RFC3339))
	}
	return nil
}

// Download fetches the release artifact into StateDir and verifies its
// size, SHA-256 and executable format. The caller passes the returned
// path to Install (which removes it).
func (u *Updater) Download(ctx context.Context, rel *Release) (string, error) {
	dir := filepath.Join(u.o.StateDir, "download")
	cleanStaging(dir, u.o.Now())
	p, err := download(ctx, u.o.HTTPClient, rel.FileURL, u.ua, rel.File, dir, int64(u.o.Settings.MaxDownloadMB)<<20)
	if err != nil {
		return "", err
	}
	if err := checkExecutableFormat(p, u.o.Target); err != nil {
		os.Remove(p)
		return "", err
	}
	return p, nil
}

// UpdateOptions modify Update.
type UpdateOptions struct {
	// CheckOnly reports whether an update exists without installing it.
	CheckOnly bool
	// Force installs the manifest's version even when it is not newer
	// (reinstall, or an explicit downgrade by the operator).
	Force bool
}

// Update runs Check, Download and Install.
func (u *Updater) Update(ctx context.Context, uo UpdateOptions) (Result, error) {
	res := Result{From: u.o.CurrentVersion}
	rel, err := u.Check(ctx)
	if err != nil {
		return res, err
	}
	res.To = rel.Manifest.Version
	if !rel.Newer && !uo.Force {
		res.Outcome = OutcomeUpToDate
		res.To = ""
		return res, nil
	}
	if uo.CheckOnly {
		res.Outcome = OutcomeAvailable
		if !rel.Newer {
			res.Outcome = OutcomeUpToDate
		}
		return res, nil
	}
	if err := CheckWritable(u.o.Exe); err != nil {
		return res, err
	}
	u.o.Logf("selfupdate: downloading %s %s for %s", u.o.Product, rel.Manifest.Version, u.o.Target)
	p, err := u.Download(ctx, rel)
	if err != nil {
		return res, err
	}
	return u.Install(ctx, rel, p, uo.Force)
}

// Install swaps the verified artifact in, restarts the program and either
// supervises the health window (Probe set) or leaves the confirmation to
// the new process (Probe nil). On an unhealthy result the previous binary
// is restored and restarted, and the error wraps ErrRolledBack.
func (u *Updater) Install(ctx context.Context, rel *Release, verified string, force bool) (Result, error) {
	defer os.Remove(verified)
	res := Result{From: u.o.CurrentVersion, To: rel.Manifest.Version}
	if u.o.Restarter == nil {
		return res, errors.New("selfupdate: no Restarter configured")
	}
	release, err := acquireLock(u.o.StateDir, u.o.Now())
	if err != nil {
		return res, err
	}
	locked := true
	unlock := func() {
		if locked {
			release()
			locked = false
		}
	}
	defer unlock()

	exe := u.o.Exe
	cleanupDeferred(exe)
	j, err := loadJournal(u.o.StateDir)
	if err != nil {
		return res, err
	}
	if j.Phase == PhaseSwapped && u.o.Now().Before(j.Deadline.Add(10*time.Minute)) {
		return res, fmt.Errorf("%w: the update to %s is still on trial", ErrBusy, j.To)
	}
	fromSHA, err := fileSHA256(exe)
	if err != nil {
		return res, err
	}
	if fromSHA == rel.File.SHA256 && !force {
		res.Outcome, res.Detail = OutcomeUpToDate, "the installed binary already matches the release"
		return res, nil
	}
	if got, err := fileSHA256(verified); err != nil || got != rel.File.SHA256 {
		return res, fmt.Errorf("staged file changed after verification (sha %s)", got)
	}
	next, err := stageNext(exe, verified)
	if err != nil {
		return res, err
	}
	old, err := swapIn(exe, next, u.strategy)
	if err != nil {
		os.Remove(next)
		return res, err
	}
	now := u.o.Now().UTC()
	j = Journal{Phase: PhaseSwapped, Product: u.o.Product, From: u.o.CurrentVersion, To: rel.Manifest.Version,
		Target: u.o.Target, Exe: exe, Old: old, FromSHA: fromSHA, ToSHA: rel.File.SHA256, Started: now,
		Deadline: now.Add(u.o.Health.Window + 2*time.Minute)}
	os.Remove(u.bootsPath())
	if err := saveJournal(u.o.StateDir, j); err != nil {
		if rerr := restoreOld(exe, old, fromSHA, u.strategy); rerr != nil {
			return res, fmt.Errorf("write journal: %v; restore: %v", err, rerr)
		}
		return res, fmt.Errorf("write journal: %w", err)
	}
	u.o.Logf("selfupdate: %s %s -> %s installed; restarting", u.o.Product, u.o.CurrentVersion, rel.Manifest.Version)

	if u.o.Probe == nil {
		// Self-confirm mode: the new process runs Recover/Trial.Confirm.
		unlock()
		err := u.o.Restarter.Restart(ctx)
		if err == nil || errors.Is(err, ErrExitRequested) {
			res.Outcome, res.Detail = OutcomePending, "the new version confirms itself after the health window"
			return res, err
		}
		u.o.Logf("selfupdate: restart failed (%v); restoring %s", err, u.o.CurrentVersion)
		if rerr := u.rollback(&j, "restart failed: "+err.Error()); rerr != nil {
			return res, fmt.Errorf("restart: %v; rollback: %w", err, rerr)
		}
		res.Outcome = OutcomeRolledBack
		return res, fmt.Errorf("%w: restart failed: %v", ErrRolledBack, err)
	}

	herr := u.o.Restarter.Restart(ctx)
	if herr == nil {
		pol := u.o.Health
		pol.ExpectVersion = rel.Manifest.Version
		herr = WaitHealthy(ctx, u.o.Probe, pol)
		if herr != nil && ctx.Err() != nil {
			return res, fmt.Errorf("update interrupted during the health window (the journal keeps the trial; the next start decides): %w", ctx.Err())
		}
	} else {
		herr = fmt.Errorf("restart: %w", herr)
	}
	if herr == nil {
		if cur, err := loadJournal(u.o.StateDir); err == nil && cur.Phase == PhaseRolledBack {
			herr = fmt.Errorf("the new version rolled itself back: %s", cur.Result)
		}
	}
	if herr == nil {
		u.commit(&j, "healthy for "+u.o.Health.Window.String())
		res.Outcome = OutcomeUpdated
		u.o.Logf("selfupdate: %s %s is healthy; update kept", u.o.Product, rel.Manifest.Version)
		return res, nil
	}
	u.o.Logf("selfupdate: %s %s failed its health check (%v); rolling back to %s", u.o.Product, rel.Manifest.Version, herr, u.o.CurrentVersion)
	if rerr := u.rollback(&j, herr.Error()); rerr != nil {
		return res, fmt.Errorf("%v; ROLLBACK FAILED: %w", herr, rerr)
	}
	res.Outcome = OutcomeRolledBack
	res.Detail = herr.Error()
	if err := u.o.Restarter.Restart(ctx); err != nil {
		return res, fmt.Errorf("%w: %v; restarting the previous version failed: %v", ErrRolledBack, herr, err)
	}
	pol := u.o.Health
	pol.ExpectVersion = u.o.CurrentVersion
	if err := WaitHealthy(ctx, u.o.Probe, pol); err != nil {
		u.o.Logf("selfupdate: the restored version %s is not healthy either: %v", u.o.CurrentVersion, err)
		res.Detail += "; the restored version is not healthy either: " + err.Error()
	}
	return res, fmt.Errorf("%w: %v", ErrRolledBack, herr)
}

func (u *Updater) commit(j *Journal, why string) {
	j.Phase, j.Result, j.Finished = PhaseCommitted, why, u.o.Now().UTC()
	if err := saveJournal(u.o.StateDir, *j); err != nil {
		u.o.Logf("selfupdate: cannot write journal: %v", err)
	}
	os.Remove(u.bootsPath())
	removeOrDefer(j.Old)
}

func (u *Updater) rollback(j *Journal, why string) error {
	if err := restoreOld(j.Exe, j.Old, j.FromSHA, u.strategy); err != nil {
		j.Result = "rollback failed: " + err.Error()
		_ = saveJournal(u.o.StateDir, *j)
		return err
	}
	j.Phase, j.Result, j.Finished = PhaseRolledBack, why, u.o.Now().UTC()
	os.Remove(u.bootsPath())
	return saveJournal(u.o.StateDir, *j)
}

func (u *Updater) bootsPath() string { return filepath.Join(u.o.StateDir, "selfupdate.boots") }

// Trial is a new version on probation, returned by Recover.
type Trial struct {
	u *Updater
	j Journal
}

// Version is the version on trial.
func (t *Trial) Version() string { return t.j.To }

// Recover must be called early at program start (after the configuration
// is loaded, before listeners open). It
//   - waits for the previous instance to exit after a Windows
//     SelfRestarter restart,
//   - counts start-ups of a new version on trial and rolls back after
//     MaxTrialBoots (crash loop): it then returns an error wrapping
//     ErrRolledBack and the program must exit non-zero so its supervisor
//     starts the previous binary (or restart itself),
//   - returns a Trial when this process is the new version on trial; the
//     program calls Trial.Confirm once it is up.
//
// It returns (nil, nil) when nothing is pending.
func (u *Updater) Recover() (*Trial, error) {
	waitPIDFromEnv(u.o.Getenv, 30*time.Second)
	j, err := loadJournal(u.o.StateDir)
	if err != nil || j.Phase != PhaseSwapped || !samePath(j.Exe, u.o.Exe) {
		return nil, err
	}
	if j.To != u.o.CurrentVersion {
		// The old binary runs: someone restored it (a supervisor, the
		// installer, the operator). Record that, if the files agree.
		if h, err := fileSHA256(u.o.Exe); err == nil && h == j.FromSHA {
			if rel, err := acquireLock(u.o.StateDir, u.o.Now()); err == nil {
				j.Phase, j.Result, j.Finished = PhaseRolledBack, "previous version found running", u.o.Now().UTC()
				_ = saveJournal(u.o.StateDir, j)
				rel()
			}
		}
		return nil, nil
	}
	boots := u.countBoot()
	if boots > u.o.MaxTrialBoots {
		rel, err := acquireLock(u.o.StateDir, u.o.Now())
		if err != nil {
			// A supervisor holds the lock and will roll back itself.
			return nil, nil
		}
		defer rel()
		why := fmt.Sprintf("crash loop: %d start-ups during the trial", boots)
		if err := u.rollback(&j, why); err != nil {
			return nil, fmt.Errorf("selfupdate: %s; rollback failed: %w", why, err)
		}
		u.o.Logf("selfupdate: %s %s %s; previous version %s restored", u.o.Product, j.To, why, j.From)
		return nil, fmt.Errorf("%w: %s", ErrRolledBack, why)
	}
	return &Trial{u: u, j: j}, nil
}

// countBoot appends one line to the boots file and returns the count.
func (u *Updater) countBoot() int {
	p := u.bootsPath()
	if f, err := os.OpenFile(p, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644); err == nil {
		fmt.Fprintf(f, "%d %d\n", os.Getpid(), u.o.Now().Unix())
		f.Sync()
		f.Close()
	}
	b, _ := os.ReadFile(p)
	return strings.Count(string(b), "\n")
}

// Confirm keeps the new version once it has run for the health window
// with check (optional) passing at the end; check failing or the process
// dying earlier leads to a rollback (now, or by the next Recover). It
// returns nil when the version was kept or a supervisor owns the decision,
// and an error wrapping ErrRolledBack when it rolled back (the program
// should then restart, e.g. exit non-zero under a service manager).
func (t *Trial) Confirm(ctx context.Context, check func(ctx context.Context) error) error {
	u := t.u
	timer := time.NewTimer(u.o.Health.Window)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
	}
	var cerr error
	if check != nil {
		cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		cerr = check(cctx)
		cancel()
	}
	rel, err := acquireLock(u.o.StateDir, u.o.Now())
	if err != nil {
		return nil // a supervisor decides
	}
	defer rel()
	j, err := loadJournal(u.o.StateDir)
	if err != nil || j.Phase != PhaseSwapped || j.To != t.j.To {
		return err
	}
	if cerr != nil {
		why := "self check failed: " + cerr.Error()
		if err := u.rollback(&j, why); err != nil {
			return fmt.Errorf("selfupdate: %s; rollback failed: %w", why, err)
		}
		return fmt.Errorf("%w: %s", ErrRolledBack, why)
	}
	u.commit(&j, "self-confirmed after "+u.o.Health.Window.String())
	u.o.Logf("selfupdate: %s %s confirmed; update kept", u.o.Product, j.To)
	return nil
}

// Status is a snapshot for status pages and the "update -status" command.
type Status struct {
	Product   string  `json:"product"`
	Version   string  `json:"version"`
	Target    string  `json:"target"`
	Channel   string  `json:"channel"`
	Enabled   bool    `json:"enabled"`
	LastCheck string  `json:"lastCheck,omitempty"`
	LastError string  `json:"lastError,omitempty"`
	Available string  `json:"available,omitempty"`
	Last      Journal `json:"last"`
}

// Status returns the current state.
func (u *Updater) Status() Status {
	st := u.loadStatus()
	j, _ := loadJournal(u.o.StateDir)
	s := Status{Product: u.o.Product, Version: u.o.CurrentVersion, Target: u.o.Target, Channel: u.o.Settings.Channel,
		Enabled: u.o.Settings.Enabled, LastError: st.LastError, Available: st.Available, Last: j}
	if !st.LastCheck.IsZero() {
		s.LastCheck = st.LastCheck.Format(time.RFC3339)
	}
	return s
}

// checkStatus is the persisted outcome of the last Check.
type checkStatus struct {
	LastCheck     time.Time            `json:"lastCheck,omitzero"`
	LastError     string               `json:"lastError,omitempty"`
	Available     string               `json:"available,omitempty"`
	LastPublished map[string]time.Time `json:"lastPublished,omitempty"`
}

func (u *Updater) statusPath() string { return filepath.Join(u.o.StateDir, "selfupdate-status.json") }

func (u *Updater) loadStatus() checkStatus {
	var st checkStatus
	if b, err := os.ReadFile(u.statusPath()); err == nil {
		_ = json.Unmarshal(b, &st)
	}
	return st
}

func (u *Updater) saveStatus(st checkStatus) error {
	if err := os.MkdirAll(u.o.StateDir, 0o755); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(st, "", "  ")
	return writeFileAtomic(u.statusPath(), append(b, '\n'), 0o644)
}

// Loop runs automatic updates until ctx ends: a first check after a random
// 2-15 minute delay, then every CheckInterval ±25 % (so a fleet of feeders
// spreads its requests). It does nothing unless Settings.Enabled. It
// returns ErrExitRequested when a Windows SelfRestarter started the new
// process (the program must exit).
func (u *Updater) Loop(ctx context.Context) error {
	if !u.o.Settings.Enabled {
		return nil
	}
	if !u.Configured() {
		u.o.Logf("selfupdate: automatic updates enabled but %v", ErrNotConfigured)
		return nil
	}
	delay := 2*time.Minute + time.Duration(rand.Int64N(int64(13*time.Minute)))
	for {
		t := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			t.Stop()
			return nil
		case <-t.C:
		}
		res, err := u.Update(ctx, UpdateOptions{})
		switch {
		case errors.Is(err, ErrExitRequested):
			return err
		case err != nil:
			u.o.Logf("selfupdate: %v", err)
		case res.Outcome != OutcomeUpToDate:
			u.o.Logf("selfupdate: %s %s -> %s", res.Outcome, res.From, res.To)
		}
		iv := u.o.Settings.CheckInterval()
		delay = iv*3/4 + time.Duration(rand.Int64N(int64(iv/2)+1))
	}
}
