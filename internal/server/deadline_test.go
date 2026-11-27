package server

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

// SEC-05: a client that sends headers and then trickles the body is cut
// off after bodyReadTimeout (shortened here through a real listener).
func TestSlowBodyDeadline(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	rt := &Route{Method: "POST", API: func(*Server, *http.Request) (any, error) { return nil, nil }}
	s := &Server{}
	got := make(chan error, 1)
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r = s.bodyDeadlineFor(w, r, rt, 200*time.Millisecond)
		buf := make([]byte, 64)
		var err error
		for err == nil {
			_, err = r.Body.Read(buf)
		}
		got <- err
	})}
	go srv.Serve(ln)
	defer srv.Shutdown(context.Background())
	c, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	fmt.Fprintf(c, "POST /x HTTP/1.1\r\nHost: x\r\nContent-Length: 1000\r\n\r\nab")
	select {
	case err := <-got:
		if err == nil || !strings.Contains(err.Error(), "timeout") {
			t.Fatalf("slow body: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("slow body was not cut off")
	}
}

// A body read to the end lifts the deadline: the handler may then work
// longer than the read timeout without its context being cancelled.
func TestBodyDeadlineLiftedAfterEOF(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	rt := &Route{Method: "POST", API: func(*Server, *http.Request) (any, error) { return nil, nil }}
	s := &Server{}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r = s.bodyDeadlineFor(w, r, rt, 100*time.Millisecond)
		buf := make([]byte, 64)
		for {
			if _, err := r.Body.Read(buf); err != nil {
				break
			}
		}
		select {
		case <-r.Context().Done():
			w.WriteHeader(500)
		case <-time.After(400 * time.Millisecond):
			w.WriteHeader(200)
		}
	})}
	go srv.Serve(ln)
	defer srv.Shutdown(context.Background())
	resp, err := http.Post("http://"+ln.Addr().String()+"/x", "text/plain", strings.NewReader("hello"))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("handler context cancelled after the body was read: %d", resp.StatusCode)
	}
}
