package model

import "testing"

func TestACARSConfigNormalize(t *testing.T) {
	a := ACARSConfig{UDPListen: " bad ", TCPListen: "0.0.0.0:5551", Keep: 5}
	if n := a.Normalize(); len(n) != 2 || a.UDPListen != "" || a.Keep != 0 || a.TCPListen != "0.0.0.0:5551" {
		t.Fatalf("%v %+v", n, a)
	}
	if a.UDPAddr() != DefaultACARSUDP {
		t.Fatal(a.UDPAddr())
	}
	a = ACARSConfig{UDPListen: "off", TCPListen: "nope"}
	if n := a.Normalize(); len(n) != 1 || a.UDPAddr() != "" || a.TCPListen != "" {
		t.Fatalf("%v %+v", n, a)
	}
	a = ACARSConfig{UDPListen: ":5550", Keep: 20000}
	if n := a.Normalize(); len(n) != 0 || a.UDPAddr() != ":5550" {
		t.Fatalf("%v %+v", n, a)
	}
}
