package main

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"debug/elf"
	"debug/pe"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"text/tabwriter"
)

func cmdCheck(args []string, stdout, stderr io.Writer) error {
	fs := newFlags("check", stderr)
	dir := fs.String("dir", "", "release directory")
	version := fs.String("version", "", "expected version (VERSION files in bundles)")
	require := fs.String("require", "", "comma-separated file names that must exist")
	sums := fs.Bool("sums", true, "verify SHA256SUMS")
	if err := parse(fs, args); err != nil {
		return err
	}
	if *dir == "" {
		return usageError{errors.New("-dir is required")}
	}
	req := splitList(*require)
	rep, err := Check(*dir, CheckOptions{Version: *version, Require: req, Sums: *sums})
	if err != nil {
		return err
	}
	rep.Print(stdout)
	if len(rep.Problems) > 0 {
		return fmt.Errorf("%d problem(s) in %s", len(rep.Problems), *dir)
	}
	return nil
}

// CheckOptions selects what Check verifies.
type CheckOptions struct {
	Version string   // expected content of VERSION files ("" = not checked)
	Require []string // names that must exist in the directory
	Sums    bool     // verify SHA256SUMS
}

// Artifact is one checked file.
type Artifact struct {
	Name   string
	Size   int64
	Kind   string
	Detail string
}

// Report is the result of Check.
type Report struct {
	Dir       string
	Artifacts []Artifact
	Problems  []string
}

func (r *Report) problem(format string, a ...any) {
	r.Problems = append(r.Problems, fmt.Sprintf(format, a...))
}

// Print writes the artifact table and the problems.
func (r *Report) Print(w io.Writer) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintf(tw, "ARTIFACT\tMB\tKIND\tCHECKED\n")
	for _, a := range r.Artifacts {
		fmt.Fprintf(tw, "%s\t%.2f\t%s\t%s\n", a.Name, float64(a.Size)/(1<<20), a.Kind, a.Detail)
	}
	tw.Flush()
	if len(r.Problems) == 0 {
		fmt.Fprintf(w, "OK: %d artifact(s) in %s\n", len(r.Artifacts), r.Dir)
		return
	}
	sort.Strings(r.Problems)
	fmt.Fprintf(w, "PROBLEMS (%d):\n", len(r.Problems))
	for _, p := range r.Problems {
		fmt.Fprintf(w, "  - %s\n", p)
	}
}

// Check verifies the release directory dir (see doc.go). An error is
// returned only when the directory cannot be read; findings are in
// Report.Problems.
func Check(dir string, o CheckOptions) (*Report, error) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	rep := &Report{Dir: dir}
	have := map[string]bool{}
	for _, e := range ents {
		have[e.Name()] = true
	}
	for _, r := range o.Require {
		if !have[r] {
			rep.problem("%s: required artifact missing", r)
		}
	}
	match := regexp.MustCompile(defaultMatch)
	required := map[string]bool{}
	for _, r := range o.Require {
		required[r] = true
	}
	for _, e := range ents {
		n := e.Name()
		if !e.Type().IsRegular() || !(match.MatchString(n) || n == SumsName) || strings.HasSuffix(strings.ToLower(n), ".old") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			rep.problem("%s: %v", n, err)
			continue
		}
		a := Artifact{Name: n, Size: info.Size()}
		p := filepath.Join(dir, n)
		l := strings.ToLower(n)
		switch {
		case strings.HasSuffix(l, ".sha256"):
			a.Kind, a.Detail = "checksum", checkSidecar(rep, dir, n)
		case len(required) > 0 && !required[n] && n != SumsName:
			a.Kind, a.Detail = "extra", "not part of this build (left from an earlier one; not checked)"
		case n == SumsName:
			a.Kind, a.Detail = "checksums", checkSums(rep, dir, match, o.Sums, o.Require)
		case strings.HasSuffix(l, ".tar.gz"):
			a.Kind, a.Detail = "tar.gz", checkTarGz(rep, p, o.Version)
		case strings.HasSuffix(l, ".zip"):
			a.Kind, a.Detail = "zip", checkZip(rep, p)
		case strings.HasSuffix(l, ".exe"):
			a.Kind, a.Detail = "windows exe", checkPEFile(rep, p)
		case strings.HasSuffix(l, ".sh"):
			a.Kind, a.Detail = "script", checkScriptFile(rep, p)
		case strings.HasSuffix(l, ".img"):
			a.Kind, a.Detail = "disk image", "size only"
		default:
			a.Kind, a.Detail = classifyFile(rep, p)
		}
		rep.Artifacts = append(rep.Artifacts, a)
	}
	if !have[SumsName] && o.Sums {
		rep.problem("%s missing", SumsName)
	}
	return rep, nil
}

// checkSums verifies SHA256SUMS. With a require list, exactly those names
// must be listed (files of earlier builds in the same directory are not this
// release); without one, every artifact name in the directory must be.
func checkSums(rep *Report, dir string, match *regexp.Regexp, verify bool, require []string) string {
	if !verify {
		return "not verified"
	}
	f, err := os.Open(filepath.Join(dir, SumsName))
	if err != nil {
		rep.problem("%s: %v", SumsName, err)
		return "unreadable"
	}
	defer f.Close()
	sums, err := parseSums(f)
	if err != nil {
		rep.problem("%s: %v", SumsName, err)
		return "malformed"
	}
	names := require
	if len(names) == 0 {
		if names, err = artifactNames(dir, match); err != nil {
			rep.problem("%s: %v", SumsName, err)
			return "unreadable"
		}
	}
	for _, n := range names {
		if _, ok := sums[n]; !ok {
			rep.problem("%s: %s is not listed", SumsName, n)
		}
	}
	bad := 0
	for n, want := range sums {
		got, err := fileSHA256(filepath.Join(dir, n))
		switch {
		case err != nil:
			rep.problem("%s: %s: %v", SumsName, n, err)
			bad++
		case got != want:
			rep.problem("%s: %s does not match (file %s, listed %s)", SumsName, n, got, want)
			bad++
		}
	}
	return fmt.Sprintf("%d entries, %d mismatched", len(sums), bad)
}

var archRe = regexp.MustCompile(`(?:^|[-_.])(amd64|x86_64|arm64|aarch64|armv7|armhf)(?:$|[-_.])`)

// archOf returns the CPU named in a file name ("" when none).
func archOf(name string) string {
	m := archRe.FindStringSubmatch(strings.ToLower(name))
	if m == nil {
		return ""
	}
	switch m[1] {
	case "x86_64":
		return "amd64"
	case "aarch64":
		return "arm64"
	case "armhf":
		return "armv7"
	}
	return m[1]
}

var elfMachine = map[string]elf.Machine{"amd64": elf.EM_X86_64, "arm64": elf.EM_AARCH64, "armv7": elf.EM_ARM}

// checkELF verifies a Linux binary: the CPU of its name and static linking.
func checkELF(rep *Report, where string, b []byte, arch string) string {
	f, err := elf.NewFile(bytes.NewReader(b))
	if err != nil {
		rep.problem("%s: not a valid ELF file: %v", where, err)
		return "bad ELF"
	}
	for _, p := range f.Progs {
		if p.Type == elf.PT_INTERP {
			rep.problem("%s: dynamically linked (has an interpreter); release binaries must be static", where)
		}
	}
	if arch == "" {
		rep.problem("%s: CPU architecture not named in the file or archive name", where)
		return f.Machine.String()
	}
	if want := elfMachine[arch]; f.Machine != want {
		rep.problem("%s: ELF machine %v, want %v (%s)", where, f.Machine, want, arch)
	}
	if arch == "armv7" && f.Class != elf.ELFCLASS32 {
		rep.problem("%s: armv7 binary is not 32-bit", where)
	}
	return arch
}

func checkTarGz(rep *Report, p, version string) string {
	name := filepath.Base(p)
	top := strings.TrimSuffix(name, ".tar.gz")
	f, err := os.Open(p)
	if err != nil {
		rep.problem("%s: %v", name, err)
		return "unreadable"
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		rep.problem("%s: %v", name, err)
		return "not gzip"
	}
	tr := tar.NewReader(gz)
	var files, dirs, execs, elfs int
	sawTop := false
	notices := map[string]bool{}
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			rep.problem("%s: %v", name, err)
			return "damaged"
		}
		en := strings.TrimSuffix(h.Name, "/")
		where := name + ": " + en
		if path.IsAbs(en) || strings.Contains("/"+en+"/", "/../") || strings.Contains(en, `\`) {
			rep.problem("%s: unsafe path", where)
			continue
		}
		if en != top && !strings.HasPrefix(en, top+"/") {
			rep.problem("%s: outside the top-level directory %s/", where, top)
		}
		if h.Uid != 0 || h.Gid != 0 || h.Uname != "root" || h.Gname != "root" {
			rep.problem("%s: owner %d:%d (%s:%s), want root", where, h.Uid, h.Gid, h.Uname, h.Gname)
		}
		mode := h.Mode & 0o7777
		switch h.Typeflag {
		case tar.TypeDir:
			dirs++
			if en == top {
				sawTop = true
			}
			if mode != modeDir {
				rep.problem("%s: directory mode %04o, want %04o", where, mode, modeDir)
			}
			continue
		case tar.TypeReg:
		default:
			rep.problem("%s: entry type %q (only files and directories)", where, h.Typeflag)
			continue
		}
		files++
		b, err := io.ReadAll(tr)
		if err != nil {
			rep.problem("%s: %v", where, err)
			return "damaged"
		}
		exec := isExecContent(b)
		want := int64(modeFile)
		if exec {
			want = modeExec
			execs++
		}
		if mode != want {
			rep.problem("%s: mode %04o, want %04o", where, mode, want)
		}
		base := path.Base(en)
		if en == top+"/"+base && noticeFiles[base] {
			notices[base] = len(bytes.TrimSpace(b)) > 0
		}
		switch {
		case bytes.HasPrefix(b, []byte("\x7fELF")):
			elfs++
			arch := archOf(base)
			if arch == "" {
				arch = archOf(top)
			}
			checkELF(rep, where, b, arch)
		case bytes.HasPrefix(b, []byte("#!")):
			if bytes.Contains(b, []byte("\r\n")) {
				rep.problem("%s: script has CRLF line ends", where)
			}
		case strings.HasSuffix(base, ".sh"):
			// A library sourced by the other scripts (deploy/server/lib.sh): not
			// executable (0644 above), but sh still needs LF line ends.
			if bytes.Contains(b, []byte("\r\n")) {
				rep.problem("%s: script has CRLF line ends", where)
			}
		}
		if base == "VERSION" && en == top+"/VERSION" && version != "" {
			if got := strings.TrimSpace(string(b)); got != version {
				rep.problem("%s: version %q, want %q", where, got, version)
			}
		}
	}
	if !sawTop {
		rep.problem("%s: no top-level directory entry %s/", name, top)
	}
	if elfs > 0 {
		requireNotices(rep, name, top+"/", notices)
	}
	return fmt.Sprintf("%d files (%d exec, %d ELF), %d dirs, root-owned, modes ok", files, execs, elfs, dirs)
}

func checkZip(rep *Report, p string) string {
	name := filepath.Base(p)
	zr, err := zip.OpenReader(p)
	if err != nil {
		rep.problem("%s: %v", name, err)
		return "unreadable"
	}
	defer zr.Close()
	arch := archOf(name)
	var n, exes int
	var total int64
	notices := map[string]bool{}
	for _, zf := range zr.File {
		where := name + ": " + zf.Name
		if strings.Contains(zf.Name, `\`) || path.IsAbs(zf.Name) || strings.Contains("/"+zf.Name+"/", "/../") {
			rep.problem("%s: unsafe path", where)
		}
		if zf.FileInfo().IsDir() {
			continue
		}
		rc, err := zf.Open()
		if err != nil {
			rep.problem("%s: %v", where, err)
			continue
		}
		var head bytes.Buffer
		isExe := strings.HasSuffix(strings.ToLower(zf.Name), ".exe")
		var w io.Writer = io.Discard
		if isExe {
			w = &head
		}
		k, err := io.Copy(w, rc) // the reader checks the CRC-32 at EOF
		rc.Close()
		if err != nil {
			rep.problem("%s: %v", where, err)
			continue
		}
		if isExe {
			checkPE(rep, where, head.Bytes(), peArch(name, arch))
			exes++
		}
		// At the top or in the single top-level folder of the ZIP.
		if b := path.Base(zf.Name); noticeFiles[b] && strings.Count(strings.Trim(zf.Name, "/"), "/") <= 1 {
			notices[b] = k > 0
		}
		n++
		total += k
	}
	if exes > 0 {
		requireNotices(rep, name, "", notices)
	}
	return fmt.Sprintf("%d files, %.1f MB unpacked, CRC ok", n, float64(total)/(1<<20))
}

// noticeFiles must accompany the programs in every archive that holds one
// (the Go standard library's BSD licence, DLV-05).
var noticeFiles = map[string]bool{"LICENSE.txt": true, "THIRD-PARTY-NOTICES.txt": true}

func requireNotices(rep *Report, name, dir string, seen map[string]bool) {
	for _, f := range []string{"LICENSE.txt", "THIRD-PARTY-NOTICES.txt"} {
		if !seen[f] {
			rep.problem("%s: programs without %s%s (licence notices must ship with the binaries)", name, dir, f)
		}
	}
}

// peArch is the CPU expected for a Windows executable: named in the file
// (or its container), else amd64.
func peArch(name, container string) string {
	if a := archOf(name); a != "" {
		return a
	}
	if container != "" {
		return container
	}
	return "amd64"
}

var peMachine = map[string]uint16{"amd64": pe.IMAGE_FILE_MACHINE_AMD64, "arm64": pe.IMAGE_FILE_MACHINE_ARM64}

func checkPE(rep *Report, where string, b []byte, arch string) string {
	f, err := pe.NewFile(bytes.NewReader(b))
	if err != nil {
		rep.problem("%s: not a valid PE file: %v", where, err)
		return "bad PE"
	}
	defer f.Close()
	want, ok := peMachine[arch]
	if !ok {
		rep.problem("%s: no Windows build for %s", where, arch)
		return arch
	}
	if f.Machine != want {
		rep.problem("%s: PE machine %#x, want %#x (%s)", where, f.Machine, want, arch)
	}
	return "PE " + arch
}

func checkPEFile(rep *Report, p string) string {
	b, err := os.ReadFile(p)
	if err != nil {
		rep.problem("%s: %v", filepath.Base(p), err)
		return "unreadable"
	}
	return checkPE(rep, filepath.Base(p), b, peArch(filepath.Base(p), ""))
}

// checkSidecar verifies NAME.sha256 ("<hex>  NAME" or a bare hash) against
// NAME in the same directory.
func checkSidecar(rep *Report, dir, n string) string {
	b, err := os.ReadFile(filepath.Join(dir, n))
	if err != nil {
		rep.problem("%s: %v", n, err)
		return "unreadable"
	}
	target := n[:len(n)-len(".sha256")]
	fields := strings.Fields(string(b))
	if len(fields) == 0 {
		rep.problem("%s: empty", n)
		return "empty"
	}
	got, err := fileSHA256(filepath.Join(dir, target))
	if err != nil {
		rep.problem("%s: %s: %v", n, target, err)
		return "target missing"
	}
	if !strings.EqualFold(fields[0], got) {
		rep.problem("%s: does not match %s", n, target)
		return "mismatch"
	}
	return "matches " + target
}

func checkScriptFile(rep *Report, p string) string {
	b, err := os.ReadFile(p)
	if err != nil {
		rep.problem("%s: %v", filepath.Base(p), err)
		return "unreadable"
	}
	if !bytes.HasPrefix(b, []byte("#!")) {
		rep.problem("%s: no #! line", filepath.Base(p))
	}
	if bytes.Contains(b, []byte("\r")) {
		rep.problem("%s: CR characters (sh needs LF line ends)", filepath.Base(p))
	}
	return "#! line, LF"
}

// classifyFile checks a file without a known extension: a Linux binary
// (airview-linux-arm64 ...) or text (verify reports, notes).
func classifyFile(rep *Report, p string) (string, string) {
	head, err := readHead(p, 4)
	if err != nil {
		rep.problem("%s: %v", filepath.Base(p), err)
		return "?", "unreadable"
	}
	if bytes.HasPrefix(head, []byte("\x7fELF")) {
		b, err := os.ReadFile(p)
		if err != nil {
			rep.problem("%s: %v", filepath.Base(p), err)
			return "linux binary", "unreadable"
		}
		return "linux binary", checkELF(rep, filepath.Base(p), b, archOf(filepath.Base(p))) + ", static"
	}
	return "file", "present"
}
