package main

import (
	"net"
	"strings"

	"airview/internal/model"
)

// defaultManifestURL fills update.manifestUrl when the release keys are
// set but no manifest URL is (DLV-03 follow-up): the feeder then asks its
// first enabled AVF/1 aggregator, which serves the feeder release from
// aggregator.feederReleaseDir at https://<host>/feeder/manifest.json (the
// same URL the Pi installer derives). The keys stay the trust anchor: the
// manifest is only used when its Ed25519 signature verifies.
func defaultManifestURL(eff *model.FeederConfig) {
	if eff.Update.ManifestURL != "" || len(eff.Update.Keys) == 0 {
		return
	}
	if u := derivedManifestURL(eff.Uplinks); u != "" {
		eff.Update.ManifestURL = u
	}
}

// derivedManifestURL is https://<first enabled AVF/1 uplink host>/feeder/manifest.json,
// or "" when there is none.
func derivedManifestURL(ups []model.Uplink) string {
	for _, u := range ups {
		h := strings.TrimSpace(u.Host)
		if u.Disabled || u.IsBeast() || h == "" {
			continue
		}
		if ip := net.ParseIP(h); ip != nil && ip.To4() == nil {
			h = "[" + h + "]" // IPv6 literal
		}
		return "https://" + h + "/feeder/manifest.json"
	}
	return ""
}
