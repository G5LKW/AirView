package selfupdate

import (
	"fmt"
	"runtime"
	"strconv"
	"strings"
)

// Version is a parsed semantic version (MAJOR.MINOR.PATCH[-PRERELEASE]
// [+BUILD]); a leading "v" is accepted. Build metadata is ignored for
// ordering, as in Semantic Versioning 2.0.0.
type Version struct {
	Major, Minor, Patch int
	Pre                 []string
	raw                 string
}

// ParseVersion parses s. Missing minor/patch parts are zero ("1.2" = 1.2.0).
func ParseVersion(s string) (Version, error) {
	v := Version{raw: s}
	t := strings.TrimPrefix(strings.TrimSpace(s), "v")
	if i := strings.IndexByte(t, '+'); i >= 0 {
		t = t[:i]
	}
	if i := strings.IndexByte(t, '-'); i >= 0 {
		pre := t[i+1:]
		t = t[:i]
		if pre == "" {
			return v, fmt.Errorf("version %q: empty pre-release", s)
		}
		v.Pre = strings.Split(pre, ".")
		for _, p := range v.Pre {
			if p == "" || strings.Trim(p, "0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ-") != "" {
				return v, fmt.Errorf("version %q: bad pre-release", s)
			}
		}
	}
	parts := strings.Split(t, ".")
	if len(parts) == 0 || len(parts) > 3 || parts[0] == "" {
		return v, fmt.Errorf("version %q: want MAJOR.MINOR.PATCH", s)
	}
	nums := [3]int{}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 || (len(p) > 1 && p[0] == '0') {
			return v, fmt.Errorf("version %q: bad number %q", s, p)
		}
		nums[i] = n
	}
	v.Major, v.Minor, v.Patch = nums[0], nums[1], nums[2]
	return v, nil
}

// String returns the text the version was parsed from.
func (v Version) String() string { return v.raw }

// IsPrerelease reports whether v has a pre-release part (1.2.0-beta.1).
func (v Version) IsPrerelease() bool { return len(v.Pre) > 0 }

// Compare returns -1, 0 or +1.
func (v Version) Compare(w Version) int {
	for _, d := range [3]int{v.Major - w.Major, v.Minor - w.Minor, v.Patch - w.Patch} {
		if d != 0 {
			return sign(d)
		}
	}
	switch {
	case len(v.Pre) == 0 && len(w.Pre) == 0:
		return 0
	case len(v.Pre) == 0:
		return 1
	case len(w.Pre) == 0:
		return -1
	}
	for i := 0; i < len(v.Pre) && i < len(w.Pre); i++ {
		a, b := v.Pre[i], w.Pre[i]
		an, aerr := strconv.Atoi(a)
		bn, berr := strconv.Atoi(b)
		switch {
		case aerr == nil && berr == nil:
			if an != bn {
				return sign(an - bn)
			}
		case aerr == nil:
			return -1
		case berr == nil:
			return 1
		default:
			if c := strings.Compare(a, b); c != 0 {
				return c
			}
		}
	}
	return sign(len(v.Pre) - len(w.Pre))
}

func sign(d int) int {
	switch {
	case d < 0:
		return -1
	case d > 0:
		return 1
	}
	return 0
}

// CompareVersions compares two version strings; unparsable versions sort
// before every valid one (a development build "dev" is older than any
// release, so it can always be updated).
func CompareVersions(a, b string) int {
	va, ea := ParseVersion(a)
	vb, eb := ParseVersion(b)
	switch {
	case ea != nil && eb != nil:
		return strings.Compare(a, b)
	case ea != nil:
		return -1
	case eb != nil:
		return 1
	}
	return va.Compare(vb)
}

// CurrentTarget returns the release target of this build: "<GOOS>-<GOARCH>",
// with 32-bit ARM reported as "armv7" (the only ARM level AirView builds).
func CurrentTarget() string {
	return targetFor(runtime.GOOS, runtime.GOARCH)
}

func targetFor(goos, goarch string) string {
	if goarch == "arm" {
		goarch = "armv7"
	}
	return goos + "-" + goarch
}
