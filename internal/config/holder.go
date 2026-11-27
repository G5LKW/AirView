package config

import (
	"reflect"
	"slices"
	"sync"

	"airview/internal/model"
)

// Clone returns a deep copy of cfg (the antenna, feed, range-ring and uplink
// slices and the aggregator lists are copied, so the result shares no memory
// with cfg).
func Clone(cfg model.Config) model.Config {
	out := cfg
	out.Antennas = slices.Clone(cfg.Antennas)
	out.Feeds = slices.Clone(cfg.Feeds)
	out.UI.RangeRingsKm = slices.Clone(cfg.UI.RangeRingsKm)
	out.Uplinks = model.CloneUplinks(cfg.Uplinks)
	out.Aggregator = cfg.Aggregator.Clone()
	out.Update = cfg.Update.Clone()
	out.SSR = cfg.SSR.Clone()
	return out
}

// Equal reports whether two configs are identical in every setting.
func Equal(a, b model.Config) bool { return reflect.DeepEqual(a, b) }

// Holder is a thread-safe container for the live configuration. Components
// read it with Get and react to changes through Subscribe.
type Holder struct {
	mu  sync.RWMutex // guards cfg, subs, nextID
	cfg model.Config

	setMu  sync.Mutex // serialises Set/Update including their callbacks
	subs   []subscriber
	nextID int
}

type subscriber struct {
	id int
	fn func(old, cur model.Config)
}

// NewHolder returns a Holder initialised with a copy of cfg.
func NewHolder(cfg model.Config) *Holder {
	return &Holder{cfg: Clone(cfg)}
}

// Get returns a deep copy of the current configuration; the caller may
// modify it freely.
func (h *Holder) Get() model.Config {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return Clone(h.cfg)
}

// Set replaces the configuration and then calls every subscriber, in
// subscription order, on the calling goroutine with private copies of the
// old and the new config. Concurrent Set calls are serialised, so
// subscribers observe changes one at a time and in order. Subscribers are
// called even when nothing changed (compare with Equal if that matters).
//
// A subscriber may call Get, Subscribe or an unsubscribe function, but must
// not call Set or Update on the same Holder (deadlock); hand such work to
// another goroutine.
func (h *Holder) Set(cfg model.Config) {
	h.setMu.Lock()
	defer h.setMu.Unlock()
	h.set(cfg)
}

// Update applies fn to a copy of the current config and stores the result,
// atomically with respect to other Set/Update calls. It returns the stored
// config. Subscribers are notified as for Set.
func (h *Holder) Update(fn func(cfg *model.Config)) model.Config {
	h.setMu.Lock()
	defer h.setMu.Unlock()
	cfg := h.Get()
	fn(&cfg)
	h.set(cfg)
	return Clone(cfg)
}

// set stores cfg and notifies; setMu must be held.
func (h *Holder) set(cfg model.Config) {
	next := Clone(cfg)
	h.mu.Lock()
	old := h.cfg
	h.cfg = next
	subs := slices.Clone(h.subs)
	h.mu.Unlock()

	for _, s := range subs {
		s.fn(Clone(old), Clone(next))
	}
}

// Subscribe registers fn to be called after every Set/Update with the
// previous and the new configuration. The returned function removes the
// subscription; it is safe to call more than once.
func (h *Holder) Subscribe(fn func(old, cur model.Config)) (unsubscribe func()) {
	h.mu.Lock()
	h.nextID++
	id := h.nextID
	h.subs = append(h.subs, subscriber{id: id, fn: fn})
	h.mu.Unlock()

	return func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		h.subs = slices.DeleteFunc(h.subs, func(s subscriber) bool { return s.id == id })
	}
}
