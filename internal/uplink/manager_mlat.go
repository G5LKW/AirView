package uplink

import (
	"fmt"
	"sync"

	"airview/internal/model"
)

// ApplyMLAT makes the running MLAT outputs match list (see ApplyBeast for
// the rules). It returns the normalisation notes.
func (m *Manager) ApplyMLAT(list []MLATUplink) []string {
	var notes []string
	want := make([]MLATUplink, 0, len(list))
	seen := map[string]bool{}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil
	}
	for i, u := range list {
		u = u.clone()
		path := fmt.Sprintf("mlat[%d]", i)
		notes = append(notes, u.Normalize(path)...)
		if !model.ValidSlug(u.ID) {
			notes = append(notes, fmt.Sprintf("%s.id %q is not a valid ID; entry skipped", path, u.ID))
			continue
		}
		if err := u.Validate(); err != nil {
			notes = append(notes, fmt.Sprintf("%s: %v; entry skipped", path, err))
			continue
		}
		if seen[u.ID] || m.byID[u.ID] != nil || m.beastByID[u.ID] != nil {
			notes = append(notes, fmt.Sprintf("%s.id %q is already used by another uplink; entry skipped", path, u.ID))
			continue
		}
		seen[u.ID] = true
		want = append(want, u)
	}
	for _, n := range notes {
		m.o.Logf("uplink: %s", n)
	}
	byWant := map[string]MLATUplink{}
	for _, u := range want {
		byWant[u.ID] = u
	}
	var stop []*mlatClient
	for id, c := range m.mlatByID {
		u, ok := byWant[id]
		c.mu.Lock()
		old := c.cfg
		c.mu.Unlock()
		if !ok || u.connKey() != old.connKey() {
			stop = append(stop, c)
			delete(m.mlatByID, id)
		}
	}
	var wg sync.WaitGroup
	for _, c := range stop {
		wg.Add(1)
		go func(c *mlatClient) {
			defer wg.Done()
			c.close()
		}(c)
	}
	wg.Wait()
	out := make([]*mlatClient, 0, len(want))
	for _, u := range want {
		c := m.mlatByID[u.ID]
		if c != nil {
			c.update(u)
		} else {
			c = newMLATClient(&m.o, u)
			m.mlatByID[u.ID] = c
			c.start()
		}
		out = append(out, c)
	}
	m.mlats.Store(&out)
	return notes
}

// MLATStatus returns the status of every MLAT output in configuration order.
func (m *Manager) MLATStatus() []MLATStatus {
	list := *m.mlats.Load()
	if len(list) == 0 {
		return nil
	}
	out := make([]MLATStatus, 0, len(list))
	for _, c := range list {
		out = append(out, c.Status())
	}
	return out
}
