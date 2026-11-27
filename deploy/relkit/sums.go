package main

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// SumsName is the checksum file of a release directory.
const SumsName = "SHA256SUMS"

// defaultMatch selects the release artifacts of a directory: every AirView
// file name starts with "airview" (AirView-Setup-..., airview-pi-..., ...).
const defaultMatch = `(?i)^airview`

func cmdSums(args []string, stdout, stderr io.Writer) error {
	fs := newFlags("sums", stderr)
	dir := fs.String("dir", "", "release directory")
	out := fs.String("o", "", "output file (default DIR/SHA256SUMS)")
	match := fs.String("match", defaultMatch, "regular expression the file names must match")
	files := fs.String("files", "", "comma-separated file names to list instead of matching (the artifacts of this build)")
	if err := parse(fs, args); err != nil {
		return err
	}
	if *dir == "" {
		return usageError{errors.New("-dir is required")}
	}
	re, err := regexp.Compile(*match)
	if err != nil {
		return usageError{err}
	}
	if *out == "" {
		*out = filepath.Join(*dir, SumsName)
	}
	var names []string
	if *files != "" {
		names = splitList(*files)
		sort.Strings(names)
		for _, n := range names {
			if strings.ContainsAny(n, `/\`) || excludedFromSums(n) {
				return usageError{fmt.Errorf("-files: %q cannot be listed", n)}
			}
		}
	} else if names, err = artifactNames(*dir, re); err != nil {
		return err
	}
	if len(names) == 0 {
		return fmt.Errorf("no artifacts matching %s in %s", *match, *dir)
	}
	var b bytes.Buffer
	for _, n := range names {
		h, err := fileSHA256(filepath.Join(*dir, n))
		if err != nil {
			return err
		}
		fmt.Fprintf(&b, "%s  %s\n", h, n)
	}
	if err := os.WriteFile(*out, b.Bytes(), 0o644); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "wrote %s (%d files)\n", *out, len(names))
	return nil
}

// artifactNames lists the top-level regular files of dir that belong in
// SHA256SUMS, sorted.
func artifactNames(dir string, re *regexp.Regexp) ([]string, error) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range ents {
		n := e.Name()
		if !e.Type().IsRegular() || !re.MatchString(n) || excludedFromSums(n) {
			continue
		}
		names = append(names, n)
	}
	sort.Strings(names)
	return names, nil
}

// splitList splits a comma-separated list, dropping empty items.
func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func excludedFromSums(n string) bool {
	l := strings.ToLower(n)
	return n == SumsName || strings.HasSuffix(l, ".old") || strings.HasSuffix(l, ".sha256") ||
		strings.HasPrefix(n, ".") || strings.HasSuffix(l, ".tmp")
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// parseSums reads a SHA256SUMS file ("<hex>  <name>" or "<hex> *<name>").
func parseSums(r io.Reader) (map[string]string, error) {
	out := map[string]string{}
	sc := bufio.NewScanner(r)
	line := 0
	for sc.Scan() {
		line++
		s := sc.Text()
		if strings.HasSuffix(s, "\r") {
			return nil, fmt.Errorf("line %d: CRLF line end (sha256sum -c would fail)", line)
		}
		if s == "" {
			continue
		}
		h, name, ok := strings.Cut(s, " ")
		if !ok || len(h) != 64 || (!strings.HasPrefix(name, " ") && !strings.HasPrefix(name, "*")) {
			return nil, fmt.Errorf("line %d: not \"<sha256>  <name>\": %q", line, s)
		}
		if _, err := hex.DecodeString(h); err != nil {
			return nil, fmt.Errorf("line %d: %v", line, err)
		}
		name = name[1:]
		if name == "" || strings.ContainsAny(name, `/\`) {
			return nil, fmt.Errorf("line %d: bad file name %q", line, name)
		}
		if _, dup := out[name]; dup {
			return nil, fmt.Errorf("line %d: %s listed twice", line, name)
		}
		out[name] = strings.ToLower(h)
	}
	return out, sc.Err()
}
