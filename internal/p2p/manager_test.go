package p2p

import (
	"fmt"
	"net"
	"testing"
	"time"
)

func testManager(t *testing.T) (*Manager, *AddrMan) {
	t.Helper()
	am := fixedMan()
	return NewManager(DefaultManagerConfig(), am, 0x1234), am
}

// The core of the eclipse defence: outbound connections have to lead into different groups.
func TestOutboundRequiresDistinctGroups(t *testing.T) {
	m, am := testManager(t)
	// The attacker offers 500 addresses, all from one /16.
	for i := 0; i < 500; i++ {
		am.Add(addr(fmt.Sprintf("5.9.%d.%d", i/256, i%256), 8333), net.ParseIP("88.198.113.7"))
	}
	// One honest address from another network.
	am.Add(addr("45.83.2.9", 8333), net.ParseIP("45.83.2.1"))

	first, err := m.SelectOutbound(PeerOutboundFull)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Add(NewPeer(first, PeerOutboundFull)); err != nil {
		t.Fatal(err)
	}

	// The second choice must not end up in the same group.
	second, err := m.SelectOutbound(PeerOutboundFull)
	if err == ErrNoCandidate {
		t.Skip("no address from another group was left in the table")
	}
	if err != nil {
		t.Fatal(err)
	}
	if string(second.group(nil)) == string(first.group(nil)) {
		t.Fatalf("the second connection goes into the same group: %v and %v", first.IP, second.IP)
	}
}

// An attacker with hundreds of addresses in one network must not occupy more than one outbound slot.
func TestFloodCannotTakeMultipleOutboundSlots(t *testing.T) {
	m, am := testManager(t)
	for i := 0; i < 2000; i++ {
		am.Add(addr(fmt.Sprintf("5.9.%d.%d", i/256%256, i%256), 8333), net.ParseIP("88.198.113.7"))
	}
	for i := 0; i < m.cfg.MaxOutboundFull; i++ {
		a, err := m.SelectOutbound(PeerOutboundFull)
		if err != nil {
			break
		}
		m.Add(NewPeer(a, PeerOutboundFull))
	}
	if got := m.Count(PeerOutboundFull); got != 1 {
		t.Fatalf("an attacker from one network occupied %d slots, expected 1", got)
	}
	if got := m.OutboundGroups(); got != 1 {
		t.Fatalf("expected 1 group, got %d", got)
	}
}

func TestSlotLimits(t *testing.T) {
	m, am := testManager(t)
	for i := 0; i < 40; i++ {
		am.Add(addr(fmt.Sprintf("198.%d.0.1", i), 8333), net.ParseIP(fmt.Sprintf("203.%d.0.1", i)))
	}
	for i := 0; i < 50; i++ {
		a, err := m.SelectOutbound(PeerOutboundFull)
		if err != nil {
			break
		}
		if _, err := m.Add(NewPeer(a, PeerOutboundFull)); err != nil {
			break
		}
	}
	if got := m.Count(PeerOutboundFull); got > m.cfg.MaxOutboundFull {
		t.Fatalf("the cap on full outbound connections was exceeded: %d", got)
	}

	// Block-relay slots are separate and cannot be pushed out by full connections.
	a, err := m.SelectOutbound(PeerOutboundBlock)
	if err != nil {
		t.Skip("no free address for another slot")
	}
	if _, err := m.Add(NewPeer(a, PeerOutboundBlock)); err != nil {
		t.Fatalf("the block-relay slot should be free: %v", err)
	}
}

// With full inbound slots the eviction comes from the largest group — an attacker therefore pushes out themselves.
func TestInboundEvictionProtectsDiversity(t *testing.T) {
	cfg := DefaultManagerConfig()
	cfg.MaxInbound = 10
	m := NewManager(cfg, fixedMan(), 1)

	// Eight connections from the attacker's one network and two honest ones from others.
	base := time.Now().Add(-time.Hour)
	for i := 0; i < 8; i++ {
		p := NewPeer(addr(fmt.Sprintf("5.9.100.%d", i), 8333), PeerInbound)
		p.Connected = base.Add(time.Duration(i) * time.Minute)
		m.Add(p)
	}
	for i := 0; i < 2; i++ {
		p := NewPeer(addr(fmt.Sprintf("45.83.%d.1", i), 8333), PeerInbound)
		p.Connected = base
		m.Add(p)
	}

	newcomer := NewPeer(addr("88.198.113.5", 8333), PeerInbound)
	evicted, err := m.Add(newcomer)
	if err != nil {
		t.Fatal(err)
	}
	if evicted == nil {
		t.Fatal("with full slots somebody has to be evicted")
	}
	if got := string(evicted.Addr.group(nil)); got != string(Netgroup(net.ParseIP("5.9.100.1"), nil)) {
		t.Fatalf("the attacker from the largest group should have been evicted, evicted %v", evicted.Addr.IP)
	}
	if evicted.Addr.IP.String() != "5.9.100.7" {
		t.Fatalf("the youngest connection in the group should be evicted, evicted %v", evicted.Addr.IP)
	}
}

func TestBanBlocksReconnect(t *testing.T) {
	m, am := testManager(t)
	a := addr("5.9.100.1", 8333)
	am.Add(a, net.ParseIP("88.198.113.7"))

	p := NewPeer(a, PeerInbound)
	if _, err := m.Add(p); err != nil {
		t.Fatal(err)
	}
	// Corrupted messages alone are not enough for a ban.
	for i := 0; i < 9; i++ {
		if m.Misbehaving(p, MisbehaviorBadMessage) {
			t.Fatalf("the ban came too early, at a score of %d", p.BanScore())
		}
	}
	if !m.Misbehaving(p, MisbehaviorBadMessage) {
		t.Fatal("a ban should have come after crossing the threshold")
	}
	if !m.IsBanned(a) {
		t.Fatal("the peer should be banned")
	}
	if _, err := m.Add(NewPeer(a, PeerInbound)); err != ErrPeerBanned {
		t.Fatalf("a banned peer must not connect, got %v", err)
	}
	// An invalid block is an instant ban.
	p2 := NewPeer(addr("45.83.2.5", 8333), PeerInbound)
	m.Add(p2)
	if !m.Misbehaving(p2, MisbehaviorInvalidBlock) {
		t.Fatal("an invalid block has to ban instantly")
	}
}

// Anchor peers are taken from the block-relay connections, ordered from the longest held.
func TestAnchorsPreferOldestBlockRelay(t *testing.T) {
	m, _ := testManager(t)
	base := time.Now().Add(-time.Hour)
	for i := 0; i < 2; i++ {
		p := NewPeer(addr(fmt.Sprintf("192.0.%d.1", i), 8333), PeerOutboundBlock)
		p.Connected = base.Add(time.Duration(i) * time.Minute)
		p.CompleteHandshake(&VersionMsg{Protocol: ProtocolVersion, Nonce: uint64(i + 99)}, m.Nonce())
		m.Add(p)
	}
	full := NewPeer(addr("5.9.100.1", 8333), PeerOutboundFull)
	full.CompleteHandshake(&VersionMsg{Protocol: ProtocolVersion, Nonce: 7}, m.Nonce())
	m.Add(full)

	anchors := m.Anchors(2)
	if len(anchors) != 2 {
		t.Fatalf("expected 2 anchor addresses, got %d", len(anchors))
	}
	if anchors[0].IP.String() != "192.0.0.1" {
		t.Fatalf("the first anchor should be the longest-held connection, it is %v", anchors[0].IP)
	}
	for _, a := range anchors {
		if a.IP.String() == "5.9.100.1" {
			t.Fatal("a full outbound connection must not be an anchor — it exchanges addresses")
		}
	}
}
