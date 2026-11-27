package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strings"

	"airview/internal/fatfs"
)

// roFile adapts an *os.File opened read-only to fatfs.Device.
type roFile struct{ f *os.File }

func (r roFile) ReadAt(p []byte, off int64) (int, error) { return r.f.ReadAt(p, off) }
func (r roFile) WriteAt([]byte, int64) (int, error)      { return 0, fatfs.ErrReadOnly }

type imageView struct {
	path string
	f    *os.File
	size int64
	mbr  fatfs.MBR
	boot fatfs.Partition
	fs   *fatfs.FS
}

func openImage(p string) (*imageView, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	v := &imageView{path: p, f: f, size: st.Size()}
	if v.mbr, err = fatfs.ReadMBR(f, v.size); err != nil {
		f.Close()
		return nil, err
	}
	var ok bool
	if v.boot, ok = v.mbr.FirstFAT(); !ok {
		f.Close()
		return nil, errors.New("no FAT boot partition")
	}
	if v.fs, err = fatfs.Open(roFile{f}, v.boot.Offset(), v.boot.Size(), fatfs.Options{ReadOnly: true}); err != nil {
		f.Close()
		return nil, err
	}
	return v, nil
}

func (v *imageView) Close() error { return v.f.Close() }

func mib(n int64) string { return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20)) }

// printTable prints the partition table.
func (v *imageView) printTable(w io.Writer) {
	fmt.Fprintf(w, "Image %s: %d bytes (%s), disk identifier 0x%08x\n", path.Base(strings.ReplaceAll(v.path, "\\", "/")), v.size, mib(v.size), v.mbr.DiskID)
	fmt.Fprintf(w, "  %-2s %-4s %-12s %10s %10s %12s  %s\n", "#", "boot", "type", "start", "sectors", "size", "PARTUUID")
	for _, p := range v.mbr.Partitions {
		b := ""
		if p.Bootable {
			b = "*"
		}
		fmt.Fprintf(w, "  %-2d %-4s %-12s %10d %10d %12s  %s\n", p.Index, b, p.TypeName(), p.StartLBA, p.Sectors, mib(p.Size()), v.mbr.PartUUID(p.Index))
	}
	fmt.Fprintf(w, "Boot partition: FAT32 label %q, cluster %d bytes, %s free of %s\n",
		v.fs.Label(), v.fs.ClusterSize(), mib(v.fs.FreeBytes()), mib(int64(v.fs.TotalClusters())*v.fs.ClusterSize()))
}

// listInjected prints vendor-data and /airview with sizes and SHA-256.
func (v *imageView) listInjected(w io.Writer) (map[string][]byte, error) {
	files := map[string][]byte{}
	fmt.Fprintln(w, "AirView files on the boot partition:")
	show := func(p string, e fatfs.DirEntry) error {
		if e.IsDir {
			fmt.Fprintf(w, "  %-44s %12s\n", p+"/", "<dir>")
			return nil
		}
		b, err := v.fs.ReadFile(p)
		if err != nil {
			return err
		}
		s := sha256.Sum256(b)
		fmt.Fprintf(w, "  %-44s %12d  %s  (8.3: %s)\n", p, len(b), hex.EncodeToString(s[:8]), e.ShortName)
		files[p] = b
		return nil
	}
	e, err := v.fs.Stat("vendor-data")
	if err != nil {
		fmt.Fprintln(w, "  (no vendor-data: not an AirView image)")
	} else if err := show("/vendor-data", e); err != nil {
		return nil, err
	}
	if _, err := v.fs.Stat("airview"); err != nil {
		fmt.Fprintln(w, "  (no /airview directory)")
		return files, nil
	}
	err = v.fs.Walk("airview", show)
	return files, err
}

// hashOutside hashes every byte of the image except the boot partition.
func hashOutside(f *os.File, size int64, boot fatfs.Partition) (string, error) {
	h := sha256.New()
	if _, err := io.Copy(h, io.NewSectionReader(f, 0, boot.Offset())); err != nil {
		return "", err
	}
	end := boot.Offset() + boot.Size()
	if _, err := io.Copy(h, io.NewSectionReader(f, end, size-end)); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// verifyAgainst checks an AirView image against its base image: same size,
// identical MBR sector and identical bytes outside the boot partition; the
// base's boot files are unchanged except for the additions; the boot file
// system is consistent; and the injected files equal the payload.
func verifyAgainst(w io.Writer, imgPath, basePath string, want []payloadFile) error {
	img, err := openImage(imgPath)
	if err != nil {
		return fmt.Errorf("%s: %w", imgPath, err)
	}
	defer img.Close()
	base, err := openImage(basePath)
	if err != nil {
		return fmt.Errorf("%s: %w", basePath, err)
	}
	defer base.Close()
	img.printTable(w)
	var problems []string
	if img.size != base.size {
		problems = append(problems, fmt.Sprintf("image size %d differs from the base (%d)", img.size, base.size))
	}
	if img.mbr.Raw != base.mbr.Raw {
		problems = append(problems, "the MBR sector differs from the base")
	} else {
		fmt.Fprintln(w, "MBR: identical to the base image (partition table, disk identifier, sizes unchanged)")
	}
	hi, err := hashOutside(img.f, img.size, img.boot)
	if err != nil {
		return err
	}
	hb, err := hashOutside(base.f, base.size, base.boot)
	if err != nil {
		return err
	}
	if hi != hb {
		problems = append(problems, "bytes outside the boot partition differ from the base")
	} else {
		fmt.Fprintf(w, "Outside the boot partition (MBR, gap, root file system): identical to the base, SHA-256 %s\n", hi[:16])
	}
	// Every base boot file is unchanged.
	changed := 0
	err = base.fs.Walk("/", func(p string, e fatfs.DirEntry) error {
		if e.IsDir {
			return nil
		}
		a, err := base.fs.ReadFile(p)
		if err != nil {
			return err
		}
		b, err := img.fs.ReadFile(p)
		if err != nil || !bytes.Equal(a, b) {
			changed++
			problems = append(problems, "base boot file changed or missing: "+p)
		}
		return nil
	})
	if err != nil {
		return err
	}
	if changed == 0 {
		fmt.Fprintln(w, "Base boot files (cmdline.txt, config.txt, user-data, kernels, overlays, ...): all unchanged")
	}
	files, err := img.listInjected(w)
	if err != nil {
		return err
	}
	for _, pf := range want {
		got, ok := files["/airview/"+pf.Name]
		if !ok || !bytes.Equal(got, pf.Data) {
			problems = append(problems, "injected file differs from its source: /airview/"+pf.Name)
		}
	}
	if b, ok := files["/vendor-data"]; !ok || !bytes.Equal(b, embedded("vendor-data")) {
		problems = append(problems, "/vendor-data missing or different")
	}
	warn := 0
	for _, p := range img.fs.Check() {
		if p.Severe {
			problems = append(problems, "boot file system: "+p.Text)
		} else {
			warn++
			fmt.Fprintln(w, "  fsck "+p.String())
		}
	}
	if len(problems) > 0 {
		for _, p := range problems {
			fmt.Fprintln(w, "PROBLEM: "+p)
		}
		return fmt.Errorf("%s failed verification (%d problems)", imgPath, len(problems))
	}
	fmt.Fprintf(w, "Boot file system check: consistent (%d warnings)\nVERIFIED: %s\n", warn, path.Base(strings.ReplaceAll(imgPath, "\\", "/")))
	return nil
}
