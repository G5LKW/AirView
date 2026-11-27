package engine

import (
	"sync"
	"time"

	"airview/internal/tracker"
)

// Notice is a message for the UI (the SSE "notice" event): tracker notices
// (emergencies, records, catches), feed up / down, configuration warnings
// and engine messages. JSON: {level, text, t, kind?, icao?}.
type Notice = tracker.Notice

const (
	noticeBacklog = 50 // recent notices kept for late subscribers
	noticeBuffer  = 64 // per-subscriber channel capacity
)

// broker fans notices out to subscribers. A slow subscriber loses notices;
// publish never blocks.
type broker struct {
	mu     sync.Mutex
	subs   map[int]chan Notice
	next   int
	recent []Notice
	closed bool
}

func newBroker() *broker {
	return &broker{subs: make(map[int]chan Notice)}
}

func (b *broker) publish(n Notice) {
	if n.T == 0 {
		n.T = time.Now().UnixMilli()
	}
	if n.Level == "" {
		n.Level = "info"
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return
	}
	if len(b.recent) >= noticeBacklog {
		copy(b.recent, b.recent[1:])
		b.recent = b.recent[:noticeBacklog-1]
	}
	b.recent = append(b.recent, n)
	for _, c := range b.subs {
		select {
		case c <- n:
		default: // subscriber is not keeping up: drop
		}
	}
}

func (b *broker) subscribe() (<-chan Notice, func()) {
	c := make(chan Notice, noticeBuffer)
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		close(c)
		return c, func() {}
	}
	id := b.next
	b.next++
	b.subs[id] = c
	b.mu.Unlock()
	var once sync.Once
	return c, func() {
		once.Do(func() {
			b.mu.Lock()
			if _, ok := b.subs[id]; ok {
				delete(b.subs, id)
				close(c)
			}
			b.mu.Unlock()
		})
	}
}

func (b *broker) backlog() []Notice {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]Notice(nil), b.recent...)
}

func (b *broker) close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return
	}
	b.closed = true
	for id, c := range b.subs {
		delete(b.subs, id)
		close(c)
	}
}

// Notices subscribes to the notice stream. The channel is buffered; notices
// are dropped for a subscriber that does not keep up. Call cancel when done
// (it closes the channel); the channel is also closed when the engine stops.
func (e *Engine) Notices() (ch <-chan Notice, cancel func()) {
	return e.notices.subscribe()
}

// RecentNotices returns the newest notices (oldest first, at most 50), e.g.
// the configuration warnings raised before any client connected.
func (e *Engine) RecentNotices() []Notice {
	return e.notices.backlog()
}

// Notify publishes a notice. level is "info", "warn" or "alert".
func (e *Engine) Notify(level, text string) {
	e.notices.publish(Notice{Level: level, Text: text})
}
