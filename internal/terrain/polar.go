package terrain

import (
	"bufio"
	"compress/gzip"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"runtime"
	"sync"
	"sync/atomic"

	"airview/internal/geo"
)

// Polar sampling limits (memory: Rays × Samples × 4 bytes; the defaults for
// a 450 km radius at 0.5° and ~0.25 km are about 5.8 MB).
const (
	maxPolarRays    = 3600
	maxPolarSamples = 4000
	minSampleKm     = 0.03
)

// PolarSpec defines a polar terrain grid around a site.
type PolarSpec struct {
	Lat            float64 `json:"lat"`
	Lon            float64 `json:"lon"`
	RadiusKm       float64 `json:"radiusKm"`
	BearingStepDeg float64 `json:"bearingStepDeg"`
	SampleKm       float64 `json:"sampleKm"`
	Zoom           int     `json:"zoom"` // informational (data source zoom)
}

// normalize fills defaults and clamps the grid to the memory limits.
func (s PolarSpec) normalize(src Source) PolarSpec {
	if !(s.RadiusKm > 0) {
		s.RadiusKm = 450
	}
	if s.RadiusKm > 1500 {
		s.RadiusKm = 1500
	}
	if !(s.BearingStepDeg > 0) {
		s.BearingStepDeg = 0.5
	}
	if 360/s.BearingStepDeg > maxPolarRays {
		s.BearingStepDeg = 360.0 / maxPolarRays
	}
	// Make the step divide the circle exactly.
	rays := int(math.Round(360 / s.BearingStepDeg))
	if rays < 8 {
		rays = 8
	}
	s.BearingStepDeg = 360 / float64(rays)
	if !(s.SampleKm > 0) {
		s.SampleKm = math.Max(sourceResolution(src, s.Lat)/1000, 0.1)
	}
	if s.SampleKm < minSampleKm {
		s.SampleKm = minSampleKm
	}
	if s.RadiusKm/s.SampleKm > maxPolarSamples {
		s.SampleKm = s.RadiusKm / maxPolarSamples
	}
	return s
}

// Rays returns the number of bearings.
func (s PolarSpec) Rays() int { return int(math.Round(360 / s.BearingStepDeg)) }

// Samples returns the number of samples per ray.
func (s PolarSpec) Samples() int { return int(math.Ceil(s.RadiusKm/s.SampleKm - 1e-9)) }

// PolarTerrain holds the terrain elevation along radial rays from a site:
// ray i has bearing i·BearingStepDeg, sample j lies at (j+1)·SampleKm. It is
// the expensive DEM-reading step; panoramas for any antenna height or k are
// derived from it cheaply. Immutable after creation.
type PolarTerrain struct {
	Spec    PolarSpec
	rays    int
	samples int
	elev    []float32 // ray-major
	NoData  int       // samples without elevation data
}

// Rays returns the number of rays.
func (pt *PolarTerrain) Rays() int { return pt.rays }

// SamplesPerRay returns the number of samples per ray.
func (pt *PolarTerrain) SamplesPerRay() int { return pt.samples }

// At returns the elevation of sample j on ray i.
func (pt *PolarTerrain) At(i, j int) float64 { return float64(pt.elev[i*pt.samples+j]) }

// DistKm returns the distance of sample j.
func (pt *PolarTerrain) DistKm(j int) float64 { return float64(j+1) * pt.Spec.SampleKm }

// NoDataFraction is the fraction of samples without elevation data.
func (pt *PolarTerrain) NoDataFraction() float64 {
	if len(pt.elev) == 0 {
		return 0
	}
	return float64(pt.NoData) / float64(len(pt.elev))
}

// SamplePolar reads the terrain of src on the polar grid spec. Rays are
// processed in parallel (up to 4 workers). progress (may be nil) receives
// the number of finished rays and the total, from one goroutine at a time.
func SamplePolar(ctx context.Context, src Source, spec PolarSpec, progress func(done, total int)) (*PolarTerrain, error) {
	spec = spec.normalize(src)
	pt := &PolarTerrain{Spec: spec, rays: spec.Rays(), samples: spec.Samples()}
	pt.elev = make([]float32, pt.rays*pt.samples)
	o := newOrigin(spec.Lat, spec.Lon)
	sinD := make([]float64, pt.samples)
	cosD := make([]float64, pt.samples)
	for j := range sinD {
		sinD[j], cosD[j] = math.Sincos(pt.DistKm(j) / geo.EarthRadiusKm)
	}
	workers := runtime.NumCPU()
	if workers > 4 {
		workers = 4
	}
	if workers < 1 {
		workers = 1
	}
	var next, done, noData atomic.Int64
	var pmu sync.Mutex
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var nd int64
			defer func() { noData.Add(nd) }()
			for {
				i := int(next.Add(1) - 1)
				if i >= pt.rays || ctx.Err() != nil {
					return
				}
				sb, cb := math.Sincos(float64(i) * spec.BearingStepDeg * degToRad)
				row := pt.elev[i*pt.samples : (i+1)*pt.samples]
				for j := range row {
					la, lo := o.dest(sb, cb, sinD[j], cosD[j])
					e, ok := src.ElevationM(la, lo)
					if !ok {
						nd++
					}
					row[j] = float32(e)
				}
				d := done.Add(1)
				if progress != nil {
					pmu.Lock()
					progress(int(d), pt.rays)
					pmu.Unlock()
				}
			}
		}()
	}
	wg.Wait()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	pt.NoData = int(noData.Load())
	return pt, nil
}

// Binary cache format (gzip): magic, version, spec, sizes, noData, float32s.
var polarMagic = [8]byte{'A', 'V', 'P', 'O', 'L', 'A', 'R', '1'}

// WriteTo serialises the polar terrain (gzip-compressed binary).
func (pt *PolarTerrain) WriteTo(w io.Writer) (int64, error) {
	zw, _ := gzip.NewWriterLevel(w, gzip.BestSpeed)
	bw := bufio.NewWriterSize(zw, 64<<10)
	hdr := []any{polarMagic, pt.Spec.Lat, pt.Spec.Lon, pt.Spec.RadiusKm, pt.Spec.BearingStepDeg, pt.Spec.SampleKm,
		int32(pt.Spec.Zoom), int32(pt.rays), int32(pt.samples), int64(pt.NoData)}
	for _, v := range hdr {
		if err := binary.Write(bw, binary.LittleEndian, v); err != nil {
			return 0, err
		}
	}
	var buf [4]byte
	for _, v := range pt.elev {
		binary.LittleEndian.PutUint32(buf[:], math.Float32bits(v))
		if _, err := bw.Write(buf[:]); err != nil {
			return 0, err
		}
	}
	if err := bw.Flush(); err != nil {
		return 0, err
	}
	return 0, zw.Close()
}

// ReadPolarTerrain reads data written by WriteTo.
func ReadPolarTerrain(r io.Reader) (*PolarTerrain, error) {
	zr, err := gzip.NewReader(r)
	if err != nil {
		return nil, err
	}
	defer zr.Close()
	br := bufio.NewReaderSize(zr, 64<<10)
	var magic [8]byte
	var zoom, rays, samples int32
	var nd int64
	pt := &PolarTerrain{}
	for _, v := range []any{&magic, &pt.Spec.Lat, &pt.Spec.Lon, &pt.Spec.RadiusKm, &pt.Spec.BearingStepDeg, &pt.Spec.SampleKm, &zoom, &rays, &samples, &nd} {
		if err := binary.Read(br, binary.LittleEndian, v); err != nil {
			return nil, fmt.Errorf("terrain: polar cache header: %w", err)
		}
	}
	if magic != polarMagic {
		return nil, errors.New("terrain: not a polar terrain cache file")
	}
	if rays <= 0 || samples <= 0 || rays > maxPolarRays || samples > maxPolarSamples+1 {
		return nil, errors.New("terrain: polar cache has invalid dimensions")
	}
	pt.Spec.Zoom, pt.rays, pt.samples, pt.NoData = int(zoom), int(rays), int(samples), int(nd)
	pt.elev = make([]float32, int(rays)*int(samples))
	var buf [4]byte
	for i := range pt.elev {
		if _, err := io.ReadFull(br, buf[:]); err != nil {
			return nil, fmt.Errorf("terrain: polar cache data: %w", err)
		}
		pt.elev[i] = math.Float32frombits(binary.LittleEndian.Uint32(buf[:]))
	}
	return pt, nil
}
