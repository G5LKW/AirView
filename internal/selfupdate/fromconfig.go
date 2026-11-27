package selfupdate

import (
	"slices"

	"airview/internal/model"
)

// SettingsFromModel converts the configuration block (model.UpdateSettings,
// "update" in config.json / airview.jsonc / feeder.json) field by field.
func SettingsFromModel(m model.UpdateSettings) Settings {
	return Settings{
		Enabled: m.Enabled, Channel: m.Channel, ManifestURL: m.ManifestURL,
		Keys: slices.Clone(m.Keys), CheckIntervalMin: m.CheckIntervalMin,
		HealthWindowS: m.HealthWindowS, HealthURL: m.HealthURL, MaxDownloadMB: m.MaxDownloadMB,
	}
}

// FeederSettings is SettingsFromModel for airview-feeder: its autoUpdate
// switch (default true) turns automatic installs on, but only when the
// installer wrote a manifest URL and at least one key.
func FeederSettings(autoUpdate bool, m model.UpdateSettings) Settings {
	s := SettingsFromModel(m)
	s.Enabled = (autoUpdate || m.Enabled) && s.ManifestURL != "" && len(s.Keys) > 0
	return s
}
