package main

import (
	"net"
	"slices"
	"testing"
	"time"

	"powmachina/internal/chain"
	"powmachina/internal/mh"
	"powmachina/internal/p2p"
)

func heldEpochs(n *Node) []uint64 {
	n.epochs.mu.Lock()
	defer n.epochs.mu.Unlock()
	var held []uint64
	for e := range n.epochs.sets {
		held = append(held, e)
	}
	slices.Sort(held)
	return held
}

// The height in a header is the peer's word. An epoch is a DAG of up to 8 GiB and a RandomX cache, and
// the node keeps two: building one for every height a peer writes down would run the node out of memory
// and throw away the epoch it verifies the real chain with.
func TestNoEpochIsBuiltForAHeaderThatFailsTheCheapChecks(t *testing.T) {
	n := NewNode(chain.RegTest, chain.RegtestGenesis(), mh.TestProfile)
	defer n.Close()
	if err := n.MineBlocks(2); err != nil {
		t.Fatal(err)
	}
	before := heldEpochs(n)
	tip, height, tipID := n.chain.Tip()
	now := uint32(time.Now().Unix())
	const epoch = uint32(mh.EpochBlocks)

	peer := p2p.NewPeer(p2p.Address{IP: net.ParseIP("203.0.113.7"), Port: 1, LastSeen: time.Now()}, p2p.PeerInbound)
	// Parents nobody has ever seen: skipped, as headers from another branch are.
	var orphans []*chain.Header
	for _, e := range []uint32{7, 900, 800000} {
		orphans = append(orphans, &chain.Header{Version: 1, PrevBlock: [32]byte{0xAA, byte(e)}, Height: e * epoch,
			Bits: chain.RegtestTargetBits, Lane: mh.LaneG, Time: now})
	}
	added, err := n.acceptHeaders(peer, orphans)
	if added != 0 || err != nil {
		t.Fatalf("orphan headers: added %d, err %v", added, err)
	}
	if got := heldEpochs(n); !slices.Equal(got, before) {
		t.Fatalf("orphan headers changed the epochs held from %v to %v", before, got)
	}

	// A known parent and a height that does not follow it: refused, and the peer pays for it.
	lie := &chain.Header{Version: tip.Version, PrevBlock: tipID, Height: uint32(height) + 9*epoch,
		Bits: tip.Bits, Lane: mh.LaneG, Time: now}
	if _, err := n.acceptHeaders(peer, []*chain.Header{lie}); err == nil {
		t.Fatal("a header nine epochs above its parent was accepted")
	}
	if got := heldEpochs(n); !slices.Equal(got, before) {
		t.Fatalf("a header with a made-up height changed the epochs held from %v to %v", before, got)
	}
	if peer.BanScore() == 0 {
		t.Fatal("a header with a made-up height cost the peer nothing")
	}

	// And the chain still goes on.
	if err := n.MineBlocks(1); err != nil {
		t.Fatal(err)
	}
}
