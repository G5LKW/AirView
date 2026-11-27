package fatfs

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// SectorSize is the logical sector size of an MBR disk image.
const SectorSize = 512

// Partition is one primary MBR partition table entry.
type Partition struct {
	Index    int    // 1..4 (Linux numbering, as in PARTUUID=<disk id>-0N)
	Bootable bool   // status byte 0x80
	Type     byte   // partition type (0x0C FAT32 LBA, 0x0B FAT32 CHS, 0x83 Linux)
	StartLBA uint32 // first sector
	Sectors  uint32 // length in sectors
}

// Offset is the partition's byte offset in the image.
func (p Partition) Offset() int64 { return int64(p.StartLBA) * SectorSize }

// Size is the partition's size in bytes.
func (p Partition) Size() int64 { return int64(p.Sectors) * SectorSize }

// IsFAT reports a FAT partition type (FAT12/16/32, CHS or LBA).
func (p Partition) IsFAT() bool {
	switch p.Type {
	case 0x01, 0x04, 0x06, 0x0B, 0x0C, 0x0E:
		return true
	}
	return false
}

// TypeName describes the partition type.
func (p Partition) TypeName() string {
	switch p.Type {
	case 0x0B:
		return "FAT32 (CHS)"
	case 0x0C:
		return "FAT32 (LBA)"
	case 0x0E:
		return "FAT16 (LBA)"
	case 0x06:
		return "FAT16"
	case 0x83:
		return "Linux"
	case 0x82:
		return "Linux swap"
	case 0x05, 0x0F:
		return "extended"
	case 0xEE:
		return "GPT protective"
	}
	return fmt.Sprintf("type %#02x", p.Type)
}

// MBR is a parsed master boot record.
type MBR struct {
	DiskID     uint32      // disk signature (offset 440); PARTUUID prefix on Linux
	Partitions []Partition // non-empty primary entries, in table order
	Raw        [SectorSize]byte
}

// ErrNoMBR reports a first sector without the 0x55AA signature.
var ErrNoMBR = errors.New("fatfs: no MBR partition table (missing 0x55AA signature)")

// ReadMBR parses the MBR of a disk image. A GPT protective MBR is reported as
// an error (GPT images are not supported).
func ReadMBR(r io.ReaderAt, imageSize int64) (MBR, error) {
	var m MBR
	if _, err := r.ReadAt(m.Raw[:], 0); err != nil {
		return m, fmt.Errorf("fatfs: reading the MBR: %w", err)
	}
	if m.Raw[510] != 0x55 || m.Raw[511] != 0xAA {
		return m, ErrNoMBR
	}
	m.DiskID = binary.LittleEndian.Uint32(m.Raw[440:])
	for i := 0; i < 4; i++ {
		e := m.Raw[446+16*i : 446+16*(i+1)]
		p := Partition{
			Index:    i + 1,
			Bootable: e[0] == 0x80,
			Type:     e[4],
			StartLBA: binary.LittleEndian.Uint32(e[8:]),
			Sectors:  binary.LittleEndian.Uint32(e[12:]),
		}
		if p.Type == 0 || p.Sectors == 0 {
			continue
		}
		if e[0] != 0 && e[0] != 0x80 {
			return m, fmt.Errorf("fatfs: partition %d has an invalid status byte %#02x", p.Index, e[0])
		}
		if p.Type == 0xEE {
			return m, errors.New("fatfs: GPT disk images are not supported")
		}
		if imageSize > 0 && p.Offset()+p.Size() > imageSize {
			return m, fmt.Errorf("fatfs: partition %d (%d+%d sectors) extends beyond the image (%d bytes)", p.Index, p.StartLBA, p.Sectors, imageSize)
		}
		m.Partitions = append(m.Partitions, p)
	}
	for i, a := range m.Partitions {
		for _, b := range m.Partitions[i+1:] {
			if a.StartLBA < b.StartLBA+b.Sectors && b.StartLBA < a.StartLBA+a.Sectors {
				return m, fmt.Errorf("fatfs: partitions %d and %d overlap", a.Index, b.Index)
			}
		}
	}
	return m, nil
}

// PartUUID is the Linux PARTUUID of a partition on this disk ("27b94958-01").
func (m MBR) PartUUID(index int) string { return fmt.Sprintf("%08x-%02x", m.DiskID, index) }

// FirstFAT returns the first FAT partition (the Raspberry Pi boot partition).
func (m MBR) FirstFAT() (Partition, bool) {
	for _, p := range m.Partitions {
		if p.IsFAT() {
			return p, true
		}
	}
	return Partition{}, false
}
