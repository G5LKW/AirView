package refdata

import "airview/internal/model"

// Info is everything the embedded tables know about a 24-bit ICAO address.
type Info struct {
	ICAO uint32 `json:"-"`
	// Country / CountryCode: state (or territory, see AllocTerritory) the
	// address block is allocated to. Empty when Alloc is not "state" or
	// "territory".
	Country     string `json:"country,omitempty"`
	CountryCode string `json:"countryCode,omitempty"` // ISO 3166-1 alpha-2
	// State / StateCode: the ICAO contracting state the block was allocated
	// to. Differs from Country only for territory sub-blocks (Bermuda ->
	// United Kingdom, Hong Kong -> China).
	State     string `json:"state,omitempty"`
	StateCode string `json:"stateCode,omitempty"`
	// Alloc classifies the address block; Region names the ICAO regional pool
	// or reserved range for addresses outside the directly allocated blocks.
	Alloc  AllocKind `json:"alloc"`
	Region string    `json:"region,omitempty"`
	// Military is the heuristic military-block flag (see IsMilitary).
	Military bool `json:"military,omitempty"`
	// Registration is the mark derived arithmetically from the address (see
	// Registration); "" when the state does not allocate that way. It is a
	// heuristic: a record from the imported aircraft database takes precedence.
	Registration string `json:"reg,omitempty"`
}

// Hex returns the address as six upper-case hexadecimal digits.
func (i Info) Hex() string { return FormatICAO(i.ICAO) }

// Lookup returns the embedded reference information for a 24-bit ICAO
// address. It is safe for concurrent use and performs at most one small
// allocation (the derived registration string, when there is one).
//
// Addresses that are zero, above FFFFFF or outside every published block
// return an Info with Alloc == AllocNone.
func Lookup(icao uint32) Info {
	info := Info{ICAO: icao}
	c, kind, region := allocation(icao, true)
	info.Alloc, info.Region = kind, region
	switch kind {
	case AllocNone:
		return info
	case AllocState:
		info.Country, info.CountryCode = c.Name, c.ISO2
		info.State, info.StateCode = c.Name, c.ISO2
	case AllocTerritory:
		info.Country, info.CountryCode = c.Name, c.ISO2
		if s, k, r := allocation(icao, false); k == AllocState {
			info.State, info.StateCode, info.Region = s.Name, s.ISO2, r
		}
	}
	info.Military = IsMilitary(icao)
	info.Registration = Registration(icao)
	return info
}

// Apply copies the reference data into the live aircraft DTO: country,
// country code and the military flag are always set from the address; the
// derived registration is only used when the DTO has none yet (so a
// registration from the aircraft database is never overwritten).
func (i Info) Apply(a *model.Aircraft) {
	if a == nil {
		return
	}
	a.Country, a.CountryCode = i.Country, i.CountryCode
	if i.Military {
		a.Military = true
	}
	if a.Registration == "" {
		a.Registration = i.Registration
	}
}

const hexDigits = "0123456789ABCDEF"

// FormatICAO formats a 24-bit address as six upper-case hexadecimal digits
// (the form used in model.Aircraft.ICAO). Bits above 24 are ignored.
func FormatICAO(icao uint32) string {
	var b [6]byte
	for i := 5; i >= 0; i-- {
		b[i] = hexDigits[icao&0xF]
		icao >>= 4
	}
	return string(b[:])
}

// ParseICAO parses a 24-bit address written as 1 to 6 hexadecimal digits
// (either case; an optional leading '~', which readsb uses to mark non-ICAO
// addresses, is skipped). It does not allocate.
func ParseICAO(s string) (icao uint32, ok bool) {
	if len(s) > 0 && s[0] == '~' {
		s = s[1:]
	}
	if len(s) == 0 || len(s) > 6 {
		return 0, false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		var d byte
		switch {
		case c >= '0' && c <= '9':
			d = c - '0'
		case c >= 'A' && c <= 'F':
			d = c - 'A' + 10
		case c >= 'a' && c <= 'f':
			d = c - 'a' + 10
		default:
			return 0, false
		}
		icao = icao<<4 | uint32(d)
	}
	return icao, true
}
