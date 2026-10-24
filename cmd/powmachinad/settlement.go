package main

import (
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"powmachina/internal/chain"
	"powmachina/internal/mh"
)

// The settlement bodies of the blocks a node applies (the Sharepool spec §3.1).
//
// A block's shares are checked against its own branch: each has to be a share found on one of the
// ShareExpiry blocks below it, and none may have been settled by one of them already. The chain
// keeps headers only, so what the recent blocks settled is kept here, by block id, filled as
// blocks are applied and rebuilt from the stored bodies after a restart.

// settledCap bounds the memory: a few hundred blocks' worth covers every branch within the expiry.
const settledCap = 1024

type settledIndex struct {
	mu  sync.Mutex
	ids map[[32]byte][][32]byte // block id → the ids of the shares it settled
}

// epochHasher computes a share's PoW with the epoch its height needs.
type epochHasher struct{ c *epochCache }

func (e epochHasher) PoWHash(h *chain.Header) ([32]byte, error) {
	set, err := e.c.forHeight(uint64(h.Height))
	if err != nil {
		return [32]byte{}, err
	}
	return set.Hash(h)
}

// checkSettlement verifies a block's settlement body against its branch and remembers what it
// settled. The block's header has to be in the chain already, or at least its parent.
//
// An error wrapping chain.ErrSettlementUnchecked is the node's (a body it cannot read, an epoch
// it cannot build), not the block's; settlementFault tells the two apart.
func (n *Node) checkSettlement(b *chain.Block) error {
	id := b.Header.ID()
	if len(b.Settlement.Shares) == 0 {
		n.rememberSettled(id, nil)
		return nil
	}
	settled, err := n.settledBelow(b.Header.PrevBlock)
	if err != nil {
		return fmt.Errorf("%w: %w", chain.ErrSettlementUnchecked, err)
	}
	v, err := n.chain.SettlementViewOn(b.Header.PrevBlock, settled)
	if err != nil {
		return fmt.Errorf("%w: %w", chain.ErrSettlementUnchecked, err)
	}
	// A node without the full DAG verifies no shares (the Sharepool spec §7): at 35–100 ms a share
	// a block's settlement would take minutes. It checks the rest and trusts the PoW.
	var pow chain.PoWHasher
	if n.epochs.isFull() {
		pow = n.shares.PoWHash(epochHasher{n.epochs})
	}
	paid, err := b.CheckSettlement(v, pow, n.params)
	if err != nil {
		return err
	}
	ids := make([][32]byte, len(paid))
	for i, s := range paid {
		ids[i] = s.ID
	}
	n.rememberSettled(id, ids)
	return nil
}

func (n *Node) rememberSettled(block [32]byte, ids [][32]byte) {
	n.settled.mu.Lock()
	defer n.settled.mu.Unlock()
	if n.settled.ids == nil || len(n.settled.ids) >= settledCap {
		n.settled.ids = make(map[[32]byte][][32]byte)
	}
	n.settled.ids[block] = ids
}

// settledBelow answers whether a share was settled by `parent` or one of the blocks below it
// within the expiry — the blocks that could have included a share the next block includes.
func (n *Node) settledBelow(parent [32]byte) (func([32]byte) bool, error) {
	seen := make(map[[32]byte]bool)
	id := parent
	for d := 1; d <= chain.ShareExpiry; d++ {
		h, ok := n.chain.HeaderByID(id)
		if !ok || h.Height == 0 {
			break
		}
		ids, err := n.settledBy(id, h)
		if err != nil {
			return nil, err
		}
		for _, s := range ids {
			seen[s] = true
		}
		id = h.PrevBlock
	}
	return func(s [32]byte) bool { return seen[s] }, nil
}

// settledBy is what one applied block settled: from memory, or rebuilt from its body.
func (n *Node) settledBy(id [32]byte, h *chain.Header) ([][32]byte, error) {
	n.settled.mu.Lock()
	ids, ok := n.settled.ids[id]
	n.settled.mu.Unlock()
	if ok {
		return ids, nil
	}
	b, err := n.store.Block(id)
	if err != nil {
		return nil, fmt.Errorf("the body of %x: %w", id[:6], err)
	}
	if len(b.Settlement.Shares) > 0 {
		v, err := n.chain.SettlementViewOn(h.PrevBlock, nil)
		if err != nil {
			return nil, err
		}
		if ids, err = b.Settlement.ShareIDs(v); err != nil {
			return nil, err
		}
	}
	n.rememberSettled(id, ids)
	return ids, nil
}

// errBadShare marks a share that is wrong whoever looks at it: its PoW, its commitment, its bits.
// A peer that sends one is misbehaving. A share on a block we do not have, or one that expired on
// the way, is not that peer's fault.
var errBadShare = errors.New("a share that is invalid on any branch")

// errLightNode: the node runs without the full DAG and verifies no shares (-dag=full does).
var errLightNode = errors.New("this node verifies no shares — it runs without the full DAG (-dag=full)")

// acceptShare verifies a share from a miner or a peer, files it into the pool and pushes it to the
// other peers. `from` is the peer it came from, "" for a local miner.
//
// Only a node with the full DAG takes shares: a light one would spend 35–100 ms on each, a hundred
// a second. It neither pools nor relays them, and its templates settle none.
// acceptShare takes a share: from is the peer it came from (not relayed back there), origin the
// address it came in from, for the map of where shares come from (sharegeo.go).
func (n *Node) acceptShare(s *chain.Share, from, origin string) (err error) {
	known := false
	defer func() {
		if known {
			n.stats.inc(&n.stats.shares, "known")
		} else {
			n.stats.inc(&n.stats.shares, shareResult(err))
		}
	}()
	if !n.epochs.isFull() {
		return errLightNode
	}
	if n.shares.Has(s.ID()) {
		known = true
		return nil
	}
	hash, err := n.chain.CheckShare(s, epochHasher{n.epochs})
	if errors.Is(err, chain.ErrUnknownParent) {
		n.shareOrphans.add(s, from, origin, time.Now())
		return err
	}
	if err != nil {
		switch {
		case errors.Is(err, chain.ErrShareRoot), errors.Is(err, chain.ErrShareWeight),
			errors.Is(err, chain.ErrSharePoW), errors.Is(err, chain.ErrBadBits),
			errors.Is(err, chain.ErrBadLane), errors.Is(err, chain.ErrBadHeight):
			return fmt.Errorf("%w: %w", errBadShare, err)
		}
		return err
	}
	if !n.shares.Add(s, hash) {
		known = true
		return nil // the pool is full or another path filed it meanwhile
	}
	n.geo.note(origin)
	n.announceShare(s, from)
	// A vote for a sibling of the tip can make it the preferred one (the Sharepool spec §4).
	if n.chain.ReconsiderSibling(s.Header.PrevBlock) {
		log.Printf("switching to sibling %x: the tip no longer leads it by more than %d share units",
			s.Header.PrevBlock[:8], chain.SiblingVoteMargin)
		n.stats.siblingSwitched()
		return n.ApplyReadyBodies()
	}
	return nil
}

// SharePool sums the share pool for RPC getsharepool.
func (n *Node) SharePool() (bool, map[[32]byte]chain.ShareTally) {
	return n.epochs.isFull(), n.shares.Tally()
}

// Credits is the credit state of the applied tip, for getcredits.
func (n *Node) Credits() (int64, uint64, map[[32]byte]chain.Credit, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	tip, err := n.store.Tip()
	if err != nil {
		return 0, 0, nil, err
	}
	out := map[[32]byte]chain.Credit{}
	if err := n.store.ForEachCredit(func(k [32]byte, c chain.Credit) error {
		out[k] = c
		return nil
	}); err != nil {
		return 0, 0, nil, err
	}
	return n.store.Pot(), tip.Height, out, nil
}

// SubmitShare takes a share from a local miner (RPC submitshare) and says whether it is a block
// too. The hash comes from the pool, which computed it on the way in.
func (n *Node) SubmitShare(s *chain.Share) (bool, error) { return n.submitShare(s, "") }

// SubmitStratumShare implements stratum.Backend: a device's share, with the device's address.
func (n *Node) SubmitStratumShare(s *chain.Share, ip string) (bool, error) {
	return n.submitShare(s, ip)
}

func (n *Node) submitShare(s *chain.Share, origin string) (bool, error) {
	if err := n.acceptShare(s, "", origin); err != nil {
		return false, err
	}
	hash, err := n.shares.PoWHash(epochHasher{n.epochs}).PoWHash(&s.Header)
	if err != nil {
		return false, err
	}
	target, err := chain.CompactToTarget(s.Header.Bits)
	if err != nil {
		return false, err
	}
	return mh.MeetsTarget(&hash, &target), nil
}

// templateSettlement is the settlement body for the next block on the tip: the shares in the pool
// built on the tip or the blocks below it within the expiry, that none of them settled yet.
func (n *Node) templateSettlement(tipID [32]byte, tipHeight uint64) (*chain.Settlement, error) {
	branch := make(map[[32]byte]bool, chain.ShareExpiry)
	id := tipID
	for d := 1; d <= chain.ShareExpiry; d++ {
		h, ok := n.chain.HeaderByID(id)
		if !ok {
			break
		}
		branch[id] = true
		if h.Height == 0 {
			break
		}
		id = h.PrevBlock
	}
	settled, err := n.settledBelow(tipID)
	if err != nil {
		return nil, err
	}
	shares := n.shares.Candidates(func(prev [32]byte) bool { return branch[prev] }, settled,
		chain.MaxSettlementShares)
	// A block of the branch is not a share to pay again (CheckSettlement); one that reached the
	// pool as a share before it was a block leaves it here.
	kept := shares[:0]
	for _, s := range shares {
		if !branch[s.ID()] {
			kept = append(kept, s)
		}
	}
	shares = kept
	for {
		s, err := chain.NewSettlement(shares, tipHeight+1)
		if err != nil {
			return nil, err
		}
		if len(s.Serialize()) <= chain.MaxSettlementBytes {
			return s, nil
		}
		// Too many distinct miners for the bytes: the newest tenth waits for the next block.
		shares = shares[:len(shares)*9/10]
	}
}
