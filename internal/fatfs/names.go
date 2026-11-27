package fatfs

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf16"
)

// Name handling follows the published Microsoft FAT specification (FAT32 File
// System Specification 1.03, "FAT: General Overview of On-Disk Format"): 8.3
// short names, the basis-name generation with numeric tails, long-name
// entries of 13 UCS-2 characters and their short-name checksum.

const (
	maxLongName = 255
	lfnChars    = 13
)

// invalid in any name; also control characters < 0x20.
const invalidLong = "\"*/:<>?\\|"

// valid in short names besides A-Z and 0-9 (no lowercase, no space for our generator)
const shortExtra = "$%'-_@~`!(){}^#&"

// ValidName reports whether s can be a FAT long file name.
func ValidName(s string) error {
	if s == "" || s == "." || s == ".." {
		return fmt.Errorf("fatfs: invalid file name %q", s)
	}
	if len(utf16.Encode([]rune(s))) > maxLongName {
		return fmt.Errorf("fatfs: file name %q is longer than 255 characters", s)
	}
	for _, r := range s {
		if r < 0x20 || strings.ContainsRune(invalidLong, r) {
			return fmt.Errorf("fatfs: file name %q contains an invalid character %q", s, r)
		}
	}
	if strings.HasSuffix(s, " ") || strings.HasSuffix(s, ".") {
		return fmt.Errorf("fatfs: file name %q ends with a space or a period", s)
	}
	return nil
}

func shortChar(r rune) bool {
	return (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || strings.ContainsRune(shortExtra, r)
}

// shortName is an 11-byte 8.3 name as stored on disk.
type shortName [11]byte

func (s shortName) String() string {
	base := strings.TrimRight(string(s[:8]), " ")
	ext := strings.TrimRight(string(s[8:]), " ")
	if base != "" && base[0] == 0x05 {
		base = "\xe5" + base[1:]
	}
	if ext == "" {
		return base
	}
	return base + "." + ext
}

// display applies the NT case flags (0x08 lowercase base, 0x10 lowercase extension).
func (s shortName) display(ntres byte) string {
	base := strings.TrimRight(string(s[:8]), " ")
	ext := strings.TrimRight(string(s[8:]), " ")
	if ntres&0x08 != 0 {
		base = strings.ToLower(base)
	}
	if ntres&0x10 != 0 {
		ext = strings.ToLower(ext)
	}
	if ext == "" {
		return base
	}
	return base + "." + ext
}

func (s shortName) checksum() byte {
	var sum byte
	for _, c := range s {
		sum = (sum>>1 | sum<<7) + c
	}
	return sum
}

// exactShort returns the short name and NT case flags when name can be stored
// as a short entry alone (8.3, valid characters, each part all upper- or all
// lower-case), as Windows and Linux do.
func exactShort(name string) (shortName, byte, bool) {
	var sn shortName
	base, ext := name, ""
	if i := strings.LastIndexByte(name, '.'); i >= 0 {
		base, ext = name[:i], name[i+1:]
		if strings.ContainsRune(base, '.') || ext == "" {
			return sn, 0, false
		}
	}
	if len(base) == 0 || len(base) > 8 || len(ext) > 3 {
		return sn, 0, false
	}
	var flags byte
	part := func(p string, bit byte, dst []byte) bool {
		up := strings.ToUpper(p)
		lo := strings.ToLower(p)
		switch {
		case p == up:
		case p == lo:
			flags |= bit
		default:
			return false
		}
		for i, r := range up {
			if r > 0x7f || !shortChar(r) {
				return false
			}
			dst[i] = byte(r)
		}
		return true
	}
	for i := range sn {
		sn[i] = ' '
	}
	if !part(base, 0x08, sn[:8]) || !part(ext, 0x10, sn[8:]) {
		return sn, 0, false
	}
	if sn[0] == 0xE5 {
		sn[0] = 0x05
	}
	return sn, flags, true
}

// basisName builds the uppercase basis name; lossy reports a changed or
// truncated name (which then needs a numeric tail).
func basisName(name string) (base, ext string, lossy bool) {
	s := strings.TrimLeft(name, ".")
	if len(s) != len(name) {
		lossy = true
	}
	b, e := s, ""
	if i := strings.LastIndexByte(s, '.'); i >= 0 {
		b, e = s[:i], s[i+1:]
	}
	conv := func(p string, max int) string {
		var out []byte
		for _, r := range strings.ToUpper(p) {
			switch {
			case r == ' ' || r == '.':
				lossy = true
				continue
			case r > 0x7f || !shortChar(r):
				lossy = true
				r = '_'
			}
			if len(out) == max {
				lossy = true
				break
			}
			out = append(out, byte(r))
		}
		return string(out)
	}
	base = conv(b, 8)
	ext = conv(e, 3)
	if base == "" {
		base = "_"
		lossy = true
	}
	return base, ext, lossy
}

// generateShort picks a unique short name for a long name, using numeric
// tails "~1".."~999999". exists reports a short name already in the directory.
func generateShort(name string, exists func(shortName) bool) (shortName, error) {
	base, ext, lossy := basisName(name)
	mk := func(b string) shortName {
		var sn shortName
		for i := range sn {
			sn[i] = ' '
		}
		copy(sn[:8], b)
		copy(sn[8:], ext)
		return sn
	}
	if !lossy {
		if sn := mk(base); !exists(sn) {
			return sn, nil
		}
	}
	for n := 1; n <= 999999; n++ {
		tail := "~" + strconv.Itoa(n)
		b := base
		if len(b)+len(tail) > 8 {
			b = b[:8-len(tail)]
		}
		if sn := mk(b + tail); !exists(sn) {
			return sn, nil
		}
	}
	return shortName{}, errors.New("fatfs: no free short name")
}

// lfnEntries encodes the long-name entries for name (in on-disk order: the
// last part first, flagged 0x40).
func lfnEntries(name string, sum byte) [][32]byte {
	u := utf16.Encode([]rune(name))
	n := (len(u) + lfnChars - 1) / lfnChars
	padded := make([]uint16, n*lfnChars)
	for i := range padded {
		switch {
		case i < len(u):
			padded[i] = u[i]
		case i == len(u):
			padded[i] = 0
		default:
			padded[i] = 0xFFFF
		}
	}
	out := make([][32]byte, 0, n)
	for k := n; k >= 1; k-- {
		var e [32]byte
		ord := byte(k)
		if k == n {
			ord |= 0x40
		}
		e[0] = ord
		e[11] = attrLFN
		e[13] = sum
		chars := padded[(k-1)*lfnChars : k*lfnChars]
		put := func(off int, cs []uint16) {
			for i, c := range cs {
				e[off+2*i] = byte(c)
				e[off+2*i+1] = byte(c >> 8)
			}
		}
		put(1, chars[0:5])
		put(14, chars[5:11])
		put(28, chars[11:13])
		out = append(out, e)
	}
	return out
}

// lfnChunk extracts the 13 characters of one long-name entry.
func lfnChunk(e []byte) []uint16 {
	var cs []uint16
	get := func(off, n int) {
		for i := 0; i < n; i++ {
			cs = append(cs, uint16(e[off+2*i])|uint16(e[off+2*i+1])<<8)
		}
	}
	get(1, 5)
	get(14, 6)
	get(28, 2)
	return cs
}
