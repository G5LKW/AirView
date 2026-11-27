package beastin

import (
	"context"
	"crypto/rand"
	"fmt"
	"sync"

	"airview/internal/model"
)

// MemRegistry is an in-memory Registry: every identity gets a station on
// first sight and keeps it for the life of the process. It is the reference
// behaviour for tests, the load tool and a first wiring of the engine before
// internal/registry exists. Admission is Admission for every identity unless
// Decide is set.
type MemRegistry struct {
	// Admission for new identities (default model.AdmApproved).
	Admission string
	// Decide, when set, overrides Admission per identity. Returning a
	// non-nil error refuses the link.
	Decide func(id Identity) (string, error)

	mu       sync.Mutex
	next     model.StationIdx
	byKey    map[string]*memStation
	attached int
}

type memStation struct {
	idx   model.StationIdx
	sid   string
	feed  string
	links int
}

// NewMemRegistry returns an empty registry; station indexes start at 2
// (1 is the aggregator's host station).
func NewMemRegistry() *MemRegistry {
	return &MemRegistry{next: model.HostStation + 1, byKey: map[string]*memStation{}}
}

// Attach implements Registry.
func (r *MemRegistry) Attach(_ context.Context, id Identity) (Attachment, error) {
	adm := r.Admission
	if adm == "" {
		adm = model.AdmApproved
	}
	if r.Decide != nil {
		a, err := r.Decide(id)
		if err != nil {
			return Attachment{}, err
		}
		adm = a
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.byKey == nil {
		r.byKey = map[string]*memStation{}
		if r.next == 0 {
			r.next = model.HostStation + 1
		}
	}
	key := id.Key()
	st := r.byKey[key]
	if st == nil {
		sid, err := model.NewSID(rand.Reader)
		if err != nil {
			return Attachment{}, err
		}
		rx := uint64(0)
		if id.Relay {
			rx = id.ReceiverID
		}
		st = &memStation{idx: r.next, sid: sid, feed: fmt.Sprintf("st/%s/%d", sid, rx)}
		r.next++
		r.byKey[key] = st
	}
	if adm == model.AdmApproved || adm == model.AdmPending {
		st.links++
		r.attached++
	}
	return Attachment{Station: st.idx, FeedID: st.feed, Admission: adm}, nil
}

// Detach implements Registry.
func (r *MemRegistry) Detach(id Identity, a Attachment) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if st := r.byKey[id.Key()]; st != nil && st.links > 0 {
		st.links--
		r.attached--
	}
}

// Stations returns the number of known stations.
func (r *MemRegistry) Stations() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.byKey)
}

// Attached returns the number of live attachments.
func (r *MemRegistry) Attached() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.attached
}

// Lookup returns the station of an identity key (Identity.Key).
func (r *MemRegistry) Lookup(key string) (model.StationIdx, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if st := r.byKey[key]; st != nil {
		return st.idx, true
	}
	return 0, false
}
