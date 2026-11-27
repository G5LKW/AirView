package selfupdate

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeExe returns bytes that pass checkExecutableFormat for this OS.
func fakeExe(tag string) []byte {
	hdr := "\x7fELF"
	if runtime.GOOS == "windows" {
		hdr = "MZ\x90\x00"
	}
	return append([]byte(hdr), bytes.Repeat([]byte(tag), 512)...)
}

// releaseEnv is a release directory served by ReleaseHandler plus an
// installed "program" in a temp directory.
type releaseEnv struct {
	t        *testing.T
	pub      PublicKey
	priv     ed25519.PrivateKey
	relDir   string
	srv      *httptest.Server
	exe      string
	state    string
	manifest string
}

func newReleaseEnv(t *testing.T) *releaseEnv {
	t.Helper()
	pub, priv, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	e := &releaseEnv{t: t, pub: pub, priv: priv, relDir: t.TempDir()}
	mux := http.NewServeMux()
	mux.Handle("/feeder/", http.StripPrefix("/feeder", ReleaseHandler{Dir: e.relDir}))
	e.srv = httptest.NewServer(mux)
	t.Cleanup(e.srv.Close)
	e.manifest = e.srv.URL + "/feeder/manifest.json"
	app := t.TempDir()
	e.exe = filepath.Join(app, "airview-feeder"+exeSuffix())
	if err := os.WriteFile(e.exe, fakeExe("v1"), 0o755); err != nil {
		t.Fatal(err)
	}
	e.state = filepath.Join(t.TempDir(), "state")
	return e
}

func exeSuffix() string {
	if runtime.GOOS == "windows" {
		return ".exe"
	}
	return ""
}

// publish writes a release with one artifact for this target.
func (e *releaseEnv) publish(version, channel string, content []byte, at time.Time) *Manifest {
	e.t.Helper()
	src := filepath.Join(e.t.TempDir(), "artifact")
	if err := os.WriteFile(src, content, 0o755); err != nil {
		e.t.Fatal(err)
	}
	m, err := Publish(PublishOptions{Product: "airview-feeder", Version: version, Channel: channel, Published: at,
		Artifacts: map[string]string{CurrentTarget(): src}, OutDir: e.relDir, Key: e.priv})
	if err != nil {
		e.t.Fatal(err)
	}
	return m
}

type fakeRestarter struct {
	mu    sync.Mutex
	calls int
	err   error
}

func (f *fakeRestarter) Restart(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return f.err
}

// exeProbe reports the version of whatever binary is at exe (as a running
// program would), healthy unless the content is in sick.
type exeProbe struct {
	exe      string
	versions map[string]string // content tag -> version
	sick     map[string]bool
}

func (p exeProbe) Probe(context.Context) (ProbeResult, error) {
	b, err := os.ReadFile(p.exe)
	if err != nil {
		return ProbeResult{}, err
	}
	for tag, v := range p.versions {
		if bytes.Contains(b, []byte(tag+tag)) {
			if p.sick[tag] {
				return ProbeResult{}, errors.New("HTTP 503")
			}
			return ProbeResult{Version: v, Uptime: -1}, nil
		}
	}
	return ProbeResult{}, errors.New("unknown binary")
}

func (e *releaseEnv) updater(cur string, mod func(*Options)) *Updater {
	e.t.Helper()
	o := Options{Product: "airview-feeder", CurrentVersion: cur, Exe: e.exe, StateDir: e.state,
		Settings: Settings{ManifestURL: e.manifest, Keys: []string{e.pub.String()}},
		Health:   HealthPolicy{Window: 300 * time.Millisecond, Interval: 20 * time.Millisecond},
		Logf:     e.t.Logf}
	if mod != nil {
		mod(&o)
	}
	u, err := New(o)
	if err != nil {
		e.t.Fatal(err)
	}
	return u
}

func readFile(t *testing.T, p string) []byte {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestCheck(t *testing.T) {
	e := newReleaseEnv(t)
	now := time.Now()
	e.publish("1.1.0", ChannelStable, fakeExe("v2"), now)
	u := e.updater("1.0.0", nil)
	rel, err := u.Check(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !rel.Newer || rel.Manifest.Version != "1.1.0" || rel.FileURL != e.srv.URL+"/feeder/dl/"+CurrentTarget() {
		t.Fatalf("%+v %s", rel, rel.FileURL)
	}
	if st := u.Status(); st.Available != "1.1.0" || st.LastCheck == "" {
		t.Fatalf("status %+v", st)
	}
	if rel, err := e.updater("1.1.0", nil).Check(context.Background()); err != nil || rel.Newer {
		t.Fatalf("same version: %v %v", rel, err)
	}
	if rel, err := e.updater("2.0.0", nil).Check(context.Background()); err != nil || rel.Newer {
		t.Fatalf("older release: %v %v", rel, err)
	}
	// Not configured.
	if _, err := e.updater("1.0.0", func(o *Options) { o.Settings.Keys = nil }).Check(context.Background()); !errors.Is(err, ErrNotConfigured) {
		t.Errorf("no keys: %v", err)
	}
}

// TestTamperedManifestRejected is the WP-UP acceptance test: any change to
// the served manifest or signature is refused and nothing is installed.
func TestTamperedManifestRejected(t *testing.T) {
	e := newReleaseEnv(t)
	e.publish("1.1.0", ChannelStable, fakeExe("v2"), time.Now())
	mpath := filepath.Join(e.relDir, "manifest.json")
	orig := readFile(t, mpath)
	origSig := readFile(t, mpath+".sig")
	_, otherPriv, _ := GenerateKey()
	cases := map[string]func(){
		"version bumped": func() { os.WriteFile(mpath, bytes.Replace(orig, []byte("1.1.0"), []byte("9.9.9"), 1), 0o644) },
		"hash swapped": func() {
			m, _ := ParseManifest(orig)
			m.Files[0].SHA256 = strings.Repeat("0", 64)
			b, _ := m.Marshal()
			os.WriteFile(mpath, b, 0o644)
		},
		"url redirected": func() {
			os.WriteFile(mpath, bytes.Replace(orig, []byte(`"url": "dl/`), []byte(`"url": "/evil/`), 1), 0o644)
		},
		"signed by another key": func() { os.WriteFile(mpath+".sig", SignManifest(otherPriv, orig), 0o644) },
		"signature corrupted":   func() { os.WriteFile(mpath+".sig", bytes.Replace(origSig, origSig[:4], []byte("AAAA"), 1), 0o644) },
		"signature empty":       func() { os.WriteFile(mpath+".sig", nil, 0o644) },
		"signature missing":     func() { os.Remove(mpath + ".sig") },
	}
	for name, tamper := range cases {
		os.WriteFile(mpath, orig, 0o644)
		os.WriteFile(mpath+".sig", origSig, 0o644)
		tamper()
		fr := &fakeRestarter{}
		u := e.updater("1.0.0", func(o *Options) { o.Restarter = fr; o.Probe = exeProbe{exe: e.exe} })
		res, err := u.Update(context.Background(), UpdateOptions{})
		if err == nil {
			t.Errorf("%s: accepted (%+v)", name, res)
			continue
		}
		if name != "signature missing" && !errors.Is(err, ErrBadSignature) {
			t.Errorf("%s: %v", name, err)
		}
		if !bytes.Equal(readFile(t, e.exe), fakeExe("v1")) || fr.calls != 0 {
			t.Errorf("%s: program changed or restarted", name)
		}
		if st := u.Status(); st.LastError == "" {
			t.Errorf("%s: error not recorded", name)
		}
	}
}

func TestCheckPolicy(t *testing.T) {
	e := newReleaseEnv(t)
	now := time.Now()
	// Wrong product.
	src := filepath.Join(t.TempDir(), "a")
	os.WriteFile(src, fakeExe("v2"), 0o755)
	if _, err := Publish(PublishOptions{Product: "airview", Version: "1.1.0", Channel: ChannelStable, Published: now,
		Artifacts: map[string]string{CurrentTarget(): src}, OutDir: e.relDir, Key: e.priv}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.updater("1.0.0", nil).Check(context.Background()); err == nil || !strings.Contains(err.Error(), "not \"airview-feeder\"") {
		t.Errorf("product: %v", err)
	}
	// Beta manifest served at the stable URL.
	e.publish("1.2.0-beta.1", ChannelBeta, fakeExe("v3"), now)
	b := readFile(t, filepath.Join(e.relDir, "beta", "manifest.json"))
	os.WriteFile(filepath.Join(e.relDir, "manifest.json"), b, 0o644)
	os.WriteFile(filepath.Join(e.relDir, "manifest.json.sig"), readFile(t, filepath.Join(e.relDir, "beta", "manifest.json.sig")), 0o644)
	if _, err := e.updater("1.0.0", nil).Check(context.Background()); err == nil || !strings.Contains(err.Error(), "channel") {
		t.Errorf("channel: %v", err)
	}
	// The beta channel reads beta/manifest.json; its relative URL resolves below beta/.
	rel, err := e.updater("1.0.0", func(o *Options) { o.Settings.Channel = ChannelBeta }).Check(context.Background())
	if err != nil || rel.Manifest.Version != "1.2.0-beta.1" || !strings.Contains(rel.FileURL, "/feeder/beta/dl/") {
		t.Fatalf("beta: %v %+v", err, rel)
	}
	// Replay: a newer manifest was seen, an older one is refused.
	e.publish("1.1.0", ChannelStable, fakeExe("v2"), now)
	u := e.updater("1.0.0", nil)
	if _, err := u.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	e.publish("1.0.5", ChannelStable, fakeExe("v9"), now.Add(-time.Hour))
	if _, err := u.Check(context.Background()); err == nil || !strings.Contains(err.Error(), "replayed") {
		t.Errorf("replay: %v", err)
	}
	// Future publication time.
	e.publish("1.3.0", ChannelStable, fakeExe("v4"), now.Add(48*time.Hour))
	if _, err := u.Check(context.Background()); err == nil || !strings.Contains(err.Error(), "future") {
		t.Errorf("future: %v", err)
	}
	// Expired.
	src2 := filepath.Join(t.TempDir(), "b")
	os.WriteFile(src2, fakeExe("v5"), 0o755)
	Publish(PublishOptions{Product: "airview-feeder", Version: "1.4.0", Channel: ChannelStable, Published: now.Add(-2 * time.Hour),
		Expires: now.Add(-time.Hour), Artifacts: map[string]string{CurrentTarget(): src2}, OutDir: e.relDir, Key: e.priv})
	e2 := e.updater("1.0.0", func(o *Options) { o.StateDir = filepath.Join(t.TempDir(), "s2") })
	if _, err := e2.Check(context.Background()); err == nil || !strings.Contains(err.Error(), "expired") {
		t.Errorf("expired: %v", err)
	}
	// No build for this target.
	other := "linux-riscv64"
	if CurrentTarget() == other {
		other = "linux-386"
	}
	e3 := e.updater("1.0.0", func(o *Options) { o.Target = other; o.StateDir = filepath.Join(t.TempDir(), "s3") })
	e.publish("1.5.0", ChannelStable, fakeExe("v6"), now)
	if _, err := e3.Check(context.Background()); err == nil || !strings.Contains(err.Error(), "no build") {
		t.Errorf("target: %v", err)
	}
}

func TestDownloadVerification(t *testing.T) {
	e := newReleaseEnv(t)
	e.publish("1.1.0", ChannelStable, fakeExe("v2"), time.Now())
	u := e.updater("1.0.0", nil)
	rel, err := u.Check(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	dl := filepath.Join(e.relDir, "dl", CurrentTarget())
	// Artifact replaced on the server after signing.
	os.WriteFile(dl, fakeExe("XX"), 0o644)
	if _, err := u.Download(context.Background(), rel); err == nil || !strings.Contains(err.Error(), "SHA-256") {
		t.Errorf("hash: %v", err)
	}
	// Truncated / extended.
	os.WriteFile(dl, fakeExe("v2")[:100], 0o644)
	if _, err := u.Download(context.Background(), rel); err == nil || !strings.Contains(err.Error(), "size") {
		t.Errorf("short: %v", err)
	}
	os.WriteFile(dl, append(fakeExe("v2"), 0), 0o644)
	if _, err := u.Download(context.Background(), rel); err == nil || !strings.Contains(err.Error(), "size") {
		t.Errorf("long: %v", err)
	}
	// Too large for the configured limit.
	big := *rel
	big.File.Size = 300 << 20
	if _, err := u.Download(context.Background(), &big); err == nil || !strings.Contains(err.Error(), "limit") {
		t.Errorf("limit: %v", err)
	}
	// No partial files remain.
	ents, _ := os.ReadDir(filepath.Join(e.state, "download"))
	if len(ents) != 0 {
		t.Errorf("leftovers: %v", ents)
	}
	// A correctly signed artifact of the wrong format is refused.
	if _, err := Publish(PublishOptions{Product: "airview-feeder", Version: "1.1.1", Channel: ChannelStable,
		Artifacts: map[string]string{CurrentTarget(): writeTemp(t, []byte("#!/bin/sh\necho hi\n"))}, OutDir: e.relDir, Key: e.priv}); err == nil {
		t.Error("Publish accepted a non-executable")
	}
}

func writeTemp(t *testing.T, b []byte) string {
	p := filepath.Join(t.TempDir(), "f")
	os.WriteFile(p, b, 0o755)
	return p
}

func TestInstallHealthyCommits(t *testing.T) {
	for _, st := range []swapStrategy{strategyRenameAside, strategyLinkReplace} {
		t.Run(fmt.Sprint(st), func(t *testing.T) {
			e := newReleaseEnv(t)
			e.publish("1.1.0", ChannelStable, fakeExe("v2"), time.Now())
			fr := &fakeRestarter{}
			u := e.updater("1.0.0", func(o *Options) {
				o.Restarter = fr
				o.Probe = exeProbe{exe: e.exe, versions: map[string]string{"v1": "1.0.0", "v2": "1.1.0"}}
			})
			u.strategy = st
			res, err := u.Update(context.Background(), UpdateOptions{})
			if err != nil || res.Outcome != OutcomeUpdated || res.To != "1.1.0" {
				t.Fatalf("%+v %v", res, err)
			}
			if !bytes.Equal(readFile(t, e.exe), fakeExe("v2")) || fr.calls != 1 {
				t.Fatal("not installed")
			}
			if _, err := os.Stat(e.exe + suffixOld); !os.IsNotExist(err) {
				t.Error(".old kept after commit")
			}
			if j := u.Status().Last; j.Phase != PhaseCommitted || j.From != "1.0.0" || j.To != "1.1.0" {
				t.Errorf("journal %+v", j)
			}
			if fi, _ := os.Stat(e.exe); runtime.GOOS != "windows" && fi.Mode().Perm()&0o100 == 0 {
				t.Error("not executable")
			}
			// Second run: nothing to do.
			u2 := e.updater("1.1.0", func(o *Options) { o.Restarter = fr; o.Probe = exeProbe{exe: e.exe} })
			if res, err := u2.Update(context.Background(), UpdateOptions{}); err != nil || res.Outcome != OutcomeUpToDate {
				t.Fatalf("%+v %v", res, err)
			}
			if res, err := u2.Update(context.Background(), UpdateOptions{CheckOnly: true}); err != nil || res.Outcome != OutcomeUpToDate {
				t.Fatalf("check only: %+v %v", res, err)
			}
			// Force with the same binary installed is a no-op on the files (hash equal), not an error.
			if res, err := u2.Update(context.Background(), UpdateOptions{Force: true}); err == nil && res.Outcome != OutcomeUpdated && res.Outcome != OutcomeUpToDate {
				t.Fatalf("force: %+v %v", res, err)
			}
		})
	}
}

func TestInstallUnhealthyRollsBack(t *testing.T) {
	for _, st := range []swapStrategy{strategyRenameAside, strategyLinkReplace} {
		t.Run(fmt.Sprint(st), func(t *testing.T) {
			e := newReleaseEnv(t)
			e.publish("1.1.0", ChannelStable, fakeExe("v2"), time.Now())
			fr := &fakeRestarter{}
			u := e.updater("1.0.0", func(o *Options) {
				o.Restarter = fr
				o.Probe = exeProbe{exe: e.exe, versions: map[string]string{"v1": "1.0.0", "v2": "1.1.0"}, sick: map[string]bool{"v2": true}}
			})
			u.strategy = st
			res, err := u.Update(context.Background(), UpdateOptions{})
			if !errors.Is(err, ErrRolledBack) || res.Outcome != OutcomeRolledBack {
				t.Fatalf("%+v %v", res, err)
			}
			if !bytes.Equal(readFile(t, e.exe), fakeExe("v1")) {
				t.Fatal("previous binary not restored")
			}
			if fr.calls != 2 {
				t.Errorf("restarts %d, want 2 (new, then previous)", fr.calls)
			}
			if !bytes.Equal(readFile(t, e.exe+suffixFailed), fakeExe("v2")) {
				t.Error("failed binary not kept for diagnosis")
			}
			if j := u.Status().Last; j.Phase != PhaseRolledBack || !strings.Contains(j.Result, "503") {
				t.Errorf("journal %+v", j)
			}
		})
	}
}

func TestInstallWrongVersionRollsBack(t *testing.T) {
	e := newReleaseEnv(t)
	e.publish("1.1.0", ChannelStable, fakeExe("v2"), time.Now())
	// The new binary runs but reports the old version (e.g. a stale build).
	u := e.updater("1.0.0", func(o *Options) {
		o.Restarter = &fakeRestarter{}
		o.Probe = exeProbe{exe: e.exe, versions: map[string]string{"v1": "1.0.0", "v2": "1.0.0"}}
	})
	if _, err := u.Update(context.Background(), UpdateOptions{}); !errors.Is(err, ErrRolledBack) {
		t.Fatal(err)
	}
	if !bytes.Equal(readFile(t, e.exe), fakeExe("v1")) {
		t.Fatal("not restored")
	}
}

func TestInstallRestartFailureRollsBack(t *testing.T) {
	e := newReleaseEnv(t)
	e.publish("1.1.0", ChannelStable, fakeExe("v2"), time.Now())
	fr := &fakeRestarter{err: errors.New("access denied")}
	u := e.updater("1.0.0", func(o *Options) {
		o.Restarter = fr
		o.Probe = exeProbe{exe: e.exe, versions: map[string]string{"v1": "1.0.0", "v2": "1.1.0"}}
	})
	if _, err := u.Update(context.Background(), UpdateOptions{}); !errors.Is(err, ErrRolledBack) || !strings.Contains(err.Error(), "access denied") {
		t.Fatal(err)
	}
	if !bytes.Equal(readFile(t, e.exe), fakeExe("v1")) {
		t.Fatal("not restored")
	}
}

func TestLockBusy(t *testing.T) {
	e := newReleaseEnv(t)
	e.publish("1.1.0", ChannelStable, fakeExe("v2"), time.Now())
	rel, err := acquireLock(e.state, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	u := e.updater("1.0.0", func(o *Options) { o.Restarter = &fakeRestarter{}; o.Probe = exeProbe{exe: e.exe} })
	if _, err := u.Update(context.Background(), UpdateOptions{}); !errors.Is(err, ErrBusy) {
		t.Fatalf("busy: %v", err)
	}
	rel()
	// A stale lock is taken over.
	os.WriteFile(filepath.Join(e.state, "selfupdate.lock"), []byte(fmt.Sprintf("1 %d\n", time.Now().Add(-2*time.Hour).Unix())), 0o644)
	r2, err := acquireLock(e.state, time.Now())
	if err != nil {
		t.Fatalf("stale lock: %v", err)
	}
	r2()
}

// TestSelfConfirmMode covers the in-process path (SelfRestarter): the new
// process confirms itself, or rolls back after a crash loop.
func TestSelfConfirmMode(t *testing.T) {
	setup := func(t *testing.T) (*releaseEnv, *fakeRestarter) {
		e := newReleaseEnv(t)
		e.publish("1.1.0", ChannelStable, fakeExe("v2"), time.Now())
		fr := &fakeRestarter{}
		u := e.updater("1.0.0", func(o *Options) { o.Restarter = fr })
		res, err := u.Update(context.Background(), UpdateOptions{})
		if err != nil || res.Outcome != OutcomePending {
			t.Fatalf("%+v %v", res, err)
		}
		if !bytes.Equal(readFile(t, e.exe), fakeExe("v2")) {
			t.Fatal("not swapped")
		}
		return e, fr
	}
	t.Run("confirm", func(t *testing.T) {
		e, _ := setup(t)
		nu := e.updater("1.1.0", nil)
		tr, err := nu.Recover()
		if err != nil || tr == nil || tr.Version() != "1.1.0" {
			t.Fatalf("%v %v", tr, err)
		}
		if err := tr.Confirm(context.Background(), func(context.Context) error { return nil }); err != nil {
			t.Fatal(err)
		}
		if j := nu.Status().Last; j.Phase != PhaseCommitted {
			t.Fatalf("%+v", j)
		}
		if _, err := os.Stat(e.exe + suffixOld); !os.IsNotExist(err) {
			t.Error(".old kept")
		}
		if tr, err := nu.Recover(); tr != nil || err != nil {
			t.Fatal("trial after commit")
		}
	})
	t.Run("self check fails", func(t *testing.T) {
		e, _ := setup(t)
		nu := e.updater("1.1.0", nil)
		tr, _ := nu.Recover()
		err := tr.Confirm(context.Background(), func(context.Context) error { return errors.New("no uplink config") })
		if !errors.Is(err, ErrRolledBack) || !bytes.Equal(readFile(t, e.exe), fakeExe("v1")) {
			t.Fatalf("%v", err)
		}
	})
	t.Run("crash loop", func(t *testing.T) {
		e, _ := setup(t)
		nu := e.updater("1.1.0", func(o *Options) { o.MaxTrialBoots = 3 })
		for i := 0; i < 3; i++ {
			if tr, err := nu.Recover(); err != nil || tr == nil {
				t.Fatalf("boot %d: %v %v", i, tr, err)
			}
		}
		if _, err := nu.Recover(); !errors.Is(err, ErrRolledBack) {
			t.Fatalf("4th boot: %v", err)
		}
		if !bytes.Equal(readFile(t, e.exe), fakeExe("v1")) {
			t.Fatal("not restored")
		}
		// The restored old version starts normally.
		if tr, err := e.updater("1.0.0", nil).Recover(); tr != nil || err != nil {
			t.Fatalf("old: %v %v", tr, err)
		}
	})
	t.Run("old version found running", func(t *testing.T) {
		e, _ := setup(t)
		os.Remove(e.exe)
		os.Rename(e.exe+suffixOld, e.exe) // the operator restored by hand
		ou := e.updater("1.0.0", nil)
		if tr, err := ou.Recover(); tr != nil || err != nil {
			t.Fatal(tr, err)
		}
		if j := ou.Status().Last; j.Phase != PhaseRolledBack {
			t.Fatalf("%+v", j)
		}
	})
	t.Run("restart fails", func(t *testing.T) {
		e := newReleaseEnv(t)
		e.publish("1.1.0", ChannelStable, fakeExe("v2"), time.Now())
		u := e.updater("1.0.0", func(o *Options) { o.Restarter = &fakeRestarter{err: errors.New("exec format error")} })
		if _, err := u.Update(context.Background(), UpdateOptions{}); !errors.Is(err, ErrRolledBack) {
			t.Fatal(err)
		}
		if !bytes.Equal(readFile(t, e.exe), fakeExe("v1")) {
			t.Fatal("not restored")
		}
	})
	t.Run("busy while on trial", func(t *testing.T) {
		e, fr := setup(t)
		u := e.updater("1.1.0", func(o *Options) { o.Restarter = fr })
		e.publish("1.2.0", ChannelStable, fakeExe("v3"), time.Now())
		if _, err := u.Update(context.Background(), UpdateOptions{}); !errors.Is(err, ErrBusy) {
			t.Fatalf("%v", err)
		}
	})
}

func TestRestoreOldIdempotent(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "p")
	os.WriteFile(exe, fakeExe("v1"), 0o755)
	from, _ := fileSHA256(exe)
	next := filepath.Join(dir, "p.new")
	os.WriteFile(next, fakeExe("v2"), 0o755)
	old, err := swapIn(exe, next, strategyRenameAside)
	if err != nil {
		t.Fatal(err)
	}
	if err := restoreOld(exe, old, from, strategyRenameAside); err != nil {
		t.Fatal(err)
	}
	if err := restoreOld(exe, old, from, strategyRenameAside); err != nil {
		t.Fatalf("second restore: %v", err)
	}
	// .old with the wrong content is not restored.
	os.WriteFile(exe, fakeExe("v2"), 0o755)
	os.WriteFile(old, fakeExe("zz"), 0o755)
	if err := restoreOld(exe, old, from, strategyRenameAside); err == nil {
		t.Fatal("restored a foreign .old")
	}
	os.Remove(old)
	if err := restoreOld(exe, old, from, strategyRenameAside); !errors.Is(err, errNothingToRestore) {
		t.Fatal(err)
	}
}

func TestReleaseHandler(t *testing.T) {
	e := newReleaseEnv(t)
	e.publish("1.1.0", ChannelStable, fakeExe("v2"), time.Now())
	os.WriteFile(filepath.Join(e.relDir, "secret.txt"), []byte("x"), 0o644)
	get := func(p string) (int, http.Header) {
		resp, err := http.Get(e.srv.URL + p)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode, resp.Header
	}
	for _, p := range []string{"/feeder/manifest.json", "/feeder/manifest.json.sig", "/feeder/dl/" + CurrentTarget()} {
		if c, h := get(p); c != 200 || h.Get("Cache-Control") != "no-cache" {
			t.Errorf("%s: %d %v", p, c, h)
		}
	}
	for _, p := range []string{"/feeder/secret.txt", "/feeder/dl/../secret.txt", "/feeder/dl/linux-amd64/../../secret.txt",
		"/feeder/dl/nope", "/feeder/beta/manifest.json", "/feeder/", "/feeder/dl/"} {
		if c, _ := get(p); c != 404 {
			t.Errorf("%s: %d", p, c)
		}
	}
	os.Remove(filepath.Join(e.relDir, "manifest.json.sig"))
	if c, _ := get("/feeder/manifest.json"); c != 404 {
		t.Errorf("unsigned manifest served: %d", c)
	}
	resp, _ := http.Post(e.srv.URL+"/feeder/manifest.json", "text/plain", nil)
	if resp.StatusCode != 405 {
		t.Errorf("POST %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestScanArtifacts(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{"airview-feeder-linux-arm64", "airview-feeder-linux-armv7", "airview-feeder-windows-amd64.exe",
		"airview-feeder-linux-amd64.exe", "airview-feeder-windows-arm64", "airview-feeder-pi.tar.gz", "airview-linux-amd64", "airview.exe", "SHA256SUMS"} {
		os.WriteFile(filepath.Join(dir, n), []byte("x"), 0o644)
	}
	got, err := ScanArtifacts(dir, "airview-feeder")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got["windows-amd64"] == "" || got["linux-armv7"] == "" || got["linux-arm64"] == "" {
		t.Fatalf("%v", got)
	}
	got, _ = ScanArtifacts(dir, "airview")
	if len(got) != 2 || filepath.Base(got["windows-amd64"]) != "airview.exe" || got["linux-amd64"] == "" {
		t.Fatalf("airview: %v", got)
	}
}
