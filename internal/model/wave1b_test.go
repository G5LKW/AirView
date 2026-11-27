package model

import (
	"encoding/json"
	"testing"
)

func TestStorageAndIngestBlocks(t *testing.T) {
	a := DefaultAggregator()
	if n := a.Normalize(); len(n) != 0 {
		t.Fatalf("defaults produce notes: %v", n)
	}
	// A block added in code before the fields existed gets the defaults.
	a.Storage, a.Ingest = AggStorage{}, AggIngest{}
	if n := a.Normalize(); len(n) != 0 || a.Storage != DefaultAggStorage() || a.Ingest != DefaultAggIngest() {
		t.Fatalf("zero blocks: %v %+v %+v", n, a.Storage, a.Ingest)
	}
	// Missing keys decode to the defaults.
	var b Aggregator
	if err := json.Unmarshal([]byte(`{"storage":{"histDays":0},"ingest":{"windowMs":2000}}`), &b); err != nil {
		t.Fatal(err)
	}
	if b.Storage.HistDays != 0 || b.Storage.HeatIdleDays != 395 || b.Ingest.WindowMs != 2000 || b.Ingest.FlushAfterMs != 400 {
		t.Fatalf("decoded %+v %+v", b.Storage, b.Ingest)
	}
	// Out-of-range values are corrected with a note.
	b.Storage.FreePct, b.Ingest.FlushAfterMs, b.Ingest.LateAfterMs = 99, 3000, 10
	b.ListenAddr = " [::1] "
	n := b.Normalize()
	if len(n) != 3 || b.Storage.FreePct != 10 || b.Ingest.FlushAfterMs != 400 || b.Ingest.LateAfterMs != 0 || b.ListenAddr != "::1" {
		t.Fatalf("notes %v storage %+v ingest %+v listen %q", n, b.Storage, b.Ingest, b.ListenAddr)
	}
	b.ListenAddr = "eth0"
	if n := b.Normalize(); len(n) != 1 || b.ListenAddr != "" {
		t.Fatalf("bad listenAddr: %v %q", n, b.ListenAddr)
	}
}

func TestFeederResultsBindAddr(t *testing.T) {
	f := DefaultFeederConfig()
	if f.Results.BindAddr != "127.0.0.1" {
		t.Fatalf("default %q", f.Results.BindAddr)
	}
	f.Results.BindAddr = ""
	if n := f.Normalize(); len(n) != 0 || f.Results.BindAddr != "127.0.0.1" {
		t.Fatalf("empty: %v %q", n, f.Results.BindAddr)
	}
	f.Results.BindAddr = "0.0.0.0"
	if n := f.Normalize(); len(n) != 0 || f.Results.BindAddr != "0.0.0.0" {
		t.Fatalf("any: %v %q", n, f.Results.BindAddr)
	}
	f.Results.BindAddr = "lan-host"
	if n := f.Normalize(); len(n) != 1 || f.Results.BindAddr != "127.0.0.1" {
		t.Fatalf("invalid: %v %q", n, f.Results.BindAddr)
	}
}
