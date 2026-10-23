package p2p

import (
	"errors"
	"sync"
	"time"
)

// Manager holds the connection slots and decides who to connect to.
//
// The whole point is spread: outbound connections have to lead into **different network groups**.
// Eight connections into one network are, from an eclipse attack's point of view, one connection —
// and eclipsing an exchange's node is cheaper than a 51 % attack (SECURITY.md §4).

type ManagerConfig struct {
	// MaxOutboundFull is the number of full outbound connections (blocks, transactions, addresses).
	MaxOutboundFull int
	// MaxOutboundBlock is the number of blocks-only connections. They do not exchange addresses, so
	// an attacker cannot influence them through `addr` messages — that is precisely their purpose.
	MaxOutboundBlock int
	// MaxInbound is the cap on incoming connections.
	MaxInbound int
	// BanDuration is how long a ban lasts.
	BanDuration time.Duration
	// RequireDistinctGroups turns the spread off, for tests and regtest only.
	RequireDistinctGroups bool
}

func DefaultManagerConfig() ManagerConfig {
	return ManagerConfig{
		MaxOutboundFull:       8,
		MaxOutboundBlock:      2,
		MaxInbound:            64,
		BanDuration:           24 * time.Hour,
		RequireDistinctGroups: true,
	}
}

var (
	ErrSlotsFull   = errors.New("p2p: the slots are full")
	ErrPeerBanned  = errors.New("p2p: the peer is banned")
	ErrDuplicate   = errors.New("p2p: we are already connected to this peer")
	ErrNoCandidate = errors.New("p2p: there is no peer meeting the conditions")
)

type Manager struct {
	mu    sync.Mutex
	cfg   ManagerConfig
	addrs *AddrMan
	peers map[string]*Peer
	// bans are by machine (HostKey), not by `IP:port` — an incoming connection has a different
	// source port every time and a ban on `IP:port` would be bypassed by a new connection.
	bans  *BanList
	nonce uint64
	now   func() time.Time
	// OnBan is called after every ban (outside the lock) — the node writes the firewall file from it.
	OnBan func()
}

func NewManager(cfg ManagerConfig, addrs *AddrMan, nonce uint64) *Manager {
	return &Manager{
		cfg:   cfg,
		addrs: addrs,
		peers: make(map[string]*Peer),
		bans:  NewBanList(MaxBans),
		nonce: nonce,
		now:   time.Now,
	}
}

// Nonce is this node's random number — it serves to detect a connection to ourselves.
func (m *Manager) Nonce() uint64 { return m.nonce }

// SelectOutbound picks an address for a new outbound connection.
//
// It skips addresses we are already connected to, banned ones, and — this is the important part —
// addresses from a group we already have an outbound connection in.
func (m *Manager) SelectOutbound(kind PeerKind) (Address, error) {
	if !kind.Outbound() {
		return Address{}, errors.New("p2p: SelectOutbound is for outbound connections only")
	}
	m.mu.Lock()
	used := m.outboundGroupsLocked()
	m.mu.Unlock()

	// Feeler connections deliberately go into already used groups too — they verify old addresses
	// and close right away, so they do not block a slot.
	checkGroup := m.cfg.RequireDistinctGroups && kind != PeerFeeler

	for i := 0; i < 64; i++ {
		addr, err := m.addrs.Select(kind == PeerAnchor)
		if err != nil {
			return Address{}, err
		}
		m.mu.Lock()
		_, connected := m.peers[addr.key()]
		banned := m.isBannedLocked(addr)
		m.mu.Unlock()

		if connected || banned {
			continue
		}
		if checkGroup && used[string(addr.group(m.addrs.asn))] {
			continue
		}
		return addr, nil
	}
	return Address{}, ErrNoCandidate
}

// SelectOutboundCandidate is a wrapper over SelectOutbound, so that code keeping the addrman and
// the manager apart can use it too.
func (m *Manager) SelectOutboundCandidate(kind PeerKind) (Address, error) {
	return m.SelectOutbound(kind)
}

// SelectOutboundServices is SelectOutbound limited to addresses seen with every service in `want`
// — a node holding the full DAG looks for others like it this way, because shares only travel
// between those. The spread over network groups holds here too.
func (m *Manager) SelectOutboundServices(kind PeerKind, want uint64) (Address, error) {
	if !kind.Outbound() {
		return Address{}, errors.New("p2p: SelectOutboundServices is for outbound connections only")
	}
	// A snapshot, so the filter runs under the addrman's lock without taking the manager's.
	m.mu.Lock()
	used := m.outboundGroupsLocked()
	connected := make(map[string]bool, len(m.peers))
	for k := range m.peers {
		connected[k] = true
	}
	now := m.now()
	m.mu.Unlock()
	checkGroup := m.cfg.RequireDistinctGroups && kind != PeerFeeler
	return m.addrs.SelectServices(want, func(a Address) bool {
		return connected[a.key()] || m.bans.Banned(HostKey(a), now) ||
			(checkGroup && used[string(a.group(m.addrs.asn))])
	})
}

// CountServices is the number of connections of a kind whose peer gave every service in `want` in
// its handshake.
func (m *Manager) CountServices(kind PeerKind, want uint64) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, p := range m.peers {
		if p.Kind == kind && p.Version != nil && p.Version.Services&want == want {
			n++
		}
	}
	return n
}

// NewestWithout is the youngest connection of a kind whose peer lacks a service in `want` — the
// one to give up for a peer that has them. The youngest, because the old ones have proved
// themselves and an attacker's connections are the new ones.
func (m *Manager) NewestWithout(kind PeerKind, want uint64) *Peer {
	m.mu.Lock()
	defer m.mu.Unlock()
	var newest *Peer
	for _, p := range m.peers {
		if p.Kind != kind || p.Version == nil || p.Version.Services&want == want {
			continue
		}
		if newest == nil || p.Connected.After(newest.Connected) {
			newest = p
		}
	}
	return newest
}

func (m *Manager) outboundGroupsLocked() map[string]bool {
	used := make(map[string]bool)
	for _, p := range m.peers {
		if p.Kind.Outbound() && p.Kind != PeerFeeler {
			used[string(p.Addr.group(m.addrs.asn))] = true
		}
	}
	return used
}

// Add files a new connection into a slot. With full inbound slots it returns a peer to evict.
func (m *Manager) Add(p *Peer) (evict *Peer, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, dup := m.peers[p.Addr.key()]; dup {
		return nil, ErrDuplicate
	}
	if m.isBannedLocked(p.Addr) {
		return nil, ErrPeerBanned
	}

	switch {
	case p.Kind == PeerOutboundFull:
		if m.countLocked(PeerOutboundFull) >= m.cfg.MaxOutboundFull {
			return nil, ErrSlotsFull
		}
	case p.Kind == PeerOutboundBlock || p.Kind == PeerAnchor:
		if m.countLocked(PeerOutboundBlock)+m.countLocked(PeerAnchor) >= m.cfg.MaxOutboundBlock {
			return nil, ErrSlotsFull
		}
	case p.Kind == PeerInbound:
		if m.countLocked(PeerInbound) >= m.cfg.MaxInbound {
			evict = m.evictionCandidateLocked()
			if evict == nil {
				return nil, ErrSlotsFull
			}
			delete(m.peers, evict.Addr.key())
		}
	}

	m.peers[p.Addr.key()] = p
	return evict, nil
}

// evictionCandidateLocked picks an incoming connection to evict.
//
// It protects the spread: it evicts from the **largest** group, and within it the **youngest**
// connection. An attacker connecting with thousands of connections from one network therefore
// pushes out only themselves.
func (m *Manager) evictionCandidateLocked() *Peer {
	byGroup := make(map[string][]*Peer)
	for _, p := range m.peers {
		if p.Kind != PeerInbound {
			continue
		}
		g := string(p.Addr.group(m.addrs.asn))
		byGroup[g] = append(byGroup[g], p)
	}
	var biggest []*Peer
	for _, peers := range byGroup {
		if len(peers) > len(biggest) {
			biggest = peers
		}
	}
	if len(biggest) == 0 {
		return nil
	}
	newest := biggest[0]
	for _, p := range biggest[1:] {
		if p.Connected.After(newest.Connected) {
			newest = p
		}
	}
	return newest
}

func (m *Manager) countLocked(kind PeerKind) int {
	n := 0
	for _, p := range m.peers {
		if p.Kind == kind {
			n++
		}
	}
	return n
}

// Remove takes a connection out of its slot.
func (m *Manager) Remove(addr Address) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.peers, addr.key())
}

// Ban bans the machine a peer came from and disconnects it.
func (m *Manager) Ban(addr Address) {
	m.mu.Lock()
	now := m.now()
	m.bans.Ban(HostKey(addr), now.Add(m.cfg.BanDuration), now)
	delete(m.peers, addr.key())
	hook := m.OnBan
	m.mu.Unlock()
	if hook != nil {
		hook()
	}
}

// ActiveBans returns the valid bans as IP addresses and networks — for the firewall.
func (m *Manager) ActiveBans() []string {
	return m.bans.Active(m.now())
}

// Misbehaving adds to the score and bans the peer once it crosses the threshold.
func (m *Manager) Misbehaving(p *Peer, points int) bool {
	if !p.Misbehaving(points) {
		return false
	}
	m.Ban(p.Addr)
	return true
}

func (m *Manager) IsBanned(addr Address) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.isBannedLocked(addr)
}

func (m *Manager) isBannedLocked(addr Address) bool {
	return m.bans.Banned(HostKey(addr), m.now())
}

// Count returns the number of connections of a given kind.
func (m *Manager) Count(kind PeerKind) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.countLocked(kind)
}

// PeerHeights returns the number of peers and the highest height any of them reported in the
// handshake.
//
// It is an **estimate of the sync target**: otherwise the wallet has no way to say "I am at 40 %",
// because it knows its own tip but not how far the chain reaches. The number comes from strangers,
// so it cannot be relied on for consensus — for a progress indicator it is enough.
func (m *Manager) PeerHeights() (count int, best uint64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, p := range m.peers {
		count++
		if h := p.KnownHeight(); h > best {
			best = h
		}
	}
	return count, best
}

// OutboundGroups returns the number of distinct groups the outbound connections lead into.
// This number is a direct measure of resilience against an eclipse attack.
func (m *Manager) OutboundGroups() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.outboundGroupsLocked())
}

// Anchors returns the addresses to store for the next startup.
//
// After a restart the node connects to them first — an attacker who flooded the addrman in the
// meantime therefore does not get every slot. Only block-relay connections are taken, because they
// do not exchange addresses.
func (m *Manager) Anchors(max int) []Address {
	m.mu.Lock()
	defer m.mu.Unlock()

	var candidates []*Peer
	for _, p := range m.peers {
		if (p.Kind == PeerOutboundBlock || p.Kind == PeerAnchor) && p.HandshakeDone() {
			candidates = append(candidates, p)
		}
	}
	// The longest-held connections first.
	for i := 1; i < len(candidates); i++ {
		for j := i; j > 0 && candidates[j].Connected.Before(candidates[j-1].Connected); j-- {
			candidates[j], candidates[j-1] = candidates[j-1], candidates[j]
		}
	}
	out := make([]Address, 0, max)
	for _, p := range candidates {
		if len(out) >= max {
			break
		}
		out = append(out, p.Addr)
	}
	return out
}
