// Package feederui is the local status page of airview-feeder (DESIGN §12.1):
// a small embedded HTML page in the AirView dark blue theme and a JSON API,
// served by default on 127.0.0.1:8099.
//
// The page shows each receiver (input, connection, local message rate,
// aircraft heard, signal, clock class and epoch), each aggregator uplink
// (state, approval, error and what the user should do about it, counters,
// buffers, notices), the local MLAT result outputs, and offers "Get claim
// code", "Retry now" and a settings form that edits feeder.json.
//
// The package does not know how the feeder is built: the program implements
// Backend. Stats is the local frame statistics collector the program puts
// into its feed tee; it is also reusable by the Windows tray (WP-WF).
//
// Security model: see Server. In short, loopback clients are trusted, LAN
// clients need the FeederUI.PasswordHash password (Basic auth), host names
// other than IP literals and localhost are refused (DNS rebinding), and
// mutating requests need the X-AirView-Feeder header (CSRF).
package feederui
