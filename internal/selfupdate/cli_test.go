package selfupdate

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
)

func TestRunCommand(t *testing.T) {
	e := newReleaseEnv(t)
	e.publish("1.1.0", ChannelStable, fakeExe("v2"), time.Now())
	base := CommandOptions{Product: "airview-feeder", Version: "1.0.0", Name: "airview-feeder", Exe: e.exe, StateDir: e.state,
		Settings: Settings{ManifestURL: e.manifest, Keys: []string{e.pub.String()}, HealthWindowS: 10}}
	run := func(o CommandOptions, args ...string) (int, string, string) {
		var out, errb bytes.Buffer
		code := RunCommand(args, &out, &errb, o)
		return code, out.String(), errb.String()
	}
	if code, out, _ := run(base, "-check"); code != ExitAvailable || !strings.Contains(out, "1.1.0 is available") {
		t.Fatalf("check: %d %s", code, out)
	}
	if code, out, _ := run(base, "-check", "-json"); code != ExitAvailable || !strings.Contains(out, `"outcome": "available"`) {
		t.Fatalf("check json: %d %s", code, out)
	}
	if code, out, _ := run(base, "-status"); code != 0 || !strings.Contains(out, "1.1.0 available") {
		t.Fatalf("status: %d %s", code, out)
	}
	var st Status
	if code, out, _ := run(base, "-status", "-json"); code != 0 || json.Unmarshal([]byte(out), &st) != nil || st.Available != "1.1.0" {
		t.Fatalf("status json: %d %s", code, out)
	}
	// Not configured / bad options / no service.
	nc := base
	nc.Settings = Settings{}
	if code, _, _ := run(nc, "-check"); code != ExitNotConfigured {
		t.Errorf("not configured: %d", code)
	}
	if code, _, _ := run(base, "-channel", "nightly"); code != ExitUsage {
		t.Errorf("bad channel: %d", code)
	}
	if code, _, _ := run(base, "extra"); code != ExitUsage {
		t.Errorf("extra arg: %d", code)
	}
	if code, _, errs := run(base); code != ExitUsage || !strings.Contains(errs, "-service") {
		t.Errorf("no service: %d %s", code, errs)
	}
	// Tampered manifest: exit 1, program untouched.
	mp := e.relDir + "/manifest.json"
	orig := readFile(t, mp)
	os.WriteFile(mp, bytes.Replace(orig, []byte("1.1.0"), []byte("1.1.1"), 1), 0o644)
	if code, _, errs := run(base, "-check"); code != ExitError || !strings.Contains(errs, "signature") {
		t.Errorf("tampered: %d %s", code, errs)
	}
	os.WriteFile(mp, orig, 0o644)
	// Install through an injected restarter: unhealthy -> exit 4, healthy -> 0.
	sick := base
	sick.Restarter = &fakeRestarter{}
	sick.Probe = exeProbe{exe: e.exe, versions: map[string]string{"v1": "1.0.0", "v2": "1.1.0"}, sick: map[string]bool{"v2": true}}
	if code, _, errs := run(sick, "-health-window", "300ms"); code != ExitRolledBack || !strings.Contains(errs, "rolled back") {
		t.Fatalf("rollback: %d %s", code, errs)
	}
	if !bytes.Equal(readFile(t, e.exe), fakeExe("v1")) {
		t.Fatal("not restored")
	}
	good := base
	good.Restarter = &fakeRestarter{}
	good.Probe = exeProbe{exe: e.exe, versions: map[string]string{"v1": "1.0.0", "v2": "1.1.0"}}
	if code, out, errs := run(good, "-health-window", "300ms"); code != 0 || !strings.Contains(out, "updated 1.0.0 -> 1.1.0") {
		t.Fatalf("update: %d %s %s", code, out, errs)
	}
}
