package p2p

import (
	"net"
	"testing"
)

// What may be dialled from a public server. The list is the same one Bitcoin Core has in
// `IsRoutable()`. The rule is not about the chain's security but about the provider not locking our
// server for scanning an internal network.
func TestRoutable(t *testing.T) {
	no := []string{
		"0.0.0.0", "0.1.2.3", // "this network"
		"127.0.0.1", "::1", // loopback
		"0.0.0.0", "0.1.2.3", // "this network"
		"100.64.0.1",           // CGNAT
		"169.254.169.254",      // cloud metadata
		"224.0.0.1", "ff02::1", // multicast
		"fe80::1",                      // link-local IPv6
		"fc00::1", "fd12:3456:789a::1", // IPv6 ULA
		"192.0.2.1", "198.51.100.1", "203.0.113.1", // documentation (RFC 5737)
		"2001:db8::1",     // documentation IPv6 (RFC 3849)
		"2001:10::1",      // ORCHID (RFC 4843)
		"198.18.0.1",      // benchmarking
		"240.0.0.1",       // reserved
		"255.255.255.255", // broadcast
	}
	for _, s := range no {
		if Routable(net.ParseIP(s)) {
			t.Errorf("%s must not be dialled", s)
		}
	}
	yes := []string{"1.1.1.1", "5.9.10.11", "88.198.1.2", "2a01:4f8:1:2::10", "2606:4700::1111"}
	for _, s := range yes {
		if !Routable(net.ParseIP(s)) {
			t.Errorf("%s is a public address and should pass", s)
		}
	}
	if Routable(nil) {
		t.Error("nil address")
	}
}

// Onion always goes (it is routed through a proxy, not by IP); with AllowPrivate an internal network passes too,
// but not nonsense even then.
func TestDialable(t *testing.T) {
	priv := Address{IP: net.ParseIP("10.0.0.5"), Port: 7696}
	if Dialable(priv, false) {
		t.Error("a private address passed on the public network")
	}
	if !Dialable(priv, true) {
		t.Error("a private address did not pass even with AllowPrivate")
	}
	if !Dialable(Address{Onion: "abc.onion", Port: 7696}, false) {
		t.Error("an onion address should pass")
	}
	if Dialable(Address{IP: net.ParseIP("5.9.10.11")}, false) {
		t.Error("an address without a port passed")
	}
	if Dialable(Address{IP: net.ParseIP("0.0.0.0"), Port: 7696}, true) {
		t.Error("an unspecified address passed even with AllowPrivate")
	}
}

// The address table has to refuse a private address already on insertion — so the node never tries to
// connect to it, whether it comes from a seed, from gossip or from a manually given peer.
func TestAddrManRejectsPrivateByDefault(t *testing.T) {
	priv := Address{IP: net.ParseIP("10.0.0.5"), Port: 7696}
	src := net.ParseIP("5.9.10.11")

	m := NewAddrMan(nil)
	if m.Add(priv, src) {
		t.Fatal("a private address got into the table")
	}
	if a, err := m.Select(false); err == nil {
		t.Fatalf("the table handed out the address %v, it should be empty", a)
	}
	m.AllowPrivate = true
	if !m.Add(priv, src) {
		t.Fatal("with AllowPrivate it should have passed")
	}
}
