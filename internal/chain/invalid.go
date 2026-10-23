package chain

import (
	"errors"
	"math/big"
)

// Invalid branches.
//
// # Why this has to exist
//
// A header is verified for PoW, difficulty, times and lane rotation — **not for the validity of
// its transactions**. Those can only be verified in the context of the UTXO set, that is, when the
// block's turn comes. A block with valid PoW and one invalid transaction therefore passes as a
// header, its body matches it (the merkle root is in the header), and it only fails on apply.
//
// Without marking the branch invalid the node **stops for good** at that moment: applying fails at
// that height, higher bodies have nothing to attach to and nothing brings it back. And since such
// a header spreads like any other, the whole network stops. The cost to the attacker is one block
// of work — which on testnet is almost nothing.
//
// # What invalidation does
//
//  1. marks the id invalid (and with it everything that grows out of it — a child of an invalid
//     block is invalid, no matter how heavy it is),
//  2. picks a new tip: **the heaviest node with no invalid ancestor**,
//  3. the node stores the mark, so after a restart or after another peer sends it the same header,
//     it refuses it right away — without downloading the body and without stalling again.

// ErrInvalidAncestor is a header that grows out of a block marked invalid.
var ErrInvalidAncestor = errors.New("chain: the header grows out of an invalid block")

// Invalidate marks a block invalid and returns the new tip of the active branch.
//
// The genesis cannot be invalidated — it is the root of trust from the binary, not a block from the
// network.
func (hc *HeaderChain) Invalidate(id [32]byte) (newTip [32]byte, changed bool, err error) {
	hc.mu.Lock()
	defer hc.mu.Unlock()

	if id == hc.genesis {
		return hc.tip.id, false, errors.New("chain: the genesis cannot be marked invalid")
	}
	if hc.invalid == nil {
		hc.invalid = make(map[[32]byte]bool)
	}
	if hc.invalid[id] {
		return hc.tip.id, false, nil
	}
	hc.invalid[id] = true
	hc.retaintLocked()

	old := hc.tip
	hc.tip = hc.bestValidLocked()
	return hc.tip.id, hc.tip != old, nil
}

// MarkInvalidLoaded inserts a mark read from storage. It does not compute a new tip — at startup
// the chain is assembled only afterwards.
func (hc *HeaderChain) MarkInvalidLoaded(id [32]byte) {
	hc.mu.Lock()
	defer hc.mu.Unlock()
	if hc.invalid == nil {
		hc.invalid = make(map[[32]byte]bool)
	}
	hc.invalid[id] = true
	hc.retaintLocked()
}

// IsInvalid says whether a block is marked invalid or grows out of one.
func (hc *HeaderChain) IsInvalid(id [32]byte) bool {
	hc.mu.RLock()
	defer hc.mu.RUnlock()
	if n, ok := hc.byID[id]; ok {
		return n.tainted
	}
	return hc.invalid[id]
}

// InvalidIDs returns the marked blocks — for printing and for storing.
func (hc *HeaderChain) InvalidIDs() [][32]byte {
	hc.mu.RLock()
	defer hc.mu.RUnlock()
	out := make([][32]byte, 0, len(hc.invalid))
	for id := range hc.invalid {
		out = append(out, id)
	}
	return out
}

// bestValidLocked picks the heaviest node that does not grow out of an invalid block or out of a
// parked one.
//
// It walks every known node — this only happens on an invalidation, not in ordinary operation.
// With equal work the lower id decides, so the result is the same on every node.
func (hc *HeaderChain) bestValidLocked() *node {
	best := hc.byID[hc.genesis]
	for _, n := range hc.byID {
		if n.tainted || hc.hasParkedAncestorLocked(n) {
			continue
		}
		switch n.work.Cmp(best.work) {
		case 1:
			best = n
		case 0:
			if lessID(n.id, best.id) {
				best = n
			}
		}
	}
	return best
}

// retaintLocked recomputes `tainted` on every node after a mark was added. It walks each node at
// most once (the memo), and it only runs on an invalidation or when the marks are loaded.
func (hc *HeaderChain) retaintLocked() {
	memo := make(map[[32]byte]bool, len(hc.byID))
	for _, n := range hc.byID {
		n.tainted = hc.hasInvalidAncestorLocked(n, memo)
	}
}

// hasInvalidAncestorLocked walks down from a node until it hits an invalid block, the genesis or a
// node whose answer it already knows (memo).
func (hc *HeaderChain) hasInvalidAncestorLocked(n *node, memo map[[32]byte]bool) bool {
	var path [][32]byte
	cur := n
	bad := false
	for cur != nil {
		if v, ok := memo[cur.id]; ok {
			bad = v
			break
		}
		if hc.invalid[cur.id] {
			bad = true
			break
		}
		if cur.id == hc.genesis {
			break
		}
		path = append(path, cur.id)
		cur = hc.byID[cur.header.PrevBlock]
	}
	for _, id := range path {
		memo[id] = bad
	}
	return bad
}

func lessID(a, b [32]byte) bool {
	for i := range a {
		if a[i] != b[i] {
			return a[i] < b[i]
		}
	}
	return false
}

// workOf is a helper for tests and diagnostics: a node's cumulative chainwork.
func (hc *HeaderChain) workOf(id [32]byte) *big.Int {
	hc.mu.RLock()
	defer hc.mu.RUnlock()
	if n, ok := hc.byID[id]; ok {
		return new(big.Int).Set(n.work)
	}
	return nil
}
