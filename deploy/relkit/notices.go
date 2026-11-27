package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

var licenceFileRe = regexp.MustCompile(`(?i)^(LICEN[CS]E|COPYING)(\.(md|txt))?$`)

func cmdNotices(args []string, stdout, stderr io.Writer) error {
	fs := newFlags("notices", stderr)
	pkg := fs.String("pkg", "", "Go package of the program (e.g. ./cmd/airview)")
	goos := fs.String("goos", "linux", "target GOOS")
	goarch := fs.String("goarch", "amd64", "target GOARCH (armv7 means arm with GOARM=7)")
	web := fs.String("web", "", "directory whose LICENSE* files are appended (web/static/vendor)")
	out := fs.String("o", "", "output file")
	crlf := fs.Bool("crlf", false, "Windows line ends")
	if err := parse(fs, args); err != nil {
		return err
	}
	if *pkg == "" || *out == "" {
		return usageError{errors.New("-pkg and -o are required")}
	}
	mods, err := linkedModules(*pkg, *goos, *goarch)
	if err != nil {
		return err
	}
	text, err := Notices(mods, *web)
	if err != nil {
		return err
	}
	if *crlf {
		text = strings.ReplaceAll(text, "\n", "\r\n")
	}
	if err := os.WriteFile(*out, []byte(text), 0o644); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "wrote %s (%d modules)\n", *out, len(mods))
	return nil
}

// module is a Go module linked into a program.
type module struct{ Path, Version, Dir string }

// linkedModules runs "go list -deps" for pkg on the target and returns the
// third-party modules (not the main module) it links, sorted by path.
func linkedModules(pkg, goos, goarch string) ([]module, error) {
	cmd := exec.Command("go", "list", "-deps", "-f", "{{with .Module}}{{if not .Main}}{{.Path}}|{{.Version}}|{{.Dir}}{{end}}{{end}}", pkg)
	env := append([]string{}, os.Environ()...)
	env = append(env, "CGO_ENABLED=0", "GOOS="+goos, "GOFLAGS=-mod=readonly")
	if goarch == "armv7" {
		env = append(env, "GOARCH=arm", "GOARM=7")
	} else {
		env = append(env, "GOARCH="+goarch)
	}
	cmd.Env = env
	var errb bytes.Buffer
	cmd.Stderr = &errb
	b, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("go list %s: %v: %s", pkg, err, strings.TrimSpace(errb.String()))
	}
	seen := map[string]bool{}
	var mods []module
	for _, l := range strings.Split(strings.ReplaceAll(string(b), "\r", ""), "\n") {
		if l == "" || seen[l] {
			continue
		}
		seen[l] = true
		p := strings.SplitN(l, "|", 3)
		if len(p) != 3 {
			continue
		}
		mods = append(mods, module{p[0], p[1], p[2]})
	}
	sort.Slice(mods, func(i, j int) bool { return mods[i].Path < mods[j].Path })
	return mods, nil
}

// Notices returns the THIRD-PARTY-NOTICES.txt text (LF line ends) for the
// modules, the Go distribution itself and, when webDir is set, the
// LICENSE* files below it.
func Notices(mods []module, webDir string) (string, error) {
	type entry struct{ name, file string }
	goroot, err := goEnv("GOROOT")
	if err != nil {
		return "", err
	}
	ents := []entry{{"The Go programming language (standard library and runtime)", filepath.Join(goroot, "LICENSE")}}
	for _, m := range mods {
		lic, err := findLicence(m.Dir)
		if err != nil {
			return "", fmt.Errorf("module %s %s: %w", m.Path, m.Version, err)
		}
		ents = append(ents, entry{m.Path + " " + m.Version, lic})
	}
	if webDir != "" {
		var files []string
		err := filepath.WalkDir(webDir, func(p string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if !d.IsDir() && strings.HasPrefix(strings.ToUpper(d.Name()), "LICENSE") {
				files = append(files, p)
			}
			return nil
		})
		if err != nil {
			return "", err
		}
		sort.Strings(files)
		for _, f := range files {
			ents = append(ents, entry{"web/static/vendor/" + filepath.Base(filepath.Dir(f)) + " (" + filepath.Base(f) + ")", f})
		}
	}
	var b strings.Builder
	b.WriteString("Third-party notices\n===================\n\n")
	b.WriteString("This program includes the following open-source components. Their licences follow.\n\n")
	rule := strings.Repeat("-", 78)
	for _, e := range ents {
		body, err := os.ReadFile(e.file)
		if err != nil {
			return "", err
		}
		text := strings.TrimRight(strings.ReplaceAll(string(body), "\r\n", "\n"), "\n \t")
		fmt.Fprintf(&b, "%s\n%s\n%s\n%s\n\n", rule, e.name, rule, text)
	}
	return b.String(), nil
}

func findLicence(dir string) (string, error) {
	if dir == "" {
		return "", errors.New("module is not in the module cache (run go mod download)")
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		return "", err
	}
	for _, e := range ents {
		if !e.IsDir() && licenceFileRe.MatchString(e.Name()) {
			return filepath.Join(dir, e.Name()), nil
		}
	}
	return "", fmt.Errorf("no licence file in %s", dir)
}

func goEnv(name string) (string, error) {
	b, err := exec.Command("go", "env", name).Output()
	if err != nil {
		return "", fmt.Errorf("go env %s: %v", name, err)
	}
	return strings.TrimSpace(string(b)), nil
}
