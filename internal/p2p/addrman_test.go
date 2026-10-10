package p2p

import (
	"fmt"
	"net"
	"testing"
	"time"
)

func addr(ip string, port uint16) Address {
	return Address{IP: net.ParseIP(ip), Port: port, LastSeen: time.Now()}
}

func fixedMan() *AddrMan {
	var key [32]byte
	for i := range key {
		key[i] = byte(i)
	}
	return NewAddrManWithKey(key, nil)
}

// This is the test the addrman exists for: an attacker with thousands of addresses in one range
// must not occupy more than a fraction of the table. Otherwise they can cut the victim off the network.
func TestFloodFromOneSourceGroupIsContained(t *testing.T) {
	// An attacker with thousands of addresses across many /16s, but announced from one source.
	m := fixedMan()
	source := net.ParseIP("88.198.113.7")
	for i := 0; i < 5000; i++ {
		a := addr(fmt.Sprintf("198.%d.%d.%d", i/256%256, i%256, i%251), 8333)
		m.Add(a, source)
	}
	occupied := m.OccupiedNewBuckets()
	if occupied > newBucketsPerSourceGroup {
		t.Fatalf("one source occupied %d buckets, the cap is %d", occupied, newBucketsPerSourceGroup)
	}
	t.Logf("5 000 addresses across /16s from one source: %d of %d buckets (%.1f %%)",
		occupied, newBucketCount, float64(occupied)/newBucketCount*100)

	// An attacker with only one /16 range gets a single bucket.
	m2 := fixedMan()
	for i := 0; i < 5000; i++ {
		m2.Add(addr(fmt.Sprintf("5.9.%d.%d", i/256%256, i%256), 8333), source)
	}
	if got := m2.OccupiedNewBuckets(); got != 1 {
		t.Fatalf("an attacker with one /16 occupied %d buckets, expected 1", got)
	}
	t.Logf("5 000 addresses from one /16: 1 bucket of %d", newBucketCount)
}

// The same address from different sources has to land in different buckets — otherwise an attacker
// could compute in advance where their address will end up.
func TestSameAddressDifferentSourcesSpread(t *testing.T) {
	m := fixedMan()
	a := addr("5.9.100.1", 8333)
	buckets := make(map[int]bool)
	for i := 0; i < 20; i++ {
		src := net.ParseIP(fmt.Sprintf("203.%d.0.1", i))
		buckets[m.newBucket(a, Netgroup(src, nil))] = true
	}
	if len(buckets) < 5 {
		t.Fatalf("the same address landed in only %d different buckets, that is too few", len(buckets))
	}
}

// One address must not spread across the whole table, not even from many sources.
func TestAddressLimitedToFewBuckets(t *testing.T) {
	m := fixedMan()
	a := addr("5.9.100.1", 8333)
	for i := 0; i < 100; i++ {
		m.Add(a, net.ParseIP(fmt.Sprintf("203.%d.0.1", i%256)))
	}
	if got := m.OccupiedNewBuckets(); got > newBucketsPerAddress {
		t.Fatalf("one address is in %d buckets, the cap is %d", got, newBucketsPerAddress)
	}
}

// The distribution has to depend on the secret key — otherwise an attacker precomputes it.
func TestBucketsDependOnSecretKey(t *testing.T) {
	var k1, k2 [32]byte
	for i := range k1 {
		k1[i] = byte(i)
		k2[i] = byte(255 - i)
	}
	m1 := NewAddrManWithKey(k1, nil)
	m2 := NewAddrManWithKey(k2, nil)

	a := addr("5.9.100.1", 8333)
	src := Netgroup(net.ParseIP("88.198.113.7"), nil)
	same := 0
	for i := 0; i < 50; i++ {
		x := addr(fmt.Sprintf("5.9.100.%d", i), 8333)
		if m1.newBucket(x, src) == m2.newBucket(x, src) {
			same++
		}
	}
	if same > 10 {
		t.Fatalf("the distribution barely changed with another key (%d/50 matches)", same)
	}
	_ = a
}

// A successful connection moves an address from new to tried.
func TestGoodPromotesToTried(t *testing.T) {
	m := fixedMan()
	a := addr("5.9.100.1", 8333)
	m.Add(a, net.ParseIP("88.198.113.7"))
	if n, tr := m.Size(); n != 1 || tr != 0 {
		t.Fatalf("after Add I expect 1 new and 0 tried, I have %d/%d", n, tr)
	}
	m.Good(a)
	if n, tr := m.Size(); n != 0 || tr != 1 {
		t.Fatalf("after Good I expect 0 new and 1 tried, I have %d/%d", n, tr)
	}
	got, err := m.Select(true)
	if err != nil || got.key() != a.key() {
		t.Fatalf("Select(tried) has to return the verified address, I have %v %v", got, err)
	}
}

// An overfull tried bucket does not throw the address away but returns it to new (test-before-evict).
func TestTriedEvictionDemotesInsteadOfDropping(t *testing.T) {
	m := fixedMan()
	base := time.Now().Add(-time.Hour)
	// Find the addresses that land in the same tried bucket and fill it past its capacity.
	target := -1
	added := 0
	for i := 0; i < 400_000 && added < bucketSize+5; i++ {
		a := Address{IP: net.ParseIP(fmt.Sprintf("198.%d.%d.%d", i/65536%256, i/256%256, i%256)),
			Port: 8333, LastSeen: base.Add(time.Duration(i) * time.Second)}
		b := m.triedBucket(a)
		if target == -1 {
			target = b
		}
		if b != target {
			continue
		}
		m.Add(a, net.ParseIP("88.198.113.7"))
		m.Good(a)
		added++
	}
	if added <= bucketSize {
		t.Fatalf("the bucket could not be filled (%d addresses)", added)
	}
	n, tr := m.Size()
	if tr > bucketSize {
		t.Fatalf("the tried bucket exceeded its capacity: %d", tr)
	}
	if n == 0 {
		t.Fatal("the evicted addresses should have gone back to new, not disappeared")
	}
	t.Logf("after overfilling: %d in new (returned), %d in tried", n, tr)
}

// Onion addresses form a group of their own and do not mix with IPv6.
func TestOnionHasOwnGroup(t *testing.T) {
	onion := Address{Onion: "abcdefghijklmnop.onion", Port: 8333, LastSeen: time.Now()}
	v6 := addr("2001:db8::1", 8333)
	if string(onion.group(nil)) == string(v6.group(nil)) {
		t.Fatal("onion and IPv6 must not be in the same group")
	}
	m := fixedMan()
	if !m.Add(onion, net.ParseIP("88.198.113.7")) {
		t.Fatal("an onion address should be addable")
	}
}

func TestRejectsGarbage(t *testing.T) {
	m := fixedMan()
	if m.Add(Address{}, net.ParseIP("88.198.113.7")) {
		t.Fatal("an empty address must not be accepted")
	}
	if m.Add(Address{IP: net.ParseIP("5.9.100.1")}, nil) {
		t.Fatal("an address without a port must not be accepted")
	}
	if _, err := m.Select(false); err != ErrNoAddresses {
		t.Fatalf("an empty addrman should return ErrNoAddresses, it returned %v", err)
	}
}

// The address choice must not be proportional to the number of addresses, otherwise an attacker buys
// a chance by sheer quantity. The bucket occupancy should decide — and that is limited for them.
func TestSelectIsNotProportionalToAddressCount(t *testing.T) {
	m := fixedMan()
	// The attacker: 2 000 addresses from one /16 (they land in one bucket).
	for i := 0; i < 2000; i++ {
		m.Add(addr(fmt.Sprintf("5.9.%d.%d", i/256%256, i%256), 8333), net.ParseIP("88.198.113.7"))
	}
	// An honest peer: one address from another network and another source.
	honest := addr("45.83.2.9", 8333)
	m.Add(honest, net.ParseIP("45.83.2.1"))

	hits := 0
	const rounds = 400
	for i := 0; i < rounds; i++ {
		got, err := m.Select(false)
		if err != nil {
			t.Fatal(err)
		}
		if got.key() == honest.key() {
			hits++
		}
	}
	// With a choice by address count that would be ~0.2 %. With a choice by buckets ~50 %.
	if hits < rounds/5 {
		t.Fatalf("the honest address came up only %d times out of %d — the attacker bought a chance by quantity",
			hits, rounds)
	}
	t.Logf("the honest address came up %d times out of %d (the attacker had 2 000 addresses, it had one)", hits, rounds)
}

// The feeler has to get addresses from the new table — those are precisely the ones to verify.
func TestSelectNewOnlyReturnsUnverified(t *testing.T) {
	m := fixedMan()
	verified := addr("45.83.2.1", 8333)
	m.Add(verified, net.ParseIP("45.83.2.100"))
	m.Good(verified) // it moves into tried

	fresh := addr("5.9.100.5", 8333)
	m.Add(fresh, net.ParseIP("88.198.113.9"))

	for i := 0; i < 20; i++ {
		got, err := m.SelectNew()
		if err != nil {
			t.Fatal(err)
		}
		if got.key() == verified.key() {
			t.Fatal("SelectNew returned a verified address from the tried table")
		}
	}

	// When there is nothing in the new table, it has to say so.
	empty := fixedMan()
	only := addr("45.83.2.7", 8333)
	empty.Add(only, net.ParseIP("45.83.2.100"))
	empty.Good(only)
	if _, err := empty.SelectNew(); err != ErrNoAddresses {
		t.Fatalf("an empty new table should give ErrNoAddresses, it gave %v", err)
	}
}
