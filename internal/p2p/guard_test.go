package p2p

import (
	"fmt"
	"net"
	"testing"
	"time"
)

func v4(s string, port uint16) Address { return Address{IP: net.ParseIP(s), Port: port} }
func v6(s string, port uint16) Address { return Address{IP: net.ParseIP(s), Port: port} }

// A banned attacker reconnects from a different source port — the ban has to stop them.
// This was a real hole: the ban was on `IP:port`.
func TestBanHoldsAcrossSourcePorts(t *testing.T) {
	m := NewManager(DefaultManagerConfig(), NewAddrMan(nil), 1)
	m.Ban(v4("88.198.113.7", 51234))
	for _, port := range []uint16{51235, 40000, 7696} {
		if !m.IsBanned(v4("88.198.113.7", port)) {
			t.Fatalf("the ban does not hold for port %d — an attacker would bypass it with a new connection", port)
		}
	}
	if m.IsBanned(v4("88.198.113.8", 51234)) {
		t.Fatal("the ban hit a neighbouring address")
	}
}

// IPv6: one machine has a whole /64. A ban on one address would be bypassed with another of the 2⁶⁴.
func TestBanCoversIPv6Slash64(t *testing.T) {
	m := NewManager(DefaultManagerConfig(), NewAddrMan(nil), 1)
	m.Ban(v6("2001:db8:aa:bb::1", 7696))
	if !m.IsBanned(v6("2001:db8:aa:bb:dead:beef:1:2", 7696)) {
		t.Fatal("the ban does not hold for another address in the same /64")
	}
	if m.IsBanned(v6("2001:db8:aa:bc::1", 7696)) {
		t.Fatal("the ban hit a different /64")
	}
}

// The ban list has a cap — an attacker with thousands of addresses cannot inflate it forever.
func TestBanListIsBounded(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	b := NewBanList(100)
	for i := range 10_000 {
		b.Ban(fmt.Sprintf("4:10.%d.%d.1", i/256, i%256), now.Add(24*time.Hour), now)
	}
	if b.Len() > 100 {
		t.Fatalf("the ban list has %d entries, the cap is 100", b.Len())
	}
	// A new ban is always recorded — a full list must not refuse a ban.
	b.Ban("4:5.9.100.9", now.Add(24*time.Hour), now)
	if !b.Banned("4:5.9.100.9", now) {
		t.Fatal("with a full list the new ban was not recorded")
	}
}

// The bans for the firewall: IPv4 as an address, IPv6 as a /64, expired ones not at all.
func TestBanListActiveForFirewall(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	b := NewBanList(0)
	b.Ban(HostKey(v4("88.198.113.7", 1)), now.Add(time.Hour), now)
	b.Ban(HostKey(v6("2001:db8:1:2::5", 1)), now.Add(time.Hour), now)
	b.Ban(HostKey(v4("88.198.113.8", 1)), now.Add(-time.Second), now)
	got := b.Active(now)
	want := []string{"2001:db8:1:2::/64", "88.198.113.7"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("for the firewall %v, expected %v", got, want)
	}
}

func testGate() (*Gate, time.Time) {
	now := time.Unix(1_800_000_000, 0)
	return NewGate(DefaultGateConfig(), nil, now), now
}

// At most two connections at a time from one machine.
func TestGateLimitsPerHost(t *testing.T) {
	g, now := testGate()
	var ok int
	for i := range 5 {
		if tk, _ := g.Admit(v4("88.198.113.7", uint16(40000+i)), now.Add(time.Duration(i)*10*time.Second)); tk != nil {
			tk.Established()
			ok++
		}
	}
	if ok != 2 {
		t.Fatalf("%d connections from one machine passed, expected 2", ok)
	}
}

// At most eight connections from one network — not even many addresses in one network help.
func TestGateLimitsPerGroup(t *testing.T) {
	g, now := testGate()
	var ok int
	for i := range 30 {
		// the same /16, different machines, slowly — so the rate limit does not hit first
		addr := v4(fmt.Sprintf("203.0.%d.%d", i/200, 1+i%200), 40000)
		if tk, _ := g.Admit(addr, now.Add(time.Duration(i)*10*time.Second)); tk != nil {
			tk.Established()
			ok++
		}
	}
	if ok != 8 {
		t.Fatalf("%d connections from one network passed, expected 8", ok)
	}
}

// A flood of new connections from one network hits that network's limit and does not spend the
// global limit — others still connect.
func TestGateFloodFromOneGroupDoesNotStarveOthers(t *testing.T) {
	g, now := testGate()
	for i := range 1000 {
		if tk, _ := g.Admit(v4(fmt.Sprintf("5.9.%d.%d", i/250, 1+i%250), 40000), now); tk != nil {
			tk.Release() // close the connection right away so the connection cap does not stop it
		}
	}
	tk, why := g.Admit(v4("45.83.2.10", 40000), now)
	if tk == nil {
		t.Fatalf("an honest peer from another network did not pass: %s", why)
	}
}

// Unfinished handshakes have a cap — an attacker that opens a connection and stays silent does not
// occupy more than 16 places.
func TestGateLimitsPending(t *testing.T) {
	g, now := testGate()
	var ok int
	for i := range 100 {
		addr := v4(fmt.Sprintf("10.%d.%d.1", i, i), 40000) // each from a different /16
		if tk, _ := g.Admit(addr, now.Add(time.Duration(i)*time.Second)); tk != nil {
			ok++ // Established is not called — the handshake "hangs"
		}
	}
	if ok != DefaultGateConfig().MaxPending {
		t.Fatalf("%d hanging handshakes passed, the cap is %d", ok, DefaultGateConfig().MaxPending)
	}
}

// Releasing gives the place back; releasing again breaks nothing.
func TestGateReleaseReturnsSlot(t *testing.T) {
	g, now := testGate()
	a := v4("88.198.113.7", 40000)
	t1, _ := g.Admit(a, now)
	t2, _ := g.Admit(a, now.Add(10*time.Second))
	if t1 == nil || t2 == nil {
		t.Fatal("the first two connections should have passed")
	}
	t1.Established()
	t1.Release()
	t1.Release()
	if tk, why := g.Admit(a, now.Add(20*time.Second)); tk == nil {
		t.Fatalf("the place did not come back after the release: %s", why)
	}
	pending, _, _ := g.Stats()
	if pending < 0 {
		t.Fatalf("the counter of unfinished handshakes is negative: %d", pending)
	}
}

// The group tracking has a cap, and groups with live connections do not fall out of it — otherwise
// an attacker would bypass a group's limit by pushing it off the list.
func TestGateTrackingIsBoundedAndKeepsLiveGroups(t *testing.T) {
	cfg := DefaultGateConfig()
	cfg.MaxTracked = 50
	cfg.MaxPending = 1 << 30
	cfg.AcceptRate, cfg.AcceptBurst = 1e9, 1e9
	now := time.Unix(1_800_000_000, 0)
	g := NewGate(cfg, nil, now)

	// A group with eight live connections.
	for i := range 8 {
		tk, why := g.Admit(v4(fmt.Sprintf("88.198.113.%d", i+1), 40000), now.Add(time.Duration(i)*10*time.Second))
		if tk == nil {
			t.Fatalf("connection %d did not pass: %s", i, why)
		}
		tk.Established()
	}
	// A flood from thousands of other networks.
	for i := range 5000 {
		if tk, _ := g.Admit(v4(fmt.Sprintf("%d.%d.1.1", 11+i/250, i%250), 40000), now.Add(time.Hour)); tk != nil {
			tk.Release()
		}
	}
	if _, _, groups := g.Stats(); groups > 51 {
		t.Fatalf("%d groups are tracked, the cap is 50", groups)
	}
	if tk, _ := g.Admit(v4("88.198.113.200", 40000), now.Add(2*time.Hour)); tk != nil {
		t.Fatal("a ninth connection from the network passed — the group limit could be bypassed by pushing it off the list")
	}
}

// Limits do not apply to our own machine (local tools, tests).
func TestGateExemptsLoopback(t *testing.T) {
	g, now := testGate()
	for i := range 100 {
		if tk, why := g.Admit(v4("127.0.0.1", uint16(40000+i)), now); tk == nil {
			t.Fatalf("loopback connection %d did not pass: %s", i, why)
		}
	}
}

// An honest peer does not come close to the per-peer limit: a block every two minutes and a few small messages.
func TestPeerMeterAllowsHonestAndStopsFlood(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	m := NewPeerMeter(InboundPeerLimits(), now)
	for i := range 600 { // 20 minutes of honest traffic
		at := now.Add(time.Duration(i) * 2 * time.Second)
		size := 64
		if i%60 == 0 {
			size = 256 << 10 // a block
		}
		if !m.Allow(size, at) {
			t.Fatalf("an honest peer was stopped at message %d", i)
		}
	}
	flood := NewPeerMeter(InboundPeerLimits(), now)
	var blocked bool
	for i := range 10_000 {
		if !flood.Allow(1024, now.Add(time.Duration(i)*time.Millisecond)) {
			blocked = true
			break
		}
	}
	if !blocked {
		t.Fatal("10 000 messages in 10 seconds passed — the flood was not stopped")
	}
}
