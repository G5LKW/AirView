package mlatc

import (
	"context"
	"crypto/rand"
	"sync"

	"airview/internal/model"
)

// MemRegistry is an in-memory Registry: every identity gets a station on
// first sight and keeps it for the life of the process. It is the
// reference behaviour for tests and tools. Admission is Admission for every
// identity unless Decide is set.
type MemRegistry struct {
	Admission string
	// Decide, when set, overrides Admission; an error refuses the client.
	Decide func(id Identity) (string, error)

	mu       sync.Mutex
	next     model.StationIdx
	byKey    map[string]*memStation
	byIdx    map[model.StationIdx]*memStation
	attached int
}

type memStation struct {
	idx   model.StationIdx
	feed  string
	last  Identity
	links int
}

// NewMemRegistry returns an empty registry; station indexes start at 2 (1
// is the aggregator's host station).
func NewMemRegistry() *MemRegistry {
	return &MemRegistry{next: model.HostStation + 1, byKey: map[string]*memStation{}, byIdx: map[model.StationIdx]*memStation{}}
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
	st := r.byKey[id.Key()]
	if st == nil {
		sid, err := model.NewSID(rand.Reader)
		if err != nil {
			return Attachment{}, err
		}
		st = &memStation{idx: r.next, feed: model.StationFeedID(sid, 0)}
		r.next++
		r.byKey[id.Key()] = st
		r.byIdx[st.idx] = st
	}
	st.last = id
	a := Attachment{Station: st.idx, FeedID: st.feed, Admission: adm}
	if a.Admitted() {
		st.links++
		r.attached++
	} else if adm == model.AdmDenied || adm == model.AdmRevoked {
		a.Reason = "station " + adm
	}
	return a, nil
}

// Detach implements Registry.
func (r *MemRegistry) Detach(id Identity, a Attachment) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if st := r.byIdx[a.Station]; st != nil && st.links > 0 {
		st.links--
		r.attached--
	}
}

// Attached returns the number of live attachments.
func (r *MemRegistry) Attached() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.attached
}

// Stations returns the number of known stations.
func (r *MemRegistry) Stations() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.byKey)
}

// Lookup returns the station of an identity key and the identity it last
// attached with.
func (r *MemRegistry) Lookup(key string) (model.StationIdx, Identity, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if st := r.byKey[key]; st != nil {
		return st.idx, st.last, true
	}
	return 0, Identity{}, false
}
