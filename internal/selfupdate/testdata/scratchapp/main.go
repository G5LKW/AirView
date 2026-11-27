// Command scratchapp is a stand-in program for the selfupdate integration
// test. Its behaviour is fixed at build time:
//
//	go build -ldflags "-X main.version=1.0.0 -X main.mode=good" ./testdata/scratchapp
//
// mode: good (serves /health), crash (exits at once), flaky (healthy for
// 800 ms, then exits), sick (answers 503).
//
// Usage: scratchapp ADDR
package main

import (
	"encoding/json"
	"net/http"
	"os"
	"time"
)

var (
	version = "dev"
	mode    = "good"
)

func main() {
	if len(os.Args) < 2 {
		os.Exit(2)
	}
	start := time.Now()
	switch mode {
	case "crash":
		os.Exit(3)
	case "flaky":
		time.AfterFunc(800*time.Millisecond, func() { os.Exit(4) })
	}
	http.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		code := http.StatusOK
		if mode == "sick" {
			code = http.StatusServiceUnavailable
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		json.NewEncoder(w).Encode(map[string]any{"version": version, "uptimeS": time.Since(start).Seconds(), "pid": os.Getpid()})
	})
	if err := http.ListenAndServe(os.Args[1], nil); err != nil {
		os.Exit(5)
	}
}
