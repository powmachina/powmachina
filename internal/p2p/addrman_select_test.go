package p2p

import (
	"fmt"
	"net"
	"testing"
	"time"
)

func addrAt(ip string, port uint16) Address {
	return Address{IP: net.ParseIP(ip), Port: port, LastSeen: time.Now()}
}

// A node with one verified address has to be able to pick an unverified one too — otherwise it grows
// towards eight connections at the rate of one feeler every two minutes and is vulnerable to an
// eclipse the whole time.
func TestSelectMixesTriedAndNew(t *testing.T) {
	m := NewAddrMan(nil)
	tried := addrAt("88.198.113.1", 7696)
	m.Add(tried, net.ParseIP("88.198.113.1"))
	m.Good(tried)
	for i := 2; i < 8; i++ {
		m.Add(addrAt(fmt.Sprintf("45.83.104.%d", i), 7696), net.ParseIP("45.83.104.1"))
	}

	sawNew := false
	for i := 0; i < 200 && !sawNew; i++ {
		a, err := m.Select(false)
		if err != nil {
			t.Fatal(err)
		}
		if !a.IP.Equal(tried.IP) {
			sawNew = true
		}
	}
	if !sawNew {
		t.Fatal("the choice never reached the unverified addresses")
	}
}

// Anchor peers only make sense from verified addresses — there no coin is flipped.
func TestSelectTriedOnlyIgnoresNew(t *testing.T) {
	m := NewAddrMan(nil)
	m.Add(addrAt("45.83.104.9", 7696), net.ParseIP("45.83.104.9"))
	if _, err := m.Select(true); err != ErrNoAddresses {
		t.Fatalf("without verified addresses triedOnly has to fail, got %v", err)
	}
	tried := addrAt("88.198.113.2", 7696)
	m.Add(tried, net.ParseIP("88.198.113.2"))
	m.Good(tried)
	for i := 0; i < 30; i++ {
		a, err := m.Select(true)
		if err != nil {
			t.Fatal(err)
		}
		if !a.IP.Equal(tried.IP) {
			t.Fatalf("triedOnly returned the unverified address %v", a.IP)
		}
	}
}
