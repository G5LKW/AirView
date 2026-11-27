//go:build !windows

package main

import "airview/internal/model"

// applyAdminTrust: on Linux the update command runs as root from a root
// timer and feeder.json in /etc/airview-feeder is root-owned (the status
// page no longer changes the update settings, SEC-R1), so the anchors of
// the configuration file are used as they are.
func applyAdminTrust(eff *model.FeederConfig, retrust bool) (string, error) {
	_, _ = eff, retrust
	return "", nil
}
