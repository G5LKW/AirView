// Package winfeeder holds the Windows packaging of airview-feeder (DESIGN
// §12.3, work package WP-WF): the Inno Setup script airview-feeder.iss, the
// portable ZIP files (portable/), the installed README (installer/), the
// icon generator (mkicon) and build.ps1, which builds both architectures,
// the ZIPs and, when Inno Setup is installed, the installers. See README.md.
//
// The package has no code of its own: its tests check the packaging
// against the program (every command and flag the installer and the
// scripts use exists; the consent texts match docs/aggregator/CONSENT.md;
// the tray's autostart value name matches).
package winfeeder
