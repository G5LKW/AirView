package server

import (
	"bufio"
	"compress/gzip"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

// TestSSEGzip: a client that accepts gzip gets a gzip-encoded event stream
// whose events can be decoded one by one as they arrive (sync flush per
// event); a client that does not gets the plain stream.
func TestSSEGzip(t *testing.T) {
	r := shared(t)
	url := strings.TrimSuffix(r.srv.LocalURL(), "/") + "/api/events"
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	req, _ := http.NewRequestWithContext(ctx, "GET", url, nil)
	// Set explicitly: the transport then hands the raw gzip bytes through.
	req.Header.Set("Accept-Encoding", "gzip")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 || resp.Header.Get("Content-Encoding") != "gzip" {
		t.Fatalf("status %d, encoding %q", resp.StatusCode, resp.Header.Get("Content-Encoding"))
	}
	zr, err := gzip.NewReader(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	sc := bufio.NewScanner(zr)
	sc.Buffer(make([]byte, 0, 1<<20), 8<<20)
	var event string
	var times []time.Time
	for len(times) < 3 && sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "event: "):
			event = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: ") && event == "aircraft":
			var snap struct {
				Now int64 `json:"now"`
			}
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &snap); err != nil || snap.Now == 0 {
				t.Fatalf("aircraft event: %v", err)
			}
			times = append(times, time.Now())
		}
	}
	if len(times) < 3 {
		t.Fatalf("got %d aircraft events: %v", len(times), sc.Err())
	}
	// Incremental: the events came about 1 s apart, not in one burst at
	// the end of a buffer.
	if d := times[2].Sub(times[1]); d < 300*time.Millisecond {
		t.Errorf("events 2 and 3 arrived %v apart: not flushed per event", d)
	}

	// Without gzip: plain text/event-stream.
	plain := &http.Client{Transport: &http.Transport{DisableCompression: true}}
	req2, _ := http.NewRequestWithContext(ctx, "GET", url, nil)
	resp2, err := plain.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if enc := resp2.Header.Get("Content-Encoding"); enc != "" {
		t.Fatalf("plain client got encoding %q", enc)
	}
	sc2 := bufio.NewScanner(resp2.Body)
	if !sc2.Scan() || !strings.HasPrefix(sc2.Text(), "retry:") {
		t.Fatalf("plain stream starts with %q", sc2.Text())
	}
}

// TestSSEGzipCap: once sseGzipMax streams hold a compressor, further
// streams are sent plain even when the client accepts gzip.
func TestSSEGzipCap(t *testing.T) {
	r := shared(t)
	h := r.srv.live
	h.gzStreams.Add(sseGzipMax)
	defer h.gzStreams.Add(-sseGzipMax)
	url := strings.TrimSuffix(r.srv.LocalURL(), "/") + "/api/events"
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", url, nil)
	req.Header.Set("Accept-Encoding", "gzip")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if enc := resp.Header.Get("Content-Encoding"); resp.StatusCode != 200 || enc != "" {
		t.Fatalf("status %d, encoding %q; want a plain stream above the cap", resp.StatusCode, enc)
	}
	sc := bufio.NewScanner(resp.Body)
	if !sc.Scan() || !strings.HasPrefix(sc.Text(), "retry:") {
		t.Fatalf("stream starts with %q", sc.Text())
	}
}
