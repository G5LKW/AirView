package federation

import (
	"reflect"
	"sort"
	"strings"
	"testing"

	"airview/internal/model"
)

// jsonKeys lists the JSON keys of t and its nested structs as dotted paths.
func jsonKeys(t reflect.Type, prefix string, out *[]string) {
	for t.Kind() == reflect.Slice || t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct {
		return
	}
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		name := strings.Split(f.Tag.Get("json"), ",")[0]
		if name == "" || name == "-" {
			continue
		}
		*out = append(*out, prefix+name)
		jsonKeys(f.Type, prefix+name+".", out)
	}
}

// TestModelBlockInStep: model.AggFederation (the config file block) and
// Config have exactly the same JSON keys, and a full block survives the
// conversion.
func TestModelBlockInStep(t *testing.T) {
	var a, b []string
	jsonKeys(reflect.TypeOf(model.AggFederation{}), "", &a)
	jsonKeys(reflect.TypeOf(Config{}), "", &b)
	sort.Strings(a)
	sort.Strings(b)
	if !reflect.DeepEqual(a, b) {
		t.Fatalf("keys differ:\nmodel  %v\nconfig %v", a, b)
	}
	m := model.AggFederation{Enabled: true, Name: "n", Listen: ":30170", MaxHops: 4, MaxInflateBps: 123456,
		Peers: []model.AggFedPeer{{Name: "p", ID: "abcd", Addr: "h:1", Send: model.AggFedPolicy{Aircraft: "nomlat", DelayS: 30,
			Boxes: []model.AggFedBox{{South: 1, West: 2, North: 3, East: 4}}}, Receive: model.AggFedReceive{MLAT: "none"}}},
		Feeds: []model.AggFedFeed{{Name: "f", Format: "beast", Mode: "listen", Addr: "127.0.0.1:1", Allow: []string{"10.0.0.0/8"}}}}
	c := FromModel(m)
	if !c.Enabled || c.MaxHops != 4 || c.MaxInflateBps != 123456 || len(c.Peers) != 1 || c.Peers[0].Send.DelayS != 30 ||
		c.Peers[0].Send.Boxes[0].East != 4 || c.Peers[0].Receive.MLAT != "none" || c.Feeds[0].Allow[0] != "10.0.0.0/8" {
		t.Fatalf("%+v", c)
	}
}
