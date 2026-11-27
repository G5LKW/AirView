package uplink

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
)

// persisted is the small per-uplink state file (<StateDir>/uplinks/<id>/
// state.json): receiver numbers given to local feeds, and which enrol
// tokens a server has already accepted (so a used-up single-use token is
// not sent again).
type persisted struct {
	Rx       map[string]int    `json:"rx,omitempty"`
	Enrolled map[string]string `json:"enrolled,omitempty"` // serverId -> sha256(token) hex
	// ServerID is the serverId of the aggregator last connected to.
	ServerID string `json:"serverId,omitempty"`
}

const stateFile = "state.json"

func loadState(dir string) persisted {
	var p persisted
	if dir != "" {
		if b, err := os.ReadFile(filepath.Join(dir, stateFile)); err == nil {
			_ = json.Unmarshal(b, &p)
		}
	}
	if p.Rx == nil {
		p.Rx = map[string]int{}
	}
	if p.Enrolled == nil {
		p.Enrolled = map[string]string{}
	}
	return p
}

func saveState(dir string, p persisted) error {
	if dir == "" {
		return nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(p, "", " ")
	if err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(dir, stateFile), b, 0o600)
}

func tokenHash(tok string) string {
	h := sha256.Sum256([]byte(tok))
	return hex.EncodeToString(h[:])
}

// assignRx returns the receiver number of feedID, assigning the lowest free
// number 1..255 on first use. ok is false when all are taken.
func (p *persisted) assignRx(feedID string, used map[int]bool) (int, bool) {
	if n, ok := p.Rx[feedID]; ok && n >= 1 && n <= 255 {
		return n, true
	}
	taken := map[int]bool{}
	for _, n := range p.Rx {
		taken[n] = true
	}
	for n := range used {
		taken[n] = true
	}
	for n := 1; n <= 255; n++ {
		if !taken[n] {
			p.Rx[feedID] = n
			return n, true
		}
	}
	return 0, false
}
