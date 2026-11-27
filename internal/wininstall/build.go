package wininstall

import (
	"archive/zip"
	"bytes"
	"compress/flate"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// FileSpec is one file of the payload: Src on the build machine, Dest the
// slash-separated path inside the install directory.
type FileSpec struct {
	Src  string
	Dest string
}

// BuildOptions describe one installer.
type BuildOptions struct {
	Stub    string // the setup stub executable (cmd/airview-setup)
	Product *Product
	Files   []FileSpec
	Out     string // the installer to write
	// Level is the deflate level (default flate.BestCompression).
	Level int
}

// buildEpoch is the modification time stored for every payload entry, so
// the same inputs give a byte-identical installer.
var buildEpoch = time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)

// Build writes an installer: the stub followed by the payload. It refuses
// invalid products, missing files, duplicate destinations and a stub that
// already carries a payload.
func Build(o BuildOptions) error {
	if o.Product == nil {
		return fmt.Errorf("build: no product")
	}
	if err := o.Product.Validate(); err != nil {
		return fmt.Errorf("build: %w", err)
	}
	stub, err := os.ReadFile(o.Stub)
	if err != nil {
		return fmt.Errorf("build: stub: %w", err)
	}
	if len(stub) < 2 || stub[0] != 'M' || stub[1] != 'Z' {
		return fmt.Errorf("build: %s is not a Windows executable", o.Stub)
	}
	if _, err := ReadPayload(bytes.NewReader(stub), int64(len(stub))); err == nil {
		return fmt.Errorf("build: %s already carries a payload; use the bare stub", o.Stub)
	}
	zipData, err := buildZip(o)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(o.Out), 0o755); err != nil {
		return err
	}
	tmp := o.Out + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	_, err = f.Write(stub)
	if err == nil {
		err = WritePayload(f, zipData, int64(len(stub)))
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		_ = os.Remove(o.Out)
		err = os.Rename(tmp, o.Out)
	}
	if err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("build: %w", err)
	}
	// Prove the result opens.
	p, err := OpenPayload(o.Out)
	if err != nil {
		return fmt.Errorf("build: verifying %s: %w", o.Out, err)
	}
	return p.Close()
}

func buildZip(o BuildOptions) ([]byte, error) {
	level := o.Level
	if level == 0 {
		level = flate.BestCompression
	}
	files := append([]FileSpec(nil), o.Files...)
	seen := map[string]bool{}
	for i := range files {
		files[i].Dest = strings.ReplaceAll(files[i].Dest, `\`, "/")
		d := files[i].Dest
		if !safeZipName(d) {
			return nil, fmt.Errorf("build: destination %q is not a clean relative path", d)
		}
		if seen[strings.ToLower(d)] {
			return nil, fmt.Errorf("build: destination %q given twice", d)
		}
		seen[strings.ToLower(d)] = true
	}
	if !seen[strings.ToLower(strings.ReplaceAll(o.Product.MainExe, `\`, "/"))] {
		return nil, fmt.Errorf("build: mainExe %q is not among the payload files", o.Product.MainExe)
	}
	if o.Product.Licence != "" && !seen[strings.ToLower(strings.ReplaceAll(o.Product.Licence, `\`, "/"))] {
		return nil, fmt.Errorf("build: licence %q is not among the payload files", o.Product.Licence)
	}
	if seen["uninstall.exe"] || seen[strings.ToLower(ReceiptName)] {
		return nil, fmt.Errorf("build: uninstall.exe and %s are written by the installer", ReceiptName)
	}
	sort.SliceStable(files, func(i, j int) bool { return files[i].Dest < files[j].Dest })

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	zw.RegisterCompressor(zip.Deflate, func(w io.Writer) (io.WriteCloser, error) {
		return flate.NewWriter(w, level)
	})
	pj, err := json.MarshalIndent(o.Product, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := addZip(zw, ProductEntry, bytes.NewReader(pj)); err != nil {
		return nil, err
	}
	for _, fs := range files {
		f, err := os.Open(fs.Src)
		if err != nil {
			return nil, fmt.Errorf("build: %w", err)
		}
		err = addZip(zw, FilesPrefix+fs.Dest, f)
		f.Close()
		if err != nil {
			return nil, fmt.Errorf("build: %s: %w", fs.Src, err)
		}
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func addZip(zw *zip.Writer, name string, r io.Reader) error {
	w, err := zw.CreateHeader(&zip.FileHeader{Name: name, Method: zip.Deflate, Modified: buildEpoch})
	if err != nil {
		return err
	}
	_, err = io.Copy(w, r)
	return err
}
