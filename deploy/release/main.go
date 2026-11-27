// Command release is AirView's release signing tool (DESIGN §12, WP-UP).
// It creates the Ed25519 release key, publishes signed releases of
// airview and airview-feeder into a release directory that an aggregator
// (internal/selfupdate.ReleaseHandler) or any static web server serves,
// and verifies manifests.
//
//	go run ./deploy/release keygen  -out DIR [-name airview-release]
//	go run ./deploy/release pubkey  -key FILE.key
//	go run ./deploy/release publish -key FILE.key -product airview-feeder -version 1.2.3
//	                                [-channel stable|beta] -dist DIR | -file target=PATH...
//	                                -out RELEASEDIR [-notes TEXT] [-expires-days N] [-url-prefix dl/]
//	go run ./deploy/release sign    -key FILE.key -in manifest.json [-out manifest.json.sig] [-raw]
//	go run ./deploy/release verify  -pub BASE64|-pubfile FILE.pub -in manifest.json [-sig FILE]
//	                                [-dir RELEASEDIR]   (also checks every artifact's size and SHA-256)
//	go run ./deploy/release check   -pub BASE64|-pubfile FILE.pub -url MANIFEST-URL [-channel beta] [-download]
//	go run ./deploy/release serve   -dir RELEASEDIR [-addr 127.0.0.1:8080] [-prefix /feeder]
//
// See deploy/release/README.md. The private key never leaves the release
// machine; only the .pub value goes into configurations and installers.
package main

import (
	"crypto/ed25519"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"airview/internal/selfupdate"
)

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		usage(stderr)
		return 2
	}
	cmds := map[string]func([]string, io.Writer, io.Writer) error{
		"keygen": cmdKeygen, "pubkey": cmdPubkey, "publish": cmdPublish, "sign": cmdSign, "verify": cmdVerify,
		"check": cmdCheck, "serve": cmdServe,
	}
	if args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		usage(stdout)
		return 0
	}
	c, ok := cmds[args[0]]
	if !ok {
		fmt.Fprintf(stderr, "release: unknown command %q\n", args[0])
		usage(stderr)
		return 2
	}
	if err := c(args[1:], stdout, stderr); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		var ue usageError
		if errors.As(err, &ue) {
			fmt.Fprintf(stderr, "release %s: %v\n", args[0], err)
			return 2
		}
		fmt.Fprintf(stderr, "release %s: %v\n", args[0], err)
		return 1
	}
	return 0
}

type usageError struct{ error }

func usage(w io.Writer) {
	fmt.Fprint(w, `AirView release tool

  keygen  -out DIR [-name airview-release]     create a signing key pair (NAME.key 0600, NAME.pub)
  pubkey  -key FILE.key                        print the public key (base64)
  publish -key FILE.key -product P -version V [-channel stable|beta]
          (-dist DIR | -file target=PATH ...) -out RELEASEDIR [-notes T] [-expires-days N]
                                               copy artifacts, write and sign manifest.json
  sign    -key FILE.key -in manifest.json [-out manifest.json.sig] [-raw: any file]
  verify  (-pub B64 | -pubfile FILE.pub) -in manifest.json [-sig FILE] [-dir RELEASEDIR]
  check   (-pub B64 | -pubfile FILE.pub) -url https://host/feeder/manifest.json [-channel beta]
          [-download]                          verify a published channel as a client sees it
  serve   -dir RELEASEDIR [-addr 127.0.0.1:8080] [-prefix /feeder]
                                               serve a release directory over HTTP (testing)
`)
}

func newFlags(name string, stderr io.Writer) *flag.FlagSet {
	fs := flag.NewFlagSet("release "+name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	return fs
}

func parse(fs *flag.FlagSet, args []string) error {
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return err
		}
		return usageError{err}
	}
	if fs.NArg() > 0 {
		return usageError{fmt.Errorf("unexpected argument %q", fs.Arg(0))}
	}
	return nil
}

func cmdKeygen(args []string, stdout, stderr io.Writer) error {
	fs := newFlags("keygen", stderr)
	out := fs.String("out", "", "directory for the key files (created 0700)")
	name := fs.String("name", "airview-release", "base name of the key files")
	if err := parse(fs, args); err != nil {
		return err
	}
	if *out == "" {
		return usageError{errors.New("-out is required")}
	}
	if strings.ContainsAny(*name, `/\`) || *name == "" {
		return usageError{errors.New("-name must be a plain file name")}
	}
	if err := os.MkdirAll(*out, 0o700); err != nil {
		return err
	}
	keyPath := filepath.Join(*out, *name+".key")
	pubPath := filepath.Join(*out, *name+".pub")
	for _, p := range []string{keyPath, pubPath} {
		if _, err := os.Stat(p); err == nil {
			return fmt.Errorf("%s exists: refusing to overwrite a release key", p)
		}
	}
	pub, priv, err := selfupdate.GenerateKey()
	if err != nil {
		return err
	}
	pemb, err := selfupdate.MarshalPrivateKeyPEM(priv)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(keyPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(pemb); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.WriteFile(pubPath, []byte(pub.String()+"\n"), 0o644); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "private key: %s (keep it offline; back it up)\npublic key : %s\n%s\n", keyPath, pubPath, pub)
	return nil
}

func loadKey(path string) (ed25519.PrivateKey, error) {
	if path == "" {
		return nil, usageError{errors.New("-key is required")}
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return selfupdate.ParsePrivateKeyPEM(b)
}

func cmdPubkey(args []string, stdout, stderr io.Writer) error {
	fs := newFlags("pubkey", stderr)
	key := fs.String("key", "", "private key file (PEM)")
	if err := parse(fs, args); err != nil {
		return err
	}
	priv, err := loadKey(*key)
	if err != nil {
		return err
	}
	fmt.Fprintln(stdout, selfupdate.PublicKey(priv.Public().(ed25519.PublicKey)).String())
	return nil
}

type fileFlag map[string]string

func (f fileFlag) String() string { return "" }
func (f fileFlag) Set(v string) error {
	t, p, ok := strings.Cut(v, "=")
	if !ok || !selfupdate.ValidTarget(t) || p == "" {
		return fmt.Errorf("want target=path with a target like linux-arm64, got %q", v)
	}
	f[t] = p
	return nil
}

func cmdPublish(args []string, stdout, stderr io.Writer) error {
	fs := newFlags("publish", stderr)
	key := fs.String("key", "", "private key file (PEM)")
	product := fs.String("product", "", "airview or airview-feeder")
	version := fs.String("version", "", "release version (MAJOR.MINOR.PATCH[-pre])")
	channel := fs.String("channel", selfupdate.ChannelStable, "stable or beta")
	dist := fs.String("dist", "", "build output directory to scan for <product>-<target>[.exe]")
	out := fs.String("out", "", "release directory (served under /feeder/ for the feeder)")
	notes := fs.String("notes", "", "short release notes")
	expires := fs.Int("expires-days", 0, "manifest expiry in days (0 = never)")
	prefix := fs.String("url-prefix", "dl/", "URL prefix of the artifacts in the manifest (relative to the manifest, or absolute)")
	files := fileFlag{}
	fs.Var(files, "file", "artifact as target=path (repeatable; overrides -dist entries)")
	if err := parse(fs, args); err != nil {
		return err
	}
	switch {
	case !selfupdate.ValidProduct(*product):
		return usageError{fmt.Errorf("invalid -product %q", *product)}
	case !selfupdate.ValidChannel(*channel):
		return usageError{fmt.Errorf("invalid -channel %q", *channel)}
	case *out == "":
		return usageError{errors.New("-out is required")}
	case *dist == "" && len(files) == 0:
		return usageError{errors.New("-dist or -file is required")}
	case *expires < 0:
		return usageError{errors.New("-expires-days must be >= 0")}
	}
	if _, err := selfupdate.ParseVersion(*version); err != nil {
		return usageError{err}
	}
	priv, err := loadKey(*key)
	if err != nil {
		return err
	}
	arts := map[string]string{}
	if *dist != "" {
		if arts, err = selfupdate.ScanArtifacts(*dist, *product); err != nil {
			return err
		}
	}
	for t, p := range files {
		arts[t] = p
	}
	if len(arts) == 0 {
		return fmt.Errorf("no artifacts named %s-<target>[.exe] in %s", *product, *dist)
	}
	now := time.Now()
	var exp time.Time
	if *expires > 0 {
		exp = now.Add(time.Duration(*expires) * 24 * time.Hour)
	}
	m, err := selfupdate.Publish(selfupdate.PublishOptions{Product: *product, Version: *version, Channel: *channel,
		Notes: *notes, Published: now, Expires: exp, Artifacts: arts, OutDir: *out, URLPrefix: *prefix, Key: priv})
	if err != nil {
		return err
	}
	dir := *out
	if *channel == selfupdate.ChannelBeta {
		dir = filepath.Join(dir, selfupdate.ChannelBeta)
	}
	fmt.Fprintf(stdout, "published %s %s (%s) in %s\n", m.Product, m.Version, m.Channel, dir)
	for _, f := range m.Files {
		fmt.Fprintf(stdout, "  %-14s %10d  %s\n", f.Target, f.Size, f.SHA256)
	}
	return nil
}

func cmdSign(args []string, stdout, stderr io.Writer) error {
	fs := newFlags("sign", stderr)
	key := fs.String("key", "", "private key file (PEM)")
	in := fs.String("in", "", "manifest.json to sign")
	out := fs.String("out", "", "signature file (default: <in>.sig)")
	raw := fs.Bool("raw", false, "sign any file (e.g. SHA256SUMS of the Pi station bundles, SEC-R6), not only a manifest")
	if err := parse(fs, args); err != nil {
		return err
	}
	if *in == "" {
		return usageError{errors.New("-in is required")}
	}
	priv, err := loadKey(*key)
	if err != nil {
		return err
	}
	body, err := os.ReadFile(*in)
	if err != nil {
		return err
	}
	if !*raw {
		if _, err := selfupdate.ParseManifest(body); err != nil {
			return fmt.Errorf("refusing to sign an invalid manifest: %w", err)
		}
	}
	if *out == "" {
		*out = *in + ".sig"
	}
	if err := os.WriteFile(*out, selfupdate.SignManifest(priv, body), 0o644); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "signed %s -> %s\n", *in, *out)
	return nil
}

func cmdVerify(args []string, stdout, stderr io.Writer) error {
	fs := newFlags("verify", stderr)
	pubS := fs.String("pub", "", "trusted public key (base64)")
	pubF := fs.String("pubfile", "", "file with the trusted public key")
	in := fs.String("in", "", "manifest.json")
	sigP := fs.String("sig", "", "signature file (default: <in>.sig)")
	dir := fs.String("dir", "", "release directory: also check every artifact the manifest names")
	if err := parse(fs, args); err != nil {
		return err
	}
	if *in == "" {
		return usageError{errors.New("-in is required")}
	}
	key, err := readPubKey(*pubS, *pubF)
	if err != nil {
		return err
	}
	if *sigP == "" {
		*sigP = *in + ".sig"
	}
	body, err := os.ReadFile(*in)
	if err != nil {
		return err
	}
	sig, err := os.ReadFile(*sigP)
	if err != nil {
		return err
	}
	m, err := selfupdate.VerifyManifest([]selfupdate.PublicKey{key}, body, sig)
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "OK: %s %s (%s), published %s, %d file(s)\n", m.Product, m.Version, m.Channel,
		m.Published.Format(time.RFC3339), len(m.Files))
	if *dir == "" {
		return nil
	}
	var bad []string
	for _, f := range m.Files {
		ref := f.URL
		if ref == "" {
			ref = "dl/" + f.Target
		}
		if strings.Contains(ref, "://") || strings.HasPrefix(ref, "/") {
			fmt.Fprintf(stdout, "  %-14s skipped (absolute URL %s)\n", f.Target, ref)
			continue
		}
		p := filepath.Join(*dir, filepath.FromSlash(ref))
		sum, size, err := hashLocal(p)
		switch {
		case err != nil:
			bad = append(bad, fmt.Sprintf("%s: %v", f.Target, err))
		case sum != f.SHA256 || size != f.Size:
			bad = append(bad, fmt.Sprintf("%s: %s does not match the manifest", f.Target, p))
		default:
			fmt.Fprintf(stdout, "  %-14s OK\n", f.Target)
		}
	}
	if len(bad) > 0 {
		sort.Strings(bad)
		return errors.New(strings.Join(bad, "; "))
	}
	return nil
}
