package chain

import "errors"

// Parked blocks.
//
// # Why this has to exist
//
// The tip moves on a header alone: the PoW is in the header, and a node that waited for the body
// before telling anybody would slow every block down. But a header says nothing about whether its
// body will ever come. A miner that announces a header and keeps the body leaves every node with a
// tip nothing can be applied to — and every honest miner building on a block nobody can validate.
//
// A block whose body does not come is therefore parked: it stays in the chain, its body keeps being
// asked for, headers that grow out of it keep being accepted, but neither it nor anything above it
// can be the tip. The tip falls back to the heaviest branch that is not parked, and mining goes on
// there. When the body arrives, the block is unparked and competes for the tip like any other
// branch, under the same reorg rules.
//
// Unlike an invalid mark, a parked one is a guess about the network, not a fact about the block. It
// lives in memory only and says nothing to peers.

// Park takes a block out of the running for the tip and returns the new tip.
func (hc *HeaderChain) Park(id [32]byte) (newTip [32]byte, changed bool, err error) {
	hc.mu.Lock()
	defer hc.mu.Unlock()

	if id == hc.genesis {
		return hc.tip.id, false, errors.New("chain: the genesis cannot be parked")
	}
	if _, ok := hc.byID[id]; !ok {
		return hc.tip.id, false, ErrUnknownParent
	}
	if hc.parked[id] {
		return hc.tip.id, false, nil
	}
	if hc.parked == nil {
		hc.parked = make(map[[32]byte]bool)
	}
	hc.parked[id] = true

	old := hc.tip
	hc.tip = hc.bestValidLocked()
	return hc.tip.id, hc.tip != old, nil
}

// Unpark lets a block compete for the tip again. It reports whether the tip moved.
//
// The branch does not get the tip back for free: it has to win it the way a competing branch does,
// through maybeReorgLocked — heavier, within the reorg limit, not below finality.
func (hc *HeaderChain) Unpark(id [32]byte) (bool, error) {
	hc.mu.Lock()
	defer hc.mu.Unlock()
	if !hc.parked[id] {
		return false, nil
	}
	delete(hc.parked, id)
	best := hc.bestValidLocked()
	if best == hc.tip || best.work.Cmp(hc.tip.work) <= 0 {
		return false, nil
	}
	return hc.maybeReorgLocked(best)
}

// IsParked says whether this very block is parked (not whether it grows out of one).
func (hc *HeaderChain) IsParked(id [32]byte) bool {
	hc.mu.RLock()
	defer hc.mu.RUnlock()
	return hc.parked[id]
}

// ParkedIDs returns the parked blocks — their bodies are what the node keeps asking for.
func (hc *HeaderChain) ParkedIDs() [][32]byte {
	hc.mu.RLock()
	defer hc.mu.RUnlock()
	out := make([][32]byte, 0, len(hc.parked))
	for id := range hc.parked {
		out = append(out, id)
	}
	return out
}

// hasParkedAncestorLocked says whether a node is parked or grows out of a parked block. With nothing
// parked — the ordinary state — it costs one length check; otherwise it walks down only as far as the
// lowest parked block.
func (hc *HeaderChain) hasParkedAncestorLocked(n *node) bool {
	if len(hc.parked) == 0 {
		return false
	}
	lowest := n.height
	for id := range hc.parked {
		if p, ok := hc.byID[id]; ok && p.height < lowest {
			lowest = p.height
		}
	}
	for cur := n; cur != nil && cur.height >= lowest; cur = hc.byID[cur.header.PrevBlock] {
		if hc.parked[cur.id] {
			return true
		}
		if cur.id == hc.genesis {
			break
		}
	}
	return false
}
