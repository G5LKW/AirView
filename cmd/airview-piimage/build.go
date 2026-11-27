package main

import (
	"bytes"
	"crypto/sha256"
	"debug/elf"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"text/template"
	"time"

	piimage "airview/deploy/feeder/pi-image"
	"airview/internal/fatfs"
)

// kindInfo describes one image flavour.
type kindInfo struct {
	Kind     string // station | feeder
	Title    string // "Station" | "Feeder"
	Hostname string // default host name
	URL      string
	Unit     string
}

var kinds = map[string]kindInfo{
	"station": {"station", "Station", "airview", "http://airview.local/", "airview"},
	"feeder":  {"feeder", "Feeder", "airview-feeder", "http://airview-feeder.local:8099/", "airview-pisetup  /  journalctl -u airview-feeder"},
}

// imageName is the file name stem: AirView-Station-Pi-<ver>.
func imageName(k kindInfo, version string) string {
	return "AirView-" + k.Title + "-Pi-" + version
}

// payloadFile is one file written to /airview on the boot partition.
type payloadFile struct {
	Name    string // name in /airview
	Data    []byte
	Checked bool // listed in SHA256SUMS (copied to /opt/airview-image on the Pi)
}

// buildInputs are the sources of an image.
type buildInputs struct {
	Kind        kindInfo
	Version     string
	Base        string // path of the base image (.img), for image.conf only
	BaseSHA256  string // of the downloaded base file (.img.xz)
	Built       time.Time
	Airview     string // linux/arm64 airview binary (station)
	Feeder      string // linux/arm64 airview-feeder binary (feeder)
	Pisetup     string // linux/arm64 airview-pisetup binary (feeder)
	StationDir  string // deploy/pi: install.sh, airview.service
	FeederSetup string // deploy/feeder/pi/install.sh
	// Licence is LICENSE.txt and Notices the THIRD-PARTY-NOTICES.txt of the
	// program in this image (relkit notices). Both are required: the Go
	// standard library's BSD licence must ship with the binaries (DLV-05).
	// They are listed in SHA256SUMS, so they reach /opt/airview-image and
	// the installers copy them to /opt/airview(-feeder).
	Licence, Notices string
	// ManifestKey (base64 Ed25519 public key of the release signing key)
	// and UpdateURL (release base URL, <URL>/feeder/manifest.json; default
	// the aggregator) let image feeders update themselves (DLV-03).
	ManifestKey, UpdateURL string
}

var (
	manifestKeyRE = regexp.MustCompile(`^[A-Za-z0-9+/]{43}=$`)
	updateURLRE   = regexp.MustCompile(`^https?://[A-Za-z0-9.-]+(:[0-9]{1,5})?(/[A-Za-z0-9._~%/+-]*)?$`)
)

// checkUpdate validates the release key and URL baked into a feeder image.
func checkUpdate(key, url string) error {
	if key != "" && !manifestKeyRE.MatchString(key) {
		return fmt.Errorf("-manifest-key %q is not the base64 of a 32-byte Ed25519 public key (deploy/release pubkey)", key)
	}
	if url != "" && !updateURLRE.MatchString(url) {
		return fmt.Errorf("-update-url %q is not an http(s) base URL", url)
	}
	if url != "" && key == "" {
		return errors.New("-update-url needs -manifest-key (unsigned updates are never configured)")
	}
	return nil
}

// lf converts a text file to LF line endings (a Windows checkout may have CRLF).
func lf(b []byte) []byte { return bytes.ReplaceAll(b, []byte("\r\n"), []byte("\n")) }

func readText(path string) ([]byte, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return lf(b), nil
}

func embedded(name string) []byte {
	b, err := fs.ReadFile(piimage.Files, name)
	if err != nil {
		panic(err)
	}
	return lf(b)
}

// checkARM64 refuses anything but a linux/arm64 ELF executable.
func checkARM64(path string) ([]byte, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	f, err := elf.NewFile(bytes.NewReader(b))
	if err != nil {
		return nil, fmt.Errorf("%s is not a Linux executable: %v", path, err)
	}
	if f.Machine != elf.EM_AARCH64 || f.Class != elf.ELFCLASS64 || f.Type != elf.ET_EXEC && f.Type != elf.ET_DYN {
		return nil, fmt.Errorf("%s is %v/%v, not a linux/arm64 executable", path, f.Machine, f.Class)
	}
	if f.Section(".interp") != nil {
		return nil, fmt.Errorf("%s is dynamically linked; build it with CGO_ENABLED=0", path)
	}
	return b, nil
}

// payload assembles the /airview files for an image.
func payload(in buildInputs) ([]payloadFile, error) {
	var files []payloadFile
	add := func(name string, data []byte, checked bool) {
		files = append(files, payloadFile{name, data, checked})
	}
	switch in.Kind.Kind {
	case "station":
		bin, err := checkARM64(in.Airview)
		if err != nil {
			return nil, err
		}
		inst, err := readText(filepath.Join(in.StationDir, "install.sh"))
		if err != nil {
			return nil, err
		}
		unit, err := readText(filepath.Join(in.StationDir, "airview.service"))
		if err != nil {
			return nil, err
		}
		add("airview-linux-arm64", bin, true)
		add("install.sh", inst, true)
		add("airview.service", unit, true)
		// Optional (WP-BUILD): the same /opt/airview tools as a bundle
		// install. install.sh records VERSION and installs update.sh /
		// uninstall.sh when they are next to it; without them it still works.
		for _, name := range []string{"update.sh", "uninstall.sh"} {
			if b, err := readText(filepath.Join(in.StationDir, name)); err == nil {
				add(name, b, true)
			} else if !errors.Is(err, fs.ErrNotExist) {
				return nil, err
			}
		}
		if v := strings.TrimSpace(in.Version); v != "" {
			add("VERSION", []byte(v+"\n"), true)
		}
	case "feeder":
		bin, err := checkARM64(in.Feeder)
		if err != nil {
			return nil, err
		}
		ps, err := checkARM64(in.Pisetup)
		if err != nil {
			return nil, err
		}
		inst, err := readText(in.FeederSetup)
		if err != nil {
			return nil, err
		}
		add("airview-feeder-linux-arm64", bin, true)
		add("install-feeder.sh", inst, true)
		add("airview-pisetup", ps, true)
		add("airview-pisetup.service", embedded("airview-pisetup.service"), true)
		add("feeder-setup.txt", embedded("feeder-setup.txt"), false)
		if err := checkUpdate(in.ManifestKey, in.UpdateURL); err != nil {
			return nil, err
		}
		upd := "# Release signing key and URL for automatic feeder updates (read by\n" +
			"# airview-pisetup; empty = the feeder does not update itself).\n" +
			"MANIFEST_KEY=" + in.ManifestKey + "\nUPDATE_URL=" + in.UpdateURL + "\n"
		add("update.conf", []byte(upd), true)
	default:
		return nil, fmt.Errorf("unknown image kind %q", in.Kind.Kind)
	}
	if in.Licence == "" || in.Notices == "" {
		return nil, errors.New("the licence and third-party notices of the image's programs are required (-licence, -notices-station / -notices-feeder)")
	}
	for _, f := range []struct{ name, path string }{{"LICENSE.txt", in.Licence}, {"THIRD-PARTY-NOTICES.txt", in.Notices}} {
		b, err := readText(f.path)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", f.name, err)
		}
		if len(bytes.TrimSpace(b)) == 0 {
			return nil, fmt.Errorf("%s (%s) is empty", f.name, f.path)
		}
		add(f.name, b, true)
	}
	add("firstboot.sh", embedded("firstboot.sh"), false)
	conf := fmt.Sprintf("# AirView image description, read by firstboot.sh (never executed).\n"+
		"AIRVIEW_IMAGE_KIND=%s\nAIRVIEW_IMAGE_VERSION=%s\nAIRVIEW_HOSTNAME=%s\n"+
		"AIRVIEW_IMAGE_BASE=%s\nAIRVIEW_IMAGE_BASE_SHA256=%s\nAIRVIEW_IMAGE_BUILT=%s\nAIRVIEW_MANIFEST_KEY=%s\n",
		in.Kind.Kind, in.Version, in.Kind.Hostname, filepath.Base(in.Base), in.BaseSHA256, in.Built.UTC().Format(time.RFC3339), in.ManifestKey)
	add("image.conf", []byte(conf), false)
	readme, err := renderReadme(in)
	if err != nil {
		return nil, err
	}
	add("README.txt", readme, false)
	var sums strings.Builder
	for _, f := range files {
		if f.Checked {
			s := sha256.Sum256(f.Data)
			fmt.Fprintf(&sums, "%s  %s\n", hex.EncodeToString(s[:]), f.Name)
		}
	}
	add("SHA256SUMS", []byte(sums.String()), false)
	return files, nil
}

func renderReadme(in buildInputs) ([]byte, error) {
	t, err := template.New("r").Parse(string(embedded("boot-readme.txt")))
	if err != nil {
		return nil, err
	}
	var b bytes.Buffer
	err = t.Execute(&b, map[string]string{
		"Title": in.Kind.Title, "Version": in.Version, "URL": in.Kind.URL, "Hostname": in.Kind.Hostname,
		"Kind": in.Kind.Kind, "Unit": in.Kind.Unit, "Base": filepath.Base(in.Base),
	})
	return b.Bytes(), err
}

// vendorMarker identifies our vendor-data (refuse to overwrite anybody else's).
const vendorMarker = "AirView Raspberry Pi image"

// provisioning reports how the base image provisions itself on first boot.
// Only cloud-init NoCloud on the boot partition (Raspberry Pi OS Trixie,
// 2025-10 and later) is supported.
func provisioning(f *fatfs.FS) (string, error) {
	var missing []string
	for _, n := range []string{"cmdline.txt", "config.txt", "user-data", "meta-data"} {
		if _, err := f.Stat(n); err != nil {
			missing = append(missing, n)
		}
	}
	if len(missing) > 0 {
		return "", fmt.Errorf("the base image's boot partition lacks %s: it is not a Raspberry Pi OS release with cloud-init (Trixie, 2025-10 or later). Older releases (Bookworm: firstrun.sh) are not supported; use the pi-gen route (deploy/feeder/pi-image/pi-gen)", strings.Join(missing, ", "))
	}
	if b, err := f.ReadFile("vendor-data"); err == nil && !bytes.Contains(b, []byte(vendorMarker)) {
		return "", errors.New("the base image already has a vendor-data file that is not AirView's; refusing to replace it")
	}
	return "cloud-init NoCloud seed on the boot partition (user-data, meta-data, network-config; AirView adds vendor-data)", nil
}

// inject writes vendor-data and /airview into the boot partition of img.
func inject(img *os.File, files []payloadFile, now time.Time) (report []string, err error) {
	st, err := img.Stat()
	if err != nil {
		return nil, err
	}
	m, err := fatfs.ReadMBR(img, st.Size())
	if err != nil {
		return nil, err
	}
	bp, ok := m.FirstFAT()
	if !ok {
		return nil, errors.New("the image has no FAT boot partition")
	}
	f, err := fatfs.Open(img, bp.Offset(), bp.Size(), fatfs.Options{Now: func() time.Time { return now }})
	if err != nil {
		return nil, fmt.Errorf("boot partition: %w", err)
	}
	for _, p := range f.Check() {
		if p.Severe {
			return nil, fmt.Errorf("the base image's boot partition is damaged: %s", p.Text)
		}
	}
	how, err := provisioning(f)
	if err != nil {
		return nil, err
	}
	report = append(report, "first-boot mechanism: "+how)
	var need int64
	cs := f.ClusterSize()
	for _, pf := range files {
		need += (int64(len(pf.Data)) + cs - 1) / cs * cs
	}
	if need+1<<20 > f.FreeBytes() {
		return nil, fmt.Errorf("the boot partition has %d MiB free, the payload needs %d MiB", f.FreeBytes()>>20, need>>20)
	}
	if err := f.RemoveAll("airview"); err != nil {
		return nil, err
	}
	if err := f.Mkdir("airview"); err != nil {
		return nil, err
	}
	sort.SliceStable(files, func(i, j int) bool { return len(files[i].Data) > len(files[j].Data) }) // big ones contiguous first
	for _, pf := range files {
		if err := f.WriteFile("airview/"+pf.Name, pf.Data); err != nil {
			return nil, fmt.Errorf("writing airview/%s: %w", pf.Name, err)
		}
	}
	if err := f.WriteFile("vendor-data", embedded("vendor-data")); err != nil {
		return nil, err
	}
	if err := f.Close(); err != nil {
		return nil, err
	}
	for _, p := range f.Check() {
		if p.Severe {
			return nil, fmt.Errorf("boot partition inconsistent after writing: %s", p.Text)
		}
	}
	report = append(report, fmt.Sprintf("boot partition: %d files written, %d MiB free afterwards", len(files)+1, f.FreeBytes()>>20))
	return report, nil
}

// copyFile copies src to dst (dst is created or truncated).
func copyFile(dst, src string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.CopyBuffer(out, in, make([]byte, 4<<20)); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
