package uplink

import (
	"fmt"
	"sync"

	"airview/internal/model"
)

// ApplyBeast makes the running Beast outputs ("also feed others") match
// list: new ones start, removed ones stop, changed connection settings
// (host, port, format, uuid, disabled) reconnect, other changes (inputs,
// reduce interval, timestamps) apply in place. Entries are normalised
// (BeastUplink.Normalize); invalid ones and IDs already used by an AVF/1
// uplink are skipped with a log line. It returns the notes of the
// normalisation (for a config check).
func (m *Manager) ApplyBeast(list []BeastUplink) []string {
	var notes []string
	want := make([]BeastUplink, 0, len(list))
	seen := map[string]bool{}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil
	}
	for i, b := range list {
		b = b.clone()
		path := fmt.Sprintf("beast[%d]", i)
		notes = append(notes, b.Normalize(path)...)
		if !model.ValidSlug(b.ID) {
			notes = append(notes, fmt.Sprintf("%s.id %q is not a valid ID; entry skipped", path, b.ID))
			continue
		}
		if err := b.Validate(); err != nil {
			notes = append(notes, fmt.Sprintf("%s: %v; entry skipped", path, err))
			continue
		}
		if seen[b.ID] || m.byID[b.ID] != nil {
			notes = append(notes, fmt.Sprintf("%s.id %q is already used by another uplink; entry skipped", path, b.ID))
			continue
		}
		seen[b.ID] = true
		want = append(want, b)
	}
	for _, n := range notes {
		m.o.Logf("uplink: %s", n)
	}
	byWant := map[string]BeastUplink{}
	for _, b := range want {
		byWant[b.ID] = b
	}
	var stop []*beastClient
	for id, c := range m.beastByID {
		b, ok := byWant[id]
		c.mu.Lock()
		old := c.cfg
		c.mu.Unlock()
		if !ok || b.connKey() != old.connKey() {
			stop = append(stop, c)
			delete(m.beastByID, id)
		}
	}
	var wg sync.WaitGroup
	for _, c := range stop {
		wg.Add(1)
		go func(c *beastClient) {
			defer wg.Done()
			c.close()
		}(c)
	}
	wg.Wait()
	out := make([]*beastClient, 0, len(want))
	for _, b := range want {
		c := m.beastByID[b.ID]
		if c != nil {
			c.update(b)
		} else {
			c = newBeastClient(&m.o, b)
			m.beastByID[b.ID] = c
			c.start()
		}
		out = append(out, c)
	}
	m.beasts.Store(&out)
	return notes
}

// BeastStatus returns the status of every Beast output in configuration
// order.
func (m *Manager) BeastStatus() []BeastStatus {
	list := *m.beasts.Load()
	if len(list) == 0 {
		return nil
	}
	out := make([]BeastStatus, 0, len(list))
	for _, c := range list {
		out = append(out, c.Status())
	}
	return out
}

func (m *Manager) beast(id string) *beastClient {
	for _, c := range *m.beasts.Load() {
		if c.uid == id {
			return c
		}
	}
	return nil
}
