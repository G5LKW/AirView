package registry

import (
	"context"
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"math"
	"net/netip"
	"strconv"
	"time"

	"airview/internal/model"
)

// keySet holds the server secret and the purpose keys derived from it with
// HKDF-SHA256. The server secret is 32 random bytes created on the first
// Open and kept in registry_keys, so every derived value (public pin points,
// unidentified-station keys, audit pseudonyms, peer tags) is stable across
// restarts and changes only if the database is replaced.
type keySet struct {
	server []byte
	pin    []byte // public pin PRF (§8.3)
	ip     []byte // unidentified compat stations: "ip:" + HMAC(ip‖port)
	claim  []byte // claim code hashes
	token  []byte // enrol token hashes
	audit  []byte // pseudonyms of deleted subjects
	peer   []byte // unlinkable sync-peer tags (§8.2)
}

const serverKeyName = "server"

func (r *Registry) loadKeys(ctx context.Context) error {
	var key []byte
	err := r.wdb.QueryRowContext(ctx, `SELECT key FROM registry_keys WHERE name=?`, serverKeyName).Scan(&key)
	if err == sql.ErrNoRows {
		key = make([]byte, 32)
		if _, err := io.ReadFull(r.rnd, key); err != nil {
			return fmt.Errorf("registry: server key: %w", err)
		}
		// INSERT OR IGNORE + re-read: a concurrent first Open of the same
		// file must end up with one key.
		if _, err := r.wdb.ExecContext(ctx, `INSERT OR IGNORE INTO registry_keys(name, key, created) VALUES(?,?,?)`,
			serverKeyName, key, r.now().Unix()); err != nil {
			return fmt.Errorf("registry: store server key: %w", err)
		}
		err = r.wdb.QueryRowContext(ctx, `SELECT key FROM registry_keys WHERE name=?`, serverKeyName).Scan(&key)
	}
	if err != nil {
		return fmt.Errorf("registry: load server key: %w", err)
	}
	if len(key) < 32 {
		return fmt.Errorf("registry: server key is %d bytes; stations.db is damaged", len(key))
	}
	derive := func(info string) []byte {
		k, err := hkdf.Key(sha256.New, key, nil, "airview registry "+info, 32)
		if err != nil {
			panic(err) // cannot happen for 32-byte output
		}
		return k
	}
	r.keys = keySet{
		server: key,
		pin:    derive("pin"),
		ip:     derive("ip"),
		claim:  derive("claim"),
		token:  derive("enrol"),
		audit:  derive("audit"),
		peer:   derive("peer"),
	}
	return nil
}

func mac(key []byte, parts ...string) []byte {
	m := hmac.New(sha256.New, key)
	for _, p := range parts {
		var l [4]byte
		binary.BigEndian.PutUint32(l[:], uint32(len(p)))
		m.Write(l[:]) // length-prefixed: no ambiguity between part boundaries
		m.Write([]byte(p))
	}
	return m.Sum(nil)
}

// UnidentifiedKey returns the identity key of a compat link without a UUID:
// "ip:" + 20 hex digits of HMAC(serverKey, ip‖port) (DESIGN §3). Pass
// port 0 to key by address only, which is what ingest wants for "stats merge
// by IP" (a feeder's source port changes on every reconnect).
func (r *Registry) UnidentifiedKey(ip netip.Addr, port int) string {
	return "ip:" + hex.EncodeToString(mac(r.keys.ip, ip.Unmap().String(), strconv.Itoa(port))[:10])
}

// PeerTag returns the unlinkable tag under which the owner of viewerSID
// sees the sync peer peerSID during the ISO week of t (DESIGN §8.2):
// HMAC(key, viewerSID‖peerSID‖ISOweek), first 6 bytes, hex. Tags differ
// between viewers and between weeks, so they cannot be joined.
func (r *Registry) PeerTag(viewerSID, peerSID string, t time.Time) string {
	y, w := t.UTC().ISOWeek()
	return hex.EncodeToString(mac(r.keys.peer, viewerSID, peerSID, strconv.Itoa(y)+"W"+strconv.Itoa(w))[:6])
}

// pseudonym returns the stable audit pseudonym of a deleted subject.
func (r *Registry) pseudonym(kind, id string) string {
	return "x-" + hex.EncodeToString(mac(r.keys.audit, kind, id)[:6])
}

// ---------------------------------------------------------------- public pin

// PublicPoint is a station's stored public pin (DESIGN §8.3).
type PublicPoint struct {
	// Precision is what the owner chose (model.Pin*); Effective is what is
	// shown after the rural guard (cell01 may be shown as region).
	Precision, Effective string
	Lat, Lon             float64
}

// cellIndex returns the integer cell coordinates of (lat, lon) for a cell
// size in degrees. Longitudes are wrapped into [-180, 180).
func cellIndex(lat, lon, size float64) (int64, int64) {
	lon = math.Mod(lon+180, 360)
	if lon < 0 {
		lon += 360
	}
	lon -= 180
	lat = min(max(lat, -90), 90-1e-9)
	return int64(math.Floor(lat / size)), int64(math.Floor(lon / size))
}

// pinPoint computes the keyed point for a station inside its cell: the
// cell origin plus a uniform offset derived from
// HMAC-SHA256(HKDF(serverKey,"pin"), sid‖precision‖cell). The offset is
// kept 1 % away from the cell edges so rounding never moves a point into a
// neighbouring cell.
func (r *Registry) pinPoint(sid, precision string, cLat, cLon int64) (lat, lon float64) {
	size := model.PinCellDeg(precision)
	h := mac(r.keys.pin, sid, precision, strconv.FormatInt(cLat, 10), strconv.FormatInt(cLon, 10))
	u := float64(binary.BigEndian.Uint64(h[0:8])>>11) / (1 << 53) // [0,1)
	v := float64(binary.BigEndian.Uint64(h[8:16])>>11) / (1 << 53)
	u = 0.01 + 0.98*u
	v = 0.01 + 0.98*v
	lat = (float64(cLat) + u) * size
	lon = (float64(cLon) + v) * size
	return min(max(lat, -90), 90), lon
}
