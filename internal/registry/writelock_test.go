package registry

import (
	"context"
	"sync"
	"testing"
	"time"
)

// SCALE-09: an admission (high priority) waiting for the write lock is
// served before every queued bulk write, and a queued writer whose context
// ends gives up without taking the lock.
func TestPrioLockAdmissionFirst(t *testing.T) {
	var l prioLock
	l.lock(false)
	var mu sync.Mutex
	var order []string
	var wg sync.WaitGroup
	start := func(name string, high bool) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			l.lock(high)
			mu.Lock()
			order = append(order, name)
			mu.Unlock()
			time.Sleep(time.Millisecond)
			l.unlock()
		}()
	}
	for i := 0; i < 20; i++ {
		start("bulk", false)
	}
	time.Sleep(20 * time.Millisecond) // the bulk writers queue first
	start("admit", true)
	time.Sleep(20 * time.Millisecond)
	l.unlock()
	wg.Wait()
	if len(order) != 21 || order[0] != "admit" {
		t.Fatalf("order %v: the admission was not served first", order)
	}
}

func TestPrioLockContext(t *testing.T) {
	var l prioLock
	l.lock(false)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := l.lockCtx(ctx, true); err == nil {
		t.Fatal("lock taken while held")
	}
	// the abandoned high-priority wait must not block normal writers
	done := make(chan struct{})
	go func() { l.lock(false); l.unlock(); close(done) }()
	l.unlock()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("normal writer blocked after an abandoned admission wait")
	}
}
