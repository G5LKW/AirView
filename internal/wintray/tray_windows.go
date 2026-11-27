package wintray

import (
	"errors"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Item is one popup menu entry.
type Item struct {
	// Title is the text. "&" marks the access key; use "&&" for a literal
	// ampersand (Escape does that for text that is not ours).
	Title     string
	Disabled  bool
	Checked   bool
	Default   bool // bold; the action of a double click
	Separator bool // a separator line (the other fields are ignored)
	// OnClick runs on its own goroutine when the entry is chosen.
	OnClick func()
}

// Escape doubles "&" so text is shown as is in a menu.
func Escape(s string) string { return strings.ReplaceAll(s, "&", "&&") }

// NotifyKind selects the balloon icon.
type NotifyKind int

const (
	NotifyInfo NotifyKind = iota
	NotifyWarning
	NotifyError
)

// Options configure Run.
type Options struct {
	// ClassName is the window class of the hidden tray window. Signal finds
	// running trays by it, so use one name per application (and per
	// instance, if several may run).
	ClassName string
	// Tooltip is the initial hover text (at most 127 characters).
	Tooltip string
	// Icon returns PNG bytes of the icon at size×size pixels. Required.
	Icon func(size int) []byte
	// Menu builds the popup menu each time it opens. Required.
	Menu func() []Item
	// OnDoubleClick runs (on its own goroutine) on a double click of the
	// icon; nil runs the menu's Default item.
	OnDoubleClick func()
	// OnBalloonClick runs when the user clicks a balloon.
	OnBalloonClick func()
	// OnSignal receives SignalRefresh and custom signals (≥ 100).
	OnSignal func(Signal)
	// OnEndSession runs synchronously (on the UI thread) when Windows logs
	// off or shuts down; keep it short. The process may be ended after it.
	OnEndSession func()
	// Ready is called on the UI thread once the icon exists.
	Ready func(*Tray)
}

// Tray is a running tray icon.
type Tray struct {
	o        Options
	hwnd     windows.HWND
	icon     windows.Handle
	tip      string
	taskbarC uint32 // "TaskbarCreated" message

	qmu   sync.Mutex
	queue []func()

	mu      sync.Mutex
	menuIDs map[uint32]Item // last menu (tests)
	closed  bool
}

var (
	wndProcOnce sync.Once
	wndProcCB   uintptr
	traysMu     sync.Mutex
	trays       = map[windows.HWND]*Tray{}
)

// ErrNoIcon is returned when Options.Icon is missing or produces no image.
var ErrNoIcon = errors.New("wintray: no icon")

// Run creates the tray icon and runs the message loop on the calling
// goroutine (locked to its OS thread) until Quit, WM_CLOSE or log-off.
func Run(o Options) error {
	if o.Icon == nil {
		return ErrNoIcon
	}
	if o.Menu == nil {
		return errors.New("wintray: no menu")
	}
	if o.ClassName == "" {
		o.ClassName = "AirViewWinTray"
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	enableDPIAwareness()

	wndProcOnce.Do(func() { wndProcCB = windows.NewCallback(wndProc) })
	inst := moduleHandle()
	cls := utf16Ptr(o.ClassName)
	wc := wndClassEx{WndProc: wndProcCB, Instance: inst, ClassName: cls}
	wc.CbSize = uint32(unsafe.Sizeof(wc))
	if r, _, err := procRegisterClassExW.Call(uintptr(unsafe.Pointer(&wc))); r == 0 {
		return fmt.Errorf("wintray: RegisterClassEx: %w", err)
	}
	defer procUnregisterClassW.Call(uintptr(unsafe.Pointer(cls)), uintptr(inst))

	t := &Tray{o: o, tip: o.Tooltip, menuIDs: map[uint32]Item{}}
	tc, _, _ := procRegisterWindowMessageW.Call(uintptr(unsafe.Pointer(utf16Ptr("TaskbarCreated"))))
	t.taskbarC = uint32(tc)

	// A hidden top-level window (not message-only: TrackPopupMenu and
	// SetForegroundWindow need a real one).
	h, _, err := procCreateWindowExW.Call(0, uintptr(unsafe.Pointer(cls)), uintptr(unsafe.Pointer(utf16Ptr(o.ClassName))),
		0, 0, 0, 0, 0, 0, 0, uintptr(inst), 0)
	if h == 0 {
		return fmt.Errorf("wintray: CreateWindowEx: %w", err)
	}
	t.hwnd = windows.HWND(h)
	traysMu.Lock()
	trays[t.hwnd] = t
	traysMu.Unlock()
	defer func() {
		traysMu.Lock()
		delete(trays, t.hwnd)
		traysMu.Unlock()
	}()

	if err := t.loadIcon(); err != nil {
		procDestroyWindow.Call(uintptr(t.hwnd))
		return err
	}
	if err := t.add(); err != nil {
		destroyIcon(t.icon)
		procDestroyWindow.Call(uintptr(t.hwnd))
		return err
	}
	if o.Ready != nil {
		o.Ready(t)
	}

	var m msg
	for {
		r, _, err := procGetMessageW.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0)
		switch int32(r) {
		case -1:
			return fmt.Errorf("wintray: GetMessage: %w", err)
		case 0:
			destroyIcon(t.icon)
			return nil
		}
		procTranslateMessage.Call(uintptr(unsafe.Pointer(&m)))
		procDispatchMessageW.Call(uintptr(unsafe.Pointer(&m)))
	}
}

// HWND returns the hidden window handle.
func (t *Tray) HWND() windows.HWND { return t.hwnd }

// Do runs fn on the UI thread (from any goroutine). It returns false when
// the tray has already closed.
func (t *Tray) Do(fn func()) bool {
	t.mu.Lock()
	closed := t.closed
	t.mu.Unlock()
	if closed {
		return false
	}
	t.qmu.Lock()
	t.queue = append(t.queue, fn)
	t.qmu.Unlock()
	return postMessage(t.hwnd, wmDo, 0, 0)
}

// Quit removes the icon and ends Run.
func (t *Tray) Quit() { postMessage(t.hwnd, wmClose, 0, 0) }

// SetTooltip changes the hover text (from any goroutine).
func (t *Tray) SetTooltip(s string) {
	t.Do(func() {
		t.tip = s
		nid := t.nid(nifTip | nifShowTip)
		procShellNotifyIconW.Call(nimModify, uintptr(unsafe.Pointer(nid)))
	})
}

// SetIcon replaces the icon generator and redraws (from any goroutine).
func (t *Tray) SetIcon(icon func(size int) []byte) {
	t.Do(func() {
		old := t.icon
		t.o.Icon = icon
		if err := t.loadIcon(); err != nil {
			t.icon = old
			return
		}
		nid := t.nid(nifIcon)
		procShellNotifyIconW.Call(nimModify, uintptr(unsafe.Pointer(nid)))
		destroyIcon(old)
	})
}

// Notify shows a balloon / toast (from any goroutine).
func (t *Tray) Notify(title, text string, kind NotifyKind) {
	t.Do(func() {
		nid := t.nid(nifInfo)
		copyUTF16(nid.InfoTitle[:], title)
		copyUTF16(nid.Info[:], text)
		switch kind {
		case NotifyWarning:
			nid.InfoFlags = niifWarning
		case NotifyError:
			nid.InfoFlags = niifError
		default:
			nid.InfoFlags = niifInfo
		}
		procShellNotifyIconW.Call(nimModify, uintptr(unsafe.Pointer(nid)))
	})
}

// ShowMenu opens the popup menu at the mouse position (from any goroutine).
func (t *Tray) ShowMenu() { postMessage(t.hwnd, wmTray, 0, wmContextMenu) }

// Choose runs the menu entry with the given title as if the user had
// clicked it (tests, automation). It reports whether an enabled entry
// matched.
func (t *Tray) Choose(title string) bool {
	for _, it := range t.o.Menu() {
		if !it.Separator && !it.Disabled && it.Title == title && it.OnClick != nil {
			go it.OnClick()
			return true
		}
	}
	return false
}

func (t *Tray) nid(flags uint32) *notifyIconData {
	nid := &notifyIconData{HWnd: t.hwnd, ID: 1, Flags: flags | nifMessage, CallbackMessage: wmTray, Icon: t.icon}
	nid.CbSize = uint32(unsafe.Sizeof(*nid))
	copyUTF16(nid.Tip[:], t.tip)
	return nid
}

func (t *Tray) loadIcon() error {
	size := systemMetric(smCxSmIcon)
	if size <= 0 {
		size = 16
	}
	h, err := iconFromPNG(t.o.Icon(size), size)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrNoIcon, err)
	}
	t.icon = h
	return nil
}

func (t *Tray) add() error {
	nid := t.nid(nifIcon | nifTip | nifShowTip)
	if r, _, err := procShellNotifyIconW.Call(nimAdd, uintptr(unsafe.Pointer(nid))); r == 0 {
		return fmt.Errorf("wintray: Shell_NotifyIcon(NIM_ADD): %w", err)
	}
	nid.Version = notifyIconVersion4
	procShellNotifyIconW.Call(nimSetVersion, uintptr(unsafe.Pointer(nid)))
	return nil
}

func (t *Tray) remove() {
	nid := t.nid(0)
	procShellNotifyIconW.Call(nimDelete, uintptr(unsafe.Pointer(nid)))
}

// showMenu builds and tracks the popup menu; the chosen entry runs on its
// own goroutine.
func (t *Tray) showMenu() {
	items := t.o.Menu()
	hm, _, _ := procCreatePopupMenu.Call()
	if hm == 0 {
		return
	}
	defer procDestroyMenu.Call(hm)
	ids := map[uint32]Item{}
	var def uint32
	for i, it := range items {
		id := uint32(i + 1)
		if it.Separator {
			procAppendMenuW.Call(hm, mfSeparator, 0, 0)
			continue
		}
		flags := uintptr(mfString)
		if it.Disabled {
			flags |= mfGrayed
		}
		if it.Checked {
			flags |= mfChecked
		}
		procAppendMenuW.Call(hm, flags, uintptr(id), uintptr(unsafe.Pointer(utf16Ptr(it.Title))))
		ids[id] = it
		if it.Default {
			def = id
		}
	}
	if def != 0 {
		procSetMenuDefaultItem.Call(hm, uintptr(def), 0)
	}
	t.mu.Lock()
	t.menuIDs = ids
	t.mu.Unlock()

	var pt point
	procGetCursorPos.Call(uintptr(unsafe.Pointer(&pt)))
	flags := uintptr(tpmRightButton | tpmReturnCmd | tpmNoNotify | tpmBottomAlign)
	if systemMetric(smMenuDropAlignment) != 0 {
		flags |= tpmRightAlign
	} else {
		flags |= tpmLeftAlign
	}
	// Without the foreground the menu would not close when the user clicks
	// elsewhere (documented TrackPopupMenu behaviour).
	procSetForegroundWindow.Call(uintptr(t.hwnd))
	cmd, _, _ := procTrackPopupMenuEx.Call(hm, flags, uintptr(pt.X), uintptr(pt.Y), uintptr(t.hwnd), 0)
	postMessage(t.hwnd, wmNull, 0, 0)
	if it, ok := ids[uint32(cmd)]; ok && !it.Disabled && it.OnClick != nil {
		go it.OnClick()
	}
}

func (t *Tray) doubleClick() {
	if t.o.OnDoubleClick != nil {
		go t.o.OnDoubleClick()
		return
	}
	for _, it := range t.o.Menu() {
		if it.Default && !it.Disabled && it.OnClick != nil {
			go it.OnClick()
			return
		}
	}
}

func (t *Tray) runQueue() {
	t.qmu.Lock()
	q := t.queue
	t.queue = nil
	t.qmu.Unlock()
	for _, fn := range q {
		fn()
	}
}

func wndProc(hwnd, m, wp, lp uintptr) uintptr {
	traysMu.Lock()
	t := trays[windows.HWND(hwnd)]
	traysMu.Unlock()
	if t == nil {
		r, _, _ := procDefWindowProcW.Call(hwnd, m, wp, lp)
		return r
	}
	switch uint32(m) {
	case wmTray:
		switch uint32(lp & 0xFFFF) {
		case wmContextMenu, ninSelect, ninKeySelect:
			t.showMenu()
		case wmLButtonDblClk:
			t.doubleClick()
		case ninBalloonUserClick:
			if t.o.OnBalloonClick != nil {
				go t.o.OnBalloonClick()
			}
		}
		return 0
	case wmDo:
		t.runQueue()
		return 0
	case wmSignal:
		switch Signal(wp) {
		case SignalQuit:
			procDestroyWindow.Call(hwnd)
		case SignalMenu:
			t.showMenu()
		default:
			if t.o.OnSignal != nil {
				s := Signal(wp)
				go t.o.OnSignal(s)
			}
		}
		return 0
	case wmClose:
		procDestroyWindow.Call(hwnd)
		return 0
	case wmQueryEndSess:
		return 1
	case wmEndSession:
		if wp != 0 && t.o.OnEndSession != nil {
			t.o.OnEndSession()
		}
		return 0
	case wmDestroy:
		t.mu.Lock()
		t.closed = true
		t.mu.Unlock()
		t.remove()
		t.qmu.Lock()
		t.queue = nil // drop pending work; nothing waits for it
		t.qmu.Unlock()
		procPostQuitMessage.Call(0)
		return 0
	}
	if t.taskbarC != 0 && uint32(m) == t.taskbarC {
		// Explorer restarted: the icon is gone, add it again.
		_ = t.add()
		return 0
	}
	r, _, _ := procDefWindowProcW.Call(hwnd, m, wp, lp)
	return r
}

// Signal is a request to a running tray (see Signal).
type Signal uintptr

const (
	SignalQuit    Signal = 1 // remove the icon and exit Run
	SignalMenu    Signal = 2 // open the popup menu
	SignalRefresh Signal = 3 // passed to Options.OnSignal
	// SignalUser is the first application-defined signal (OnSignal).
	SignalUser Signal = 100
)

// SignalRunning sends s to every tray window of the given class in this
// desktop session and returns how many it found.
func SignalRunning(className string, s Signal) int {
	cls := utf16Ptr(className)
	n := 0
	var after uintptr
	for {
		h, _, _ := procFindWindowExW.Call(0, after, uintptr(unsafe.Pointer(cls)), 0)
		if h == 0 {
			return n
		}
		if postMessage(windows.HWND(h), wmSignal, uintptr(s), 0) {
			n++
		}
		after = h
	}
}
