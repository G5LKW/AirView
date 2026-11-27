package selfupdate

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"
	"time"
)

// SchemaVersion is the manifest layout version this package writes and
// accepts. A manifest with a higher schema is refused (the client is too old
// to understand it and must be updated by hand or by the installer).
const SchemaVersion = 1

// MaxManifestBytes bounds the manifest download. A real manifest is a few
// kilobytes; the limit protects the client from a hostile server.
const MaxManifestBytes = 1 << 20

// Channels a release can be published on.
const (
	ChannelStable = "stable"
	ChannelBeta   = "beta"
)

// Manifest is a signed release description (one product, one channel, one
// version). Its JSON layout is a contract with the Raspberry Pi installer,
// which parses it with awk (docs/aggregator/HOOKS.md, WP-PI-installer ->
// WP-UP): "version" comes before "files", and every file is a flat object
// (no nested objects) containing its target as a string value.
type Manifest struct {
	Schema    int       `json:"schema"`
	Product   string    `json:"product"`
	Version   string    `json:"version"`
	Channel   string    `json:"channel"`
	Published time.Time `json:"published"`
	// Expires, when set, is the time after which clients refuse the
	// manifest (protection against a server that keeps serving an old
	// signed manifest to hide a newer release). Zero = never.
	Expires time.Time `json:"expires,omitzero"`
	Files   []File    `json:"files"`
	// Notes is a short human-readable description of the release.
	Notes string `json:"notes,omitempty"`
}

// File is one downloadable artifact of a release.
type File struct {
	// Target is "<os>-<arch>": linux-amd64, linux-arm64, linux-armv7,
	// windows-amd64, windows-arm64, ... (see CurrentTarget).
	Target string `json:"target"`
	// URL is absolute (https://...), absolute-path ("/feeder/dl/x") or
	// relative to the manifest URL ("dl/linux-arm64"). Empty means
	// "dl/<target>" relative to the manifest URL.
	URL    string `json:"url,omitempty"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

var (
	targetRe  = regexp.MustCompile(`^(linux|windows|darwin|freebsd)-(amd64|arm64|armv7|386|riscv64)$`)
	productRe = regexp.MustCompile(`^[a-z][a-z0-9-]{0,47}$`)
	sha256Re  = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// ValidTarget reports whether t is a well-formed target name.
func ValidTarget(t string) bool { return targetRe.MatchString(t) }

// ValidProduct reports whether p is a well-formed product name
// ("airview", "airview-feeder").
func ValidProduct(p string) bool { return productRe.MatchString(p) }

// ValidChannel reports whether c is a known channel.
func ValidChannel(c string) bool { return c == ChannelStable || c == ChannelBeta }

// Validate checks the manifest's own consistency (not the signature, not
// the policy of a particular client).
func (m *Manifest) Validate() error {
	var errs []error
	if m.Schema < 1 || m.Schema > SchemaVersion {
		errs = append(errs, fmt.Errorf("schema %d not supported (this build understands up to %d)", m.Schema, SchemaVersion))
	}
	if !ValidProduct(m.Product) {
		errs = append(errs, fmt.Errorf("invalid product %q", m.Product))
	}
	if _, err := ParseVersion(m.Version); err != nil {
		errs = append(errs, err)
	}
	if !ValidChannel(m.Channel) {
		errs = append(errs, fmt.Errorf("invalid channel %q", m.Channel))
	}
	if m.Published.IsZero() {
		errs = append(errs, errors.New("published time missing"))
	}
	if !m.Expires.IsZero() && !m.Expires.After(m.Published) {
		errs = append(errs, errors.New("expires is not after published"))
	}
	if len(m.Files) == 0 {
		errs = append(errs, errors.New("no files"))
	}
	seen := map[string]bool{}
	for i, f := range m.Files {
		if !ValidTarget(f.Target) {
			errs = append(errs, fmt.Errorf("files[%d]: invalid target %q", i, f.Target))
		}
		if seen[f.Target] {
			errs = append(errs, fmt.Errorf("files[%d]: duplicate target %q", i, f.Target))
		}
		seen[f.Target] = true
		if !sha256Re.MatchString(f.SHA256) {
			errs = append(errs, fmt.Errorf("files[%d]: sha256 must be 64 lower-case hex digits", i))
		}
		if f.Size <= 0 {
			errs = append(errs, fmt.Errorf("files[%d]: size must be > 0", i))
		}
		if strings.ContainsAny(f.URL, "\"\\{}\r\n\t ") {
			errs = append(errs, fmt.Errorf("files[%d]: url contains forbidden characters", i))
		}
	}
	return errors.Join(errs...)
}

// FileFor returns the artifact for target.
func (m *Manifest) FileFor(target string) (File, bool) {
	for _, f := range m.Files {
		if f.Target == target {
			return f, true
		}
	}
	return File{}, false
}

// SHA256Bytes returns the decoded digest of f.
func (f File) SHA256Bytes() ([]byte, error) {
	b, err := hex.DecodeString(f.SHA256)
	if err != nil || len(b) != 32 {
		return nil, fmt.Errorf("bad sha256 %q", f.SHA256)
	}
	return b, nil
}

// ParseManifest decodes and validates manifest bytes. It does NOT check
// the signature: callers that act on a manifest must use VerifyManifest.
func ParseManifest(b []byte) (*Manifest, error) {
	if len(b) > MaxManifestBytes {
		return nil, errors.New("manifest too large")
	}
	if err := checkNoDuplicateKeys(b); err != nil {
		return nil, fmt.Errorf("manifest: %w", err)
	}
	var m Manifest
	dec := json.NewDecoder(bytes.NewReader(b))
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("manifest: %w", err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errors.New("manifest: trailing data after the JSON object")
	}
	if err := m.Validate(); err != nil {
		return nil, fmt.Errorf("manifest: %w", err)
	}
	return &m, nil
}

// checkNoDuplicateKeys rejects objects with repeated keys. encoding/json
// keeps the last value while the installer's awk parser keeps the first;
// refusing duplicates keeps both readers in agreement.
func checkNoDuplicateKeys(b []byte) error {
	dec := json.NewDecoder(bytes.NewReader(b))
	var walk func() error
	walk = func() error {
		tok, err := dec.Token()
		if err != nil {
			return err
		}
		d, ok := tok.(json.Delim)
		if !ok {
			return nil
		}
		switch d {
		case '{':
			keys := map[string]bool{}
			for dec.More() {
				kt, err := dec.Token()
				if err != nil {
					return err
				}
				k, _ := kt.(string)
				if keys[k] {
					return fmt.Errorf("duplicate key %q", k)
				}
				keys[k] = true
				if err := walk(); err != nil {
					return err
				}
			}
			_, err = dec.Token()
			return err
		case '[':
			for dec.More() {
				if err := walk(); err != nil {
					return err
				}
			}
			_, err = dec.Token()
			return err
		}
		return nil
	}
	return walk()
}

// Marshal writes the manifest in the canonical layout: fixed key order,
// "version" before "files", one flat object per line for each file, files
// sorted by target, notes last, and the characters '{' and '}' inside
// strings escaped (the installer's awk parser splits records on braces).
// The output ends with a newline. Marshal validates first.
func (m *Manifest) Marshal() ([]byte, error) {
	if err := m.Validate(); err != nil {
		return nil, err
	}
	files := append([]File(nil), m.Files...)
	sort.Slice(files, func(i, j int) bool { return files[i].Target < files[j].Target })
	var b bytes.Buffer
	b.WriteString("{\n")
	fmt.Fprintf(&b, "  \"schema\": %d,\n", m.Schema)
	fmt.Fprintf(&b, "  \"product\": %s,\n", jsonString(m.Product))
	fmt.Fprintf(&b, "  \"version\": %s,\n", jsonString(m.Version))
	fmt.Fprintf(&b, "  \"channel\": %s,\n", jsonString(m.Channel))
	fmt.Fprintf(&b, "  \"published\": %s,\n", jsonString(m.Published.UTC().Format(time.RFC3339)))
	if !m.Expires.IsZero() {
		fmt.Fprintf(&b, "  \"expires\": %s,\n", jsonString(m.Expires.UTC().Format(time.RFC3339)))
	}
	b.WriteString("  \"files\": [\n")
	for i, f := range files {
		fmt.Fprintf(&b, "    {\"target\": %s", jsonString(f.Target))
		if f.URL != "" {
			fmt.Fprintf(&b, ", \"url\": %s", jsonString(f.URL))
		}
		fmt.Fprintf(&b, ", \"sha256\": %s, \"size\": %d}", jsonString(f.SHA256), f.Size)
		if i < len(files)-1 {
			b.WriteByte(',')
		}
		b.WriteByte('\n')
	}
	b.WriteString("  ]")
	if m.Notes != "" {
		fmt.Fprintf(&b, ",\n  \"notes\": %s", jsonString(m.Notes))
	}
	b.WriteString("\n}\n")
	return b.Bytes(), nil
}

// jsonString quotes s as a JSON string with '{' and '}' escaped.
func jsonString(s string) string {
	q, _ := json.Marshal(s)
	r := strings.NewReplacer("{", `\u007b`, "}", `\u007d`)
	return r.Replace(string(q))
}
