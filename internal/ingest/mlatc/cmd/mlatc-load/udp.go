package main

import (
	"encoding/json"
	"fmt"
	"net"
	"strconv"

	"airview/internal/ingest/mlatc"
)

// udpClient sends a load client's timed frames as mlat-client datagrams:
// a header with the key, a sequence number and a base timestamp, then
// messages with 32-bit deltas, flushed at about 1,400 bytes.
type udpClient struct {
	pc   net.Conn
	key  uint32
	seq  uint16
	base uint64
	buf  []byte
}

// dialUDP connects to the [host, port, key] of a handshake reply (nil when
// the server answered false).
func dialUDP(raw json.RawMessage) (*udpClient, error) {
	var v []json.RawMessage
	if len(raw) == 0 || json.Unmarshal(raw, &v) != nil || len(v) != 3 {
		return nil, nil
	}
	var host string
	var port int
	var key uint32
	if json.Unmarshal(v[0], &host) != nil || json.Unmarshal(v[1], &port) != nil || json.Unmarshal(v[2], &key) != nil {
		return nil, fmt.Errorf("bad udp_transport %s", raw)
	}
	pc, err := net.Dial("udp", net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		return nil, err
	}
	return &udpClient{pc: pc, key: key, buf: make([]byte, 0, 1500)}, nil
}

func (u *udpClient) room(t uint64, n int, st *loadStats) {
	if len(u.buf) > 0 && len(u.buf)+n > 1400 {
		u.flush(st)
	}
	if len(u.buf) == 0 {
		u.base = t
		u.buf = mlatc.AppendUDPHeader(u.buf, u.key, u.seq, t)
		u.seq++
	}
}

func (u *udpClient) mlat(t uint64, frame []byte, st *loadStats) {
	u.room(t, 19, st)
	u.buf, _ = mlatc.AppendUDPMLAT(u.buf, int32(int64(t)-int64(u.base)), frame)
}

func (u *udpClient) sync(et, ot uint64, frame []byte, st *loadStats) {
	u.room(et, 37, st)
	u.buf, _ = mlatc.AppendUDPSync(u.buf, int32(int64(et)-int64(u.base)), int32(int64(ot)-int64(u.base)), frame, frame)
}

func (u *udpClient) flush(st *loadStats) {
	if len(u.buf) == 0 {
		return
	}
	if _, err := u.pc.Write(u.buf); err == nil {
		st.datagrams.Add(1)
	}
	u.buf = u.buf[:0]
}
