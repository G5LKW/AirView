//go:build windows

package main

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"

	"airview/internal/wininstall"
)

// Layout in 96-DPI pixels, scaled by the system DPI.
const (
	clientW   = 600
	clientH   = 420
	headerH   = 68
	footerH   = 56
	marginX   = 28
	bodyTop   = headerH + 18
	buttonW   = 92
	buttonH   = 28
	idNext    = 1 // IDOK: Enter
	idCancel  = 2 // IDCANCEL: Esc
	idBack    = 3
	idBrowse  = 10
	idAccept  = 11
	idDecline = 12
	idOpt0    = 20 // option check boxes idOpt0 + n
	idDir     = 40

	msgProgress = wmApp + 1
	msgDone     = wmApp + 2
)

// Branding: AirView dark blue header (the web UI's navy), light text.
var (
	colHeader    = rgb(0x0B, 0x24, 0x47)
	colHeaderSub = rgb(0xA9, 0xC4, 0xEB)
	colAccent    = rgb(0x2E, 0x8B, 0xFF)
	colBody      = rgb(0xFF, 0xFF, 0xFF)
	colFooter    = rgb(0xF2, 0xF4, 0xF8)
	colRule      = rgb(0xD5, 0xDB, 0xE5)
	colText      = rgb(0x1A, 0x1F, 0x2B)
)

type pageKind int

const (
	pgWelcome pageKind = iota
	pgLicence
	pgDir
	pgOptions
	pgReady
	pgProgress
	pgFinish
	pgUninstConfirm
)

type page struct {
	kind            pageKind
	title, subtitle string
	ctrls           []windows.HWND
}

type option struct {
	label string
	get   func(*wininstall.Options) *bool
	h     windows.HWND
	note  string
}

// wizard is the installer / uninstaller window. One per process.
type wizard struct {
	app       *app
	uninstall bool
	hwnd      windows.HWND
	scale     float64
	pages     []*page
	cur       int
	opts      wininstall.Options
	options   []*option
	accepted  bool
	upgrade   bool
	launch    bool
	removeDat bool

	fontBody, fontBold, fontTitle, fontSub uintptr
	brBody, brHeader, brFooter, brRule     uintptr
	icon                                   uintptr

	btnBack, btnNext, btnCancel windows.HWND
	edDir, stFree, stDirNote    windows.HWND
	edLicence                   windows.HWND
	rbAccept, rbDecline         windows.HWND
	stReady                     windows.HWND
	stStatus, pbar, stDetail    windows.HWND
	stFinish, cbLaunch          windows.HWND
	cbRemoveData, stData        windows.HWND

	mu       sync.Mutex
	status   string
	frac     float64
	running  bool
	finished bool
	cancel   context.CancelFunc
	result   *wininstall.Result
	err      error
	exitCode int
}

var theWizard *wizard

func (w *wizard) px(v int) int32 { return int32(float64(v)*w.scale + 0.5) }

func newWizard(a *app, uninstall bool) *wizard {
	w := &wizard{app: a, uninstall: uninstall, exitCode: wininstall.ExitCancelled, launch: true}
	theWizard = w
	return w
}

// run shows the window and returns the exit code.
func (w *wizard) run() int {
	if err := w.create(); err != nil {
		messageBox(0, "The setup window could not be created: "+err.Error(), w.app.title(), mbOK|mbIconError)
		return wininstall.ExitFailed
	}
	w.showPage(0)
	_, _, _ = pShowWindow.Call(uintptr(w.hwnd), swShowNormal)
	_, _, _ = pUpdateWindow.Call(uintptr(w.hwnd))
	_, _, _ = pSetForegroundWnd.Call(uintptr(w.hwnd))
	w.loop()
	return w.exitCode
}

func (w *wizard) loop() {
	var m msg
	for {
		r, _, _ := pGetMessageW.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0)
		if int32(r) <= 0 {
			return
		}
		if d, _, _ := pIsDialogMessageW.Call(uintptr(w.hwnd), uintptr(unsafe.Pointer(&m))); d != 0 {
			continue
		}
		_, _, _ = pTranslateMessage.Call(uintptr(unsafe.Pointer(&m)))
		_, _, _ = pDispatchMessageW.Call(uintptr(unsafe.Pointer(&m)))
	}
}

// pump processes pending messages (screenshot mode).
func (w *wizard) pump() {
	var m msg
	for {
		r, _, _ := pPeekMessageW.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0, pmRemove)
		if r == 0 {
			return
		}
		_, _, _ = pTranslateMessage.Call(uintptr(unsafe.Pointer(&m)))
		_, _, _ = pDispatchMessageW.Call(uintptr(unsafe.Pointer(&m)))
	}
}

func (w *wizard) create() error {
	const iccStandard = 0x00003FFF
	icc := struct{ size, icc uint32 }{8, iccStandard}
	_, _, _ = pInitCommonControlsEx.Call(uintptr(unsafe.Pointer(&icc)))
	const coinitApartmentThreaded = 0x2
	_ = windows.CoInitializeEx(0, coinitApartmentThreaded) // for the folder browser

	dpi, _, _ := pGetDpiForSystem.Call()
	if dpi == 0 {
		dpi = 96
	}
	w.scale = float64(dpi) / 96
	font := func(pt int, weight int) uintptr {
		h := -int32(float64(pt)*float64(dpi)/72 + 0.5)
		f, _, _ := pCreateFontW.Call(uintptr(h), 0, 0, 0, uintptr(weight), 0, 0, 0, 1 /*DEFAULT_CHARSET*/, 0, 0, 5 /*CLEARTYPE_QUALITY*/, 0, u16p("Segoe UI"))
		return f
	}
	w.fontBody = font(9, fwNormal)
	w.fontBold = font(9, fwBold)
	w.fontTitle = font(13, fwBold)
	w.fontSub = font(9, fwNormal)
	brush := func(c uintptr) uintptr { b, _, _ := pCreateSolidBrush.Call(c); return b }
	w.brBody, w.brHeader, w.brFooter, w.brRule = brush(colBody), brush(colHeader), brush(colFooter), brush(colRule)

	inst := moduleHandle()
	big, _, _ := pLoadImageW.Call(uintptr(inst), 1, imageIcon, uintptr(w.px(40)), uintptr(w.px(40)), 0)
	w.icon = big
	clsIcon, _, _ := pLoadImageW.Call(uintptr(inst), 1, imageIcon, 32, 32, lrShared)
	smIcon, _, _ := pLoadImageW.Call(uintptr(inst), 1, imageIcon, 16, 16, lrShared)
	cursor, _, _ := pLoadCursorW.Call(0, idcArrow)
	className := u16("AirViewSetupWizard")
	wc := wndClassEx{
		lpfnWndProc: syscall.NewCallback(wndProc), hInstance: inst, hIcon: windows.Handle(clsIcon),
		hCursor: windows.Handle(cursor), hbrBackground: windows.Handle(w.brBody), lpszClassName: className, hIconSm: windows.Handle(smIcon),
	}
	wc.cbSize = uint32(unsafe.Sizeof(wc))
	const errorClassAlreadyExists = windows.Errno(1410) // a second wizard in /SHOTS mode
	if r, _, err := pRegisterClassExW.Call(uintptr(unsafe.Pointer(&wc))); r == 0 && !errors.Is(err, errorClassAlreadyExists) {
		return fmt.Errorf("RegisterClassEx: %v", err)
	}
	style := uintptr(wsOverlapped | wsCaption | wsSysMenu | wsMinimizeBox)
	r := rect{0, 0, w.px(clientW), w.px(clientH)}
	_, _, _ = pAdjustWindowRectEx.Call(uintptr(unsafe.Pointer(&r)), style, 0, wsExControlPar)
	ww, wh := r.right-r.left, r.bottom-r.top
	sw, _, _ := pGetSystemMetrics.Call(smCXScreen)
	sh, _, _ := pGetSystemMetrics.Call(smCYScreen)
	x, y := (int32(sw)-ww)/2, (int32(sh)-wh)/2
	h, _, err := pCreateWindowExW.Call(wsExControlPar, uintptr(unsafe.Pointer(className)), u16p(w.windowTitle()), style,
		uintptr(x), uintptr(y), uintptr(ww), uintptr(wh), 0, 0, uintptr(inst), 0)
	if h == 0 {
		return fmt.Errorf("CreateWindowEx: %v", err)
	}
	w.hwnd = windows.HWND(h)
	w.build()
	return nil
}

func (w *wizard) windowTitle() string {
	p := w.app.product
	if w.uninstall {
		return p.Name + " Uninstall"
	}
	return p.Name + " " + p.Version + " Setup"
}

func (w *wizard) control(class, text string, style, exStyle uintptr, x, y, cx, cy int, id int) windows.HWND {
	h, _, _ := pCreateWindowExW.Call(exStyle, u16p(class), u16p(text), wsChild|style,
		uintptr(w.px(x)), uintptr(w.px(y)), uintptr(w.px(cx)), uintptr(w.px(cy)), uintptr(w.hwnd), uintptr(id), uintptr(moduleHandle()), 0)
	hw := windows.HWND(h)
	sendMessage(hw, wmSetFont, w.fontBody, 1)
	return hw
}

func (w *wizard) label(pg *page, text string, x, y, cx, cy int) windows.HWND {
	h := w.control("STATIC", text, ssLeft|ssNoPrefix, 0, x, y, cx, cy, 0)
	pg.ctrls = append(pg.ctrls, h)
	return h
}

func (w *wizard) addPage(kind pageKind, title, sub string) *page {
	p := &page{kind: kind, title: title, subtitle: sub}
	w.pages = append(w.pages, p)
	return p
}

// build creates every page's controls (hidden); showPage switches.
func (w *wizard) build() {
	p := w.app.product
	bodyW := clientW - 2*marginX
	by := clientH - footerH + (footerH-buttonH)/2
	w.btnBack = w.control("BUTTON", "< &Back", wsTabStop|bsPushButton, 0, clientW-marginX-3*buttonW-16, by, buttonW, buttonH, idBack)
	w.btnNext = w.control("BUTTON", "&Next >", wsTabStop|bsDefPushButton, 0, clientW-marginX-2*buttonW-16, by, buttonW, buttonH, idNext)
	w.btnCancel = w.control("BUTTON", "Cancel", wsTabStop|bsPushButton, 0, clientW-marginX-buttonW, by, buttonW, buttonH, idCancel)
	for _, b := range []windows.HWND{w.btnBack, w.btnNext, w.btnCancel} {
		show(b, true)
	}

	if w.uninstall {
		pg := w.addPage(pgUninstConfirm, "Uninstall "+p.Name, "Remove "+p.Name+" from this computer.")
		w.label(pg, fmt.Sprintf("%s %s will be removed from:\r\n%s\r\n\r\n%s",
			p.Name, p.Version, w.app.dir, w.app.removalText()), marginX, bodyTop, bodyW, 96)
		w.cbRemoveData = w.control("BUTTON", "Also delete the configuration and data", wsTabStop|bsAutoCheckbox, 0, marginX, bodyTop+110, bodyW, 22, idOpt0)
		pg.ctrls = append(pg.ctrls, w.cbRemoveData)
		w.stData = w.label(pg, w.app.dataText(), marginX+18, bodyTop+136, bodyW-18, 120)
		w.addProgressPages("Uninstalling", "Please wait while "+p.Name+" is removed.")
		return
	}

	// Welcome.
	pg := w.addPage(pgWelcome, "Welcome to "+p.Name+" Setup", "Version "+p.Version)
	welcome := fmt.Sprintf("This will install %s %s on your computer.", p.Name, p.Version)
	if p.Comments != "" {
		welcome += "\r\n\r\n" + p.Comments
	}
	if w.app.existing != "" {
		welcome += fmt.Sprintf("\r\n\r\n%s %s is already installed in %s. It will be upgraded; its settings and data are kept.", p.Name, w.app.existing, w.app.defaultDir)
	}
	welcome += "\r\n\r\nClose other running copies of " + p.Name + " before you continue.\r\n\r\nClick Next to continue, or Cancel to exit Setup."
	w.label(pg, welcome, marginX, bodyTop, bodyW, 250)

	// Licence.
	if w.app.licence != "" {
		pg = w.addPage(pgLicence, "Licence Agreement", "Please read the following important information before continuing.")
		w.label(pg, "Please read the following licence agreement. You must accept its terms before continuing.", marginX, bodyTop, bodyW, 20)
		w.edLicence = w.control("EDIT", w.app.licence, wsTabStop|wsVScroll|esMultiline|esReadOnly|esAutoVScroll, wsExClientEdge, marginX, bodyTop+24, bodyW, 190, 0)
		w.rbAccept = w.control("BUTTON", "I &accept the agreement", wsTabStop|wsGroup|bsAutoRadio, 0, marginX, bodyTop+222, bodyW, 20, idAccept)
		w.rbDecline = w.control("BUTTON", "I &do not accept the agreement", bsAutoRadio, 0, marginX, bodyTop+244, bodyW, 20, idDecline)
		setChecked(w.rbDecline, true)
		pg.ctrls = append(pg.ctrls, w.edLicence, w.rbAccept, w.rbDecline)
	}

	// Destination.
	pg = w.addPage(pgDir, "Select Destination Location", "Where should "+p.Name+" be installed?")
	w.label(pg, "Setup will install "+p.Name+" into the following folder. To continue, click Next. To choose a different folder, click Browse.", marginX, bodyTop, bodyW, 36)
	w.edDir = w.control("EDIT", w.app.defaultDir, wsTabStop|esAutoHScroll, wsExClientEdge, marginX, bodyTop+46, bodyW-buttonW-12, 26, idDir)
	b := w.control("BUTTON", "B&rowse...", wsTabStop|bsPushButton, 0, clientW-marginX-buttonW, bodyTop+45, buttonW, 28, idBrowse)
	pg.ctrls = append(pg.ctrls, w.edDir, b)
	w.stFree = w.label(pg, fmt.Sprintf("At least %.1f MB of free disk space is required.", float64(w.app.needBytes())/(1<<20)), marginX, bodyTop+86, bodyW, 20)
	scope := "It is installed for all users of this computer."
	if w.opts.Scope == wininstall.PerUser {
		scope = "It is installed for your user account only."
	}
	w.stDirNote = w.label(pg, scope+" Settings and data are kept in a separate folder and survive upgrades.", marginX, bodyTop+112, bodyW, 40)

	// Options.
	pg = w.addPage(pgOptions, "Select Additional Tasks", "Which additional tasks should be performed?")
	w.label(pg, "Select the additional tasks you would like Setup to perform while installing "+p.Name+", then click Next.", marginX, bodyTop, bodyW, 36)
	y := bodyTop + 44
	for i, o := range w.options {
		o.h = w.control("BUTTON", o.label, wsTabStop|bsAutoCheckbox, 0, marginX, y, bodyW, 22, idOpt0+i)
		setChecked(o.h, *o.get(&w.opts))
		pg.ctrls = append(pg.ctrls, o.h)
		y += 24
		if o.note != "" {
			w.label(pg, o.note, marginX+18, y, bodyW-18, 34)
			y += 36
		}
	}

	// Ready.
	pg = w.addPage(pgReady, "Ready to Install", "Setup is now ready to install "+p.Name+" on your computer.")
	w.label(pg, "Click Install to continue, or Back to review or change any settings.", marginX, bodyTop, bodyW, 20)
	w.stReady = w.control("EDIT", "", wsVScroll|esMultiline|esReadOnly|esAutoVScroll, wsExClientEdge, marginX, bodyTop+28, bodyW, 220, 0)
	pg.ctrls = append(pg.ctrls, w.stReady)

	w.addProgressPages("Installing", "Please wait while Setup installs "+p.Name+" on your computer.")
}

func (w *wizard) addProgressPages(title, sub string) {
	bodyW := clientW - 2*marginX
	pg := w.addPage(pgProgress, title, sub)
	w.stStatus = w.label(pg, "Preparing...", marginX, bodyTop+10, bodyW, 20)
	w.pbar = w.control("msctls_progress32", "", 0, 0, marginX, bodyTop+36, bodyW, 22, 0)
	sendMessage(w.pbar, pbmSetRange32, 0, 1000)
	pg.ctrls = append(pg.ctrls, w.pbar)
	w.stDetail = w.label(pg, "", marginX, bodyTop+68, bodyW, 60)

	p := w.app.product
	title = "Completing the " + p.Name + " Setup"
	if w.uninstall {
		title = p.Name + " Uninstall"
	}
	pg = w.addPage(pgFinish, title, "")
	w.stFinish = w.label(pg, "", marginX, bodyTop, bodyW, 200)
	if !w.uninstall && p.Launch != nil && !w.app.args.NoLaunch {
		caption := p.Launch.Caption
		if caption == "" {
			caption = "Launch " + p.Name
		}
		w.cbLaunch = w.control("BUTTON", caption, wsTabStop|bsAutoCheckbox, 0, marginX, bodyTop+210, bodyW, 22, idOpt0+30)
		setChecked(w.cbLaunch, true)
		pg.ctrls = append(pg.ctrls, w.cbLaunch)
	}
}

func (w *wizard) pageIndex(k pageKind) int {
	for i, p := range w.pages {
		if p.kind == k {
			return i
		}
	}
	return -1
}

func (w *wizard) showPage(i int) {
	if i < 0 || i >= len(w.pages) {
		return
	}
	for j, p := range w.pages {
		for _, c := range p.ctrls {
			show(c, j == i)
		}
	}
	w.cur = i
	pg := w.pages[i]
	next := "&Next >"
	enable(w.btnBack, i > 0 && pg.kind != pgProgress && pg.kind != pgFinish && pg.kind != pgUninstConfirm)
	enable(w.btnNext, true)
	enable(w.btnCancel, true)
	show(w.btnBack, pg.kind != pgFinish && pg.kind != pgUninstConfirm)
	show(w.btnCancel, pg.kind != pgFinish)
	switch pg.kind {
	case pgLicence:
		enable(w.btnNext, checked(w.rbAccept))
	case pgReady:
		next = "&Install"
		setText(w.stReady, w.summary())
	case pgUninstConfirm:
		next = "&Uninstall"
	case pgProgress:
		enable(w.btnNext, false)
		enable(w.btnBack, false)
	case pgFinish:
		next = "&Finish"
	}
	setText(w.btnNext, next)
	_, _, _ = pInvalidateRect.Call(uintptr(w.hwnd), 0, 1)
	if pg.kind == pgDir {
		_, _, _ = pSetFocus.Call(uintptr(w.edDir))
	} else {
		_, _, _ = pSetFocus.Call(uintptr(w.btnNext))
	}
}

// summary is the Ready page text.
func (w *wizard) summary() string {
	o := w.opts
	var b strings.Builder
	b.WriteString("Destination location:\r\n      " + o.Dir + "\r\n\r\n")
	if w.app.existing != "" {
		b.WriteString("Upgrade from version " + w.app.existing + " (settings and data are kept)\r\n\r\n")
	}
	b.WriteString("Additional tasks:\r\n")
	n := 0
	for _, op := range w.options {
		if *op.get(&o) {
			b.WriteString("      " + strings.ReplaceAll(op.label, "&", "") + "\r\n")
			n++
		}
	}
	if n == 0 {
		b.WriteString("      (none)\r\n")
	}
	return b.String()
}

// leave validates the current page before moving forward.
func (w *wizard) leave() bool {
	pg := w.pages[w.cur]
	switch pg.kind {
	case pgDir:
		dir := strings.TrimSpace(getText(w.edDir))
		if err := w.app.checkDir(dir); err != nil {
			messageBox(w.hwnd, err.Error(), w.app.title(), mbOK|mbIconError)
			return false
		}
		w.opts.Dir = dir
	case pgOptions:
		for _, o := range w.options {
			*o.get(&w.opts) = checked(o.h)
		}
	}
	return true
}

func (w *wizard) next() {
	if w.finishedState() {
		if w.cbLaunch != 0 && checked(w.cbLaunch) && w.err == nil {
			w.app.launch()
		}
		_, _, _ = pDestroyWindow.Call(uintptr(w.hwnd))
		return
	}
	pg := w.pages[w.cur]
	if !w.leave() {
		return
	}
	switch pg.kind {
	case pgReady:
		w.start()
		return
	case pgUninstConfirm:
		w.removeDat = checked(w.cbRemoveData)
		if w.removeDat && messageBox(w.hwnd, "Delete the configuration and data of "+w.app.product.Name+"? This cannot be undone.\r\n\r\n"+w.app.dataText(),
			w.app.title(), mbYesNo|mbIconWarn|mbDefButton2) != idYes {
			return
		}
		w.start()
		return
	}
	w.showPage(w.cur + 1)
}

func (w *wizard) finishedState() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.finished
}

// start runs the install / uninstall in a goroutine; progress comes back
// as posted messages.
func (w *wizard) start() {
	ctx, cancel := context.WithCancel(context.Background())
	w.mu.Lock()
	w.running, w.cancel = true, cancel
	w.mu.Unlock()
	w.showPage(w.pageIndex(pgProgress))
	go func() {
		progress := func(s string, f float64) {
			w.mu.Lock()
			w.status, w.frac = s, f
			w.mu.Unlock()
			postMessage(w.hwnd, msgProgress, 0, 0)
		}
		var res *wininstall.Result
		var err error
		if w.uninstall {
			res, err = w.app.doUninstall(ctx, w.removeDat, progress)
		} else {
			res, err = w.app.doInstall(ctx, w.opts, progress)
		}
		w.mu.Lock()
		w.result, w.err, w.running = res, err, false
		w.mu.Unlock()
		postMessage(w.hwnd, msgDone, 0, 0)
	}()
}

func (w *wizard) onProgress() {
	w.mu.Lock()
	s, f := w.status, w.frac
	w.mu.Unlock()
	setText(w.stStatus, s)
	sendMessage(w.pbar, pbmSetPos, uintptr(f*1000), 0)
}

func (w *wizard) onDone() {
	w.mu.Lock()
	res, err := w.result, w.err
	w.finished = true
	w.mu.Unlock()
	w.exitCode = wininstall.ExitCode(res, err)
	p := w.app.product
	var text string
	switch {
	case errors.Is(err, context.Canceled):
		text = "Setup was cancelled. Nothing was changed.\r\n\r\nClick Finish to exit Setup."
	case err != nil:
		verb := "installed"
		if w.uninstall {
			verb = "removed"
		}
		text = fmt.Sprintf("%s could not be %s:\r\n\r\n%v\r\n\r\nThe setup log is %s.", p.Name, verb, err, w.app.logPath)
	case w.uninstall:
		text = p.Name + " was removed from your computer."
		if len(res.Kept) > 0 {
			text += "\r\n\r\nKept (delete by hand if you no longer need it):\r\n      " + strings.Join(res.Kept, "\r\n      ")
		}
	default:
		text = "Setup has finished installing " + p.Name + " on your computer."
		if p.Launch != nil {
			text += " The application may be launched by selecting the installed shortcuts."
		}
		if p.FinishNote != "" {
			text += "\r\n\r\n" + p.FinishNote
		}
	}
	if res != nil && len(res.StepErrors) > 0 {
		text += "\r\n\r\nSome steps need attention (details in " + w.app.logPath + "):\r\n- " + strings.Join(res.StepErrors, "\r\n- ")
	}
	if res != nil && res.RebootRequired {
		text += "\r\n\r\nRestart Windows to finish: some files were in use."
	}
	if !w.uninstall && err == nil {
		text += "\r\n\r\nClick Finish to exit Setup."
	}
	setText(w.stFinish, text)
	if w.cbLaunch != 0 && err != nil {
		setChecked(w.cbLaunch, false)
		enable(w.cbLaunch, false)
	}
	w.showPage(w.pageIndex(pgFinish))
}

func (w *wizard) cancelRequest() {
	w.mu.Lock()
	running, cancel, finished := w.running, w.cancel, w.finished
	w.mu.Unlock()
	if finished {
		_, _, _ = pDestroyWindow.Call(uintptr(w.hwnd))
		return
	}
	if running {
		if !w.uninstall && messageBox(w.hwnd, "Stop the installation? Files already copied are removed again.", w.app.title(), mbYesNo|mbIconQuest|mbDefButton2) == idYes && cancel != nil {
			cancel()
		}
		return
	}
	if messageBox(w.hwnd, "Setup is not complete. If you exit now, "+w.app.product.Name+" will not be "+map[bool]string{true: "removed", false: "installed"}[w.uninstall]+".\r\n\r\nExit Setup?",
		w.app.title(), mbYesNo|mbIconQuest|mbDefButton2) == idYes {
		w.exitCode = wininstall.ExitCancelled
		_, _, _ = pDestroyWindow.Call(uintptr(w.hwnd))
	}
}

func (w *wizard) browse() {
	var name [windows.MAX_PATH]uint16
	bi := browseInfo{hwndOwner: w.hwnd, pszDisplayName: &name[0], lpszTitle: u16("Select the folder to install " + w.app.product.Name + " into."),
		ulFlags: 0x1 | 0x10 | 0x40} // BIF_RETURNONLYFSDIRS | BIF_EDITBOX | BIF_NEWDIALOGSTYLE
	pidl, _, _ := pSHBrowseForFolderW.Call(uintptr(unsafe.Pointer(&bi)))
	if pidl == 0 {
		return
	}
	defer pCoTaskMemFree.Call(pidl)
	var path [windows.MAX_PATH + 1]uint16
	if r, _, _ := pSHGetPathFromIDListW.Call(pidl, uintptr(unsafe.Pointer(&path[0]))); r == 0 {
		return
	}
	dir := windows.UTF16ToString(path[:])
	// Like other installers: picking "D:\Apps" means "D:\Apps\<Product>".
	if !strings.EqualFold(filepath.Base(dir), w.app.product.Name) {
		dir = filepath.Join(dir, w.app.product.Name)
	}
	setText(w.edDir, dir)
}

func (w *wizard) paint(hdc uintptr) {
	var cr rect
	_, _, _ = pGetClientRect.Call(uintptr(w.hwnd), uintptr(unsafe.Pointer(&cr)))
	fill := func(r rect, br uintptr) { _, _, _ = pFillRect.Call(hdc, uintptr(unsafe.Pointer(&r)), br) }
	hh, fh := w.px(headerH), w.px(footerH)
	fill(rect{0, 0, cr.right, hh}, w.brHeader)
	accent, _, _ := pCreateSolidBrush.Call(colAccent)
	fill(rect{0, hh - w.px(3), cr.right, hh}, accent)
	_, _, _ = pDeleteObject.Call(accent)
	fill(rect{0, hh, cr.right, cr.bottom - fh}, w.brBody)
	fill(rect{0, cr.bottom - fh, cr.right, cr.bottom}, w.brFooter)
	fill(rect{0, cr.bottom - fh, cr.right, cr.bottom - fh + w.px(1)}, w.brRule)

	if w.icon != 0 {
		_, _, _ = pDrawIconEx.Call(hdc, uintptr(cr.right-w.px(marginX)-w.px(40)), uintptr(w.px(12)), w.icon, uintptr(w.px(40)), uintptr(w.px(40)), 0, 0, diNormal)
	}
	if w.cur < 0 || w.cur >= len(w.pages) {
		return
	}
	pg := w.pages[w.cur]
	_, _, _ = pSetBkMode.Call(hdc, transparent)
	text := func(s string, font, color uintptr, r rect, flags uintptr) {
		old, _, _ := pSelectObject.Call(hdc, font)
		_, _, _ = pSetTextColor.Call(hdc, color)
		_, _, _ = pDrawTextW.Call(hdc, u16p(s), ^uintptr(0), uintptr(unsafe.Pointer(&r)), flags|dtNoPrefix)
		_, _, _ = pSelectObject.Call(hdc, old)
	}
	tr := rect{w.px(marginX), w.px(12), cr.right - w.px(marginX+52), w.px(38)}
	text(pg.title, w.fontTitle, rgb(0xFF, 0xFF, 0xFF), tr, dtSingleLine|dtVCenter|dtEndEllip)
	sr := rect{w.px(marginX), w.px(38), cr.right - w.px(marginX+52), w.px(60)}
	text(pg.subtitle, w.fontSub, colHeaderSub, sr, dtSingleLine|dtVCenter|dtEndEllip)
	br := rect{w.px(marginX), cr.bottom - fh, w.px(240), cr.bottom}
	text(w.app.product.Publisher+" Setup", w.fontBody, rgb(0x7A, 0x85, 0x99), br, dtSingleLine|dtVCenter)
}

func wndProc(hwnd, m, wp, lp uintptr) uintptr {
	w := theWizard
	if w == nil || w.hwnd == 0 {
		r, _, _ := pDefWindowProcW.Call(hwnd, m, wp, lp)
		return r
	}
	switch m {
	case wmPaint:
		var ps paintStruct
		hdc, _, _ := pBeginPaint.Call(hwnd, uintptr(unsafe.Pointer(&ps)))
		w.paint(hdc)
		_, _, _ = pEndPaint.Call(hwnd, uintptr(unsafe.Pointer(&ps)))
		return 0
	case wmEraseBkgnd:
		return 1
	case wmCtlColorStatic, wmCtlColorEdit:
		_, _, _ = pSetBkColor.Call(wp, colBody)
		_, _, _ = pSetTextColor.Call(wp, colText)
		return w.brBody
	case wmCtlColorBtn:
		return w.brFooter
	case wmCommand:
		id, code := loWord(wp), hiWord(wp)
		if code != 0 && id != idDir { // only BN_CLICKED
			break
		}
		switch int(id) {
		case idNext:
			if isWindowEnabled(w.btnNext) {
				w.next()
			}
		case idBack:
			if isWindowEnabled(w.btnBack) && w.cur > 0 {
				w.showPage(w.cur - 1)
			}
		case idCancel:
			w.cancelRequest()
		case idBrowse:
			w.browse()
		case idAccept, idDecline:
			enable(w.btnNext, checked(w.rbAccept))
		}
		return 0
	case msgProgress:
		w.onProgress()
		return 0
	case msgDone:
		w.onDone()
		return 0
	case wmClose:
		w.cancelRequest()
		return 0
	case wmDestroy:
		_, _, _ = pPostQuitMessage.Call(0)
		return 0
	}
	r, _, _ := pDefWindowProcW.Call(hwnd, m, wp, lp)
	return r
}

var pIsWindowEnabled = user32.NewProc("IsWindowEnabled")

func isWindowEnabled(h windows.HWND) bool {
	r, _, _ := pIsWindowEnabled.Call(uintptr(h))
	return r != 0
}
