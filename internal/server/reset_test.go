package server

import (
	"bufio"
	"fmt"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

// A factory reset must not wait for an aircraft database upload whose body
// arrives slowly: the upload holds the reset read lock while it parses the
// body, so it is aborted (context cancelled, read deadline moved to now).
func TestFactoryResetDuringSlowUpload(t *testing.T) {
	r := newRig(t, rigOptions{listen: true})
	addr := fmt.Sprintf("127.0.0.1:%d", r.srv.Port())

	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	const size = 50 << 20
	head := "POST /api/db/acdb/upload?name=slow.csv HTTP/1.1\r\nHost: " + addr +
		"\r\nContent-Type: text/csv\r\nContent-Length: " + fmt.Sprint(size) + "\r\n\r\n" +
		strings.Repeat("3C6444;D-AIBD;A319;00;AIRBUS A-319;;;\n", 200)
	if _, err := conn.Write([]byte(head)); err != nil {
		t.Fatal(err)
	}
	// Keep trickling a line now and then: the handler is reading the body.
	stopTrickle := make(chan struct{})
	defer close(stopTrickle)
	go func() {
		tk := time.NewTicker(200 * time.Millisecond)
		defer tk.Stop()
		for {
			select {
			case <-stopTrickle:
				return
			case <-tk.C:
				if _, err := conn.Write([]byte("3C6445;D-AIBE;A319;00;AIRBUS A-319;;;\n")); err != nil {
					return
				}
			}
		}
	}()
	// Wait until the import is really running (the handler holds the lock).
	for deadline := time.Now().Add(10 * time.Second); !r.eng.ImportStatus().Running; {
		if time.Now().After(deadline) {
			t.Fatalf("upload import did not start: %+v", r.eng.ImportStatus())
		}
		time.Sleep(20 * time.Millisecond)
	}

	start := time.Now()
	done := make(chan *http.Response, 1)
	go func() {
		rec := r.do("POST", "/api/factory-reset", loopbackAddr, `{"confirm":"RESET"}`)
		done <- rec.Result()
	}()
	select {
	case res := <-done:
		if res.StatusCode != 200 {
			t.Fatalf("factory reset: %d", res.StatusCode)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("factory reset waited for the slow upload")
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Errorf("factory reset took %v", d)
	}
	// The upload got an error (or its connection was closed), it did not
	// complete.
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if res, err := http.ReadResponse(bufio.NewReader(conn), nil); err == nil {
		if res.StatusCode == 200 {
			t.Errorf("aborted upload answered 200")
		}
		res.Body.Close()
	} else if !strings.Contains(err.Error(), "EOF") && !strings.Contains(err.Error(), "reset") && !strings.Contains(err.Error(), "closed") {
		t.Logf("upload connection: %v", err)
	}
	if st := r.eng.ImportStatus(); st.Running {
		t.Errorf("import still running after the reset: %+v", st)
	}
}
