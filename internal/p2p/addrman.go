package p2p

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"math/big"
	"net"
	"sync"
	"time"
)

// AddrMan is the table of known addresses.
//
// The design copies Bitcoin Core, because it is the only implementation that has survived eclipse
// attacks in practice: an address does not land in a bucket by itself, but by the **node's secret
// key**, the **source's group** and the **address's group**. An attacker therefore cannot compute
// where their addresses will land, and from one source group they can occupy only a small fraction
// of the table.
//
// Without that property, eclipsing an exchange's node is cheaper than a 51 % attack (SECURITY.md §4).
const (
	newBucketCount   = 1024
	triedBucketCount = 256
	bucketSize       = 64

	// From one source group at most this many new buckets can be occupied.
	newBucketsPerSourceGroup = 64
	// One address group occupies at most this many tried buckets.
	triedBucketsPerGroup = 8
	// The same address appears in at most this many buckets of the new table.
	newBucketsPerAddress = 8
)

var ErrNoAddresses = errors.New("p2p: the addrman is empty")

// Address is a peer's address.
type Address struct {
	IP       net.IP
	Port     uint16
	Onion    string // non-empty for .onion peers
	LastSeen time.Time
	// Services the address was last seen with: from the handshake once we connected to it,
	// before that what the peer that told us about it claimed. A hint for the choice, not trust.
	Services uint64
}

// String is the address's stable text key (for onion peers too).
func (a Address) String() string { return a.key() }

func (a Address) key() string {
	if a.Onion != "" {
		return a.Onion
	}
	return net.JoinHostPort(a.IP.String(), itoa(a.Port))
}

func (a Address) group(asn ASNLookup) []byte {
	if a.Onion != "" {
		return OnionGroup(a.Onion)
	}
	return Netgroup(a.IP, asn)
}

type entry struct {
	addr     Address
	source   []byte // the group of the source we got the address from
	attempts int    // failed connection attempts
	lastTry  time.Time
	tried    bool
	refCount int // in how many new buckets the address is stored
}

// AddrMan is safe for concurrent use.
type AddrMan struct {
	// AllowPrivate allows private ranges (a closed deployment of your own, regtest).
	// It stays off on the public network: an attempt to connect into somebody else's internal
	// network is scanning as far as a provider is concerned — see routable.go.
	AllowPrivate bool

	mu    sync.Mutex
	key   [32]byte
	asn   ASNLookup
	byKey map[string]*entry
	newB  [newBucketCount]map[string]bool
	tried [triedBucketCount]map[string]bool
	// local are the **IP addresses** the node knows are its own (the handshake reveals them). The
	// key is the IP, not IP:port — a dialer announces itself from an ephemeral port, and there is
	// no point calling ourselves on any port anyway.
	local map[string]bool
}

// NewAddrMan creates the table with a random secret key. The key has to be persisted together with
// the table — once it changes, the whole bucket distribution changes.
func NewAddrMan(asn ASNLookup) *AddrMan {
	var key [32]byte
	if _, err := rand.Read(key[:]); err != nil {
		panic("p2p: cannot obtain randomness for the addrman key: " + err.Error())
	}
	return NewAddrManWithKey(key, asn)
}

func NewAddrManWithKey(key [32]byte, asn ASNLookup) *AddrMan {
	m := &AddrMan{key: key, asn: asn, byKey: make(map[string]*entry)}
	for i := range m.newB {
		m.newB[i] = make(map[string]bool)
	}
	for i := range m.tried {
		m.tried[i] = make(map[string]bool)
	}
	return m
}

func (m *AddrMan) hash(parts ...[]byte) []byte {
	h := sha256.New()
	h.Write(m.key[:])
	for _, p := range parts {
		var l [4]byte
		binary.LittleEndian.PutUint32(l[:], uint32(len(p)))
		h.Write(l[:])
		h.Write(p)
	}
	return h.Sum(nil)
}

func (m *AddrMan) newBucket(addr Address, source []byte) int {
	g := addr.group(m.asn)
	i := binary.LittleEndian.Uint64(m.hash(g, source)[:8]) % newBucketsPerSourceGroup
	var ib [8]byte
	binary.LittleEndian.PutUint64(ib[:], i)
	return int(binary.LittleEndian.Uint64(m.hash(source, ib[:])[:8]) % newBucketCount)
}

func (m *AddrMan) triedBucket(addr Address) int {
	i := binary.LittleEndian.Uint64(m.hash([]byte(addr.key()))[:8]) % triedBucketsPerGroup
	var ib [8]byte
	binary.LittleEndian.PutUint64(ib[:], i)
	return int(binary.LittleEndian.Uint64(m.hash(addr.group(m.asn), ib[:])[:8]) % triedBucketCount)
}

// Add records an address we received from a peer whose source address is source.
func (m *AddrMan) Add(addr Address, source net.IP) bool {
	if !Dialable(addr, m.AllowPrivate) {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	k := addr.key()
	if addr.IP != nil && m.local[addr.IP.String()] {
		return false // our own address; otherwise it comes back after every seed query
	}
	srcGroup := Netgroup(source, m.asn)
	e, exists := m.byKey[k]
	if !exists {
		e = &entry{addr: addr, source: srcGroup}
		m.byKey[k] = e
	} else if addr.LastSeen.After(e.addr.LastSeen) {
		e.addr.LastSeen = addr.LastSeen
		if !e.tried {
			// A tried address has services from its handshake; a hearsay one takes the newest claim.
			e.addr.Services = addr.Services
		}
	}
	if e.tried || e.refCount >= newBucketsPerAddress {
		return false
	}

	b := m.newBucket(addr, srcGroup)
	if m.newB[b][k] {
		return false
	}
	if len(m.newB[b]) >= bucketSize {
		m.evictOldest(b)
	}
	m.newB[b][k] = true
	e.refCount++
	return true
}

// evictOldest frees space in a full bucket — it throws out the oldest address.
func (m *AddrMan) evictOldest(bucket int) {
	var oldestKey string
	var oldest time.Time
	for k := range m.newB[bucket] {
		e := m.byKey[k]
		if oldestKey == "" || e.addr.LastSeen.Before(oldest) {
			oldestKey, oldest = k, e.addr.LastSeen
		}
	}
	if oldestKey == "" {
		return
	}
	delete(m.newB[bucket], oldestKey)
	if e := m.byKey[oldestKey]; e != nil {
		e.refCount--
		if e.refCount <= 0 && !e.tried {
			delete(m.byKey, oldestKey)
		}
	}
}

// removeLocked throws an address out of every table.
func (m *AddrMan) removeLocked(key string) {
	for i := range m.newB {
		delete(m.newB[i], key)
	}
	for i := range m.tried {
		delete(m.tried[i], key)
	}
	delete(m.byKey, key)
}

// Good marks an address a connection succeeded with — it moves it into the tried table. The
// services in `addr` are the ones the peer gave in the handshake, and they replace any heard ones.
func (m *AddrMan) Good(addr Address) {
	m.mu.Lock()
	defer m.mu.Unlock()

	k := addr.key()
	e, ok := m.byKey[k]
	if !ok {
		e = &entry{addr: addr, source: addr.group(m.asn)}
		m.byKey[k] = e
	}
	e.attempts = 0
	e.addr.Services = addr.Services
	e.addr.LastSeen = time.Now()
	if e.tried {
		return
	}
	for i := range m.newB {
		if m.newB[i][k] {
			delete(m.newB[i], k)
		}
	}
	e.refCount = 0
	e.tried = true

	b := m.triedBucket(addr)
	if len(m.tried[b]) >= bucketSize {
		// Test-before-evict: the eviction candidate goes back into the new table, it is not thrown away.
		m.demoteOldestTried(b)
	}
	m.tried[b][k] = true
}

func (m *AddrMan) demoteOldestTried(bucket int) {
	var oldestKey string
	var oldest time.Time
	for k := range m.tried[bucket] {
		e := m.byKey[k]
		if oldestKey == "" || e.addr.LastSeen.Before(oldest) {
			oldestKey, oldest = k, e.addr.LastSeen
		}
	}
	if oldestKey == "" {
		return
	}
	delete(m.tried[bucket], oldestKey)
	if e := m.byKey[oldestKey]; e != nil {
		e.tried = false
		nb := m.newBucket(e.addr, e.source)
		if len(m.newB[nb]) >= bucketSize {
			m.evictOldest(nb)
		}
		m.newB[nb][oldestKey] = true
		e.refCount = 1
	}
}

// MarkLocal remembers that an address is our own and throws it out of the tables.
//
// It is only discovered during the handshake (the nonce in the version message), because a node
// behind NAT does not know its public address. Without this, a node that is a DNS seed itself keeps
// calling itself: it adds its own address from the seed, the selection offers it, the handshake
// refuses it — and ten seconds later again.
func (m *AddrMan) MarkLocal(addr Address) {
	if addr.IP == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.local == nil {
		m.local = make(map[string]bool)
	}
	m.local[addr.IP.String()] = true
	for key, e := range m.byKey {
		if e.addr.IP.Equal(addr.IP) {
			m.removeLocked(key)
		}
	}
}

// IsLocal says whether an address is our own.
func (m *AddrMan) IsLocal(addr Address) bool {
	if addr.IP == nil {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.local[addr.IP.String()]
}

// Attempt records a failed connection attempt.
func (m *AddrMan) Attempt(addr Address) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if e, ok := m.byKey[addr.key()]; ok {
		e.attempts++
		e.lastTry = time.Now()
	}
}

// Select picks an address to attempt a connection to.
//
// `triedOnly` limits the choice to verified addresses — that is for anchor peers, which only make
// sense when the node has already connected to them successfully at some point.
func (m *AddrMan) Select(triedOnly bool) (Address, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	// A random **bucket** is picked first, and only then an address inside it.
	//
	// This matters: if the choice were proportional to the number of addresses, an attacker with
	// 500 addresses would have a 500× better chance than an honest address. This way the occupancy
	// of the buckets decides — and that is limited for an attacker (addresses from one /16 land in
	// a single bucket).
	pick := func(buckets []map[string]bool) (Address, bool) {
		nonEmpty := make([]int, 0, 32)
		for i := range buckets {
			if len(buckets[i]) > 0 {
				nonEmpty = append(nonEmpty, i)
			}
		}
		if len(nonEmpty) == 0 {
			return Address{}, false
		}
		b := buckets[nonEmpty[randInt(len(nonEmpty))]]
		n := randInt(len(b))
		i := 0
		for k := range b {
			if i == n {
				return m.byKey[k].addr, true
			}
			i++
		}
		return Address{}, false
	}

	if triedOnly {
		if addr, ok := pick(m.tried[:]); ok {
			return addr, nil
		}
		return Address{}, ErrNoAddresses
	}

	// Between verified and unverified a **coin is flipped** — not "verified first, then the rest".
	// The difference is fundamental: with the latter, a node with one verified address never picks
	// another (that one is already connected, there are no others in tried) and it grows towards
	// eight connections at the rate of one feeler every two minutes — exactly when it is most
	// vulnerable to an eclipse. Bitcoin Core solves it the same way.
	//
	// At the same time it is no reason to worry about an attacker flooding the new table: the
	// choice goes to a bucket first, and from one /16 an attacker has a limited number of those.
	first, second := m.tried[:], m.newB[:]
	if randInt(2) == 0 {
		first, second = second, first
	}
	if addr, ok := pick(first); ok {
		return addr, nil
	}
	if addr, ok := pick(second); ok {
		return addr, nil
	}
	return Address{}, ErrNoAddresses
}

// SelectServices picks an address that was seen with every service in `want`, from either table,
// skipping those `skip` names. Like Select it picks a bucket first and an address inside it, only
// among the buckets that hold a match: whoever claims a service for a thousand addresses from one
// /16 still has only that network's buckets.
func (m *AddrMan) SelectServices(want uint64, skip func(Address) bool) (Address, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var buckets [][]string
	collect := func(tables []map[string]bool) {
		for i := range tables {
			var match []string
			for k := range tables[i] {
				e := m.byKey[k]
				if e.addr.Services&want == want && (skip == nil || !skip(e.addr)) {
					match = append(match, k)
				}
			}
			if len(match) > 0 {
				buckets = append(buckets, match)
			}
		}
	}
	collect(m.tried[:])
	collect(m.newB[:])
	if len(buckets) == 0 {
		return Address{}, ErrNoAddresses
	}
	b := buckets[randInt(len(buckets))]
	return m.byKey[b[randInt(len(b))]].addr, nil
}

// Sample is what the node answers a `getaddr` with: random addresses, leaving out onion ones and
// those that keep failing. From a table of more than a hundred it gives at most 23 % (Bitcoin
// Core's share), so one question does not copy out the whole table; a small one it gives whole.
func (m *AddrMan) Sample(max int) []Address {
	m.mu.Lock()
	all := make([]Address, 0, len(m.byKey))
	for _, e := range m.byKey {
		if e.addr.Onion != "" || e.addr.IP == nil || (!e.tried && e.attempts >= 3) {
			continue
		}
		all = append(all, e.addr)
	}
	m.mu.Unlock()
	n := len(all)
	if n > 100 {
		n = n * 23 / 100
	}
	if n > max {
		n = max
	}
	for i := 0; i < n; i++ {
		j := i + randInt(len(all)-i)
		all[i], all[j] = all[j], all[i]
	}
	return all[:n]
}

// SelectNew picks an address **from the new table only** — that is the job for a feeler connection.
//
// The point: when an attacker floods the addrman with addresses that lead nowhere, the ordinary
// selection would keep offering them. The feeler tries them, and what works moves into tried; what
// does not stays behind. The tried table therefore holds verified addresses that can be relied on
// after a restart.
func (m *AddrMan) SelectNew() (Address, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	nonEmpty := make([]int, 0, 32)
	for i := range m.newB {
		if len(m.newB[i]) > 0 {
			nonEmpty = append(nonEmpty, i)
		}
	}
	if len(nonEmpty) == 0 {
		return Address{}, ErrNoAddresses
	}
	b := m.newB[nonEmpty[randInt(len(nonEmpty))]]
	n := randInt(len(b))
	i := 0
	for k := range b {
		if i == n {
			return m.byKey[k].addr, nil
		}
		i++
	}
	return Address{}, ErrNoAddresses
}

// Size returns the number of addresses in the new and tried tables.
func (m *AddrMan) Size() (newCount, triedCount int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, e := range m.byKey {
		if e.tried {
			triedCount++
		} else {
			newCount++
		}
	}
	return
}

// OccupiedNewBuckets says how many new buckets are occupied — it is used in tests to verify that
// one source cannot fill the table.
func (m *AddrMan) OccupiedNewBuckets() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for i := range m.newB {
		if len(m.newB[i]) > 0 {
			n++
		}
	}
	return n
}

func randInt(n int) int {
	if n <= 1 {
		return 0
	}
	v, err := rand.Int(rand.Reader, big.NewInt(int64(n)))
	if err != nil {
		return 0
	}
	return int(v.Int64())
}

func itoa(p uint16) string {
	if p == 0 {
		return "0"
	}
	var buf [5]byte
	i := len(buf)
	for p > 0 {
		i--
		buf[i] = byte('0' + p%10)
		p /= 10
	}
	return string(buf[i:])
}
