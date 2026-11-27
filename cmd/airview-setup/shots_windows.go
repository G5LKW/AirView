//go:build windows

package main

import (
	"fmt"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"

	"airview/internal/wininstall"
)

var (
	dwmapi                 = windows.NewLazySystemDLL("dwmapi.dll")
	pDwmGetWindowAttribute = dwmapi.NewProc("DwmGetWindowAttribute")
)

// screenshots renders every wizard page to PNG files in dir (the /SHOTS=
// switch, for documentation and the acceptance record). Nothing is
// installed or removed: the pages are shown with sample progress values.
func (a *app) screenshots(dir string) int {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fail(a.args, err.Error(), wininstall.ExitFailed)
	}
	p := a.product
	n := 0
	if a.payload != nil {
		scope := wininstall.PerMachine
		if a.args.ScopeSet {
			scope = a.args.Scope
		}
		o := wininstall.DefaultOptions(p, scope)
		d, err := wininstall.DefaultDir(a.sys, p, scope)
		if err != nil {
			return fail(a.args, err.Error(), wininstall.ExitFailed)
		}
		a.defaultDir, o.Dir = d, d
		if p.Licence != "" {
			if rc, err := a.payload.Open(p.Licence); err == nil {
				b := make([]byte, 1<<20)
				k, _ := rc.Read(b)
				rc.Close()
				a.licence = toCRLF(string(b[:k]))
			}
		}
		w := newWizard(a, false)
		w.opts = o
		w.options = a.optionList(scope)
		if err := w.create(); err != nil {
			return fail(a.args, err.Error(), wininstall.ExitFailed)
		}
		for i, pg := range w.pages {
			switch pg.kind {
			case pgLicence:
				setChecked(w.rbDecline, false)
				setChecked(w.rbAccept, true)
			case pgProgress:
				w.mu.Lock()
				w.status, w.frac = "Installing "+p.MainExe, 0.62
				w.mu.Unlock()
			}
			w.showPage(i)
			switch pg.kind {
			case pgProgress:
				w.onProgress()
			case pgFinish:
				setText(w.stFinish, "Setup has finished installing "+p.Name+" on your computer. The application may be launched by selecting the installed shortcuts.\r\n\r\nClick Finish to exit Setup.")
			}
			n++
			if err := w.shot(filepath.Join(dir, fmt.Sprintf("%s-setup-%d-%s.png", p.ID, i+1, pageName(pg.kind)))); err != nil {
				return fail(a.args, err.Error(), wininstall.ExitFailed)
			}
		}
		_, _, _ = pDestroyWindow.Call(uintptr(w.hwnd))
		w.pump()

		// The uninstaller's pages, for a receipt as this install would write it.
		f, _ := a.sys.Folders(scope)
		e := wininstall.Expander{Folders: f, App: d, Port: o.Port, Product: p}
		if rp, err := wininstall.ResolveNames(p, &e); err == nil {
			p = rp
		}
		r := &wininstall.Receipt{Product: *p, Scope: scope.String(), Dir: d, Service: o.Service, Firewall: ""}
		if p.Service != nil {
			r.ServiceName = p.Service.Name
		}
		if p.Tray != nil && o.Tray {
			r.TrayRunValue = p.Tray.RunValue
		}
		for _, dd := range p.DataDirs {
			if v, err := e.Expand(dd); err == nil {
				r.DataDirs = append(r.DataDirs, v)
			}
		}
		a.receipt, a.dir = r, d
	}
	w := newWizard(a, true)
	if err := w.create(); err != nil {
		return fail(a.args, err.Error(), wininstall.ExitFailed)
	}
	for i, pg := range w.pages {
		w.showPage(i)
		switch pg.kind {
		case pgProgress:
			w.mu.Lock()
			w.status, w.frac = "Removing files...", 0.45
			w.mu.Unlock()
			w.onProgress()
		case pgFinish:
			setText(w.stFinish, p.Name+" was removed from your computer.\r\n\r\nKept (delete by hand if you no longer need it):\r\n      "+firstOr(a.receipt.DataDirs, "(nothing)"))
		}
		n++
		if err := w.shot(filepath.Join(dir, fmt.Sprintf("%s-uninstall-%d-%s.png", p.ID, i+1, pageName(pg.kind)))); err != nil {
			return fail(a.args, err.Error(), wininstall.ExitFailed)
		}
	}
	_, _, _ = pDestroyWindow.Call(uintptr(w.hwnd))
	fmt.Printf("%d screenshots written to %s\n", n, dir)
	return wininstall.ExitOK
}

func firstOr(l []string, def string) string {
	if len(l) == 0 {
		return def
	}
	return l[0]
}

func pageName(k pageKind) string {
	return map[pageKind]string{pgWelcome: "welcome", pgLicence: "licence", pgDir: "destination", pgOptions: "options",
		pgReady: "ready", pgProgress: "progress", pgFinish: "finished", pgUninstConfirm: "confirm"}[k]
}

// shot shows the window, lets it paint and saves its image as PNG.
func (w *wizard) shot(path string) error {
	const swShowNoActivate = 4
	_, _, _ = pShowWindow.Call(uintptr(w.hwnd), swShowNoActivate)
	_, _, _ = pUpdateWindow.Call(uintptr(w.hwnd))
	for i := 0; i < 6; i++ {
		w.pump()
		time.Sleep(60 * time.Millisecond)
	}
	var wr, ext rect
	_, _, _ = pGetWindowRect.Call(uintptr(w.hwnd), uintptr(unsafe.Pointer(&wr)))
	ext = wr
	const dwmwaExtendedFrameBounds = 9
	_, _, _ = pDwmGetWindowAttribute.Call(uintptr(w.hwnd), dwmwaExtendedFrameBounds, uintptr(unsafe.Pointer(&ext)), unsafe.Sizeof(ext))
	width, height := wr.right-wr.left, wr.bottom-wr.top
	screen, _, _ := pGetDC.Call(0)
	defer pReleaseDC.Call(0, screen)
	mem, _, _ := pCreateCompatibleDC.Call(screen)
	defer pDeleteDC.Call(mem)
	bi := bitmapInfoHeader{biWidth: width, biHeight: -height, biPlanes: 1, biBitCount: 32}
	bi.biSize = uint32(unsafe.Sizeof(bi))
	var bits unsafe.Pointer
	hbm, _, _ := pCreateDIBSection.Call(mem, uintptr(unsafe.Pointer(&bi)), 0, uintptr(unsafe.Pointer(&bits)), 0, 0)
	if hbm == 0 || bits == nil {
		return fmt.Errorf("CreateDIBSection failed")
	}
	defer pDeleteObject.Call(hbm)
	old, _, _ := pSelectObject.Call(mem, hbm)
	const pwRenderFullContent = 2
	r, _, _ := pPrintWindow.Call(uintptr(w.hwnd), mem, pwRenderFullContent)
	_, _, _ = pSelectObject.Call(mem, old)
	if r == 0 {
		return fmt.Errorf("PrintWindow failed")
	}
	pix := unsafe.Slice((*byte)(bits), int(width*height*4))
	x0, y0 := int(ext.left-wr.left), int(ext.top-wr.top)
	cw, ch := int(ext.right-ext.left), int(ext.bottom-ext.top)
	if x0 < 0 || y0 < 0 || x0+cw > int(width) || y0+ch > int(height) || cw <= 0 || ch <= 0 {
		x0, y0, cw, ch = 0, 0, int(width), int(height)
	}
	img := image.NewRGBA(image.Rect(0, 0, cw, ch))
	for y := 0; y < ch; y++ {
		for x := 0; x < cw; x++ {
			o := ((y+y0)*int(width) + x + x0) * 4
			img.SetRGBA(x, y, color.RGBA{pix[o+2], pix[o+1], pix[o], 255})
		}
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	if err := png.Encode(f, img); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}
