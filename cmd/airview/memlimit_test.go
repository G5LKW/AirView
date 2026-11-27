package main

import "testing"

func TestMemoryLimitFor(t *testing.T) {
	for _, c := range []struct {
		goos, goarch, env string
		want              int64
	}{
		{"linux", "arm64", "", piMemoryLimit},
		{"linux", "arm", "", piMemoryLimit},
		{"linux", "arm64", "512MiB", 0},
		{"linux", "amd64", "", 0},
		{"windows", "arm64", "", 0},
		{"darwin", "arm64", "", 0},
	} {
		if got := memoryLimitFor(c.goos, c.goarch, c.env); got != c.want {
			t.Errorf("%s/%s GOMEMLIMIT=%q: %d, want %d", c.goos, c.goarch, c.env, got, c.want)
		}
	}
}

func TestCgroupSoftLimit(t *testing.T) {
	for in, want := range map[string]int64{
		"max\n":          0,
		"":               0,
		"1073741824\n":   1073741824 / 100 * 85,
		"104857600":      0, // 100 MiB: too small
		"not-a-number\n": 0,
	} {
		if got := cgroupSoftLimit(in); got != want {
			t.Errorf("%q: %d, want %d", in, got, want)
		}
	}
}
