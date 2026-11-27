package wininstall

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
)

// Scope is where a product is installed: for all users (Program Files,
// HKLM, needs administrator rights) or for the current user only
// (%LOCALAPPDATA%\Programs, HKCU, no service and no firewall rule).
type Scope int

const (
	PerMachine Scope = iota
	PerUser
)

func (s Scope) String() string {
	if s == PerUser {
		return "user"
	}
	return "machine"
}

// ParseScope is the inverse of String.
func ParseScope(s string) (Scope, error) {
	switch strings.ToLower(s) {
	case "", "machine", "allusers":
		return PerMachine, nil
	case "user", "currentuser":
		return PerUser, nil
	}
	return 0, fmt.Errorf("unknown scope %q", s)
}

// Folder identifiers resolved by System.Folder.
const (
	FolderProgramFiles  = "pf"            // Program Files (machine) / %LOCALAPPDATA%\Programs (user)
	FolderCommonAppData = "commonappdata" // %ProgramData%
	FolderUserAppData   = "userappdata"   // %APPDATA% (roaming) of the installing user
	FolderLocalAppData  = "localappdata"  // %LOCALAPPDATA% of the installing user
	FolderPrograms      = "programs"      // Start menu\Programs (all users / current user)
	FolderDesktop       = "desktop"       // public desktop / user desktop
	FolderTemp          = "tmp"           // the installer's temporary directory
	FolderSystem        = "sys"           // %SystemRoot%\System32
)

// builtinTokens are the names Expand resolves itself; product Vars may not
// shadow them.
var builtinTokens = map[string]bool{
	"app": true, FolderProgramFiles: true, FolderCommonAppData: true, FolderUserAppData: true,
	FolderLocalAppData: true, FolderPrograms: true, FolderDesktop: true, FolderTemp: true,
	FolderSystem: true, "port": true, "version": true, "id": true, "name": true,
}

var tokenRE = regexp.MustCompile(`\{([a-z][a-z0-9]*)\}`)

// Expander resolves {token}s. Folders holds the folder tokens for one
// scope; App the install directory; Vars the product variables (which may
// themselves contain tokens, expanded once).
type Expander struct {
	Folders map[string]string
	App     string
	Port    int
	Product *Product
}

// Expand replaces every {token}; unknown tokens are an error so a typo in a
// product definition cannot produce a path like "C:\{ap}\x".
func (e *Expander) Expand(s string) (string, error) {
	return e.expand(s, 0)
}

func (e *Expander) expand(s string, depth int) (string, error) {
	if depth > 4 {
		return "", fmt.Errorf("expand %q: variables nested too deeply", s)
	}
	var firstErr error
	out := tokenRE.ReplaceAllStringFunc(s, func(m string) string {
		name := m[1 : len(m)-1]
		v, ok := e.lookup(name, depth)
		if !ok && firstErr == nil {
			firstErr = fmt.Errorf("expand %q: unknown token {%s}", s, name)
		}
		return v
	})
	if firstErr != nil {
		return "", firstErr
	}
	return out, nil
}

func (e *Expander) lookup(name string, depth int) (string, bool) {
	switch name {
	case "app":
		return e.App, e.App != ""
	case "port":
		return fmt.Sprint(e.Port), e.Port > 0
	}
	if e.Product != nil {
		switch name {
		case "version":
			return e.Product.Version, true
		case "id":
			return e.Product.ID, true
		case "name":
			return e.Product.Name, true
		}
		if v, ok := e.Product.Vars[name]; ok {
			r, err := e.expand(v, depth+1)
			return r, err == nil
		}
	}
	v, ok := e.Folders[name]
	return v, ok && v != ""
}

// MustExpandAll expands a list, stopping at the first error.
func (e *Expander) ExpandAll(in []string) ([]string, error) {
	out := make([]string, len(in))
	for i, s := range in {
		v, err := e.Expand(s)
		if err != nil {
			return nil, err
		}
		out[i] = v
	}
	return out, nil
}

// CheckInstallDir refuses destinations that would be dangerous to install
// into or, worse, to uninstall from: drive roots, the Windows directory,
// the bare Program Files / ProgramData / profile folders, UNC roots and
// relative paths. folders are the resolved folder tokens (any scope).
func CheckInstallDir(dir string, protected []string) error {
	if dir == "" {
		return fmt.Errorf("no destination folder given")
	}
	if !isAbsWindows(dir) {
		return fmt.Errorf("the destination %q is not an absolute path (e.g. C:\\Program Files\\AirView)", dir)
	}
	if strings.ContainsAny(dir[2:], `*?"<>|`) || strings.Contains(dir[2:], ":") {
		return fmt.Errorf("the destination %q contains characters that are not allowed in a folder name", dir)
	}
	clean := cleanWin(dir)
	parts := strings.Split(strings.TrimPrefix(clean, `\\`), `\`)
	if strings.HasPrefix(clean, `\\`) {
		if len(parts) < 3 {
			return fmt.Errorf("the destination %q is a network share root", dir)
		}
	} else if len(parts) < 2 || parts[1] == "" {
		return fmt.Errorf("the destination %q is the root of a drive; choose a folder", dir)
	}
	for _, p := range protected {
		if p != "" && strings.EqualFold(cleanWin(p), clean) {
			return fmt.Errorf("the destination %q is a system folder; choose a sub-folder", dir)
		}
	}
	return nil
}

// isAbsWindows: "C:\x" or "\\server\share\x" (also accepted on other OSes
// so the checks are testable everywhere).
func isAbsWindows(p string) bool {
	if len(p) >= 3 && ((p[0] >= 'A' && p[0] <= 'Z') || (p[0] >= 'a' && p[0] <= 'z')) && p[1] == ':' && (p[2] == '\\' || p[2] == '/') {
		return true
	}
	return strings.HasPrefix(p, `\\`) && len(p) > 2
}

func cleanWin(p string) string {
	p = strings.ReplaceAll(p, "/", `\`)
	unc := strings.HasPrefix(p, `\\`)
	parts := strings.Split(p, `\`)
	var out []string
	for _, s := range parts {
		if s == "" || s == "." {
			continue
		}
		if s == ".." {
			if len(out) > 1 {
				out = out[:len(out)-1]
			}
			continue
		}
		out = append(out, s)
	}
	r := strings.Join(out, `\`)
	if unc {
		return `\\` + r
	}
	if len(out) == 1 { // "C:"
		return r + `\`
	}
	return r
}

// joinRel joins a slash-separated payload path to a directory.
func joinRel(dir, rel string) string {
	return filepath.Join(dir, filepath.FromSlash(strings.ReplaceAll(rel, `\`, "/")))
}
