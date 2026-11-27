package main

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"time"

	"airview/internal/selfupdate"
)

// readPubKey returns the trusted key from -pub or -pubfile.
func readPubKey(pubS, pubF string) (selfupdate.PublicKey, error) {
	keyText := pubS
	if pubF != "" {
		b, err := os.ReadFile(pubF)
		if err != nil {
			return nil, err
		}
		keyText = string(b)
	}
	if keyText == "" {
		return nil, usageError{errors.New("-pub or -pubfile is required")}
	}
	return selfupdate.ParsePublicKey(keyText)
}

// httpGet fetches u with a size limit.
func httpGet(ctx context.Context, c *http.Client, u string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "airview-release-check")
	req.Header.Set("Cache-Control", "no-cache")
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: HTTP %d", u, resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("GET %s: more than %d bytes", u, limit)
	}
	return b, nil
}

// cmdCheck verifies a published channel the way a client does: signature,
// channel, expiry, and (with -download) every artifact's size and SHA-256.
func cmdCheck(args []string, stdout, stderr io.Writer) error {
	fs := newFlags("check", stderr)
	pubS := fs.String("pub", "", "trusted public key (base64)")
	pubF := fs.String("pubfile", "", "file with the trusted public key")
	mURL := fs.String("url", "", "stable manifest URL (https://host/feeder/manifest.json)")
	channel := fs.String("channel", selfupdate.ChannelStable, "stable or beta")
	dl := fs.Bool("download", false, "also download every artifact and check its size and SHA-256")
	timeout := fs.Duration("timeout", 10*time.Minute, "overall time limit")
	if err := parse(fs, args); err != nil {
		return err
	}
	if *mURL == "" {
		return usageError{errors.New("-url is required")}
	}
	if !selfupdate.ValidChannel(*channel) {
		return usageError{fmt.Errorf("invalid -channel %q", *channel)}
	}
	key, err := readPubKey(*pubS, *pubF)
	if err != nil {
		return err
	}
	u, err := selfupdate.ChannelURL(*mURL, *channel)
	if err != nil {
		return usageError{err}
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	c := &http.Client{}
	body, err := httpGet(ctx, c, u, selfupdate.MaxManifestBytes)
	if err != nil {
		return err
	}
	sig, err := httpGet(ctx, c, u+".sig", 4096)
	if err != nil {
		return err
	}
	m, err := selfupdate.VerifyManifest([]selfupdate.PublicKey{key}, body, sig)
	if err != nil {
		return err
	}
	var problems []string
	if m.Channel != *channel {
		problems = append(problems, fmt.Sprintf("the manifest is for channel %q", m.Channel))
	}
	now := time.Now()
	if !m.Expires.IsZero() {
		switch left := m.Expires.Sub(now); {
		case left <= 0:
			problems = append(problems, "the manifest has expired ("+m.Expires.Format(time.RFC3339)+"): clients refuse it")
		case left < 7*24*time.Hour:
			fmt.Fprintf(stdout, "warning: the manifest expires in %v: re-publish soon\n", left.Round(time.Hour))
		}
	}
	fmt.Fprintf(stdout, "OK: %s %s (%s), published %s, signature verified\n", m.Product, m.Version, m.Channel, m.Published.Format(time.RFC3339))
	for _, f := range m.Files {
		fu, err := selfupdate.ResolveFileURL(u, f)
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s: %v", f.Target, err))
			continue
		}
		if !*dl {
			fmt.Fprintf(stdout, "  %-14s %10d  %s\n", f.Target, f.Size, fu)
			continue
		}
		sum, n, err := hashURL(ctx, c, fu, f.Size)
		switch {
		case err != nil:
			problems = append(problems, fmt.Sprintf("%s: %v", f.Target, err))
		case n != f.Size || sum != f.SHA256:
			problems = append(problems, fmt.Sprintf("%s: %s does not match the manifest (%d bytes, sha256 %s)", f.Target, fu, n, sum))
		default:
			fmt.Fprintf(stdout, "  %-14s OK  %s\n", f.Target, fu)
		}
	}
	if len(problems) > 0 {
		return errors.New(strings.Join(problems, "; "))
	}
	return nil
}

// hashURL streams u (at most size+1 bytes) through SHA-256.
func hashURL(ctx context.Context, c *http.Client, u string, size int64) (string, int64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", 0, err
	}
	req.Header.Set("User-Agent", "airview-release-check")
	resp, err := c.Do(req)
	if err != nil {
		return "", 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", 0, fmt.Errorf("GET %s: HTTP %d", u, resp.StatusCode)
	}
	h := sha256.New()
	n, err := io.Copy(h, io.LimitReader(resp.Body, size+1))
	if err != nil {
		return "", n, err
	}
	return fmt.Sprintf("%x", h.Sum(nil)), n, nil
}

// cmdServe serves a release directory with selfupdate.ReleaseHandler, for
// testing a release before copying it to the aggregator (or on a machine
// without one). Ctrl+C stops it.
func cmdServe(args []string, stdout, stderr io.Writer) error {
	fs := newFlags("serve", stderr)
	dir := fs.String("dir", "", "release directory written by publish")
	addr := fs.String("addr", "127.0.0.1:8080", "listen address")
	prefix := fs.String("prefix", "/feeder", "URL prefix (the aggregator uses /feeder)")
	if err := parse(fs, args); err != nil {
		return err
	}
	if *dir == "" {
		return usageError{errors.New("-dir is required")}
	}
	if info, err := os.Stat(*dir); err != nil || !info.IsDir() {
		return fmt.Errorf("%s is not a directory", *dir)
	}
	p := "/" + strings.Trim(*prefix, "/")
	if p == "/" {
		p = ""
	}
	mux := http.NewServeMux()
	mux.Handle(p+"/", http.StripPrefix(p, selfupdate.ReleaseHandler{Dir: *dir}))
	srv := &http.Server{Addr: *addr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	fmt.Fprintf(stdout, "serving %s at http://%s%s/manifest.json (Ctrl+C stops)\n", *dir, *addr, p)
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return srv.Shutdown(sctx)
}
