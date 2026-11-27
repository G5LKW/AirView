package wininstall

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Product describes one installable product (the station or the feeder).
// It is stored as product.json inside the payload and copied into the
// receipt, so the uninstaller knows exactly what the installer did. Paths
// may use the tokens of Expand ({app}, {pf}, {programs}, ...) and the
// product's own Vars ({name} for Vars["name"]).
type Product struct {
	// ID names the uninstall registry key (Software\Microsoft\Windows\
	// CurrentVersion\Uninstall\<ID>), the setup log and the named mutex.
	// Letters, digits, '.', '_' and '-' only.
	ID        string `json:"id"`
	Name      string `json:"name"`      // display name (Apps & features, wizard)
	Publisher string `json:"publisher"` // Apps & features "Publisher"
	Version   string `json:"version"`   // DisplayVersion; set by the builder
	Comments  string `json:"comments,omitempty"`
	// FinishNote is added to the wizard's finish page after an install
	// (e.g. the feeder's "next: enter your aggregator" step, UX-10).
	FinishNote string `json:"finishNote,omitempty"`
	HelpLink   string `json:"helpLink,omitempty"`

	// MainExe is the program relative to the install directory: the
	// DisplayIcon of the uninstall entry and the "launch now" target.
	MainExe string `json:"mainExe"`
	// DefaultDir is the default destination; {pf} is Program Files for a
	// per-machine install and %LOCALAPPDATA%\Programs for a per-user one.
	DefaultDir string `json:"defaultDir"`
	// StartMenuFolder is the folder below the Start menu's Programs.
	StartMenuFolder string `json:"startMenuFolder,omitempty"`

	// Licence is the payload path of the licence text shown by the wizard
	// (relative to files/, e.g. "LICENSE.txt"); empty skips the page.
	Licence string `json:"licence,omitempty"`

	Shortcuts    []Shortcut    `json:"shortcuts,omitempty"`
	URLShortcuts []URLShortcut `json:"urlShortcuts,omitempty"`

	Service   *ServiceSpec   `json:"service,omitempty"`
	Tray      *TraySpec      `json:"tray,omitempty"`
	Firewall  *FirewallSpec  `json:"firewall,omitempty"`
	Configure *ConfigureSpec `json:"configure,omitempty"`
	// PostInstall commands run after Configure and before the service is
	// registered, each only when every entry of its When list expands to
	// a non-empty string. The feeder uses it to write the release
	// manifest URL and signing key baked in at build time
	// (mkinstaller -var updateurl=... -var manifestkey=...): without
	// them the feeder never updates itself (DLV-03).
	PostInstall []PostCommand `json:"postInstall,omitempty"`

	// Launch is run (not elevated, as the desktop user) after an
	// interactive install when the user leaves "Launch" ticked.
	Launch *Launch `json:"launch,omitempty"`

	// DataDirs are the product's user data. Never touched on install or
	// upgrade; deleted by the uninstaller only when "remove data" is chosen.
	DataDirs []string `json:"dataDirs,omitempty"`
	// PurgeData runs before the data folders are deleted (while the
	// program files still exist), e.g. the feeder's "uninstall-helper
	// -purge -yes".
	PurgeData *Command `json:"purgeData,omitempty"`

	// Vars are product variables usable as {name} in every expanded string.
	Vars map[string]string `json:"vars,omitempty"`
}

// Shortcut is a .lnk in the Start-menu folder (and optionally the desktop).
type Shortcut struct {
	Name        string   `json:"name"`              // file name without .lnk
	Target      string   `json:"target"`            // e.g. {app}\airview.exe
	Args        []string `json:"args,omitempty"`    // command-line arguments
	WorkDir     string   `json:"workDir,omitempty"` // default: the target's folder
	Icon        string   `json:"icon,omitempty"`    // default: the target
	Description string   `json:"description,omitempty"`
	// Desktop also puts the shortcut on the desktop when the desktop
	// option is chosen.
	Desktop bool `json:"desktop,omitempty"`
}

// URLShortcut is an Internet shortcut (.url) in the Start-menu folder.
type URLShortcut struct {
	Name string `json:"name"`
	URL  string `json:"url"`
	Icon string `json:"icon,omitempty"`
}

// Command is a program the installer runs.
type Command struct {
	Exe  string   `json:"exe"` // e.g. {app}\airview-feeder.exe
	Args []string `json:"args,omitempty"`
	// TimeoutSec bounds the run (default 120 s).
	TimeoutSec int `json:"timeoutSec,omitempty"`
}

// Service.Name, Tray.RunValue and Firewall.Rule may use {var} tokens of
// Vars (so one definition serves the release and a scratch test identity);
// they are resolved once at install time and stored resolved in the receipt.

// ServiceSpec is a Windows service registered by a command of the product
// (the feeder's "service install", which also sets ACLs, recovery actions
// and the event log source) and removed by the uninstaller.
type ServiceSpec struct {
	Name    string  `json:"name"`
	Default bool    `json:"default"`
	Install Command `json:"install"`
	Remove  Command `json:"remove"`
}

// TraySpec is a per-session helper started at log-on (HKLM or HKCU Run).
type TraySpec struct {
	Exe      string   `json:"exe"`      // e.g. {app}\airview-feeder-tray.exe
	RunValue string   `json:"runValue"` // value name under ...\Run
	Default  bool     `json:"default"`
	Quit     *Command `json:"quit,omitempty"` // closes running trays before files are replaced
}

// FirewallSpec is an inbound allow rule for the product's HTTP port.
type FirewallSpec struct {
	Rule    string `json:"rule"`    // rule name
	Program string `json:"program"` // e.g. {app}\airview.exe
	Port    int    `json:"port"`    // default TCP port (/PORT= overrides)
	Default bool   `json:"default"`
	// Profiles for netsh (default "private,domain").
	Profiles string `json:"profiles,omitempty"`
}

// ConfigureSpec forwards installer switches (/SERVER=host ...) to a
// configuration command, run after the files are installed when the
// Trigger switch is present.
type ConfigureSpec struct {
	Trigger string  `json:"trigger"` // e.g. "SERVER"
	Command Command `json:"command"`
	Params  []Param `json:"params"`
}

// PostCommand is a Command run after install when its When values (e.g.
// "{manifestkey}") are all non-empty once expanded. A failure is reported
// as a step error, never as a failed install.
type PostCommand struct {
	Command
	When []string `json:"when,omitempty"`
	// What names the step in the log and in step errors.
	What string `json:"what,omitempty"`
	// MachineOnly runs the step only for a per-machine (elevated) install,
	// e.g. sealing the feeder's update trust anchors in HKLM (SEC-R1).
	MachineOnly bool `json:"machineOnly,omitempty"`
}

// Param maps one installer switch to command-line arguments.
type Param struct {
	Switch string `json:"switch"`         // e.g. "LAT" for /LAT=51.5
	Flag   string `json:"flag"`           // e.g. "-lat"
	Bool   bool   `json:"bool,omitempty"` // a bare switch (/NOLOCATION) adding just Flag
}

// Launch is the program offered on the last wizard page.
type Launch struct {
	Target  string   `json:"target"`
	Args    []string `json:"args,omitempty"`
	Caption string   `json:"caption,omitempty"`
}

var (
	idRE      = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)
	versionRE = regexp.MustCompile(`^[0-9]+(\.[0-9]+){0,3}([-+][A-Za-z0-9.-]+)?$`)
	switchRE  = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,31}$`)
)

// Validate checks the definition; the builder refuses invalid products and
// the installer re-checks the embedded one.
func (p *Product) Validate() error {
	var errs []error
	bad := func(f string, a ...any) { errs = append(errs, fmt.Errorf(f, a...)) }
	if !idRE.MatchString(p.ID) {
		bad("id %q: 1-64 letters, digits, '.', '_' or '-'", p.ID)
	}
	if strings.TrimSpace(p.Name) == "" || strings.ContainsAny(p.Name, `\/:*?"<>|`) {
		bad("name %q: required, and no \\ / : * ? \" < > |", p.Name)
	}
	if p.Version != "" && !versionRE.MatchString(p.Version) {
		bad("version %q: expected e.g. 1.2.3", p.Version)
	}
	if !relPathOK(p.MainExe) {
		bad("mainExe %q: a path inside the install directory", p.MainExe)
	}
	if !strings.Contains(p.DefaultDir, "{") && !filepath.IsAbs(p.DefaultDir) {
		bad("defaultDir %q: absolute, or starting with a token such as {pf}", p.DefaultDir)
	}
	if strings.ContainsAny(p.StartMenuFolder, `:*?"<>|`) || strings.Contains(p.StartMenuFolder, "..") {
		bad("startMenuFolder %q", p.StartMenuFolder)
	}
	if p.Licence != "" && !relPathOK(p.Licence) {
		bad("licence %q: a payload path", p.Licence)
	}
	names := map[string]bool{}
	for i, s := range p.Shortcuts {
		if s.Name == "" || strings.ContainsAny(s.Name, `\/:*?"<>|`) {
			bad("shortcuts[%d].name %q", i, s.Name)
		}
		if s.Target == "" {
			bad("shortcuts[%d].target: required", i)
		}
		if names[strings.ToLower(s.Name)] {
			bad("shortcuts[%d].name %q: duplicate", i, s.Name)
		}
		names[strings.ToLower(s.Name)] = true
	}
	for i, s := range p.URLShortcuts {
		if s.Name == "" || strings.ContainsAny(s.Name, `\/:*?"<>|`) {
			bad("urlShortcuts[%d].name %q", i, s.Name)
		}
		if !strings.HasPrefix(s.URL, "http://") && !strings.HasPrefix(s.URL, "https://") {
			bad("urlShortcuts[%d].url %q: http(s) only", i, s.URL)
		}
		if names[strings.ToLower(s.Name)] {
			bad("urlShortcuts[%d].name %q: duplicate", i, s.Name)
		}
		names[strings.ToLower(s.Name)] = true
	}
	if s := p.Service; s != nil {
		if !idRE.MatchString(s.Name) && !strings.Contains(s.Name, "{") {
			bad("service.name %q", s.Name)
		}
		if s.Install.Exe == "" || s.Remove.Exe == "" {
			bad("service: install and remove commands are required")
		}
	}
	if t := p.Tray; t != nil {
		if t.Exe == "" || t.RunValue == "" || strings.ContainsAny(t.RunValue, `\`) {
			bad("tray: exe and runValue (no backslash) are required")
		}
	}
	if f := p.Firewall; f != nil {
		if f.Rule == "" || strings.ContainsAny(f.Rule, `"`) {
			bad("firewall.rule %q: required, no quotes", f.Rule)
		}
		if f.Port < 1 || f.Port > 65535 {
			bad("firewall.port %d: 1-65535", f.Port)
		}
		if f.Profiles != "" && !regexp.MustCompile(`^(private|public|domain|any)(,(private|public|domain))*$`).MatchString(f.Profiles) {
			bad("firewall.profiles %q", f.Profiles)
		}
	}
	if c := p.Configure; c != nil {
		if !switchRE.MatchString(c.Trigger) || c.Command.Exe == "" {
			bad("configure: trigger (upper-case switch) and command.exe are required")
		}
		for i, prm := range c.Params {
			if !switchRE.MatchString(prm.Switch) || !strings.HasPrefix(prm.Flag, "-") {
				bad("configure.params[%d]: switch %q / flag %q", i, prm.Switch, prm.Flag)
			}
			if reservedSwitches[prm.Switch] {
				bad("configure.params[%d]: /%s is an installer switch", i, prm.Switch)
			}
		}
	}
	for i, c := range p.PostInstall {
		if c.Exe == "" {
			bad("postInstall[%d]: exe is required", i)
		}
	}
	for k := range p.Vars {
		if !regexp.MustCompile(`^[a-z][a-z0-9]*$`).MatchString(k) || builtinTokens[k] {
			bad("vars: %q is not a valid or free variable name", k)
		}
	}
	for i, d := range p.DataDirs {
		if !strings.HasPrefix(d, "{") && !filepath.IsAbs(d) {
			bad("dataDirs[%d] %q: absolute or token-based", i, d)
		}
	}
	return errors.Join(errs...)
}

// relPathOK: a clean relative path that stays inside its root.
func relPathOK(p string) bool {
	if p == "" || filepath.IsAbs(p) || strings.HasPrefix(p, `\`) || strings.HasPrefix(p, "/") || strings.Contains(p, ":") {
		return false
	}
	for _, part := range strings.FieldsFunc(p, func(r rune) bool { return r == '/' || r == '\\' }) {
		if part == ".." || part == "." {
			return false
		}
	}
	return true
}

// LoadProduct reads a product definition (JSON).
func LoadProduct(path string) (*Product, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return ParseProduct(b)
}

// ParseProduct decodes and validates a product definition. Unknown fields
// are errors (a typo must not silently drop a setting).
func ParseProduct(b []byte) (*Product, error) {
	var p Product
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		return nil, fmt.Errorf("product: %w", err)
	}
	if err := p.Validate(); err != nil {
		return nil, fmt.Errorf("product: %w", err)
	}
	return &p, nil
}
