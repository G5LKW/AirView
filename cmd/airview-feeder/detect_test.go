package main

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"

	"airview/internal/model"
)

func TestSniff(t *testing.T) {
	beast := appendBeastLong(nil, 12345, 40, esFrame(0x4ca123, 11<<51))
	for in, want := range map[string]string{
		string(beast):                                  model.FeedBeast,
		"xx" + string(beast) + string(beast):           model.FeedBeast, // starts mid-frame
		"*8D4CA2D158C901A0C0B8C2A2B0F4;\n":             model.FeedAVR,
		"@00001234ABCD8D4CA2D158C901A0C0B8C2A2B0F4;\n": model.FeedAVR,
		"MSG,3,1,1,4CA2D1,1,2024/01/01,...\n":          model.FeedSBS,
		"\r\nMSG,8,1,1,4CA2D1\n":                       model.FeedSBS,
		"":                                             "",
		"hello world":                                  "",
	} {
		if got := sniff([]byte(in)); got != want {
			t.Errorf("sniff(%q) = %q, want %q", in[:min(len(in), 20)], got, want)
		}
	}
}

// fakeReceiver serves data (once per connection) on a random port.
func fakeReceiver(t *testing.T, data []byte) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				if len(data) > 0 {
					_, _ = c.Write(data)
				}
				time.Sleep(3 * time.Second)
			}()
		}
	}()
	return ln.Addr().(*net.TCPAddr).Port
}

func TestDetectPort(t *testing.T) {
	var stream []byte
	for i := 0; i < 20; i++ {
		stream = appendBeastLong(stream, uint64(1000*i), 40, esFrame(0x4ca123, 11<<51))
	}
	port := fakeReceiver(t, stream)
	ds := detect(context.Background(), "", port, time.Second)
	if len(ds) != 1 || !ds[0].ok() || ds[0].Format != model.FeedBeast || ds[0].Type != model.FeedBeast {
		t.Fatalf("detect: %+v", ds)
	}
	b, ok := best(ds)
	if !ok || b.Port != port || b.Host != "127.0.0.1" {
		t.Fatalf("best %+v %v", b, ok)
	}

	// SBS data on an unknown port: type from the data
	sport := fakeReceiver(t, []byte("MSG,3,1,1,4CA2D1,1,2024/01/01,12:00:00.000,2024/01/01,12:00:00.000,,35000,,,51.5,-0.1,,,0,0,0,0\n"))
	ds = detect(context.Background(), "127.0.0.1", sport, time.Second)
	if ds[0].Type != model.FeedSBS {
		t.Fatalf("sbs: %+v", ds)
	}
	// quiet listener: listening, type from the well-known port (none: beast)
	qport := fakeReceiver(t, nil)
	ds = detect(context.Background(), "", qport, 200*time.Millisecond)
	if !ds[0].ok() || ds[0].Format != "" || ds[0].Type != model.FeedBeast {
		t.Fatalf("quiet: %+v", ds)
	}
	// nothing listening
	dead := freePort(t)
	ds = detect(context.Background(), "", dead, 200*time.Millisecond)
	if ds[0].ok() {
		t.Fatalf("dead port ok: %+v", ds)
	}
	if _, ok := best(ds); ok {
		t.Fatal("best of nothing")
	}
}

func TestBestPreference(t *testing.T) {
	ds := []detection{
		{Port: 10003, Type: model.FeedBeast, Err: "nothing listening"},
		{Port: 30005, Type: model.FeedBeast},                      // quiet
		{Port: 30002, Type: model.FeedAVR, Format: model.FeedAVR}, // data
		{Port: 30003, Type: model.FeedSBS, Format: model.FeedSBS},
	}
	if b, _ := best(ds); b.Port != 30002 {
		t.Fatalf("best %+v", b)
	}
	ds[1].Format = model.FeedBeast
	if b, _ := best(ds); b.Port != 30005 {
		t.Fatalf("best %+v", b)
	}
	if b, _ := best(ds[3:]); b.Port != 30003 {
		t.Fatalf("sbs only %+v", b)
	}
}

func TestDetectCandidatesProbedInParallel(t *testing.T) {
	// with dialProbe replaced, every candidate is tried on the given host
	old := dialProbe
	defer func() { dialProbe = old }()
	var seen = make(chan string, 8)
	dialProbe = func(ctx context.Context, addr string) (net.Conn, error) {
		seen <- addr
		return nil, &net.OpError{Op: "dial", Err: errRefusedForTest{}}
	}
	ds := detect(context.Background(), "radarcape", 0, time.Second)
	close(seen)
	n := 0
	for a := range seen {
		if !strings.HasPrefix(a, "radarcape:") {
			t.Errorf("probed %s", a)
		}
		n++
	}
	if n != len(candidates) || len(ds) != len(candidates) || ds[0].Err != "nothing listening" {
		t.Fatalf("n=%d %+v", n, ds)
	}
}

type errRefusedForTest struct{}

func (errRefusedForTest) Error() string { return "connection refused" }
