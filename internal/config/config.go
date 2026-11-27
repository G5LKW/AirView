// Package config loads, validates, saves and resets the AirView configuration
// file (config.json in the data directory) and resolves where that data
// directory lives.
//
// # Typical use
//
//	dir, err := config.ResolveDataDir(*dataFlag)
//	cfg, warnings, err := config.LoadWithWarnings(dir) // or config.Load(dir)
//	holder := config.NewHolder(cfg)
//
//	// PUT /api/config
//	next, problems := config.ApplyUpdate(incoming, holder.Get()) // MergeSecrets + Validate
//	if err := config.Save(dir, next); err == nil {
//	    holder.Set(next)
//	}
//	reply(config.Redact(next))
//
// # Secret handling on the wire
//
// [Redact] replaces the three secrets (http.adminPasswordHash, hub.password,
// remote.password) by "" (not set) or the marker "set". [MergeSecrets]
// interprets the values a client sends back:
//
//	""  or "set"     keep the stored secret
//	"clear"          remove the secret
//	"plain:<text>"   use <text> as the new secret ("plain:" alone removes it)
//	anything else    the new secret itself
//
// For http.adminPasswordHash the new secret is a plain password: it is hashed
// on the server with [HashPassword] and only the hash is stored, so the hash
// never has to leave the server and the UI never has to compute one. (A value
// that already is a well-formed "sha256:<salt>:<hash>" string is stored
// unchanged, which lets a config backup be restored.)
package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"airview/internal/model"
)

// FileName is the name of the configuration file inside the data directory.
const FileName = "config.json"

// saveMu serialises writers of config.json within this process.
var saveMu sync.Mutex

// ResolveDataDir decides where AirView keeps its data and makes sure the
// directory exists and is writable.
//
//  1. flagValue, when non-empty (the -data flag): used as given (made
//     absolute); an error is returned if it cannot be created or written.
//  2. A "data" directory next to the executable, if it can be created and
//     written (portable / standalone EXE use).
//  3. <os.UserConfigDir()>/AirView (e.g. %AppData%\AirView, ~/.config/AirView).
func ResolveDataDir(flagValue string) (string, error) {
	return resolveDataDir(flagValue, os.Executable, os.UserConfigDir)
}

func resolveDataDir(flagValue string, exePath, userCfg func() (string, error)) (string, error) {
	if v := strings.TrimSpace(flagValue); v != "" {
		dir, err := filepath.Abs(v)
		if err != nil {
			return "", fmt.Errorf("config: data directory %q: %w", v, err)
		}
		if err := ensureWritableDir(dir); err != nil {
			return "", fmt.Errorf("config: data directory %q is not usable: %w", dir, err)
		}
		return dir, nil
	}

	var errs []error
	if exe, err := exePath(); err != nil {
		errs = append(errs, fmt.Errorf("locating executable: %w", err))
	} else {
		if real, err := filepath.EvalSymlinks(exe); err == nil {
			exe = real
		}
		dir := filepath.Join(filepath.Dir(exe), "data")
		if err := ensureWritableDir(dir); err == nil {
			return dir, nil
		} else {
			errs = append(errs, fmt.Errorf("%s: %w", dir, err))
		}
	}

	if base, err := userCfg(); err != nil {
		errs = append(errs, fmt.Errorf("user config directory: %w", err))
	} else {
		dir := filepath.Join(base, model.AppName)
		if err := ensureWritableDir(dir); err == nil {
			return dir, nil
		} else {
			errs = append(errs, fmt.Errorf("%s: %w", dir, err))
		}
	}
	return "", fmt.Errorf("config: no writable data directory found (use -data <dir>): %w", errors.Join(errs...))
}

// ensureWritableDir creates dir if needed and proves a file can be created in it.
func ensureWritableDir(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".airview-write-test-*")
	if err != nil {
		return err
	}
	name := f.Name()
	_ = f.Close()
	return os.Remove(name)
}

// Load reads config.json from dir. It never fails just because the file is
// missing, old or damaged:
//
//   - missing file: model.DefaultConfig() is saved and returned;
//   - older / version-less file: migrated to the current layout and re-saved;
//   - newer file (written by a later AirView): known settings are kept, the
//     original is preserved as config.json.v<N>.bak;
//   - corrupt file: renamed to config.json.bad-<n>, defaults saved and returned.
//
// Everything noteworthy that happened is logged as a warning; use
// [LoadWithWarnings] to receive the messages instead. The returned config is
// always validated and usable, also when err is non-nil (err then reports
// that the file could not be read or written, e.g. a permission problem).
func Load(dir string) (model.Config, error) {
	cfg, warnings, err := LoadWithWarnings(dir)
	for _, w := range warnings {
		slog.Warn("config: " + w)
	}
	return cfg, err
}

// LoadWithWarnings is [Load] returning the warnings (migration notes, values
// corrected by [Validate], a corrupt file that was moved aside) to the caller
// instead of logging them.
func LoadWithWarnings(dir string) (model.Config, []string, error) {
	path := filepath.Join(dir, FileName)
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		cfg, _ := Validate(model.DefaultConfig())
		return cfg, nil, Save(dir, cfg)
	}
	if err != nil {
		cfg, _ := Validate(model.DefaultConfig())
		return cfg, nil, fmt.Errorf("config: reading %s: %w", path, err)
	}

	cfg, warnings, fileVersion, perr := parse(data)
	if perr != nil {
		msg := fmt.Sprintf("%s is corrupt (%v)", FileName, perr)
		if aside, rerr := moveAside(path); rerr != nil {
			msg += fmt.Sprintf("; it could not be moved aside (%v) and is being overwritten", rerr)
		} else {
			msg += "; it was kept as " + filepath.Base(aside)
		}
		msg += "; factory defaults are in use"
		cfg, _ = Validate(model.DefaultConfig())
		return cfg, []string{msg}, Save(dir, cfg)
	}

	if fileVersion > model.ConfigVersion {
		bak := path + ".v" + strconv.Itoa(fileVersion) + ".bak"
		if _, serr := os.Stat(bak); errors.Is(serr, fs.ErrNotExist) {
			if werr := os.WriteFile(bak, data, 0o600); werr != nil {
				warnings = append(warnings, fmt.Sprintf("could not back up the newer config file: %v", werr))
			}
		}
	}

	cfg, problems := Validate(cfg)
	warnings = append(warnings, problems...)

	// Re-save only when the stored bytes are not already the canonical form.
	if want, merr := encode(cfg); merr != nil || !bytes.Equal(want, data) {
		if serr := Save(dir, cfg); serr != nil {
			return cfg, warnings, serr
		}
	}
	return cfg, warnings, nil
}

// parse decodes the bytes of a config file on top of the factory defaults.
// A non-nil error means the file is unusable (corrupt).
func parse(data []byte) (cfg model.Config, warnings []string, fileVersion int, err error) {
	data = bytes.TrimPrefix(data, []byte("\xef\xbb\xbf")) // UTF-8 BOM (Notepad)
	if len(bytes.TrimSpace(data)) == 0 {
		return cfg, nil, 0, errors.New("file is empty")
	}
	// config.json is plain JSON written by AirView, but a hand edit that adds
	// a comment or a trailing comma must not make the whole file "corrupt".
	root, extended, perr := parseJSONC(data)
	if perr != nil {
		return cfg, nil, 0, perr
	}
	if root.Kind != ObjectNode {
		return cfg, nil, 0, errors.New("not a JSON object")
	}
	if extended {
		warnings = append(warnings, FileName+" contains comments or trailing commas; they are removed when AirView saves the file (put hand-written settings in "+OperatorFileName+" instead)")
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(root.JSON(), &raw); err != nil {
		return cfg, nil, 0, err
	}

	if v, ok := raw["version"]; ok {
		var f float64
		if json.Unmarshal(v, &f) == nil && f >= 0 && f < 1e9 {
			fileVersion = int(f)
		}
	}
	switch {
	case fileVersion > model.ConfigVersion:
		warnings = append(warnings, fmt.Sprintf(
			"%s was written by a newer AirView (config version %d, this build understands %d); settings this build does not know are dropped",
			FileName, fileVersion, model.ConfigVersion))
	case fileVersion < model.ConfigVersion:
		for v := fileVersion; v < model.ConfigVersion; v++ {
			if m := migrations[v]; m != nil {
				if err := m(raw); err != nil {
					return cfg, nil, fileVersion, fmt.Errorf("migrating from version %d: %w", v, err)
				}
			}
		}
		warnings = append(warnings, fmt.Sprintf("%s migrated from config version %d to %d",
			FileName, fileVersion, model.ConfigVersion))
	}
	delete(raw, "version")

	doc, err := json.Marshal(raw)
	if err != nil {
		return cfg, nil, fileVersion, err
	}

	// Scalars: decode over the defaults so settings missing from the file
	// keep their default value.
	cfg = model.DefaultConfig()
	if err := json.Unmarshal(doc, &cfg); err != nil {
		var te *json.UnmarshalTypeError
		if !errors.As(err, &te) {
			return cfg, nil, fileVersion, err
		}
		// Wrong JSON type for a field: encoding/json skips such fields and
		// decodes the rest, so the damage is limited to those settings.
		warnings = append(warnings, fmt.Sprintf("setting %q has the wrong type (%s); the default is used", te.Field, te.Value))
	}
	// Lists: take them from a decode into a zero value so that elements never
	// inherit fields from the default list entries.
	var z model.Config
	_ = json.Unmarshal(doc, &z)
	if z.Antennas != nil {
		cfg.Antennas = z.Antennas
	}
	if z.Feeds != nil {
		cfg.Feeds = z.Feeds
	}
	if z.UI.RangeRingsKm != nil {
		cfg.UI.RangeRingsKm = z.UI.RangeRingsKm
	}
	if z.Uplinks != nil {
		cfg.Uplinks = z.Uplinks
	}
	cfg.Version = model.ConfigVersion
	return cfg, warnings, fileVersion, nil
}

// migrations[v] upgrades a raw config document from layout version v to v+1.
// Version 0 means "no version field" (hand-written or pre-release files).
var migrations = map[int]func(raw map[string]json.RawMessage) error{
	0: migrateV0,
}

// migrateV0 accepts version-less files (hand-written, or trimmed down to a
// few settings). Their layout is the version 1 layout, so there is nothing to
// rewrite: the settings present are kept, everything else gets its default.
// Future layout changes add an entry to migrations that edits raw in place.
func migrateV0(map[string]json.RawMessage) error { return nil }

// moveAside renames a damaged config file to config.json.bad-<n> (first free n).
func moveAside(path string) (string, error) {
	for n := 1; n <= 100000; n++ {
		aside := path + ".bad-" + strconv.Itoa(n)
		if _, err := os.Lstat(aside); err == nil {
			continue
		} else if !errors.Is(err, fs.ErrNotExist) {
			return "", err
		}
		return aside, os.Rename(path, aside)
	}
	return "", errors.New("too many .bad files")
}

func encode(cfg model.Config) ([]byte, error) {
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

// Save writes cfg to dir/config.json atomically (temp file in the same
// directory, fsync, rename). The config is passed through [Validate] first so
// the file can never contain out-of-range values or wire-only secret markers;
// callers that keep the config in memory should store the validated value
// (Validate is idempotent, so saving an already validated config writes it
// unchanged).
func Save(dir string, cfg model.Config) error {
	cfg, _ = Validate(cfg)
	data, err := encode(cfg)
	if err != nil {
		return fmt.Errorf("config: encoding: %w", err)
	}

	saveMu.Lock()
	defer saveMu.Unlock()

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("config: %w", err)
	}
	path := filepath.Join(dir, FileName)
	f, err := os.CreateTemp(dir, FileName+".tmp-*")
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}
	tmp := f.Name()
	cleanup := func() {
		_ = f.Close()
		_ = os.Remove(tmp)
	}
	_ = f.Chmod(0o600) // the file holds secrets; best effort (no-op on Windows)
	if _, err := f.Write(data); err != nil {
		cleanup()
		return fmt.Errorf("config: writing %s: %w", tmp, err)
	}
	if err := f.Sync(); err != nil {
		cleanup()
		return fmt.Errorf("config: syncing %s: %w", tmp, err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("config: closing %s: %w", tmp, err)
	}
	if err := renameReplace(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("config: replacing %s: %w", path, err)
	}
	// Persist the rename itself where the platform supports syncing a directory.
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

// renameReplace renames tmp over path. On Windows the destination can be
// briefly locked by a virus scanner or indexer, so a few retries are made.
func renameReplace(tmp, path string) error {
	var err error
	for attempt := 0; attempt < 6; attempt++ {
		if err = os.Rename(tmp, path); err == nil {
			return nil
		}
		time.Sleep(time.Duration(20*(attempt+1)) * time.Millisecond)
	}
	return err
}

// FactoryReset overwrites config.json in dir with model.DefaultConfig() and
// returns it. Wiping the database is the caller's job (store.Reset).
func FactoryReset(dir string) (model.Config, error) {
	cfg, _ := Validate(model.DefaultConfig())
	return cfg, Save(dir, cfg)
}
