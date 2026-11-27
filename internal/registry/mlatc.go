package registry

import (
	"context"
	"sync"

	"airview/internal/ingest/mlatc"
	"airview/internal/model"
)

// MLATCRegistry adapts the registry to mlatc.Registry (HOOKS WP-B3-mlatc ->
// internal/registry): it admits mlat-client links on the MLAT port (DESIGN
// §3, §5.2), records them in the connection log, marks their stations as
// seen and stores the handshake's position as the declared position when
// the station has none.
//
// Identities map to stations as for Beast links: a UUID client by its
// normalised UUID, a UUID-less client by Identity.Key() ("ip:<base32>" with
// a "/<slot>" suffix for further clients behind the same address; keyed
// with IngestKey so it is stable across restarts). The "user" field is the
// suggested name of a new station; "privacy": false gives a new station the
// region pin (DESIGN §19.2).
//
// The handshake's altitude does not say whether it is above the geoid or
// the ellipsoid: it is stored with model.AltRefAmbiguous and the survey
// resolves it (DESIGN §5.2). An owner-declared position is never replaced;
// later handshake positions go through FeederPosition (rate limit, large
// moves held pending a survey).
type MLATCRegistry struct {
	r *Registry

	mu    sync.Mutex
	conns map[mlatcConnKey]int64 // conn_log row ids of attached identities
}

type mlatcConnKey struct {
	link uint64
	key  string
}

// MLATC returns the mlatc.Registry adapter of r.
func (r *Registry) MLATC() *MLATCRegistry {
	return &MLATCRegistry{r: r, conns: map[mlatcConnKey]int64{}}
}

var _ mlatc.Registry = (*MLATCRegistry)(nil)

// Attach implements mlatc.Registry.
func (m *MLATCRegistry) Attach(ctx context.Context, id mlatc.Identity) (mlatc.Attachment, error) {
	req := AdmitRequest{Kind: model.StationKindMLATC, Name: id.User, ClockCls: id.ClockCls, PrivacyOff: id.PrivacyOff}
	if id.UUID != "" {
		req.UUID = id.UUID
	} else {
		req.IdentKey = id.Key()
	}
	res, err := m.r.Admit(ctx, req)
	if err != nil {
		return mlatc.Attachment{}, err
	}
	if !res.Allowed() {
		adm := model.AdmDenied
		if res.Code == CodeRevoked {
			adm = model.AdmRevoked
		}
		return mlatc.Attachment{Station: res.Station.Idx, Admission: adm, Reason: res.Reason}, nil
	}
	st := res.Station
	if id.Located {
		// Rate-limited and held pending a survey when it moves (PRIV-1,
		// position.go); an owner-declared position is never replaced.
		if up, _, err := m.r.FeederPosition(ctx, st.Idx, FeederPos{Lat: id.Lat, Lon: id.Lon, Alt: id.Alt,
			AltRef: model.AltRefAmbiguous}); err == nil {
			st = up
		}
	}
	m.r.Seen(st.Idx, m.r.now())
	if id.RemoteIP.IsValid() {
		if cid, err := m.r.ConnOpen(ctx, st.Idx, id.RemoteIP, model.StationKindMLATC); err == nil {
			m.mu.Lock()
			m.conns[mlatcConnKey{id.Link, id.Key()}] = cid
			m.mu.Unlock()
		}
	}
	return mlatc.Attachment{
		Station:   st.Idx,
		FeedID:    model.StationFeedID(st.SID, 0),
		Admission: st.Admission,
	}, nil
}

// Detach implements mlatc.Registry.
func (m *MLATCRegistry) Detach(id mlatc.Identity, a mlatc.Attachment) {
	k := mlatcConnKey{id.Link, id.Key()}
	m.mu.Lock()
	cid, ok := m.conns[k]
	delete(m.conns, k)
	m.mu.Unlock()
	if a.Station != 0 {
		m.r.Seen(a.Station, m.r.now())
	}
	if ok {
		_ = m.r.ConnClose(context.Background(), cid, 0)
	}
}
