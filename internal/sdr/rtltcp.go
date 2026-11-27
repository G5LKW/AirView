package sdr

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"
)

// rtl_tcp wire protocol (as observed on the wire and described in the
// rtl-sdr project's documentation; written without the GPL source):
//
//   - On connect the server sends a 12-byte header: the magic "RTL0", the
//     tuner type (uint32, big endian) and the number of discrete tuner gain
//     steps (uint32, big endian).
//   - After that the server streams raw samples: interleaved unsigned 8-bit
//     I and Q (zero at 127.5), at the configured sample rate, with no
//     framing, sequence numbers or timestamps. When the client reads too
//     slowly the server drops whole buffers silently.
//   - The client sends 5-byte commands: a command byte followed by a uint32
//     big-endian parameter.

// Command bytes of the rtl_tcp protocol.
const (
	CmdSetFrequency      = 0x01 // Hz
	CmdSetSampleRate     = 0x02 // Hz
	CmdSetGainMode       = 0x03 // 0 = automatic (tuner AGC), 1 = manual
	CmdSetGain           = 0x04 // tenths of a dB; the server picks the nearest step
	CmdSetFreqCorrection = 0x05 // ppm (signed, two's complement)
	CmdSetIFGain         = 0x06 // stage<<16 | gain in tenths of a dB (E4000 only)
	CmdSetTestMode       = 0x07 // 1 = counter instead of samples
	CmdSetAGCMode        = 0x08 // RTL2832U digital AGC, 0/1
	CmdSetDirectSampling = 0x09 // 0 off, 1 I branch, 2 Q branch
	CmdSetOffsetTuning   = 0x0a // 0/1
	CmdSetRTLXtal        = 0x0b // Hz
	CmdSetTunerXtal      = 0x0c // Hz
	CmdSetGainByIndex    = 0x0d // index into the tuner's gain table
	CmdSetBiasTee        = 0x0e // 0/1 (newer servers; ignored by older ones)
)

// TunerType is the tuner chip reported in the rtl_tcp header.
type TunerType uint32

// Tuner types as numbered by rtl_tcp.
const (
	TunerUnknown TunerType = iota
	TunerE4000
	TunerFC0012
	TunerFC0013
	TunerFC2580
	TunerR820T
	TunerR828D
)

func (t TunerType) String() string {
	switch t {
	case TunerE4000:
		return "E4000"
	case TunerFC0012:
		return "FC0012"
	case TunerFC0013:
		return "FC0013"
	case TunerFC2580:
		return "FC2580"
	case TunerR820T:
		return "R820T/R820T2/R860"
	case TunerR828D:
		return "R828D"
	}
	return fmt.Sprintf("unknown(%d)", uint32(t))
}

// DongleInfo is the rtl_tcp connection header.
type DongleInfo struct {
	Tuner     TunerType
	GainCount uint32 // number of discrete gain steps (0 when unknown)
}

// ErrNotRTLTCP is returned when the peer does not start with the "RTL0"
// magic.
var ErrNotRTLTCP = errors.New("sdr: peer is not an rtl_tcp server (no RTL0 header)")

// Client is a connection to an rtl_tcp server. Read returns raw I/Q bytes;
// the Set* methods may be called concurrently with Read.
type Client struct {
	conn net.Conn
	info DongleInfo
	wmu  sync.Mutex
}

// Dial connects to an rtl_tcp server and reads its header. The context
// bounds the connection set-up only.
func Dial(ctx context.Context, addr string) (*Client, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetReadDeadline(dl)
	} else {
		_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	}
	c, err := NewClient(conn)
	if err != nil {
		conn.Close()
		return nil, err
	}
	_ = conn.SetReadDeadline(time.Time{})
	return c, nil
}

// NewClient reads the rtl_tcp header from an established connection.
func NewClient(conn net.Conn) (*Client, error) {
	var h [12]byte
	if _, err := io.ReadFull(conn, h[:]); err != nil {
		return nil, fmt.Errorf("sdr: reading rtl_tcp header: %w", err)
	}
	if string(h[:4]) != "RTL0" {
		return nil, ErrNotRTLTCP
	}
	return &Client{conn: conn, info: DongleInfo{
		Tuner:     TunerType(binary.BigEndian.Uint32(h[4:8])),
		GainCount: binary.BigEndian.Uint32(h[8:12]),
	}}, nil
}

// Info returns the connection header.
func (c *Client) Info() DongleInfo { return c.info }

// Conn returns the underlying connection (for deadlines).
func (c *Client) Conn() net.Conn { return c.conn }

// Read reads raw interleaved I/Q bytes.
func (c *Client) Read(p []byte) (int, error) { return c.conn.Read(p) }

// Close closes the connection.
func (c *Client) Close() error { return c.conn.Close() }

// Command sends one raw command.
func (c *Client) Command(cmd byte, param uint32) error {
	var b [5]byte
	b[0] = cmd
	binary.BigEndian.PutUint32(b[1:], param)
	c.wmu.Lock()
	defer c.wmu.Unlock()
	_ = c.conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	_, err := c.conn.Write(b[:])
	_ = c.conn.SetWriteDeadline(time.Time{})
	return err
}

func b2u(v bool) uint32 {
	if v {
		return 1
	}
	return 0
}

// SetFrequency tunes the receiver (Hz).
func (c *Client) SetFrequency(hz uint32) error { return c.Command(CmdSetFrequency, hz) }

// SetSampleRate sets the complex sample rate (Hz).
func (c *Client) SetSampleRate(hz uint32) error { return c.Command(CmdSetSampleRate, hz) }

// SetTunerAGC switches between the tuner's automatic gain (true) and a
// manual gain (false).
func (c *Client) SetTunerAGC(auto bool) error { return c.Command(CmdSetGainMode, b2u(!auto)) }

// SetGain sets a manual gain in tenths of a dB (call SetTunerAGC(false)
// first); the server rounds to the nearest supported step.
func (c *Client) SetGain(tenthsDB int) error { return c.Command(CmdSetGain, uint32(int32(tenthsDB))) }

// SetGainIndex selects the index-th entry of the tuner's gain table
// (0 .. GainCount-1, ascending gain).
func (c *Client) SetGainIndex(i int) error { return c.Command(CmdSetGainByIndex, uint32(int32(i))) }

// SetFreqCorrection sets the oscillator correction in ppm.
func (c *Client) SetFreqCorrection(ppm int) error {
	return c.Command(CmdSetFreqCorrection, uint32(int32(ppm)))
}

// SetRTLAGC switches the RTL2832U's digital AGC.
func (c *Client) SetRTLAGC(on bool) error { return c.Command(CmdSetAGCMode, b2u(on)) }

// SetBiasTee switches the 4.5 V antenna supply of dongles that have one
// (servers that do not know the command ignore it).
func (c *Client) SetBiasTee(on bool) error { return c.Command(CmdSetBiasTee, b2u(on)) }

// SetDirectSampling selects direct sampling (0 off, 1 I, 2 Q).
func (c *Client) SetDirectSampling(mode int) error {
	return c.Command(CmdSetDirectSampling, uint32(mode))
}

// SetOffsetTuning switches offset tuning (E4000).
func (c *Client) SetOffsetTuning(on bool) error { return c.Command(CmdSetOffsetTuning, b2u(on)) }
