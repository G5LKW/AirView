// Package pi holds the Raspberry Pi feeder installer (install.sh) and its
// tests. The aggregator serves the script at GET /feeder/install.sh with the
// network's presets filled in (internal/server, api_feeder.go).
package pi

import _ "embed"

// InstallScript is install.sh as shipped in this source tree.
//
//go:embed install.sh
var InstallScript string
