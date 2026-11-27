# internal/fatfs — wiring

Pure-Go FAT32 (+ MBR) access to disk images, and (`xz/`) an .xz decoder. Used only by
`cmd/airview-piimage` (WP-PIIMG) to build Raspberry Pi images on Windows.

```go
f, _ := os.OpenFile("image.img", os.O_RDWR, 0)
st, _ := f.Stat()
m, _ := fatfs.ReadMBR(f, st.Size())      // partition table (never written)
bp, _ := m.FirstFAT()                    // boot partition
fs, _ := fatfs.Open(f, bp.Offset(), bp.Size(), fatfs.Options{Now: fixedTime})
fs.MkdirAll("airview")
fs.WriteFile("airview/firstboot.sh", data) // create or replace; syncs FATs + FSInfo
b, _ := fs.ReadFile("cmdline.txt")
probs := fs.Check()                       // fsck-like, read-only
```

- Every mutating call (`WriteFile`, `Mkdir`, `Remove`) writes the directory, the data and the changed
  FAT sectors (all copies) and FSInfo before returning; `Close` is a final `Sync`.
- Long names: VFAT entries are written for every name that is not an all-uppercase 8.3 name; short
  names get `~N` tails, unique per directory. Lookups ignore case.
- Not supported: FAT12/16, exFAT, GPT, files ≥ 4 GiB, concurrent use.

`xz.DecodeFile(dst io.WriterAt, src io.ReaderAt, size, workers)` decodes a multi-block file in
parallel (≈ 9 s for the 2.9 GiB Raspberry Pi OS image on 8 cores); `xz.Decode(w, r)` streams.

Tests: `go test ./internal/fatfs/...` (round trips, directory growth, fragmentation, corruption
detection, 7-Zip cross-check when installed, xz vectors made with xz-utils, corruption and
truncation). Opt-in: `AIRVIEW_PIIMG_TEST=<raspios.img>`, `AIRVIEW_XZ_TEST=<img.xz>` with
`AIRVIEW_XZ_SHA256=<hex>` (SHA-256 of the *decompressed* .img, not of the .xz).
