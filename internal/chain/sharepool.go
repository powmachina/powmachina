package chain

import (
	"bytes"
	"slices"
	"sync"
)

// SharePool keeps the verified shares that a block could still settle (the Sharepool spec §1):
// a full node holds the shares of the last ShareExpiry blocks, with the PoW hash it computed for
// each, so a block settling them costs a lookup and not a hash.
//
// It is a pool, not a judge: what is in it passed CheckShare when it arrived. Which of them a block
// may still include depends on the branch, and the template asks for that with Candidates.
type SharePool struct {
	mu     sync.Mutex
	max    int
	shares map[[32]byte]*pooledShare
	// votes are the share units built on each block — what the fork choice between two siblings
	// counts (the Sharepool spec §4).
	votes map[[32]byte]uint64
}

type pooledShare struct {
	share *Share
	hash  [32]byte
}

// DefaultSharePoolSize is four blocks' worth of settlements at the cap: enough for the expiry at
// a hundred shares a second with room for bursts, ~1 MB of memory.
const DefaultSharePoolSize = 4 * MaxSettlementShares

// NewSharePool makes a pool of at most `max` shares.
func NewSharePool(max int) *SharePool {
	return &SharePool{max: max, shares: make(map[[32]byte]*pooledShare), votes: make(map[[32]byte]uint64)}
}

// Has says whether the pool has a share.
func (p *SharePool) Has(id [32]byte) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	_, ok := p.shares[id]
	return ok
}

// Add files a verified share with its PoW hash. It returns false for a share the pool already has
// or has no room for — a full pool keeps what it has: the oldest shares leave with Expire, not
// with whoever sends the most.
func (p *SharePool) Add(s *Share, hash [32]byte) bool {
	id := s.ID()
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, ok := p.shares[id]; ok || len(p.shares) >= p.max {
		return false
	}
	p.shares[id] = &pooledShare{share: s, hash: hash}
	p.votes[s.Header.PrevBlock] += uint64(1) << s.Commit.Weight
	return true
}

// ShareTally is how much of something one payout program has: shares and their units.
type ShareTally struct {
	Shares int
	Units  uint64
}

// Tally sums the pool by payout program.
func (p *SharePool) Tally() map[[32]byte]ShareTally { return p.TallyWhere(nil) }

// TallyWhere sums the shares `keep` says yes to (by id and parent; nil keeps all) by payout program.
func (p *SharePool) TallyWhere(keep func(id, prev [32]byte) bool) map[[32]byte]ShareTally {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make(map[[32]byte]ShareTally)
	for id, ps := range p.shares {
		if keep != nil && !keep(id, ps.share.Header.PrevBlock) {
			continue
		}
		t := out[ps.share.Commit.Payout]
		t.Shares++
		t.Units += ps.share.Commit.Units()
		out[ps.share.Commit.Payout] = t
	}
	return out
}

// Votes are the share units in the pool built on block `id`.
func (p *SharePool) Votes(id [32]byte) uint64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.votes[id]
}

// Len is the number of shares in the pool.
func (p *SharePool) Len() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.shares)
}

// Expire drops the shares the block after `tipHeight` could no longer include: its depth for that
// block, tipHeight + 1 - (height - 1), would be over ShareExpiry.
func (p *SharePool) Expire(tipHeight uint64) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	dropped := 0
	for id, ps := range p.shares {
		if uint64(ps.share.Header.Height)+ShareExpiry <= tipHeight+1 {
			delete(p.shares, id)
			prev := ps.share.Header.PrevBlock
			if p.votes[prev] -= uint64(1) << ps.share.Commit.Weight; p.votes[prev] == 0 {
				delete(p.votes, prev)
			}
			dropped++
		}
	}
	return dropped
}

// PoWHash answers a share's hash from the pool, and computes it with `fallback` for one the pool
// does not have. The id of a header commits to every byte the hash is computed over, so a hash
// found by id is the hash of that exact header.
func (p *SharePool) PoWHash(fallback PoWHasher) PoWHasher {
	return poolHasher{p, fallback}
}

type poolHasher struct {
	p        *SharePool
	fallback PoWHasher
}

func (h poolHasher) PoWHash(hdr *Header) ([32]byte, error) {
	id := hdr.ID()
	h.p.mu.Lock()
	ps, ok := h.p.shares[id]
	h.p.mu.Unlock()
	if ok {
		return ps.hash, nil
	}
	return h.fallback.PoWHash(hdr)
}

// Candidates returns the shares the next block on `parent` may settle: those built on a block
// `onBranch` names (the parent and the blocks below it within the expiry), not `settled` by one of
// them, at most `max`. Shares of the oldest parents come first — they are the ones about to expire.
func (p *SharePool) Candidates(onBranch func(prev [32]byte) bool, settled func(id [32]byte) bool, max int) []*Share {
	p.mu.Lock()
	all := make([]*Share, 0, len(p.shares))
	for id, ps := range p.shares {
		if !onBranch(ps.share.Header.PrevBlock) || (settled != nil && settled(id)) {
			continue
		}
		all = append(all, ps.share)
	}
	p.mu.Unlock()
	// Oldest first; within a height the order does not matter, only that it is deterministic.
	sortSharesByHeight(all)
	if len(all) > max {
		all = all[:max]
	}
	return all
}

func sortSharesByHeight(s []*Share) {
	type keyed struct {
		h  uint32
		id [32]byte
		s  *Share
	}
	k := make([]keyed, len(s))
	for i, sh := range s {
		k[i] = keyed{sh.Header.Height, sh.ID(), sh}
	}
	slices.SortFunc(k, func(a, b keyed) int {
		if a.h != b.h {
			if a.h < b.h {
				return -1
			}
			return 1
		}
		return bytes.Compare(a.id[:], b.id[:])
	})
	for i := range k {
		s[i] = k[i].s
	}
}
