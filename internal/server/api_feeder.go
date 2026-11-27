package server

// The Pi feeder installer and its releases (WP-PI request, DESIGN §12.2):
// GET /feeder/install.sh is deploy/feeder/pi/install.sh with this network's
// presets filled in (feeder host and port, the AVF certificate pin, the web
// address, the release signing key), so the one-liner on the "Feed this
// network" page installs a feeder without questions about the server. The
// signed releases (manifest.json, manifest.json.sig, one file per target)
// are served from aggregator.feederReleaseDir; the operator builds and signs
// them offline (the server never holds the signing key).

import (
	"airview/internal/selfupdate"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"airview/deploy/feeder/pi"
)

func init() {
	routes = append(routes, feederRoutes...)
}

var feederRoutes = []Route{
	{Method: "GET", Path: "/feeder/install.sh", Group: grpPublic, Access: AccessPublic, NetOnly: true, Raw: rawFeederInstall, NoAuto: true, ContentType: "text/x-shellscript",
		Summary: "The Raspberry Pi feeder installer with this network's presets (feeder host and port, AVF certificate pin, web address, release key).",
		Notes:   "Presets come from `aggregator.publicHost`, `aggregator.avfPort`, the AVF certificate and `aggregator.feederManifestKey`; a preset that is not configured stays empty and the installer asks for it. Never cached."},
	{Method: "GET", Path: "/feeder/manifest.json", Group: grpPublic, Access: AccessPublic, NetOnly: true, Raw: rawFeederRelease, NoAuto: true,
		Summary: "The signed airview-feeder release manifest from `aggregator.feederReleaseDir` (404 when none is configured)."},
	{Method: "GET", Path: "/feeder/manifest.json.sig", Group: grpPublic, Access: AccessPublic, NetOnly: true, Raw: rawFeederRelease, NoAuto: true,
		Summary: "The Ed25519 signature of manifest.json (base64)."},
	{Method: "GET", Path: "/feeder/dl/{target}", Group: grpPublic, Access: AccessPublic, NetOnly: true, Raw: rawFeederRelease, NoAuto: true, ContentType: "application/octet-stream",
		Summary: "One airview-feeder release file (`dl/<target>` in the release directory): `linux-arm64`, `linux-armv7`, `linux-amd64`, `windows-amd64`, `windows-arm64`, ...",
		Notes:   "Range requests are supported (resumed downloads)."},
	{Method: "GET", Path: "/feeder/beta/manifest.json", Group: grpPublic, Access: AccessPublic, NetOnly: true, Raw: rawFeederRelease, NoAuto: true,
		Summary: "The beta channel's signed manifest (`beta/` in the release directory)."},
	{Method: "GET", Path: "/feeder/beta/manifest.json.sig", Group: grpPublic, Access: AccessPublic, NetOnly: true, Raw: rawFeederRelease, NoAuto: true,
		Summary: "The beta manifest's signature."},
	{Method: "GET", Path: "/feeder/beta/dl/{target}", Group: grpPublic, Access: AccessPublic, NetOnly: true, Raw: rawFeederRelease, NoAuto: true, ContentType: "application/octet-stream",
		Summary: "One beta release file."},
}

var (
	presetHostRE = regexp.MustCompile(`^[A-Za-z0-9.-]{1,253}$`)
	presetPinRE  = regexp.MustCompile(`^sha256/[A-Za-z0-9+/]{43}=$`)
	presetKeyRE  = regexp.MustCompile(`^[A-Za-z0-9+/]{43}=$`)
)

// feederInstallScript returns install.sh with the presets filled in. Only
// values that pass the installer's own validators are inserted (no quotes,
// $ or backslashes can reach the script).
func feederInstallScript(publicHost string, avfPort int, avfPin, manifestKey string) string {
	src := pi.InstallScript
	set := func(name, val string) {
		line := "\t" + name + `=""`
		if val == "" || strings.Count(src, line) != 1 {
			return
		}
		src = strings.Replace(src, line, "\t"+name+`="`+val+`"`, 1)
	}
	host := strings.TrimSpace(publicHost)
	if presetHostRE.MatchString(host) {
		if avfPort > 0 && avfPort < 65536 {
			set("PRESET_SERVER", host+":"+strconv.Itoa(avfPort))
		}
		set("PRESET_URL", "https://"+host)
	}
	if presetPinRE.MatchString(avfPin) {
		set("PRESET_PIN", avfPin)
	}
	if presetKeyRE.MatchString(manifestKey) {
		set("PRESET_MANIFEST_KEY", manifestKey)
	}
	return src
}

func rawFeederInstall(s *Server, w http.ResponseWriter, r *http.Request) {
	a := s.eng.EffectiveConfig().Aggregator
	port := a.AVFPort
	pin := s.eng.NetworkStatus().AVFPin
	w.Header().Set("Content-Type", "text/x-shellscript; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Content-Disposition", `inline; filename="install.sh"`)
	_, _ = w.Write([]byte(feederInstallScript(a.PublicHost, port, pin, a.FeederManifestKey)))
}

// rawFeederRelease serves the operator's signed releases from
// aggregator.feederReleaseDir in the layout `deploy/release publish -out`
// writes (manifest.json, manifest.json.sig, dl/<target>, and the same
// below beta/), through selfupdate.ReleaseHandler: only those names, a
// manifest without its signature is 404, Range requests work, never cached.
func rawFeederRelease(s *Server, w http.ResponseWriter, r *http.Request) {
	dir := s.eng.EffectiveConfig().Aggregator.FeederReleaseDir
	if dir == "" {
		writeError(w, http.StatusNotFound, "this network does not serve feeder releases")
		return
	}
	http.StripPrefix("/feeder", selfupdate.ReleaseHandler{Dir: dir}).ServeHTTP(w, r)
}
