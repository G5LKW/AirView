package selfupdate

import (
	"context"
	"errors"
)

// Attachment is the in-process integration of a long-running program
// (airview serve, airview-feeder run, the Windows service bodies): it
// finishes a pending update at start-up (Recover), confirms or rolls back a
// new version on trial, and runs the automatic check-and-install loop.
//
// Typical use (see WIRING.md):
//
//	att, err := selfupdate.Attach(selfupdate.Options{...,
//		Restarter: selfupdate.SelfRestarter{Exe: exe, Exit: cancelMain}})
//	switch {
//	case errors.Is(err, selfupdate.ErrRolledBack):
//		os.Exit(75) // the previous binary is back; let the supervisor start it
//	case err != nil:
//		log.Printf("self-update off: %v", err) // never fatal
//	}
//	... open listeners, start the engine ...
//	go func() {
//		err := att.Run(ctx, healthCheck)
//		if errors.Is(err, selfupdate.ErrRolledBack) || errors.Is(err, selfupdate.ErrExitRequested) {
//			requestRestart(err) // exit non-zero under a supervisor, or restart
//		}
//	}()
//
// A nil *Attachment is valid and does nothing, so callers need no checks
// when Attach failed.
type Attachment struct {
	u     *Updater
	trial *Trial
}

// ErrProbeInProcess is returned by Attach when Options.Probe is set: a
// process cannot supervise its own restart. Probes belong to the external
// "update" command (RunCommand), which restarts the service and watches it.
var ErrProbeInProcess = errors.New("selfupdate: Attach runs in the program itself and takes no Probe (the new process confirms itself)")

// Attach creates the Updater and runs Recover. It must be called early,
// after the configuration is loaded and before listeners open.
//
// Errors:
//   - wrapping ErrRolledBack: the new version crash-looped and the
//     previous binary was restored; the program must exit non-zero at once
//     so that its supervisor starts the restored binary. The returned
//     Attachment is nil.
//   - anything else (bad options, unreadable journal): self-update is
//     unavailable for this run; the program should log it and continue.
//     The returned Attachment is nil.
func Attach(o Options) (*Attachment, error) {
	if o.Probe != nil {
		return nil, ErrProbeInProcess
	}
	u, err := New(o)
	if err != nil {
		return nil, err
	}
	t, err := u.Recover()
	if err != nil {
		return nil, err
	}
	return &Attachment{u: u, trial: t}, nil
}

// Updater returns the underlying Updater (nil for a nil Attachment), for
// status pages ("Check for updates", Status) and manual installs.
func (a *Attachment) Updater() *Updater {
	if a == nil {
		return nil
	}
	return a.u
}

// Trial returns the version on trial, or nil when this process is not a
// freshly installed version waiting for confirmation.
func (a *Attachment) Trial() *Trial {
	if a == nil {
		return nil
	}
	return a.trial
}

// Status returns the updater state (zero Status for a nil Attachment).
func (a *Attachment) Status() Status {
	if a == nil {
		return Status{}
	}
	return a.u.Status()
}

// Run is called once per process. It first settles a pending trial: after the health window, check (may be
// nil) runs once; success keeps the new version, failure restores the
// previous one. Then, when Settings.Enabled, it runs the automatic update
// loop until ctx ends.
//
// It returns nil when ctx ends. An error wrapping ErrRolledBack means this
// process is the rejected version: the program must exit non-zero (or
// restart itself) so the restored binary runs. ErrExitRequested means a
// Windows SelfRestarter started the new version and this process must exit.
// Other errors are only reported for the trial; the loop logs its own.
func (a *Attachment) Run(ctx context.Context, check func(context.Context) error) error {
	if a == nil {
		return nil
	}
	if a.trial != nil {
		err := a.trial.Confirm(ctx, check)
		switch {
		case ctx.Err() != nil:
			return nil
		case errors.Is(err, ErrRolledBack):
			return err
		case err != nil:
			a.u.o.Logf("selfupdate: confirming the new version: %v", err)
		}
	}
	return a.u.Loop(ctx)
}
