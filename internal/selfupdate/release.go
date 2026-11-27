package selfupdate

import (
	"crypto/ed25519"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Release-side helpers used by the release tool (deploy/release) and tests.

// ScanArtifacts finds the build outputs of product in dir, named the way
// deploy/build.ps1 names them: "<product>-<target>" and
// "<product>-<target>.exe" (e.g. airview-feeder-linux-arm64,
// airview-feeder-windows-amd64.exe), plus "<product>.exe" as
// windows-amd64 (the main desktop build). It returns target -> path.
func ScanArtifacts(dir, product string) (map[string]string, error) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, e := range ents {
		if e.IsDir() {
			continue
		}
		n := e.Name()
		if n == product+".exe" {
			if _, dup := out["windows-amd64"]; !dup {
				out["windows-amd64"] = filepath.Join(dir, n)
			}
			continue
		}
		rest, ok := strings.CutPrefix(n, product+"-")
		if !ok {
			continue
		}
		t := strings.TrimSuffix(rest, ".exe")
		if !ValidTarget(t) || (strings.HasSuffix(rest, ".exe") != strings.HasPrefix(t, "windows-")) {
			continue
		}
		out[t] = filepath.Join(dir, n) // an explicit -windows-amd64.exe wins over <product>.exe
	}
	return out, nil
}

// PublishOptions describe one release to publish.
type PublishOptions struct {
	Product   string
	Version   string
	Channel   string
	Notes     string
	Published time.Time // default now
	Expires   time.Time // optional
	// Artifacts maps target -> local file.
	Artifacts map[string]string
	// OutDir is the release directory (ReleaseHandler layout); the beta
	// channel goes to OutDir/beta.
	OutDir string
	// URLPrefix is prepended to each target in the manifest's file URLs
	// (default "dl/": relative to the manifest).
	URLPrefix string
	Key       ed25519.PrivateKey
}

// Publish copies the artifacts to <out>[/beta]/dl/<target>, writes and
// signs manifest.json, verifies the result with the key's public half and
// returns the manifest. Existing files of the channel are replaced; the
// manifest is written last (signature first, then the manifest renamed in
// place), so a client never sees a manifest whose files are missing.
func Publish(o PublishOptions) (*Manifest, error) {
	if len(o.Key) != ed25519.PrivateKeySize {
		return nil, errors.New("publish: no signing key")
	}
	if len(o.Artifacts) == 0 {
		return nil, errors.New("publish: no artifacts")
	}
	if o.Published.IsZero() {
		o.Published = time.Now()
	}
	if o.URLPrefix == "" {
		o.URLPrefix = "dl/"
	}
	dir := o.OutDir
	if o.Channel == ChannelBeta {
		dir = filepath.Join(o.OutDir, ChannelBeta)
	}
	m := &Manifest{Schema: SchemaVersion, Product: o.Product, Version: o.Version, Channel: o.Channel,
		Published: o.Published.UTC().Truncate(time.Second), Expires: o.Expires.UTC().Truncate(time.Second), Notes: o.Notes}
	targets := make([]string, 0, len(o.Artifacts))
	for t := range o.Artifacts {
		targets = append(targets, t)
	}
	sort.Strings(targets)
	for _, t := range targets {
		if !ValidTarget(t) {
			return nil, fmt.Errorf("publish: invalid target %q", t)
		}
		sum, size, err := hashFile(o.Artifacts[t])
		if err != nil {
			return nil, fmt.Errorf("publish %s: %w", t, err)
		}
		if err := checkExecutableFormat(o.Artifacts[t], t); err != nil {
			return nil, fmt.Errorf("publish %s (%s): %w", t, o.Artifacts[t], err)
		}
		m.Files = append(m.Files, File{Target: t, URL: o.URLPrefix + t, SHA256: sum, Size: size})
	}
	body, err := m.Marshal()
	if err != nil {
		return nil, fmt.Errorf("publish: %w", err)
	}
	sig := SignManifest(o.Key, body)
	pub := PublicKey(o.Key.Public().(ed25519.PublicKey))
	if _, err := VerifyManifest([]PublicKey{pub}, body, sig); err != nil {
		return nil, fmt.Errorf("publish: self-check: %w", err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "dl"), 0o755); err != nil {
		return nil, err
	}
	for _, f := range m.Files {
		dst := filepath.Join(dir, "dl", f.Target)
		tmp := dst + ".tmp"
		os.Remove(tmp)
		if err := copyFile(o.Artifacts[f.Target], tmp, 0o644); err != nil {
			os.Remove(tmp)
			return nil, err
		}
		if sum, _, err := hashFile(tmp); err != nil || sum != f.SHA256 {
			os.Remove(tmp)
			return nil, fmt.Errorf("publish %s: copy does not match (%v)", f.Target, err)
		}
		if err := os.Rename(tmp, dst); err != nil {
			return nil, err
		}
	}
	// The signature is written before the manifest: during the swap a
	// client may see the new .sig with the old manifest (verification
	// fails, it retries later) but never an unsigned new manifest.
	if err := writeFileAtomic(filepath.Join(dir, "manifest.json.sig"), sig, 0o644); err != nil {
		return nil, err
	}
	if err := writeFileAtomic(filepath.Join(dir, "manifest.json"), body, 0o644); err != nil {
		return nil, err
	}
	return m, nil
}

func hashFile(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	return fmt.Sprintf("%x", h.Sum(nil)), n, nil
}
