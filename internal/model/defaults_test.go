package model

import "testing"

func TestDefaultConfigIsGeneric(t *testing.T) {
	c := DefaultConfig()
	if len(c.Antennas) != 1 || c.Antennas[0].Type != AntVertical || c.Antennas[0].Channel != 0 {
		t.Fatalf("antennas: %+v", c.Antennas)
	}
	if len(c.Feeds) != 1 {
		t.Fatalf("feeds: %+v", c.Feeds)
	}
	f := c.Feeds[0]
	if f.ID != "beast" || f.Type != FeedBeast || f.Host != "localhost" || f.Port != 30005 || !f.Enabled || f.AntennaID != "" {
		t.Fatalf("feed: %+v", f)
	}
	if c.Receiver.Model != "" {
		t.Fatalf("receiver model %q: the default station is not a specific product", c.Receiver.Model)
	}
}

func TestIsFactoryDefaultFeed(t *testing.T) {
	cases := []struct {
		f    Feed
		want bool
	}{
		{DefaultConfig().Feeds[0], true},
		{Feed{ID: "radarcape", Type: FeedBeast, Host: "radarcape", Port: 10003}, true}, // earlier releases' default
		{Feed{ID: "beast", Type: FeedBeast, Host: "localhost", Port: 30006}, false},
		{Feed{ID: "beast", Type: FeedBeast, Host: "pi.local", Port: 30005}, false},
		{Feed{ID: "beast", Type: FeedAVR, Host: "localhost", Port: 30005}, false},
		{Feed{ID: "radarcape", Type: FeedBeast, Host: "192.0.2.7", Port: 10003}, false},
		{Feed{ID: "rc", Type: FeedBeast, Host: "radarcape", Port: 10003}, false},
	}
	for _, c := range cases {
		if got := IsFactoryDefaultFeed(c.f); got != c.want {
			t.Errorf("IsFactoryDefaultFeed(%+v) = %v, want %v", c.f, got, c.want)
		}
	}
}
