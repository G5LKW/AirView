package wininstall

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestExitCode(t *testing.T) {
	cases := []struct {
		res  *Result
		err  error
		want int
	}{
		{&Result{}, nil, ExitOK},
		{&Result{RebootRequired: true}, nil, ExitRebootRequired},
		{&Result{StepErrors: []string{"service"}, RebootRequired: true}, nil, ExitFailed},
		{nil, context.Canceled, ExitCancelled},
		{nil, ErrNeedsAdmin, ExitNeedsAdmin},
		{nil, errors.New("disk full"), ExitFailed},
	}
	for i, c := range cases {
		if got := ExitCode(c.res, c.err); got != c.want {
			t.Errorf("case %d: %d, want %d", i, got, c.want)
		}
	}
}

func TestRemovalText(t *testing.T) {
	got := RemovalText(&Receipt{Scope: "machine", Service: true, ServiceName: "AirViewFeeder", TrayRunValue: "x", Firewall: "rule"})
	want := "The program files, the shortcuts, the uninstall entry, the Windows service AirViewFeeder, the tray autostart and the firewall rule are removed."
	if got != want {
		t.Errorf("got %q", got)
	}
	if got := RemovalText(&Receipt{Scope: "user", Firewall: "rule"}); !strings.HasPrefix(got, "The program files, the shortcuts and the uninstall entry are removed") {
		t.Errorf("got %q", got)
	}
}
