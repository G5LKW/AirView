# internal/wintray: wiring

Pure-Go Windows notification-area icon (DESIGN §12.3, WP-WF). No cgo, no third-party modules: Win32 through `golang.org/x/sys/windows` (`win32_windows.go` lists every entry point). `icon.go` builds on every platform (used by `deploy/feeder/windows/mkicon`); everything else is `_windows.go`.

## Users

| Caller | Uses |
|---|---|
| `cmd/airview-feeder/tray_windows.go` | `Run`, `Tray.SetIcon/SetTooltip/Notify/Quit`, `SignalRunning`, `MessageBox`, `Open`, `SetClipboardText`, `RunElevated`, `PNG`, `Escape` |
| `deploy/feeder/windows/mkicon` | `ICO` (installer and shortcut icon) |

## API

```go
err := wintray.Run(wintray.Options{
    ClassName: "AirViewFeederTray-" + serviceName, // SignalRunning finds the tray by it
    Tooltip:   "AirView Feeder",
    Icon:      func(size int) []byte { return wintray.PNG(size, wintray.LevelIdle) },
    Menu:      func() []wintray.Item { ... },       // rebuilt every time the menu opens
    OnBalloonClick: func() { ... },
    OnSignal:  func(s wintray.Signal) { ... },      // SignalRefresh and custom (>= SignalUser)
    OnEndSession: func() { ... },                   // log-off / shutdown, UI thread, keep short
    Ready:     func(t *wintray.Tray) { go poll(t) },
})
```

- `Run` blocks on the calling goroutine (locked to its OS thread) running the message loop until `Tray.Quit`, `SignalQuit`, `WM_CLOSE` or the end of the session.
- `Item{Title, Disabled, Checked, Default, Separator, OnClick}`. `&` marks the access key; wrap foreign text in `Escape`. `OnClick` runs on its own goroutine, so it may block (message boxes, HTTP, UAC).
- `Tray.SetIcon`, `SetTooltip` (max 127 characters), `Notify(title, text, NotifyInfo|NotifyWarning|NotifyError)`, `ShowMenu`, `Quit` and `Do(fn)` are safe from any goroutine: they queue onto the UI thread.
- `Tray.Choose(title)` runs a menu entry as if clicked (automation, tests).
- `SignalRunning(class, SignalQuit|SignalMenu|SignalRefresh)` posts to every tray of that class in the current desktop session and returns how many it found. Installers send `SignalQuit` before replacing files; `airview-feeder tray -signal menu` opens the menu (screenshots).
- Explorer restarts (`TaskbarCreated`) re-add the icon. The process is per-monitor DPI aware; the icon is rendered at `SM_CXSMICON` pixels.

Helpers: `MessageBox(title, text, flags)` (top-most, owner-less; never call it on the UI thread), `Open(url|path)` (ShellExecute "open"), `SetClipboardText` / `ClipboardText`, `RunElevated(exe, args, wait)` (UAC "runas", returns the exit code; `ErrCancelled` when the user says no), `RunHidden`.

Icon: `DrawIcon(size, level)`, `PNG`, `ICO(sizes, level)`. Levels: `LevelNone` (application icon, no badge), `LevelOK` green, `LevelWarn` amber, `LevelError` red, `LevelIdle` grey.

## Tests

`go test ./internal/wintray/` creates a real tray icon (`TestTrayLifecycle`: menu, signals, quit) and uses the clipboard; set `WINTRAY_NO_GUI=1` on a machine without an interactive desktop (CI service accounts). `TestStructLayouts` pins the C structure sizes for 64-bit targets.

## Limits

- One icon per `Run` (one `NOTIFYICONDATA` ID). Several trays need several processes or several `Run` goroutines with distinct class names.
- No sub-menus and no menu bitmaps: the feeder does not need them.
- Balloons follow the Windows notification settings (Focus assist may hide them).
