package p2p

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"testing"
	"time"
)

func TestAddrRoundTrip(t *testing.T) {
	now := time.Unix(time.Now().Unix(), 0)
	in := []Address{
		{IP: net.ParseIP("88.198.113.1").To4(), Port: 7696, Services: ServiceFullNode | ServiceFullDag, LastSeen: now},
		{IP: net.ParseIP("2a01:4f8::1"), Port: 17696, Services: ServiceFullNode, LastSeen: now},
		{Onion: "abc.onion", Port: 7696}, // no place in this form
		{IP: net.IPv4zero, Port: 27696, LastSeen: now},
	}
	payload, err := EncodeAddr(in)
	if err != nil {
		t.Fatal(err)
	}
	out, err := DecodeAddr(payload)
	if err != nil {
		t.Fatal(err)
	}
	want := []Address{in[0], in[1], in[3]}
	if len(out) != len(want) {
		t.Fatalf("decoded %d addresses, expected %d", len(out), len(want))
	}
	for i := range want {
		if !out[i].IP.Equal(want[i].IP) || out[i].Port != want[i].Port || out[i].Services != want[i].Services ||
			!out[i].LastSeen.Equal(want[i].LastSeen) {
			t.Fatalf("entry %d: %+v, expected %+v", i, out[i], want[i])
		}
	}
	if !out[2].IP.IsUnspecified() {
		t.Fatal("the self-announcement lost its unspecified IP")
	}
}

func TestAddrRejectsBadPayload(t *testing.T) {
	one, _ := EncodeAddr([]Address{{IP: net.ParseIP("88.198.113.1"), Port: 7696, LastSeen: time.Now()}})
	tooMany := make([]byte, 2)
	binary.LittleEndian.PutUint16(tooMany, MaxAddrPerMessage+1)
	for name, p := range map[string][]byte{
		"empty":     nil,
		"truncated": one[:len(one)-1],
		"trailing":  append(append([]byte{}, one...), 0),
		"too many":  tooMany,
	} {
		if _, err := DecodeAddr(p); err == nil {
			t.Errorf("%s: decoded", name)
		}
	}
	many := make([]Address, MaxAddrPerMessage+1)
	for i := range many {
		many[i] = Address{IP: net.ParseIP(fmt.Sprintf("5.9.%d.%d", i/250, i%250+1)), Port: 7696}
	}
	if _, err := EncodeAddr(many); !errors.Is(err, ErrTooManyItems) {
		t.Fatalf("encoding over the cap: %v", err)
	}
}

// A full-DAG node looking for its own kind gets only addresses seen with the service, and none
// when there are none.
func TestSelectServices(t *testing.T) {
	m := NewAddrMan(nil)
	full := addrAt("88.198.113.1", 7696)
	full.Services = ServiceFullDag | ServiceFullNode
	m.Add(full, net.ParseIP("88.198.113.1"))
	for i := 2; i < 20; i++ {
		a := addrAt(fmt.Sprintf("45.83.%d.1", i), 7696)
		a.Services = ServiceFullNode
		m.Add(a, net.ParseIP(fmt.Sprintf("45.83.%d.1", i)))
	}
	for i := 0; i < 50; i++ {
		a, err := m.SelectServices(ServiceFullDag, nil)
		if err != nil || !a.IP.Equal(full.IP) {
			t.Fatalf("picked %v (%v), expected the full-DAG address", a.IP, err)
		}
	}
	if _, err := m.SelectServices(ServiceFullDag, func(a Address) bool { return a.IP.Equal(full.IP) }); err != ErrNoAddresses {
		t.Fatalf("with the only match skipped: %v", err)
	}
}

// The handshake decides the services of an address, not what somebody said about it.
func TestGoodTakesTheHandshakeServices(t *testing.T) {
	m := NewAddrMan(nil)
	a := addrAt("88.198.113.1", 7696)
	a.Services = ServiceFullDag
	m.Add(a, net.ParseIP("45.83.104.1"))
	a.Services = ServiceFullNode
	m.Good(a)
	later := a
	later.Services, later.LastSeen = ServiceFullDag, time.Now().Add(time.Minute)
	m.Add(later, net.ParseIP("45.83.104.1"))
	if _, err := m.SelectServices(ServiceFullDag, nil); err != ErrNoAddresses {
		t.Fatal("hearsay overrode the services from the handshake")
	}
}

// A getaddr answer gives a small table whole and a large one only in part.
func TestSampleShare(t *testing.T) {
	m := NewAddrMan(nil)
	for i := 0; i < 50; i++ {
		m.Add(addrAt(fmt.Sprintf("5.%d.1.1", i+1), 7696), net.ParseIP(fmt.Sprintf("5.%d.1.1", i+1)))
	}
	if n, _ := m.Size(); len(m.Sample(1000)) != n {
		t.Fatalf("a table of %d gave %d", n, len(m.Sample(1000)))
	}
	for i := 0; i < 400; i++ {
		m.Add(addrAt(fmt.Sprintf("6.%d.%d.1", i/200+1, i%200+1), 7696), net.ParseIP(fmt.Sprintf("6.%d.%d.1", i/200+1, i%200+1)))
	}
	n, _ := m.Size()
	if got := len(m.Sample(1000)); got != n*23/100 {
		t.Fatalf("a table of %d gave %d, expected %d", n, got, n*23/100)
	}
	if got := len(m.Sample(10)); got != 10 {
		t.Fatalf("the cap: %d", got)
	}
}
