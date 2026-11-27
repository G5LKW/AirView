// Package fatfs reads and writes FAT32 file systems inside raw disk images,
// in pure Go, for building Raspberry Pi SD-card images on any OS
// (cmd/airview-piimage).
//
// It covers what an image builder needs and nothing more:
//
//   - ReadMBR parses a DOS (MBR) partition table; the partition table is
//     never written.
//   - Open mounts the FAT32 file system of one partition. Files can be read
//     (ReadFile, ReadDir, Stat, Walk), created or replaced (WriteFile),
//     directories created (Mkdir, MkdirAll) and entries removed (Remove,
//     RemoveAll). Long file names (VFAT) are read and written; new short
//     names get numeric tails ("FIRSTB~1.SH"). Names are matched without
//     regard to case, as FAT does.
//   - Allocation prefers contiguous runs; the first FAT is cached in memory
//     and written to every FAT copy by Sync (each mutating call syncs), with
//     the FSInfo free count kept exact.
//   - Check is a read-only consistency check (chains, cross-links, sizes,
//     "." and "..", FAT copies, lost clusters).
//   - Format creates an empty FAT32 file system (tests, small images).
//
// FAT12/16, exFAT and GPT are not supported; the boot sector and the backup
// boot sector are never modified. Files are limited to 4 GiB - 1 and time
// stamps are stored as UTC (the Linux vfat default).
//
// The implementation follows the published Microsoft FAT32 File System
// Specification; no third-party code was used (docs/aggregator/CLEANROOM.md,
// WP-PIIMG). Subpackage xz decodes .xz images (LZMA2).
package fatfs
