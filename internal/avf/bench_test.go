package avf

import (
	"bytes"
	"io"
	"math/rand/v2"
	"testing"
	"time"
)

// benchFrames builds a realistic FRAMES record: n frames of a 12 MHz-style
// receiver from a 300-aircraft sky, ~75 % long (DF17) and 25 % short, ticks
// ~0.5 ms apart with jitter. Payload bytes after the address are random
// (ME fields and CRC have high entropy), so compression is not flattered.
func benchFrames(n int, rng *rand.Rand) *Frames {
	base := int64(1_763_000_000_000_000_000)
	r := &Frames{Seq: 1, RxNo: 1, EpochID: 3, BaseTicks: 1 << 40, BaseHostNs: base}
	ticks := uint64(1 << 40)
	host := base
	for i := 0; i < n; i++ {
		icao := uint32(0x400000 + rng.IntN(300)*977)
		var d []byte
		if rng.IntN(4) == 0 {
			d = make([]byte, 7)
			d[0] = 0x5D
		} else {
			d = make([]byte, 14)
			d[0] = 0x8D
		}
		d[1], d[2], d[3] = byte(icao>>16), byte(icao>>8), byte(icao)
		for k := 4; k < len(d); k++ {
			d[k] = byte(rng.Uint32())
		}
		ticks += uint64(rng.IntN(12000))
		host += int64(rng.IntN(1000)) * 1000
		r.Frames = append(r.Frames, Frame{Ticks: ticks, HostNs: host, Data: d, Channel: 1, Sig: byte(0x20 + rng.IntN(150))})
	}
	return r
}

// benchRecords returns k distinct FRAMES records of n frames.
func benchRecords(k, n int) []*Frames {
	rng := rand.New(rand.NewPCG(1, 2))
	out := make([]*Frames, k)
	for i := range out {
		out[i] = benchFrames(n, rng)
	}
	return out
}

func BenchmarkFramesEncode(b *testing.B) {
	recs := benchRecords(64, 64)
	buf := make([]byte, 0, 4096)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		buf, _ = AppendFrames(buf[:0], recs[i%len(recs)])
	}
	b.ReportMetric(float64(b.N*64)/b.Elapsed().Seconds(), "frames/s")
}

func BenchmarkFramesDecode(b *testing.B) {
	p, _ := AppendFrames(nil, benchRecords(1, 64)[0])
	var r Frames
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := r.Decode(p); err != nil {
			b.Fatal(err)
		}
	}
	b.ReportMetric(float64(b.N*64)/b.Elapsed().Seconds(), "frames/s")
}

// BenchmarkPipelineSend measures the client send path per core: FRAMES
// encode + record framing + DEFLATE level 1 with a 50 ms-style batch flush
// (one sync flush per 4 records here).
func BenchmarkPipelineSend(b *testing.B) {
	recs := benchRecords(256, 64)
	w := NewStreamWriter(io.Discard, time.Hour)
	buf := make([]byte, 0, 4096)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		r := recs[i%len(recs)]
		r.Seq = uint64(i + 1)
		buf, _ = AppendFrames(buf[:0], r)
		if err := w.WriteRecord(TFrames, buf); err != nil {
			b.Fatal(err)
		}
		if i%4 == 3 {
			w.Flush()
		}
	}
	w.Flush()
	st := w.Stats()
	b.ReportMetric(float64(b.N*64)/b.Elapsed().Seconds(), "frames/s")
	b.ReportMetric(float64(st.WireBytes)/float64(b.N*64), "wireB/frame")
}

// BenchmarkPipelineReceive measures the server receive path per core:
// inflate with guards + record framing + FRAMES decode.
func BenchmarkPipelineReceive(b *testing.B) {
	recs := benchRecords(2000, 64)
	var wire bytes.Buffer
	w := NewStreamWriter(&wire, time.Hour)
	for i, r := range recs {
		r.Seq = uint64(i + 1)
		p, _ := AppendFrames(nil, r)
		w.WriteRecord(TFrames, p)
		if i%4 == 3 {
			w.Flush()
		}
	}
	w.Flush()
	data := wire.Bytes()
	g := DefaultGuard()
	g.MaxRate = 0 // the benchmark exceeds any per-connection rate by design
	var f Frames
	b.ReportAllocs()
	b.SetBytes(int64(len(data)))
	b.ResetTimer()
	frames := 0
	for i := 0; i < b.N; i++ {
		rr := NewRecordReader(NewStreamReader(bytes.NewReader(data), g), FromClient)
		for {
			rec, err := rr.Next()
			if err != nil {
				break
			}
			if err := f.Decode(rec.Payload); err != nil {
				b.Fatal(err)
			}
			frames += len(f.Frames)
		}
	}
	b.ReportMetric(float64(frames)/b.Elapsed().Seconds(), "frames/s")
}
