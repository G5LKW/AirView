//go:build windows

package main

import (
	"unsafe"

	"golang.org/x/sys/windows"
)

// The few Win32 calls the wizard needs, through x/sys/windows lazy DLLs
// (no cgo, no third-party GUI toolkit).
var (
	user32   = windows.NewLazySystemDLL("user32.dll")
	gdi32    = windows.NewLazySystemDLL("gdi32.dll")
	comctl32 = windows.NewLazySystemDLL("comctl32.dll")
	shell32  = windows.NewLazySystemDLL("shell32.dll")
	kernel32 = windows.NewLazySystemDLL("kernel32.dll")

	pRegisterClassExW   = user32.NewProc("RegisterClassExW")
	pCreateWindowExW    = user32.NewProc("CreateWindowExW")
	pDefWindowProcW     = user32.NewProc("DefWindowProcW")
	pGetMessageW        = user32.NewProc("GetMessageW")
	pPeekMessageW       = user32.NewProc("PeekMessageW")
	pTranslateMessage   = user32.NewProc("TranslateMessage")
	pDispatchMessageW   = user32.NewProc("DispatchMessageW")
	pIsDialogMessageW   = user32.NewProc("IsDialogMessageW")
	pPostQuitMessage    = user32.NewProc("PostQuitMessage")
	pPostMessageW       = user32.NewProc("PostMessageW")
	pSendMessageW       = user32.NewProc("SendMessageW")
	pShowWindow         = user32.NewProc("ShowWindow")
	pUpdateWindow       = user32.NewProc("UpdateWindow")
	pEnableWindow       = user32.NewProc("EnableWindow")
	pSetWindowTextW     = user32.NewProc("SetWindowTextW")
	pGetWindowTextW     = user32.NewProc("GetWindowTextW")
	pGetWindowTextLenW  = user32.NewProc("GetWindowTextLengthW")
	pBeginPaint         = user32.NewProc("BeginPaint")
	pEndPaint           = user32.NewProc("EndPaint")
	pFillRect           = user32.NewProc("FillRect")
	pDrawTextW          = user32.NewProc("DrawTextW")
	pLoadImageW         = user32.NewProc("LoadImageW")
	pLoadCursorW        = user32.NewProc("LoadCursorW")
	pGetDpiForSystem    = user32.NewProc("GetDpiForSystem")
	pDestroyWindow      = user32.NewProc("DestroyWindow")
	pGetClientRect      = user32.NewProc("GetClientRect")
	pGetWindowRect      = user32.NewProc("GetWindowRect")
	pInvalidateRect     = user32.NewProc("InvalidateRect")
	pSetFocus           = user32.NewProc("SetFocus")
	pDrawIconEx         = user32.NewProc("DrawIconEx")
	pGetSystemMetrics   = user32.NewProc("GetSystemMetrics")
	pAdjustWindowRectEx = user32.NewProc("AdjustWindowRectEx")
	pPrintWindow        = user32.NewProc("PrintWindow")
	pGetDC              = user32.NewProc("GetDC")
	pReleaseDC          = user32.NewProc("ReleaseDC")
	pSetForegroundWnd   = user32.NewProc("SetForegroundWindow")
	pMessageBoxW        = user32.NewProc("MessageBoxW")

	pCreateFontW        = gdi32.NewProc("CreateFontW")
	pCreateSolidBrush   = gdi32.NewProc("CreateSolidBrush")
	pSelectObject       = gdi32.NewProc("SelectObject")
	pSetTextColor       = gdi32.NewProc("SetTextColor")
	pSetBkMode          = gdi32.NewProc("SetBkMode")
	pSetBkColor         = gdi32.NewProc("SetBkColor")
	pDeleteObject       = gdi32.NewProc("DeleteObject")
	pCreateCompatibleDC = gdi32.NewProc("CreateCompatibleDC")
	pCreateDIBSection   = gdi32.NewProc("CreateDIBSection")
	pDeleteDC           = gdi32.NewProc("DeleteDC")

	pInitCommonControlsEx = comctl32.NewProc("InitCommonControlsEx")

	pSHBrowseForFolderW   = shell32.NewProc("SHBrowseForFolderW")
	pSHGetPathFromIDListW = shell32.NewProc("SHGetPathFromIDListW")
	pShellExecuteExW      = shell32.NewProc("ShellExecuteExW")

	pGetModuleHandleW = kernel32.NewProc("GetModuleHandleW")
	pCoTaskMemFree    = windows.NewLazySystemDLL("ole32.dll").NewProc("CoTaskMemFree")
)

const (
	wsOverlapped   = 0x00000000
	wsCaption      = 0x00C00000
	wsSysMenu      = 0x00080000
	wsMinimizeBox  = 0x00020000
	wsChild        = 0x40000000
	wsVisible      = 0x10000000
	wsTabStop      = 0x00010000
	wsGroup        = 0x00020000
	wsVScroll      = 0x00200000
	wsBorder       = 0x00800000
	wsExClientEdge = 0x00000200
	wsExControlPar = 0x00010000

	esLeft        = 0x0000
	esMultiline   = 0x0004
	esAutoVScroll = 0x0040
	esAutoHScroll = 0x0080
	esReadOnly    = 0x0800

	bsPushButton    = 0x0
	bsDefPushButton = 0x1
	bsAutoCheckbox  = 0x3
	bsAutoRadio     = 0x9
	ssLeft          = 0x0
	ssNoPrefix      = 0x80

	swHide       = 0
	swShowNormal = 1
	swShow       = 5

	wmDestroy        = 0x0002
	wmPaint          = 0x000F
	wmClose          = 0x0010
	wmEraseBkgnd     = 0x0014
	wmSetFont        = 0x0030
	wmCommand        = 0x0111
	wmTimer          = 0x0113
	wmCtlColorEdit   = 0x0133
	wmCtlColorBtn    = 0x0135
	wmCtlColorStatic = 0x0138
	wmApp            = 0x8000
	bmGetCheck       = 0x00F0
	bmSetCheck       = 0x00F1
	bmSetStyle       = 0x00F4
	dmSetDefID       = 0x0401
	pbmSetRange32    = 0x0406
	pbmSetPos        = 0x0402
	emSetSel         = 0x00B1

	pmRemove = 0x0001

	colorWindow = 5
	idcArrow    = 32512
	transparent = 1

	dtLeft       = 0x0
	dtWordBreak  = 0x10
	dtNoPrefix   = 0x800
	dtSingleLine = 0x20
	dtVCenter    = 0x4
	dtEndEllip   = 0x8000

	smCXScreen = 0
	smCYScreen = 1

	imageIcon = 1
	lrShared  = 0x8000
	diNormal  = 0x3

	mbOK          = 0x0
	mbOKCancel    = 0x1
	mbYesNo       = 0x4
	mbIconError   = 0x10
	mbIconQuest   = 0x20
	mbIconWarn    = 0x30
	mbIconInfo    = 0x40
	mbDefButton2  = 0x100
	mbSetForegrnd = 0x10000
	idOK          = 1
	idYes         = 6

	fwNormal = 400
	fwBold   = 700
)

type wndClassEx struct {
	cbSize        uint32
	style         uint32
	lpfnWndProc   uintptr
	cbClsExtra    int32
	cbWndExtra    int32
	hInstance     windows.Handle
	hIcon         windows.Handle
	hCursor       windows.Handle
	hbrBackground windows.Handle
	lpszMenuName  *uint16
	lpszClassName *uint16
	hIconSm       windows.Handle
}

type msg struct {
	hwnd    windows.HWND
	message uint32
	wParam  uintptr
	lParam  uintptr
	time    uint32
	pt      struct{ x, y int32 }
	private uint32
}

type rect struct{ left, top, right, bottom int32 }

type paintStruct struct {
	hdc         uintptr
	fErase      int32
	rcPaint     rect
	fRestore    int32
	fIncUpdate  int32
	rgbReserved [32]byte
}

type browseInfo struct {
	hwndOwner      windows.HWND
	pidlRoot       uintptr
	pszDisplayName *uint16
	lpszTitle      *uint16
	ulFlags        uint32
	lpfn           uintptr
	lParam         uintptr
	iImage         int32
}

type bitmapInfoHeader struct {
	biSize          uint32
	biWidth         int32
	biHeight        int32
	biPlanes        uint16
	biBitCount      uint16
	biCompression   uint32
	biSizeImage     uint32
	biXPelsPerMeter int32
	biYPelsPerMeter int32
	biClrUsed       uint32
	biClrImportant  uint32
}

type shellExecuteInfo struct {
	cbSize       uint32
	fMask        uint32
	hwnd         windows.HWND
	lpVerb       *uint16
	lpFile       *uint16
	lpParameters *uint16
	lpDirectory  *uint16
	nShow        int32
	hInstApp     windows.Handle
	lpIDList     uintptr
	lpClass      *uint16
	hkeyClass    windows.Handle
	dwHotKey     uint32
	hIcon        windows.Handle
	hProcess     windows.Handle
}

func u16(s string) *uint16 {
	p, err := windows.UTF16PtrFromString(s)
	if err != nil {
		p, _ = windows.UTF16PtrFromString("")
	}
	return p
}

func u16p(s string) uintptr { return uintptr(unsafe.Pointer(u16(s))) }

func rgb(r, g, b byte) uintptr { return uintptr(r) | uintptr(g)<<8 | uintptr(b)<<16 }

func sendMessage(h windows.HWND, m uint32, w, l uintptr) uintptr {
	r, _, _ := pSendMessageW.Call(uintptr(h), uintptr(m), w, l)
	return r
}

func postMessage(h windows.HWND, m uint32, w, l uintptr) {
	_, _, _ = pPostMessageW.Call(uintptr(h), uintptr(m), w, l)
}

func setText(h windows.HWND, s string) { _, _, _ = pSetWindowTextW.Call(uintptr(h), u16p(s)) }

func getText(h windows.HWND) string {
	n, _, _ := pGetWindowTextLenW.Call(uintptr(h))
	buf := make([]uint16, n+1)
	_, _, _ = pGetWindowTextW.Call(uintptr(h), uintptr(unsafe.Pointer(&buf[0])), n+1)
	return windows.UTF16ToString(buf)
}

func enable(h windows.HWND, on bool) {
	v := uintptr(0)
	if on {
		v = 1
	}
	_, _, _ = pEnableWindow.Call(uintptr(h), v)
}

func show(h windows.HWND, on bool) {
	v := uintptr(swHide)
	if on {
		v = swShow
	}
	_, _, _ = pShowWindow.Call(uintptr(h), v)
}

func checked(h windows.HWND) bool { return sendMessage(h, bmGetCheck, 0, 0) == 1 }

func setChecked(h windows.HWND, on bool) {
	v := uintptr(0)
	if on {
		v = 1
	}
	sendMessage(h, bmSetCheck, v, 0)
}

func messageBox(owner windows.HWND, text, title string, flags uint32) int {
	r, _, _ := pMessageBoxW.Call(uintptr(owner), u16p(text), u16p(title), uintptr(flags|mbSetForegrnd))
	return int(r)
}

func moduleHandle() windows.Handle {
	h, _, _ := pGetModuleHandleW.Call(0)
	return windows.Handle(h)
}

func loWord(v uintptr) uint16 { return uint16(v & 0xffff) }
func hiWord(v uintptr) uint16 { return uint16((v >> 16) & 0xffff) }
