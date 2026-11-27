// Command geoidgen builds the embedded geoid grid of internal/geoid from a
// published EGM96 source grid (NGA WW15MGH.GRD or GeographicLib
// egm96-15.pgm). It keeps every step-th node (exact source values, no
// smoothing) and writes the AVGEOID1 format.
//
//	go run ./internal/geoid/cmd/geoidgen -in WW15MGH.GRD -out internal/geoid/data/egm96-1deg.avg
//
// It is a development tool; nothing in the application runs it.
package main

import (
	"flag"
	"fmt"
	"math"
	"os"

	"airview/internal/geoid"
)

func main() {
	in := flag.String("in", "", "source grid: NGA ASCII (WW15MGH.GRD), GeographicLib PGM or AVGEOID1")
	out := flag.String("out", "internal/geoid/data/egm96-1deg.avg", "output AVGEOID1 file")
	res := flag.Float64("res", 1, "output resolution in degrees (must be a multiple of the source spacing)")
	verify := flag.Bool("verify", true, "fail when the output misses an NGA reference point by more than 2 m")
	flag.Parse()
	if *in == "" {
		flag.Usage()
		os.Exit(2)
	}
	if err := run(*in, *out, *res, *verify); err != nil {
		fmt.Fprintln(os.Stderr, "geoidgen:", err)
		os.Exit(1)
	}
}

func run(in, out string, res float64, verify bool) error {
	src, err := geoid.Load(in)
	if err != nil {
		return err
	}
	fmt.Printf("source: %d×%d nodes, %.4g°×%.4g°, global=%v\n", src.NLat, src.NLon, src.DLat, src.DLon, src.Global())
	step := int(math.Round(res / src.DLat))
	if step < 1 || math.Abs(float64(step)*src.DLat-res) > 1e-9 || math.Abs(float64(step)*src.DLon-res) > 1e-9 {
		return fmt.Errorf("resolution %v° is not a multiple of the source spacing %v°×%v°", res, src.DLat, src.DLon)
	}
	g, err := src.Resample(step)
	if err != nil {
		return err
	}
	b, err := g.MarshalBinary()
	if err != nil {
		return err
	}
	if err := os.WriteFile(out, b, 0o644); err != nil {
		return err
	}
	fmt.Printf("wrote %s: %d×%d nodes, %d bytes\n", out, g.NLat, g.NLon, len(b))

	worst := 0.0
	fmt.Println("NGA reference points (published N, source grid, output grid):")
	for _, p := range geoid.NGATestPoints {
		s, o := src.Undulation(p.Lat, p.Lon), g.Undulation(p.Lat, p.Lon)
		worst = math.Max(worst, math.Abs(o-p.N))
		fmt.Printf("  %10.5f %11.5f  %8.3f  %8.3f  %8.3f  (Δ %+.3f m)\n", p.Lat, p.Lon, p.N, s, o, o-p.N)
	}
	fmt.Printf("worst output-grid error at the reference points: %.3f m\n", worst)
	if verify && worst > 2 {
		return fmt.Errorf("output grid misses a reference point by %.2f m (> 2 m)", worst)
	}
	return nil
}
