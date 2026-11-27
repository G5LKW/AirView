package fatfs

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// FormatOptions for Format.
type FormatOptions struct {
	Label             string // up to 11 characters, default "NO NAME"
	VolumeID          uint32
	SectorsPerCluster uint32 // power of two; default 8 (4 KiB clusters)
	ReservedSectors   uint32 // default 32
}

// Format writes an empty FAT32 file system (512-byte sectors, two FATs, the
// FSInfo sector at 1 and a backup boot sector at 6) over [offset,
// offset+size) of dev. It exists for tests and for building small images; it
// does not zero the data area.
func Format(dev Device, offset, size int64, o FormatOptions) error {
	if o.SectorsPerCluster == 0 {
		o.SectorsPerCluster = 8
	}
	if o.ReservedSectors == 0 {
		o.ReservedSectors = 32
	}
	if o.Label == "" {
		o.Label = "NO NAME"
	}
	if len(o.Label) > 11 {
		return errors.New("fatfs: label longer than 11 characters")
	}
	spc := o.SectorsPerCluster
	if spc&(spc-1) != 0 || spc > 128 {
		return errors.New("fatfs: sectors per cluster must be a power of two <= 128")
	}
	const bps = 512
	total := uint32(size / bps)
	if int64(total)*bps != size || size/bps > 0xFFFFFFFF {
		return fmt.Errorf("fatfs: size %d is not a whole number of sectors (or too large)", size)
	}
	// FAT size (FAT32 formula from the Microsoft specification).
	tmp1 := total - o.ReservedSectors
	tmp2 := (256*spc + 2) / 2
	fatSz := (tmp1 + tmp2 - 1) / tmp2
	data := total - o.ReservedSectors - 2*fatSz
	clusters := data / spc
	if clusters < 2 || int64(total) <= int64(o.ReservedSectors+2*fatSz) {
		return errors.New("fatfs: too small for FAT32")
	}
	var bs [bps]byte
	copy(bs[0:], []byte{0xEB, 0x58, 0x90})
	copy(bs[3:], "AIRVIEW ")
	binary.LittleEndian.PutUint16(bs[11:], bps)
	bs[13] = byte(spc)
	binary.LittleEndian.PutUint16(bs[14:], uint16(o.ReservedSectors))
	bs[16] = 2
	bs[21] = 0xF8
	binary.LittleEndian.PutUint16(bs[24:], 63)
	binary.LittleEndian.PutUint16(bs[26:], 255)
	binary.LittleEndian.PutUint32(bs[32:], total)
	binary.LittleEndian.PutUint32(bs[36:], fatSz)
	binary.LittleEndian.PutUint32(bs[44:], 2)
	binary.LittleEndian.PutUint16(bs[48:], 1)
	binary.LittleEndian.PutUint16(bs[50:], 6)
	bs[64] = 0x80
	bs[66] = 0x29
	binary.LittleEndian.PutUint32(bs[67:], o.VolumeID)
	label := []byte(o.Label + "           ")[:11]
	copy(bs[71:], label)
	copy(bs[82:], "FAT32   ")
	bs[510], bs[511] = 0x55, 0xAA
	var fi [bps]byte
	binary.LittleEndian.PutUint32(fi[0:], 0x41615252)
	binary.LittleEndian.PutUint32(fi[484:], 0x61417272)
	binary.LittleEndian.PutUint32(fi[488:], clusters-1)
	binary.LittleEndian.PutUint32(fi[492:], 3)
	binary.LittleEndian.PutUint32(fi[508:], 0xAA550000)
	for _, s := range []int64{0, 6} {
		if _, err := dev.WriteAt(bs[:], offset+s*bps); err != nil {
			return err
		}
		if _, err := dev.WriteAt(fi[:], offset+(s+1)*bps); err != nil {
			return err
		}
	}
	fat := make([]byte, int64(fatSz)*bps)
	binary.LittleEndian.PutUint32(fat[0:], 0x0FFFFFF8)
	binary.LittleEndian.PutUint32(fat[4:], 0x0FFFFFFF)
	binary.LittleEndian.PutUint32(fat[8:], fatEOC) // root directory
	for k := int64(0); k < 2; k++ {
		if _, err := dev.WriteAt(fat, offset+(int64(o.ReservedSectors)+k*int64(fatSz))*bps); err != nil {
			return err
		}
	}
	// Empty root directory cluster, with the volume label entry.
	root := make([]byte, int64(spc)*bps)
	copy(root[0:11], label)
	root[11] = attrVolumeID
	_, err := dev.WriteAt(root, offset+(int64(o.ReservedSectors)+2*int64(fatSz))*bps)
	return err
}
