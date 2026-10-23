package p2p

import (
	"encoding/hex"
	"net"
	"sort"
	"sync"
	"time"
)

// Protection against flooding — designed to work even when an attacker reads this code.
//
// Nothing here rests on a secret. It rests on three rules:
//
//  1. **The key is the machine, not the connection.** Bans and limits go on an IPv4 address, on an
//     IPv6 /64 network (that is what one machine gets from a provider) and on an onion address —
//     not on `IP:port`. An incoming connection has a new source port every time; a ban on
//     `IP:port` would be bypassed by the banned peer's next connection.
//  2. **Everything an attacker can fill has a fixed cap.** The ban list, the group tracking for the
//     rate limit, the connection counts. An attacker with a million addresses inflates the cap at most.
//  3. **The limits apply to network groups too**, not only to individual addresses — otherwise it
//     would be enough to have many addresses in one network.

// HostKey is a machine's identity for bans and limits.
func HostKey(a Address) string {
	if a.Onion != "" {
		return "o:" + a.Onion
	}
	if v4 := a.IP.To4(); v4 != nil {
		return "4:" + v4.String()
	}
	if v6 := a.IP.To16(); v6 != nil {
		return "6:" + hex.EncodeToString(v6[:8]) // /64
	}
	return "?"
}

// isLoopback — connections from our own machine have no limits (local tools, tests).
// Connecting from loopback from the outside is not possible.
func isLoopback(a Address) bool {
	return a.Onion == "" && a.IP != nil && a.IP.IsLoopback()
}

// ---------------------------------------------------------------- token bucket

// TokenBucket is a plain token bucket with injectable clocks.
type TokenBucket struct {
	rate   float64 // tokens per second
	burst  float64
	tokens float64
	last   time.Time
}

func NewTokenBucket(rate, burst float64, now time.Time) *TokenBucket {
	return &TokenBucket{rate: rate, burst: burst, tokens: burst, last: now}
}

// Take removes n tokens if there are enough of them.
func (b *TokenBucket) Take(n float64, now time.Time) bool {
	if dt := now.Sub(b.last).Seconds(); dt > 0 {
		b.tokens += dt * b.rate
		if b.tokens > b.burst {
			b.tokens = b.burst
		}
		b.last = now
	}
	if b.tokens < n {
		return false
	}
	b.tokens -= n
	return true
}

// ---------------------------------------------------------------- ban list

// MaxBans is the cap on the ban list. With a full list, an expired ban goes first, then the one that
// would expire soonest.
const MaxBans = 65_536

// BanList are the bans by HostKey. Safe for concurrent use.
type BanList struct {
	mu    sync.Mutex
	until map[string]time.Time
	max   int
}

func NewBanList(max int) *BanList {
	if max <= 0 {
		max = MaxBans
	}
	return &BanList{until: make(map[string]time.Time), max: max}
}

// Ban bans a machine until a given time. It does not shorten an existing ban.
func (b *BanList) Ban(key string, until time.Time, now time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if cur, ok := b.until[key]; ok && cur.After(until) {
		return
	}
	if _, ok := b.until[key]; !ok && len(b.until) >= b.max {
		b.evictLocked(now)
	}
	b.until[key] = until
}

// evictLocked makes room: it first throws out everything expired, and if that is not enough, the
// ban that would expire soonest. It never throws out anything an attacker can deliberately keep
// alive longer than honest bans — all of them have the same length.
func (b *BanList) evictLocked(now time.Time) {
	for k, t := range b.until {
		if !now.Before(t) {
			delete(b.until, k)
		}
	}
	if len(b.until) < b.max {
		return
	}
	var oldestKey string
	var oldest time.Time
	for k, t := range b.until {
		if oldestKey == "" || t.Before(oldest) {
			oldestKey, oldest = k, t
		}
	}
	delete(b.until, oldestKey)
}

// Banned says whether a machine is banned. It deletes an expired ban on the way.
func (b *BanList) Banned(key string, now time.Time) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	t, ok := b.until[key]
	if !ok {
		return false
	}
	if !now.Before(t) {
		delete(b.until, key)
		return false
	}
	return true
}

// Len is the number of bans (including expired ones not yet deleted).
func (b *BanList) Len() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.until)
}

// Active returns the valid bans as IP addresses / networks — for the firewall. It leaves out onion
// addresses, which the firewall does not know. Sorted, so the file does not change without a reason.
func (b *BanList) Active(now time.Time) []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]string, 0, len(b.until))
	for k, t := range b.until {
		if !now.Before(t) {
			continue
		}
		switch {
		case len(k) > 2 && k[:2] == "4:":
			out = append(out, k[2:])
		case len(k) > 2 && k[:2] == "6:":
			raw, err := hex.DecodeString(k[2:])
			if err != nil || len(raw) != 8 {
				continue
			}
			ip := make(net.IP, 16)
			copy(ip, raw)
			out = append(out, ip.String()+"/64")
		}
	}
	sort.Strings(out)
	return out
}

// ---------------------------------------------------------------- the entry gate

// GateConfig are the limits on incoming connections **before the handshake**.
type GateConfig struct {
	MaxPerHost  int     // connections from one machine (IPv4 / IPv6 /64)
	MaxPerGroup int     // connections from one network group (ASN, otherwise /16 or /32)
	MaxPending  int     // connections that have not finished the handshake yet
	AcceptRate  float64 // new connections per second in total
	AcceptBurst float64
	GroupRate   float64 // new connections per second from one group
	GroupBurst  float64
	MaxTracked  int // cap on the groups tracked for the rate limit
}

func DefaultGateConfig() GateConfig {
	return GateConfig{
		MaxPerHost:  2,
		MaxPerGroup: 8,
		MaxPending:  16,
		AcceptRate:  10,
		AcceptBurst: 40,
		GroupRate:   0.5,
		GroupBurst:  4,
		MaxTracked:  4096,
	}
}

// Gate decides about incoming connections before they cost anything expensive.
type Gate struct {
	mu      sync.Mutex
	cfg     GateConfig
	asn     ASNLookup
	global  *TokenBucket
	groups  map[string]*groupState
	hosts   map[string]int
	pending int
}

type groupState struct {
	bucket *TokenBucket
	conns  int
	seen   time.Time
}

func NewGate(cfg GateConfig, asn ASNLookup, now time.Time) *Gate {
	return &Gate{
		cfg:    cfg,
		asn:    asn,
		global: NewTokenBucket(cfg.AcceptRate, cfg.AcceptBurst, now),
		groups: make(map[string]*groupState),
		hosts:  make(map[string]int),
	}
}

// The reasons for refusal — for the log and the tests.
const (
	GateOK         = ""
	GateRateGlobal = "too many new connections in total"
	GateRateGroup  = "too many new connections from one network"
	GatePending    = "too many unfinished handshakes"
	GatePerHost    = "too many connections from one machine"
	GatePerGroup   = "too many connections from one network"
)

// Ticket holds a place in the gate while the connection lives.
type Ticket struct {
	g        *Gate
	host     string
	group    string
	pending  bool
	released bool
	exempt   bool
}

// Admit decides about a new connection. On success it returns a ticket that has to be released
// (`Established` after the handshake, `Release` on close).
func (g *Gate) Admit(a Address, now time.Time) (*Ticket, string) {
	if isLoopback(a) {
		return &Ticket{g: g, exempt: true}, GateOK
	}
	host := HostKey(a)
	group := string(a.group(g.asn))

	g.mu.Lock()
	defer g.mu.Unlock()

	if g.pending >= g.cfg.MaxPending {
		return nil, GatePending
	}
	if g.hosts[host] >= g.cfg.MaxPerHost {
		return nil, GatePerHost
	}
	gs := g.groupLocked(group, now)
	if gs.conns >= g.cfg.MaxPerGroup {
		return nil, GatePerGroup
	}
	// The group first, then the overall limit: an attacker from one network therefore does not
	// exhaust the global limit and cut everybody else off — they hit their own first.
	if !gs.bucket.Take(1, now) {
		return nil, GateRateGroup
	}
	if !g.global.Take(1, now) {
		return nil, GateRateGlobal
	}
	g.pending++
	g.hosts[host]++
	gs.conns++
	gs.seen = now
	return &Ticket{g: g, host: host, group: group, pending: true}, GateOK
}

// groupLocked returns a group's state. With a full list it throws out the group with no connections
// that has not been seen for the longest — groups with live connections are never thrown out,
// otherwise their limits could be bypassed by pushing them off the list.
func (g *Gate) groupLocked(group string, now time.Time) *groupState {
	if gs, ok := g.groups[group]; ok {
		return gs
	}
	if len(g.groups) >= g.cfg.MaxTracked {
		var victim string
		var oldest time.Time
		for k, gs := range g.groups {
			if gs.conns > 0 {
				continue
			}
			if victim == "" || gs.seen.Before(oldest) {
				victim, oldest = k, gs.seen
			}
		}
		if victim != "" {
			delete(g.groups, victim)
		}
	}
	gs := &groupState{bucket: NewTokenBucket(g.cfg.GroupRate, g.cfg.GroupBurst, now), seen: now}
	g.groups[group] = gs
	return gs
}

// Established moves a connection from unfinished to established.
func (t *Ticket) Established() {
	if t == nil || t.exempt {
		return
	}
	t.g.mu.Lock()
	defer t.g.mu.Unlock()
	if t.pending && !t.released {
		t.pending = false
		t.g.pending--
	}
}

// Release frees the place. It can be called several times.
func (t *Ticket) Release() {
	if t == nil || t.exempt {
		return
	}
	t.g.mu.Lock()
	defer t.g.mu.Unlock()
	if t.released {
		return
	}
	t.released = true
	if t.pending {
		t.g.pending--
	}
	if t.g.hosts[t.host]--; t.g.hosts[t.host] <= 0 {
		delete(t.g.hosts, t.host)
	}
	if gs, ok := t.g.groups[t.group]; ok {
		gs.conns--
	}
}

// Stats are the counts for tests and monitoring.
func (g *Gate) Stats() (pending, hosts, groups int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.pending, len(g.hosts), len(g.groups)
}

// ---------------------------------------------------------------- the per-peer limit

// PeerLimits bound how much one peer may send. An honest peer does not come close to them. The
// most frequent legitimate message is a share: the network finds 100 a second and a peer relays
// every one it verified, more while a jump in hashrate waits for the retarget (an hour's half-life)
// — so the message rate leaves room for five times that, besides the pings, transactions and
// attestations. The largest message is a block with its settlement body (chain.MaxBlockWireBytes),
// one every 30 s.
type PeerLimits struct {
	MsgRate, MsgBurst     float64 // messages per second
	BytesRate, BytesBurst float64 // bytes per second
}

func InboundPeerLimits() PeerLimits {
	return PeerLimits{MsgRate: 600, MsgBurst: 3000, BytesRate: 2 << 20, BytesBurst: 8 << 20}
}

// OutboundPeerLimits are looser: we download blocks from these peers ourselves, and fast during a sync.
func OutboundPeerLimits() PeerLimits {
	return PeerLimits{MsgRate: 2400, MsgBurst: 12000, BytesRate: 32 << 20, BytesBurst: 64 << 20}
}

// PeerMeter measures one peer.
type PeerMeter struct {
	msgs, bytes *TokenBucket
}

func NewPeerMeter(l PeerLimits, now time.Time) *PeerMeter {
	return &PeerMeter{
		msgs:  NewTokenBucket(l.MsgRate, l.MsgBurst, now),
		bytes: NewTokenBucket(l.BytesRate, l.BytesBurst, now),
	}
}

// Allow accounts for a received message. False = the peer sends more than an honest peer ever sends.
func (m *PeerMeter) Allow(size int, now time.Time) bool {
	okMsg := m.msgs.Take(1, now)
	okBytes := m.bytes.Take(float64(size), now)
	return okMsg && okBytes
}
