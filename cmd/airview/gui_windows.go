//go:build windows

package main

import (
	"encoding/json"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	webview2 "github.com/jchv/go-webview2"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

const (
	// windowClass is the window class go-webview2 registers.
	windowClass = "webview"
	// iconResourceID is the RT_GROUP_ICON ID in winres/winres.json.
	iconResourceID = 1

	windowStateFile = "window.json"

	defaultWidth  = 1500 // at 96 dpi
	defaultHeight = 950
	minWidth      = 900
	minHeight     = 600

	wmClose = 0x0010
)

// webview2Runtime looks for an installed (Evergreen) WebView2 runtime the way
// Microsoft documents it: the "pv" value of the runtime's EdgeUpdate client
// key, per machine or per user. go-webview2 aborts the process when the
// runtime is missing, so this has to be checked first.
func webview2Runtime() (version string, ok bool) {
	const client = `Microsoft\EdgeUpdate\Clients\{F3017226-FE2A-4295-8BDF-00C3A9A7E4C5}`
	keys := []struct {
		root registry.Key
		path string
	}{
		{registry.LOCAL_MACHINE, `SOFTWARE\WOW6432Node\` + client},
		{registry.LOCAL_MACHINE, `SOFTWARE\` + client},
		{registry.CURRENT_USER, `Software\` + client},
	}
	for _, k := range keys {
		key, err := registry.OpenKey(k.root, k.path, registry.QUERY_VALUE)
		if err != nil {
			continue
		}
		pv, _, err := key.GetStringValue("pv")
		_ = key.Close()
		if err == nil && pv != "" && pv != "0.0.0.0" {
			return pv, true
		}
	}
	// A fixed-version runtime selected through the documented environment
	// variable also works.
	if dir := os.Getenv("WEBVIEW2_BROWSER_EXECUTABLE_FOLDER"); dir != "" {
		if st, err := os.Stat(dir); err == nil && st.IsDir() {
			return "fixed (" + dir + ")", true
		}
	}
	return "", false
}

type rect struct{ Left, Top, Right, Bottom int32 }

type point struct{ X, Y int32 }

// windowPlacement mirrors WINDOWPLACEMENT.
type windowPlacement struct {
	Length         uint32
	Flags          uint32
	ShowCmd        uint32
	MinPosition    point
	MaxPosition    point
	NormalPosition rect
}

// windowState is what window.json remembers.
type windowState struct {
	X         int32 `json:"x"`
	Y         int32 `json:"y"`
	W         int32 `json:"w"`
	H         int32 `json:"h"`
	Maximized bool  `json:"maximized"`
}

func systemDPI() int {
	if procGetDpiForSystem.Find() != nil {
		return 96
	}
	r, _, _ := procGetDpiForSystem.Call()
	if r < 96 || r > 960 {
		return 96
	}
	return int(r)
}

// defaultWindowSize is 1500 x 950 scaled to the system DPI, limited to the
// work area of the primary monitor.
func defaultWindowSize() (w, h uint) {
	dpi := systemDPI()
	ww, hh := defaultWidth*dpi/96, defaultHeight*dpi/96
	var work rect
	const spiGetWorkArea = 0x0030
	if r, _, _ := procSystemParametersInf.Call(spiGetWorkArea, 0, uintptr(unsafe.Pointer(&work)), 0); r != 0 {
		if maxW := int(work.Right-work.Left) * 96 / 100; maxW >= 640 && ww > maxW {
			ww = maxW
		}
		if maxH := int(work.Bottom-work.Top) * 94 / 100; maxH >= 480 && hh > maxH {
			hh = maxH
		}
	}
	return uint(ww), uint(hh)
}

func getPlacement(hwnd windows.HWND) (windowPlacement, bool) {
	var wp windowPlacement
	wp.Length = uint32(unsafe.Sizeof(wp))
	r, _, _ := procGetWindowPlacement.Call(uintptr(hwnd), uintptr(unsafe.Pointer(&wp)))
	return wp, r != 0
}

func stateFromPlacement(wp windowPlacement) (windowState, bool) {
	const wpfRestoreToMaximized = 0x0002
	n := wp.NormalPosition
	s := windowState{X: n.Left, Y: n.Top, W: n.Right - n.Left, H: n.Bottom - n.Top}
	switch wp.ShowCmd {
	case swShowMaximized:
		s.Maximized = true
	case swShowMinimized:
		s.Maximized = wp.Flags&wpfRestoreToMaximized != 0
	}
	return s, s.W >= 300 && s.H >= 200
}

func loadWindowState(dir string) (windowState, bool) {
	var s windowState
	b, err := os.ReadFile(filepath.Join(dir, windowStateFile))
	if err != nil || json.Unmarshal(b, &s) != nil {
		return s, false
	}
	if s.W < 300 || s.H < 200 || s.W > 32000 || s.H > 32000 {
		return s, false
	}
	return s, true
}

func saveWindowState(dir string, s windowState) {
	b, err := json.Marshal(s)
	if err != nil {
		return
	}
	path := filepath.Join(dir, windowStateFile)
	if old, err := os.ReadFile(path); err == nil && string(old) == string(b) {
		return
	}
	_ = os.WriteFile(path, b, 0o644)
}

// restorePlacement moves the new window to where it was last time, provided
// that place is still on a connected monitor.
func restorePlacement(hwnd windows.HWND, dir string) {
	s, ok := loadWindowState(dir)
	if !ok {
		return
	}
	r := rect{Left: s.X, Top: s.Y, Right: s.X + s.W, Bottom: s.Y + s.H}
	const monitorDefaultToNull = 0
	if m, _, _ := procMonitorFromRect.Call(uintptr(unsafe.Pointer(&r)), monitorDefaultToNull); m == 0 {
		return
	}
	wp, ok := getPlacement(hwnd)
	if !ok {
		return
	}
	wp.NormalPosition = r
	wp.Flags = 0
	wp.ShowCmd = swShowNormal
	if s.Maximized {
		wp.ShowCmd = swShowMaximized
	}
	_, _, _ = procSetWindowPlacement.Call(uintptr(hwnd), uintptr(unsafe.Pointer(&wp)))
}

// closeHook remembers the window placement at the moment the window is
// closed. go-webview2 owns the window procedure and destroys the window on
// WM_CLOSE, so the procedure is subclassed to look first.
var closeHook struct {
	sync.Mutex
	hwnd  windows.HWND
	prev  uintptr
	state windowState
	ok    bool
}

func closeHookProc(hwnd, msg, wp, lp uintptr) uintptr {
	if msg == wmClose {
		if p, ok := getPlacement(windows.HWND(hwnd)); ok {
			if s, ok := stateFromPlacement(p); ok {
				closeHook.Lock()
				closeHook.state, closeHook.ok = s, true
				closeHook.Unlock()
			}
		}
	}
	if closeHook.prev == 0 {
		r, _, _ := procDefWindowProcW.Call(hwnd, msg, wp, lp)
		return r
	}
	r, _, _ := procCallWindowProcW.Call(closeHook.prev, hwnd, msg, wp, lp)
	return r
}

func installCloseHook(hwnd windows.HWND) {
	const gwlpWndProc = ^uintptr(3) // -4
	closeHook.hwnd = hwnd
	prev, _, _ := procSetWindowLongPtrW.Call(uintptr(hwnd), gwlpWndProc, windows.NewCallback(closeHookProc))
	closeHook.prev = prev
	if prev == 0 {
		// Subclassing failed: nothing was replaced, the window still works.
		closeHook.hwnd = 0
	}
}

// externalLinksJS sends links that would open a new window (target=_blank,
// window.open) to another site to the default browser instead of a bare
// WebView2 popup. Without the binding (e.g. inside a cross-origin frame
// where the host object is not exposed) the default behaviour stays.
const externalLinksJS = `(function () {
  if (window.__airviewExternalLinks) return;
  window.__airviewExternalLinks = true;
  function external(u) {
    try {
      var x = new URL(u, location.href);
      if ((x.protocol === 'http:' || x.protocol === 'https:') && x.origin !== location.origin) return x.href;
    } catch (e) {}
    return null;
  }
  function send(u) {
    try {
      if (typeof window.airviewOpenExternal === 'function') { window.airviewOpenExternal(u); return true; }
    } catch (e) {}
    return false;
  }
  document.addEventListener('click', function (ev) {
    var a = ev.target && ev.target.closest ? ev.target.closest('a[href]') : null;
    if (!a) return;
    var t = (a.getAttribute('target') || '').toLowerCase();
    if (t !== '_blank' && t !== '_new') return;
    var u = external(a.href);
    if (u && send(u)) { ev.preventDefault(); ev.stopPropagation(); }
  }, true);
  var nativeOpen = window.open;
  window.open = function (u) {
    var x = u ? external(String(u)) : null;
    if (x && send(x)) return null;
    return nativeOpen.apply(window, arguments);
  };
})();`

// runWindow shows the application window and blocks until it is closed.
// It returns false when the window could not be created.
func runWindow(a *app) bool {
	debug := false
	if v := os.Getenv("AIRVIEW_DEBUG"); v == "1" || v == "true" {
		debug = true // context menu + DevTools (F12)
	}
	width, height := defaultWindowSize()
	w := webview2.NewWithOptions(webview2.WebViewOptions{
		Debug:     debug,
		DataPath:  filepath.Join(a.dir, "webview2"),
		AutoFocus: true,
		WindowOptions: webview2.WindowOptions{
			Title:  appTitle,
			Width:  width,
			Height: height,
			IconId: iconResourceID,
			Center: true,
		},
	})
	if w == nil {
		// The library leaves its window behind when the browser fails.
		if hwnd := findAppWindow(os.Getpid()); hwnd != 0 {
			_, _, _ = procDestroyWindow.Call(uintptr(hwnd))
		}
		return false
	}
	hwnd := windows.HWND(uintptr(w.Window()))
	dpi := systemDPI()
	w.SetSize(minWidth*dpi/96, minHeight*dpi/96, webview2.HintMin)
	restorePlacement(hwnd, a.dir)
	installCloseHook(hwnd)

	// External links -> default browser, at most two per second.
	var lastOpen time.Time
	var openMu sync.Mutex
	err := w.Bind("airviewOpenExternal", func(target string) {
		openMu.Lock()
		ok := time.Since(lastOpen) > 500*time.Millisecond
		if ok {
			lastOpen = time.Now()
		}
		openMu.Unlock()
		if !ok || len(target) > 4096 || strings.ContainsAny(target, "\r\n\x00") {
			return
		}
		if err := openBrowser(target); err != nil {
			a.log.Warn("opening external link", "err", err)
		}
	})
	if err != nil {
		a.log.Warn("external link binding", "err", err)
	} else {
		w.Init(externalLinksJS)
	}

	start := a.startURL()
	a.log.Info("window opened", "url", start)
	w.Navigate(start)

	// Ctrl+C in an attached console, logoff, shutdown: close the window,
	// which ends the message loop below.
	sig := make(chan os.Signal, 2)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	done := make(chan struct{})
	go func() {
		select {
		case s := <-sig:
			a.log.Info("signal received", "signal", s.String())
			w.Destroy() // posts WM_CLOSE; safe from any thread
		case <-done:
		}
	}()

	w.Run()
	close(done)
	signal.Stop(sig)

	closeHook.Lock()
	state, ok := closeHook.state, closeHook.ok
	closeHook.Unlock()
	if ok {
		saveWindowState(a.dir, state)
	}
	a.log.Info("window closed")
	return true
}
