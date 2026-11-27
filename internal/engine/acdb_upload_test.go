package engine

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"airview/internal/refdata"
)

func TestUploadedFormatInfo(t *testing.T) {
	for _, tc := range []struct {
		f       refdata.Format
		src     string
		licence bool
	}{
		{refdata.FormatTar1090, refdata.SourceTar1090, true},
		{refdata.FormatOpenSky, refdata.SourceOpenSky, true},
		{refdata.FormatAuto, "", false},
		{refdata.Format("other"), "", false},
	} {
		info := ACDBImportInfo{Source: "my-file.csv"}
		uploadedFormatInfo(&info, tc.f)
		if info.Source != "my-file.csv" {
			t.Errorf("%q: source changed to %q", tc.f, info.Source)
		}
		if !tc.licence {
			if info.Licence != "" || info.Attribution != "" || info.Format != "" {
				t.Errorf("%q: info = %+v, want no licence", tc.f, info)
			}
			continue
		}
		want, _ := refdata.SourceByID(tc.src)
		if info.Format != string(tc.f) || info.Licence != want.Licence || info.Attribution != want.Attribution || info.Attribution == "" {
			t.Errorf("%q: info = %+v", tc.f, info)
		}
	}
}

// An uploaded tar1090-db file keeps the ODC-By attribution of the download.
func TestAircraftDBUploadKeepsAttribution(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("..", "refdata", "testdata", "tar1090.csv"))
	if err != nil {
		t.Skipf("refdata test data not available: %v", err)
	}
	e := New(t.TempDir(), Options{Offline: true, Logger: quietLogger()})
	if err := e.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer e.Stop()
	ctx := context.Background()
	if _, err := e.ImportAircraftDBFile(ctx, bytes.NewReader(src), int64(len(src)), "aircraft.csv"); err != nil {
		t.Fatal(err)
	}
	info, _, ok, err := e.AircraftDBInfo(ctx)
	if err != nil || !ok {
		t.Fatalf("info ok=%v err=%v", ok, err)
	}
	if info.Source != "aircraft.csv" || info.Format != "tar1090" || !strings.Contains(info.Licence, "ODC-By") || !strings.Contains(info.Attribution, "tar1090-db") {
		t.Fatalf("info = %+v", info)
	}
}
