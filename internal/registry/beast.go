package registry

import (
	"context"
	"fmt"
	"sync"

	"airview/internal/ingest/beastin"
	"airview/internal/model"
)

// BeastRegistry adapts the registry to beastin.Registry (HOOKS WP-B2-beastin
// -> internal/registry): it admits Beast compat links (DESIGN §3, §5.1),
// records them in the connection log and marks their stations as seen.
//
// Identities map to stations as follows:
//
//   - UUID links: AdmitRequest.UUID (normalised by Admit);
//   - trusted-relay sub-stations: UUID plus RelaySub = the 0xE3 receiver ID
//     as 16 hex digits (beastin only reports Relay for TrustedRelays);
//   - UUID-less links: IdentKey = Identity.Key() ("ip:<base32>[/<slot>]",
//     keyed with IngestKey so it is stable across restarts).
//
// Admission under "invite" follows Admit: a Beast feeder cannot present an
// enrol token, so a new UUID station is held pending until the admin
// approves it (its frames reach only its own lane), and a UUID-less link is
// refused. link_guess (joining a UUID-less link to a station seen on the
// same address) is not done: each address slot is its own station.
//
// EpochBase is always 0: the registry does not persist clock epochs, so
// epochs restart at 1 on every connection (the ingest treats a new link as
// a new epoch anyway).
type BeastRegistry struct {
	r *Registry

	mu    sync.Mutex
	conns map[beastConnKey]int64 // conn_log row ids of attached identities
}

type beastConnKey struct {
	link uint64
	key  string
}

// Beast returns the beastin.Registry adapter of r.
func (r *Registry) Beast() *BeastRegistry {
	return &BeastRegistry{r: r, conns: map[beastConnKey]int64{}}
}

// IngestKey returns the persistent key for beastin.Options.ServerKey (HMAC
// of unidentified link addresses). It is derived from the registry's server
// secret, so UUID-less stations keep their identity across restarts.
func (r *Registry) IngestKey() []byte {
	return append([]byte(nil), mac(r.keys.server, "beastin ip key")...)
}

var _ beastin.Registry = (*BeastRegistry)(nil)

// Attach implements beastin.Registry.
func (b *BeastRegistry) Attach(ctx context.Context, id beastin.Identity) (beastin.Attachment, error) {
	req := AdmitRequest{Kind: model.StationKindBeast}
	if id.UUID != "" {
		req.UUID = id.UUID
		if id.Relay {
			req.RelaySub = fmt.Sprintf("%016x", id.ReceiverID)
		}
	} else {
		req.IdentKey = id.Key()
	}
	res, err := b.r.Admit(ctx, req)
	if err != nil {
		return beastin.Attachment{}, err
	}
	if !res.Allowed() {
		adm := model.AdmDenied
		if res.Code == CodeRevoked {
			adm = model.AdmRevoked
		}
		return beastin.Attachment{Station: res.Station.Idx, Admission: adm, Reason: res.Reason}, nil
	}
	st := res.Station
	b.r.Seen(st.Idx, b.r.now())
	if id.RemoteIP.IsValid() {
		if cid, err := b.r.ConnOpen(ctx, st.Idx, id.RemoteIP, model.StationKindBeast); err == nil {
			b.mu.Lock()
			b.conns[beastConnKey{id.Link, id.Key()}] = cid
			b.mu.Unlock()
		}
	}
	return beastin.Attachment{
		Station:   st.Idx,
		FeedID:    model.StationFeedID(st.SID, 0),
		Admission: st.Admission,
		Located:   st.Located,
	}, nil
}

// Detach implements beastin.Registry.
func (b *BeastRegistry) Detach(id beastin.Identity, a beastin.Attachment) {
	k := beastConnKey{id.Link, id.Key()}
	b.mu.Lock()
	cid, ok := b.conns[k]
	delete(b.conns, k)
	b.mu.Unlock()
	if a.Station != 0 {
		b.r.Seen(a.Station, b.r.now())
	}
	if ok {
		_ = b.r.ConnClose(context.Background(), cid, 0)
	}
}
