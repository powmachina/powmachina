package p2p

import (
	"context"
	"errors"
	"fmt"
	"net"
	"testing"
)

// fakeResolver returns prepared answers — the tests must not go onto the network.
func fakeResolver(answers map[string][]net.IP) Resolver {
	return func(_ context.Context, host string) ([]net.IP, error) {
		ips, ok := answers[host]
		if !ok {
			return nil, errors.New("an unknown seed")
		}
		return ips, nil
	}
}

func ips(prefix string, n int) []net.IP {
	out := make([]net.IP, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, net.ParseIP(fmt.Sprintf("%s.%d.%d", prefix, i/256%256, i%256)))
	}
	return out
}

func TestBootstrapFillsAddrman(t *testing.T) {
	m := fixedMan()
	resolve := fakeResolver(map[string][]net.IP{
		"seed1.example": ips("198.51", 10),
		"seed2.example": ips("203.0", 10),
	})
	added, err := Bootstrap(context.Background(), m,
		[]string{"seed1.example", "seed2.example"}, nil, resolve, 7696)
	if err != nil {
		t.Fatal(err)
	}
	if added != 20 {
		t.Fatalf("expected 20 addresses added, got %d", added)
	}
	if newCount, _ := m.Size(); newCount != 20 {
		t.Fatalf("the addrman holds %d addresses", newCount)
	}
}

// A seed that returns thousands of addresses must not get more influence than the others.
func TestSeedCannotFloodAddrman(t *testing.T) {
	m := fixedMan()
	resolve := fakeResolver(map[string][]net.IP{
		"greedy.example": ips("198", 5000),
		"honest.example": ips("203.0", 5),
	})
	added, err := Bootstrap(context.Background(), m,
		[]string{"greedy.example", "honest.example"}, nil, resolve, 7696)
	if err != nil {
		t.Fatal(err)
	}
	if added > MaxAddrsPerSeed+5 {
		t.Fatalf("too many addresses were taken from the greedy seed: %d", added)
	}
	t.Logf("the seed offered 5 000 addresses, the node took at most %d", MaxAddrsPerSeed)
}

// Every seed has a source group of its own — otherwise they would all occupy the same buckets.
func TestSeedsHaveDistinctSources(t *testing.T) {
	a := seedSource("seed1.example")
	b := seedSource("seed2.example")
	if a.Equal(b) {
		t.Fatal("two seeds have the same source")
	}
	if !seedSource("seed1.example").Equal(a) {
		t.Fatal("a seed's source is not stable")
	}
	if string(Netgroup(a, nil)) == string(Netgroup(b, nil)) {
		t.Fatal("the seeds belong to the same network group")
	}
}

// When DNS fails (an outage or censorship), the node uses the addresses baked into the binary.
func TestFixedSeedsFallback(t *testing.T) {
	m := fixedMan()
	failing := func(context.Context, string) ([]net.IP, error) {
		return nil, errors.New("DNS unreachable")
	}
	added, err := Bootstrap(context.Background(), m, []string{"seed1.example"},
		[]string{"5.9.100.1:7696", "88.198.113.5"}, failing, 7696)
	if err != nil {
		t.Fatal(err)
	}
	if added != 2 {
		t.Fatalf("expected 2 addresses from the baked-in list, got %d", added)
	}
	// An address without a port gets the default one.
	got, err := m.Select(false)
	if err != nil {
		t.Fatal(err)
	}
	if got.Port != 7696 {
		t.Fatalf("expected the default port 7696, got %d", got.Port)
	}
}

// Once the addrman has addresses, the seeds are not asked — after a restart it is a better source than any seed.
func TestBootstrapSkippedWhenAddrmanIsFull(t *testing.T) {
	m := fixedMan()
	for i := 0; i < EnoughAddresses+10; i++ {
		m.Add(addr(fmt.Sprintf("198.%d.%d.1", i/256%256, i%256), 7696),
			net.ParseIP(fmt.Sprintf("203.%d.0.1", i%256)))
	}
	called := false
	resolve := func(context.Context, string) ([]net.IP, error) {
		called = true
		return ips("198.51", 5), nil
	}
	added, err := Bootstrap(context.Background(), m, []string{"seed1.example"}, nil, resolve, 7696)
	if err != nil || added != 0 {
		t.Fatalf("with a full addrman nothing should be added: %d, %v", added, err)
	}
	if called {
		t.Fatal("a seed was asked even though the addrman was full")
	}
}

// Unusable addresses have to be filtered out.
func TestUnusableAddressesFiltered(t *testing.T) {
	m := fixedMan()
	resolve := fakeResolver(map[string][]net.IP{
		"seed.example": {
			net.ParseIP("127.0.0.1"),        // loopback
			net.ParseIP("0.0.0.0"),          // unspecified
			net.ParseIP("169.254.1.1"),      // link-local (cloud metadata too)
			net.ParseIP("224.0.0.1"),        // multicast
			net.ParseIP("10.0.0.5"),         // RFC1918 — the provider's internal network
			net.ParseIP("192.168.1.10"),     // RFC1918
			net.ParseIP("100.64.0.1"),       // CGNAT
			net.ParseIP("fc00::1"),          // IPv6 ULA
			net.ParseIP("5.9.10.11"),        // genuinely routable
			net.ParseIP("2a01:4f8:1:2::10"), // a genuinely routable IPv6
		},
	})
	added, err := Bootstrap(context.Background(), m, []string{"seed.example"}, nil, resolve, 7696)
	if err != nil {
		t.Fatal(err)
	}
	if added != 2 {
		t.Fatalf("expected 2 usable addresses, got %d", added)
	}
}

// A poisoned seed must not get the node to knock on a provider's internal network — abuse reports
// and server locks follow from that. With `-allow-private` this stops applying, because
// then it is a closed deployment of your own.
func TestPrivateAddressesPassOnlyWithAllowPrivate(t *testing.T) {
	resolve := fakeResolver(map[string][]net.IP{
		"seed.example": {net.ParseIP("10.0.0.5"), net.ParseIP("192.168.1.10")},
	})
	strict := fixedMan()
	if added, _ := Bootstrap(context.Background(), strict, []string{"seed.example"}, nil, resolve, 7696); added != 0 {
		t.Fatalf("%d private addresses passed on the public network", added)
	}
	loose := fixedMan()
	loose.AllowPrivate = true
	added, err := Bootstrap(context.Background(), loose, []string{"seed.example"}, nil, resolve, 7696)
	if err != nil || added != 2 {
		t.Fatalf("with -allow-private: %d addresses, %v", added, err)
	}
}

// When everything fails, the node has to say so rather than continue silently.
func TestBootstrapReportsTotalFailure(t *testing.T) {
	m := fixedMan()
	failing := func(context.Context, string) ([]net.IP, error) {
		return nil, errors.New("DNS unreachable")
	}
	if _, err := Bootstrap(context.Background(), m, []string{"seed.example"}, nil, failing, 7696); !errors.Is(err, ErrNoSeeds) {
		t.Fatalf("expected ErrNoSeeds, got %v", err)
	}
}
