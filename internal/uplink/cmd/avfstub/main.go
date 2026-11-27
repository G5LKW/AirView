// Command avfstub runs the AVF/1 test server of internal/uplink/avftest as a
// process, for live checks of feeders and of the uplink client without a
// full aggregator. It prints its SPKI pin and, every few seconds, what it
// received. It is a development tool, not an aggregator.
//
//	avfstub -listen 127.0.0.1:30160 [-token TOKEN] [-mode full|selective]
package main

import (
	"flag"
	"fmt"
	"os"
	"os/signal"
	"time"

	"airview/internal/avf"
	"airview/internal/uplink/avftest"
)

func main() {
	listen := flag.String("listen", "127.0.0.1:30160", "listen address")
	name := flag.String("name", "avfstub", "network name sent in HELLO_S")
	token := flag.String("token", "", "enrol token required from new feeders (invite admission)")
	mode := flag.String("mode", avf.ModeFull, "traffic mode assigned in WELCOME (full|selective)")
	every := flag.Duration("every", 5*time.Second, "report interval")
	outAt := flag.Duration("outage-at", 0, "simulate an outage starting this long after start (0 = none)")
	outFor := flag.Duration("outage-for", time.Minute, "length of the simulated outage")
	flag.Parse()

	adm := "open"
	if *token != "" {
		adm = "invite"
	}
	s, err := avftest.New(avftest.Options{Addr: *listen, Name: *name, EnrolToken: *token, Admission: adm, Mode: *mode})
	if err != nil {
		fmt.Fprintln(os.Stderr, "avfstub:", err)
		os.Exit(1)
	}
	defer s.Close()
	fmt.Printf("avfstub listening on %s\nserver pin: %s\nserver id:  %s\n", s.Addr, s.Pin, s.ServerID)

	if *outAt > 0 {
		go func() {
			time.Sleep(*outAt)
			fmt.Println(time.Now().Format("15:04:05"), "outage: refusing and dropping every session")
			s.Refuse(true)
			s.DropAll()
			time.Sleep(*outFor)
			s.Refuse(false)
			fmt.Println(time.Now().Format("15:04:05"), "outage over")
		}()
	}
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt)
	t := time.NewTicker(*every)
	defer t.Stop()
	for {
		select {
		case <-sig:
			report(s)
			return
		case <-t.C:
			report(s)
		}
	}
}

func report(s *avftest.Server) {
	fr := s.Frames()
	late, untimed := 0, 0
	for _, f := range fr {
		if f.Late {
			late++
		}
		if f.TsInvalid {
			untimed++
		}
	}
	counts := map[string]int{}
	for _, r := range s.Records() {
		counts[r.Type.String()]++
	}
	fmt.Printf("%s sessions=%d/%d records=%v frames=%d late=%d untimed=%d dups=%d pongs=%d errors=%d\n",
		time.Now().Format("15:04:05"), s.Sessions(), s.TotalSessions(), counts, len(fr), late, untimed, s.Duplicates(), s.Pongs(), len(s.Errors()))
	for _, e := range s.Errors() {
		fmt.Println("  protocol error:", e)
	}
}
