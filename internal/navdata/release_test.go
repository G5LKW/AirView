package navdata

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
)

// TestReimportReturnsSpace imports a large airports file twice: the second
// import replaces the first, and the pages of the replaced table must go
// back to the file system (incremental vacuum in steps) and the WAL must be
// truncated afterwards (DB-EFFICIENCY-DESIGN §3.20).
func TestReimportReturnsSpace(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	nd := New(st, Options{})
	var b strings.Builder
	b.WriteString(strings.SplitN(fixAirports, "\n", 2)[0] + "\n")
	for i := range 40_000 {
		fmt.Fprintf(&b, "%d,\"X%05d\",\"small_airport\",\"Airfield number %d with a reasonably long name\",%.5f,%.5f,%d,\"EU\",\"GB\",\"GB-ENG\",\"Town %d\",\"no\",,,\"X%05d\",,,,\n",
			100000+i, i, i, 50+float64(i%1000)/1000, -1+float64(i/1000)/100, i%900, i, i)
	}
	csv := b.String()
	if _, err := nd.ImportReader(ctx, DatasetAirports, strings.NewReader(csv), nil); err != nil {
		t.Fatal(err)
	}
	var pages1 int64
	_ = st.DB().QueryRow(`PRAGMA page_count`).Scan(&pages1)
	if _, err := nd.ImportReader(ctx, DatasetAirports, strings.NewReader(csv), nil); err != nil {
		t.Fatal(err)
	}
	var free, pages2 int64
	if err := st.DB().QueryRow(`PRAGMA freelist_count`).Scan(&free); err != nil {
		t.Fatal(err)
	}
	_ = st.DB().QueryRow(`PRAGMA page_count`).Scan(&pages2)
	t.Logf("pages after first import %d, after re-import %d, free %d", pages1, pages2, free)
	if free > vacuumStepPages/4 || pages2 > pages1+pages1/10 {
		t.Fatalf("re-import left %d free pages, file %d pages (first import: %d)", free, pages2, pages1)
	}
	if fi, err := os.Stat(st.Stats().Path + "-wal"); err == nil && fi.Size() > 1<<20 {
		t.Fatalf("WAL not truncated after the import: %d bytes", fi.Size())
	}
	var n int
	_ = st.DB().QueryRow(`SELECT COUNT(*) FROM navdata_airport`).Scan(&n)
	if n < 39_000 {
		t.Fatalf("%d airports after the re-import", n)
	}
}
