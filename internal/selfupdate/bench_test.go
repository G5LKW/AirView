package selfupdate

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Budgets (DESIGN §20: 1,000+ feeders): a daily check of 10,000 feeders
// is 0.12 requests/s on average, so the costs below matter only for burst
// tolerance. Budget: VerifyManifest < 250 µs; ReleaseHandler manifest
// request < 100 µs (i.e. > 10,000 req/s on one core).

func BenchmarkVerifyManifest(b *testing.B) {
	pub, priv, _ := GenerateKey()
	m := sampleManifest()
	for _, t := range []string{"linux-armv7", "windows-amd64", "windows-arm64"} {
		f := m.Files[0]
		f.Target = t
		m.Files = append(m.Files, f)
	}
	body, _ := m.Marshal()
	sig := SignManifest(priv, body)
	keys := []PublicKey{pub}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := VerifyManifest(keys, body, sig); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkReleaseHandlerManifest(b *testing.B) {
	dir := b.TempDir()
	_, priv, _ := GenerateKey()
	src := filepath.Join(dir, "a")
	os.WriteFile(src, fakeExe("v1"), 0o755)
	if _, err := Publish(PublishOptions{Product: "airview-feeder", Version: "1.0.0", Channel: ChannelStable, Published: time.Now(),
		Artifacts: map[string]string{CurrentTarget(): src}, OutDir: filepath.Join(dir, "rel"), Key: priv}); err != nil {
		b.Fatal(err)
	}
	h := ReleaseHandler{Dir: filepath.Join(dir, "rel")}
	req := httptest.NewRequest("GET", "/manifest.json", nil)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != 200 {
			b.Fatal(w.Code)
		}
	}
}
