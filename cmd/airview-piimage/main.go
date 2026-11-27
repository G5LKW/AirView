// Command airview-piimage builds the AirView Raspberry Pi SD-card images on
// any OS (Windows included) without Linux tools: it decompresses the official
// Raspberry Pi OS Lite (64-bit) image (.img.xz, internal/fatfs/xz), opens the
// FAT32 boot partition in place (internal/fatfs) and adds the AirView payload
// and a cloud-init vendor-data script that installs it on the first boot
// (deploy/feeder/pi-image). Only the boot partition changes: the partition
// table, its sizes and the root file system stay byte for byte as released.
//
//	airview-piimage build   -base raspios.img.xz -out DIR [-kinds station,feeder] [-version V]
//	                        -airview BIN -feeder BIN -pisetup BIN [-repo DIR]
//	                        [-licence F] [-notices-station F] [-notices-feeder F]
//	                        [-manifest-key BASE64 [-update-url URL]]
//	airview-piimage inspect [-base base.img] IMAGE.img
//	airview-piimage decompress IN.img.xz OUT.img
//	airview-piimage payload -kind K -out DIR ...   (boot files for pi-gen)
//
// build writes, per kind, AirView-<Kind>-Pi-<ver>.img, its .img.zip (what
// Raspberry Pi Imager opens as "Use custom"), .sha256 files, a verification
// report (<name>.verify.txt) and SHA256SUMS. See
// deploy/feeder/pi-image/README.md.
//
// Every payload carries LICENSE.txt and the THIRD-PARTY-NOTICES.txt of its
// program (generated with deploy/relkit unless -notices-* is given), and a
// feeder payload carries update.conf with the release key and URL
// (-manifest-key / -update-url) that airview-pisetup passes to the feeder
// installer, so feeders from the image update themselves.
package main

import (
	"archive/zip"
	"compress/flate"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"airview/internal/fatfs"
	"airview/internal/fatfs/xz"
	"airview/internal/model"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func usage(w io.Writer) {
	fmt.Fprint(w, `Usage:
  airview-piimage build -base raspios-lite-arm64.img.xz -out DIR -airview BIN -feeder BIN -pisetup BIN
                        [-kinds station,feeder] [-version V] [-repo DIR] [-time RFC3339] [-zip=false] [-keep-img=false]
  airview-piimage inspect [-base base.img] IMAGE.img
  airview-piimage decompress IN.img.xz OUT.img
  airview-piimage payload -kind station|feeder -out DIR [-airview BIN | -feeder BIN -pisetup BIN] [-version V] [-repo DIR]
                        (the boot partition files as a directory, for the pi-gen route)
`)
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		usage(stderr)
		return 2
	}
	var err error
	switch args[0] {
	case "build":
		err = cmdBuild(args[1:], stdout, stderr)
	case "inspect":
		err = cmdInspect(args[1:], stdout, stderr)
	case "decompress":
		err = cmdDecompress(args[1:], stdout)
	case "payload":
		err = cmdPayload(args[1:], stdout, stderr)
	case "help", "-h", "--help":
		usage(stdout)
		return 0
	default:
		usage(stderr)
		return 2
	}
	if errors.Is(err, flag.ErrHelp) {
		return 2
	}
	if err != nil {
		fmt.Fprintln(stderr, "airview-piimage:", err)
		return 1
	}
	return 0
}

func fileSHA256(p string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.CopyBuffer(h, f, make([]byte, 4<<20)); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// decompress writes the decompressed .xz file to out.
func decompress(in, out string) (int64, error) {
	src, err := os.Open(in)
	if err != nil {
		return 0, err
	}
	defer src.Close()
	st, err := src.Stat()
	if err != nil {
		return 0, err
	}
	dst, err := os.Create(out)
	if err != nil {
		return 0, err
	}
	n, err := xz.DecodeFile(dst, src, st.Size(), 0)
	if cerr := dst.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(out)
		return 0, fmt.Errorf("decompressing %s: %w", in, err)
	}
	return n, nil
}

func cmdDecompress(args []string, stdout io.Writer) error {
	if len(args) != 2 {
		return errors.New("usage: airview-piimage decompress IN.img.xz OUT.img")
	}
	t := time.Now()
	n, err := decompress(args[0], args[1])
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "%s: %d bytes in %s\n", args[1], n, time.Since(t).Round(time.Millisecond))
	return nil
}

func cmdInspect(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("inspect", flag.ContinueOnError)
	fs.SetOutput(stderr)
	base := fs.String("base", "", "base `image` (.img) to compare with")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("usage: airview-piimage inspect [-base base.img] IMAGE.img")
	}
	if *base != "" {
		return verifyAgainst(stdout, fs.Arg(0), *base, nil)
	}
	v, err := openImage(fs.Arg(0))
	if err != nil {
		return err
	}
	defer v.Close()
	v.printTable(stdout)
	if _, err := v.listInjected(stdout); err != nil {
		return err
	}
	if b, err := v.fs.ReadFile("cmdline.txt"); err == nil {
		fmt.Fprintf(stdout, "cmdline.txt: %s\n", strings.TrimSpace(string(b)))
	}
	severe := 0
	for _, p := range v.fs.Check() {
		fmt.Fprintln(stdout, "fsck "+p.String())
		if p.Severe {
			severe++
		}
	}
	if severe > 0 {
		return fmt.Errorf("%d file system errors", severe)
	}
	fmt.Fprintln(stdout, "Boot file system check: consistent")
	return nil
}

func cmdBuild(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("build", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		basePath = fs.String("base", "", "Raspberry Pi OS Lite (64-bit) `image` (.img.xz or .img)")
		out      = fs.String("out", "", "output `directory`")
		kindList = fs.String("kinds", "station,feeder", "images to build: station, feeder or both")
		version  = fs.String("version", model.Version, "AirView `version` in the image names")
		airview  = fs.String("airview", "", "linux/arm64 airview `binary` (station image)")
		feeder   = fs.String("feeder", "", "linux/arm64 airview-feeder `binary` (feeder image)")
		pisetup  = fs.String("pisetup", "", "linux/arm64 airview-pisetup `binary` (feeder image)")
		repo     = fs.String("repo", ".", "AirView source `tree` (deploy/pi, deploy/feeder/pi)")
		stamp    = fs.String("time", "", "time stamp for the boot partition entries (RFC 3339; default now)")
		doZip    = fs.Bool("zip", true, "also write <name>.img.zip")
		keepImg  = fs.Bool("keep-img", true, "keep the uncompressed <name>.img next to the zip")
		extra    = registerExtra(fs)
	)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *basePath == "" || *out == "" {
		return errors.New("-base and -out are required")
	}
	if !*doZip && !*keepImg {
		return errors.New("-zip=false needs -keep-img")
	}
	built := time.Now().UTC().Truncate(time.Second)
	if *stamp != "" {
		t, err := time.Parse(time.RFC3339, *stamp)
		if err != nil {
			return fmt.Errorf("-time: %v", err)
		}
		built = t.UTC()
	}
	for _, c := range *version {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c == '.' || c == '-' || c == '+') {
			return fmt.Errorf("-version %q: use letters, digits, '.', '-', '+'", *version)
		}
	}
	var ks []kindInfo
	for _, k := range strings.Split(*kindList, ",") {
		ki, ok := kinds[strings.TrimSpace(k)]
		if !ok {
			return fmt.Errorf("unknown kind %q (station, feeder)", k)
		}
		ks = append(ks, ki)
	}
	if err := os.MkdirAll(*out, 0o755); err != nil {
		return err
	}
	logf := func(format string, a ...any) { fmt.Fprintf(stdout, "== "+format+"\n", a...) }

	logf("base %s", *basePath)
	baseSum, err := fileSHA256(*basePath)
	if err != nil {
		return err
	}
	logf("base SHA-256 %s", baseSum)
	base := *basePath
	if strings.HasSuffix(strings.ToLower(base), ".xz") {
		tmp := filepath.Join(*out, ".base-"+strings.TrimSuffix(filepath.Base(base), filepath.Ext(base)))
		t := time.Now()
		n, err := decompress(base, tmp)
		if err != nil {
			return err
		}
		defer os.Remove(tmp)
		logf("decompressed %d bytes in %s (xz checks verified)", n, time.Since(t).Round(time.Millisecond))
		base = tmp
	}
	var sums []string
	noticesTmp := filepath.Join(*out, ".notices")
	defer os.RemoveAll(noticesTmp)
	for _, k := range ks {
		in := buildInputs{
			Kind: k, Version: *version, Base: *basePath, BaseSHA256: baseSum, Built: built,
			Airview: *airview, Feeder: *feeder, Pisetup: *pisetup,
			StationDir:  filepath.Join(*repo, "deploy", "pi"),
			FeederSetup: filepath.Join(*repo, "deploy", "feeder", "pi", "install.sh"),
		}
		if err := extra.fill(&in, *repo, noticesTmp, stdout); err != nil {
			return fmt.Errorf("%s image: %w", k.Kind, err)
		}
		files, err := payload(in)
		if err != nil {
			return fmt.Errorf("%s image: %w", k.Kind, err)
		}
		name := imageName(k, *version)
		img := filepath.Join(*out, name+".img")
		logf("%s: copying the base image", name)
		part := img + ".part"
		if err := copyFile(part, base); err != nil {
			return err
		}
		f, err := os.OpenFile(part, os.O_RDWR, 0)
		if err != nil {
			return err
		}
		report, err := inject(f, files, built)
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			os.Remove(part)
			return fmt.Errorf("%s: %w", name, err)
		}
		for _, r := range report {
			logf("%s: %s", name, r)
		}
		if err := os.Rename(part, img); err != nil {
			return err
		}
		vf, err := os.Create(filepath.Join(*out, name+".verify.txt"))
		if err != nil {
			return err
		}
		fmt.Fprintf(vf, "Verification of %s against %s (base SHA-256 %s), %s\n\n", name+".img", filepath.Base(*basePath), baseSum, time.Now().UTC().Format(time.RFC3339))
		verr := verifyAgainst(io.MultiWriter(stdout, vf), img, base, files)
		vf.Close()
		if verr != nil {
			return verr
		}
		imgSum, err := fileSHA256(img)
		if err != nil {
			return err
		}
		if *keepImg {
			sums = append(sums, imgSum+"  "+name+".img")
			if err := os.WriteFile(img+".sha256", []byte(imgSum+"  "+name+".img\n"), 0o644); err != nil {
				return err
			}
		}
		if *doZip {
			t := time.Now()
			zp := img + ".zip"
			if err := zipImage(zp, img, name+".img", built); err != nil {
				return err
			}
			zs, err := fileSHA256(zp)
			if err != nil {
				return err
			}
			st, _ := os.Stat(zp)
			logf("%s: %s (%s) in %s", name, filepath.Base(zp), mib(st.Size()), time.Since(t).Round(time.Second))
			sums = append(sums, zs+"  "+name+".img.zip")
			if err := os.WriteFile(zp+".sha256", []byte(zs+"  "+name+".img.zip\n"), 0o644); err != nil {
				return err
			}
		}
		if !*keepImg {
			os.Remove(img)
		}
	}
	sumFile := filepath.Join(*out, "SHA256SUMS")
	if err := os.WriteFile(sumFile, []byte(strings.Join(sums, "\n")+"\n"), 0o644); err != nil {
		return err
	}
	logf("done: %s", sumFile)
	return nil
}

// zipImage stores img in a zip archive as name (Deflate, zip64 when needed),
// then re-reads the archive and compares the CRC-32.
func zipImage(zipPath, img, name string, mod time.Time) error {
	part := zipPath + ".part"
	zf, err := os.Create(part)
	if err != nil {
		return err
	}
	zw := zip.NewWriter(zf)
	zw.RegisterCompressor(zip.Deflate, func(w io.Writer) (io.WriteCloser, error) {
		return flate.NewWriter(w, 6)
	})
	src, err := os.Open(img)
	if err != nil {
		zf.Close()
		return err
	}
	defer src.Close()
	w, err := zw.CreateHeader(&zip.FileHeader{Name: name, Method: zip.Deflate, Modified: mod})
	if err == nil {
		_, err = io.CopyBuffer(w, src, make([]byte, 4<<20))
	}
	if err == nil {
		err = zw.Close()
	}
	if cerr := zf.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(part)
		return err
	}
	// Read back: the archive must hold exactly the image, and inflating it
	// must reproduce its size and CRC-32 (archive/zip checks the CRC at EOF).
	zr, err := zip.OpenReader(part)
	if err != nil {
		return err
	}
	st, _ := src.Stat()
	ok := len(zr.File) == 1 && zr.File[0].Name == name && int64(zr.File[0].UncompressedSize64) == st.Size()
	if ok {
		rc, err := zr.File[0].Open()
		if err == nil {
			var n int64
			n, err = io.CopyBuffer(io.Discard, rc, make([]byte, 4<<20))
			rc.Close()
			ok = err == nil && n == st.Size()
		} else {
			ok = false
		}
	}
	zr.Close()
	if !ok {
		os.Remove(part)
		return fmt.Errorf("%s: the zip archive does not read back correctly", zipPath)
	}
	return os.Rename(part, zipPath)
}

var _ fatfs.Device = (*os.File)(nil) // images are written in place

// cmdPayload writes vendor-data and airview/ into a directory (the pi-gen
// route, deploy/feeder/pi-image/pi-gen/build.sh).
func cmdPayload(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("payload", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		kind    = fs.String("kind", "", "station or feeder")
		out     = fs.String("out", "", "output `directory` (created)")
		version = fs.String("version", model.Version, "AirView `version`")
		airview = fs.String("airview", "", "linux/arm64 airview `binary` (station)")
		feeder  = fs.String("feeder", "", "linux/arm64 airview-feeder `binary` (feeder)")
		pisetup = fs.String("pisetup", "", "linux/arm64 airview-pisetup `binary` (feeder)")
		repo    = fs.String("repo", ".", "AirView source `tree`")
		base    = fs.String("base", "pi-gen", "base image description recorded in image.conf")
		extra   = registerExtra(fs)
	)
	if err := fs.Parse(args); err != nil {
		return err
	}
	k, ok := kinds[*kind]
	if !ok || *out == "" {
		return errors.New("-kind station|feeder and -out are required")
	}
	in := buildInputs{
		Kind: k, Version: *version, Base: *base, Built: time.Now().UTC().Truncate(time.Second),
		Airview: *airview, Feeder: *feeder, Pisetup: *pisetup,
		StationDir:  filepath.Join(*repo, "deploy", "pi"),
		FeederSetup: filepath.Join(*repo, "deploy", "feeder", "pi", "install.sh"),
	}
	noticesTmp := filepath.Join(*out, ".notices")
	defer os.RemoveAll(noticesTmp)
	if err := extra.fill(&in, *repo, noticesTmp, stdout); err != nil {
		return err
	}
	files, err := payload(in)
	if err != nil {
		return err
	}
	dir := filepath.Join(*out, "airview")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	for _, f := range files {
		mode := os.FileMode(0o644)
		if strings.HasSuffix(f.Name, ".sh") || strings.HasSuffix(f.Name, "-arm64") || f.Name == "airview-pisetup" {
			mode = 0o755
		}
		if err := os.WriteFile(filepath.Join(dir, f.Name), f.Data, mode); err != nil {
			return err
		}
	}
	if err := os.WriteFile(filepath.Join(*out, "vendor-data"), embedded("vendor-data"), 0o755); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "%s: vendor-data and airview/ (%d files) for the %s image\n", *out, len(files), k.Kind)
	return nil
}
