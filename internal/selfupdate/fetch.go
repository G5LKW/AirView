package selfupdate

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"time"
)

// fetchLimited GETs u and returns at most limit bytes (error if larger).
func fetchLimited(ctx context.Context, c *http.Client, u, userAgent string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Cache-Control", "no-cache")
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: HTTP %d", redactURL(u), resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", redactURL(u), err)
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("GET %s: response larger than %d bytes", redactURL(u), limit)
	}
	return b, nil
}

// redactURL drops the query (it may carry a token) from log/error text.
func redactURL(s string) string {
	u, err := url.Parse(s)
	if err != nil {
		return "(bad url)"
	}
	u.RawQuery, u.Fragment, u.User = "", "", nil
	return u.String()
}

// ResolveFileURL returns the absolute download URL of f relative to the
// manifest URL (see File.URL).
func ResolveFileURL(manifestURL string, f File) (string, error) {
	base, err := url.Parse(manifestURL)
	if err != nil {
		return "", err
	}
	ref := f.URL
	if ref == "" {
		ref = "dl/" + f.Target
	}
	r, err := url.Parse(ref)
	if err != nil {
		return "", fmt.Errorf("file url %q: %w", ref, err)
	}
	abs := base.ResolveReference(r)
	if abs.Scheme != "https" && abs.Scheme != "http" {
		return "", fmt.Errorf("file url %q: scheme %q not allowed", ref, abs.Scheme)
	}
	return abs.String(), nil
}

// download streams u into a new file in dir, hashing it on the way, and
// checks size and SHA-256 against f. On success the returned path holds the
// verified artifact (mode 0755); on any error nothing is left behind.
func download(ctx context.Context, c *http.Client, u, userAgent string, f File, dir string, maxBytes int64) (string, error) {
	if f.Size > maxBytes {
		return "", fmt.Errorf("artifact is %d bytes, more than the %d byte limit", f.Size, maxBytes)
	}
	want, err := f.SHA256Bytes()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", userAgent)
	resp, err := c.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("GET %s: HTTP %d", redactURL(u), resp.StatusCode)
	}
	tmp, err := os.CreateTemp(dir, "download-*.part")
	if err != nil {
		return "", err
	}
	ok := false
	defer func() {
		if !ok {
			tmp.Close()
			os.Remove(tmp.Name())
		}
	}()
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, h), io.LimitReader(resp.Body, f.Size+1))
	if err != nil {
		return "", fmt.Errorf("GET %s: %w", redactURL(u), err)
	}
	if n != f.Size {
		return "", fmt.Errorf("download size mismatch: got %d bytes, the manifest says %d", n, f.Size)
	}
	if got := h.Sum(nil); !bytes.Equal(got, want) {
		return "", fmt.Errorf("SHA-256 mismatch: got %x, the manifest says %s", got, f.SHA256)
	}
	if err := tmp.Sync(); err != nil {
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	if err := os.Chmod(tmp.Name(), 0o755); err != nil {
		return "", err
	}
	ok = true
	return tmp.Name(), nil
}

// fileSHA256 hashes a file.
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
	return fmt.Sprintf("%x", h.Sum(nil)), nil
}

// checkExecutableFormat makes sure an artifact is an executable for the
// operating system of target (ELF for linux/freebsd, PE for windows,
// Mach-O for darwin). This catches a manifest entry that points at the
// wrong file before anything is swapped.
func checkExecutableFormat(path, target string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	var hdr [4]byte
	if _, err := io.ReadFull(f, hdr[:]); err != nil {
		return errors.New("artifact is too short to be an executable")
	}
	osName := target
	if i := bytes.IndexByte([]byte(target), '-'); i >= 0 {
		osName = target[:i]
	}
	switch osName {
	case "linux", "freebsd":
		if string(hdr[:]) != "\x7fELF" {
			return errors.New("artifact is not an ELF executable")
		}
	case "windows":
		if string(hdr[:2]) != "MZ" {
			return errors.New("artifact is not a Windows (PE) executable")
		}
	case "darwin":
		switch string(hdr[:]) {
		case "\xcf\xfa\xed\xfe", "\xce\xfa\xed\xfe", "\xca\xfe\xba\xbe":
		default:
			return errors.New("artifact is not a Mach-O executable")
		}
	}
	return nil
}

// cleanStaging removes leftover partial downloads older than an hour.
func cleanStaging(dir string, now time.Time) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range ents {
		if e.IsDir() {
			continue
		}
		if m, _ := filepath.Match("download-*.part", e.Name()); !m {
			continue
		}
		if info, err := e.Info(); err == nil && now.Sub(info.ModTime()) > time.Hour {
			os.Remove(filepath.Join(dir, e.Name()))
		}
	}
}
