package main

import (
	"encoding/json"
	"os"
	"path/filepath"

	"airview/internal/model"
)

// stationConfig is the operator file (<data>/airview.jsonc) every instance
// runs with. It pins everything that would make two runs differ for
// reasons other than the build under test:
//
//   - the HTTP port (and loopback-only binding), so the instances never
//     collide with the operator's own AirView or with each other;
//   - the receiver position (= the simulator's station) and the Beast
//     feeds from the harness (feed.go: one per antenna for the fixture,
//     one combined stream for the live phase);
//   - no internet: weather, terrain download, NTP, map tile cache, routes
//     and photos, self-update are off (their results would depend on the
//     network and on timing, and the harness must work offline);
//   - optionally short retention (-retention short), so that the
//     downsampling / pruning tiers run inside a few days of history.
func stationConfig(port int, streams []stream, feedPorts []int, recv model.Receiver, retention string) map[string]any {
	feeds := []map[string]any{}
	for i, a := range streams {
		id := a.ID
		if id == "" {
			id = "combined"
		}
		feeds = append(feeds, map[string]any{
			"id": "parity-" + id, "name": "parity " + id, "enabled": true, "type": "beast",
			"host": "127.0.0.1", "port": feedPorts[i], "antennaId": a.ID,
		})
	}
	cfg := map[string]any{
		"mode": "standalone",
		"http": map[string]any{"port": port, "bindAddr": "127.0.0.1", "tileCache": false},
		"receiver": map[string]any{"name": "Parity", "model": "Radarcape EVO 2CH", "located": true,
			"lat": recv.Lat, "lon": recv.Lon, "groundElevM": recv.GroundElevM},
		"feeds":   feeds,
		"ntp":     map[string]any{"enabled": false},
		"weather": map[string]any{"enabled": false},
		"terrain": map[string]any{"enabled": false, "autoPrefetch": false},
		"navData": map[string]any{"routesOnline": false, "photos": false},
		"update":  map[string]any{"enabled": false},
		"demo":    map[string]any{"enabled": false},
	}
	if retention == "short" {
		cfg["storage"] = map[string]any{"trailRetentionDays": 2, "minuteRetentionDays": 1}
	}
	return cfg
}

// writeStationConfig writes airview.jsonc into dir (replacing an older one:
// the ports change from run to run).
func writeStationConfig(dir string, cfg map[string]any) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	head := "// Written by tools/dbparity (parity harness). Overwritten on every run.\n"
	return os.WriteFile(filepath.Join(dir, "airview.jsonc"), append([]byte(head), b...), 0o644)
}
