package server

import "testing"

func TestTileCacheMaxMB(t *testing.T) {
	cases := []struct {
		profile int
		env     string
		want    int64
	}{
		{0, "", tileDefaultMaxMB},
		{128, "", 128},  // low-wear profile
		{512, "", 512},  // standard / server
		{128, "64", 64}, // the variable wins
		{128, "8", 128}, // below 16: ignored
		{128, "x", 128},
	}
	for _, c := range cases {
		if got := tileCacheMaxMB(c.profile, c.env); got != c.want {
			t.Errorf("tileCacheMaxMB(%d, %q) = %d, want %d", c.profile, c.env, got, c.want)
		}
	}
}
