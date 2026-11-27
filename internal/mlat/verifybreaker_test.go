package mlat

import (
	"testing"

	"airview/internal/mlat/quality"
)

// SCALE-05: a few contradictions among many confirmations are charged; a
// network where most checks contradict (clock trouble, no spoofers) stops
// charging aircraft until the share drops again.
func TestVerifyBreaker(t *testing.T) {
	var b verifyBreaker
	for i := 0; i < 500; i++ {
		b.note(quality.VerdictConfirmed)
	}
	for i := 0; i < 20; i++ {
		if !b.note(quality.VerdictContradicted) {
			t.Fatal("a rare contradiction was muted")
		}
	}
	b.rotate()
	b.rotate() // both windows empty
	muted := 0
	for i := 0; i < 1000; i++ {
		b.note(quality.VerdictConfirmed)
		if !b.note(quality.VerdictContradicted) {
			muted++
		}
		if i%10 == 0 {
			b.note(quality.VerdictContradicted)
		}
	}
	if muted < 800 {
		t.Errorf("only %d of 1000 contradictions muted at a ~52 %% contradicted share", muted)
	}
	if !b.note(quality.VerdictNone) {
		t.Error("a non-contradiction was refused")
	}
	b.rotate()
	b.rotate()
	if !b.note(quality.VerdictContradicted) {
		t.Error("still muted after two quiet minutes")
	}
}
