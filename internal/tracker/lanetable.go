package tracker

// acTable is a lane's aircraft table: an open-addressing hash table (linear
// probing, backward-shift deletion) from address key to an inline laneAC.
// Compared with a Go map of pointers it costs one cache miss per look-up,
// holds no pointers for the garbage collector to scan and recycles its
// storage. Pointers returned by get / getOrAdd are valid until the next
// getOrAdd or del. Not safe for concurrent use (the shard lock guards it).
type acTable struct {
	keys  []uint32 // key+1; 0 = empty slot
	vals  []laneAC
	n     int
	mask  uint32
	shift uint8
}

const acTableMin = 64 // initial slots (a power of two)

func (t *acTable) init(slots int) {
	bits := uint8(0)
	for 1<<bits < slots {
		bits++
	}
	t.keys = make([]uint32, 1<<bits)
	t.vals = make([]laneAC, 1<<bits)
	t.mask = 1<<bits - 1
	t.shift = 32 - bits
	t.n = 0
}

// home returns the preferred slot of key (Fibonacci hashing).
func (t *acTable) home(key uint32) uint32 { return (key * 0x9E3779B1) >> t.shift }

// len returns the number of entries.
func (t *acTable) len() int { return t.n }

// get returns the entry of key, or nil.
func (t *acTable) get(key uint32) *laneAC {
	if t.n == 0 {
		return nil
	}
	k := key + 1
	for i := t.home(key); ; i = (i + 1) & t.mask {
		switch t.keys[i] {
		case k:
			return &t.vals[i]
		case 0:
			return nil
		}
	}
}

// getOrAdd returns the entry of key, adding a zero entry when it is missing.
func (t *acTable) getOrAdd(key uint32) *laneAC {
	if t.keys == nil {
		t.init(acTableMin)
	}
	k := key + 1
	i := t.home(key)
	for ; ; i = (i + 1) & t.mask {
		switch t.keys[i] {
		case k:
			return &t.vals[i]
		case 0:
			if (t.n+1)*4 > len(t.keys)*3 {
				t.grow()
				return t.getOrAdd(key)
			}
			t.keys[i] = k
			t.vals[i] = laneAC{heardIx: -1}
			t.n++
			return &t.vals[i]
		}
	}
}

// grow doubles the table.
func (t *acTable) grow() {
	keys, vals := t.keys, t.vals
	t.init(2 * len(keys))
	for i, k := range keys {
		if k == 0 {
			continue
		}
		j := t.home(k - 1)
		for t.keys[j] != 0 {
			j = (j + 1) & t.mask
		}
		t.keys[j], t.vals[j] = k, vals[i]
		t.n++
	}
}

// del removes key (no-op when missing).
func (t *acTable) del(key uint32) {
	if t.n == 0 {
		return
	}
	k := key + 1
	hole := t.home(key)
	for ; t.keys[hole] != k; hole = (hole + 1) & t.mask {
		if t.keys[hole] == 0 {
			return
		}
	}
	// Backward shift: move later entries of the probe run into the hole
	// unless their home slot lies cyclically in (hole, j].
	for j := hole; ; {
		j = (j + 1) & t.mask
		kj := t.keys[j]
		if kj == 0 {
			break
		}
		h := t.home(kj - 1)
		var stay bool
		if hole <= j {
			stay = hole < h && h <= j
		} else {
			stay = hole < h || h <= j
		}
		if stay {
			continue
		}
		t.keys[hole], t.vals[hole] = kj, t.vals[j]
		hole = j
	}
	t.keys[hole] = 0
	t.vals[hole] = laneAC{}
	t.n--
}

// each calls fn for every entry. fn must not add or delete entries.
func (t *acTable) each(fn func(key uint32, e *laneAC)) {
	for i, k := range t.keys {
		if k != 0 {
			fn(k-1, &t.vals[i])
		}
	}
}
