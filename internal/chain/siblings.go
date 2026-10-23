package chain

// Choosing between two blocks on the same parent (the Sharepool spec §4).
//
// Two siblings happen whenever two closing shares are found within the propagation time, and every
// share found after that builds on one of them — it votes. A node prefers the sibling with the
// lower block ID unless the other leads by more than SiblingVoteMargin share units of votes in
// what it has seen. "More votes wins" alone lets a miner with a quarter of the hashrate keep the
// fork open by voting for whichever half is about to switch; a tie-break that every node computes
// the same way closes it as soon as both blocks are known. Between branches of different length
// chainwork decides, MESS and finality as before.
//
// It is a node's preference, not a validity rule: a light node holds no shares, counts no votes,
// and prefers the lower ID. Either way the next block settles it.

// SiblingVoteMargin is the lead in share units that beats the lower ID: more than the share rate
// times the worst latency between parts of the network — 300 failed at 10 s in the simulation,
// 1,000 held. A third of a block's shares.
const SiblingVoteMargin = 1000

// SetVotes gives the chain what counts the votes for a block: the share units seen built on it.
// Without it every sibling has none. It is called under the chain's lock, so it must not call
// back into the chain.
func (hc *HeaderChain) SetVotes(votes func(id [32]byte) uint64) {
	hc.mu.Lock()
	defer hc.mu.Unlock()
	hc.votes = votes
}

func (hc *HeaderChain) votesLocked(id [32]byte) uint64 {
	if hc.votes == nil {
		return 0
	}
	return hc.votes(id)
}

// isSiblingLocked: two different blocks on the same parent.
func isSiblingLocked(a, b *node) bool {
	return a != b && a.height == b.height && a.header.PrevBlock == b.header.PrevBlock
}

// prefersSiblingLocked says whether `cand` beats its sibling `cur`.
func (hc *HeaderChain) prefersSiblingLocked(cand, cur *node) bool {
	vc, vt := hc.votesLocked(cand.id), hc.votesLocked(cur.id)
	switch {
	case vc > vt && vc-vt > SiblingVoteMargin:
		return true
	case vt > vc && vt-vc > SiblingVoteMargin:
		return false
	}
	return lessID(cand.id, cur.id)
}

// ReconsiderSibling looks again at block `id` after a vote for it came in: if it is a sibling of
// the tip that now beats it, it becomes the tip. It returns true if the tip moved.
func (hc *HeaderChain) ReconsiderSibling(id [32]byte) bool {
	hc.mu.Lock()
	defer hc.mu.Unlock()
	cand, ok := hc.byID[id]
	if !ok || !isSiblingLocked(cand, hc.tip) || cand.tainted || hc.hasParkedAncestorLocked(cand) {
		return false
	}
	if !hc.prefersSiblingLocked(cand, hc.tip) {
		return false
	}
	hc.tip = cand
	return true
}
