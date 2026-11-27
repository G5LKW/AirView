package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// testKey is a syntactically valid Ed25519 public key (base64).
const testKey = "g5f6wBvtvtt5GfYhg1aQ6eg+cvLmHcfLD61AK20Q/SU="

func TestCheckUpdate(t *testing.T) {
	for _, c := range []struct {
		key, url string
		ok       bool
	}{
		{"", "", true},
		{testKey, "", true},
		{testKey, "https://releases.example.net/airview", true},
		{"", "https://releases.example.net", false}, // a URL without a key: never unsigned
		{"tooshort=", "", false},
		{testKey, "ftp://x", false},
		{testKey, "https://a b", false},
	} {
		if err := checkUpdate(c.key, c.url); (err == nil) != c.ok {
			t.Errorf("checkUpdate(%q, %q) = %v", c.key, c.url, err)
		}
	}
}

// DLV-05: an image cannot be built without the licence texts of its binaries.
func TestPayloadNeedsNotices(t *testing.T) {
	dir := t.TempDir()
	b := fakeBins(t, dir)
	lic := filepath.Join(dir, "LICENSE.txt")
	empty := filepath.Join(dir, "empty.txt")
	os.WriteFile(lic, []byte("licence\n"), 0o644)
	os.WriteFile(empty, []byte("\n"), 0o644)
	in := buildInputs{Kind: kinds["feeder"], Version: "1.0.0", Feeder: b.feeder, Pisetup: b.pisetup,
		FeederSetup: filepath.Join(repoRoot(t), "deploy", "feeder", "pi", "install.sh"), Licence: lic}
	if _, err := payload(in); err == nil || !strings.Contains(err.Error(), "notices") {
		t.Fatalf("payload without notices: %v", err)
	}
	in.Notices = empty
	if _, err := payload(in); err == nil {
		t.Fatal("payload with empty notices accepted")
	}
	in.Notices = lic
	in.UpdateURL = "https://releases.example.net"
	if _, err := payload(in); err == nil {
		t.Fatal("update URL without a key accepted")
	}
	in.ManifestKey = testKey
	files, err := payload(in)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]payloadFile{}
	for _, f := range files {
		got[f.Name] = f
	}
	for _, n := range []string{"LICENSE.txt", "THIRD-PARTY-NOTICES.txt", "update.conf"} {
		if f, ok := got[n]; !ok || !f.Checked {
			t.Errorf("%s missing or not in SHA256SUMS", n)
		}
	}
	if u := string(got["update.conf"].Data); !strings.Contains(u, "MANIFEST_KEY="+testKey+"\n") || !strings.Contains(u, "UPDATE_URL=https://releases.example.net\n") {
		t.Errorf("update.conf:\n%s", u)
	}
}
