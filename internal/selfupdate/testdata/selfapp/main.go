// Command selfapp is a stand-in program for the self-confirm integration
// test: it integrates selfupdate the way airview and airview-feeder do
// (Attach at start, Run with a health check), with behaviour fixed at
// build time:
//
//	go build -ldflags "-X main.version=1.1.0 -X main.mode=crash" ./testdata/selfapp
//
// mode: good (healthy), crash (exits right after Attach, every start),
// sick (runs, but its own health check fails at the end of the window).
//
// Usage: selfapp ADDR STATEDIR
//
// Exit codes: 3 crash mode, 75 Attach rolled back (crash loop), 76 Run
// rolled back (failed self check), 2 usage, 5 listen error.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"time"

	"airview/internal/selfupdate"
)

var (
	version = "dev"
	mode    = "good"
)

func main() {
	if len(os.Args) < 3 {
		os.Exit(2)
	}
	addr, state := os.Args[1], os.Args[2]
	exe, err := selfupdate.ExecutablePath()
	if err != nil {
		os.Exit(2)
	}
	att, err := selfupdate.Attach(selfupdate.Options{
		Product: "airview-feeder", CurrentVersion: version, Exe: exe, StateDir: state,
		Health: selfupdate.HealthPolicy{Window: 1500 * time.Millisecond},
	})
	if errors.Is(err, selfupdate.ErrRolledBack) {
		os.Exit(75)
	}
	if mode == "crash" {
		os.Exit(3)
	}
	start := time.Now()
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"version": version, "uptimeS": time.Since(start).Seconds(), "pid": os.Getpid()})
	})
	var ln net.Listener
	for i := 0; i < 50; i++ { // the previous instance may still hold the port
		if ln, err = net.Listen("tcp", addr); err == nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if err != nil {
		os.Exit(5)
	}
	go http.Serve(ln, mux)
	check := func(context.Context) error {
		if mode == "sick" {
			return errors.New("self check: sick build")
		}
		return nil
	}
	if err := att.Run(context.Background(), check); errors.Is(err, selfupdate.ErrRolledBack) {
		os.Exit(76)
	}
	select {}
}
