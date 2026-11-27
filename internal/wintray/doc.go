// Package wintray is a small, dependency-free Windows notification-area
// ("system tray") library for the AirView Windows feeder (DESIGN §12.3,
// work package WP-WF). It talks to Win32 directly through
// golang.org/x/sys/windows (Shell_NotifyIconW, CreateWindowExW,
// TrackPopupMenuEx, ShellExecuteExW, the clipboard) and needs no cgo and no
// third-party module.
//
// What it offers:
//
//   - Run: a tray icon with a tooltip and a popup menu that is rebuilt from a
//     callback every time it opens (so it always shows the current state),
//     balloon notifications, icon and tooltip updates from any goroutine
//     (Tray.Do marshals onto the UI thread), recovery when Explorer
//     restarts ("TaskbarCreated"), and a clean exit on WM_CLOSE / log-off.
//   - Signal: finds a running tray by its window class and asks it to quit,
//     open its menu or refresh (installers close the tray before an
//     upgrade; tests open the menu for a screenshot).
//   - Helpers every tray needs: MessageBox, Open (default browser /
//     Explorer), SetClipboardText and RunElevated (UAC "runas" with the exit
//     code).
//   - Icon drawing (icon.go, every platform): the feeder icon, a radio
//     "broadcast" glyph on the AirView navy tile with a coloured status
//     badge, rendered at any size and encoded as PNG or a multi-size ICO.
//     No image files are shipped.
//
// The package knows nothing about the feeder: cmd/airview-feeder
// (tray_windows.go) builds the menu, polls the service and the local status
// page, and decides the icon state. See WIRING.md.
//
// Threading: Run locks its goroutine to an OS thread and runs the Win32
// message loop there until Quit. Menu callbacks run on their own goroutine,
// so they may block (dialogs, HTTP) without freezing the icon.
//
// Clean room: written from the Win32 API documentation (Microsoft Learn);
// no code from other tray libraries was used (docs/aggregator/CLEANROOM.md).
package wintray
