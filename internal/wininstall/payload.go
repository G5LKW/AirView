package wininstall

import (
	"archive/zip"
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strings"
)

// An installer is the setup stub (cmd/airview-setup, a normal PE file)
// followed by an overlay:
//
//	stub PE image
//	payload   ZIP archive: product.json + files/<install-relative path>
//	padding   0-7 zero bytes, so the overlay ends 8-byte aligned
//	trailer   16 bytes: payload length (uint32 LE) | padding length (uint32 LE) | "AVSETUP1"
//
// Windows ignores data after the last section, so the stub runs normally
// and finds its payload at the end of its own file. Authenticode signing
// appends the certificate table after the overlay and records it in the
// PE security directory; the reader honours that, so signed installers
// work too (the 8-byte alignment means signtool inserts no padding).
const (
	trailerMagic = "AVSETUP1"
	trailerSize  = 16
	// ProductEntry is the product definition inside the payload.
	ProductEntry = "product.json"
	// FilesPrefix prefixes the files to install inside the payload.
	FilesPrefix = "files/"
)

// ErrNoPayload: the executable is a bare stub (an uninstaller).
var ErrNoPayload = errors.New("no installer payload")

// Payload is an opened installer payload.
type Payload struct {
	Product *Product
	Files   []*zip.File // the files/ entries, in archive order
	// StubSize is the size of the stub PE image without the overlay: the
	// uninstaller is the first StubSize bytes of the installer.
	StubSize int64
	zr       *zip.Reader
	closer   io.Closer
}

// Close releases the underlying file.
func (p *Payload) Close() error {
	if p.closer != nil {
		return p.closer.Close()
	}
	return nil
}

// Open returns a payload file by its payload path (without files/).
func (p *Payload) Open(rel string) (io.ReadCloser, error) {
	for _, f := range p.Files {
		if f.Name == FilesPrefix+rel {
			return f.Open()
		}
	}
	return nil, fmt.Errorf("payload: %s not found", rel)
}

// TotalSize is the uncompressed size of the files to install.
func (p *Payload) TotalSize() int64 {
	var n int64
	for _, f := range p.Files {
		n += int64(f.UncompressedSize64)
	}
	return n
}

// RelName is the install-relative path (slash-separated) of a payload file.
func RelName(f *zip.File) string { return strings.TrimPrefix(f.Name, FilesPrefix) }

// OpenPayload opens the payload appended to the executable at exePath.
func OpenPayload(exePath string) (*Payload, error) {
	f, err := os.Open(exePath)
	if err != nil {
		return nil, err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	p, err := ReadPayload(f, st.Size())
	if err != nil {
		f.Close()
		return nil, err
	}
	p.closer = f
	return p, nil
}

// ReadPayload locates and opens the payload in an installer image of the
// given size. It returns ErrNoPayload for a bare stub.
func ReadPayload(r io.ReaderAt, size int64) (*Payload, error) {
	end, err := overlayEnd(r, size)
	if err != nil {
		return nil, err
	}
	if end < trailerSize {
		return nil, ErrNoPayload
	}
	var tr [trailerSize]byte
	if _, err := r.ReadAt(tr[:], end-trailerSize); err != nil {
		return nil, err
	}
	if string(tr[8:]) != trailerMagic {
		return nil, ErrNoPayload
	}
	n := int64(binary.LittleEndian.Uint32(tr[:4]))
	pad := int64(binary.LittleEndian.Uint32(tr[4:8]))
	start := end - trailerSize - pad - n
	if n <= 0 || pad > 7 || start <= 0 {
		return nil, fmt.Errorf("payload: corrupt trailer (length %d)", n)
	}
	zr, err := zip.NewReader(io.NewSectionReader(r, start, n), n)
	if err != nil {
		return nil, fmt.Errorf("payload: %w", err)
	}
	p := &Payload{StubSize: start, zr: zr}
	for _, f := range zr.File {
		switch {
		case f.Name == ProductEntry:
			rc, err := f.Open()
			if err != nil {
				return nil, err
			}
			b, err := io.ReadAll(io.LimitReader(rc, 1<<20))
			rc.Close()
			if err != nil {
				return nil, err
			}
			if p.Product, err = ParseProduct(b); err != nil {
				return nil, err
			}
		case strings.HasPrefix(f.Name, FilesPrefix) && !strings.HasSuffix(f.Name, "/"):
			if !safeZipName(RelName(f)) {
				return nil, fmt.Errorf("payload: unsafe file name %q", f.Name)
			}
			p.Files = append(p.Files, f)
		}
	}
	if p.Product == nil {
		return nil, fmt.Errorf("payload: %s missing", ProductEntry)
	}
	return p, nil
}

// safeZipName refuses absolute paths, drive letters, backslashes and ".."
// (zip-slip) in payload names.
func safeZipName(n string) bool {
	if n == "" || strings.ContainsAny(n, `\:`) || strings.HasPrefix(n, "/") {
		return false
	}
	return path.Clean(n) == n && !strings.HasPrefix(n, "../") && n != ".."
}

// overlayEnd is where the overlay ends: the start of the Authenticode
// certificate table when the PE has one at the end of the file, else the
// file size. Non-PE input (tests) simply ends at size.
func overlayEnd(r io.ReaderAt, size int64) (int64, error) {
	var dos [64]byte
	if size < 64 {
		return size, nil
	}
	if _, err := r.ReadAt(dos[:], 0); err != nil {
		return 0, err
	}
	if dos[0] != 'M' || dos[1] != 'Z' {
		return size, nil
	}
	peOff := int64(binary.LittleEndian.Uint32(dos[0x3c:]))
	var hdr [24 + 2]byte // signature, COFF header, optional-header magic
	if peOff <= 0 || peOff+int64(len(hdr)) > size {
		return size, nil
	}
	if _, err := r.ReadAt(hdr[:], peOff); err != nil {
		return 0, err
	}
	if !bytes.Equal(hdr[:4], []byte("PE\x00\x00")) {
		return size, nil
	}
	opt := peOff + 24
	var ddOff int64
	switch binary.LittleEndian.Uint16(hdr[24:]) {
	case 0x10b: // PE32
		ddOff = opt + 96
	case 0x20b: // PE32+
		ddOff = opt + 112
	default:
		return size, nil
	}
	var sec [8]byte // data directory 4: IMAGE_DIRECTORY_ENTRY_SECURITY
	if ddOff+4*8+8 > size {
		return size, nil
	}
	if _, err := r.ReadAt(sec[:], ddOff+4*8); err != nil {
		return 0, err
	}
	off := int64(binary.LittleEndian.Uint32(sec[:4]))
	n := int64(binary.LittleEndian.Uint32(sec[4:]))
	if n > 0 && off > 0 && off+n <= size && off+n >= size-7 {
		return off, nil
	}
	return size, nil
}

// WritePayload appends a payload (a complete ZIP archive) and the trailer
// to w, which must be positioned at the end of the stub (stubSize bytes
// written so far, used for alignment).
func WritePayload(w io.Writer, zipData []byte, stubSize int64) error {
	n := int64(len(zipData))
	if n == 0 || n >= 1<<32 {
		return fmt.Errorf("payload: size %d out of range (1 B to 4 GB)", n)
	}
	if _, err := w.Write(zipData); err != nil {
		return err
	}
	pad := (8 - (stubSize+n)%8) % 8
	if _, err := w.Write(make([]byte, pad)); err != nil {
		return err
	}
	var tr [trailerSize]byte
	binary.LittleEndian.PutUint32(tr[:4], uint32(n))
	binary.LittleEndian.PutUint32(tr[4:8], uint32(pad))
	copy(tr[8:], trailerMagic)
	_, err := w.Write(tr[:])
	return err
}

// StubLength returns the size of the bare stub inside an installer file
// (the whole file when it has no payload).
func StubLength(exePath string) (int64, error) {
	p, err := OpenPayload(exePath)
	if errors.Is(err, ErrNoPayload) {
		st, err := os.Stat(exePath)
		if err != nil {
			return 0, err
		}
		end := st.Size()
		if f, err := os.Open(exePath); err == nil {
			end, err = overlayEnd(f, st.Size())
			f.Close()
			if err != nil {
				return 0, err
			}
		}
		return end, nil
	}
	if err != nil {
		return 0, err
	}
	defer p.Close()
	return p.StubSize, nil
}
