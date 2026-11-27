package wininstall

import (
	"fmt"
	"strconv"
	"strings"
)

// Args is the installer / uninstaller command line. Switches follow the
// NSIS / Inno Setup conventions administrators already script against:
//
//	/S (/SILENT, /VERYSILENT)   no user interface
//	/D=<dir>                    destination; must be last, may contain spaces, no quotes
//	/ALLUSERS | /CURRENTUSER    per-machine (default) or per-user install
//	/DESKTOP | /NODESKTOP       desktop shortcut
//	/STARTMENU | /NOSTARTMENU   Start-menu shortcuts
//	/FIREWALL | /NOFIREWALL     inbound firewall rule for the HTTP port
//	/PORT=<n>                   port of that rule
//	/SERVICE | /NOSERVICE       Windows service (products that have one)
//	/TRAY | /NOTRAY             tray icon at log-on (products that have one)
//	/NOLAUNCH                   do not offer to start the program at the end
//	/LOG=<file>                 write the setup log there (default %TEMP%\<id>-setup.log)
//	/UNINSTALL                  uninstall (implied for uninstall.exe)
//	/REMOVEDATA | /KEEPDATA     uninstall: delete or keep the data folders (default keep)
//	/SHOTS=<dir>                render every wizard page to PNG files and exit (documentation)
//	/NAME=value, /FLAG          product configuration switches (e.g. the feeder's /SERVER=)
//
// '-' may be used instead of '/', and switches are case-insensitive.
type Args struct {
	Silent     bool
	Dir        string
	Scope      Scope
	ScopeSet   bool
	Desktop    *bool
	StartMenu  *bool
	Firewall   *bool
	Service    *bool
	Tray       *bool
	Port       int
	NoLaunch   bool
	Log        string
	Uninstall  bool
	RemoveData bool
	Shots      string
	Help       bool
	// Elevated marks the copy the stub relaunched with "runas" (internal
	// /ELEVATED switch, first on the command line): when that copy still
	// lacks administrator rights it fails instead of relaunching again.
	Elevated bool
	// Params are the remaining /NAME=value (and bare /FLAG, value "")
	// switches, upper-cased names, for the product's Configure step.
	Params map[string]string
}

// ParseArgs parses the arguments after the program name.
func ParseArgs(args []string) (*Args, error) {
	a := &Args{Params: map[string]string{}}
	t, f := true, false
	for i := 0; i < len(args); i++ {
		raw := strings.TrimSpace(args[i])
		if raw == "" {
			continue
		}
		if raw[0] != '/' && raw[0] != '-' {
			return nil, fmt.Errorf("unexpected argument %q (switches start with /)", raw)
		}
		body := raw[1:]
		name, val, hasVal := strings.Cut(body, "=")
		name = strings.ToUpper(name)
		if name == "D" && hasVal {
			// NSIS: /D= is last and takes the rest of the line verbatim.
			a.Dir = strings.Trim(strings.Join(append([]string{val}, args[i+1:]...), " "), `" `)
			break
		}
		needVal := func() (string, error) {
			if !hasVal || val == "" {
				return "", fmt.Errorf("/%s needs a value (/%s=...)", name, name)
			}
			return val, nil
		}
		var err error
		switch name {
		case "S", "SILENT", "VERYSILENT", "Q", "QUIET":
			a.Silent = true
		case "ALLUSERS":
			a.Scope, a.ScopeSet = PerMachine, true
		case "CURRENTUSER":
			a.Scope, a.ScopeSet = PerUser, true
		case "DESKTOP":
			a.Desktop = &t
		case "NODESKTOP":
			a.Desktop = &f
		case "STARTMENU":
			a.StartMenu = &t
		case "NOSTARTMENU":
			a.StartMenu = &f
		case "FIREWALL":
			a.Firewall = &t
		case "NOFIREWALL":
			a.Firewall = &f
		case "SERVICE":
			a.Service = &t
		case "NOSERVICE":
			a.Service = &f
		case "TRAY":
			a.Tray = &t
		case "NOTRAY":
			a.Tray = &f
		case "NOLAUNCH", "NORUN":
			a.NoLaunch = true
		case "UNINSTALL":
			a.Uninstall = true
		case "REMOVEDATA", "PURGE":
			a.RemoveData = true
		case "KEEPDATA":
			a.RemoveData = false
		case "?", "H", "HELP":
			a.Help = true
		case elevatedSwitch:
			a.Elevated = true
		case "PORT":
			var v string
			if v, err = needVal(); err == nil {
				a.Port, err = strconv.Atoi(v)
				if err != nil || a.Port < 1 || a.Port > 65535 {
					err = fmt.Errorf("/PORT=%s: a TCP port 1-65535", v)
				}
			}
		case "LOG":
			a.Log, err = needVal()
		case "SHOTS":
			a.Shots, err = needVal()
		default:
			if !switchRE.MatchString(name) {
				return nil, fmt.Errorf("unknown switch %q", raw)
			}
			if strings.ContainsAny(val, "\r\n\x00") {
				return nil, fmt.Errorf("/%s: line breaks are not allowed", name)
			}
			a.Params[name] = val
		}
		if err != nil {
			return nil, err
		}
	}
	return a, nil
}

// reservedSwitches may not be used as product configuration switches.
var reservedSwitches = map[string]bool{
	"S": true, "SILENT": true, "VERYSILENT": true, "Q": true, "QUIET": true, "D": true,
	"ALLUSERS": true, "CURRENTUSER": true, "DESKTOP": true, "NODESKTOP": true,
	"STARTMENU": true, "NOSTARTMENU": true, "FIREWALL": true, "NOFIREWALL": true,
	"SERVICE": true, "NOSERVICE": true, "TRAY": true, "NOTRAY": true, "NOLAUNCH": true,
	"NORUN": true, "UNINSTALL": true, "REMOVEDATA": true, "PURGE": true, "KEEPDATA": true,
	"PORT": true, "LOG": true, "SHOTS": true, "H": true, "HELP": true, elevatedSwitch: true,
}

// elevatedSwitch is the internal switch of a relaunched, elevated copy.
const elevatedSwitch = "ELEVATED"

// Elevation is what the stub does before a per-machine install or
// uninstall (DLV-06: the release stub is as-invoker).
type Elevation int

const (
	// ElevationNone: go ahead (per-user scope, or already elevated).
	ElevationNone Elevation = iota
	// ElevationRelaunch: run the stub again with "runas" (UAC prompt),
	// with ElevatedArgs, and return its exit code.
	ElevationRelaunch
	// ElevationFail: the relaunched copy still has no administrator
	// rights; fail with ExitNeedsAdmin rather than loop.
	ElevationFail
)

// NeedElevation decides what to do for a scope.
func NeedElevation(scope Scope, elevated bool, a *Args) Elevation {
	switch {
	case scope != PerMachine || elevated:
		return ElevationNone
	case a != nil && a.Elevated:
		return ElevationFail
	}
	return ElevationRelaunch
}

// ElevatedArgs is the command line of the elevated copy: /ELEVATED first
// (so an NSIS-style /D=, which takes the rest of the line, stays last).
func ElevatedArgs(args []string) []string {
	out := []string{"/" + elevatedSwitch}
	for _, a := range args {
		if strings.EqualFold(strings.TrimLeft(strings.TrimSpace(a), "/-"), elevatedSwitch) {
			continue
		}
		out = append(out, a)
	}
	return out
}

// Usage is the text shown for /? (a message box in the GUI build).
func Usage(p *Product, uninstaller bool) string {
	name := "the program"
	if p != nil {
		name = p.Name
	}
	if uninstaller {
		return "Removes " + name + ".\n\n" +
			"uninstall.exe [/S] [/REMOVEDATA] [/LOG=file]\n\n" +
			"/S            silent: no questions\n" +
			"/REMOVEDATA   also delete the configuration and data folders (default: keep them)\n" +
			"/LOG=file     write the log to this file\n\n" +
			"Exit codes: 0 done, 1 failed, 2 cancelled, 3010 done (restart to finish)."
	}
	s := "Installs " + name + ".\n\n" +
		"Setup.exe [/S] [/ALLUSERS | /CURRENTUSER] [/NODESKTOP] [/NOSTARTMENU]\n" +
		"          [/FIREWALL [/PORT=n]] [/NOLAUNCH] [/LOG=file] [/D=folder]\n"
	if p != nil && p.Service != nil {
		s += "          [/NOSERVICE]"
	}
	if p != nil && p.Tray != nil {
		s += " [/NOTRAY]"
	}
	if p != nil && p.Configure != nil {
		s += "\n          [/" + p.Configure.Trigger + "=..."
		for _, prm := range p.Configure.Params {
			if prm.Switch == p.Configure.Trigger {
				continue
			}
			if prm.Bool {
				s += " /" + prm.Switch
			} else {
				s += " /" + prm.Switch + "=..."
			}
		}
		s += "]"
	}
	s += "\n\n/S             silent install with the default options\n" +
		"/D=folder      destination (last, no quotes, spaces allowed)\n" +
		"/CURRENTUSER   install for the current user only (no service, no firewall rule)\n" +
		"/LOG=file      write the setup log to this file\n\n" +
		"Exit codes: 0 done, 1 failed, 2 cancelled, 3 needs administrator rights,\n" +
		"4 bad command line, 3010 done (restart to finish)."
	return s
}

// Exit codes of the installer and the uninstaller.
const (
	ExitOK             = 0
	ExitFailed         = 1
	ExitCancelled      = 2
	ExitNeedsAdmin     = 3
	ExitUsage          = 4
	ExitRebootRequired = 3010 // like msiexec: success, restart to finish
)

// RedactCommandLine masks secret values in an installer command line for
// the setup log (SEC-R10): "/ENROL=tok" and "-enrol=tok" become
// "/ENROL=***", and a value following a bare secret flag ("-enrol tok")
// is masked too. Secret names contain TOKEN, ENROL, ENROLL, PASSWORD or
// SECRET (case-insensitive).
func RedactCommandLine(args []string) []string {
	secret := func(name string) bool {
		n := strings.ToLower(strings.TrimLeft(name, "/-"))
		return strings.Contains(n, "token") || strings.Contains(n, "enrol") || strings.Contains(n, "password") || strings.Contains(n, "secret")
	}
	out := append([]string(nil), args...)
	for i := 0; i < len(out); i++ {
		a := out[i]
		if a == "" || (a[0] != '/' && a[0] != '-') {
			continue
		}
		if name, _, ok := strings.Cut(a, "="); ok {
			if secret(name) {
				out[i] = name + "=***"
			}
			continue
		}
		if secret(a) && i+1 < len(out) {
			out[i+1] = "***"
			i++
		}
	}
	return out
}
