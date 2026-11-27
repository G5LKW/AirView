package main

import (
	"bytes"
	"context"
	"io"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"airview/internal/auth"
	"airview/internal/engine"
	"airview/internal/model"
)

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

var secretRE = regexp.MustCompile(`secret: ([A-Z2-7]+)`)

// runAdminScripted runs "airview admin <sub>" with a scripted console: the
// password twice, then the TOTP code computed from the printed secret.
func runAdminScripted(t *testing.T, dir, sub, pw string) (int, string, string) {
	t.Helper()
	pr, pwr := io.Pipe()
	var out, errb syncBuf
	go func() {
		io.WriteString(pwr, pw+"\n"+pw+"\n")
		deadline := time.Now().Add(20 * time.Second)
		for time.Now().Before(deadline) {
			if m := secretRE.FindStringSubmatch(out.String()); m != nil {
				sec, err := auth.ParseTOTPSecret(m[1])
				if err == nil {
					io.WriteString(pwr, sec.Code(time.Now())+"\n")
				}
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		pwr.Close()
	}()
	code := runAdmin([]string{sub, "-data", dir, "-user", "root"}, pr, &out, &errb)
	pr.Close()
	return code, out.String(), errb.String()
}

func TestAdminInitAndReset(t *testing.T) {
	t.Setenv("AIRVIEW_CONFIG", "")
	t.Setenv("AIRVIEW_DATA", "")
	dir := t.TempDir()
	code, out, errs := runAdminScripted(t, dir, "init", "correct-horse-battery-1")
	if code != 0 || !strings.Contains(out, "Recovery codes") {
		t.Fatalf("init: code %d\nout: %s\nerr: %s", code, out, errs)
	}
	if strings.Contains(out, "correct-horse") {
		t.Fatal("password echoed to stdout")
	}
	// A second init is refused; reset works.
	if code, _, errs := runAdminScripted(t, dir, "init", "correct-horse-battery-2"); code == 0 || !strings.Contains(errs, "already set up") {
		t.Fatalf("second init: %d %s", code, errs)
	}
	if code, out, errs := runAdminScripted(t, dir, "reset", "correct-horse-battery-3"); code != 0 {
		t.Fatalf("reset: %d\n%s\n%s", code, out, errs)
	}
	// The account is persistent: a new registry handle sees a ready admin.
	reg, err := engine.OpenRegistry(dir, model.DefaultAggregator(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer reg.Close()
	svc := auth.NewService(reg.Accounts(), reg.Sessions(), engine.AccountOptions(model.DefaultAggregator()))
	if ok, err := svc.AdminReady(context.Background()); !ok || err != nil {
		t.Fatalf("admin not ready after reset: %v %v", ok, err)
	}
}

func TestAdminUsage(t *testing.T) {
	var out, errb bytes.Buffer
	if code := runAdmin(nil, strings.NewReader(""), &out, &errb); code != 2 || !strings.Contains(errb.String(), "Usage") {
		t.Fatalf("usage: %d %q", code, errb.String())
	}
}
