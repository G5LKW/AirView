// Command mkinstaller assembles an AirView Windows installer: the setup stub
// (cmd/airview-setup) followed by a payload of product.json and the files to
// install (see internal/wininstall). It runs on any platform.
//
//	go run ./cmd/airview-setup/mkinstaller -stub airview-setup.exe \
//	    -product deploy/installer/products/station.json -version 1.2.3 \
//	    -file airview.exe=dist/airview.exe -file LICENSE.txt=deploy/installer/LICENSE.txt \
//	    -out AirView-Setup-1.2.3-windows-amd64.exe
//
// -id, -name, -default-dir, -var and -data-dir override the product
// definition, e.g. for a scratch identity in automated tests:
//
//	-id AirViewSetupTest -name "AirView SetupTest" -var datadir=D:\scratch\data
//
// -verify prints the payload of an existing installer instead.
package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"

	"airview/internal/wininstall"
)

type multi []string

func (m *multi) String() string     { return strings.Join(*m, ",") }
func (m *multi) Set(v string) error { *m = append(*m, v); return nil }

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "mkinstaller:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	fs := flag.NewFlagSet("mkinstaller", flag.ContinueOnError)
	stub := fs.String("stub", "", "setup stub `exe` (go build ./cmd/airview-setup)")
	productFile := fs.String("product", "", "product definition `json`")
	version := fs.String("version", "", "product `version` (DisplayVersion), e.g. 1.2.3")
	out := fs.String("out", "", "installer to write")
	id := fs.String("id", "", "override the product `id` (uninstall registry key)")
	name := fs.String("name", "", "override the product display `name`")
	defDir := fs.String("default-dir", "", "override the default destination `folder`")
	verify := fs.String("verify", "", "print the payload of this `installer` and exit")
	var files, vars, dataDirs multi
	fs.Var(&files, "file", "`dest=src`: install src as dest (relative, slash-separated); repeatable")
	fs.Var(&vars, "var", "`name=value`: set a product variable; repeatable")
	fs.Var(&dataDirs, "data-dir", "replace the product's data folders (repeatable)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *verify != "" {
		return printPayload(*verify)
	}
	if *stub == "" || *productFile == "" || *out == "" || *version == "" {
		fs.Usage()
		return errors.New("-stub, -product, -version and -out are required")
	}
	p, err := wininstall.LoadProduct(*productFile)
	if err != nil {
		return err
	}
	p.Version = *version
	if *id != "" {
		p.ID = *id
	}
	if *name != "" {
		p.Name = *name
	}
	if *defDir != "" {
		p.DefaultDir = *defDir
	}
	for _, v := range vars {
		k, val, ok := strings.Cut(v, "=")
		if !ok {
			return fmt.Errorf("-var %q: expected name=value", v)
		}
		if p.Vars == nil {
			p.Vars = map[string]string{}
		}
		p.Vars[k] = val
	}
	if len(dataDirs) > 0 {
		p.DataDirs = append([]string(nil), dataDirs...)
	}
	var specs []wininstall.FileSpec
	for _, f := range files {
		dest, src, ok := strings.Cut(f, "=")
		if !ok || dest == "" || src == "" {
			return fmt.Errorf("-file %q: expected dest=src", f)
		}
		specs = append(specs, wininstall.FileSpec{Src: src, Dest: dest})
	}
	if err := wininstall.Build(wininstall.BuildOptions{Stub: *stub, Product: p, Files: specs, Out: *out}); err != nil {
		return err
	}
	st, err := os.Stat(*out)
	if err != nil {
		return err
	}
	fmt.Printf("%s: %s %s, %d files, %.1f MB\n", *out, p.Name, p.Version, len(specs), float64(st.Size())/(1<<20))
	return nil
}

func printPayload(path string) error {
	pl, err := wininstall.OpenPayload(path)
	if err != nil {
		return err
	}
	defer pl.Close()
	p := pl.Product
	fmt.Printf("product:  %s %s (id %s, publisher %s)\nstub:     %d bytes\ndefault:  %s\n", p.Name, p.Version, p.ID, p.Publisher, pl.StubSize, p.DefaultDir)
	names := make([]string, 0, len(pl.Files))
	sizes := map[string]uint64{}
	for _, f := range pl.Files {
		n := wininstall.RelName(f)
		names = append(names, n)
		sizes[n] = f.UncompressedSize64
	}
	sort.Strings(names)
	for _, n := range names {
		fmt.Printf("  %10d  %s\n", sizes[n], n)
	}
	return nil
}
