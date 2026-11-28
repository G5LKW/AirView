package routes

import "strings"

// Callsign hygiene (docs/ROUTES-DESIGN.md §2.1).

// Normalize upper-cases and trims a callsign ("" when it is not a plausible
// callsign at all: empty, longer than 8 characters or not alphanumeric).
func Normalize(cs string) string {
	cs = strings.ToUpper(strings.TrimSpace(cs))
	if cs == "" || len(cs) > 8 {
		return ""
	}
	for i := 0; i < len(cs); i++ {
		c := cs[i]
		if !(c >= 'A' && c <= 'Z' || c >= '0' && c <= '9') {
			return ""
		}
	}
	return cs
}

// Lookupable reports whether a normalised callsign is worth a route look-up:
// an airline-style callsign (three letters, a digit, then up to three
// letters or digits) that is not the registration and not a test or
// placeholder value. Other callsigns can still be shown, just without a route.
func Lookupable(cs, reg string) bool {
	if len(cs) < 4 || len(cs) > 7 {
		return false
	}
	for i := 0; i < 3; i++ {
		if cs[i] < 'A' || cs[i] > 'Z' {
			return false
		}
	}
	if cs[3] < '0' || cs[3] > '9' {
		return false
	}
	for i := 4; i < len(cs); i++ {
		c := cs[i]
		if !(c >= 'A' && c <= 'Z' || c >= '0' && c <= '9') {
			return false
		}
	}
	if strings.HasPrefix(cs, "TEST") || strings.Trim(cs, "0") == "" {
		return false
	}
	if reg != "" {
		r := strings.ToUpper(strings.ReplaceAll(strings.TrimSpace(reg), "-", ""))
		if r == cs {
			return false
		}
	}
	return true
}

// Variants returns the variant spellings of a callsign that may carry the
// same route: leading zeros of the flight number stripped (BAW0123 ->
// BAW123) and one trailing letter stripped from an all-numeric flight
// number (BAW101P -> BAW101). Alphanumeric flight numbers such as EZY12AB
// or RYR1234 style suffixes are never stripped: they are different routes.
// Variants enter fusion with a penalty and are flagged "variant".
func Variants(cs string) []string {
	if len(cs) < 5 {
		return nil
	}
	prefix, num := cs[:3], cs[3:]
	var out []string
	if t := strings.TrimLeft(num, "0"); t != num && t != "" && t[0] >= '0' && t[0] <= '9' {
		out = append(out, prefix+t)
	}
	if n := len(num); n >= 2 {
		last := num[n-1]
		body := num[:n-1]
		if last >= 'A' && last <= 'Z' && allDigits(body) && len(body) >= 1 {
			out = append(out, prefix+body)
		}
	}
	return out
}

func allDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}
