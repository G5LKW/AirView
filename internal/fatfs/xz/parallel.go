package xz

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"io"
	"runtime"
	"sync"
)

// DecodeFile decompresses the .xz file r (of the given size) into w.
//
// A single-stream file with several blocks (as produced by multi-threaded xz)
// is decoded with up to workers blocks in parallel, each written at its offset;
// anything else falls back to the sequential Decode. workers <= 0 means
// min(GOMAXPROCS, 8). Each worker holds one dictionary (up to 64 MiB for
// "xz -9"). It returns the decompressed size.
func DecodeFile(w io.WriterAt, r io.ReaderAt, size int64, workers int) (int64, error) {
	if workers <= 0 {
		workers = runtime.GOMAXPROCS(0)
		if workers > 8 {
			workers = 8
		}
	}
	plan, err := planBlocks(r, size)
	if err != nil || len(plan.blocks) < 2 || workers == 1 {
		return Decode(io.NewOffsetWriter(w, 0), io.NewSectionReader(r, 0, size))
	}
	type job struct {
		i int
	}
	jobs := make(chan job)
	errs := make([]error, len(plan.blocks))
	var wg sync.WaitGroup
	var stop sync.Once
	quit := make(chan struct{})
	for k := 0; k < workers; k++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range jobs {
				b := plan.blocks[j.i]
				sr := io.NewSectionReader(r, b.inOff, b.inLen)
				br := bufio.NewReaderSize(sr, 1<<20)
				first, err := br.ReadByte()
				if err == nil && first == 0 {
					err = ErrFormat
				}
				if err == nil {
					var u, c int64
					u, c, err = decodeBlock(br, first, plan.check, io.NewOffsetWriter(w, b.outOff))
					if err == nil && (u != b.unpadded || c != b.outLen) {
						err = errors.New("xz: index does not match the blocks")
					}
				}
				if err != nil {
					errs[j.i] = err
					stop.Do(func() { close(quit) })
				}
			}
		}()
	}
feed:
	for i := range plan.blocks {
		select {
		case jobs <- job{i}:
		case <-quit:
			break feed
		}
	}
	close(jobs)
	wg.Wait()
	for _, e := range errs {
		if e != nil {
			return 0, e
		}
	}
	return plan.total, nil
}

type blockPlan struct {
	inOff, inLen   int64
	outOff, outLen int64
	unpadded       int64
}

type filePlan struct {
	check  byte
	blocks []blockPlan
	total  int64
}

// planBlocks reads the stream header, footer and index of a single-stream
// file and returns where every block starts.
func planBlocks(r io.ReaderAt, size int64) (filePlan, error) {
	var p filePlan
	if size < 32 {
		return p, ErrFormat
	}
	var hdr [12]byte
	if _, err := r.ReadAt(hdr[:], 0); err != nil {
		return p, err
	}
	if !bytes.Equal(hdr[:6], headerMagic) || crc32.ChecksumIEEE(hdr[6:8]) != binary.LittleEndian.Uint32(hdr[8:]) {
		return p, ErrFormat
	}
	check, err := parseStreamFlags(hdr[6:8])
	if err != nil {
		return p, err
	}
	p.check = check
	end := size
	var pad [4]byte
	for end >= 4 {
		if _, err := r.ReadAt(pad[:], end-4); err != nil {
			return p, err
		}
		if pad != [4]byte{} {
			break
		}
		end -= 4
	}
	var ft [12]byte
	if _, err := r.ReadAt(ft[:], end-12); err != nil {
		return p, err
	}
	if !bytes.Equal(ft[10:], footerMagic) || crc32.ChecksumIEEE(ft[4:10]) != binary.LittleEndian.Uint32(ft[:4]) || !bytes.Equal(ft[8:10], hdr[6:8]) {
		return p, ErrFormat
	}
	isize := (int64(binary.LittleEndian.Uint32(ft[4:8])) + 1) * 4
	istart := end - 12 - isize
	if istart < 12 {
		return p, ErrFormat
	}
	ibr := bufio.NewReader(io.NewSectionReader(r, istart, isize))
	if b, err := ibr.ReadByte(); err != nil || b != 0 {
		return p, ErrFormat
	}
	recs, n, err := readIndex(ibr)
	if err != nil || n != isize {
		return p, ErrFormat
	}
	cs, _ := checkSize(check)
	off, out := int64(12), int64(0)
	for _, rc := range recs {
		padded := (rc.unpadded + 3) &^ 3
		if rc.unpadded < int64(cs)+5 {
			return p, ErrFormat
		}
		p.blocks = append(p.blocks, blockPlan{inOff: off, inLen: padded, outOff: out, outLen: rc.uncompressed, unpadded: rc.unpadded})
		off += padded
		out += rc.uncompressed
	}
	if off != istart {
		return p, errors.New("xz: not a single-stream file") // concatenated streams: sequential path
	}
	p.total = out
	return p, nil
}
