package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"airview/internal/model"
)

// Receiver auto-detection (DESIGN §12.1 "Inputs", §12.2 step 2). The feeder
// never reconfigures anything: it only connects to well-known output ports
// and looks at the first bytes.
//
// Candidates, in order of preference:
//
//	10003  Radarcape / Beast binary (GPS timestamps when the receiver has them)
//	30005  readsb / dump1090 Beast output (12 MHz timestamps, sdr12 by default)
//	30002  readsb / dump1090 AVR (raw) output (timestamps only in '@' form)
//	30003  readsb / dump1090 SBS (BaseStation) output: map only, no MLAT
//
// Without a host the candidates are probed on 127.0.0.1; with a host
// (setup --input host or host:port, or input.host in feeder.json with type
// auto) on that host.

// candidate is one well-known receiver output.
type candidate struct {
	Port  int
	Type  string // model.FeedBeast / FeedAVR / FeedSBS
	Label string
}

var candidates = []candidate{
	{10003, model.FeedBeast, "Radarcape (Beast 10003)"},
	{30005, model.FeedBeast, "readsb/dump1090 (Beast 30005)"},
	{30002, model.FeedAVR, "readsb/dump1090 (AVR 30002)"},
	{30003, model.FeedSBS, "readsb/dump1090 (SBS 30003)"},
}

// detection is the result of probing one address.
type detection struct {
	Host   string `json:"host"`
	Port   int    `json:"port"`
	Type   string `json:"type"`   // feed type to use
	Label  string `json:"label"`  // human readable source
	Format string `json:"format"` // format seen in the data ("" = no data within the probe time)
	Err    string `json:"error,omitempty"`
}

func (d detection) ok() bool { return d.Err == "" }

// Addr returns host:port.
func (d detection) Addr() string { return net.JoinHostPort(d.Host, strconv.Itoa(d.Port)) }

// dialer is replaced in tests.
var dialProbe = func(ctx context.Context, addr string) (net.Conn, error) {
	var d net.Dialer
	return d.DialContext(ctx, "tcp", addr)
}

// sniff classifies the first bytes of a stream: "beast", "avr", "sbs" or "".
func sniff(b []byte) string {
	b = bytes.TrimLeft(b, "\r\n ")
	if len(b) == 0 {
		return ""
	}
	// Beast: escape byte then a frame type; a stream may start mid-frame,
	// so look for the pattern anywhere in the first bytes.
	for i := 0; i+1 < len(b); i++ {
		if b[i] == 0x1a && b[i+1] >= '1' && b[i+1] <= '5' && (i == 0 || b[i-1] != 0x1a) {
			if i == 0 || bytes.Count(b, []byte{0x1a}) >= 2 {
				return model.FeedBeast
			}
		}
	}
	switch b[0] {
	case '*', '@', '%', ':', '<':
		if bytes.IndexByte(b, ';') > 0 {
			return model.FeedAVR
		}
	}
	for _, p := range []string{"MSG,", "AIR,", "ID,", "STA,", "SEL,", "CLK,"} {
		if bytes.HasPrefix(b, []byte(p)) || bytes.Contains(b, []byte("\n"+p)) {
			return model.FeedSBS
		}
	}
	return ""
}

// probe connects to addr and reads for up to wait. connected is false when
// nothing listens.
func probe(ctx context.Context, host string, port int, wait time.Duration) (format string, err error) {
	addr := net.JoinHostPort(host, strconv.Itoa(port))
	dctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	c, err := dialProbe(dctx, addr)
	if err != nil {
		return "", err
	}
	defer c.Close()
	_ = c.SetReadDeadline(time.Now().Add(wait))
	buf := make([]byte, 0, 4096)
	tmp := make([]byte, 2048)
	for len(buf) < 256 {
		n, err := c.Read(tmp)
		buf = append(buf, tmp[:n]...)
		if f := sniff(buf); f != "" && len(buf) >= 64 {
			return f, nil
		}
		if err != nil {
			break
		}
	}
	return sniff(buf), nil
}

// detect probes the candidates on host ("" = 127.0.0.1) in parallel and
// returns all results in preference order. With port > 0 only that port is
// probed (its type is taken from the data, else from the well-known port,
// else Beast).
func detect(ctx context.Context, host string, port int, wait time.Duration) []detection {
	if host == "" {
		host = "127.0.0.1"
	}
	cands := candidates
	if port > 0 {
		cands = nil
		for _, c := range candidates {
			if c.Port == port {
				cands = []candidate{c}
			}
		}
		if cands == nil {
			cands = []candidate{{port, model.FeedBeast, fmt.Sprintf("receiver on port %d", port)}}
		}
	}
	out := make([]detection, len(cands))
	var wg sync.WaitGroup
	for i, c := range cands {
		wg.Add(1)
		go func() {
			defer wg.Done()
			d := detection{Host: host, Port: c.Port, Type: c.Type, Label: c.Label}
			f, err := probe(ctx, host, c.Port, wait)
			if err != nil {
				d.Err = describeDialErr(err)
			} else if f != "" {
				d.Format = f
				if f != c.Type {
					d.Type = f
					d.Label = fmt.Sprintf("%s output on port %d", f, c.Port)
				}
			}
			out[i] = d
		}()
	}
	wg.Wait()
	return out
}

// best returns the preferred usable detection: one that delivered data in a
// timed format first (Beast, then AVR), then any listening port, in
// candidate order.
func best(ds []detection) (detection, bool) {
	rank := func(d detection) int {
		switch {
		case !d.ok():
			return 99
		case d.Format == model.FeedBeast:
			return 0
		case d.Format == model.FeedAVR:
			return 1
		case d.Format == "" && d.Type != model.FeedSBS:
			return 2 // listening, quiet (no traffic yet)
		case d.Format == model.FeedSBS:
			return 3
		}
		return 4
	}
	bi := -1
	for i, d := range ds {
		if rank(d) < 99 && (bi < 0 || rank(d) < rank(ds[bi])) {
			bi = i
		}
	}
	if bi < 0 {
		return detection{}, false
	}
	return ds[bi], true
}

func describeDialErr(err error) string {
	var op *net.OpError
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "no answer"
	case errors.As(err, &op) && op.Op == "dial" && strings.Contains(err.Error(), "refused"):
		return "nothing listening"
	case errors.As(err, &op) && op.Timeout():
		return "no answer"
	}
	return err.Error()
}

// detectedFeed turns a detection into the concrete feed of an auto
// receiver (ID and name from the configuration).
func detectedFeed(base model.Feed, d detection) model.Feed {
	f := base
	f.Type, f.Host, f.Port, f.Enabled = d.Type, d.Host, d.Port, true
	return f
}
