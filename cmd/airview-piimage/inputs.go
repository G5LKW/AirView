package main

import (
	"bytes"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// extraFlags are the build/payload inputs beyond the binaries: the licence
// texts that must ship with them (DLV-05) and the release key and URL that
// let image feeders update themselves (DLV-03).
type extraFlags struct {
	licence, noticesStation, noticesFeeder *string
	manifestKey, updateURL                 *string
}

func registerExtra(fs *flag.FlagSet) extraFlags {
	return extraFlags{
		licence:        fs.String("licence", "", "LICENSE.txt `file` (default <repo>/deploy/installer/LICENSE.txt)"),
		noticesStation: fs.String("notices-station", "", "THIRD-PARTY-NOTICES.txt `file` of the station image's airview binary (default: generated with deploy/relkit notices)"),
		noticesFeeder:  fs.String("notices-feeder", "", "THIRD-PARTY-NOTICES.txt `file` of the feeder image's airview-feeder binary (default: generated)"),
		manifestKey:    fs.String("manifest-key", "", "base64 Ed25519 public `key` of the release signing key, baked into feeder images (update.keys)"),
		updateURL:      fs.String("update-url", "", "release base `URL` for feeder images (<URL>/feeder/manifest.json; default: the aggregator)"),
	}
}

// fill completes in with the licence, the notices for its kind and the
// update settings. Missing notices are generated from the source tree
// (go run ./deploy/relkit notices) into tmp.
func (e extraFlags) fill(in *buildInputs, repo, tmp string, stdout io.Writer) error {
	in.ManifestKey = strings.TrimSpace(*e.manifestKey)
	in.UpdateURL = strings.TrimRight(strings.TrimSpace(*e.updateURL), "/")
	in.Licence = *e.licence
	if in.Licence == "" {
		in.Licence = filepath.Join(repo, "deploy", "installer", "LICENSE.txt")
	}
	pkg, web, given := "./cmd/airview", true, *e.noticesStation
	if in.Kind.Kind == "feeder" {
		pkg, web, given = "./cmd/airview-feeder", false, *e.noticesFeeder
	}
	if given != "" {
		in.Notices = given
		return nil
	}
	out := filepath.Join(tmp, "notices-"+in.Kind.Kind+".txt")
	if _, err := os.Stat(out); err == nil {
		in.Notices = out
		return nil
	}
	if err := os.MkdirAll(tmp, 0o755); err != nil {
		return err
	}
	args := []string{"run", "./deploy/relkit", "notices", "-pkg", pkg, "-goos", "linux", "-goarch", "arm64", "-o", out}
	if web {
		args = append(args, "-web", filepath.Join("web", "static", "vendor"))
	}
	cmd := exec.Command("go", args...)
	cmd.Dir = repo
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=", "GOARCH=")
	var b bytes.Buffer
	cmd.Stdout, cmd.Stderr = &b, &b
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("generating the third-party notices of %s (pass -notices-%s): %v\n%s", pkg, in.Kind.Kind, err, b.String())
	}
	fmt.Fprintf(stdout, "== %s image: generated %s\n", in.Kind.Kind, out)
	in.Notices = out
	return nil
}
