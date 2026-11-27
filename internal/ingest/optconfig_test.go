package ingest

import (
	"testing"
	"time"

	"airview/internal/model"
)

func TestOptionsFromConfigIngestBlock(t *testing.T) {
	a := model.DefaultAggregator()
	a.Ingest.WindowMs, a.Ingest.FlowGraceSec, a.Ingest.Shards = 1500, 20, 100
	o := OptionsFromConfig(a)
	if o.Window != 1500*time.Millisecond || o.FlowGrace != 20*time.Second || o.Shards != 100 || o.LateAfter != 0 {
		t.Fatalf("%+v", o)
	}
	o.Handler = &nopHandler{}
	o, err := o.withDefaults()
	if err != nil {
		t.Fatal(err)
	}
	if o.Shards != 128 || o.LateAfter != 1500*time.Millisecond+time.Second+500*time.Millisecond {
		t.Fatalf("defaults %d %v", o.Shards, o.LateAfter)
	}
}
