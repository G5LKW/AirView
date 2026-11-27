package main

import (
	"errors"
	"fmt"
	"strings"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"

	"airview/internal/model"
)

// Release trust anchors on Windows (SEC-R1).
//
// The "AirView Feeder update" scheduled task runs "airview-feeder update"
// as SYSTEM. feeder.json lives in %ProgramData%\AirView Feeder, which the
// service account (NT SERVICE\AirViewFeeder) may modify, and the status
// page (open to every local user) used to write it too. So the manifest
// URL and the release keys the task trusts are kept under
// HKLM\SOFTWARE\AirView Feeder\Update, which only administrators and
// SYSTEM can write (the channel stays a feeder.json setting: it only picks
// among releases signed by these keys):
//
//   - the installer writes them there (docs/aggregator/HOOKS.md);
//   - when the key does not exist yet (installs from before this change),
//     the first elevated "airview-feeder update" copies the anchors of
//     feeder.json there once (trust on first use) and says so;
//   - "airview-feeder update -retrust" (as administrator) copies the
//     anchors of feeder.json there again, after an administrator changed
//     them in the file on purpose.
//
// From then on feeder.json's update.manifestUrl and update.keys are
// ignored by the update command.

const trustKeyPath = `SOFTWARE\AirView Feeder\Update`

// adminTrust is the HKLM copy of the anchors.
type adminTrust struct {
	ManifestURL string
	Keys        []string
}

func loadAdminTrust() (adminTrust, bool, error) {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, trustKeyPath, registry.QUERY_VALUE)
	if errors.Is(err, registry.ErrNotExist) {
		return adminTrust{}, false, nil
	}
	if err != nil {
		return adminTrust{}, false, err
	}
	defer k.Close()
	var t adminTrust
	if t.ManifestURL, _, err = k.GetStringValue("ManifestUrl"); err != nil && !errors.Is(err, registry.ErrNotExist) {
		return t, false, err
	}
	if t.Keys, _, err = k.GetStringsValue("Keys"); err != nil && !errors.Is(err, registry.ErrNotExist) {
		return t, false, err
	}
	return t, true, nil
}

// removeAdminTrust deletes the HKLM anchors (uninstall). A missing key is
// not an error.
func removeAdminTrust() error {
	err := registry.DeleteKey(registry.LOCAL_MACHINE, trustKeyPath)
	if err == nil || errors.Is(err, registry.ErrNotExist) {
		// Remove the now-empty parent too when nothing else lives there.
		_ = registry.DeleteKey(registry.LOCAL_MACHINE, `SOFTWARE\AirView Feeder`)
		return nil
	}
	return err
}

func storeAdminTrust(t adminTrust) error {
	k, _, err := registry.CreateKey(registry.LOCAL_MACHINE, trustKeyPath, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer k.Close()
	if err := k.SetStringValue("ManifestUrl", t.ManifestURL); err != nil {
		return err
	}
	keys := t.Keys
	if keys == nil {
		keys = []string{}
	}
	return k.SetStringsValue("Keys", keys)
}

// applyAdminTrust replaces the anchors of eff with the HKLM copy, sealing
// feeder.json's anchors there first when there is none yet (or with
// retrust). It returns a message for the operator.
func applyAdminTrust(eff *model.FeederConfig, retrust bool) (string, error) {
	t, ok, err := loadAdminTrust()
	if err != nil {
		return "", fmt.Errorf("reading the release trust anchors (HKLM\\%s): %w", trustKeyPath, err)
	}
	fromFile := adminTrust{ManifestURL: eff.Update.ManifestURL, Keys: eff.Update.Keys}
	if !ok || retrust {
		if fromFile.ManifestURL == "" || len(fromFile.Keys) == 0 {
			if retrust {
				return "", errors.New("-retrust: feeder.json has no update.manifestUrl and update.keys to trust")
			}
			return "", nil // nothing configured: the update command reports that itself
		}
		if !windows.GetCurrentProcessToken().IsElevated() {
			if retrust {
				return "", errors.New("-retrust needs an administrator (elevated) command prompt")
			}
			// Not elevated: nothing installs as SYSTEM from here, and a
			// standard user cannot replace the program files anyway.
			return "", nil
		}
		if err := storeAdminTrust(fromFile); err != nil {
			return "", fmt.Errorf("storing the release trust anchors in HKLM\\%s: %w", trustKeyPath, err)
		}
		eff.Update.ManifestURL, eff.Update.Keys = fromFile.ManifestURL, fromFile.Keys
		return fmt.Sprintf("Release trust anchors stored in HKLM\\%s (manifest %s, %d key(s)); later changes in feeder.json need \"airview-feeder update -retrust\" as administrator.",
			trustKeyPath, fromFile.ManifestURL, len(fromFile.Keys)), nil
	}
	if t.ManifestURL != fromFile.ManifestURL || strings.Join(t.Keys, "\n") != strings.Join(fromFile.Keys, "\n") {
		eff.Update.ManifestURL, eff.Update.Keys = t.ManifestURL, t.Keys
		return "feeder.json names other update settings than the administrator's (HKLM); using HKLM. Run \"airview-feeder update -retrust\" as administrator to adopt feeder.json's.", nil
	}
	return "", nil
}
