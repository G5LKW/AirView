package selfupdate

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAttachNilSafe(t *testing.T) {
	var a *Attachment
	if a.Updater() != nil || a.Trial() != nil || a.Status().Product != "" {
		t.Fatal("nil attachment")
	}
	if err := a.Run(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
}

func TestAttachRejectsProbe(t *testing.T) {
	_, err := Attach(Options{Product: "airview", CurrentVersion: "1.0.0", StateDir: t.TempDir(), Exe: "x",
		Probe: ProbeFunc(func(context.Context) (ProbeResult, error) { return ProbeResult{}, nil })})
	if !errors.Is(err, ErrProbeInProcess) {
		t.Fatal(err)
	}
}

// swappedState writes a journal of an update 1.0.0 -> 1.1.0 of exe on trial
// and returns the previous binary's content.
func swappedState(t *testing.T) (exe, state string) {
	t.Helper()
	dir := t.TempDir()
	exe = filepath.Join(dir, "airview"+exeSuffix())
	old := exe + suffixOld
	if err := os.WriteFile(exe, fakeExe("v2"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(old, fakeExe("v1"), 0o755); err != nil {
		t.Fatal(err)
	}
	fromSHA, _ := fileSHA256(old)
	toSHA, _ := fileSHA256(exe)
	state = filepath.Join(dir, "state")
	now := time.Now().UTC()
	if err := saveJournal(state, Journal{Phase: PhaseSwapped, Product: "airview", From: "1.0.0", To: "1.1.0",
		Target: CurrentTarget(), Exe: exe, Old: old, FromSHA: fromSHA, ToSHA: toSHA, Started: now, Deadline: now.Add(time.Minute)}); err != nil {
		t.Fatal(err)
	}
	return exe, state
}

func TestAttachTrialConfirmAndLoopDisabled(t *testing.T) {
	exe, state := swappedState(t)
	o := Options{Product: "airview", CurrentVersion: "1.1.0", Exe: exe, StateDir: state, Health: HealthPolicy{Window: 50 * time.Millisecond}}
	a, err := Attach(o)
	if err != nil || a.Trial() == nil || a.Trial().Version() != "1.1.0" {
		t.Fatalf("%v %+v", err, a)
	}
	if err := a.Run(context.Background(), func(context.Context) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if j, _ := loadJournal(state); j.Phase != PhaseCommitted {
		t.Fatalf("%+v", j)
	}
	if _, err := os.Stat(exe + suffixOld); !os.IsNotExist(err) {
		t.Fatal("previous binary kept after commit")
	}
	// A second start of the kept version has nothing to settle.
	if a, err := Attach(o); err != nil || a.Trial() != nil {
		t.Fatalf("%v %+v", err, a)
	}
}

func TestAttachTrialRollback(t *testing.T) {
	exe, state := swappedState(t)
	a, err := Attach(Options{Product: "airview", CurrentVersion: "1.1.0", Exe: exe, StateDir: state, Health: HealthPolicy{Window: 20 * time.Millisecond}})
	if err != nil {
		t.Fatal(err)
	}
	err = a.Run(context.Background(), func(context.Context) error { return errors.New("db closed") })
	if !errors.Is(err, ErrRolledBack) || !strings.Contains(err.Error(), "db closed") {
		t.Fatal(err)
	}
	if b := readFile(t, exe); !strings.Contains(string(b), "v1v1") {
		t.Fatal("previous binary not restored")
	}
}

func TestAttachCrashLoop(t *testing.T) {
	exe, state := swappedState(t)
	o := Options{Product: "airview", CurrentVersion: "1.1.0", Exe: exe, StateDir: state, MaxTrialBoots: 2}
	for i := 0; i < 2; i++ {
		if a, err := Attach(o); err != nil || a.Trial() == nil {
			t.Fatalf("boot %d: %v", i+1, err)
		}
	}
	a, err := Attach(o)
	if !errors.Is(err, ErrRolledBack) || a != nil {
		t.Fatalf("third boot: %v %+v", err, a)
	}
	if j, _ := loadJournal(state); j.Phase != PhaseRolledBack {
		t.Fatalf("%+v", j)
	}
}

func TestAttachRunCancelledDuringTrial(t *testing.T) {
	exe, state := swappedState(t)
	a, err := Attach(Options{Product: "airview", CurrentVersion: "1.1.0", Exe: exe, StateDir: state, Health: HealthPolicy{Window: time.Hour}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if err := a.Run(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if j, _ := loadJournal(state); j.Phase != PhaseSwapped {
		t.Fatalf("a cancelled trial must stay pending for the next start: %+v", j)
	}
}
