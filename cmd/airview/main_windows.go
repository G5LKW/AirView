//go:build windows

package main

import (
	"errors"
	"fmt"
	"hash/fnv"
	"net/url"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// guiAvailable: the Windows build has the WebView2 window.
const guiAvailable = true

const appTitle = "AirView"

var (
	kernel32          = windows.NewLazySystemDLL("kernel32.dll")
	procAttachConsole = kernel32.NewProc("AttachConsole")

	user32                  = windows.NewLazySystemDLL("user32.dll")
	procFindWindowW         = user32.NewProc("FindWindowW")
	procShowWindow          = user32.NewProc("ShowWindow")
	procSetForegroundWindow = user32.NewProc("SetForegroundWindow")
	procIsIconic            = user32.NewProc("IsIconic")
	procIsWindow            = user32.NewProc("IsWindow")
	procDestroyWindow       = user32.NewProc("DestroyWindow")
	procGetWindowPlacement  = user32.NewProc("GetWindowPlacement")
	procSetWindowPlacement  = user32.NewProc("SetWindowPlacement")
	procGetDpiForSystem     = user32.NewProc("GetDpiForSystem")
	procSystemParametersInf = user32.NewProc("SystemParametersInfoW")
	procMonitorFromRect     = user32.NewProc("MonitorFromRect")
	procSetWindowLongPtrW   = user32.NewProc("SetWindowLongPtrW")
	procCallWindowProcW     = user32.NewProc("CallWindowProcW")
	procDefWindowProcW      = user32.NewProc("DefWindowProcW")
)

const (
	swShowNormal    = 1
	swShowMinimized = 2
	swShowMaximized = 3
	swRestore       = 9

	mbOK            = 0x00000000
	mbIconError     = 0x00000010
	mbIconWarning   = 0x00000030
	mbIconInfo      = 0x00000040
	mbSetForeground = 0x00010000
)

func init() {
	// The window, its message loop and the WebView2 COM objects must all
	// live on the process's main thread.
	runtime.LockOSThread()
}

// platformInit attaches the parent console for the console-style
// invocations (-version, -headless, -help): the EXE is linked for the GUI
// subsystem and has no console of its own. Output that is already
// redirected (a pipe or file) is left alone.
func platformInit(wantConsole bool) {
	if !wantConsole {
		return
	}
	valid := func(f *os.File) bool {
		if f == nil {
			return false
		}
		t, err := windows.GetFileType(windows.Handle(f.Fd()))
		return err == nil && t != windows.FILE_TYPE_UNKNOWN
	}
	outOK, errOK := valid(os.Stdout), valid(os.Stderr)
	if outOK && errOK {
		return
	}
	const attachParentProcess = ^uintptr(0)
	if r, _, _ := procAttachConsole.Call(attachParentProcess); r == 0 {
		return
	}
	con, err := os.OpenFile("CONOUT$", os.O_WRONLY, 0)
	if err != nil {
		return
	}
	if !outOK {
		os.Stdout = con
	}
	if !errOK {
		os.Stderr = con
	}
}

func messageBox(text string, flags uint32) {
	t, err1 := windows.UTF16PtrFromString(text)
	c, err2 := windows.UTF16PtrFromString(appTitle)
	if err1 != nil || err2 != nil {
		return
	}
	_, _ = windows.MessageBox(0, t, c, flags|mbSetForeground)
}

// fatalMessage reports a start-up failure: on stderr, and in a message box
// when the application was started as a desktop program.
func fatalMessage(text string, headless bool) {
	fmt.Fprintln(os.Stderr, "airview: "+text)
	if !headless {
		messageBox(text, mbOK|mbIconError)
	}
}

// acquireInstance takes the named mutex of this data directory. already is
// true when another process holds it.
func acquireInstance(dir string) (release func(), already bool) {
	h := fnv.New64a()
	_, _ = h.Write([]byte(strings.ToLower(dir)))
	name, err := windows.UTF16PtrFromString(fmt.Sprintf(`Local\AirView-%016x`, h.Sum64()))
	if err != nil {
		return func() {}, false
	}
	handle, err := windows.CreateMutex(nil, false, name)
	if errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
		if handle != 0 {
			_ = windows.CloseHandle(handle)
		}
		return func() {}, true
	}
	if err != nil || handle == 0 {
		// Cannot tell: run rather than refuse to start.
		return func() {}, false
	}
	return func() { _ = windows.CloseHandle(handle) }, false
}

// findAppWindow returns the AirView window of process pid (any process when
// pid is 0), or 0.
func findAppWindow(pid int) windows.HWND {
	class, _ := windows.UTF16PtrFromString(windowClass)
	title, _ := windows.UTF16PtrFromString(appTitle)
	r, _, _ := procFindWindowW.Call(uintptr(unsafe.Pointer(class)), uintptr(unsafe.Pointer(title)))
	hwnd := windows.HWND(r)
	if hwnd == 0 {
		return 0
	}
	if pid != 0 {
		var owner uint32
		_, _ = windows.GetWindowThreadProcessId(hwnd, &owner)
		if int(owner) != pid {
			return 0
		}
	}
	return hwnd
}

// activateRunningInstance brings the first instance's window to the front.
// When that instance has no window (headless) and show is set, its web
// interface is opened in the default browser instead.
func activateRunningInstance(dir string, show bool) {
	info, ok := readInstanceInfo(dir)
	pid := 0
	if ok {
		pid = info.PID
	}
	if hwnd := findAppWindow(pid); hwnd != 0 {
		if r, _, _ := procIsIconic.Call(uintptr(hwnd)); r != 0 {
			_, _, _ = procShowWindow.Call(uintptr(hwnd), swRestore)
		}
		_, _, _ = procSetForegroundWindow.Call(uintptr(hwnd))
		return
	}
	if ok && show {
		_ = openBrowser(info.URL)
	}
}

// openBrowser opens an http(s) URL in the default browser.
func openBrowser(target string) error {
	u, err := url.Parse(target)
	if err != nil {
		return err
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("refusing to open %q: not an http(s) URL", target)
	}
	verb, _ := windows.UTF16PtrFromString("open")
	file, err := windows.UTF16PtrFromString(u.String())
	if err != nil {
		return err
	}
	return windows.ShellExecute(0, verb, file, nil, nil, swShowNormal)
}

// runUI shows the window, or runs headless when asked to or when the window
// cannot be created.
func runUI(a *app, gui bool) int {
	if !gui {
		return runHeadless(a, a.flags.open)
	}
	if ver, ok := webview2Runtime(); !ok {
		a.log.Warn("WebView2 runtime not found: falling back to the default browser")
		messageBox("The Microsoft Edge WebView2 runtime is not installed, so AirView cannot show its own window.\n\n"+
			"AirView keeps running in the background and opens in your default browser instead:\n"+a.url+"\n\n"+
			"Install the \"Evergreen WebView2 Runtime\" from Microsoft to get the desktop window. "+
			"To quit AirView now, end the airview.exe process in Task Manager.", mbOK|mbIconWarning)
		return runHeadless(a, true)
	} else {
		a.log.Info("WebView2 runtime", "version", ver)
	}
	if runWindow(a) {
		return 0
	}
	a.log.Warn("the application window could not be created: falling back to the default browser")
	messageBox("AirView could not create its window (WebView2 did not start).\n\n"+
		"AirView keeps running in the background and opens in your default browser instead:\n"+a.url+"\n\n"+
		"To quit AirView, end the airview.exe process in Task Manager.", mbOK|mbIconWarning)
	return runHeadless(a, true)
}

// runHeadless serves until Ctrl+C / console close / logoff.
func runHeadless(a *app, open bool) int {
	fmt.Printf("%s is running: %s\n", appTitle, a.url)
	if open {
		if err := openBrowser(a.startURL()); err != nil {
			a.log.Warn("could not open a browser", "err", err)
		}
	}
	sig := make(chan os.Signal, 2)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	var s os.Signal
	select {
	case s = <-sig:
	case code := <-a.exitReq:
		signal.Stop(sig)
		return code
	}
	a.log.Info("signal received", "signal", s.String())
	signal.Stop(sig)
	return 0
}
