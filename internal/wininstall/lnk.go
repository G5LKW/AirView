package wininstall

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"unicode/utf16"
)

// Link is what ReadLink extracts from a shell link (.lnk). It is used to
// verify the shortcuts an install created (tests, the acceptance script).
//
// Format: [MS-SHLLINK] Shell Link Binary File Format (Microsoft Open
// Specifications): ShellLinkHeader, optional LinkTargetIDList, optional
// LinkInfo (LocalBasePath + CommonPathSuffix), then the StringData fields
// in a fixed order.
type Link struct {
	Target      string // LinkInfo LocalBasePath + CommonPathSuffix (may be empty)
	Description string // NAME_STRING
	RelPath     string
	WorkDir     string
	Args        string
	IconPath    string
}

const (
	lnkHasIDList    = 1 << 0
	lnkHasLinkInfo  = 1 << 1
	lnkHasName      = 1 << 2
	lnkHasRelPath   = 1 << 3
	lnkHasWorkDir   = 1 << 4
	lnkHasArgs      = 1 << 5
	lnkHasIcon      = 1 << 6
	lnkIsUnicode    = 1 << 7
	lnkHeaderSize   = 0x4C
	lnkLocalBaseUni = 0x24 // LinkInfoHeaderSize with the Unicode offsets
)

var lnkCLSID = []byte{0x01, 0x14, 0x02, 0x00, 0x00, 0x00, 0x00, 0x00, 0xC0, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x46}

// ReadLink parses a .lnk file.
func ReadLink(path string) (*Link, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return ParseLink(b)
}

// ParseLink parses .lnk data.
func ParseLink(b []byte) (*Link, error) {
	bad := errors.New("lnk: truncated or malformed")
	if len(b) < lnkHeaderSize || binary.LittleEndian.Uint32(b) != lnkHeaderSize || !bytes.Equal(b[4:20], lnkCLSID) {
		return nil, errors.New("lnk: not a shell link")
	}
	flags := binary.LittleEndian.Uint32(b[0x14:])
	off := lnkHeaderSize
	if flags&lnkHasIDList != 0 {
		if off+2 > len(b) {
			return nil, bad
		}
		off += 2 + int(binary.LittleEndian.Uint16(b[off:]))
	}
	l := &Link{}
	if flags&lnkHasLinkInfo != 0 {
		if off+0x1C > len(b) {
			return nil, bad
		}
		size := int(binary.LittleEndian.Uint32(b[off:]))
		if size < 0x1C || off+size > len(b) {
			return nil, bad
		}
		li := b[off : off+size]
		hdr := binary.LittleEndian.Uint32(li[4:])
		liFlags := binary.LittleEndian.Uint32(li[8:])
		if liFlags&1 != 0 { // VolumeIDAndLocalBasePath
			if hdr >= lnkLocalBaseUni && size >= 0x24 {
				base := utf16z(li, int(binary.LittleEndian.Uint32(li[0x1C:])))
				suffix := utf16z(li, int(binary.LittleEndian.Uint32(li[0x20:])))
				l.Target = base + suffix
			} else {
				l.Target = ansiz(li, int(binary.LittleEndian.Uint32(li[0x10:]))) + ansiz(li, int(binary.LittleEndian.Uint32(li[0x18:])))
			}
		}
		off += size
	}
	uni := flags&lnkIsUnicode != 0
	for _, f := range []struct {
		bit uint32
		dst *string
	}{{lnkHasName, &l.Description}, {lnkHasRelPath, &l.RelPath}, {lnkHasWorkDir, &l.WorkDir}, {lnkHasArgs, &l.Args}, {lnkHasIcon, &l.IconPath}} {
		if flags&f.bit == 0 {
			continue
		}
		if off+2 > len(b) {
			return nil, bad
		}
		n := int(binary.LittleEndian.Uint16(b[off:]))
		off += 2
		if uni {
			if off+2*n > len(b) {
				return nil, bad
			}
			u := make([]uint16, n)
			for i := range u {
				u[i] = binary.LittleEndian.Uint16(b[off+2*i:])
			}
			*f.dst = string(utf16.Decode(u))
			off += 2 * n
		} else {
			if off+n > len(b) {
				return nil, bad
			}
			*f.dst = string(b[off : off+n])
			off += n
		}
	}
	if l.Target == "" && l.RelPath == "" {
		return l, fmt.Errorf("lnk: no target path recorded")
	}
	return l, nil
}

func utf16z(b []byte, off int) string {
	if off <= 0 || off >= len(b) {
		return ""
	}
	var u []uint16
	for i := off; i+1 < len(b); i += 2 {
		c := binary.LittleEndian.Uint16(b[i:])
		if c == 0 {
			break
		}
		u = append(u, c)
	}
	return string(utf16.Decode(u))
}

func ansiz(b []byte, off int) string {
	if off <= 0 || off >= len(b) {
		return ""
	}
	end := bytes.IndexByte(b[off:], 0)
	if end < 0 {
		return string(b[off:])
	}
	return string(b[off : off+end])
}
