package astro

import "time"

// SynodicMonthDays is the mean length of a lunation.
const SynodicMonthDays = 29.530588853

// Moon is the moon's phase at an instant.
type Moon struct {
	Illumination float64 `json:"illumination"` // illuminated fraction of the disc, 0..1
	Phase        float64 `json:"phase"`        // 0 = new, 0.25 = first quarter, 0.5 = full, 0.75 = last quarter
	AgeDays      float64 `json:"ageDays"`      // approximate days since new moon
	Waxing       bool    `json:"waxing"`
	Name         string  `json:"name"` // "New Moon", "Waxing Crescent", "First Quarter", …
}

// MoonAt returns the moon's phase and illuminated fraction at time t using
// the low-accuracy method of Meeus, "Astronomical Algorithms" ch. 48
// (eq. 48.4 with the mean arguments of ch. 47): illumination is good to
// about ±0.01, which is plenty for a status display.
func MoonAt(t time.Time) Moon {
	T := (JulianDay(t) - 2451545.0) / 36525.0
	// Mean elongation of the moon, sun's mean anomaly, moon's mean anomaly.
	D := 297.8501921 + T*(445267.1114034+T*(-0.0018819+T*(1.0/545868-T/113065000)))
	M := 357.5291092 + T*(35999.0502909+T*(-0.0001536+T/24490000))
	Mp := 134.9633964 + T*(477198.8675055+T*(0.0087414+T*(1.0/69699-T/14712000)))

	// Meeus 48.4 gives the phase angle i = 180° − (elongation with the main
	// periodic terms); keep the elongation itself so that waxing/waning is
	// known.
	elong := norm360(D + 6.289*sinD(Mp) - 2.100*sinD(M) + 1.274*sinD(2*D-Mp) +
		0.658*sinD(2*D) + 0.214*sinD(2*Mp) + 0.110*sinD(D))

	m := Moon{
		Illumination: (1 - cosD(elong)) / 2,
		Phase:        elong / 360,
		Waxing:       elong < 180,
	}
	m.AgeDays = m.Phase * SynodicMonthDays
	m.Name = moonName(m.Phase)
	return m
}

// moonName names a phase fraction. The four principal phases are instants,
// so they get a window of about ±14 hours (±0.02 of a lunation); everything
// else is a crescent or gibbous moon.
func moonName(phase float64) string {
	const w = 0.02
	switch {
	case phase < w || phase > 1-w:
		return "New Moon"
	case phase < 0.25-w:
		return "Waxing Crescent"
	case phase <= 0.25+w:
		return "First Quarter"
	case phase < 0.5-w:
		return "Waxing Gibbous"
	case phase <= 0.5+w:
		return "Full Moon"
	case phase < 0.75-w:
		return "Waning Gibbous"
	case phase <= 0.75+w:
		return "Last Quarter"
	default:
		return "Waning Crescent"
	}
}
