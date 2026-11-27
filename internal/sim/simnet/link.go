package simnet

import (
	"io"
	"math"
	"math/rand/v2"
	"sync"
)

// ByteSink receives the bytes an emitter produces, stamped with the
// feeder host time (Unix ns) at which they were written. p is only valid
// during the call.
type ByteSink interface {
	Write(hostNs int64, p []byte)
}

// Flusher is implemented by byte sinks that hold data back (Link); emitters
// call Flush from their Advance with the host time up to which everything
// has been written.
type Flusher interface {
	Flush(untilHostNs int64)
}

// ConnState is implemented by byte sinks that model a connection (Link).
// Up reports whether the connection is up at host time hostNs and the
// session number, which changes on every reconnect, so emitters resend their
// handshake / RECEIVER records and start backfill.
type ConnState interface {
	Up(hostNs int64) (up bool, session int)
}

// WriterSink adapts an io.Writer (bytes.Buffer, net.Conn, file) to a
// ByteSink; host times are ignored. Write errors are kept in Err.
type WriterSink struct {
	W   io.Writer
	Err error
}

// Write implements ByteSink.
func (w *WriterSink) Write(_ int64, p []byte) {
	if w.Err == nil {
		_, w.Err = w.W.Write(p)
	}
}

// LockedSink serialises writes of several emitters (receivers are evaluated
// concurrently) into one ByteSink.
type LockedSink struct {
	mu   sync.Mutex
	Sink ByteSink
}

// Write implements ByteSink.
func (l *LockedSink) Write(h int64, p []byte) {
	l.mu.Lock()
	l.Sink.Write(h, p)
	l.mu.Unlock()
}

// LinkSpec models the network path from a feeder to the aggregator.
type LinkSpec struct {
	// LatencyMs is the base one-way latency (0 = 20 ms), JitterMs the mean
	// of an exponential extra delay (0 = none). TCP keeps the byte order,
	// so arrivals never overtake each other.
	LatencyMs float64
	JitterMs  float64
	// Outages are scheduled disconnections (seconds since Start, compared
	// against host time); OutagesPerHour adds random ones of mean length
	// OutageMeanS (0 = 30 s).
	Outages        []Interval
	OutagesPerHour float64
	OutageMeanS    float64
}

func (l *LinkSpec) normalize() {
	if !(l.LatencyMs > 0) {
		l.LatencyMs = 20
	}
	if l.JitterMs < 0 {
		l.JitterMs = 0
	}
	if !(l.OutageMeanS > 0) {
		l.OutageMeanS = 30
	}
}

// Delivery is a chunk of bytes as it reaches the server.
type Delivery struct {
	ArrivalNs int64 // server receive time (Unix ns)
	HostNs    int64 // feeder host time it was written
	Session   int
	Data      []byte
}

// Link is a ByteSink that models the feeder's TCP connection: latency,
// jitter and disconnections. While the link is down, written bytes are lost
// (the emitter sees Up() == false and may buffer, like the AVF spool). The
// deliveries are handed to Deliver in arrival order when Flush passes their
// arrival time (emitters call Flush from Advance).
type Link struct {
	Spec    LinkSpec
	Deliver func(d Delivery)

	startNs int64
	rng     *rand.Rand
	queue   []Delivery
	lastArr int64
	session int
	wasUp   bool
	outages []Interval
	randEnd float64 // random outages generated up to here (s)
	Lost    uint64  // bytes written while down
	Sent    uint64
	inited  bool
}

// NewLink creates a link for receiver rx of network n with that receiver's
// LinkSpec; deliver receives the bytes at the server.
func (n *Network) NewLink(rx int, deliver func(Delivery)) *Link {
	return &Link{
		Spec:    n.cfg.Receivers[rx].Link,
		Deliver: deliver,
		startNs: n.startNs,
		rng:     newRand(n.cfg.Seed, streamLink, uint64(rx)),
	}
}

func (l *Link) ensureOutages(t float64) {
	if !l.inited {
		l.inited = true
		l.outages = append(l.outages, l.Spec.Outages...)
		l.wasUp = true
	}
	for l.Spec.OutagesPerHour > 0 && l.randEnd <= t+60 {
		start := l.randEnd + expRand(l.rng, 3600/l.Spec.OutagesPerHour)
		end := start + expRand(l.rng, l.Spec.OutageMeanS)
		l.outages = append(l.outages, Interval{start, end})
		l.randEnd = end
	}
}

// Up implements ConnState.
func (l *Link) Up(hostNs int64) (bool, int) {
	t := float64(hostNs-l.startNs) / 1e9
	l.ensureOutages(t)
	up := true
	for _, iv := range l.outages {
		if iv.contains(t) {
			up = false
			break
		}
	}
	if up && !l.wasUp {
		l.session++
	}
	l.wasUp = up
	return up, l.session
}

// Write implements ByteSink.
func (l *Link) Write(hostNs int64, p []byte) {
	up, sess := l.Up(hostNs)
	if !up {
		l.Lost += uint64(len(p))
		return
	}
	arr := hostNs + int64(l.Spec.LatencyMs*1e6)
	if l.Spec.JitterMs > 0 {
		arr += int64(expRand(l.rng, l.Spec.JitterMs*1e6))
	}
	if arr < l.lastArr {
		arr = l.lastArr
	}
	l.lastArr = arr
	l.Sent += uint64(len(p))
	l.queue = append(l.queue, Delivery{ArrivalNs: arr, HostNs: hostNs, Session: sess, Data: append([]byte(nil), p...)})
}

// Flush implements Flusher: deliveries that arrive by untilHostNs plus the
// base latency are handed over.
func (l *Link) Flush(untilHostNs int64) {
	lim := untilHostNs + int64(math.Max(l.Spec.LatencyMs, 0)*1e6)
	i := 0
	for i < len(l.queue) && l.queue[i].ArrivalNs <= lim {
		if l.Deliver != nil {
			l.Deliver(l.queue[i])
		}
		i++
	}
	l.queue = append(l.queue[:0], l.queue[i:]...)
}

// Pending returns the number of deliveries not yet handed over.
func (l *Link) Pending() int { return len(l.queue) }
