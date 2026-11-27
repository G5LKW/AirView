package wininstall

import (
	"path/filepath"
	"strings"
	"testing"
)

func validProduct() *Product {
	return &Product{
		ID: "AirViewTest", Name: "AirView Test", Publisher: "AirView", Version: "1.2.3",
		MainExe: "airview.exe", DefaultDir: `{pf}\AirView Test`, StartMenuFolder: "AirView Test",
		Licence: "LICENSE.txt",
		Shortcuts: []Shortcut{
			{Name: "AirView Test", Target: `{app}\airview.exe`, Desktop: true, Description: "station"},
			{Name: "Docs", Target: `{app}\docs`},
		},
		URLShortcuts: []URLShortcut{{Name: "Status page", URL: "http://127.0.0.1:{port}/"}},
		Vars:         map[string]string{"service": "AirViewTestSvc", "datadir": `{commonappdata}\AirView Test`},
		Service: &ServiceSpec{Name: "{service}", Default: true,
			Install: Command{Exe: `{app}\airview.exe`, Args: []string{"service", "install", "-name", "{service}", "-now", "-data", "{datadir}"}},
			Remove:  Command{Exe: `{app}\airview.exe`, Args: []string{"service", "remove", "-name", "{service}"}}},
		Tray:     &TraySpec{Exe: `{app}\tray.exe`, RunValue: "AirView Test Tray", Default: true, Quit: &Command{Exe: `{app}\airview.exe`, Args: []string{"tray", "-signal", "quit"}}},
		Firewall: &FirewallSpec{Rule: "AirView Test web", Program: `{app}\airview.exe`, Port: 8080},
		Configure: &ConfigureSpec{Trigger: "SERVER", Command: Command{Exe: `{app}\airview.exe`, Args: []string{"setup", "-non-interactive"}},
			Params: []Param{{Switch: "SERVER", Flag: "-server"}, {Switch: "ENROL", Flag: "-enrol"}, {Switch: "NOLOCATION", Flag: "-no-location", Bool: true}}},
		Launch:    &Launch{Target: `{app}\airview.exe`},
		DataDirs:  []string{"{datadir}", `{app}\data`},
		PurgeData: &Command{Exe: `{app}\airview.exe`, Args: []string{"purge"}},
	}
}

func TestProductValidate(t *testing.T) {
	if err := validProduct().Validate(); err != nil {
		t.Fatalf("valid product rejected: %v", err)
	}
	cases := map[string]func(p *Product){
		"bad id":          func(p *Product) { p.ID = "Air View" },
		"empty name":      func(p *Product) { p.Name = " " },
		"name with colon": func(p *Product) { p.Name = "a:b" },
		"bad version":     func(p *Product) { p.Version = "one" },
		"abs mainExe":     func(p *Product) { p.MainExe = `C:\x.exe` },
		"dotdot mainExe":  func(p *Product) { p.MainExe = `..\x.exe` },
		"relative dir":    func(p *Product) { p.DefaultDir = `AirView` },
		"dup shortcut":    func(p *Product) { p.Shortcuts = append(p.Shortcuts, Shortcut{Name: "docs", Target: "x"}) },
		"url not http":    func(p *Product) { p.URLShortcuts[0].URL = "file:///c:/x" },
		"bad service":     func(p *Product) { p.Service.Name = "a b" },
		"no remove":       func(p *Product) { p.Service.Remove.Exe = "" },
		"tray backslash":  func(p *Product) { p.Tray.RunValue = `a\b` },
		"port 0":          func(p *Product) { p.Firewall.Port = 0 },
		"bad profiles":    func(p *Product) { p.Firewall.Profiles = "home" },
		"reserved switch": func(p *Product) {
			p.Configure.Params = append(p.Configure.Params, Param{Switch: "PORT", Flag: "-port"})
		},
		"lower switch":     func(p *Product) { p.Configure.Trigger = "server" },
		"var shadows":      func(p *Product) { p.Vars["app"] = "x" },
		"relative datadir": func(p *Product) { p.DataDirs = []string{"data"} },
	}
	for name, mut := range cases {
		p := validProduct()
		mut(p)
		if err := p.Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestParseProductRejectsUnknownFields(t *testing.T) {
	_, err := ParseProduct([]byte(`{"id":"A","name":"A","mainExe":"a.exe","defaultDir":"{pf}\\A","shortcutz":[]}`))
	if err == nil || !strings.Contains(err.Error(), "shortcutz") {
		t.Fatalf("unknown field not reported: %v", err)
	}
}

// The shipped product definitions must stay valid.
func TestShippedProducts(t *testing.T) {
	for _, n := range []string{"station", "feeder"} {
		p, err := LoadProduct(filepath.Join("..", "..", "deploy", "installer", "products", n+".json"))
		if err != nil {
			t.Fatalf("%s: %v", n, err)
		}
		p.Version = "1.0.0"
		if err := p.Validate(); err != nil {
			t.Fatalf("%s: %v", n, err)
		}
		if p.Licence != "LICENSE.txt" || len(p.DataDirs) == 0 {
			t.Errorf("%s: licence %q, data dirs %v", n, p.Licence, p.DataDirs)
		}
		e := &Expander{Folders: map[string]string{FolderProgramFiles: `C:\Program Files`, FolderCommonAppData: `C:\ProgramData`,
			FolderUserAppData: `C:\Users\u\AppData\Roaming`}, App: `C:\Program Files\X`, Port: 1, Product: p}
		for _, d := range p.DataDirs {
			if _, err := e.Expand(d); err != nil {
				t.Errorf("%s: data dir %s: %v", n, d, err)
			}
		}
		if _, err := ResolveNames(p, e); err != nil {
			t.Errorf("%s: %v", n, err)
		}
	}
	feeder, _ := LoadProduct(filepath.Join("..", "..", "deploy", "installer", "products", "feeder.json"))
	if feeder.Service == nil || feeder.Tray == nil || feeder.Tray.RunValue != "{tray}" || feeder.Vars["tray"] != "AirView Feeder Tray" || feeder.Vars["service"] != "AirViewFeeder" {
		t.Errorf("feeder definition: service / tray must match cmd/airview-feeder (AirViewFeeder, %q)", "AirView Feeder Tray")
	}
	if feeder.Firewall == nil || feeder.Firewall.Default {
		t.Errorf("feeder: the firewall rule must be opt-in (the status page listens on 127.0.0.1 by default)")
	}
}
