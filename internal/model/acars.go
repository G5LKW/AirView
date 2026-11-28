package model

import (
	"net"
	"strconv"
	"strings"
)

// ACARSConfig configures the ACARS / VDL Mode 2 message input (top-level "acars"
// block; internal/acars). AirView does not demodulate VHF: a separate
// decoder (acarsdec or dumpvdl2 on its own SDR) sends its JSON output to
// these listeners. Off by default. The messages are kept in memory only
// and are shown to the station owner / admin, never in public views.
type ACARSConfig struct {
	// Enabled turns the ACARS input on (default false).
	Enabled bool `json:"enabled"`
	// UDPListen is the "host:port" for JSON datagrams from acarsdec
	// (--output json:udp:...) or dumpvdl2 (--output decoded:json:udp:...).
	// Default "127.0.0.1:5550"; ":5550" accepts decoders on other hosts of
	// the network. "off" disables the UDP input.
	UDPListen string `json:"udpListen,omitempty"`
	// TCPListen is the "host:port" for newline-separated JSON over TCP
	// (decoders in TCP client mode); "" = off (default).
	TCPListen string `json:"tcpListen,omitempty"`
	// Keep is the number of messages kept in memory (default 2000,
	// 100..20000; typically well under 1 kB each, at most about 4 kB).
	Keep int `json:"keep"`
}

// DefaultACARSUDP is the default UDP listen address of the ACARS input.
const DefaultACARSUDP = "127.0.0.1:5550"

// UDPAddr returns the effective UDP listen address ("" = off).
func (a ACARSConfig) UDPAddr() string {
	switch s := strings.TrimSpace(a.UDPListen); s {
	case "":
		return DefaultACARSUDP
	case "off":
		return ""
	default:
		return s
	}
}

// Normalize corrects out-of-range values in place and returns notes.
func (a *ACARSConfig) Normalize() []string {
	var n notes
	p := "acars."
	a.UDPListen = strings.TrimSpace(a.UDPListen)
	a.TCPListen = strings.TrimSpace(a.TCPListen)
	if a.UDPListen != "" && a.UDPListen != "off" && !validListenAddr(a.UDPListen) {
		n.addf("%sudpListen %q is not host:port; the default %s is used", p, a.UDPListen, DefaultACARSUDP)
		a.UDPListen = ""
	}
	if a.TCPListen != "" && !validListenAddr(a.TCPListen) {
		n.addf("%stcpListen %q is not host:port; the TCP input is off", p, a.TCPListen)
		a.TCPListen = ""
	}
	zeroOrRangeI(&n, p+"keep", &a.Keep, 100, 20000)
	return n
}

func validListenAddr(s string) bool {
	host, port, err := net.SplitHostPort(s)
	if err != nil || strings.ContainsAny(host, " /") {
		return false
	}
	v, err := strconv.Atoi(port)
	return err == nil && v >= 0 && v <= 65535
}
