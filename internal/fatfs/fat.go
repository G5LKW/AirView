package fatfs

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"sort"
	"strings"
	"time"
	"unicode/utf16"
)

// Device is the storage a file system lives on (an *os.File of a disk image).
type Device interface {
	io.ReaderAt
	io.WriterAt
}

const (
	attrReadOnly = 0x01
	attrHidden   = 0x02
	attrSystem   = 0x04
	attrVolumeID = 0x08
	attrDir      = 0x10
	attrArchive  = 0x20
	attrLFN      = 0x0F

	fatMask   = 0x0FFFFFFF
	fatEOC    = 0x0FFFFFFF
	fatEOCMin = 0x0FFFFFF8
	fatBad    = 0x0FFFFFF7
)

// Errors.
var (
	ErrNotFAT32 = errors.New("fatfs: not a FAT32 file system")
	ErrNoSpace  = errors.New("fatfs: no space left on the file system")
	ErrReadOnly = errors.New("fatfs: file system opened read-only")
)

// FS is a FAT32 file system inside a device region. It keeps the first FAT in
// memory; Sync (or Close) writes changed FAT sectors to every FAT copy and
// refreshes the FSInfo sector. FS is not safe for concurrent use.
type FS struct {
	dev      Device
	base     int64
	size     int64
	readOnly bool

	bps, spc     uint32 // bytes per sector, sectors per cluster
	reserved     uint32
	nfats        uint32
	fatSectors   uint32
	totalSectors uint32
	rootCluster  uint32
	fsInfoSector uint32
	clusterSize  int64
	dataStart    int64  // byte offset of cluster 2 (relative to base)
	clusters     uint32 // number of data clusters (valid: 2..clusters+1)
	volumeID     uint32
	label        string
	fat          []uint32
	dirtyLo      int // dirty FAT entry range [dirtyLo, dirtyHi)
	dirtyHi      int
	nextFree     uint32
	freeCount    uint32
	fsInfoFree   uint32 // value found in FSInfo at open (0xFFFFFFFF unknown)
	fsInfoValid  bool
	modified     bool
	now          func() time.Time
}

// Options for Open.
type Options struct {
	ReadOnly bool
	// Now supplies the time stamped on created and modified entries (UTC is
	// stored, the Linux vfat default). Default time.Now.
	Now func() time.Time
}

// Open opens the FAT32 file system at [offset, offset+size) of dev.
func Open(dev Device, offset, size int64, o Options) (*FS, error) {
	f := &FS{dev: dev, base: offset, size: size, readOnly: o.ReadOnly, now: o.Now}
	if f.now == nil {
		f.now = time.Now
	}
	var bs [512]byte
	if _, err := dev.ReadAt(bs[:], offset); err != nil {
		return nil, fmt.Errorf("fatfs: reading the boot sector: %w", err)
	}
	if bs[510] != 0x55 || bs[511] != 0xAA || (bs[0] != 0xEB && bs[0] != 0xE9) {
		return nil, ErrNotFAT32
	}
	le16 := func(o int) uint32 { return uint32(binary.LittleEndian.Uint16(bs[o:])) }
	le32 := func(o int) uint32 { return binary.LittleEndian.Uint32(bs[o:]) }
	f.bps = le16(11)
	f.spc = uint32(bs[13])
	f.reserved = le16(14)
	f.nfats = uint32(bs[16])
	rootEnt := le16(17)
	tot16 := le16(19)
	fat16 := le16(22)
	f.totalSectors = le32(32)
	f.fatSectors = le32(36)
	f.rootCluster = le32(44)
	f.fsInfoSector = le16(48)
	switch f.bps {
	case 512, 1024, 2048, 4096:
	default:
		return nil, fmt.Errorf("%w: bytes per sector %d", ErrNotFAT32, f.bps)
	}
	if f.spc == 0 || f.spc&(f.spc-1) != 0 || f.spc > 128 || f.reserved == 0 || f.nfats == 0 {
		return nil, fmt.Errorf("%w: invalid BPB", ErrNotFAT32)
	}
	if rootEnt != 0 || fat16 != 0 || tot16 != 0 || f.fatSectors == 0 || f.totalSectors == 0 {
		return nil, fmt.Errorf("%w (FAT12/16 layout)", ErrNotFAT32)
	}
	if binary.LittleEndian.Uint16(bs[42:]) != 0 {
		return nil, fmt.Errorf("%w: unsupported FAT32 version", ErrNotFAT32)
	}
	if int64(f.totalSectors)*int64(f.bps) > size {
		return nil, fmt.Errorf("fatfs: file system (%d sectors) is larger than its partition (%d bytes)", f.totalSectors, size)
	}
	f.clusterSize = int64(f.bps) * int64(f.spc)
	dataSec := f.reserved + f.nfats*f.fatSectors
	if dataSec >= f.totalSectors {
		return nil, fmt.Errorf("%w: no data region", ErrNotFAT32)
	}
	f.dataStart = int64(dataSec) * int64(f.bps)
	f.clusters = (f.totalSectors - dataSec) / f.spc
	maxEntries := uint64(f.fatSectors) * uint64(f.bps) / 4
	if uint64(f.clusters)+2 > maxEntries {
		return nil, fmt.Errorf("%w: FAT too small for %d clusters", ErrNotFAT32, f.clusters)
	}
	if bs[66] == 0x29 {
		f.volumeID = le32(67)
		f.label = strings.TrimRight(string(bs[71:82]), " ")
	}
	// Load FAT 0.
	n := int(f.clusters) + 2
	raw := make([]byte, n*4)
	if _, err := dev.ReadAt(raw, offset+int64(f.reserved)*int64(f.bps)); err != nil {
		return nil, fmt.Errorf("fatfs: reading the FAT: %w", err)
	}
	f.fat = make([]uint32, n)
	for i := range f.fat {
		f.fat[i] = binary.LittleEndian.Uint32(raw[4*i:])
	}
	if f.rootCluster < 2 || f.rootCluster >= uint32(n) {
		return nil, fmt.Errorf("%w: invalid root cluster %d", ErrNotFAT32, f.rootCluster)
	}
	f.dirtyLo, f.dirtyHi = n, 0
	f.freeCount = f.countFree()
	f.nextFree = 2
	f.fsInfoFree = 0xFFFFFFFF
	if f.fsInfoSector != 0 && f.fsInfoSector != 0xFFFF && f.fsInfoSector < f.reserved {
		var fi [512]byte
		if _, err := dev.ReadAt(fi[:], offset+int64(f.fsInfoSector)*int64(f.bps)); err == nil &&
			binary.LittleEndian.Uint32(fi[0:]) == 0x41615252 && binary.LittleEndian.Uint32(fi[484:]) == 0x61417272 {
			f.fsInfoValid = true
			f.fsInfoFree = binary.LittleEndian.Uint32(fi[488:])
			if nf := binary.LittleEndian.Uint32(fi[492:]); nf >= 2 && nf < uint32(n) {
				f.nextFree = nf
			}
		}
	}
	return f, nil
}

// ClusterSize is the allocation unit in bytes.
func (f *FS) ClusterSize() int64 { return f.clusterSize }

// Label is the volume label from the boot sector ("bootfs" on Raspberry Pi OS).
func (f *FS) Label() string { return f.label }

// VolumeID is the volume serial number.
func (f *FS) VolumeID() uint32 { return f.volumeID }

// TotalClusters is the number of data clusters.
func (f *FS) TotalClusters() uint32 { return f.clusters }

// FreeClusters is the number of free clusters in the FAT.
func (f *FS) FreeClusters() uint32 { return f.freeCount }

func (f *FS) countFree() uint32 {
	var n uint32
	for _, e := range f.fat[2:] {
		if e&fatMask == 0 {
			n++
		}
	}
	return n
}

// FreeBytes is the free space.
func (f *FS) FreeBytes() int64 { return int64(f.FreeClusters()) * f.clusterSize }

func (f *FS) entry(c uint32) uint32 { return f.fat[c] & fatMask }

func (f *FS) setEntry(c, v uint32) {
	wasFree, isFree := f.fat[c]&fatMask == 0, v&fatMask == 0
	if wasFree && !isFree {
		f.freeCount--
	} else if !wasFree && isFree {
		f.freeCount++
	}
	f.fat[c] = f.fat[c]&^fatMask | v&fatMask
	if int(c) < f.dirtyLo {
		f.dirtyLo = int(c)
	}
	if int(c)+1 > f.dirtyHi {
		f.dirtyHi = int(c) + 1
	}
	f.modified = true
}

func (f *FS) validCluster(c uint32) bool { return c >= 2 && c < f.clusters+2 }

func (f *FS) clusterOffset(c uint32) int64 {
	return f.base + f.dataStart + int64(c-2)*f.clusterSize
}

// chain follows a cluster chain, failing on loops, bad and free links.
func (f *FS) chain(first uint32) ([]uint32, error) {
	if first == 0 {
		return nil, nil
	}
	var out []uint32
	c := first
	for {
		if !f.validCluster(c) {
			return out, fmt.Errorf("fatfs: cluster chain from %d reaches invalid cluster %d", first, c)
		}
		out = append(out, c)
		if len(out) > int(f.clusters) {
			return out, fmt.Errorf("fatfs: cluster chain from %d loops", first)
		}
		n := f.entry(c)
		if n >= fatEOCMin {
			return out, nil
		}
		if n == 0 || n == fatBad || n == 1 {
			return out, fmt.Errorf("fatfs: cluster chain from %d is broken at %d (entry %#x)", first, c, n)
		}
		c = n
	}
}

func (f *FS) readChain(first uint32, size int64) ([]byte, error) {
	cl, err := f.chain(first)
	if err != nil {
		return nil, err
	}
	if size < 0 {
		size = int64(len(cl)) * f.clusterSize
	}
	if int64(len(cl))*f.clusterSize < size {
		return nil, fmt.Errorf("fatfs: chain from %d (%d clusters) shorter than the file size %d", first, len(cl), size)
	}
	buf := make([]byte, size)
	off := int64(0)
	for i := 0; i < len(cl) && off < size; {
		// Coalesce contiguous clusters into one read.
		j := i + 1
		for j < len(cl) && cl[j] == cl[j-1]+1 {
			j++
		}
		n := int64(j-i) * f.clusterSize
		if off+n > size {
			n = size - off
		}
		if _, err := f.dev.ReadAt(buf[off:off+n], f.clusterOffset(cl[i])); err != nil {
			return nil, err
		}
		off += n
		i = j
	}
	return buf, nil
}

// allocate reserves n clusters linked as one chain (preferring a contiguous
// run) and returns them. The data is not written.
func (f *FS) allocate(n int) ([]uint32, error) {
	if n == 0 {
		return nil, nil
	}
	if f.readOnly {
		return nil, ErrReadOnly
	}
	total := f.clusters + 2
	// Look for a contiguous run first, starting at the hint.
	var got []uint32
	find := func(from, to uint32) bool {
		run := uint32(0)
		for c := from; c < to; c++ {
			if f.entry(c) == 0 {
				run++
				if int(run) == n {
					start := c + 1 - run
					for k := start; k <= c; k++ {
						got = append(got, k)
					}
					return true
				}
			} else {
				run = 0
			}
		}
		return false
	}
	if !find(f.nextFree, total) && !find(2, total) {
		for c := uint32(2); c < total && len(got) < n; c++ {
			if f.entry(c) == 0 {
				got = append(got, c)
			}
		}
		if len(got) < n {
			return nil, ErrNoSpace
		}
	}
	for i, c := range got {
		if i+1 < len(got) {
			f.setEntry(c, got[i+1])
		} else {
			f.setEntry(c, fatEOC)
		}
	}
	f.nextFree = got[len(got)-1] + 1
	if f.nextFree >= total {
		f.nextFree = 2
	}
	return got, nil
}

func (f *FS) freeChain(first uint32) error {
	cl, err := f.chain(first)
	for _, c := range cl {
		f.setEntry(c, 0)
	}
	return err
}

// writeClusters writes data over the clusters (zero-filling the tail of the
// last cluster).
func (f *FS) writeClusters(cl []uint32, data []byte) error {
	for i := 0; i < len(cl); {
		j := i + 1
		for j < len(cl) && cl[j] == cl[j-1]+1 {
			j++
		}
		lo := int64(i) * f.clusterSize
		hi := int64(j) * f.clusterSize
		var chunk []byte
		switch {
		case hi <= int64(len(data)):
			chunk = data[lo:hi]
		default:
			chunk = make([]byte, hi-lo)
			if lo < int64(len(data)) {
				copy(chunk, data[lo:])
			}
		}
		if _, err := f.dev.WriteAt(chunk, f.clusterOffset(cl[i])); err != nil {
			return err
		}
		i = j
	}
	return nil
}

// Sync writes the changed FAT sectors to every FAT copy and updates FSInfo.
func (f *FS) Sync() error {
	if f.readOnly || !f.modified {
		return nil
	}
	if f.dirtyHi > f.dirtyLo {
		bps := int(f.bps)
		lo := (f.dirtyLo * 4 / bps) * bps / 4
		hi := ((f.dirtyHi*4 + bps - 1) / bps) * bps / 4
		if hi > len(f.fat) {
			hi = len(f.fat)
		}
		raw := make([]byte, (hi-lo)*4)
		for i := lo; i < hi; i++ {
			binary.LittleEndian.PutUint32(raw[4*(i-lo):], f.fat[i])
		}
		for k := uint32(0); k < f.nfats; k++ {
			off := f.base + int64(f.reserved+k*f.fatSectors)*int64(f.bps) + int64(lo)*4
			if _, err := f.dev.WriteAt(raw, off); err != nil {
				return err
			}
		}
		f.dirtyLo, f.dirtyHi = len(f.fat), 0
	}
	if f.fsInfoValid {
		var b [8]byte
		f.fsInfoFree = f.FreeClusters()
		binary.LittleEndian.PutUint32(b[0:], f.fsInfoFree)
		binary.LittleEndian.PutUint32(b[4:], f.nextFree)
		if _, err := f.dev.WriteAt(b[:], f.base+int64(f.fsInfoSector)*int64(f.bps)+488); err != nil {
			return err
		}
	}
	f.modified = false
	return nil
}

// Close syncs the file system.
func (f *FS) Close() error { return f.Sync() }

// ---------------------------------------------------------------- dirs ----

// DirEntry describes one file or directory.
type DirEntry struct {
	Name      string // long name (or the short name with NT case flags)
	ShortName string // 8.3 name as stored
	IsDir     bool
	Size      int64
	Cluster   uint32
	ModTime   time.Time
	Attr      byte

	slot  int // index of the short entry in the directory
	first int // index of the first (long-name) entry
}

// dir is a loaded directory: its cluster chain and raw entries.
type dir struct {
	cluster uint32
	chain   []uint32
	raw     []byte
}

func (f *FS) loadDir(cluster uint32) (*dir, error) {
	cl, err := f.chain(cluster)
	if err != nil {
		return nil, err
	}
	raw, err := f.readChain(cluster, -1)
	if err != nil {
		return nil, err
	}
	return &dir{cluster: cluster, chain: cl, raw: raw}, nil
}

func (d *dir) n() int { return len(d.raw) / 32 }

func (d *dir) ent(i int) []byte { return d.raw[32*i : 32*i+32] }

// entries parses the directory. Long names are used only when their
// checksum matches the short entry that follows.
func (d *dir) entries() []DirEntry {
	var out []DirEntry
	var lfn []uint16
	var lfnSum byte
	lfnNext := -1
	lfnStart := -1
	for i := 0; i < d.n(); i++ {
		e := d.ent(i)
		if e[0] == 0x00 {
			break
		}
		if e[0] == 0xE5 {
			lfn, lfnNext = nil, -1
			continue
		}
		if e[11]&0x3F == attrLFN {
			ord := int(e[0] & 0x3F)
			if e[0]&0x40 != 0 {
				lfn = make([]uint16, ord*lfnChars)
				lfnSum = e[13]
				lfnNext = ord
				lfnStart = i
			}
			if lfnNext != ord || ord == 0 || e[13] != lfnSum || lfn == nil {
				lfn, lfnNext = nil, -1
				continue
			}
			copy(lfn[(ord-1)*lfnChars:], lfnChunk(e))
			lfnNext = ord - 1
			continue
		}
		if e[11]&attrVolumeID != 0 {
			lfn, lfnNext = nil, -1
			continue
		}
		var sn shortName
		copy(sn[:], e[:11])
		de := DirEntry{
			ShortName: sn.String(),
			Attr:      e[11],
			IsDir:     e[11]&attrDir != 0,
			Size:      int64(binary.LittleEndian.Uint32(e[28:])),
			Cluster:   uint32(binary.LittleEndian.Uint16(e[20:]))<<16 | uint32(binary.LittleEndian.Uint16(e[26:])),
			ModTime:   dosTime(binary.LittleEndian.Uint16(e[24:]), binary.LittleEndian.Uint16(e[22:])),
			slot:      i,
			first:     i,
		}
		de.Name = sn.display(e[12])
		if lfn != nil && lfnNext == 0 && sn.checksum() == lfnSum {
			end := len(lfn)
			for k, c := range lfn {
				if c == 0 {
					end = k
					break
				}
			}
			de.Name = string(utf16.Decode(lfn[:end]))
			de.first = lfnStart
		}
		lfn, lfnNext = nil, -1
		if de.Name == "." || de.Name == ".." {
			continue
		}
		if de.IsDir {
			de.Size = 0
		}
		out = append(out, de)
	}
	return out
}

func (d *dir) shortExists(sn shortName) bool {
	for i := 0; i < d.n(); i++ {
		e := d.ent(i)
		if e[0] == 0 {
			return false
		}
		if e[0] == 0xE5 || e[11]&0x3F == attrLFN {
			continue
		}
		if string(e[:11]) == string(sn[:]) {
			return true
		}
	}
	return false
}

func (f *FS) writeDir(d *dir) error { return f.writeClusters(d.chain, d.raw) }

// findSlots returns the index of n consecutive free entries, extending the
// directory by a zeroed cluster when needed.
func (f *FS) findSlots(d *dir, n int) (int, error) {
	run := 0
	for i := 0; i < d.n(); i++ {
		b := d.ent(i)[0]
		if b == 0x00 || b == 0xE5 {
			run++
			if run == n {
				return i + 1 - n, nil
			}
			continue
		}
		run = 0
	}
	// Extend: the free run at the end continues into the new cluster(s).
	oldN := d.n()
	if oldN-run+n > 65536 {
		return 0, errors.New("fatfs: directory is full (65536 entries)")
	}
	need := n - run
	extra := int((int64(need)*32 + f.clusterSize - 1) / f.clusterSize)
	cl, err := f.allocate(extra)
	if err != nil {
		return 0, err
	}
	if err := f.writeClusters(cl, nil); err != nil { // zeroed: 0x00 keeps marking the end
		return 0, err
	}
	f.setEntry(d.chain[len(d.chain)-1], cl[0])
	d.chain = append(d.chain, cl...)
	d.raw = append(d.raw, make([]byte, int64(extra)*f.clusterSize)...)
	return oldN - run, nil
}

// --------------------------------------------------------------- paths ----

func splitPath(p string) ([]string, error) {
	p = strings.Trim(strings.ReplaceAll(p, "\\", "/"), "/")
	if p == "" {
		return nil, nil
	}
	parts := strings.Split(p, "/")
	for _, s := range parts {
		if err := ValidName(s); err != nil {
			return nil, err
		}
	}
	return parts, nil
}

func lookup(entries []DirEntry, name string) (DirEntry, bool) {
	for _, e := range entries {
		if strings.EqualFold(e.Name, name) || strings.EqualFold(e.ShortName, name) {
			return e, true
		}
	}
	return DirEntry{}, false
}

// walkTo returns the directory cluster for the parent path parts.
func (f *FS) walkTo(parts []string) (uint32, error) {
	c := f.rootCluster
	for i, p := range parts {
		d, err := f.loadDir(c)
		if err != nil {
			return 0, err
		}
		e, ok := lookup(d.entries(), p)
		if !ok {
			return 0, &fs.PathError{Op: "open", Path: strings.Join(parts[:i+1], "/"), Err: fs.ErrNotExist}
		}
		if !e.IsDir {
			return 0, &fs.PathError{Op: "open", Path: strings.Join(parts[:i+1], "/"), Err: errors.New("not a directory")}
		}
		c = e.Cluster
		if c == 0 {
			c = f.rootCluster
		}
	}
	return c, nil
}

// Stat returns the entry for a path ("" or "/" is the root directory).
func (f *FS) Stat(p string) (DirEntry, error) {
	parts, err := splitPath(p)
	if err != nil {
		return DirEntry{}, err
	}
	if len(parts) == 0 {
		return DirEntry{Name: "/", IsDir: true, Cluster: f.rootCluster}, nil
	}
	pc, err := f.walkTo(parts[:len(parts)-1])
	if err != nil {
		return DirEntry{}, err
	}
	d, err := f.loadDir(pc)
	if err != nil {
		return DirEntry{}, err
	}
	e, ok := lookup(d.entries(), parts[len(parts)-1])
	if !ok {
		return DirEntry{}, &fs.PathError{Op: "stat", Path: p, Err: fs.ErrNotExist}
	}
	return e, nil
}

// ReadDir lists a directory sorted by name.
func (f *FS) ReadDir(p string) ([]DirEntry, error) {
	parts, err := splitPath(p)
	if err != nil {
		return nil, err
	}
	c, err := f.walkTo(parts)
	if err != nil {
		return nil, err
	}
	d, err := f.loadDir(c)
	if err != nil {
		return nil, err
	}
	es := d.entries()
	sort.Slice(es, func(i, j int) bool { return strings.ToLower(es[i].Name) < strings.ToLower(es[j].Name) })
	return es, nil
}

// ReadFile returns the contents of a file.
func (f *FS) ReadFile(p string) ([]byte, error) {
	e, err := f.Stat(p)
	if err != nil {
		return nil, err
	}
	if e.IsDir {
		return nil, &fs.PathError{Op: "read", Path: p, Err: errors.New("is a directory")}
	}
	if e.Size == 0 {
		return []byte{}, nil
	}
	return f.readChain(e.Cluster, e.Size)
}

// Walk calls fn for every file and directory below root (depth first, sorted).
func (f *FS) Walk(root string, fn func(p string, e DirEntry) error) error {
	es, err := f.ReadDir(root)
	if err != nil {
		return err
	}
	for _, e := range es {
		p := path.Join("/", root, e.Name)
		if err := fn(p, e); err != nil {
			return err
		}
		if e.IsDir {
			if err := f.Walk(p, fn); err != nil {
				return err
			}
		}
	}
	return nil
}

// ---------------------------------------------------------------- writes ----

func (f *FS) stamp(e []byte, created bool) {
	t := f.now().UTC()
	d, tm, tenth := toDOS(t)
	if created {
		e[13] = tenth
		binary.LittleEndian.PutUint16(e[14:], tm)
		binary.LittleEndian.PutUint16(e[16:], d)
	}
	binary.LittleEndian.PutUint16(e[18:], d)
	binary.LittleEndian.PutUint16(e[22:], tm)
	binary.LittleEndian.PutUint16(e[24:], d)
}

func setCluster(e []byte, c uint32) {
	binary.LittleEndian.PutUint16(e[20:], uint16(c>>16))
	binary.LittleEndian.PutUint16(e[26:], uint16(c))
}

// addEntry creates the directory entries for name in d and returns the slot
// of the short entry.
func (f *FS) addEntry(d *dir, name string, attr byte, cluster uint32, size uint32) (int, error) {
	// A short entry alone only for all-uppercase 8.3 names; anything else
	// gets long-name entries (preserves case on every reader).
	sn, ntres, exact := exactShort(name)
	if exact && (ntres != 0 || d.shortExists(sn)) {
		exact = false
	}
	var lfns [][32]byte
	if !exact {
		var err error
		sn, err = generateShort(name, d.shortExists)
		if err != nil {
			return 0, err
		}
		ntres = 0
		lfns = lfnEntries(name, sn.checksum())
	}
	start, err := f.findSlots(d, len(lfns)+1)
	if err != nil {
		return 0, err
	}
	for i, l := range lfns {
		copy(d.ent(start+i), l[:])
	}
	slot := start + len(lfns)
	e := d.ent(slot)
	for i := range e {
		e[i] = 0
	}
	copy(e[:11], sn[:])
	e[11] = attr
	e[12] = ntres
	f.stamp(e, true)
	setCluster(e, cluster)
	binary.LittleEndian.PutUint32(e[28:], size)
	return slot, nil
}

func (f *FS) parentDir(p string) (*dir, string, error) {
	if f.readOnly {
		return nil, "", ErrReadOnly
	}
	parts, err := splitPath(p)
	if err != nil {
		return nil, "", err
	}
	if len(parts) == 0 {
		return nil, "", errors.New("fatfs: cannot write the root directory")
	}
	pc, err := f.walkTo(parts[:len(parts)-1])
	if err != nil {
		return nil, "", err
	}
	d, err := f.loadDir(pc)
	if err != nil {
		return nil, "", err
	}
	return d, parts[len(parts)-1], nil
}

// WriteFile creates or replaces a file. The parent directory must exist.
func (f *FS) WriteFile(p string, data []byte) error {
	if int64(len(data)) > 0xFFFFFFFF {
		return errors.New("fatfs: file larger than 4 GiB")
	}
	d, name, err := f.parentDir(p)
	if err != nil {
		return err
	}
	n := int((int64(len(data)) + f.clusterSize - 1) / f.clusterSize)
	old, exists := lookup(d.entries(), name)
	if exists && old.IsDir {
		return &fs.PathError{Op: "write", Path: p, Err: errors.New("is a directory")}
	}
	if exists && old.Attr&attrReadOnly != 0 {
		return &fs.PathError{Op: "write", Path: p, Err: fs.ErrPermission}
	}
	// Space check before touching anything (the old chain is reusable).
	avail := int(f.FreeClusters())
	if exists {
		oc, _ := f.chain(old.Cluster)
		avail += len(oc)
	}
	if n > avail {
		return fmt.Errorf("%w: %s needs %d bytes", ErrNoSpace, p, int64(n)*f.clusterSize)
	}
	if exists {
		if err := f.freeChain(old.Cluster); err != nil {
			return err
		}
	}
	cl, err := f.allocate(n)
	if err != nil {
		return err
	}
	if err := f.writeClusters(cl, data); err != nil {
		return err
	}
	var first uint32
	if len(cl) > 0 {
		first = cl[0]
	}
	if exists {
		e := d.ent(old.slot)
		setCluster(e, first)
		binary.LittleEndian.PutUint32(e[28:], uint32(len(data)))
		e[11] |= attrArchive
		f.stamp(e, false)
	} else {
		if _, err := f.addEntry(d, name, attrArchive, first, uint32(len(data))); err != nil {
			f.freeChain(first)
			return err
		}
	}
	if err := f.writeDir(d); err != nil {
		return err
	}
	return f.Sync()
}

// Mkdir creates a directory. The parent must exist.
func (f *FS) Mkdir(p string) error {
	d, name, err := f.parentDir(p)
	if err != nil {
		return err
	}
	if _, ok := lookup(d.entries(), name); ok {
		return &fs.PathError{Op: "mkdir", Path: p, Err: fs.ErrExist}
	}
	cl, err := f.allocate(1)
	if err != nil {
		return err
	}
	raw := make([]byte, f.clusterSize)
	dot := raw[0:32]
	copy(dot, ".          ")
	dot[11] = attrDir
	f.stamp(dot, true)
	setCluster(dot, cl[0])
	dotdot := raw[32:64]
	copy(dotdot, "..         ")
	dotdot[11] = attrDir
	f.stamp(dotdot, true)
	if d.cluster != f.rootCluster {
		setCluster(dotdot, d.cluster)
	}
	if err := f.writeClusters(cl, raw); err != nil {
		return err
	}
	if _, err := f.addEntry(d, name, attrDir, cl[0], 0); err != nil {
		f.freeChain(cl[0])
		return err
	}
	if err := f.writeDir(d); err != nil {
		return err
	}
	return f.Sync()
}

// MkdirAll creates a directory and its missing parents.
func (f *FS) MkdirAll(p string) error {
	parts, err := splitPath(p)
	if err != nil {
		return err
	}
	for i := range parts {
		sub := strings.Join(parts[:i+1], "/")
		e, err := f.Stat(sub)
		if err == nil {
			if !e.IsDir {
				return &fs.PathError{Op: "mkdir", Path: sub, Err: errors.New("not a directory")}
			}
			continue
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		if err := f.Mkdir(sub); err != nil {
			return err
		}
	}
	return nil
}

// Remove deletes a file or an empty directory.
func (f *FS) Remove(p string) error {
	d, name, err := f.parentDir(p)
	if err != nil {
		return err
	}
	e, ok := lookup(d.entries(), name)
	if !ok {
		return &fs.PathError{Op: "remove", Path: p, Err: fs.ErrNotExist}
	}
	if e.IsDir {
		sub, err := f.loadDir(e.Cluster)
		if err != nil {
			return err
		}
		if len(sub.entries()) > 0 {
			return &fs.PathError{Op: "remove", Path: p, Err: errors.New("directory not empty")}
		}
	}
	for i := e.first; i <= e.slot; i++ {
		d.ent(i)[0] = 0xE5
	}
	if err := f.freeChain(e.Cluster); err != nil {
		return err
	}
	if err := f.writeDir(d); err != nil {
		return err
	}
	return f.Sync()
}

// RemoveAll deletes a file or a directory tree; a missing path is not an error.
func (f *FS) RemoveAll(p string) error {
	e, err := f.Stat(p)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if e.IsDir {
		es, err := f.ReadDir(p)
		if err != nil {
			return err
		}
		for _, c := range es {
			if err := f.RemoveAll(path.Join(p, c.Name)); err != nil {
				return err
			}
		}
	}
	return f.Remove(p)
}

// ----------------------------------------------------------------- time ----

func dosTime(d, t uint16) time.Time {
	if d == 0 {
		return time.Time{}
	}
	return time.Date(1980+int(d>>9), time.Month(d>>5&0xF), int(d&0x1F),
		int(t>>11), int(t>>5&0x3F), int(t&0x1F)*2, 0, time.UTC)
}

func toDOS(t time.Time) (date, tm uint16, tenth byte) {
	if t.Year() < 1980 {
		t = time.Date(1980, 1, 1, 0, 0, 0, 0, time.UTC)
	}
	if t.Year() > 2107 {
		t = time.Date(2107, 12, 31, 23, 59, 58, 0, time.UTC)
	}
	date = uint16(t.Year()-1980)<<9 | uint16(t.Month())<<5 | uint16(t.Day())
	tm = uint16(t.Hour())<<11 | uint16(t.Minute())<<5 | uint16(t.Second()/2)
	tenth = byte((t.Second()%2)*100 + t.Nanosecond()/10_000_000)
	return
}
