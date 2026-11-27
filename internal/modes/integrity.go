package modes

// Containment radius limits in metres.
const (
	rc7_5m  = 7.5
	rc25m   = 25
	rc75m   = 75
	rc0_1NM = 185.2
	rc0_2NM = 370.4
	rc0_3NM = 555.6
	rc0_5NM = 926
	rc0_6NM = 1111.2
	rc1NM   = 1852
	rc2NM   = 3704
	rc4NM   = 7408
	rc8NM   = 14816
	rc10NM  = 18520
	rc20NM  = 37040
)

// NICRc derives the navigation integrity category and the horizontal
// containment radius Rc (metres) of a position message from its type code,
// the ADS-B version of the transmitter and the NIC supplement bits:
//
//   - nicA: NIC supplement A from the operational status message (TC31,
//     version 1 and 2; in version 1 it is simply "the NIC supplement").
//   - nicB: NIC supplement B from the airborne position message itself
//     (version 2 only; Message.NICSuppB).
//   - nicC: NIC supplement C from the surface operational status message
//     (version 2 only).
//
// Supplement bits that do not exist in the given version are ignored, so
// callers may pass whatever they last saw. Use version 0 when the version is
// unknown: its radii are never smaller than those of later versions for the
// same type code and cleared supplement bits.
//
// rcM is 0 when the radius is unknown or unbounded. ok is false when tc is not
// a position type code (0, 5-18, 20-22).
//
// For version 0 transmitters, which report NUCp rather than NIC, nic is the
// equivalent NIC value; see NUCp for the original figure.
func NICRc(tc uint8, version int, nicA, nicB, nicC bool) (nic int, rcM float64, ok bool) {
	if version < 2 {
		nicB, nicC = false, false
	}
	if version < 1 {
		nicA = false
	}
	switch tc {
	case 0, 18, 22:
		return 0, 0, true

	case 5, 9, 20:
		return 11, rc7_5m, true
	case 6, 10, 21:
		return 10, rc25m, true

	case 7: // surface
		if nicA && !nicC {
			return 9, rc75m, true
		}
		return 8, rc0_1NM, true
	case 8: // surface
		if version >= 2 {
			switch {
			case nicA && nicC:
				return 7, rc0_2NM, true
			case nicA:
				return 6, rc0_3NM, true
			case nicC:
				return 6, rc0_6NM, true
			}
		}
		return 0, 0, true

	case 11:
		if version >= 2 {
			if nicA && nicB {
				return 9, rc75m, true
			}
		} else if nicA {
			return 9, rc75m, true
		}
		return 8, rc0_1NM, true
	case 12:
		return 7, rc0_2NM, true
	case 13:
		if version >= 2 {
			switch {
			case !nicA && nicB:
				return 6, rc0_3NM, true
			case !nicA && !nicB:
				return 6, rc0_5NM, true
			default: // A=1,B=1 is 0.6 NM; A=1,B=0 is not assigned: assume the worst
				return 6, rc0_6NM, true
			}
		}
		if nicA {
			return 6, rc0_6NM, true
		}
		return 6, rc0_5NM, true
	case 14:
		return 5, rc1NM, true
	case 15:
		return 4, rc2NM, true
	case 16:
		switch {
		case version >= 2:
			if nicA && nicB {
				return 3, rc4NM, true
			}
			return 2, rc8NM, true
		case version == 1:
			if nicA {
				return 3, rc4NM, true
			}
			return 2, rc8NM, true
		}
		// Version 0: NUCp 2 bounds the position to 10 NM, which only
		// satisfies NIC 1.
		return 1, rc10NM, true
	case 17:
		return 1, rc20NM, true
	}
	return 0, 0, false
}

// NICRc applies NICRc to a position message, taking NIC supplement B from
// the message itself. version, nicA and nicC come from the aircraft's last
// operational status message (pass 0, false, false when unknown).
func (m *Message) NICRc(version int, nicA, nicC bool) (nic int, rcM float64, ok bool) {
	if m.Valid&FTypeCode == 0 {
		return 0, 0, false
	}
	return NICRc(m.TC, version, nicA, m.Valid&FNICSuppB != 0 && m.NICSuppB, nicC)
}

// NUCp returns the navigation uncertainty category (position) that an ADS-B
// version 0 transmitter expresses with a position type code. ok is false when
// tc is not a position type code.
func NUCp(tc uint8) (nucp int, ok bool) {
	switch {
	case tc == 0:
		return 0, true
	case tc >= 5 && tc <= 8:
		return 14 - int(tc), true // 9, 8, 7, 6
	case tc >= 9 && tc <= 18:
		return 18 - int(tc), true // 9 .. 0
	case tc == 20:
		return 9, true
	case tc == 21:
		return 8, true
	case tc == 22:
		return 0, true
	}
	return 0, false
}

// NACpEPU returns the estimated position uncertainty (95 % horizontal
// accuracy bound) in metres for a NACp value, or 0 when unknown (NACp 0 or
// out of range).
func NACpEPU(nacp uint8) float64 {
	switch nacp {
	case 1:
		return rc10NM
	case 2:
		return rc4NM
	case 3:
		return rc2NM
	case 4:
		return rc1NM
	case 5:
		return rc0_5NM
	case 6:
		return rc0_3NM
	case 7:
		return rc0_1NM
	case 8:
		return 92.6
	case 9:
		return 30
	case 10:
		return 10
	case 11:
		return 3
	}
	return 0
}

// NACvHFOM returns the horizontal velocity error bound (95 %) in m/s for a
// NACv (or version 0 NUCr) value, or 0 when unknown.
func NACvHFOM(nacv uint8) float64 {
	switch nacv {
	case 1:
		return 10
	case 2:
		return 3
	case 3:
		return 1
	case 4:
		return 0.3
	}
	return 0
}
