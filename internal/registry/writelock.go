package registry

import (
	"context"
	"sync"
)

// prioLock is the registry's write lock with two priorities: a waiting
// high-priority writer (admission) is served before every waiting normal
// one (connection log, last-seen flush, retention, owner edits). Waiting
// honours the caller's context, so a writer whose deadline passes while
// queued gives up without taking the lock (finding SCALE-09).
type prioLock struct {
	mu       sync.Mutex
	held     bool
	highWait int
	wake     chan struct{} // closed and replaced on every unlock
}

func (l *prioLock) ch() chan struct{} {
	if l.wake == nil {
		l.wake = make(chan struct{})
	}
	return l.wake
}

// lock takes the lock (no deadline).
func (l *prioLock) lock(high bool) { _ = l.lockCtx(context.Background(), high) }

// lockCtx takes the lock or returns ctx.Err() when ctx ends first.
func (l *prioLock) lockCtx(ctx context.Context, high bool) error {
	l.mu.Lock()
	if high {
		l.highWait++
	}
	for l.held || (!high && l.highWait > 0) {
		w := l.ch()
		l.mu.Unlock()
		select {
		case <-w:
		case <-ctx.Done():
			l.mu.Lock()
			if high {
				l.highWait--
				l.broadcast() // normal writers may proceed now
			}
			l.mu.Unlock()
			return ctx.Err()
		}
		l.mu.Lock()
	}
	if high {
		l.highWait--
	}
	l.held = true
	l.mu.Unlock()
	return nil
}

func (l *prioLock) unlock() {
	l.mu.Lock()
	l.held = false
	l.broadcast()
	l.mu.Unlock()
}

// broadcast wakes every waiter (caller holds mu).
func (l *prioLock) broadcast() {
	if l.wake != nil {
		close(l.wake)
		l.wake = nil
	}
}
