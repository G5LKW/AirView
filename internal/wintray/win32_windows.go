package wintray

import (
	"unsafe"

	"golang.org/x/sys/windows"
)

// Win32 entry points not wrapped by golang.org/x/sys/windows. All are
// documented on Microsoft Learn; the structures below follow the C layouts
// (natural Go alignment matches them on amd64 and arm64).
var (
	user32   = windows.NewLazySystemDLL("user32.dll")
	shell32  = windows.NewLazySystemDLL("shell32.dll")
	kernel32 = windows.NewLazySystemDLL("kernel32.dll")

	procRegisterClassExW         = user32.NewProc("RegisterClassExW")
	procUnregisterClassW         = user32.NewProc("UnregisterClassW")
	procCreateWindowExW          = user32.NewProc("CreateWindowExW")
	procDestroyWindow            = user32.NewProc("DestroyWindow")
	procDefWindowProcW           = user32.NewProc("DefWindowProcW")
	procGetMessageW              = user32.NewProc("GetMessageW")
	procTranslateMessage         = user32.NewProc("TranslateMessage")
	procDispatchMessageW         = user32.NewProc("DispatchMessageW")
	procPostMessageW             = user32.NewProc("PostMessageW")
	procPostQuitMessage          = user32.NewProc("PostQuitMessage")
	procRegisterWindowMessageW   = user32.NewProc("RegisterWindowMessageW")
	procCreatePopupMenu          = user32.NewProc("CreatePopupMenu")
	procAppendMenuW              = user32.NewProc("AppendMenuW")
	procSetMenuDefaultItem       = user32.NewProc("SetMenuDefaultItem")
	procTrackPopupMenuEx         = user32.NewProc("TrackPopupMenuEx")
	procDestroyMenu              = user32.NewProc("DestroyMenu")
	procSetForegroundWindow      = user32.NewProc("SetForegroundWindow")
	procGetCursorPos             = user32.NewProc("GetCursorPos")
	procGetSystemMetrics         = user32.NewProc("GetSystemMetrics")
	procCreateIconFromResourceEx = user32.NewProc("CreateIconFromResourceEx")
	procDestroyIcon              = user32.NewProc("DestroyIcon")
	procFindWindowExW            = user32.NewProc("FindWindowExW")
	procOpenClipboard            = user32.NewProc("OpenClipboard")
	procCloseClipboard           = user32.NewProc("CloseClipboard")
	procEmptyClipboard           = user32.NewProc("EmptyClipboard")
	procSetClipboardData         = user32.NewProc("SetClipboardData")
	procGetClipboardData         = user32.NewProc("GetClipboardData")
	procSetProcessDpiAwarenessCt = user32.NewProc("SetProcessDpiAwarenessContext")

	procShellNotifyIconW = shell32.NewProc("Shell_NotifyIconW")
	procShellExecuteExW  = shell32.NewProc("ShellExecuteExW")

	procGetModuleHandleW = kernel32.NewProc("GetModuleHandleW")
	procGlobalAlloc      = kernel32.NewProc("GlobalAlloc")
	procGlobalFree       = kernel32.NewProc("GlobalFree")
	procGlobalLock       = kernel32.NewProc("GlobalLock")
	procGlobalUnlock     = kernel32.NewProc("GlobalUnlock")
	procRtlMoveMemory    = kernel32.NewProc("RtlMoveMemory")
	procLstrlenW         = kernel32.NewProc("lstrlenW")
)

const (
	wmNull          = 0x0000
	wmDestroy       = 0x0002
	wmClose         = 0x0010
	wmQueryEndSess  = 0x0011
	wmEndSession    = 0x0016
	wmContextMenu   = 0x007B
	wmCommand       = 0x0111
	wmLButtonDblClk = 0x0203
	wmUser          = 0x0400
	wmApp           = 0x8000

	wmTray   = wmApp + 1 // notify icon callback
	wmDo     = wmApp + 2 // run queued functions
	wmSignal = wmApp + 3 // external request (Signal): wParam = Signal

	ninSelect           = wmUser + 0
	ninKeySelect        = wmUser + 1
	ninBalloonUserClick = wmUser + 5

	nimAdd        = 0
	nimModify     = 1
	nimDelete     = 2
	nimSetVersion = 4

	nifMessage  = 0x01
	nifIcon     = 0x02
	nifTip      = 0x04
	nifInfo     = 0x10
	nifShowTip  = 0x80
	niifInfo    = 0x01
	niifWarning = 0x02
	niifError   = 0x03

	notifyIconVersion4 = 4

	mfString    = 0x0000
	mfGrayed    = 0x0001
	mfChecked   = 0x0008
	mfSeparator = 0x0800

	tpmLeftAlign   = 0x0000
	tpmRightAlign  = 0x0008
	tpmBottomAlign = 0x0020
	tpmRightButton = 0x0002
	tpmNoNotify    = 0x0080
	tpmReturnCmd   = 0x0100

	smCxSmIcon            = 49
	smCySmIcon            = 50
	smMenuDropAlignment   = 40
	lrDefaultColor        = 0
	cfUnicodeText         = 13
	gmemMoveable          = 0x0002
	seeMaskNoCloseProcess = 0x00000040
	seeMaskNoAsync        = 0x00000100
	seeMaskFlagNoUI       = 0x00000400
)

type point struct{ X, Y int32 }

type msg struct {
	HWnd    windows.HWND
	Message uint32
	WParam  uintptr
	LParam  uintptr
	Time    uint32
	Pt      point
	Private uint32
}

type wndClassEx struct {
	CbSize     uint32
	Style      uint32
	WndProc    uintptr
	ClsExtra   int32
	WndExtra   int32
	Instance   windows.Handle
	Icon       windows.Handle
	Cursor     windows.Handle
	Background windows.Handle
	MenuName   *uint16
	ClassName  *uint16
	IconSm     windows.Handle
}

type notifyIconData struct {
	CbSize          uint32
	HWnd            windows.HWND
	ID              uint32
	Flags           uint32
	CallbackMessage uint32
	Icon            windows.Handle
	Tip             [128]uint16
	State           uint32
	StateMask       uint32
	Info            [256]uint16
	Version         uint32 // union with uTimeout
	InfoTitle       [64]uint16
	InfoFlags       uint32
	GUIDItem        windows.GUID
	BalloonIcon     windows.Handle
}

type shellExecuteInfo struct {
	CbSize     uint32
	Mask       uint32
	HWnd       windows.HWND
	Verb       *uint16
	File       *uint16
	Parameters *uint16
	Directory  *uint16
	Show       int32
	InstApp    windows.Handle
	IDList     uintptr
	Class      *uint16
	KeyClass   windows.Handle
	HotKey     uint32
	IconOrMon  windows.Handle
	Process    windows.Handle
}

// copyUTF16 copies s into dst, truncating, always NUL-terminated.
func copyUTF16(dst []uint16, s string) {
	u, _ := windows.UTF16FromString(sanitize(s))
	if len(u) > len(dst) {
		u = u[:len(dst)]
		u[len(u)-1] = 0
	}
	copy(dst, u)
	for i := len(u); i < len(dst); i++ {
		dst[i] = 0
	}
}

// sanitize drops NUL characters (UTF16FromString refuses them).
func sanitize(s string) string {
	for i := 0; i < len(s); i++ {
		if s[i] == 0 {
			b := []byte(s)
			out := b[:0]
			for _, c := range b {
				if c != 0 {
					out = append(out, c)
				}
			}
			return string(out)
		}
	}
	return s
}

func utf16Ptr(s string) *uint16 {
	p, _ := windows.UTF16PtrFromString(sanitize(s))
	return p
}

func moduleHandle() windows.Handle {
	h, _, _ := procGetModuleHandleW.Call(0)
	return windows.Handle(h)
}

func postMessage(hwnd windows.HWND, m uint32, w, l uintptr) bool {
	r, _, _ := procPostMessageW.Call(uintptr(hwnd), uintptr(m), w, l)
	return r != 0
}

func systemMetric(i int) int {
	r, _, _ := procGetSystemMetrics.Call(uintptr(i))
	return int(int32(r))
}

// iconFromPNG creates an HICON from PNG bytes (CreateIconFromResourceEx
// accepts PNG images since Windows Vista).
func iconFromPNG(data []byte, size int) (windows.Handle, error) {
	if len(data) == 0 {
		return 0, windows.ERROR_INVALID_PARAMETER
	}
	h, _, err := procCreateIconFromResourceEx.Call(uintptr(unsafe.Pointer(&data[0])), uintptr(len(data)), 1, 0x00030000,
		uintptr(size), uintptr(size), lrDefaultColor)
	if h == 0 {
		return 0, err
	}
	return windows.Handle(h), nil
}

func destroyIcon(h windows.Handle) {
	if h != 0 {
		procDestroyIcon.Call(uintptr(h))
	}
}

// enableDPIAwareness makes the process per-monitor DPI aware (v2) so the
// icon is drawn at the real pixel size. Errors are ignored (older Windows,
// or awareness already set by a manifest).
func enableDPIAwareness() {
	if procSetProcessDpiAwarenessCt.Find() != nil {
		return
	}
	const perMonitorAwareV2 = ^uintptr(3) // DPI_AWARENESS_CONTEXT_PER_MONITOR_AWARE_V2 = (HANDLE)-4
	procSetProcessDpiAwarenessCt.Call(perMonitorAwareV2)
}
