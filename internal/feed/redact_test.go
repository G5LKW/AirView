package feed

import (
	"strings"
	"testing"
)

// Feed URLs may carry user:password or an apikey; neither may reach the
// status (FeedStatus.Addr / LastError), which every LAN client can read.
func TestScrubURLs(t *testing.T) {
	in := `Get "http://feeduser:FeedPassw0rd@127.0.0.1:1/data/aircraft.json?apikey=SECRETKEY123": dial tcp 127.0.0.1:1: refused`
	got := scrubURLs(in)
	if strings.Contains(got, "FeedPassw0rd") || strings.Contains(got, "SECRETKEY123") ||
		!strings.Contains(got, `"http://127.0.0.1:1/data/aircraft.json"`) {
		t.Fatalf("scrubURLs = %q", got)
	}
	if scrubURLs("connection closed by peer") != "connection closed by peer" {
		t.Fatal("plain message changed")
	}
}
