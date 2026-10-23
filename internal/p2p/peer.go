package p2p

import (
	"encoding/binary"
	"errors"
	"sync"
	"time"
)

// The protocol version and the handshake.
//
// The handshake is the first thing a peer sends, so it has to be resilient on its own: fixed caps,
// no dynamic allocations based on numbers from the peer, and detection of a connection to ourselves.

const (
	// ProtocolVersion is the P2P protocol version (independent of the consensus version).
	ProtocolVersion uint32 = 1
	// MinProtocolVersion is the lowest version we still talk to.
	MinProtocolVersion uint32 = 1
	// MaxUserAgentBytes keeps the user agent short — it is information only, not room for data.
	MaxUserAgentBytes = 64
	// HandshakeTimeout is the time the handshake has to be finished within.
	HandshakeTimeout = 20 * time.Second
	// FeelerInterval is how often the node tries one address from the new table.
	// The same value as in Bitcoin Core — often enough to maintain the tried table, rarely enough
	// not to look like scanning.
	FeelerInterval = 2 * time.Minute
	// OutboundInterval is how often the node fills missing outbound connections.
	OutboundInterval = 10 * time.Second
)

// The services a peer offers.
const (
	ServiceFullNode    uint64 = 1 << 0 // has the whole chain
	ServiceFullDag     uint64 = 1 << 1 // holds the full DAG, can verify PoW fast
	ServiceBlockRelay  uint64 = 1 << 2 // relays blocks
	ServiceAttestation uint64 = 1 << 3 // spreads finality attestations
)

var (
	ErrSelfConnection = errors.New("p2p: a connection to ourselves")
	ErrOldProtocol    = errors.New("p2p: the peer's protocol version is too old")
	ErrBadVersionMsg  = errors.New("p2p: a corrupted version message")
)

// VersionMsg is the content of the handshake message.
type VersionMsg struct {
	Protocol    uint32
	Services    uint64
	Timestamp   int64
	Nonce       uint64 // a random number to detect a connection to ourselves
	UserAgent   string
	StartHeight uint32
	Relay       bool
}

func (v *VersionMsg) Encode() ([]byte, error) {
	if len(v.UserAgent) > MaxUserAgentBytes {
		return nil, ErrBadVersionMsg
	}
	out := make([]byte, 0, 33+len(v.UserAgent))
	var b [8]byte
	binary.LittleEndian.PutUint32(b[:4], v.Protocol)
	out = append(out, b[:4]...)
	binary.LittleEndian.PutUint64(b[:], v.Services)
	out = append(out, b[:]...)
	binary.LittleEndian.PutUint64(b[:], uint64(v.Timestamp))
	out = append(out, b[:]...)
	binary.LittleEndian.PutUint64(b[:], v.Nonce)
	out = append(out, b[:]...)
	out = append(out, byte(len(v.UserAgent)))
	out = append(out, v.UserAgent...)
	binary.LittleEndian.PutUint32(b[:4], v.StartHeight)
	out = append(out, b[:4]...)
	if v.Relay {
		out = append(out, 1)
	} else {
		out = append(out, 0)
	}
	return out, nil
}

// DecodeVersion reads the handshake from a foreign peer — that is, untrusted bytes.
func DecodeVersion(payload []byte) (*VersionMsg, error) {
	// 4 + 8 + 8 + 8 + 1 (the UA length) + 4 + 1 = 34 B minimum
	if len(payload) < 34 {
		return nil, ErrBadVersionMsg
	}
	v := &VersionMsg{
		Protocol:  binary.LittleEndian.Uint32(payload[0:4]),
		Services:  binary.LittleEndian.Uint64(payload[4:12]),
		Timestamp: int64(binary.LittleEndian.Uint64(payload[12:20])),
		Nonce:     binary.LittleEndian.Uint64(payload[20:28]),
	}
	uaLen := int(payload[28])
	if uaLen > MaxUserAgentBytes || len(payload) != 28+1+uaLen+4+1 {
		return nil, ErrBadVersionMsg
	}
	ua := payload[29 : 29+uaLen]
	for _, c := range ua {
		if c < 0x20 || c > 0x7e {
			return nil, ErrBadVersionMsg // the user agent is text, not room for binary data
		}
	}
	v.UserAgent = string(ua)
	v.StartHeight = binary.LittleEndian.Uint32(payload[29+uaLen : 33+uaLen])
	switch payload[33+uaLen] {
	case 0:
		v.Relay = false
	case 1:
		v.Relay = true
	default:
		return nil, ErrBadVersionMsg // the only valid form
	}
	return v, nil
}

// PeerKind says what role a connection fills. Splitting into types is a defence against an eclipse:
// block-relay-only peers do not exchange addresses, so an attacker cannot influence them via `addr`.
type PeerKind int

const (
	PeerInbound       PeerKind = iota // came to us
	PeerOutboundFull                  // we dialled it, full exchange
	PeerOutboundBlock                 // we dialled it, blocks only (no addresses, no tx)
	PeerFeeler                        // a short verification connection, closed right away
	PeerAnchor                        // a verified peer we connect to after a restart
)

func (k PeerKind) String() string {
	switch k {
	case PeerInbound:
		return "inbound"
	case PeerOutboundFull:
		return "outbound"
	case PeerOutboundBlock:
		return "blocks only"
	case PeerFeeler:
		return "feeler"
	case PeerAnchor:
		return "anchor"
	default:
		return "unknown"
	}
}

func (k PeerKind) Outbound() bool {
	return k == PeerOutboundFull || k == PeerOutboundBlock || k == PeerFeeler || k == PeerAnchor
}

// Ban scoring. Small things are not banned right away — otherwise a ban could be abused to cut off
// honest peers (and with mining pools that really happens).
const (
	BanThreshold = 100

	MisbehaviorBadMessage   = 10  // a corrupted or unknown message
	MisbehaviorTooManyItems = 20  // a list over the cap
	MisbehaviorBadHeader    = 50  // a header that failed validation
	MisbehaviorInvalidBlock = 100 // a block violating consensus — an instant ban
	// MisbehaviorBadCopy: a body that matches its header and fails validation on its witnesses. The
	// header does not cover those, so the block may be fine and the copy tampered with on the way.
	// Not an instant ban: a peer may have passed on what it was handed.
	MisbehaviorBadCopy = 50
	// MisbehaviorBadPoW is a header with invalid PoW that passed every cheap check.
	// An instant ban: an honest peer cannot send it (it only forwards verified headers), and every
	// such header costs us a RandomX computation or a walk through the DAG — otherwise an attacker
	// would send two per connection and repeat it from further addresses.
	MisbehaviorBadPoW = 100
	// MisbehaviorFlood is a peer sending more than an honest peer ever sends.
	MisbehaviorFlood = 50
)

// Peer is the state of one connection.
type Peer struct {
	mu sync.Mutex

	Addr      Address
	Kind      PeerKind
	Version   *VersionMsg
	Connected time.Time

	handshakeDone bool
	banScore      int
	lastAddrMsg   time.Time
	addrTokens    float64
	sentAddrs     bool // whether it was already answered a getaddr
	// knownHeight is how far the peer's chain reaches as far as we can tell. The handshake
	// seeds it from version.StartHeight and every headers message afterwards raises it. Reading
	// StartHeight alone is not enough: it is the height at the moment of connecting and it is
	// never updated, so after a few hours of uptime a node reports a chain shorter than its
	// own and no progress indicator can be computed from it. Consensus never uses this — it
	// comes from strangers.
	knownHeight uint64
}

func NewPeer(addr Address, kind PeerKind) *Peer {
	return &Peer{Addr: addr, Kind: kind, Connected: time.Now(), addrTokens: addrBurst}
}

// CompleteHandshake verifies the version from a peer and closes the handshake.
func (p *Peer) CompleteHandshake(v *VersionMsg, ourNonce uint64) error {
	if v.Nonce == ourNonce {
		return ErrSelfConnection
	}
	if v.Protocol < MinProtocolVersion {
		return ErrOldProtocol
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.Version = v
	p.handshakeDone = true
	if v.StartHeight > 0 {
		p.knownHeight = uint64(v.StartHeight)
	}
	return nil
}

// NoteHeight raises the height we believe the peer's chain reaches. It only ever grows — a
// headers message about an older part of the chain must not shorten the peer.
func (p *Peer) NoteHeight(h uint64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if h > p.knownHeight {
		p.knownHeight = h
	}
}

// KnownHeight is how far the peer's chain reaches as far as we can tell.
func (p *Peer) KnownHeight() uint64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.knownHeight
}

func (p *Peer) HandshakeDone() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.handshakeDone
}

// Misbehaving adds to the ban score and returns true when the peer should be banned.
func (p *Peer) Misbehaving(points int) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.banScore += points
	return p.banScore >= BanThreshold
}

func (p *Peer) BanScore() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.banScore
}

// The rate limit for `addr` messages: without it, flooding with addresses is the cheapest way to
// fill the table and set up an eclipse.
const (
	addrBurst      = 1000 // how many addresses a peer may send at once
	addrRefillRate = 0.1  // and how many it gains per second (≈ 360/h)
)

// FirstGetAddr says whether this is the peer's first getaddr. Only the first is answered: asking
// again and again would copy out the whole table a sample at a time.
func (p *Peer) FirstGetAddr() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	first := !p.sentAddrs
	p.sentAddrs = true
	return first
}

// AllowAddrs permits processing n addresses from this peer; it returns how many should be accepted.
func (p *Peer) AllowAddrs(n int, now time.Time) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.lastAddrMsg.IsZero() {
		p.addrTokens += now.Sub(p.lastAddrMsg).Seconds() * addrRefillRate
		if p.addrTokens > addrBurst {
			p.addrTokens = addrBurst
		}
	}
	p.lastAddrMsg = now
	if float64(n) > p.addrTokens {
		n = int(p.addrTokens)
	}
	p.addrTokens -= float64(n)
	return n
}
