package selfupdate

import (
	"bytes"

	"encoding/base64"
	"errors"
	"strings"
	"testing"
	"time"
)

func sampleManifest() *Manifest {
	return &Manifest{Schema: 1, Product: "airview-feeder", Version: "1.2.3", Channel: ChannelStable,
		Published: time.Date(2026, 11, 14, 10, 0, 0, 0, time.UTC),
		Files: []File{
			{Target: "linux-arm64", URL: "dl/linux-arm64", SHA256: strings.Repeat("ab", 32), Size: 100},
			{Target: "linux-amd64", SHA256: strings.Repeat("cd", 32), Size: 200},
		},
		Notes: "fixes {braces} and \"quotes\""}
}

func TestManifestRoundTrip(t *testing.T) {
	m := sampleManifest()
	b, err := m.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	got, err := ParseManifest(b)
	if err != nil {
		t.Fatalf("%v\n%s", err, b)
	}
	if got.Version != m.Version || got.Notes != m.Notes || len(got.Files) != 2 || !got.Published.Equal(m.Published) {
		t.Fatalf("round trip: %+v", got)
	}
	// Files are sorted by target.
	if got.Files[0].Target != "linux-amd64" || got.Files[1].URL != "dl/linux-arm64" {
		t.Fatalf("order: %+v", got.Files)
	}
	b2, _ := got.Marshal()
	if !bytes.Equal(b, b2) {
		t.Fatalf("not canonical:\n%s\n%s", b, b2)
	}
}

// TestManifestInstallerContract checks the layout the Pi installer's awk
// parser relies on (HOOKS.md, WP-PI-installer -> WP-UP).
func TestManifestInstallerContract(t *testing.T) {
	b, _ := sampleManifest().Marshal()
	s := string(b)
	if strings.Index(s, `"version"`) > strings.Index(s, `"files"`) {
		t.Fatal("version must come before files")
	}
	// Braces appear only as JSON structure: 1 top-level pair + 1 per file.
	if strings.Count(s, "{") != 3 || strings.Count(s, "}") != 3 {
		t.Fatalf("braces inside strings must be escaped:\n%s", s)
	}
	// Every file object is flat and on one line with its target as a value.
	for _, line := range strings.Split(s, "\n") {
		if strings.Contains(line, `"target"`) {
			if !strings.HasPrefix(strings.TrimSpace(line), "{") || !strings.Contains(line, "}") || !strings.Contains(line, `"sha256": "`) {
				t.Fatalf("file line %q", line)
			}
		}
	}
	if !strings.HasSuffix(s, "}\n") {
		t.Fatal("trailing newline")
	}
}

func TestManifestValidation(t *testing.T) {
	cases := map[string]func(m *Manifest){
		"schema":      func(m *Manifest) { m.Schema = 2 },
		"product":     func(m *Manifest) { m.Product = "Air View" },
		"version":     func(m *Manifest) { m.Version = "one" },
		"channel":     func(m *Manifest) { m.Channel = "nightly" },
		"published":   func(m *Manifest) { m.Published = time.Time{} },
		"expires":     func(m *Manifest) { m.Expires = m.Published.Add(-time.Hour) },
		"no files":    func(m *Manifest) { m.Files = nil },
		"target":      func(m *Manifest) { m.Files[0].Target = "linux-armv6" },
		"dup target":  func(m *Manifest) { m.Files[1].Target = m.Files[0].Target },
		"sha":         func(m *Manifest) { m.Files[0].SHA256 = strings.Repeat("AB", 32) },
		"size":        func(m *Manifest) { m.Files[0].Size = 0 },
		"url quote":   func(m *Manifest) { m.Files[0].URL = `dl/"x` },
		"url brace":   func(m *Manifest) { m.Files[0].URL = `dl/{x}` },
		"url newline": func(m *Manifest) { m.Files[0].URL = "dl/x\n" },
	}
	for name, mut := range cases {
		m := sampleManifest()
		mut(m)
		if err := m.Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
		if _, err := m.Marshal(); err == nil {
			t.Errorf("%s: marshalled", name)
		}
	}
}

func TestParseManifestStrict(t *testing.T) {
	b, _ := sampleManifest().Marshal()
	if _, err := ParseManifest(append(append([]byte{}, b...), []byte(`{"x":1}`)...)); err == nil {
		t.Error("trailing data accepted")
	}
	dup := bytes.Replace(b, []byte(`"version": "1.2.3",`), []byte(`"version": "1.2.3", "version": "9.9.9",`), 1)
	if _, err := ParseManifest(dup); err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Errorf("duplicate key: %v", err)
	}
	dupFile := bytes.Replace(b, []byte(`"sha256": "`+strings.Repeat("ab", 32)+`"`),
		[]byte(`"sha256": "`+strings.Repeat("ab", 32)+`", "sha256": "`+strings.Repeat("00", 32)+`"`), 1)
	if _, err := ParseManifest(dupFile); err == nil {
		t.Error("duplicate key inside a file accepted")
	}
	if _, err := ParseManifest(bytes.Repeat([]byte(" "), MaxManifestBytes+1)); err == nil {
		t.Error("oversized accepted")
	}
	// Unknown fields are allowed (forward compatibility of optional data).
	extra := bytes.Replace(b, []byte(`"schema": 1,`), []byte(`"schema": 1, "future": [1, 2],`), 1)
	if _, err := ParseManifest(extra); err != nil {
		t.Errorf("unknown field: %v", err)
	}
}

func TestSignVerifyAndTamper(t *testing.T) {
	pub, priv, _ := GenerateKey()
	b, _ := sampleManifest().Marshal()
	sig := SignManifest(priv, b)
	if !bytes.HasSuffix(sig, []byte("\n")) || len(bytes.TrimSpace(sig)) != 88 {
		t.Fatalf("signature format %q", sig)
	}
	if _, err := VerifyManifest([]PublicKey{pub}, b, sig); err != nil {
		t.Fatal(err)
	}
	// White space in the signature file is ignored (the installer allows it too).
	if _, err := VerifyManifest([]PublicKey{pub}, b, []byte("  "+string(bytes.TrimSpace(sig))+"\r\n")); err != nil {
		t.Fatal(err)
	}
	tampers := map[string][]byte{
		"version":    bytes.Replace(b, []byte("1.2.3"), []byte("1.2.4"), 1),
		"sha":        bytes.Replace(b, []byte("abab"), []byte("abac"), 1),
		"whitespace": append(append([]byte{}, b...), ' '),
		"truncated":  b[:len(b)-1],
	}
	for name, tb := range tampers {
		if _, err := VerifyManifest([]PublicKey{pub}, tb, sig); !errors.Is(err, ErrBadSignature) {
			t.Errorf("tampered %s: %v", name, err)
		}
	}
	// Every single-bit flip of the manifest is rejected.
	for i := 0; i < len(b); i += 7 {
		c := append([]byte{}, b...)
		c[i] ^= 0x01
		if _, err := VerifyManifest([]PublicKey{pub}, c, sig); err == nil {
			t.Fatalf("bit flip at %d accepted", i)
		}
	}
	// Tampered signature.
	raw, _ := DecodeSignature(sig)
	raw[10] ^= 0x80
	if _, err := VerifyManifest([]PublicKey{pub}, b, []byte(base64.StdEncoding.EncodeToString(raw))); !errors.Is(err, ErrBadSignature) {
		t.Errorf("tampered signature: %v", err)
	}
	// Wrong key, no key, garbage signature.
	other, _, _ := GenerateKey()
	if _, err := VerifyManifest([]PublicKey{other}, b, sig); !errors.Is(err, ErrBadSignature) {
		t.Errorf("wrong key: %v", err)
	}
	if _, err := VerifyManifest(nil, b, sig); err == nil {
		t.Error("no key accepted")
	}
	if _, err := VerifyManifest([]PublicKey{pub}, b, []byte("not base64!")); !errors.Is(err, ErrBadSignature) {
		t.Errorf("garbage signature: %v", err)
	}
	// Rotation: any trusted key may sign.
	if i, err := VerifySignature([]PublicKey{other, pub}, b, sig); err != nil || i != 1 {
		t.Errorf("rotation: %d %v", i, err)
	}
	// A signed but invalid manifest is still refused.
	bad := []byte(`{"schema":1,"product":"airview-feeder","version":"x"}`)
	if _, err := VerifyManifest([]PublicKey{pub}, bad, SignManifest(priv, bad)); err == nil || errors.Is(err, ErrBadSignature) {
		t.Errorf("signed invalid manifest: %v", err)
	}
}

func TestKeysText(t *testing.T) {
	pub, priv, _ := GenerateKey()
	k, err := ParsePublicKey(" " + pub.String() + "\n")
	if err != nil || !bytes.Equal(k, pub) {
		t.Fatal(err)
	}
	if _, err := ParsePublicKey(base64.StdEncoding.EncodeToString(make([]byte, 31))); err == nil {
		t.Error("short key accepted")
	}
	if _, err := ParsePublicKey("@@@"); err == nil {
		t.Error("garbage accepted")
	}
	ks, err := ParsePublicKeys([]string{pub.String() + "," + pub.String(), " " + pub.String()})
	if err != nil || len(ks) != 3 {
		t.Fatalf("%d %v", len(ks), err)
	}
	pemb, err := MarshalPrivateKeyPEM(priv)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(pemb, []byte("-----BEGIN PRIVATE KEY-----")) {
		t.Fatalf("pem %s", pemb)
	}
	back, err := ParsePrivateKeyPEM(pemb)
	if err != nil || !back.Equal(priv) {
		t.Fatal(err)
	}
	if _, err := ParsePrivateKeyPEM([]byte("-----BEGIN PUBLIC KEY-----\nAAAA\n-----END PUBLIC KEY-----\n")); err == nil {
		t.Error("public PEM accepted as private")
	}

}
