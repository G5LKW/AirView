package wininstall

import (
	"strings"
	"testing"
)

func TestExpand(t *testing.T) {
	p := validProduct()
	e := &Expander{Folders: map[string]string{FolderProgramFiles: `C:\Program Files`, FolderCommonAppData: `C:\ProgramData`}, App: `C:\Program Files\X`, Port: 8080, Product: p}
	cases := map[string]string{
		`{app}\airview.exe`:          `C:\Program Files\X\airview.exe`,
		`{datadir}\feeder.json`:      `C:\ProgramData\AirView Test\feeder.json`,
		`http://127.0.0.1:{port}/`:   `http://127.0.0.1:8080/`,
		`{name} {version} ({id})`:    `AirView Test 1.2.3 (AirViewTest)`,
		`no tokens, {Braces} stay`:   `no tokens, {Braces} stay`,
		`{pf}\{name}\{service}.json`: `C:\Program Files\AirView Test\AirViewTestSvc.json`,
	}
	for in, want := range cases {
		got, err := e.Expand(in)
		if err != nil || got != want {
			t.Errorf("Expand(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{`{ap}\x`, `{desktop}`, `{userappdata}`} {
		if _, err := e.Expand(bad); err == nil {
			t.Errorf("Expand(%q): unknown/empty token accepted", bad)
		}
	}
	p.Vars["loop"] = "{loop}"
	if _, err := e.Expand("{loop}"); err == nil {
		t.Error("self-referencing variable accepted")
	}
}

func TestCheckInstallDir(t *testing.T) {
	protected := []string{`C:\Program Files`, `C:\ProgramData`, `C:\Windows\System32`, `C:\Windows`, `C:\Users\u`}
	ok := []string{`C:\Program Files\AirView`, `D:\Apps\AirView`, `\\server\share\AirView`, `C:\Program Files\AirView\`}
	for _, d := range ok {
		if err := CheckInstallDir(d, protected); err != nil {
			t.Errorf("%s rejected: %v", d, err)
		}
	}
	bad := []string{``, `C:\`, `C:`, `D:/`, `AirView`, `\AirView`, `C:\Program Files`, `c:\program files\`, `C:\Windows`,
		`C:\Program Files\X\..`, `\\server\share`, `C:\a|b`, `C:\a?b`, `C:\a:b`, `C:\Users\u`}
	for _, d := range bad {
		if err := CheckInstallDir(d, protected); err == nil {
			t.Errorf("%q accepted", d)
		}
	}
}

func TestCleanWin(t *testing.T) {
	for in, want := range map[string]string{
		`C:\a\b\`:          `C:\a\b`,
		`C:/a//b/./c`:      `C:\a\b\c`,
		`C:\a\..\b`:        `C:\b`,
		`C:\`:              `C:\`,
		`\\srv\share\x\..`: `\\srv\share`,
	} {
		if got := cleanWin(in); !strings.EqualFold(got, want) {
			t.Errorf("cleanWin(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestScope(t *testing.T) {
	for _, s := range []Scope{PerMachine, PerUser} {
		got, err := ParseScope(s.String())
		if err != nil || got != s {
			t.Errorf("round trip %v", s)
		}
	}
	if _, err := ParseScope("galaxy"); err == nil {
		t.Error("bad scope accepted")
	}
}
