package routes

import (
	"math"
	"sort"
	"strings"
	"time"
)

// Hypothesis fusion (docs/ROUTES-DESIGN.md §3.3). Each source that asserts
// a route adds its log-likelihood ratio; the live score adds a state term;
// the probabilities are a softmax over the hypotheses plus "some other
// route" at logit 0.

// Sources and licence classes.
const (
	SrcOverride = "override"
	SrcLearned  = "learned"
	SrcStanding = "standing-data"
	SrcAdsbdb   = "adsbdb"

	ClassOpen       = "open"       // CC0 / public domain: may be shown, stored, exported
	ClassOwn        = "own"        // AirView's own observations and the operator's overrides (published CC0)
	ClassRestricted = "restricted" // third-party answer that may not be republished: station's own users only
)

// ClassOf returns the licence class of a source.
func ClassOf(src string) string {
	switch src {
	case SrcOverride, SrcLearned:
		return ClassOwn
	case SrcStanding:
		return ClassOpen
	}
	return ClassRestricted
}

// Claim is one source asserting a route.
type Claim struct {
	Source  string
	Logit   float64
	Why     string // short evidence text, e.g. "learned 9 days"
	Variant bool   // claimed for a variant spelling of the callsign
}

// Hyp is one candidate route with the claims that assert it.
type Hyp struct {
	Key    string // airport codes joined with "-"
	Stops  []Stop
	Claims []Claim
	Stale  bool // the standing data is contradicted by learned routes
}

// Prior log-likelihood ratios.
const (
	lStanding     = 1.7
	lStandingStal = -1.0
	lOnline       = 1.9
	lVerify       = 3.0
	lVariant      = -1.0
)

// LearnedLogit is the prior of a learned route seen on nDays distinct days,
// last seen age ago.
func LearnedLogit(nDays int, age time.Duration) float64 {
	l := math.Min(1.0+0.7*float64(nDays), 4.0)
	return l * math.Exp(-age.Hours()/(21*24))
}

// stateLogit is the evidence of the live score.
func stateLogit(state string) float64 {
	switch state {
	case StateFits:
		return 0.5
	case StateDoubtful:
		return -2
	case StateReversed:
		return -3
	}
	return 0
}

// Fused is the result of fusion.
type Fused struct {
	Best   int     // index into the hypotheses, -1 = none
	P      float64 // probability of the best hypothesis
	Logits []float64
	Scores []Score
}

// Fuse scores every hypothesis at the observation and returns the best.
func Fuse(hs []Hyp, o Obs) Fused {
	f := Fused{Best: -1, Logits: make([]float64, len(hs)), Scores: make([]Score, len(hs))}
	if len(hs) == 0 {
		return f
	}
	maxL := 0.0 // "other" at logit 0
	for i := range hs {
		l := 0.0
		for _, c := range hs[i].Claims {
			l += c.Logit
		}
		if hs[i].Stale {
			l += lStandingStal
		}
		sc := Evaluate(hs[i].Stops, o)
		l += stateLogit(sc.State)
		f.Scores[i], f.Logits[i] = sc, l
		if f.Best < 0 || l > f.Logits[f.Best] {
			f.Best = i
		}
		maxL = math.Max(maxL, l)
	}
	sum := math.Exp(0 - maxL)
	for _, l := range f.Logits {
		sum += math.Exp(l - maxL)
	}
	f.P = math.Exp(f.Logits[f.Best]-maxL) / sum
	return f
}

// Reason builds the one-line, public-safe explanation from the top evidence.
func Reason(h Hyp, sc Score) string {
	cs := append([]Claim(nil), h.Claims...)
	sort.SliceStable(cs, func(i, j int) bool { return math.Abs(cs[i].Logit) > math.Abs(cs[j].Logit) })
	var parts []string
	for i, c := range cs {
		if i == 2 {
			break
		}
		if c.Why != "" {
			parts = append(parts, c.Why)
		}
	}
	if sc.Reason != "" {
		parts = append(parts, sc.Reason)
	}
	r := strings.Join(parts, " · ")
	if len(r) > 120 {
		r = r[:117] + "..."
	}
	return r
}
