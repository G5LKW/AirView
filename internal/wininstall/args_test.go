package wininstall

import (
	"strings"
	"testing"
)

func TestParseArgs(t *testing.T) {
	a, err := ParseArgs([]string{"/S", "-currentuser", "/NODESKTOP", "/firewall", "/PORT=8080", "/SERVER=feed.example.org", "/NOLOCATION", "/LOG=C:\\x.log", `/D=C:\Program`, `Files\AirView`})
	if err != nil {
		t.Fatal(err)
	}
	if !a.Silent || a.Scope != PerUser || !a.ScopeSet || a.Desktop == nil || *a.Desktop || a.Firewall == nil || !*a.Firewall || a.Port != 8080 {
		t.Fatalf("switches: %+v", a)
	}
	if a.Dir != `C:\Program Files\AirView` {
		t.Errorf("/D= %q", a.Dir)
	}
	if a.Params["SERVER"] != "feed.example.org" || a.Params["NOLOCATION"] != "" || len(a.Params) != 2 {
		t.Errorf("params %v", a.Params)
	}
	if a.Log != `C:\x.log` {
		t.Errorf("log %q", a.Log)
	}

	quoted, err := ParseArgs([]string{"/S", `/D=C:\Program Files\X`})
	if err != nil || quoted.Dir != `C:\Program Files\X` {
		t.Errorf("quoted /D=: %q %v", quoted.Dir, err)
	}
	u, _ := ParseArgs([]string{"/S", "/REMOVEDATA"})
	if !u.RemoveData {
		t.Error("/REMOVEDATA")
	}
	for _, bad := range [][]string{{"x"}, {"/PORT=0"}, {"/PORT=abc"}, {"/LOG="}, {"/bad-switch"}, {"/SERVER=a\nb"}} {
		if _, err := ParseArgs(bad); err == nil {
			t.Errorf("%v accepted", bad)
		}
	}
}

func TestApplyArgs(t *testing.T) {
	p := validProduct()
	o := DefaultOptions(p, PerMachine)
	if !o.Service || !o.Tray || o.Firewall || !o.Desktop || !o.StartMenu || o.Port != 8080 {
		t.Fatalf("defaults %+v", o)
	}
	a, _ := ParseArgs([]string{"/NOSERVICE", "/FIREWALL", "/PORT=81", "/SERVER=h", "/ENROL=tok", "/NOLOCATION"})
	if err := o.ApplyArgs(p, a); err != nil {
		t.Fatal(err)
	}
	if o.Service || !o.Firewall || o.Port != 81 || o.Params["SERVER"] != "h" || o.Params["ENROL"] != "tok" {
		t.Fatalf("applied %+v", o)
	}

	// Configuration switches without the trigger, unknown switches,
	// per-user with service/firewall.
	for _, args := range [][]string{{"/ENROL=x"}, {"/LAT=1"}} {
		o := DefaultOptions(p, PerMachine)
		a, _ := ParseArgs(args)
		if err := o.ApplyArgs(p, a); err == nil {
			t.Errorf("%v accepted", args)
		}
	}
	ou := DefaultOptions(p, PerUser)
	if ou.Service || ou.Firewall {
		t.Errorf("per-user defaults %+v", ou)
	}
	a, _ = ParseArgs([]string{"/CURRENTUSER", "/SERVICE"})
	if err := ou.ApplyArgs(p, a); err == nil {
		t.Error("per-user /SERVICE accepted")
	}
	noSvc := validProduct()
	noSvc.Service, noSvc.Tray, noSvc.Firewall = nil, nil, nil
	for _, s := range []string{"/SERVICE", "/TRAY", "/FIREWALL"} {
		o := DefaultOptions(noSvc, PerMachine)
		a, _ := ParseArgs([]string{s})
		if err := o.ApplyArgs(noSvc, a); err == nil {
			t.Errorf("%s accepted for a product without it", s)
		}
	}
}

func TestUsageMentionsProductSwitches(t *testing.T) {
	u := Usage(validProduct(), false)
	for _, w := range []string{"/SERVER=", "/NOLOCATION", "/NOSERVICE", "/NOTRAY", "/D=", "3010"} {
		if !strings.Contains(u, w) {
			t.Errorf("usage lacks %s:\n%s", w, u)
		}
	}
	if !strings.Contains(Usage(validProduct(), true), "/REMOVEDATA") {
		t.Error("uninstaller usage lacks /REMOVEDATA")
	}
}

func TestQuoteArg(t *testing.T) {
	cases := map[string]string{
		``:                  `""`,
		`plain`:             `plain`,
		`C:\Program Files\`: `"C:\Program Files\\"`,
		`a "b"`:             `"a \"b\""`,
		`x\"y z`:            `"x\\\"y z"`,
		`C:\a b\c.exe`:      `"C:\a b\c.exe"`,
	}
	for in, want := range cases {
		if got := quoteArg(in); got != want {
			t.Errorf("quoteArg(%q) = %s, want %s", in, got, want)
		}
	}
	if got := ComposeArgs([]string{"service", "install", "-data", `C:\Program Data\X`}); got != `service install -data "C:\Program Data\X"` {
		t.Errorf("ComposeArgs: %s", got)
	}
}

func TestRedactArgs(t *testing.T) {
	got := redactArgs([]string{"-server", "h", "-enrol", "SECRET", "-password", "pw"})
	if strings.Contains(strings.Join(got, " "), "SECRET") || strings.Contains(strings.Join(got, " "), "pw") || got[1] != "h" {
		t.Errorf("redact %v", got)
	}
}

// DLV-06: the as-invoker stub elevates only for per-machine work, and a
// relaunched copy that is still not elevated fails instead of looping.
func TestNeedElevation(t *testing.T) {
	plain, _ := ParseArgs([]string{"/S"})
	relaunched, _ := ParseArgs(ElevatedArgs([]string{"/S"}))
	if !relaunched.Elevated || plain.Elevated {
		t.Fatalf("/ELEVATED not parsed: %+v %+v", plain, relaunched)
	}
	cases := []struct {
		scope    Scope
		elevated bool
		a        *Args
		want     Elevation
	}{
		{PerUser, false, plain, ElevationNone},
		{PerUser, false, relaunched, ElevationNone},
		{PerMachine, true, plain, ElevationNone},
		{PerMachine, true, relaunched, ElevationNone},
		{PerMachine, false, plain, ElevationRelaunch},
		{PerMachine, false, nil, ElevationRelaunch},
		{PerMachine, false, relaunched, ElevationFail},
	}
	for i, c := range cases {
		if got := NeedElevation(c.scope, c.elevated, c.a); got != c.want {
			t.Errorf("case %d: %v, want %v", i, got, c.want)
		}
	}
}

func TestElevatedArgsKeepsDirLast(t *testing.T) {
	in := []string{"/S", "/ELEVATED", "/SERVER=feed.example", `/D=C:\Program`, `Files\AirView`}
	got := ElevatedArgs(in)
	if got[0] != "/ELEVATED" || len(got) != len(in) {
		t.Fatalf("%q", got)
	}
	a, err := ParseArgs(got)
	if err != nil {
		t.Fatal(err)
	}
	if !a.Silent || !a.Elevated || a.Dir != `C:\Program Files\AirView` || a.Params["SERVER"] != "feed.example" {
		t.Fatalf("%+v", a)
	}
	if _, ok := a.Params["ELEVATED"]; ok {
		t.Fatal("/ELEVATED leaked into the product parameters")
	}
}

func TestRedactCommandLine(t *testing.T) {
	got := strings.Join(RedactCommandLine([]string{"setup.exe", "/S", "/SERVER=feed.example.org", "/ENROL=abc123", "/enrol_token=x", "-enrol", "tok", "/PASSWORD=p", "/LAT=51"}), " ")
	want := "setup.exe /S /SERVER=feed.example.org /ENROL=*** /enrol_token=*** -enrol *** /PASSWORD=*** /LAT=51"
	if got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
}
