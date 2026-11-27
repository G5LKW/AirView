package wintray

import (
	"errors"
	"fmt"
	"runtime"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// MessageBox flags (MB_*), for MessageBox.
const (
	MBOK          = 0x00000000
	MBOKCancel    = 0x00000001
	MBYesNo       = 0x00000004
	MBIconError   = 0x00000010
	MBIconWarning = 0x00000030
	MBIconInfo    = 0x00000040
	MBDefButton2  = 0x00000100
	mbSetFg       = 0x00010000
	mbTopmost     = 0x00040000
)

// Results of MessageBox.
const (
	IDOK     = 1
	IDCancel = 2
	IDYes    = 6
	IDNo     = 7
)

// MessageBox shows a modal message box (owned by no window, in front of
// other windows) and returns the button pressed (IDOK, IDYes, ...). It
// blocks; call it from a menu callback, never from the UI thread.
func MessageBox(title, text string, flags uint32) int {
	r, _ := windows.MessageBox(0, utf16Ptr(text), utf16Ptr(title), flags|mbSetFg|mbTopmost)
	return int(r)
}

// Open opens a URL in the default browser, or a file or folder with its
// default handler (Explorer for folders).
func Open(target string) error {
	return windows.ShellExecute(0, utf16Ptr("open"), utf16Ptr(target), nil, nil, windows.SW_SHOWNORMAL)
}

// SetClipboardText puts s on the clipboard as Unicode text.
func SetClipboardText(s string) error {
	u, err := windows.UTF16FromString(sanitize(s))
	if err != nil {
		return err
	}
	if err := openClipboard(); err != nil {
		return err
	}
	defer procCloseClipboard.Call()
	procEmptyClipboard.Call()
	size := uintptr(len(u) * 2)
	h, _, err := procGlobalAlloc.Call(gmemMoveable, size)
	if h == 0 {
		return fmt.Errorf("GlobalAlloc: %w", err)
	}
	p, _, err := procGlobalLock.Call(h)
	if p == 0 {
		procGlobalFree.Call(h)
		return fmt.Errorf("GlobalLock: %w", err)
	}
	procRtlMoveMemory.Call(p, uintptr(unsafe.Pointer(&u[0])), size)
	procGlobalUnlock.Call(h)
	if r, _, err := procSetClipboardData.Call(cfUnicodeText, h); r == 0 {
		procGlobalFree.Call(h) // ownership passes to the system only on success
		return fmt.Errorf("SetClipboardData: %w", err)
	}
	return nil
}

// ClipboardText returns the clipboard's Unicode text ("" if none).
func ClipboardText() (string, error) {
	if err := openClipboard(); err != nil {
		return "", err
	}
	defer procCloseClipboard.Call()
	h, _, _ := procGetClipboardData.Call(cfUnicodeText)
	if h == 0 {
		return "", nil
	}
	p, _, err := procGlobalLock.Call(h)
	if p == 0 {
		return "", fmt.Errorf("GlobalLock: %w", err)
	}
	defer procGlobalUnlock.Call(h)
	n, _, _ := procLstrlenW.Call(p)
	buf := make([]uint16, n+1)
	procRtlMoveMemory.Call(uintptr(unsafe.Pointer(&buf[0])), p, n*2)
	return windows.UTF16ToString(buf), nil
}

// openClipboard retries briefly: another program may hold the clipboard.
func openClipboard() error {
	var err error
	for i := 0; i < 10; i++ {
		var r uintptr
		r, _, err = procOpenClipboard.Call(0)
		if r != 0 {
			return nil
		}
		time.Sleep(20 * time.Millisecond)
	}
	return fmt.Errorf("OpenClipboard: %w", err)
}

// ErrCancelled means the user declined the UAC prompt.
var ErrCancelled = errors.New("the administrator prompt was cancelled")

// RunElevated starts exe with args through the UAC "runas" verb, hidden,
// waits up to wait for it to finish and returns its exit code. With wait 0
// it returns as soon as the process started (exit code 0).
func RunElevated(exe, args string, wait time.Duration) (uint32, error) {
	return shellExecWait("runas", exe, args, wait)
}

// RunHidden is RunElevated without elevation (tests, and actions the user
// may already perform).
func RunHidden(exe, args string, wait time.Duration) (uint32, error) {
	return shellExecWait("open", exe, args, wait)
}

func shellExecWait(verb, exe, args string, wait time.Duration) (uint32, error) {
	// ShellExecuteEx may use COM; keep the call on one thread.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	sei := shellExecuteInfo{
		Mask: seeMaskNoCloseProcess | seeMaskNoAsync | seeMaskFlagNoUI,
		Verb: utf16Ptr(verb), File: utf16Ptr(exe), Show: windows.SW_HIDE,
	}
	if args != "" {
		sei.Parameters = utf16Ptr(args)
	}
	sei.CbSize = uint32(unsafe.Sizeof(sei))
	if r, _, err := procShellExecuteExW.Call(uintptr(unsafe.Pointer(&sei))); r == 0 {
		if errors.Is(err, windows.ERROR_CANCELLED) {
			return 0, ErrCancelled
		}
		return 0, fmt.Errorf("ShellExecuteEx %s: %w", verb, err)
	}
	if sei.Process == 0 {
		return 0, nil
	}
	defer windows.CloseHandle(sei.Process)
	if wait <= 0 {
		return 0, nil
	}
	ev, err := windows.WaitForSingleObject(sei.Process, uint32(wait/time.Millisecond))
	if err != nil {
		return 0, err
	}
	if ev == uint32(windows.WAIT_TIMEOUT) {
		return 0, fmt.Errorf("%s did not finish within %v", exe, wait)
	}
	var code uint32
	if err := windows.GetExitCodeProcess(sei.Process, &code); err != nil {
		return 0, err
	}
	return code, nil
}
