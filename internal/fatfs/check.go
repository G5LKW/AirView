package fatfs

import (
	"fmt"
	"path"
)

// Problem is one inconsistency found by Check.
type Problem struct {
	Severe bool   // corrupts data (cross-linked or broken chains); otherwise lost clusters / stale FSInfo
	Text   string // description
}

func (p Problem) String() string {
	if p.Severe {
		return "error: " + p.Text
	}
	return "warning: " + p.Text
}

// Check walks the whole file system read-only, like a minimal fsck: every
// chain must end, no cluster may belong to two files, chain lengths must match
// file sizes, "." and ".." must point at the right directories, and the FAT
// copies must agree. Allocated clusters that no file uses ("lost clusters")
// and a wrong FSInfo free count are warnings.
func (f *FS) Check() []Problem {
	var out []Problem
	bad := func(severe bool, format string, a ...any) {
		out = append(out, Problem{severe, fmt.Sprintf(format, a...)})
	}
	owner := make(map[uint32]string)
	claim := func(p string, first uint32) int {
		cl, err := f.chain(first)
		if err != nil {
			bad(true, "%s: %v", p, err)
		}
		for _, c := range cl {
			if o, ok := owner[c]; ok {
				bad(true, "%s: cluster %d is also used by %s (cross-linked)", p, c, o)
				continue
			}
			owner[c] = p
		}
		return len(cl)
	}
	var walk func(p string, cluster, parent uint32, depth int)
	walk = func(p string, cluster, parent uint32, depth int) {
		if depth > 64 {
			bad(true, "%s: directory nesting deeper than 64 (loop?)", p)
			return
		}
		claim(p, cluster)
		d, err := f.loadDir(cluster)
		if err != nil {
			bad(true, "%s: %v", p, err)
			return
		}
		if cluster != f.rootCluster {
			if d.n() < 2 || string(d.ent(0)[:11]) != ".          " || string(d.ent(1)[:11]) != "..         " {
				bad(true, "%s: missing . or .. entries", p)
			} else {
				dc := entCluster(d.ent(0))
				pc := entCluster(d.ent(1))
				want := parent
				if parent == f.rootCluster {
					want = 0
				}
				if dc != cluster {
					bad(true, "%s: '.' points at cluster %d, not %d", p, dc, cluster)
				}
				if pc != want && !(want == 0 && pc == f.rootCluster) {
					bad(true, "%s: '..' points at cluster %d, not %d", p, pc, want)
				}
			}
		}
		for _, e := range d.entries() {
			ep := path.Join(p, e.Name)
			if e.IsDir {
				if e.Cluster == 0 {
					bad(true, "%s: directory without a cluster", ep)
					continue
				}
				walk(ep, e.Cluster, cluster, depth+1)
				continue
			}
			if e.Size == 0 {
				if e.Cluster != 0 {
					claim(ep, e.Cluster)
					bad(false, "%s: empty file owns clusters", ep)
				}
				continue
			}
			n := claim(ep, e.Cluster)
			want := int((e.Size + f.clusterSize - 1) / f.clusterSize)
			if n != want {
				bad(true, "%s: %d clusters for %d bytes (want %d)", ep, n, e.Size, want)
			}
		}
	}
	walk("/", f.rootCluster, f.rootCluster, 0)
	lost := 0
	for c := uint32(2); c < f.clusters+2; c++ {
		e := f.entry(c)
		if e != 0 && e != fatBad {
			if _, ok := owner[c]; !ok {
				lost++
			}
		}
	}
	if lost > 0 {
		bad(false, "%d allocated clusters are not used by any file (lost clusters)", lost)
	}
	if f.freeCount != f.countFree() {
		bad(true, "internal free count %d differs from the FAT (%d)", f.freeCount, f.countFree())
	}
	if f.fsInfoValid && f.fsInfoFree != 0xFFFFFFFF && !f.modified && f.fsInfoFree != f.FreeClusters() {
		bad(false, "FSInfo free count %d differs from the FAT (%d free)", f.fsInfoFree, f.FreeClusters())
	}
	// FAT copies must be identical (over the used entries).
	if f.nfats > 1 {
		n := (int(f.clusters) + 2) * 4
		first := make([]byte, n)
		if _, err := f.dev.ReadAt(first, f.base+int64(f.reserved)*int64(f.bps)); err != nil {
			bad(true, "reading FAT 1: %v", err)
		} else {
			other := make([]byte, n)
			for k := uint32(1); k < f.nfats; k++ {
				if _, err := f.dev.ReadAt(other, f.base+int64(f.reserved+k*f.fatSectors)*int64(f.bps)); err != nil {
					bad(true, "reading FAT %d: %v", k+1, err)
					continue
				}
				for i := 8; i < n; i++ { // entries 0 and 1 hold dirty flags
					if first[i] != other[i] {
						bad(true, "FAT %d differs from FAT 1 (first at entry %d)", k+1, i/4)
						break
					}
				}
			}
		}
	}
	return out
}

func entCluster(e []byte) uint32 {
	return uint32(e[20])<<16 | uint32(e[21])<<24 | uint32(e[26]) | uint32(e[27])<<8
}
