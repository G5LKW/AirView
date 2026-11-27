package main

import (
	"bytes"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"

	"airview/internal/model"
	"airview/internal/uplink"
	"airview/internal/uplink/avftest"
)

func TestWindowsDefaultArgs(t *testing.T) {
	no := func() bool { return false }
	yes := func() bool { return true }
	cases := []struct {
		args []string
		svc  func() bool
		want string
	}{
		{[]string{`C:\x\airview-feeder-tray.exe`}, no, "tray"},
		{[]string{`C:\x\AirView-Feeder-Tray.exe`, "-name", "X"}, no, "tray -name X"},
		{[]string{`C:\x\airview-feeder-tray.exe`, "status"}, no, "status"},
		{[]string{`C:\x\airview-feeder.exe`}, no, ""},
		{[]string{`C:\x\airview-feeder.exe`, "-config", "f"}, no, "-config f"},
		{[]string{`C:\x\airview-feeder.exe`}, yes, "service run"},
		{[]string{`C:\x\airview-feeder.exe`, "-data", "d"}, yes, "service run -data d"},
		{[]string{`C:\x\airview-feeder.exe`, "service", "run"}, yes, "service run"},
	}
	for _, c := range cases {
		got := strings.Join(windowsDefaultArgs(c.args, c.svc)[1:], " ")
		if got != c.want {
			t.Errorf("%q: got %q, want %q", c.args, got, c.want)
		}
	}
}

func TestServiceSIDMatchesLookup(t *testing.T) {
	// TrustedInstaller is a virtual service account on every Windows.
	sid, _, _, err := windows.LookupSID("", `NT SERVICE\TrustedInstaller`)
	if err != nil {
		t.Skipf("lookup: %v", err)
	}
	if got := serviceSID("TrustedInstaller"); got != sid.String() {
		t.Fatalf("serviceSID = %s, LookupAccountName = %s", got, sid)
	}
	if serviceSID("trustedinstaller") != serviceSID("TRUSTEDINSTALLER") {
		t.Fatal("service SIDs must be case-insensitive")
	}
}

func TestSDDLParse(t *testing.T) {
	for _, s := range []string{dataDirSDDL(""), dataDirSDDL(serviceSID("AirViewFeeder")), logDirExtraSDDL, serviceDACL(true), serviceDACL(false)} {
		sd, err := windows.SecurityDescriptorFromString(s)
		if err != nil {
			t.Fatalf("%s: %v", s, err)
		}
		if _, _, err := sd.DACL(); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	if !strings.Contains(serviceDACL(true), "RPWPDT") || strings.Contains(serviceDACL(false), "(A;;CCLCSWRPWPDTLOCRRC;;;IU)") {
		t.Fatal("user control rights wrong")
	}
}

func TestACLSafeDir(t *testing.T) {
	bad := []string{`C:\`, os.Getenv("ProgramData"), os.Getenv("SystemRoot"), os.Getenv("USERPROFILE"), filepath.Dir(os.Getenv("USERPROFILE"))}
	for _, d := range bad {
		if d == "" {
			continue
		}
		if err := aclSafeDir(d); err == nil {
			t.Errorf("aclSafeDir(%q) accepted", d)
		}
	}
	tmp := t.TempDir()
	if err := aclSafeDir(filepath.Join(tmp, "new")); err != nil {
		t.Errorf("missing dir refused: %v", err)
	}
	if err := aclSafeDir(tmp); err != nil {
		t.Errorf("empty dir refused: %v", err)
	}
	foreign := filepath.Join(tmp, "docs")
	os.MkdirAll(foreign, 0o755)
	os.WriteFile(filepath.Join(foreign, "letter.txt"), []byte("x"), 0o644)
	if err := aclSafeDir(foreign); err == nil {
		t.Error("foreign non-empty dir accepted")
	}
	os.WriteFile(filepath.Join(foreign, "feeder.json"), []byte("{}"), 0o644)
	if err := aclSafeDir(foreign); err != nil {
		t.Errorf("dir with feeder.json refused: %v", err)
	}
	named := filepath.Join(tmp, "My AirView Feeder")
	os.MkdirAll(named, 0o755)
	os.WriteFile(filepath.Join(named, "other.txt"), []byte("x"), 0o644)
	if err := aclSafeDir(named); err != nil {
		t.Errorf("AirView-named dir refused: %v", err)
	}
	if !isParentDir(`C:\a`, `C:\a\b`) || isParentDir(`C:\a`, `C:\a`) || isParentDir(`C:\a\b`, `C:\a`) || isParentDir(`C:\a`, `C:\ab`) {
		t.Error("isParentDir")
	}
}

func TestSecureDirForUser(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "airview-data")
	os.MkdirAll(dir, 0o755)
	if err := secureDirForUser(dir); err != nil {
		t.Fatal(err)
	}
	// Still ours: write, read, delete.
	p := filepath.Join(dir, "identity.key")
	if err := os.WriteFile(p, []byte("k"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := os.ReadFile(p); err != nil {
		t.Fatal(err)
	}
	sd, err := windows.GetNamedSecurityInfo(dir, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	s := sd.String()
	if strings.Contains(s, ";;;BU)") || strings.Contains(s, ";;;WD)") || strings.Contains(s, ";;;AU)") {
		t.Fatalf("users can still read the data directory: %s", s)
	}
	ctl, _, _ := sd.Control()
	if ctl&windows.SE_DACL_PROTECTED == 0 {
		t.Fatalf("DACL not protected: %s", s)
	}
}

func TestWinLock(t *testing.T) {
	dir := t.TempDir()
	rel, err := lockDataDirWin(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lockDataDirWin(dir); !errors.Is(err, errLocked) {
		t.Fatalf("second lock: %v, want errLocked", err)
	}
	if !dataDirLockedWin(dir) {
		t.Fatal("dataDirLockedWin = false while locked")
	}
	rel()
	if dataDirLockedWin(dir) {
		t.Fatal("still locked after release")
	}
	rel2, err := lockDataDirWin(dir)
	if err != nil {
		t.Fatal(err)
	}
	rel2()
}

func TestRotatingLog(t *testing.T) {
	dir := t.TempDir()
	l, err := openRotatingLog(dir, 4096, 2)
	if err != nil {
		t.Fatal(err)
	}
	line := strings.Repeat("x", 99) + "\n"
	for i := 0; i < 200; i++ { // 20 kB: several rotations
		if _, err := l.Write([]byte(line)); err != nil {
			t.Fatal(err)
		}
	}
	l.Close()
	for _, n := range []string{"feeder.log", "feeder.1.log", "feeder.2.log"} {
		st, err := os.Stat(filepath.Join(dir, n))
		if err != nil {
			t.Fatalf("%s: %v", n, err)
		}
		if st.Size() > 4096 {
			t.Fatalf("%s is %d bytes, max 4096", n, st.Size())
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "feeder.3.log")); err == nil {
		t.Fatal("kept more than 2 old files")
	}
	// Reopening appends.
	l, _ = openRotatingLog(dir, 4096, 2)
	before, _ := os.Stat(filepath.Join(dir, "feeder.log"))
	l.Write([]byte("more\n"))
	l.Close()
	after, _ := os.Stat(filepath.Join(dir, "feeder.log"))
	if after.Size() <= before.Size() && after.Size() != 5 {
		t.Fatal("reopen did not append")
	}
}

func TestLogSettingsResolve(t *testing.T) {
	var l logSettings
	l.resolve(`D:\d`, func(string) string { return "" })
	if l.Dir != `D:\d\logs` || l.MaxMB != 10 || l.Keep != 5 {
		t.Fatalf("defaults %+v", l)
	}
	env := map[string]string{envLogDir: `E:\logs`, envLogMaxMB: "3", envLogKeep: "500"}
	l = logSettings{}
	l.resolve(`D:\d`, func(k string) string { return env[k] })
	if l.Dir != `E:\logs` || l.MaxMB != 3 || l.Keep != 100 {
		t.Fatalf("env %+v", l)
	}
	l = logSettings{Dir: `F:\x`, MaxMB: 7, Keep: 2}
	l.resolve(`D:\d`, func(k string) string { return env[k] })
	if l.Dir != `F:\x` || l.MaxMB != 7 || l.Keep != 2 {
		t.Fatalf("flags must win: %+v", l)
	}
	// The log variables are not feeder.json settings.
	lc, _ := loadConfig(loadOptions{File: filepath.Join(t.TempDir(), "none.json"), Environ: []string{envLogDir + `=E:\x`, envLogKeep + "=3"}})
	for _, is := range lc.Issues {
		if strings.Contains(is.Source, "LOG") {
			t.Fatalf("log variable reported as a setting: %v", is)
		}
	}
}

func TestServiceConfigAndPaths(t *testing.T) {
	o := installOptions{name: "AirViewFeederTest", display: "x", account: "virtual", start: "delayed",
		exe: `C:\Program Files\AirView Feeder\airview-feeder.exe`,
		c:   common{config: `C:\Feeder Data\feeder.json`, data: `C:\Feeder Data`}, logs: logSettings{Dir: `C:\Feeder Logs`, MaxMB: 4}}
	cfg, sid, err := o.serviceConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ServiceStartName != `NT SERVICE\AirViewFeederTest` || sid != serviceSID("AirViewFeederTest") || !cfg.DelayedAutoStart {
		t.Fatalf("config %+v sid %s", cfg, sid)
	}
	args := splitCommandLine(cfg.BinaryPathName)
	if args[0] != o.exe || strings.Join(args[1:4], " ") != "service run -name" || args[4] != "AirViewFeederTest" {
		t.Fatalf("command line %q -> %q", cfg.BinaryPathName, args)
	}
	c, logs := serviceRunPaths(cfg.BinaryPathName)
	if c.config != o.c.config || c.data != o.c.data || logs.Dir != o.logs.Dir {
		t.Fatalf("paths %+v %+v", c, logs)
	}
	// Defaults when the command line names nothing.
	c, logs = serviceRunPaths(`"C:\x\airview-feeder.exe" service run -name A`)
	if c.data != defaultDataDir() || logs.Dir != filepath.Join(defaultDataDir(), "logs") {
		t.Fatalf("default paths %+v %+v", c, logs)
	}
	for _, k := range []string{"localsystem", "LocalService", "networkservice"} {
		if _, _, err := accountFor(k, "x"); err != nil {
			t.Error(err)
		}
	}
	if _, _, err := accountFor("bob", "x"); err == nil {
		t.Error("unknown account accepted")
	}
	if _, _, err := startTypeFor("sometimes"); err == nil {
		t.Error("unknown start type accepted")
	}
}

func TestServiceCommandsWithoutService(t *testing.T) {
	name := "AirViewFeederNoSuchService" + strconv.Itoa(os.Getpid())
	si := queryService(name)
	if si.Installed || si.Error != "" {
		t.Fatalf("queryService(missing) = %+v", si)
	}
	if _, err := controlService(name, "start", time.Second); !errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST) {
		t.Fatalf("controlService: %v", err)
	}
	var out, errb bytes.Buffer
	if code := runMain([]string{"service", "status", "-name", name}, &out, &errb); code != 3 || !strings.Contains(out.String(), "not installed") {
		t.Fatalf("status: %d %q %q", code, out.String(), errb.String())
	}
	out.Reset()
	if code := runMain([]string{"service", "sid", "-name", "TrustedInstaller"}, &out, &errb); code != 0 || !strings.HasPrefix(out.String(), "S-1-5-80-956008885-") {
		t.Fatalf("sid: %d %q", code, out.String())
	}
	if code := runMain([]string{"service", "run", "-name", name}, &out, &errb); code != 2 {
		t.Fatalf("service run outside the SCM: %d", code)
	}
	if code := runMain([]string{"service", "bogus"}, &out, &errb); code != 2 {
		t.Fatalf("unknown subcommand: %d", code)
	}
	if code := runMain([]string{"tray", "-signal", "refresh", "-name", name}, &out, &errb); code != 0 {
		t.Fatalf("tray -signal with no tray: %d %s", code, errb.String())
	}
}

// fakeRunner counts the service's calls into the feeder.
type fakeRunner struct {
	stops, reloads *atomic.Int32
	stopDelay      time.Duration
}

func (f fakeRunner) stop() {
	time.Sleep(f.stopDelay)
	f.stops.Add(1)
}
func (f fakeRunner) reload()           { f.reloads.Add(1) }
func (f fakeRunner) statusURL() string { return "http://127.0.0.1:1/" }

// TestServiceHandler drives Execute the way the SCM does.
func TestServiceHandler(t *testing.T) {
	var starts, stops, reloads atomic.Int32
	var urls []string
	var mu sync.Mutex
	h := &feederService{
		start: func() (svcRunner, error) {
			starts.Add(1)
			return fakeRunner{&stops, &reloads, 60 * time.Millisecond}, nil
		},
		logf:  t.Logf,
		event: func(kind, msg string) { t.Logf("event %s: %s", kind, msg) },
		onURL: func(u string) { mu.Lock(); urls = append(urls, u); mu.Unlock() },
		tick:  10 * time.Millisecond,
	}
	r := make(chan svc.ChangeRequest)
	s := make(chan svc.Status, 100)
	type result struct {
		specific bool
		code     uint32
	}
	done := make(chan result, 1)
	go func() {
		sp, c := h.Execute(nil, r, s)
		done <- result{sp, c}
	}()
	expect := func(want svc.State) svc.Status {
		t.Helper()
		deadline := time.After(5 * time.Second)
		for {
			select {
			case st := <-s:
				if st.State == want {
					return st
				}
			case <-deadline:
				t.Fatalf("no %s status", stateName(want))
			}
		}
	}
	st := expect(svc.Running)
	if st.Accepts&svc.AcceptPauseAndContinue == 0 || st.Accepts&svc.AcceptStop == 0 || st.Accepts&svc.AcceptParamChange == 0 {
		t.Fatalf("accepts %v", st.Accepts)
	}
	r <- svc.ChangeRequest{Cmd: svc.Interrogate, CurrentStatus: st}
	expect(svc.Running)

	r <- svc.ChangeRequest{Cmd: svc.Pause}
	pp := expect(svc.PausePending)
	if pp.WaitHint == 0 {
		t.Fatal("no wait hint")
	}
	expect(svc.Paused)
	if stops.Load() != 1 {
		t.Fatalf("pause: %d stops", stops.Load())
	}
	r <- svc.ChangeRequest{Cmd: svc.Pause} // again: stays paused, no second stop
	expect(svc.Paused)
	r <- svc.ChangeRequest{Cmd: svc.Continue}
	expect(svc.Running)
	if starts.Load() != 2 {
		t.Fatalf("continue: %d starts", starts.Load())
	}
	r <- svc.ChangeRequest{Cmd: svc.ParamChange, CurrentStatus: svc.Status{State: svc.Running, Accepts: acceptRunning}}
	expect(svc.Running)
	if reloads.Load() != 1 {
		t.Fatal("paramchange did not reload")
	}
	r <- svc.ChangeRequest{Cmd: svc.Stop}
	expect(svc.StopPending)
	res := <-done
	if res.specific || res.code != 0 || stops.Load() != 2 {
		t.Fatalf("stop: %+v, %d stops", res, stops.Load())
	}
	mu.Lock()
	defer mu.Unlock()
	if len(urls) < 2 {
		t.Fatalf("status URL recorded %d times", len(urls))
	}
}

func TestServiceHandlerStartFailure(t *testing.T) {
	h := &feederService{
		start: func() (svcRunner, error) { return nil, errors.New("configuration broken") },
		logf:  t.Logf, event: func(string, string) {}, tick: 10 * time.Millisecond,
	}
	s := make(chan svc.Status, 10)
	sp, code := h.Execute(nil, make(chan svc.ChangeRequest), s)
	if !sp || code != exitStartFailed {
		t.Fatalf("got %v %d", sp, code)
	}
}

// TestServiceBodyEndToEnd runs the real feeder under the service handler
// against a fake aggregator: start, pause (the session ends), continue (it
// comes back), stop.
func TestServiceBodyEndToEnd(t *testing.T) {
	srv, err := avftest.New(avftest.Options{Name: "testnet"})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	src := beastSource(t)
	dir := t.TempDir()
	cf := filepath.Join(dir, "feeder.json")
	cfg := model.DefaultFeederConfig()
	cfg.Receivers[0].Input.Host, cfg.Receivers[0].Input.Port = "127.0.0.1", src
	cfg.Uplinks = []model.Uplink{srv.Uplink("test")}
	cfg.UI.Addr = net.JoinHostPort("127.0.0.1", strconv.Itoa(freePort(t)))
	cfg.Results.BeastPort = 0
	if err := saveConfig(cf, cfg); err != nil {
		t.Fatal(err)
	}
	logDir := filepath.Join(dir, "logs")
	os.MkdirAll(logDir, 0o755)
	var cur atomic.Pointer[feeder]
	h := &feederService{
		start: func() (svcRunner, error) {
			f, err := startFeeder(runOptions{ConfigFile: cf, DataDir: dir, Logf: t.Logf, DetectEvery: time.Second})
			if err == nil {
				cur.Store(f)
			}
			return f, err
		},
		logf: t.Logf, event: func(string, string) {},
		onURL: func(u string) { writeStatusURL(logDir, u) },
		tick:  50 * time.Millisecond,
	}
	r := make(chan svc.ChangeRequest)
	s := make(chan svc.Status, 100)
	done := make(chan struct{})
	go func() { h.Execute(nil, r, s); close(done) }()
	connected := func() bool {
		f := cur.Load()
		if f == nil {
			return false
		}
		st := f.um.Status()
		return len(st) == 1 && st[0].State == uplink.StateConnected
	}
	waitFor(t, 15*time.Second, connected)
	b, err := os.ReadFile(filepath.Join(logDir, statusURLFile))
	if err != nil || strings.TrimSpace(string(b)) != "http://"+cfg.UI.Addr+"/" {
		t.Fatalf("status-url.txt %q %v", b, err)
	}
	sessions := len(srv.Hellos())

	r <- svc.ChangeRequest{Cmd: svc.Pause}
	// The status page goes away while paused.
	waitFor(t, 10*time.Second, func() bool {
		c, err := net.DialTimeout("tcp", cfg.UI.Addr, 200*time.Millisecond)
		if err == nil {
			c.Close()
			return false
		}
		return true
	})
	r <- svc.ChangeRequest{Cmd: svc.Continue}
	waitFor(t, 15*time.Second, func() bool { return connected() && len(srv.Hellos()) > sessions })
	r <- svc.ChangeRequest{Cmd: svc.Stop}
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("service did not stop")
	}
}
