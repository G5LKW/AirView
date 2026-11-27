package main

import (
	"os"
	"path/filepath"
	"runtime"
)

// Default locations (DESIGN §12.2, §12.3):
//
//	Linux (system install, or when run as root):
//	    config /etc/airview-feeder/feeder.json, data /var/lib/airview-feeder
//	Linux (unprivileged user without a system install):
//	    config and data in $XDG_CONFIG_HOME/airview-feeder (~/.config/airview-feeder)
//	Windows:
//	    config and data in %ProgramData%\AirView Feeder
//
// -config / AIRVIEW_FEEDER_CONFIG and -data / AIRVIEW_FEEDER_DATA override
// them. The Windows service (WP-WF) uses the same defaults.
const (
	systemConfigDir = "/etc/airview-feeder"
	systemDataDir   = "/var/lib/airview-feeder"
	configFileName  = "feeder.json"
)

// pathEnv lets tests replace the environment lookups.
var (
	getenv   = os.Getenv
	geteuid  = os.Geteuid
	statPath = func(p string) error { _, err := os.Stat(p); return err }
)

func windowsBase() string {
	pd := getenv("ProgramData")
	if pd == "" {
		pd = `C:\ProgramData`
	}
	return filepath.Join(pd, "AirView Feeder")
}

func userBase() string {
	if d, err := os.UserConfigDir(); err == nil && d != "" {
		return filepath.Join(d, "airview-feeder")
	}
	return "airview-feeder-data"
}

// systemInstall reports whether the Linux system layout applies.
func systemInstall() bool {
	return geteuid() == 0 || statPath(filepath.Join(systemConfigDir, configFileName)) == nil || statPath(systemDataDir) == nil
}

// defaultConfigFile returns the configuration file used without -config.
func defaultConfigFile(dataDir string) string {
	if v := getenv(envConfigFile); v != "" {
		return v
	}
	// The main app's names, used by the Pi installer's systemd unit.
	if v := getenv(envConfigFileMain); v != "" {
		return v
	}
	switch {
	case runtime.GOOS == "windows":
		return filepath.Join(windowsBase(), configFileName)
	case systemInstall():
		return filepath.Join(systemConfigDir, configFileName)
	case dataDir != "":
		return filepath.Join(dataDir, configFileName)
	}
	return filepath.Join(userBase(), configFileName)
}

// defaultDataDir returns the data directory used without -data.
func defaultDataDir() string {
	if v := getenv(envDataDir); v != "" {
		return v
	}
	if v := getenv(envDataDirMain); v != "" {
		return v
	}
	switch {
	case runtime.GOOS == "windows":
		return windowsBase()
	case systemInstall():
		return systemDataDir
	}
	return userBase()
}

// uplinkStateDir is where internal/uplink keeps identity.key, uuid and the
// per-uplink state and spool.
func uplinkStateDir(dataDir string) string { return dataDir }
