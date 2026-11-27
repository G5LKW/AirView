// Command mkicon writes the AirView Feeder icon (internal/wintray's drawing)
// as a multi-size .ico for the installer (SetupIconFile) and the portable
// ZIP, and optionally as PNG files.
//
//	go run ./deploy/feeder/windows/mkicon -o airview-feeder.ico [-png 256 -png-out icon.png]
package main

import (
	"flag"
	"fmt"
	"os"

	"airview/internal/wintray"
)

func main() {
	out := flag.String("o", "airview-feeder.ico", "output .ico `file`")
	pngSize := flag.Int("png", 0, "also write a PNG of this `size`")
	pngOut := flag.String("png-out", "airview-feeder.png", "PNG output `file`")
	flag.Parse()
	if err := os.WriteFile(*out, wintray.ICO([]int{16, 20, 24, 32, 40, 48, 64, 256}, wintray.LevelNone), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if *pngSize > 0 {
		if err := os.WriteFile(*pngOut, wintray.PNG(*pngSize, wintray.LevelNone), 0o644); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}
}
