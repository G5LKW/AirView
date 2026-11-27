package server

import (
	"context"
	"net/http"
	"sync"
	"time"
)

// exportMaxDuration bounds one streamed export, so a client reading very
// slowly cannot keep an export slot (and the reset read lock) for ever.
const exportMaxDuration = 20 * time.Minute

// streamSet holds the abort functions of the exports that are streaming
// under resetMu.RLock. A factory reset aborts them before it takes the write
// lock: otherwise a slow reader would hold the read lock, and the waiting
// writer would block every new API request until that reader finished.
type streamSet struct {
	mu   sync.Mutex
	next int
	m    map[int]func()
}

func (ss *streamSet) add(abort func()) int {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	if ss.m == nil {
		ss.m = map[int]func(){}
	}
	ss.next++
	ss.m[ss.next] = abort
	return ss.next
}

func (ss *streamSet) remove(id int) {
	ss.mu.Lock()
	delete(ss.m, id)
	ss.mu.Unlock()
}

// abortAll runs every abort function. They run under mu so that remove (which
// the handler calls before it returns) cannot race with an abort that would
// otherwise touch a connection already serving the next request.
func (ss *streamSet) abortAll() int {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	for _, f := range ss.m {
		f()
	}
	return len(ss.m)
}

// beginStream registers a streaming response that holds resetMu.RLock. It
// returns the request with a context that is cancelled on abort or after
// exportMaxDuration, and the function that unregisters it. Aborting also
// moves the connection's write deadline to now, so a write blocked on a slow
// client fails at once.
func (s *Server) beginStream(w http.ResponseWriter, r *http.Request) (*http.Request, func()) {
	return s.beginAbortable(w, r, exportMaxDuration)
}

// beginAbortable registers any long request that holds resetMu.RLock (an
// upload being read and imported, a terrain computation) the same way: an
// abort cancels the request context and moves both connection deadlines to
// now, so a body read blocked on a slow sender and a write blocked on a slow
// reader fail at once. maxDur > 0 also bounds the request.
func (s *Server) beginAbortable(w http.ResponseWriter, r *http.Request, maxDur time.Duration) (*http.Request, func()) {
	var ctx context.Context
	var cancel context.CancelFunc
	if maxDur > 0 {
		ctx, cancel = context.WithTimeout(r.Context(), maxDur)
	} else {
		ctx, cancel = context.WithCancel(r.Context())
	}
	rc := http.NewResponseController(w)
	id := s.streams.add(func() {
		cancel()
		now := time.Now()
		_ = rc.SetReadDeadline(now)
		_ = rc.SetWriteDeadline(now)
	})
	return r.WithContext(ctx), func() {
		s.streams.remove(id)
		cancel()
	}
}

// lockExclusive takes resetMu for writing, aborting streaming exports (also
// ones that start while it waits) so that it does not wait on slow clients.
func (s *Server) lockExclusive() {
	stop := make(chan struct{})
	go func() {
		t := time.NewTicker(50 * time.Millisecond)
		defer t.Stop()
		for {
			s.streams.abortAll()
			select {
			case <-stop:
				return
			case <-t.C:
			}
		}
	}()
	s.resetMu.Lock()
	close(stop)
}
