//go:build windows

package wininstall

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type env struct {
	t    *testing.T
	root string
	sys  *fakeSystem
	in   *Installer
	log  []string
}

func newEnv(t *testing.T) *env {
	root := t.TempDir()
	e := &env{t: t, root: root, sys: newFake(root)}
	e.in = &Installer{Sys: e.sys, Logf: func(f string, a ...any) { e.log = append(e.log, fmt.Sprintf(f, a...)) }}
	return e
}

func (e *env) installer(files map[string]string) *Payload {
	e.t.Helper()
	out := buildTestInstaller(e.t, e.t.TempDir(), validProduct(), files)
	pl, err := OpenPayload(out)
	if err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(func() { pl.Close() })
	return pl
}

func (e *env) opts(pl *Payload, scope Scope) Options {
	o := DefaultOptions(pl.Product, scope)
	d, err := DefaultDir(e.sys, pl.Product, scope)
	if err != nil {
		e.t.Fatal(err)
	}
	o.Dir = d
	return o
}

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

func TestInstallUpgradeUninstall(t *testing.T) {
	e := newEnv(t)
	pl := e.installer(testFiles())
	o := e.opts(pl, PerMachine)
	o.Firewall = true
	res, err := e.in.Install(context.Background(), pl, o)
	if err != nil {
		t.Fatal(err)
	}
	if res.Upgraded || len(res.StepErrors) > 0 || res.RebootRequired {
		t.Fatalf("result %+v", res)
	}
	dir := filepath.Join(e.root, "machine", "pf", "AirView Test")
	if o.Dir != dir {
		t.Fatalf("default dir %s", o.Dir)
	}
	for n, c := range testFiles() {
		b, err := os.ReadFile(joinRel(dir, n))
		if err != nil || string(b) != c {
			t.Errorf("%s: %q %v", n, b, err)
		}
	}
	rec, err := ReadReceipt(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(rec.Files) != 5 || rec.ServiceName != "AirViewTestSvc" || !rec.Service || rec.TrayRunValue != "AirView Test Tray" || rec.Firewall != "AirView Test web" {
		t.Errorf("receipt %+v", rec)
	}
	// Uninstall entry.
	info, ok, _ := e.sys.ReadUninstall(PerMachine, "AirViewTest")
	if !ok || info.DisplayName != "AirView Test" || info.DisplayVersion != "1.2.3" || info.Publisher != "AirView" ||
		info.InstallLocation != dir || info.UninstallString != `"`+filepath.Join(dir, "uninstall.exe")+`"` ||
		!strings.HasSuffix(info.QuietUninstallString, " /S") || info.DisplayIcon != filepath.Join(dir, "airview.exe")+",0" || info.EstimatedSizeKB == 0 {
		t.Errorf("uninstall entry %+v", info)
	}
	// Shortcuts: 2 + url in the Start menu (no uninstaller without a stub), 1 on the desktop.
	sm := filepath.Join(e.root, "machine", "programs", "AirView Test")
	for _, n := range []string{"AirView Test.lnk", "Docs.lnk", "Status page.url"} {
		if !exists(filepath.Join(sm, n)) {
			t.Errorf("start menu: %s missing", n)
		}
	}
	if !exists(filepath.Join(e.root, "machine", "desktop", "AirView Test.lnk")) || exists(filepath.Join(e.root, "machine", "desktop", "Docs.lnk")) {
		t.Error("desktop shortcuts")
	}
	if url, _ := os.ReadFile(filepath.Join(sm, "Status page.url")); !strings.Contains(string(url), "URL=http://127.0.0.1:8080/") {
		t.Errorf("url shortcut %q", url)
	}
	if s := e.sys.shortcuts[filepath.Join(sm, "AirView Test.lnk")]; s.Target != filepath.Join(dir, "airview.exe") || s.WorkDir != dir || s.Description != "station" {
		t.Errorf("shortcut %+v", s)
	}
	// Service, tray, firewall.
	if running := e.sys.services["AirViewTestSvc"]; !running {
		t.Error("service not installed")
	}
	if !strings.Contains(strings.Join(e.sys.execs, "\n"), `-data `+filepath.Join(e.root, "programdata", "AirView Test")) {
		t.Errorf("service command lacks the data dir: %v", e.sys.execs)
	}
	if v, ok := e.sys.RunValue(PerMachine, "AirView Test Tray"); !ok || v != `"`+filepath.Join(dir, "tray.exe")+`"` {
		t.Errorf("run value %q", v)
	}
	// Without /PORT= the rule follows the program on any port (UX-09).
	if fw := e.sys.firewall["AirView Test web"]; fw != filepath.Join(dir, "airview.exe")+":0:private,domain" {
		t.Errorf("firewall %q", fw)
	}

	// User data appears; an upgrade keeps it and drops files v2 no longer has.
	data := filepath.Join(e.root, "programdata", "AirView Test")
	writeTree(t, data, map[string]string{"feeder.json": "{}", "identity.key": "secret"})
	writeTree(t, filepath.Join(dir, "data"), map[string]string{"airview.db": "db"})
	v2 := testFiles()
	delete(v2, "docs/old/REMOVED.md")
	v2["airview.exe"] = "MZ program v2"
	v2["docs/NEW.md"] = "new"
	pl2 := e.installer(v2)
	o2 := e.opts(pl2, PerMachine)
	if o2.Dir != dir {
		t.Fatalf("upgrade dir %s", o2.Dir)
	}
	e.sys.execs = nil
	res, err = e.in.Install(context.Background(), pl2, o2)
	if err != nil || !res.Upgraded {
		t.Fatalf("upgrade: %+v %v", res, err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "airview.exe")); string(b) != "MZ program v2" {
		t.Errorf("not upgraded: %q", b)
	}
	if exists(filepath.Join(dir, "docs", "old", "REMOVED.md")) || exists(filepath.Join(dir, "docs", "old")) {
		t.Error("stale file / folder kept")
	}
	if !exists(filepath.Join(data, "identity.key")) || !exists(filepath.Join(dir, "data", "airview.db")) {
		t.Error("upgrade touched user data")
	}
	if !strings.Contains(strings.Join(e.sys.execs, "\n"), "tray -signal quit") {
		t.Errorf("tray not closed before the upgrade: %v", e.sys.execs)
	}
	matches, _ := filepath.Glob(filepath.Join(dir, "*.av*"))
	if len(matches) != 0 {
		t.Errorf("transaction leftovers %v", matches)
	}

	// Uninstall, keeping data.
	res, err = e.in.Uninstall(context.Background(), dir, UninstallOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := e.sys.ReadUninstall(PerMachine, "AirViewTest"); ok {
		t.Error("uninstall entry kept")
	}
	if len(e.sys.services) != 0 || len(e.sys.firewall) != 0 {
		t.Errorf("service %v / firewall %v kept", e.sys.services, e.sys.firewall)
	}
	if _, ok := e.sys.RunValue(PerMachine, "AirView Test Tray"); ok {
		t.Error("run value kept")
	}
	if exists(sm) || exists(filepath.Join(e.root, "machine", "desktop", "AirView Test.lnk")) {
		t.Error("shortcuts kept")
	}
	for n := range v2 {
		if exists(joinRel(dir, n)) {
			t.Errorf("%s kept", n)
		}
	}
	if !exists(filepath.Join(data, "identity.key")) || !exists(filepath.Join(dir, "data", "airview.db")) {
		t.Error("data removed without /REMOVEDATA")
	}
	if len(res.Kept) != 2 {
		t.Errorf("kept %v", res.Kept)
	}
	if exists(filepath.Join(dir, ReceiptName)) || exists(filepath.Join(dir, UninstallerName)) {
		t.Error("receipt / uninstaller kept")
	}
}

func TestUninstallRemoveData(t *testing.T) {
	e := newEnv(t)
	pl := e.installer(testFiles())
	o := e.opts(pl, PerMachine)
	if _, err := e.in.Install(context.Background(), pl, o); err != nil {
		t.Fatal(err)
	}
	data := filepath.Join(e.root, "programdata", "AirView Test")
	writeTree(t, data, map[string]string{"identity.key": "secret"})
	writeTree(t, filepath.Join(o.Dir, "data"), map[string]string{"airview.db": "db"})
	if _, err := e.in.Uninstall(context.Background(), o.Dir, UninstallOptions{RemoveData: true}); err != nil {
		t.Fatal(err)
	}
	if exists(data) || exists(o.Dir) {
		t.Errorf("data %v / dir %v kept", exists(data), exists(o.Dir))
	}
	if !strings.Contains(strings.Join(e.sys.execs, "\n"), "airview.exe purge") {
		t.Errorf("purge command not run: %v", e.sys.execs)
	}
}

func TestUninstallKeepsSelfAndForeignFiles(t *testing.T) {
	e := newEnv(t)
	pl := e.installer(testFiles())
	o := e.opts(pl, PerMachine)
	o.Stub = filepath.Join(e.t.TempDir(), "fake-uninstaller.exe")
	_ = os.WriteFile(o.Stub, fakePE(2048), 0o644)
	o.StubSize = 2048
	if _, err := e.in.Install(context.Background(), pl, o); err != nil {
		t.Fatal(err)
	}
	un := filepath.Join(o.Dir, UninstallerName)
	if st, err := os.Stat(un); err != nil || st.Size() != 2048 {
		t.Fatalf("uninstaller: %v", err)
	}
	unLnk := filepath.Join(e.root, "machine", "programs", "AirView Test", "Uninstall AirView Test.lnk")
	if s := e.sys.shortcuts[unLnk]; s.Target != un {
		t.Errorf("uninstall shortcut %+v", s)
	}
	writeTree(t, o.Dir, map[string]string{"notes-by-user.txt": "mine"})
	if _, err := e.in.Uninstall(context.Background(), o.Dir, UninstallOptions{Self: un}); err != nil {
		t.Fatal(err)
	}
	if !exists(un) || !exists(filepath.Join(o.Dir, "notes-by-user.txt")) {
		t.Error("the running uninstaller or a user file was deleted")
	}
}

func TestInstallRollback(t *testing.T) {
	e := newEnv(t)
	pl := e.installer(testFiles())
	o := e.opts(pl, PerMachine)
	if _, err := e.in.Install(context.Background(), pl, o); err != nil {
		t.Fatal(err)
	}
	// v2 has a file whose folder is blocked by a plain file in the
	// installation: the copy fails part-way and v1 must be back.
	writeTree(t, o.Dir, map[string]string{"zzz": "blocker"})
	v2 := testFiles()
	v2["airview.exe"] = "MZ program v2"
	v2["zzz/inner.txt"] = "cannot be created"
	pl2 := e.installer(v2)
	if _, err := e.in.Install(context.Background(), pl2, e.opts(pl2, PerMachine)); err == nil {
		t.Fatal("blocked install succeeded")
	}
	if b, _ := os.ReadFile(filepath.Join(o.Dir, "airview.exe")); string(b) != "MZ program v1" {
		t.Errorf("rollback: airview.exe = %q", b)
	}
	matches, _ := filepath.Glob(filepath.Join(o.Dir, "*.av*"))
	if len(matches) != 0 {
		t.Errorf("leftovers %v", matches)
	}
	if rec, err := ReadReceipt(o.Dir); err != nil || rec.Product.Version != "1.2.3" {
		t.Errorf("receipt after rollback: %v", err)
	}

	// A fresh install that is cancelled leaves nothing behind.
	e2 := newEnv(t)
	pl3 := e2.installer(testFiles())
	o3 := e2.opts(pl3, PerMachine)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := e2.in.Install(ctx, pl3, o3); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled: %v", err)
	}
	if exists(o3.Dir) {
		t.Error("cancelled install left its folder")
	}
	if _, ok, _ := e2.sys.ReadUninstall(PerMachine, "AirViewTest"); ok {
		t.Error("cancelled install registered")
	}
}

func TestPerUserAndElevation(t *testing.T) {
	e := newEnv(t)
	e.sys.elevated = false
	pl := e.installer(testFiles())
	if _, err := e.in.Install(context.Background(), pl, e.opts(pl, PerMachine)); !errors.Is(err, ErrNeedsAdmin) {
		t.Fatalf("per-machine without admin: %v", err)
	}
	o := e.opts(pl, PerUser)
	if o.Service || o.Firewall {
		t.Fatalf("per-user defaults %+v", o)
	}
	if _, err := e.in.Install(context.Background(), pl, o); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(o.Dir, filepath.Join(e.root, "user", "pf")) {
		t.Errorf("per-user dir %s", o.Dir)
	}
	if _, ok, _ := e.sys.ReadUninstall(PerUser, "AirViewTest"); !ok {
		t.Error("no HKCU entry")
	}
	if _, ok := e.sys.RunValue(PerUser, "AirView Test Tray"); !ok {
		t.Error("no HKCU Run value")
	}
	if len(e.sys.services) != 0 || len(e.sys.firewall) != 0 {
		t.Error("per-user install touched services / firewall")
	}
	// A per-machine install into the same folder is refused.
	e.sys.elevated = true
	om := e.opts(pl, PerMachine)
	om.Dir = o.Dir
	if _, err := e.in.Install(context.Background(), pl, om); err == nil {
		t.Error("scope change in place accepted")
	}
	if _, err := e.in.Uninstall(context.Background(), o.Dir, UninstallOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := e.sys.ReadUninstall(PerUser, "AirViewTest"); ok {
		t.Error("HKCU entry kept")
	}
}

func TestInstallRefusesForeignFolderAndSystemFolders(t *testing.T) {
	e := newEnv(t)
	pl := e.installer(testFiles())
	o := e.opts(pl, PerMachine)
	if _, err := e.in.Install(context.Background(), pl, o); err != nil {
		t.Fatal(err)
	}
	other := validProduct()
	other.ID = "SomethingElse"
	out := buildTestInstaller(t, t.TempDir(), other, testFiles())
	pl2, _ := OpenPayload(out)
	defer pl2.Close()
	o2 := DefaultOptions(other, PerMachine)
	o2.Dir = o.Dir
	if _, err := e.in.Install(context.Background(), pl2, o2); err == nil || !strings.Contains(err.Error(), "another program") {
		t.Errorf("foreign folder: %v", err)
	}
	f, _ := e.sys.Folders(PerMachine)
	for _, d := range []string{f[FolderProgramFiles], f[FolderCommonAppData], f[FolderPrograms], e.root[:3]} {
		o3 := o
		o3.Dir = d
		if _, err := e.in.Install(context.Background(), pl, o3); err == nil {
			t.Errorf("installed into %s", d)
		}
	}
}

func TestConfigureAndStepErrors(t *testing.T) {
	e := newEnv(t)
	pl := e.installer(testFiles())
	o := e.opts(pl, PerMachine)
	a, _ := ParseArgs([]string{"/SERVER=feed.example.org", "/ENROL=TOKEN123", "/NOLOCATION"})
	if err := o.ApplyArgs(pl.Product, a); err != nil {
		t.Fatal(err)
	}
	e.sys.execFn = func(exe string, args []string) (string, int, error) {
		if args[0] == "service" && args[1] == "install" {
			return "access denied", 1, nil
		}
		return "", 0, nil
	}
	res, err := e.in.Install(context.Background(), pl, o)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(e.sys.execs, "\n")
	if !strings.Contains(got, "airview.exe setup -non-interactive -server feed.example.org -enrol TOKEN123 -no-location") {
		t.Errorf("configure command: %s", got)
	}
	if len(res.StepErrors) != 1 || !strings.Contains(res.StepErrors[0], "service") {
		t.Errorf("step errors %v", res.StepErrors)
	}
	for _, l := range e.log {
		if strings.Contains(l, "TOKEN123") {
			t.Error("enrol token in the log")
		}
	}
}

// DLV-03: post-install commands run only when their conditions expand to
// non-empty values (the feeder's update key and URL baked in at build time).
func TestPostInstallWhen(t *testing.T) {
	run := func(vars map[string]string) []string {
		e := newEnv(t)
		p := validProduct()
		for k, v := range vars {
			p.Vars[k] = v
		}
		p.PostInstall = []PostCommand{
			{What: "key", When: []string{"{manifestkey}"}, Command: Command{Exe: `{app}\airview.exe`, Args: []string{"config", "set", "update.keys={manifestkey}"}}},
			{What: "url", When: []string{"{manifestkey}", "{updateurl}"}, Command: Command{Exe: `{app}\airview.exe`, Args: []string{"config", "set", "update.manifestUrl={updateurl}"}}},
			{What: "unknown var", When: []string{"{nosuchvar}"}, Command: Command{Exe: `{app}\airview.exe`, Args: []string{"never"}}},
		}
		out := buildTestInstaller(t, t.TempDir(), p, testFiles())
		pl, err := OpenPayload(out)
		if err != nil {
			t.Fatal(err)
		}
		defer pl.Close()
		if _, err := e.in.Install(context.Background(), pl, e.opts(pl, PerMachine)); err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, x := range e.sys.execs {
			if strings.Contains(x, "config set") || strings.Contains(x, "never") {
				got = append(got, x)
			}
		}
		return got
	}
	if got := run(map[string]string{"manifestkey": "", "updateurl": ""}); len(got) != 0 {
		t.Errorf("no key baked in: ran %v", got)
	}
	if got := run(map[string]string{"manifestkey": "KEY=", "updateurl": ""}); len(got) != 1 || !strings.HasSuffix(got[0], "update.keys=KEY=") {
		t.Errorf("key only: ran %v", got)
	}
	got := run(map[string]string{"manifestkey": "KEY=", "updateurl": "https://rel.example/feeder/manifest.json"})
	if len(got) != 2 || !strings.HasSuffix(got[1], "update.manifestUrl=https://rel.example/feeder/manifest.json") {
		t.Errorf("key and URL: ran %v", got)
	}
}

// The shipped feeder product writes the update settings only when the
// release build baked them in (mkinstaller -var).
func TestShippedFeederPostInstall(t *testing.T) {
	p, err := LoadProduct(filepath.Join("..", "..", "deploy", "installer", "products", "feeder.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := p.Vars["manifestkey"]; !ok {
		t.Fatal("feeder.json: no manifestkey var")
	}
	if _, ok := p.Vars["updateurl"]; !ok {
		t.Fatal("feeder.json: no updateurl var")
	}
	var keys, url bool
	for _, c := range p.PostInstall {
		a := strings.Join(c.Args, " ")
		if strings.Contains(a, "update.keys={manifestkey}") && len(c.When) > 0 {
			keys = true
		}
		if strings.Contains(a, "update.manifestUrl={updateurl}") && len(c.When) > 1 {
			url = true
		}
	}
	if !keys || !url {
		t.Errorf("feeder.json postInstall must set update.keys and update.manifestUrl conditionally: %+v", p.PostInstall)
	}
}
