// Command airview-pisetup is the first-boot setup page of the AirView Feeder
// Raspberry Pi image (deploy/feeder/pi-image). It is started by
// airview-pisetup.service, which firstboot.sh installs, and lives only until
// the feeder is configured:
//
//  1. When /etc/airview-feeder/feeder.json exists it disables its unit and
//     exits (the feeder is configured already).
//  2. When the SD card carried a feeder-setup.txt with SERVER= set (copied by
//     firstboot.sh to /var/lib/airview-image/feeder-setup.txt), it configures
//     the feeder from it without any user interaction.
//  3. Otherwise (or when that fails) it serves the setup form on port 8099
//     (http://airview-feeder.local:8099/).
//
// Configuring runs the standard feeder installer (deploy/feeder/pi/install.sh,
// copied to /opt/airview-image/install-feeder.sh) with the bundled
// airview-feeder binary and --non-interactive --no-start, sets the status
// page to listen on the network with the chosen password (hash only), then
// disables this unit and starts airview-feeder.service, which takes over port
// 8099.
//
// It runs as root because the installer does. See WIRING.md next to this file.
package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"time"

	"airview/internal/config"
)

type options struct {
	listen     string
	setupFile  string
	updateConf string
	installer  string
	binary     string
	feeder     string
	config     string
	unit       string
	service    string
	uiAddr     string
	dryRun     bool
	runTimeout time.Duration
}

type app struct {
	o          options
	initial    values
	initialErr string
	// fileToken is the enrol token of feeder-setup.txt. It is never put on
	// the page (SEC-R9: any LAN visitor can open it); an empty token field
	// keeps it.
	fileToken string
	run       func(ctx context.Context, name string, args ...string) (string, error)
	host      func() (string, error)
	addrs     func() []string
}

func main() {
	log.SetFlags(0)
	log.SetPrefix("airview-pisetup: ")
	var o options
	flag.StringVar(&o.listen, "listen", ":8099", "setup page listen `address`")
	flag.StringVar(&o.setupFile, "setup-file", "/var/lib/airview-image/feeder-setup.txt", "unattended answers (KEY=VALUE `file`)")
	flag.StringVar(&o.updateConf, "update-conf", "/opt/airview-image/update.conf", "release manifest key and URL baked into the image (KEY=VALUE `file`)")
	flag.StringVar(&o.installer, "installer", "/opt/airview-image/install-feeder.sh", "feeder installer `script`")
	flag.StringVar(&o.binary, "binary", "/opt/airview-image/airview-feeder-linux-arm64", "airview-feeder `binary` to install")
	flag.StringVar(&o.feeder, "feeder", "/opt/airview-feeder/airview-feeder", "installed airview-feeder `path`")
	flag.StringVar(&o.config, "config", "/etc/airview-feeder/feeder.json", "feeder configuration `file`")
	flag.StringVar(&o.unit, "unit", "airview-pisetup.service", "this program's systemd `unit`")
	flag.StringVar(&o.service, "service", "airview-feeder.service", "feeder systemd `unit`")
	flag.StringVar(&o.uiAddr, "ui-addr", "0.0.0.0:8099", "feeder status page `address` after setup")
	flag.BoolVar(&o.dryRun, "dry-run", false, "print the commands instead of running them")
	flag.DurationVar(&o.runTimeout, "timeout", 5*time.Minute, "installer `timeout`")
	flag.Parse()
	a := newApp(o)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := a.main(ctx); err != nil {
		log.Print(err)
		os.Exit(1)
	}
}

func newApp(o options) *app {
	a := &app{o: o, host: os.Hostname, addrs: lanAddresses}
	a.run = a.execRun
	return a
}

func (a *app) hostname() string {
	h, err := a.host()
	if err != nil {
		return ""
	}
	if i := strings.IndexByte(h, '.'); i > 0 {
		h = h[:i]
	}
	return h
}

func (a *app) addresses() []string {
	out := a.addrs()
	if len(out) == 0 {
		out = []string{a.hostname()}
	}
	return out
}

// lanAddresses lists the non-loopback unicast addresses (IPv4 first).
func lanAddresses() []string {
	ifs, _ := net.InterfaceAddrs()
	var v4, v6 []string
	for _, ad := range ifs {
		ipn, ok := ad.(*net.IPNet)
		if !ok || ipn.IP.IsLoopback() || ipn.IP.IsLinkLocalUnicast() {
			continue
		}
		if ipn.IP.To4() != nil {
			v4 = append(v4, ipn.IP.String())
		} else {
			v6 = append(v6, "["+ipn.IP.String()+"]")
		}
	}
	sort.Strings(v4)
	sort.Strings(v6)
	return append(v4, v6...)
}

func (a *app) execRun(ctx context.Context, name string, args ...string) (string, error) {
	shown := strings.Join(redact(args), " ")
	if a.o.dryRun {
		log.Printf("dry-run: %s %s", name, shown)
		return "", nil
	}
	log.Printf("running: %s %s", name, shown)
	ctx, cancel := context.WithTimeout(ctx, a.o.runTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = append(os.Environ(), "NO_COLOR=1", "TERM=dumb", "DEBIAN_FRONTEND=noninteractive")
	cmd.Stdin = nil
	var buf limitedBuffer
	buf.max = 256 << 10
	cmd.Stdout = io.MultiWriter(&buf, os.Stderr)
	cmd.Stderr = cmd.Stdout
	err := cmd.Run()
	return buf.String(), err
}

type limitedBuffer struct {
	bytes.Buffer
	max int
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if room := b.max - b.Len(); room > 0 {
		if len(p) > room {
			b.Buffer.Write(p[:room])
		} else {
			b.Buffer.Write(p)
		}
	}
	return len(p), nil
}

func (a *app) configured() bool {
	_, err := os.Stat(a.o.config)
	return err == nil
}

func (a *app) main(ctx context.Context) error {
	if a.configured() {
		log.Printf("%s exists: the feeder is configured; disabling the setup page", a.o.config)
		a.disableSelf(ctx)
		return nil
	}
	v, unattended, err := readSetupFile(a.o.setupFile, a.baseValues())
	a.initial = v
	if err != nil {
		a.initialErr = fmt.Sprintf("%s: %v", a.o.setupFile, err)
		log.Print(a.initialErr)
	} else if unattended {
		if err := v.validate(false); err != nil {
			a.initialErr = "feeder-setup.txt: " + err.Error()
			log.Print(a.initialErr)
		} else if out, err := a.configure(ctx, v); err != nil {
			a.initialErr = "Automatic setup from feeder-setup.txt failed: " + err.Error()
			log.Print(a.initialErr)
			_ = out
		} else {
			log.Print("configured from feeder-setup.txt")
			return a.finish(ctx)
		}
	}
	a.initial.Password, a.initial.Password2 = "", ""
	a.fileToken, a.initial.Token = a.initial.Token, ""
	ln, err := net.Listen("tcp", a.o.listen)
	if err != nil {
		return fmt.Errorf("listen %s: %w", a.o.listen, err)
	}
	log.Printf("setup page on http://%s.local%s/ (and the Pi's IP addresses)", a.hostname(), portOf(a.o.listen))
	s := newServer(a)
	if _, err := s.serve(ctx, ln); err != nil {
		if errors.Is(err, context.Canceled) {
			return nil
		}
		return err
	}
	return a.finish(ctx)
}

// baseValues are the defaults plus the image's update.conf (the release
// manifest key and URL baked in by airview-piimage). Without a key the
// feeder cannot verify updates, so automatic updates start switched off.
func (a *app) baseValues() values {
	base := defaults(a.hostname())
	if a.o.updateConf != "" {
		u, _, err := readSetupFile(a.o.updateConf, base)
		if err != nil {
			log.Printf("%s: %v (automatic updates are not set up)", a.o.updateConf, err)
		} else {
			base.UpdateURL, base.ManifestKey = u.UpdateURL, u.ManifestKey
		}
	}
	if !base.CanAutoUpdate() {
		base.AutoUpdate = false
	}
	return base
}

func portOf(listen string) string {
	if _, p, err := net.SplitHostPort(listen); err == nil && p != "80" {
		return ":" + p
	}
	return ""
}

// configure installs and configures the feeder. It returns the installer output.
func (a *app) configure(ctx context.Context, v values) (string, error) {
	if a.configured() && !a.o.dryRun {
		return "", errors.New("the feeder is configured already")
	}
	out, err := a.run(ctx, "bash", append([]string{a.o.installer}, v.installerArgs(a.o.binary)...)...)
	if err != nil {
		return out, fmt.Errorf("the feeder installer failed (%v); see its output below", err)
	}
	if v.Password == "" {
		log.Print("no status page password given: the status page stays on 127.0.0.1:8099 (ssh -L 8099:127.0.0.1:8099 to reach it)")
		return out, nil
	}
	own := statOwner(a.o.config)
	sets := []string{"config", "set", "-config", a.o.config, "ui.addr=" + a.o.uiAddr, "ui.passwordHash=" + config.HashPassword(v.Password)}
	if o2, err := a.run(ctx, a.o.feeder, sets...); err != nil {
		return out + o2, fmt.Errorf("setting the status page address and password failed: %v", err)
	}
	if err := own.restore(a.o.config); err != nil {
		return out, fmt.Errorf("restoring the owner of %s: %v", a.o.config, err)
	}
	return out, nil
}

// finish hands over to the feeder: the setup answers are deleted, this unit
// is disabled and the feeder service started.
func (a *app) finish(ctx context.Context) error {
	if !a.o.dryRun {
		if err := os.Remove(a.o.setupFile); err != nil && !errors.Is(err, os.ErrNotExist) {
			log.Printf("removing %s: %v", a.o.setupFile, err)
		}
	}
	a.disableSelf(ctx)
	if out, err := a.run(ctx, "systemctl", "start", "--no-block", a.o.service); err != nil {
		// The installer enabled the service: it starts on the next boot. Exiting
		// with an error would only restart this (now skipped) unit.
		log.Printf("starting %s failed (%v %s); it starts on the next boot", a.o.service, err, out)
		return nil
	}
	log.Printf("%s started; setup finished", a.o.service)
	return nil
}

func (a *app) disableSelf(ctx context.Context) {
	if out, err := a.run(ctx, "systemctl", "disable", a.o.unit); err != nil {
		log.Printf("disabling %s: %v %s", a.o.unit, err, out)
	}
}
