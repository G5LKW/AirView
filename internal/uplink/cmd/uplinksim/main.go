// Command uplinksim feeds simulated receivers (internal/sim/simnet) through
// the uplink client (internal/uplink) to an AVF/1 server in real time, and
// serves the uplink status on a local HTTP port. It is a development tool
// for live checks of the uplink against avfstub or an aggregator.
//
//	uplinksim -server 127.0.0.1:30160 -pin sha256/... [-state DIR -spool 64] [-status 127.0.0.1:8099]
//	          [-beast 127.0.0.1:30004 -beast-format beast_reduce_plus_out -beast-uuid UUID]
//
// -server "" runs the Beast output ("also feed others") alone.
package main

import (
	"context"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"time"

	"airview/internal/model"
	"airview/internal/sim/simnet"
	"airview/internal/uplink"
)

type rxFeed struct {
	n  *simnet.Network
	m  *uplink.Manager
	id string
}

func (r *rxFeed) Observe(o *simnet.Observation) {
	f := r.n.RawFrame(o)
	f.FeedID, f.Station = r.id, 0
	r.m.HandleFrame(f)
}
func (r *rxFeed) Advance(float64) {}

func main() {
	server := flag.String("server", "127.0.0.1:30160", "AVF server host:port")
	pin := flag.String("pin", "", "server SPKI pin (sha256/...); empty = system roots")
	token := flag.String("token", "", "enrol token")
	state := flag.String("state", "", "state directory (identity, spool); empty = memory")
	spool := flag.Int("spool", 0, "spool size in MiB (needs -state)")
	nrx := flag.Int("receivers", 1, "simulated receivers")
	aircraft := flag.Int("aircraft", 100, "simulated aircraft")
	device := flag.String("device", string(simnet.DeviceBeast), "receiver device model")
	dur := flag.Duration("duration", 0, "stop after this long (0 = until interrupted)")
	status := flag.String("status", "127.0.0.1:0", "status HTTP listen address (empty = none)")
	beast := flag.String("beast", "", "also feed a Beast aggregator at host:port (empty = none)")
	beastFormat := flag.String("beast-format", uplink.BeastReducePlus, "Beast output format (beast_reduce_plus_out|beast_reduce_out|beast_out)")
	beastUUID := flag.String("beast-uuid", "", "UUID sent to the Beast aggregator (empty = generated per uplink)")
	flag.Parse()
	m, err := uplink.NewManager(uplink.Options{Software: "uplinksim", StateDir: *state,
		Logf: func(f string, a ...any) { fmt.Printf(time.Now().Format("15:04:05 ")+f+"\n", a...) },
		Source: uplink.SourceFuncs{ReceiverFn: func(id string) uplink.ReceiverInfo {
			n, _ := strconv.Atoi(id[2:])
			return uplink.ReceiverInfo{RxNo: n + 1, Name: id, Decoder: "simnet " + *device, Connected: true, InputType: "beast"}
		}}})
	if err != nil {
		fail(err)
	}
	if *server != "" {
		host, portS, err := net.SplitHostPort(*server)
		if err != nil {
			fail(err)
		}
		port, _ := strconv.Atoi(portS)
		u := model.DefaultUplink()
		u.ID, u.Host, u.Port, u.ServerPin, u.EnrolToken, u.SpoolMB = "sim", host, port, *pin, *token, *spool
		m.Apply([]model.Uplink{u})
	}
	if *beast != "" {
		host, portS, err := net.SplitHostPort(*beast)
		if err != nil {
			fail(err)
		}
		port, _ := strconv.Atoi(portS)
		for _, n := range m.ApplyBeast([]uplink.BeastUplink{{ID: "beast", Host: host, Port: port, Format: *beastFormat, UUID: *beastUUID}}) {
			fmt.Println("note:", n)
		}
	}
	defer m.Close()

	var specs []simnet.ReceiverSpec
	for i := 0; i < *nrx; i++ {
		specs = append(specs, simnet.ReceiverSpec{Lat: 50 + 0.2*float64(i), Lon: 8 + 0.3*float64(i), GroundHAE: 150,
			AntennaAGL: 10, Clock: simnet.ClockSpec{Device: simnet.Device(*device)}})
	}
	n, err := simnet.New(simnet.Config{Seed: uint64(time.Now().UnixNano()), Start: time.Now(), Aircraft: *aircraft, Receivers: specs})
	if err != nil {
		fail(err)
	}
	for i := range specs {
		n.Attach(i, &rxFeed{n: n, m: m, id: fmt.Sprintf("rx%d", i)})
	}
	if *status != "" {
		ln, err := net.Listen("tcp", *status)
		if err != nil {
			fail(err)
		}
		fmt.Println("status: http://" + ln.Addr().String() + "/")
		go func() { _ = http.Serve(ln, m) }()
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	if *dur > 0 {
		ctx, cancel = context.WithTimeout(ctx, *dur)
		defer cancel()
	}
	start := time.Now()
	sim := time.Duration(0)
	for ctx.Err() == nil {
		if err := n.Advance(ctx, 100*time.Millisecond); err != nil {
			break
		}
		sim += 100 * time.Millisecond
		if d := time.Until(start.Add(sim)); d > 0 {
			select {
			case <-ctx.Done():
			case <-time.After(d):
			}
		}
	}
	m.Flush()
	for _, s := range m.Status() {
		fmt.Printf("uplink %s: state=%s frames in=%d sent=%d late=%d mlatRefused=%d records=%d dropped=%d ring=%d spool=%dB\n",
			s.ID, s.State, s.Counters.FramesIn, s.Counters.FramesSent, s.Counters.FramesLate, s.Counters.MLATRefused,
			s.Counters.RecordsSent, s.Counters.DroppedRecords, s.Buffer.RingRecords, s.Buffer.SpoolBytes)
	}
	for _, s := range m.BeastStatus() {
		fmt.Printf("beast %s: state=%s format=%s frames in=%d sent=%d reduced=%d dropped=%d mlatRefused=%d badCrc=%d bytes=%d pingsEchoed=%d slowDowns=%d connects=%d\n",
			s.ID, s.State, s.Format, s.FramesIn, s.FramesSent, s.Reduced, s.Dropped, s.MLATRefused, s.BadCRC, s.BytesSent,
			s.PingsEchoed, s.SlowDowns, s.Connects)
	}
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "uplinksim:", err)
	os.Exit(1)
}
