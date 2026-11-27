package modes

import (
	"errors"
	"math/rand"
	"testing"
)

// crcBitwise is an independent bit-by-bit implementation of the Mode S
// CRC-24: polynomial long division of data(x)*x^24 by 0x1FFF409, exactly as
// described in "The 1090 Megahertz Riddle", chapter "Error control".
func crcBitwise(data []byte) uint32 {
	const generator = 0x1FFF409
	var reg uint32
	feed := func(bit uint32) {
		reg = reg<<1 | bit
		if reg&0x1000000 != 0 {
			reg ^= generator
		}
	}
	for _, b := range data {
		for i := 7; i >= 0; i-- {
			feed(uint32(b>>uint(i)) & 1)
		}
	}
	for i := 0; i < 24; i++ {
		feed(0)
	}
	return reg
}

func TestChecksumKnownAnswer(t *testing.T) {
	// The 1090 MHz Riddle, identification chapter: the parity of
	// 8D4840D6202CC371C32CE0 is 576098.
	f := hexFrame(t, bookIdent)
	if got := Checksum(f[:11]); got != 0x576098 {
		t.Fatalf("Checksum = %06X, want 576098", got)
	}
}

func TestResidualKnownAnswers(t *testing.T) {
	// Self-generated frames: the residual is the overlay put on the parity
	// (0 for extended squitters, the interrogator code for DF11, the address
	// for the address/parity formats).
	for _, c := range []struct {
		frame string
		want  uint32
	}{
		{bookIdent, 0},
		{bookPosOdd, 0},
		{esHex(0x8D, 0x896012, posME(11, 34000, false, 25.25, 55.36)), 0},
		{esHex(0x8D, 0x45CA11, velocityME(1, false, 1, false, 300, true, 120, true, false, 9, false, 3)), 0},
		{esHex(0x8D, 0x74802A, identME(4, 3, "UAE41")), 0},
		{fhex(df11Frame(5, 0x4065DE, 0)), 0},
		{fhex(df11Frame(5, 0x4065DE, 35)), 35},
		{fhex(commBFrame(21, EncodeID13(0x3121), 0xA3923C, bds60MB(10, 250, 0.7, 0, 0))), 0xA3923C},
		{fhex(commBFrame(20, ac13For(12000), 0x49530C, 0)), 0x49530C},
		{fhex(df4Frame(0, 0, 0, ac13For(36000), 0x05644A)), 0x05644A},
	} {
		if got := Residual(hexFrame(t, c.frame)); got != c.want {
			t.Errorf("Residual(%s) = %d, want %d", c.frame, got, c.want)
		}
	}
	if got := Residual([]byte{1, 2, 3}); got != 0xFFFFFFFF {
		t.Errorf("Residual(short) = %X, want FFFFFFFF", got)
	}
}
func TestChecksumMatchesLongDivision(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	for n := 0; n <= 14; n++ {
		for i := 0; i < 200; i++ {
			data := make([]byte, n)
			rng.Read(data)
			if got, want := Checksum(data), crcBitwise(data); got != want {
				t.Fatalf("Checksum(% X) = %06X, long division gives %06X", data, got, want)
			}
		}
	}
}

func TestSetParity(t *testing.T) {
	rng := rand.New(rand.NewSource(2))
	for _, n := range []int{7, 14} {
		for i := 0; i < 100; i++ {
			f := make([]byte, n)
			rng.Read(f)
			overlay := rng.Uint32() & 0xFFFFFF
			SetParity(f, overlay)
			if got := Residual(f); got != overlay {
				t.Fatalf("Residual after SetParity(%06X) = %06X", overlay, got)
			}
		}
	}
	short := []byte{1, 2, 3}
	SetParity(short, 0x123456)
	if short[0] != 1 || short[1] != 2 || short[2] != 3 {
		t.Error("SetParity modified a frame that is too short")
	}
}

// TestSyndromeTableUnambiguous proves that every one and two bit error
// pattern of a 112-bit frame has its own syndrome, so a table hit identifies
// the damaged bits uniquely (the code has minimum distance 6 at this length).
func TestSyndromeTableUnambiguous(t *testing.T) {
	syndromeOnce.Do(buildSyndromeTable)
	const n = 112 - 5
	if want := n + n*(n-1)/2; len(syndromeTable) != want {
		t.Fatalf("table has %d entries, want %d", len(syndromeTable), want)
	}
	for i, e := range syndromeTable {
		if e.syn == 0 {
			t.Fatalf("entry %d has a zero syndrome", i)
		}
		if i > 0 && syndromeTable[i-1].syn >= e.syn {
			t.Fatalf("entries %d and %d are not strictly increasing: %06X, %06X",
				i-1, i, syndromeTable[i-1].syn, e.syn)
		}
	}
	// The single bit syndromes must agree with actually damaging a frame.
	single := bitSyndromes()
	base := hexFrame(t, bookIdent)
	for bit := 0; bit < 112; bit++ {
		f := append([]byte(nil), base...)
		f[bit>>3] ^= 0x80 >> uint(bit&7)
		if got := Residual(f); got != single[bit] {
			t.Fatalf("bit %d: residual %06X, syndrome table says %06X", bit, got, single[bit])
		}
	}
}

func flipBit(f []byte, bit int) { f[bit>>3] ^= 0x80 >> uint(bit&7) }

func TestErrorCorrectionSingleBit(t *testing.T) {
	const good = bookPosEven
	want := mustDecode(t, good)
	for _, first := range []byte{0x8D, 0x90} { // DF17 and DF18 (CF=0)
		base := hexFrame(t, good)
		base[0] = first
		SetParity(base, 0)
		var ref Message
		if err := Decode(base, &ref); err != nil {
			t.Fatal(err)
		}
		for bit := 0; bit < 112; bit++ {
			f := append([]byte(nil), base...)
			flipBit(f, bit)
			var m Message

			// Without error correction the frame must be rejected. A damaged
			// DF field may also turn it into another format altogether.
			err := Decode(f, &m)
			if bit >= 5 && !errors.Is(err, ErrBadCRC) {
				t.Fatalf("first=%02X bit %d: uncorrected decode gave %v, want ErrBadCRC", first, bit, err)
			}
			if bit >= 5 && (m.CRC != CRCBad || m.DF != first>>3) {
				t.Fatalf("first=%02X bit %d: CRC=%v DF=%d after ErrBadCRC", first, bit, m.CRC, m.DF)
			}

			err = DecodeOpts(f, &m, Options{FixErrors: 1})
			if bit < 5 {
				// Errors in the DF field are never repaired.
				if err == nil && m.FixedBits != 0 {
					t.Fatalf("first=%02X bit %d: DF bit was repaired", first, bit)
				}
				continue
			}
			if err != nil {
				t.Fatalf("first=%02X bit %d: %v", first, bit, err)
			}
			if m.CRC != CRCFixed1 || m.FixedBits != 1 || int(m.FixedBit[0]) != bit {
				t.Fatalf("first=%02X bit %d: CRC=%v fixed=%d at %d", first, bit, m.CRC, m.FixedBits, m.FixedBit[0])
			}
			if m.Raw != ref.Raw {
				t.Fatalf("first=%02X bit %d: repaired frame % X differs from original", first, bit, m.Raw)
			}
			if m.Residual == 0 {
				t.Fatalf("first=%02X bit %d: Residual must keep the syndrome as received", first, bit)
			}
			if got, wantAddr := m.AddressCorrected(), bit >= 8 && bit < 32; got != wantAddr {
				t.Fatalf("first=%02X bit %d: AddressCorrected = %v", first, bit, got)
			}
			if first == 0x8D && (m.AltBaro != want.AltBaro || m.CPRLat != want.CPRLat || m.CPRLon != want.CPRLon || m.ICAO != want.ICAO) {
				t.Fatalf("bit %d: repaired message decodes differently", bit)
			}
		}
	}
}

func TestErrorCorrectionTwoBits(t *testing.T) {
	const good = bookVelGS
	base := hexFrame(t, good)
	rng := rand.New(rand.NewSource(3))
	for i := 0; i < 2000; i++ {
		b0 := 5 + rng.Intn(107)
		b1 := 5 + rng.Intn(107)
		if b0 == b1 {
			continue
		}
		if b0 > b1 {
			b0, b1 = b1, b0
		}
		f := append([]byte(nil), base...)
		flipBit(f, b0)
		flipBit(f, b1)
		var m Message
		if err := DecodeOpts(f, &m, Options{FixErrors: 1}); !errors.Is(err, ErrBadCRC) {
			t.Fatalf("bits %d,%d: single bit mode gave %v, want ErrBadCRC", b0, b1, err)
		}
		if err := DecodeOpts(f, &m, Options{FixErrors: 2}); err != nil {
			t.Fatalf("bits %d,%d: %v", b0, b1, err)
		}
		if m.CRC != CRCFixed2 || m.FixedBits != 2 || int(m.FixedBit[0]) != b0 || int(m.FixedBit[1]) != b1 {
			t.Fatalf("bits %d,%d: CRC=%v fixed=%d at %v", b0, b1, m.CRC, m.FixedBits, m.FixedBit)
		}
		if string(m.Raw[:]) != string(base) {
			t.Fatalf("bits %d,%d: repaired frame differs from original", b0, b1)
		}
		if !near(m.GS, 159.20, 0.01) || m.VertRate != -832 {
			t.Fatalf("bits %d,%d: repaired message decodes differently", b0, b1)
		}

		// Two bit repair is reserved for DF17: the same damage to a DF18
		// frame must be rejected, as must anything above two bits.
		g := append([]byte(nil), base...)
		g[0] = 0x90
		SetParity(g, 0)
		flipBit(g, b0)
		flipBit(g, b1)
		if err := DecodeOpts(g, &m, Options{FixErrors: 2}); !errors.Is(err, ErrBadCRC) {
			t.Fatalf("DF18 bits %d,%d: got %v, want ErrBadCRC", b0, b1, err)
		}
	}

	// FixErrors above 2 behaves like 2.
	f := append([]byte(nil), base...)
	flipBit(f, 40)
	flipBit(f, 77)
	var m Message
	if err := DecodeOpts(f, &m, Options{FixErrors: 9}); err != nil || m.CRC != CRCFixed2 {
		t.Fatalf("FixErrors=9: err=%v CRC=%v", err, m.CRC)
	}
}

func TestErrorCorrectionPublishedFrame(t *testing.T) {
	// A self-generated identification frame with bit 107 (x^4, the fifth bit
	// from the end) inverted: CRC remainder 16, one damaged bit.
	g := esFrame(0x8D, 0x4CA251, identME(4, 0, "RYR7GJ"))
	flipBit(g, 107)
	frame := fhex(g)
	var m Message
	if err := Decode(hexFrame(t, frame), &m); !errors.Is(err, ErrBadCRC) || m.Residual != 16 {
		t.Fatalf("err=%v residual=%d, want ErrBadCRC and 16", err, m.Residual)
	}
	if err := DecodeOpts(hexFrame(t, frame), &m, Options{FixErrors: 1}); err != nil {
		t.Fatal(err)
	}
	if m.CRC != CRCFixed1 || m.FixedBit[0] != 107 || m.ICAO != 0x4CA251 || m.AddressCorrected() {
		t.Fatalf("CRC=%v bit=%d icao=%06X", m.CRC, m.FixedBit[0], m.ICAO)
	}
	if m.TC != 4 || !m.Has(FCallsign) {
		t.Fatalf("TC=%d callsign valid=%v", m.TC, m.Has(FCallsign))
	}
	if Residual(m.Raw[:14]) != 0 {
		t.Fatal("repaired frame still has a residual")
	}
}

func TestNoCorrectionForOtherFormats(t *testing.T) {
	// DF11 has no error correction: a damaged frame either fails or decodes
	// to a different interrogator code, but is never flagged as repaired.
	f := df11Frame(5, 0xA039B4, 0)
	flipBit(f, 20)
	var m Message
	if err := DecodeOpts(f, &m, Options{FixErrors: 2}); !errors.Is(err, ErrBadCRC) || m.FixedBits != 0 {
		t.Fatalf("err=%v fixed=%d", err, m.FixedBits)
	}
}
